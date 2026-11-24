// Package hub owns rooms, peers and their WebRTC sessions, and wires each
// speaker to the media engine stream that translates for the listener.
package hub

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"

	"github.com/whispem/ambivox/server/internal/invite"
	"github.com/whispem/ambivox/server/internal/medialink"
	"github.com/whispem/ambivox/server/internal/metrics"
)

var Languages = []string{"en", "fr", "es", "it"}

const (
	roomCapacity = 2
	maxNameRunes = 40
	echoTail     = 600 * time.Millisecond
)

var silentFrame = []byte{0xf8, 0xff, 0xfe}

var (
	ErrRoomFull     = errors.New("this room already has two participants")
	ErrTooManyRooms = errors.New("the server has reached its room limit")
	ErrBadRoom      = errors.New("room names use 1-64 letters, digits, '-' or '_'")
	ErrBadLanguage  = errors.New("unsupported language")
	ErrShuttingDown = errors.New("the server is shutting down")

	roomPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

type Config struct {
	Link     *medialink.Link
	Logger   *slog.Logger
	Metrics  *metrics.Registry
	MaxRooms int

	ICEServers      []string
	PublicIPs       []string
	UDPPortMin      uint16
	UDPPortMax      uint16
	UDPPort         uint16
	IncludeLoopback bool
	DisableMDNS     bool
	AllowedOrigins  []string
	EchoGuard       bool

	Invites   *invite.Issuer
	AccessKey string
}

type Hub struct {
	cfg  Config
	api  *webrtc.API
	log  *slog.Logger
	ctx  context.Context
	stop context.CancelFunc
	mux  ice.UDPMux

	mu       sync.Mutex
	rooms    map[string]*Room
	sessions sync.WaitGroup

	sessionsTotal *metrics.Counter
	packetsIn     *metrics.Counter
	packetsDirect *metrics.Counter
	packetsEngine *metrics.Counter
	speechOut     *metrics.Counter
	captions      *metrics.Counter
	partials      *metrics.Counter
	muted         *metrics.Counter
	floorMuted    *metrics.Counter
}

func New(cfg Config) (*Hub, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Metrics == nil {
		cfg.Metrics = metrics.NewRegistry()
	}
	if cfg.MaxRooms <= 0 {
		cfg.MaxRooms = 100
	}

	media := &webrtc.MediaEngine{}
	err := media.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: opusCapability,
		PayloadType:        111,
	}, webrtc.RTPCodecTypeAudio)
	if err != nil {
		return nil, err
	}
	err = media.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: audioLevelURI}, webrtc.RTPCodecTypeAudio)
	if err != nil {
		return nil, err
	}
	interceptors := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(media, interceptors); err != nil {
		return nil, err
	}

	settings := webrtc.SettingEngine{LoggerFactory: pionLogs{cfg.Logger}}
	settings.SetIncludeLoopbackCandidate(cfg.IncludeLoopback)
	if cfg.DisableMDNS {
		settings.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	}
	if len(cfg.PublicIPs) > 0 {
		settings.SetNAT1To1IPs(cfg.PublicIPs, webrtc.ICECandidateTypeHost)
	}
	if cfg.UDPPortMin != 0 || cfg.UDPPortMax != 0 {
		if err := settings.SetEphemeralUDPPortRange(cfg.UDPPortMin, cfg.UDPPortMax); err != nil {
			return nil, fmt.Errorf("udp port range: %w", err)
		}
	}
	var mux ice.UDPMux
	if cfg.UDPPort != 0 {
		options := []ice.UDPMuxFromPortOption{
			ice.UDPMuxFromPortWithNetworks(ice.NetworkTypeUDP4),
			ice.UDPMuxFromPortWithLogger(settings.LoggerFactory.NewLogger("udpmux")),
		}
		if cfg.IncludeLoopback {
			options = append(options, ice.UDPMuxFromPortWithLoopback())
		}
		multi, err := ice.NewMultiUDPMuxFromPort(int(cfg.UDPPort), options...)
		if err != nil {
			return nil, fmt.Errorf("udp port %d: %w", cfg.UDPPort, err)
		}
		mux = multi
		settings.SetICEUDPMux(mux)
		settings.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	}

	ctx, stop := context.WithCancel(context.Background())
	h := &Hub{
		cfg: cfg,
		api: webrtc.NewAPI(
			webrtc.WithMediaEngine(media),
			webrtc.WithInterceptorRegistry(interceptors),
			webrtc.WithSettingEngine(settings),
		),
		log:   cfg.Logger,
		ctx:   ctx,
		stop:  stop,
		mux:   mux,
		rooms: make(map[string]*Room),
	}

	m := cfg.Metrics
	h.sessionsTotal = m.Counter("ambivox_sessions_total", "Signalling sessions accepted.")
	h.packetsIn = m.Counter("ambivox_rtp_packets_received_total", "RTP packets received from participants.")
	h.packetsDirect = m.Counter("ambivox_rtp_packets_relayed_total", "Packets relayed untouched between same-language participants.")
	h.packetsEngine = m.Counter("ambivox_rtp_packets_translated_total", "Packets handed to the media engine.")
	h.speechOut = m.Counter("ambivox_speech_packets_sent_total", "Translated speech packets sent to participants.")
	h.captions = m.Counter("ambivox_captions_total", "Final captions delivered to participants.")
	h.partials = m.Counter("ambivox_partial_captions_total", "Live caption updates delivered while someone speaks.")
	h.muted = m.Counter("ambivox_rtp_packets_muted_total", "Packets silenced because the sender's translation was playing.")
	h.floorMuted = m.Counter("ambivox_rtp_packets_same_room_muted_total", "Packets silenced because the other participant in the same room was speaking.")
	m.Gauge("ambivox_rooms", "Open rooms.", func() float64 { return float64(h.Rooms()) })
	m.Gauge("ambivox_peers", "Connected participants.", func() float64 { return float64(h.Peers()) })
	m.Gauge("ambivox_media_connected", "Whether the media engine link is up.", func() float64 {
		if cfg.Link.Connected() {
			return 1
		}
		return 0
	})
	m.Gauge("ambivox_media_frames_dropped", "Frames dropped because the media engine link was saturated.", func() float64 {
		return float64(cfg.Link.Dropped())
	})
	m.Collect(func() map[string]float64 {
		out := make(map[string]float64)
		for name, value := range cfg.Link.Stats() {
			out["ambivox_engine_"+name] = value
		}
		return out
	})

	cfg.Link.OnChange(h.mediaChanged)
	return h, nil
}

func (h *Hub) Close() {
	h.stop()
	// Retire the streams before waiting out the sessions: the engine link is
	// still up, so its Close frames can be delivered, and no room can post to
	// a session once its peers are marked closed.
	h.closeRooms()
	h.sessions.Wait()
	if h.mux != nil {
		_ = h.mux.Close()
	}
}

// closeRooms ends every remaining stream and stops the rooms from writing to
// sessions that are already over.
func (h *Hub) closeRooms() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, room := range h.rooms {
		room.mu.Lock()
		for _, peer := range room.peers {
			peer.closed.Store(true)
			room.retire(peer)
		}
		room.peers = nil
		room.floor = nil
		room.mu.Unlock()
		delete(h.rooms, id)
	}
}

func (h *Hub) Rooms() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.rooms)
}

func (h *Hub) Peers() int {
	h.mu.Lock()
	rooms := make([]*Room, 0, len(h.rooms))
	for _, room := range h.rooms {
		rooms = append(rooms, room)
	}
	h.mu.Unlock()

	total := 0
	for _, room := range rooms {
		room.mu.Lock()
		total += len(room.peers)
		room.mu.Unlock()
	}
	return total
}

func (h *Hub) iceServers() []webrtc.ICEServer {
	if len(h.cfg.ICEServers) == 0 {
		return nil
	}
	return []webrtc.ICEServer{{URLs: h.cfg.ICEServers}}
}

func (h *Hub) join(roomID, lang, name, token string) (*Peer, error) {
	if !roomPattern.MatchString(roomID) {
		return nil, ErrBadRoom
	}
	if h.cfg.Invites != nil {
		if err := h.cfg.Invites.Verify(roomID, token); err != nil {
			return nil, err
		}
	}
	if !slices.Contains(Languages, lang) {
		return nil, fmt.Errorf("%w: %q", ErrBadLanguage, lang)
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ctx.Err() != nil {
		return nil, ErrShuttingDown
	}
	room := h.rooms[roomID]
	if room == nil {
		if len(h.rooms) >= h.cfg.MaxRooms {
			return nil, ErrTooManyRooms
		}
		room = &Room{id: roomID, hub: h}
	}

	room.mu.Lock()
	defer room.mu.Unlock()
	if len(room.peers) >= roomCapacity {
		return nil, ErrRoomFull
	}
	peer := &Peer{
		ID:   newID(),
		Name: cleanName(name),
		Lang: lang,
		room: room,
		hub:  h,
		send: make(chan []byte, sendQueue),
	}
	var other *Peer
	if len(room.peers) == 1 {
		other = room.peers[0]
	}
	room.peers = append(room.peers, peer)
	h.rooms[roomID] = room
	room.rewire()

	connected, sameRoom, headphones := h.cfg.Link.Connected(), room.sameRoom, room.headphones
	welcome := message{
		Type:       "joined",
		ID:         peer.ID,
		Room:       roomID,
		Lang:       peer.Lang,
		Name:       peer.Name,
		Connected:  &connected,
		SameRoom:   &sameRoom,
		Headphones: &headphones,
		Backend:    h.cfg.Link.Backend(),
		Languages:  Languages,
	}
	if len(h.cfg.ICEServers) > 0 {
		welcome.ICEServers = []iceEntry{{URLs: h.cfg.ICEServers}}
	}
	if other != nil {
		welcome.Peer = other.info()
		other.notify(message{Type: "peer-joined", Peer: peer.info()})
	}
	peer.notify(welcome)
	h.log.Info("peer joined", "room", roomID, "peer", peer.ID, "lang", lang)
	return peer, nil
}

func (h *Hub) leave(peer *Peer) {
	room := peer.room
	h.mu.Lock()
	room.mu.Lock()
	index := slices.Index(room.peers, peer)
	if index >= 0 {
		peer.closed.Store(true)
		room.peers = slices.Delete(room.peers, index, index+1)
		room.retire(peer)
		if room.floor == peer {
			room.floor = nil
		}
		room.rewire()
		for _, other := range room.peers {
			other.notify(message{Type: "peer-left", ID: peer.ID})
		}
		if len(room.peers) == 0 {
			delete(h.rooms, room.id)
		}
	}
	room.mu.Unlock()
	h.mu.Unlock()

	if index >= 0 {
		h.log.Info("peer left", "room", room.id, "peer", peer.ID)
	}
	peer.closeConnection()
}

func (h *Hub) mediaChanged(connected bool) {
	h.mu.Lock()
	rooms := make([]*Room, 0, len(h.rooms))
	for _, room := range h.rooms {
		rooms = append(rooms, room)
	}
	h.mu.Unlock()

	status := message{Type: "media", Connected: &connected, Backend: h.cfg.Link.Backend()}
	for _, room := range rooms {
		room.mu.Lock()
		// Streams belong to the connection they were opened on. When it goes
		// away every direction is bare; when it comes back every room builds a
		// fresh set for the pairing as it stands now. Either way the bridges
		// the old connection left behind must never deliver again.
		for _, peer := range room.peers {
			room.retire(peer)
		}
		if connected {
			room.rewire()
		}
		for _, peer := range room.peers {
			peer.notify(status)
		}
		room.mu.Unlock()
	}
}

type Room struct {
	id  string
	hub *Hub

	mu         sync.Mutex
	peers      []*Peer
	sameRoom   bool
	headphones bool
	floor      *Peer
	floorUntil time.Time
}

func (r *Room) setSameRoom(on bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sameRoom, r.floor = on, nil
	for _, p := range r.peers {
		p.notify(message{Type: "same-room", SameRoom: &on})
	}
}

func (r *Room) setHeadphones(on bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.headphones = on
	for _, p := range r.peers {
		p.notify(message{Type: "headphones", Headphones: &on})
	}
	r.rewire()
}

func (r *Room) other(p *Peer) *Peer {
	for _, candidate := range r.peers {
		if candidate != p {
			return candidate
		}
	}
	return nil
}

// setLang changes one participant's language and rebuilds the streams that
// depended on the old pairing. An unsupported language changes nothing.
func (r *Room) setLang(p *Peer, lang string) error {
	if !slices.Contains(Languages, lang) {
		return fmt.Errorf("%w: %q", ErrBadLanguage, lang)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if p.Lang == lang {
		return nil
	}
	p.Lang = lang
	r.rewire()
	for _, peer := range r.peers {
		peer.notify(message{Type: "lang", ID: p.ID, Lang: lang})
	}
	r.hub.log.Info("peer changed language", "room", r.id, "peer", p.ID, "lang", lang)
	return nil
}

// retire takes the peer's current translation stream out of effect: from the
// moment it returns, the bridge delivers nothing more and counts nothing more,
// and the engine is asked to finish the stream. Taking b.mu after setting the
// flag waits out a callback that is already delivering, so no stale speech or
// caption can land after the new stream is installed.
func (r *Room) retire(p *Peer) {
	b := p.stream
	if b == nil {
		return
	}
	p.stream = nil
	b.retired.Store(true)
	b.mu.Lock()
	b.mu.Unlock()
	if b.stream != nil {
		go b.stream.Close()
	}
}

// rewire makes each peer's engine stream match the current pairing. The caller
// holds r.mu. Old streams are retired before new ones are opened, so the two
// can never both be in effect. A stream that cannot be opened leaves its
// direction bare until the next change tries again.
func (r *Room) rewire() {
	for _, p := range r.peers {
		other := r.other(p)
		wanted := other != nil && other.Lang != p.Lang
		if b := p.stream; b != nil &&
			(!wanted || b.to != other || b.source != p.Lang || b.target != other.Lang || b.early != r.headphones) {
			r.retire(p)
		}
		if !wanted || p.stream != nil {
			continue
		}
		b := &bridge{room: r, from: p, to: other, source: p.Lang, target: other.Lang, early: r.headphones}
		stream, err := r.hub.cfg.Link.Open(p.Lang, other.Lang, r.headphones, b)
		if err != nil {
			r.hub.log.Warn("cannot open translation stream", "room", r.id, "peer", p.ID, "error", err)
			continue
		}
		b.stream = stream
		p.stream = b
	}
}

func (r *Room) route(from *Peer, sequence uint16, payload []byte, level int) {
	r.mu.Lock()
	other := r.other(from)
	direct := other != nil && other.Lang == from.Lang
	stream := from.stream
	guard := r.hub.cfg.EchoGuard && !r.headphones
	if r.sameRoom && !r.hasFloor(from, level, time.Now()) {
		payload = silentFrame
		r.hub.floorMuted.Inc()
	}
	r.mu.Unlock()
	r.hub.packetsIn.Inc()

	switch {
	case other == nil:
	case direct:
		other.writeSample(payload)
		r.hub.packetsDirect.Inc()
	case stream != nil:
		if guard && time.Now().UnixNano() < from.quietUntil.Load() {
			payload = silentFrame
			r.hub.muted.Inc()
		}
		// Hold the bridge for the write so a stream retired concurrently
		// cannot receive another packet after it stopped being in effect.
		stream.mu.Lock()
		if !stream.retired.Load() {
			stream.stream.Write(sequence, payload)
			r.hub.packetsEngine.Inc()
		}
		stream.mu.Unlock()
	}
}

// bridge carries one direction of a call: one speaker's audio into the engine,
// the translation and captions back out. A bridge is never patched in place;
// when the pairing changes the room retires it and builds another, and a
// retired bridge delivers nothing and counts nothing.
type bridge struct {
	room   *Room
	from   *Peer
	to     *Peer
	source string
	target string
	early  bool
	stream *medialink.Stream

	// retired is set once, before the stream is asked to close. mu makes the
	// flag a barrier: retire waits for it, so a callback either sees retired
	// or finishes before retire returns and nothing of this bridge is in
	// flight afterwards.
	retired atomic.Bool
	mu      sync.Mutex
}

// active reports whether this bridge is still the one the room routes through.
// Only the flag is consulted: the room sets it before it lets go of the
// bridge, so a callback that sees it clear has nothing left to do, and a
// callback that does not is finished before the flag is relied upon (retire
// waits on mu).
func (b *bridge) active() bool {
	return !b.retired.Load()
}

// While the listener plays the translation, the speaker's microphone may pick
// it up from the listener's loudspeaker and send it round again.
func (b *bridge) OnSpeech(packet []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.active() {
		return
	}
	b.from.quietUntil.Store(time.Now().Add(opusDuration(packet) + echoTail).UnixNano())
	b.to.writeSample(packet)
	b.room.hub.speechOut.Inc()
}

func (b *bridge) OnCaption(kind captionKind, utterance uint32, text string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.active() {
		return
	}
	final := !kind.Partial()
	caption := message{Type: "caption", From: b.from.ID, Kind: kind.String(), Utterance: &utterance, Final: &final, Text: text}
	b.from.notify(caption)
	b.to.notify(caption)
	if final {
		b.room.hub.captions.Add(2)
	} else {
		b.room.hub.partials.Add(2)
	}
}

func (b *bridge) OnError(text string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.active() {
		return
	}
	b.room.hub.log.Warn("translation error", "room", b.room.id, "peer", b.from.ID, "error", text)
	b.from.notify(message{Type: "error", Message: text})
}

// OnClosed is called by the engine link when the stream is gone. It must not
// take b.mu: retire holds b.mu only to wait out a delivery, never across this.
func (b *bridge) OnClosed() {
	b.room.mu.Lock()
	defer b.room.mu.Unlock()
	b.retired.Store(true)
	if b.from.stream == b {
		b.from.stream = nil
	}
}

func newID() string {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(raw[:])
}

func cleanName(name string) string {
	name = strings.Join(strings.Fields(name), " ")
	if utf8.RuneCountInString(name) > maxNameRunes {
		name = string([]rune(name)[:maxNameRunes])
	}
	if name == "" {
		return "Guest"
	}
	return name
}
