package sfu

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"atomirex.com/umbrella/razor"
	"github.com/atomirex/mdns"
	"github.com/gorilla/websocket"
	"github.com/pion/interceptor"
	"github.com/pion/logging"
	"github.com/pion/webrtc/v4"
)

var (
	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
)

type sfuCommand int

const (
	sfuAddOutgoingTracksForIncomingTrack sfuCommand = iota
	sfuRemoveAllOutgoingTracksForIncomingTrack
	sfuSignalClients
	sfuAddClient
	sfuRemoveClient
	sfuRemoveClientTracks

	sfuGetCurrentServers
	sfuSetCurrentServers

	sfuServerConnectionEvent
	sfuServerRetry
	sfuServerConnectTimeout

	sfuGetStatus
)

type sfuCommandMessage struct {
	intrack           *incomingTrack
	client            *client
	remoteClient      RemoteClient
	trackOwner        RemoteClient
	SetCurrentServers *CurrentServers

	serverEvent      *serverConnectionEvent
	serverURL        string
	serverGeneration uint64

	result *sfuCommandResult
}

type sfuCommandResult struct {
	servers chan *CurrentServers
	status  chan *SFUStatus
}

type Sfu struct {
	clients     []RemoteClient
	sfuCommands chan sfuCommandMessage

	peerConnectionFactory PeerConnectionFactory
	remoteClientFactory   RemoteClientFactory

	// UmbrellaID -> track
	localTracks map[string]*incomingTrack // Set of all incoming tracks which are being relayed

	// UmbrellaID -> the client which contributed that inbound track. When a client
	// goes away its tracks are removed from localTracks (and from every other
	// client), so /status cannot retain tracks from a disconnected session.
	trackOwners map[string]RemoteClient

	handler *razor.MessageHandler[sfuCommand, sfuCommandMessage]

	// intendedServers is the last submitted intent. servers tracks, for every intended
	// address, the single connection attempt or established connection working towards
	// that intent. The two converge after every evaluation; they are kept separate so
	// the visible list only shows servers which are genuinely connected.
	intendedServers map[string]bool
	servers         map[string]*serverEntry

	// serverTokenSeq hands out the tokens which identify individual connection
	// attempts, so stale timers can be recognised and ignored.
	serverTokenSeq uint64

	serverRetryBaseDelay     time.Duration
	serverRetryMaxDelay      time.Duration
	serverMaxConnectAttempts int
	serverConnectTimeout     time.Duration

	// serverHealthyConnectionDuration is how long a connection must stay up before a
	// later disconnect resets the backoff attempt count rather than continuing it.
	serverHealthyConnectionDuration time.Duration

	logger *razor.Logger

	loggerPion logging.LeveledLogger

	// mdnsMu guards mdnsConn: it is set from the mdns goroutine in main and read from
	// client dial goroutines.
	mdnsMu   sync.RWMutex
	mdnsConn *mdns.Conn
}

func (s *Sfu) GetStatus() *SFUStatus {
	msg := sfuCommandMessage{result: &sfuCommandResult{
		status: make(chan *SFUStatus, 1),
	}}

	if !s.handler.Send(sfuGetStatus, &msg) {
		return &SFUStatus{}
	}

	return <-msg.result.status
}

func (s *Sfu) GetCurrentServers() *CurrentServers {
	msg := sfuCommandMessage{result: &sfuCommandResult{
		servers: make(chan *CurrentServers, 1),
	}}

	if !s.handler.Send(sfuGetCurrentServers, &msg) {
		return &CurrentServers{}
	}

	return <-msg.result.servers
}

func (s *Sfu) SetCurrentServers(update *CurrentServers) *CurrentServers {
	msg := sfuCommandMessage{
		SetCurrentServers: update,
		result: &sfuCommandResult{
			servers: make(chan *CurrentServers, 1),
		},
	}

	if !s.handler.Send(sfuSetCurrentServers, &msg) {
		return &CurrentServers{}
	}

	return <-msg.result.servers
}

func NewSfu(logger *razor.Logger, minPort uint16, maxPort uint16, ip *string) *Sfu {
	loggerPion := logging.NewDefaultLoggerFactory().NewLogger("sfu-ws")
	loggerPion.(*logging.DefaultLeveledLogger).SetLevel(logging.LogLevelError)

	settingEngine := webrtc.SettingEngine{}

	settingEngine.SetEphemeralUDPPortRange(minPort, maxPort)

	if ip != nil {
		settingEngine.SetNAT1To1IPs([]string{*ip}, webrtc.ICECandidateTypeHost)
	}

	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		panic("Error setting default codecs")
	}

	// Now we know what this is and why . . . . facepalm
	// This is the "default" nack, sr, rr etc. handling for rtcp
	// We can come back to it when it's a problem
	interceptorRegistry := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, interceptorRegistry); err != nil {
		panic("Panic setting interceptors")
	}

	webrtcApi := webrtc.NewAPI(webrtc.WithSettingEngine(settingEngine), webrtc.WithMediaEngine(m))

	s := &Sfu{
		sfuCommands:         make(chan sfuCommandMessage, 256),
		remoteClientFactory: &DefaultRemoteClientFactory{},
		peerConnectionFactory: &PionPeerConnectionFactory{
			pcConfig: &webrtc.Configuration{
				ICEServers: []webrtc.ICEServer{
					{
						URLs: []string{"stun:stun.l.google.com:19302"},
					},
				},
			},
			webrtcApi: webrtcApi,
			logger:    logger,
		},
		localTracks:     make(map[string]*incomingTrack),
		trackOwners:     make(map[string]RemoteClient),
		intendedServers: make(map[string]bool),
		servers:         make(map[string]*serverEntry),
		logger:          logger,
		loggerPion:      loggerPion,

		serverRetryBaseDelay:     defaultServerRetryBaseDelay,
		serverRetryMaxDelay:      defaultServerRetryMaxDelay,
		serverMaxConnectAttempts: defaultServerMaxConnectAttempts,
		serverConnectTimeout:     defaultServerConnectTimeout,

		serverHealthyConnectionDuration: defaultServerHealthyConnectionDuration,
	}

	s.handler = razor.NewMessageHandler(logger, "sfu", 1024, s.newSfuHandlerCallback())

	s.handler.Loop(func() {
		panic("SFU unexpectedly terminated")
	})

	return s
}

func (s *Sfu) isServerConnected(url string) bool {
	entry, exists := s.servers[url]
	return exists && entry.state == ServerState_Connected
}

// The following implement RemoteClientObserver for relay server connections. They only
// enqueue an event; all state changes happen on the SFU message handler goroutine so
// that submissions and connection lifecycle events are applied in one serial order.

func (s *Sfu) ServerConnected(url string, client RemoteClient) {
	s.handler.Send(sfuServerConnectionEvent, &sfuCommandMessage{
		serverEvent: &serverConnectionEvent{kind: serverEventConnected, url: url, client: client},
	})
}

func (s *Sfu) ServerConnectFailed(url string, client RemoteClient, err error) {
	s.handler.Send(sfuServerConnectionEvent, &sfuCommandMessage{
		serverEvent: &serverConnectionEvent{kind: serverEventConnectFailed, url: url, client: client, err: err},
	})
}

func (s *Sfu) ServerDisconnected(url string, client RemoteClient, err error) {
	s.handler.Send(sfuServerConnectionEvent, &sfuCommandMessage{
		serverEvent: &serverConnectionEvent{kind: serverEventDisconnected, url: url, client: client, err: err},
	})
}

func (s *Sfu) addTrack(intrack *incomingTrack) {
	s.handler.Send(sfuAddOutgoingTracksForIncomingTrack, &sfuCommandMessage{intrack: intrack})
}

func (s *Sfu) removeOutgoingTracksForIncomingTrack(t *incomingTrack) {
	s.handler.Send(sfuRemoveAllOutgoingTracksForIncomingTrack, &sfuCommandMessage{intrack: t})
}

// removeLocalTrackLocked stops relaying a track and tells every remaining client to
// drop their outgoing sender for it. Must run on the SFU handler goroutine.
func (s *Sfu) removeLocalTrackLocked(intrack *incomingTrack) {
	umbrellaID := intrack.UmbrellaID()

	delete(s.localTracks, umbrellaID)
	delete(s.trackOwners, umbrellaID)

	for _, c := range s.clients {
		c.RemoveOutgoingTracksForIncomingTrack(intrack)
	}
}

// removeTracksOwnedByLocked removes every track contributed by a client which has gone
// away, so a disconnected session cannot leave its inbound tracks or their senders
// visible in /status. Returns whether anything was removed.
func (s *Sfu) removeTracksOwnedByLocked(owner RemoteClient) bool {
	if owner == nil {
		return false
	}

	removed := make([]*incomingTrack, 0)
	for umbrellaID, o := range s.trackOwners {
		if o != owner {
			continue
		}

		if intrack, exists := s.localTracks[umbrellaID]; exists {
			removed = append(removed, intrack)
		}
	}

	for _, intrack := range removed {
		s.removeLocalTrackLocked(intrack)
	}

	return len(removed) > 0
}

// dropTracksOwnedBy is the synchronous entry point used when a relay server is stopped
// from the SFU handler goroutine (removal from the intent list).
func (s *Sfu) dropTracksOwnedBy(owner RemoteClient) {
	s.removeTracksOwnedByLocked(owner)
}

func (s *Sfu) WebsocketHandler(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.logger.Error("sfu", "Failed to upgrade HTTP to Websocket: "+err.Error())
		return
	}

	s.remoteClientFactory.NewClient(&RemoteClientParameters{logger: s.logger, ws: ws, s: s})
}

func (s *Sfu) SetMdnsConn(mdnsConn *mdns.Conn) {
	s.mdnsMu.Lock()
	s.mdnsConn = mdnsConn
	s.mdnsMu.Unlock()
}

// Use our own mdns for resolving .local addresses to work around compatibility problems
// If we are in cloud mode this will return an error
// If the host is not found this will return an error
// If the host is found the string returned will contain the IP as a string . . .
func (s *Sfu) mdnsLookup(hostname string) (string, error) {
	s.mdnsMu.RLock()
	mdnsConn := s.mdnsConn
	s.mdnsMu.RUnlock()

	if mdnsConn == nil {
		return "", fmt.Errorf("attempting to resolve local subnet host while mdns not in use")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	_, addr, err := mdnsConn.QueryAddr(ctx, hostname)
	if err != nil {
		return "", err
	} else {
		return addr.String(), nil
	}
}
