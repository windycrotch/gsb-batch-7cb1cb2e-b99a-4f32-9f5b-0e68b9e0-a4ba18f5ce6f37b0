package hub

import (
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/whispem/ambivox/server/internal/medialink"
	"github.com/whispem/ambivox/server/internal/protocol"
)

const sendQueue = 64

type captionKind = protocol.CaptionKind

var opusCapability = webrtc.RTPCodecCapability{
	MimeType:    webrtc.MimeTypeOpus,
	ClockRate:   48000,
	Channels:    2,
	SDPFmtpLine: "minptime=10;useinbandfec=1",
}

var errAlreadyNegotiated = errors.New("this session already has a media connection")

type peerInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Lang string `json:"lang"`
}

type message struct {
	Type       string     `json:"type"`
	ID         string     `json:"id,omitempty"`
	Room       string     `json:"room,omitempty"`
	Lang       string     `json:"lang,omitempty"`
	Name       string     `json:"name,omitempty"`
	Token      string     `json:"token,omitempty"`
	SameRoom   *bool      `json:"sameRoom,omitempty"`
	Headphones *bool      `json:"headphones,omitempty"`
	Peer       *peerInfo  `json:"peer,omitempty"`
	SDP        string     `json:"sdp,omitempty"`
	From       string     `json:"from,omitempty"`
	Kind       string     `json:"kind,omitempty"`
	Utterance  *uint32    `json:"utterance,omitempty"`
	Final      *bool      `json:"final,omitempty"`
	Text       string     `json:"text,omitempty"`
	Message    string     `json:"message,omitempty"`
	Connected  *bool      `json:"connected,omitempty"`
	Backend    string     `json:"backend,omitempty"`
	ICEServers []iceEntry `json:"iceServers,omitempty"`
	Languages  []string   `json:"languages,omitempty"`
}

type iceEntry struct {
	URLs []string `json:"urls"`
}

type Peer struct {
	ID   string
	Name string
	Lang string

	room *Room
	hub  *Hub
	send chan []byte

	quietUntil atomic.Int64

	mu  sync.Mutex
	pc  *webrtc.PeerConnection
	out *webrtc.TrackLocalStaticSample

	// Guarded by room.mu.
	stream      *medialink.Stream
	streamTo    *Peer
	streamEarly bool
	level       int
	levelAt     time.Time
}

func (p *Peer) info() *peerInfo {
	return &peerInfo{ID: p.ID, Name: p.Name, Lang: p.Lang}
}

// notify queues a signalling message; a peer too slow to drain its queue loses it.
func (p *Peer) notify(m message) {
	data, err := json.Marshal(m)
	if err != nil {
		return
	}
	select {
	case p.send <- data:
	default:
	}
}

func (p *Peer) writeSample(payload []byte) {
	p.mu.Lock()
	out := p.out
	p.mu.Unlock()
	if out != nil {
		_ = out.WriteSample(media.Sample{Data: payload, Duration: opusDuration(payload)})
	}
}

func (p *Peer) negotiate(offer string, onFail func()) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pc != nil {
		return "", errAlreadyNegotiated
	}

	pc, err := p.hub.api.NewPeerConnection(webrtc.Configuration{ICEServers: p.hub.iceServers()})
	if err != nil {
		return "", err
	}
	fail := func(err error) (string, error) {
		_ = pc.Close()
		return "", err
	}

	out, err := webrtc.NewTrackLocalStaticSample(opusCapability, "audio", "ambivox-"+p.ID)
	if err != nil {
		return fail(err)
	}
	sender, err := pc.AddTrack(out)
	if err != nil {
		return fail(err)
	}
	go func() {
		buf := make([]byte, 1500)
		for {
			if _, _, err := sender.Read(buf); err != nil {
				return
			}
		}
	}()

	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		if track.Kind() != webrtc.RTPCodecTypeAudio {
			return
		}
		levelID := audioLevelID(receiver)
		for {
			packet, _, err := track.ReadRTP()
			if err != nil {
				return
			}
			if len(packet.Payload) > 0 {
				p.room.route(p, packet.SequenceNumber, packet.Payload, audioLevel(packet, levelID))
			}
		}
	})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		p.hub.log.Debug("connection state", "peer", p.ID, "state", state.String())
		if state == webrtc.PeerConnectionStateFailed {
			onFail()
		}
	})

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer}); err != nil {
		return fail(err)
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return fail(err)
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		return fail(err)
	}
	select {
	case <-gathered:
	case <-p.hub.ctx.Done():
		return fail(ErrShuttingDown)
	}

	p.pc, p.out = pc, out
	return pc.LocalDescription().SDP, nil
}

func (p *Peer) closeConnection() {
	p.mu.Lock()
	pc := p.pc
	p.pc, p.out = nil, nil
	p.mu.Unlock()
	if pc != nil {
		_ = pc.Close()
	}
}
