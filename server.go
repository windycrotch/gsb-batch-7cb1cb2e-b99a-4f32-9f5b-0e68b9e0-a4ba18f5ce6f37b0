// license that can be found in the LICENSE file.

// Package udpsocket is a simple UDP server to make a virtual secure channel with the clients
package udpsocket

import (
	"bytes"
	"context"
	"errors"
	"io/ioutil"
	"log"
	"net"
	"sync"
	"time"

	"github.com/theredrad/udpsocket/crypto"
	"github.com/theredrad/udpsocket/encoding"
	"github.com/theredrad/udpsocket/encoding/pb"
)

// HandlerFunc is called when a custom message type is received from the client
type HandlerFunc func(id string, t byte, p []byte)

type Option func(*Server)

// custom error types
var (
	ErrInvalidRecordType            = errors.New("invalid record type")
	ErrInsecureEncryptionKeySize    = errors.New("insecure encryption key size")
	ErrClientSessionNotFound        = errors.New("client session not found")
	ErrClientAddressIsNotRegistered = errors.New("client address is not registered")
	ErrClientNotFound               = errors.New("client not found")
	ErrMinimumPayloadSizeLimit      = errors.New("minimum payload size limit")
	ErrClientCookieIsInvalid        = errors.New("client cookie is invalid")
	ErrInvalidPayloadBodySize       = errors.New("payload body size is invalid")
	ErrClientSessionExpired         = errors.New("client session expired due to heartbeat timeout")
	ErrDuplicateHandshake           = errors.New("duplicate handshake request ignored")
)

const (
	HandshakeClientHelloRecordType byte = 1 << iota
	HandshakeHelloVerifyRecordType
	HandshakeServerHelloRecordType
	PingRecordType
	PongRecordType
	UnAuthenticated

	defaultRSAKeySize         int = 2048 // default RSA key size, this options is used to initiate new RSA implementation if no asymmetric encryption is passed
	defaultMinimumPayloadSize int = 3
	defaultReadBufferSize     int = 2048

	insecureSymmKeySize int = 32 // A symmetric key smaller than 256 bits is not secure. 256-bits = 32 bytes in size
)

// incoming bytes are parsed to the record struct
type record struct {
	Type       byte
	ProtoMajor uint8
	ProtoMinor uint8
	Body       []byte
	Extra      []byte
}

// rawRecord is sent to the rawRecords channel when a new payload has been received
type rawRecord struct {
	payload []byte
	addr    *net.UDPAddr
}

// Client is an authenticated UDP client
type Client struct {
	ID string

	sessionID []byte // Session ID is a secret byte array that indicates the client is already done the handshake process, the client must prepend these bytes into the start of each record body before encryption

	addr *net.UDPAddr // UDP address of the client

	eKey []byte // Client encryption key to decrypt & encrypt a record body with the symmetric encryption algorithm

	lastHeartbeat *time.Time // last time when a record has been received from the client.

	// removed is set once the client is deleted by expiration/cleanup. It lets
	// in-flight broadcast goroutines skip the write instead of sending to a
	// deleted client. Guarded by the embedded mutex.
	removed bool

	sync.Mutex
}

// The Server is a UDP listener that handles the handshake process, encryption, client authentication, sending records to the client & proxy custom record types to the HandlerFunc
type Server struct {
	protocolVersion         [2]byte             // Protocol version
	readBufferSize          int                 // Buffer limit size for incoming bytes
	minimumPayloadSize      int                 // Minimum payload size to ignore too short incoming bytes
	heartbeatExpiration     time.Duration       // Expiration time of last heartbeat to delete client
	conn                    *net.UDPConn        // UDP connection to listen
	authClient              AuthClient          // an implementation of the AuthClient to authenticate the user token, if not set, no authentication will apply
	transcoder              encoding.Transcoder // an implementation of the Transcoder to encode & decode the record body, if not set, an implementation of the Protobuf will use
	asymmCrypto             crypto.Asymmetric   // an implementation of Asymmetric encryption to decrypt the body of the client handshake hello record
	symmCrypto              crypto.Symmetric    // an implementation of Symmetric encryption to encrypt & decrypt records body for the client after a successful handshake
	handler                 HandlerFunc         // Handler func which is called when a custom record type received
	clients                 map[string]*Client  // Map of client with index of client ID
	garbageCollectionTicker *time.Ticker        // Client garbage collector ticker
	garbageCollectionStop   chan bool           // Client garbage collector stop channel
	sessionManager          *sessionManager     // the Session manager generates cookie & session ID
	sessions                map[string]*Client  // Map of client with index of IP_PORT
	rawRecords              chan rawRecord      // raw records channel
	logger                  *log.Logger         // Logger
	stop                    chan bool           // stop channel to stop listening
	wg                      *sync.WaitGroup

	// mu guards clients, sessions and handshakeCache.
	mu sync.RWMutex
	// handlerMu guards the user Handler callback.
	handlerMu sync.RWMutex
	// handshakeCache makes identical cookie-bearing handshakes idempotent.
	handshakeCache map[string]*handshakeLease
	handshakeTTL   time.Duration

	// serveMu serializes Serve/Stop lifecycle transitions.
	serveMu   sync.Mutex
	serving   bool
	serveDone chan struct{}

	// bcastMu makes WaitGroup accounting safe against concurrent Stop/Wait.
	bcastMu sync.Mutex
}

// NewServer accepts UDP connection & configs & returns a new instance of the Server
// If options is nil or any required options isn't passed, a default instance of it will be set, e.g. Protobuf implementation if no Transcoder is set
func NewServer(conn *net.UDPConn, options ...Option) (*Server, error) {
	s := Server{
		conn: conn,

		clients:        make(map[string]*Client),
		sessions:       make(map[string]*Client),
		handshakeCache: make(map[string]*handshakeLease),

		garbageCollectionStop: make(chan bool, 1),
		stop:                  make(chan bool, 1),

		wg: &sync.WaitGroup{},

		rawRecords: make(chan rawRecord),
	}

	for _, opt := range options {
		opt(&s)
	}

	if s.readBufferSize == 0 {
		s.readBufferSize = defaultReadBufferSize
	}

	if s.minimumPayloadSize == 0 {
		s.minimumPayloadSize = defaultMinimumPayloadSize
	}

	if s.handshakeTTL == 0 {
		s.handshakeTTL = defaultHandshakeReplayTTL
	}

	if s.symmCrypto == nil {
		s.symmCrypto = crypto.NewAES(crypto.AES_CBC)
	}

	var err error
	if s.asymmCrypto == nil {
		s.asymmCrypto, err = crypto.NewRSA(defaultRSAKeySize)
		if err != nil {
			return nil, err
		}
	}

	if s.authClient == nil {
		s.authClient = &DefaultAuthClient{}
	}

	if s.transcoder == nil {
		s.transcoder = &pb.Protobuf{}
	}

	s.sessionManager, err = newSessionManager()
	if err != nil {
		return nil, err
	}

	if s.logger == nil { // discard logging if no logger is set
		s.logger = log.New(ioutil.Discard, "", 0)
	}

	return &s, nil
}

// SetHandler sets the handler function as a callback to call when a custom record type is received from the client.
// It is safe to call while the server is serving.
func (s *Server) SetHandler(f HandlerFunc) {
	s.handlerMu.Lock()
	s.handler = f
	s.handlerMu.Unlock()
}

func (s *Server) getHandler() HandlerFunc {
	s.handlerMu.RLock()
	h := s.handler
	s.handlerMu.RUnlock()
	return h
}

func (s *Server) handleRawRecords() {
	s.serveMu.Lock()
	records := s.rawRecords
	s.serveMu.Unlock()
	s.handleRawRecordsFrom(records)
}

// handleRawRecordsFrom processes records read from the provided epoch channel.
func (s *Server) handleRawRecordsFrom(records <-chan rawRecord) {
	for r := range records {
		s.handleRecord(r.payload, r.addr)
	}
}

// Serve starts listening to the UDP port for incoming bytes & then sends payload and sender address into the rawRecords channel if no error is found.
//
// Serve is single-active: a second call while the server is already serving is
// a safe no-op. After Stop returns, Serve can be called again on the same
// connection: the new epoch starts from an empty session registry and freshly
// rotated cookie/session secrets.
func (s *Server) Serve() {
	s.serveMu.Lock()
	if s.serving {
		s.serveMu.Unlock()
		s.logger.Println("Serve called while the server is already serving, ignoring")
		return
	}

	stopCh := make(chan bool, 1)
	gcStopCh := make(chan bool, 1)
	records := make(chan rawRecord)
	done := make(chan struct{})

	var ticker *time.Ticker
	if s.heartbeatExpiration > 0 {
		ticker = time.NewTicker(s.heartbeatExpiration)
	}

	s.serving = true
	s.serveDone = done
	s.stop = stopCh
	s.garbageCollectionStop = gcStopCh
	s.rawRecords = records
	s.garbageCollectionTicker = ticker

	s.bcastMu.Lock()
	s.wg = &sync.WaitGroup{}
	s.bcastMu.Unlock()
	s.serveMu.Unlock()

	// NOTE: state is intentionally not reset here. NewServer starts from fresh
	// maps/secrets, and Stop converges the state (clears sessions and rotates
	// secrets) before returning. Resetting again on Serve would race clients
	// that compute cookies from the shared sessionManager immediately after
	// `go Serve()`.

	if err := s.conn.SetReadDeadline(time.Time{}); err != nil { // no deadline
		s.logger.Printf("error while resetting read deadline: %s", err)
	}

	procDone := make(chan struct{})
	go func() {
		defer close(procDone)
		s.handleRawRecordsFrom(records)
	}()
	if ticker != nil {
		go s.garbageCollectionLoop(gcStopCh, ticker)
	}

	s.readLoop(records, stopCh)

	// This epoch's read loop has exited; tear its auxiliary goroutines down.
	select {
	case gcStopCh <- true:
	default:
	}
	if ticker != nil {
		ticker.Stop()
	}
	close(records)
	<-procDone // no record handler can repopulate state after Stop's reset

	s.serveMu.Lock()
	s.serving = false
	if s.serveDone == done {
		s.serveDone = nil
	}
	s.serveMu.Unlock()
	close(done)
}

// readLoop reads datagrams and forwards them to the record processor. It is
// unblocked from ReadFromUDP by Stop setting a past read deadline; stopCh then
// makes it exit promptly instead of spinning or leaking across restarts.
func (s *Server) readLoop(records chan<- rawRecord, stopCh <-chan bool) {
	for {
		select {
		case <-stopCh:
			return
		default:
		}

		buf := make([]byte, s.readBufferSize)
		n, addr, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-stopCh:
				return
			default:
			}

			if errors.Is(err, net.ErrClosed) {
				s.logger.Printf("udp connection closed, stopping read loop: %s", err)
				return
			}

			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				// The past deadline poked by Stop; re-check the stop signal
				// (it may win the race against the first timeout).
				select {
				case <-stopCh:
					return
				default:
				}
				continue
			}

			s.logger.Printf("error while reading from udp: %s", err)
			continue
		}

		rec := rawRecord{payload: buf[0:n], addr: addr}
		select {
		case <-stopCh:
			return
		case records <- rec:
		}
	}
}

// Stop stops the current Serve epoch. It is idempotent and safe to call even
// if Serve was never started. It waits for in-flight broadcasts so callers can
// rely on all writes having completed when Stop returns. Session state is
// cleared and signing secrets rotated, making every old session/cookie invalid.
func (s *Server) Stop() {
	s.serveMu.Lock()
	if s.conn != nil {
		if err := s.conn.SetReadDeadline(time.Unix(0, 1)); err != nil && s.logger != nil { // past deadline unblocks ReadFromUDP
			s.logger.Printf("error while setting stop read deadline: %s", err)
		}
	}
	if s.stop != nil {
		select {
		case s.stop <- true:
		default:
		}
	}
	done := s.serveDone
	s.serveMu.Unlock()

	// Wait for broadcasts while holding bcastMu: a concurrent Broadcast blocks
	// before Add until Wait has returned, so WaitGroup accounting stays safe.
	s.bcastMu.Lock()
	var wg *sync.WaitGroup
	if s.wg != nil {
		wg = s.wg
	}
	if wg != nil {
		wg.Wait()
	}
	s.bcastMu.Unlock()

	if done != nil {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			s.logger.Println("timeout while waiting for serve loop to stop")
		}
	}

	// Invalidate everything this epoch (and any earlier epoch) left behind.
	s.resetSessionState()
}

// handlerRecord validate & parse incoming bytes to a record instance, then process it depends on the record type
// all incoming bytes will ignore if it doesn't meet minimum payload size (to prevent process empty or wrong formatted records)
func (s *Server) handleRecord(record []byte, addr *net.UDPAddr) {
	if len(record) < s.minimumPayloadSize {
		s.logger.Println(ErrMinimumPayloadSizeLimit)
		return
	}

	r, err := parseRecord(record)
	if err != nil {
		s.logger.Printf("error while parsing record: %s", err)
		return
	}

	switch r.Type {
	case HandshakeClientHelloRecordType:
		s.handleHandshakeRecord(context.Background(), addr, r)
	case PingRecordType:
		s.handlePingRecord(context.Background(), addr, r)
	default:
		s.handleCustomRecord(context.Background(), addr, r)
	}
}

// handleHandshakeRecord handles handshake process
// HandshakeClientHello record is encrypted by the server public key & contains the client encryption key
// if the ClientHello was valid, the server generates a unique cookie for the client address, encrypt it with the client key & then send it
// client must send the HandshakeClientHelloVerify request (same as Hello) with the generated cookie & the token to prove that the sender address is valid
// server validate the HelloVerify record, then authenticate the client token & if they're valid, generate a session ID, encrypt it & send it back as ServerHello record
// after client registration, the client must prepend the Session ID before the record body unencrypted bytes, then encrypt them & compose the record
func (s *Server) handleHandshakeRecord(ctx context.Context, addr *net.UDPAddr, r *record) {
	var payload []byte
	payload, err := s.asymmCrypto.Decrypt(r.Body)
	if err != nil {
		s.logger.Printf("error while decrypting record body: %s", err)
		return
	}

	var handshake encoding.HandshakeRecord
	handshake, err = s.transcoder.UnmarshalHandshake(payload)
	if err != nil {
		s.logger.Printf("error while unmarshaling ClientHello record: %s", err)
		return
	}

	//TODO validate the client random

	if len(handshake.GetCookie()) == 0 {
		cookie := s.sessionManager.GetAddrCookieHMAC(addr, []byte(handshake.GetClientVersion()), handshake.GetSessionId(), handshake.GetRandom()) //TODO session id is empty

		if len(handshake.GetKey()) < insecureSymmKeySize {
			s.logger.Printf("error while parsing ClientHello record: %s", ErrInsecureEncryptionKeySize)
			return
		}

		serverHandshakeVerify := s.transcoder.NewHandshakeRecord()
		serverHandshakeVerify.SetCookie(cookie)
		serverHandshakeVerify.SetTimestamp(time.Now().UnixNano() / int64(time.Millisecond))

		var handshakePayload []byte
		handshakePayload, err = s.transcoder.MarshalHandshake(serverHandshakeVerify)
		if err != nil {
			s.logger.Printf("error while creating HelloVerify record: %s", err)
			return
		}

		handshakePayload, err = s.symmCrypto.Encrypt(handshakePayload, handshake.GetKey())
		if err != nil {
			s.logger.Printf("error while encrypting HelloVerify record: %s", err)
			return
		}

		handshakePayload = composeRecordBytes(HandshakeHelloVerifyRecordType, s.protocolVersion, handshakePayload)

		err = s.sendToAddr(addr, handshakePayload)
		if err != nil {
			s.logger.Printf("error while sending HelloVerify record to the client: %s", err)
			return
		}
	} else {
		cookie := s.sessionManager.GetAddrCookieHMAC(addr, []byte(handshake.GetClientVersion()), handshake.GetSessionId(), handshake.GetRandom()) //TODO session id is empty
		if !crypto.HMACEqual(handshake.GetCookie(), cookie) {
			s.logger.Printf("error while validation HelloVerify record cookie from %s: %s", addrKey(addr), ErrClientCookieIsInvalid)
			return
		}

		if len(handshake.GetKey()) < insecureSymmKeySize {
			s.logger.Printf("error while validating HelloVerify record key: %s", ErrInsecureEncryptionKeySize)
			return
		}

		// Idempotency key: same source address + same cookie + same client
		// random. A retransmission inside the short window must authenticate
		// at most once and must not register again or rotate the session.
		fingerprint := handshakeFingerprint(addr, handshake.GetCookie(), handshake.GetRandom())
		if existing, ok := s.findRecentHandshake(fingerprint); ok {
			s.logger.Printf("%s from %s: keeping existing session %s", ErrDuplicateHandshake, addrKey(addr), existing.ID)
			return
		}

		var token []byte
		if len(r.Extra) > 0 {
			token, err = s.symmCrypto.Decrypt(r.Extra, handshake.GetKey())
			if err != nil {
				s.logger.Printf("error while decrypting HelloVerify record token: %s", err)
				return
			}
		}

		var ID string
		ID, err = s.authClient.Authenticate(ctx, token)
		if err != nil {
			s.logger.Printf("error while authenticating client token from %s: %s", addrKey(addr), err)
			return
		}

		var cl *Client
		var duplicate bool
		cl, duplicate, err = s.registerAuthenticatedClient(addr, ID, handshake.GetKey(), fingerprint)
		if err != nil {
			s.logger.Printf("error while registering client: %s", err)
			return
		}
		if duplicate {
			// Lost a check-then-act race with an identical request that is
			// already registering: never send a second ServerHello.
			s.logger.Printf("%s (race) from %s: keeping existing session %s", ErrDuplicateHandshake, addrKey(addr), cl.ID)
			return
		}

		serverHandshakeHello := s.transcoder.NewHandshakeRecord()
		serverHandshakeHello.SetSessionId(cl.sessionID)
		serverHandshakeHello.SetTimestamp(time.Now().UnixNano() / int64(time.Millisecond))

		var handshakePayload []byte
		handshakePayload, err = s.transcoder.MarshalHandshake(serverHandshakeHello)
		if err != nil {
			s.logger.Printf("error while marshaling server hello record: %s", err)
			return
		}

		err = s.sendToClient(cl, HandshakeServerHelloRecordType, handshakePayload)
		if err != nil {
			s.logger.Printf("error while sending server hello record: %s", err)
			return
		}
	}
}

// handlePingRecord handles ping record and sends pong response
func (s *Server) handlePingRecord(ctx context.Context, addr *net.UDPAddr, r *record) {
	cl, ok := s.findClientByAddr(addr)
	if !ok {
		s.logger.Printf("error while authenticating ping record from %s: %s", addrKey(addr), ErrClientAddressIsNotRegistered)
		s.unAuthenticated(addr)
		return
	}

	if s.expireClientIfNeeded(cl) {
		s.logger.Printf("rejecting expired ping session for %s (%s): %s", addrKey(addr), cl.ID, ErrClientSessionExpired)
		s.unAuthenticated(addr)
		return
	}

	pong := s.transcoder.NewPongRecord()
	pong.SetReceivedAt(time.Now().UnixNano())

	var payload []byte
	var err error
	payload, err = s.symmCrypto.Decrypt(r.Body, cl.eKey)
	if err != nil {
		s.logger.Printf("error while decrypting ping record: %s", err)
		return
	}

	var sessionID, body []byte
	sessionID, body, err = parseSessionID(payload, len(cl.sessionID))
	if err != nil {
		s.logger.Printf("error while parsing session id for ping: %s", err)
		return
	}

	if !cl.ValidateSessionID(sessionID) {
		s.logger.Printf("error while validating session id for ping: %s", ErrClientSessionNotFound)
		s.unAuthenticated(addr)
		return
	}

	var ping encoding.PingRecord
	ping, err = s.transcoder.UnmarshalPing(body)
	if err != nil {
		s.logger.Printf("error while unmarshaling ping record: %s", err)
		return
	}

	pong.SetPingSentAt(ping.GetSentAt())
	pong.SetSentAt(time.Now().UnixNano())

	var pongPayload []byte
	pongPayload, err = s.transcoder.MarshalPong(pong)
	if err != nil {
		s.logger.Printf("error while marshaling pong record: %s", err)
		return
	}

	err = s.sendToClient(cl, PongRecordType, pongPayload)
	if err != nil {
		s.logger.Printf("error while sending pong record: %s", err)
		return
	}

	cl.touchHeartbeat()
}

// handleCustomRecord handle custom record with authorizing the record and call the handler func if is set
func (s *Server) handleCustomRecord(ctx context.Context, addr *net.UDPAddr, r *record) {
	cl, ok := s.findClientByAddr(addr)
	if !ok {
		s.logger.Printf("error while authenticating other type record from %s: %s", addrKey(addr), ErrClientAddressIsNotRegistered)
		s.unAuthenticated(addr)
		return
	}

	// Enforce expiration on the request path as well, so an expired session is
	// rejected even before the garbage collector happens to run.
	if s.expireClientIfNeeded(cl) {
		s.logger.Printf("rejecting expired client session for %s (%s): %s", addrKey(addr), cl.ID, ErrClientSessionExpired)
		s.unAuthenticated(addr)
		return
	}

	payload, err := s.symmCrypto.Decrypt(r.Body, cl.eKey)
	if err != nil {
		s.logger.Printf("error while decrypting other type record from %s: %s", addrKey(addr), err)
		return
	}

	var sessionID, body []byte
	sessionID, body, err = parseSessionID(payload, len(cl.sessionID))
	if err != nil {
		s.logger.Printf("error while parsing session id for custom record from %s: %s", addrKey(addr), err)
		return
	}

	if !cl.ValidateSessionID(sessionID) {
		s.logger.Printf("error while validating client session for other type record from %s: %s", addrKey(addr), ErrClientSessionNotFound)
		s.unAuthenticated(addr)
		return
	}

	// Re-check expiration after decryption/validation: the heartbeat may have
	// gone stale while the record was being processed.
	if s.expireClientIfNeeded(cl) {
		s.logger.Printf("rejecting stale client session before handler for %s (%s): %s", addrKey(addr), cl.ID, ErrClientSessionExpired)
		s.unAuthenticated(addr)
		return
	}

	if handler := s.getHandler(); handler != nil {
		handler(cl.ID, r.Type, body)
	}

	cl.touchHeartbeat()
}

// parseSessionID parses the session ID from the record decrypted body, the session ID must prepend to the body before encryption in the client
func parseSessionID(p []byte, sLen int) ([]byte, []byte, error) {
	if len(p) < sLen {
		return nil, nil, ErrInvalidPayloadBodySize
	}
	return p[:sLen], p[sLen:], nil
}

// sendToAddr writes record bytes to the UDP address
func (s *Server) sendToAddr(addr *net.UDPAddr, record []byte) error {
	_, err := s.conn.WriteToUDP(record, addr)
	return err
}

// sends a record byte array to the Client. the record type is prepended to the record body as a byte
func (s *Server) sendToClient(client *Client, typ byte, payload []byte) error {
	payload, err := s.symmCrypto.Encrypt(payload, client.eKey)
	if err != nil {
		return err
	}
	payload = composeRecordBytes(typ, s.protocolVersion, payload)
	return s.sendToAddr(client.addr, payload)
}

// SendToClientByID sends bytes to the Client by ID.
// It returns ErrClientNotFound when the ID is not registered or the client has
// already been removed by expiration cleanup.
func (s *Server) SendToClientByID(clientID string, typ byte, payload []byte) error {
	cl, ok := s.findClientByID(clientID)
	if !ok {
		s.logger.Printf("rejecting send to unknown client id %q: %s", clientID, ErrClientNotFound)
		return ErrClientNotFound
	}

	cl.Lock()
	defer cl.Unlock()
	if cl.removed {
		s.logger.Printf("rejecting send to removed client id %q: %s", clientID, ErrClientNotFound)
		return ErrClientNotFound
	}

	return s.sendToClient(cl, typ, payload)
}

// clientGarbageCollection keeps the historical parameterless entry point. It
// reads the epoch channels under serveMu so it cannot race with a restart.
func (s *Server) clientGarbageCollection() {
	s.serveMu.Lock()
	stopCh := s.garbageCollectionStop
	ticker := s.garbageCollectionTicker
	s.serveMu.Unlock()
	s.garbageCollectionLoop(stopCh, ticker)
}

// garbageCollectionLoop periodically removes heartbeat-expired clients.
// Dependencies are passed explicitly to stay stable across Serve/Stop cycles.
func (s *Server) garbageCollectionLoop(stopCh <-chan bool, ticker *time.Ticker) {
	if ticker == nil {
		return
	}

	for {
		select {
		case <-stopCh:
			ticker.Stop()
			return
		case <-ticker.C:
			s.collectExpiredClients()
		}
	}
}

// collectExpiredClients takes a stable snapshot and removes every client whose
// heartbeat has expired. All map/index mutations go through removeClient, so
// this is safe to run concurrently with broadcasts and record handling.
func (s *Server) collectExpiredClients() {
	if s.heartbeatExpiration <= 0 {
		return
	}

	for _, c := range s.snapshotClients() {
		if !s.reapExpiredClient(c) {
			continue
		}
		s.logger.Printf("garbage collector removed expired client id=%s addr=%s: %s", c.ID, addrKey(c.addr), ErrClientSessionExpired)
	}
}

// BroadcastToClients broadcasts bytes to all registered Clients.
//
// The client set is snapshotted under the registry lock, so expiration cleanup
// never races with map iteration. Each send holds the client's own lock for
// the removed-check + write: a client that cleanup has marked removed is never
// written to, while a send that started first completes before removal.
func (s *Server) BroadcastToClients(typ byte, payload []byte) {
	clients := s.snapshotClients()
	for _, cl := range clients {
		s.bcastMu.Lock()
		wg := s.wg
		if wg == nil {
			wg = &sync.WaitGroup{}
			s.wg = wg
		}
		wg.Add(1)
		s.bcastMu.Unlock()

		go func(c *Client) {
			defer wg.Done()

			c.Lock()
			defer c.Unlock()
			if c.removed {
				return
			}
			if err := s.sendToClient(c, typ, payload); err != nil {
				s.logger.Printf("error while writing broadcast to the client %s: %s", c.ID, err)
			}
		}(cl)
	}
}

func (s *Server) unAuthenticated(addr *net.UDPAddr) {
	payload := composeRecordBytes(UnAuthenticated, s.protocolVersion, []byte{})
	err := s.sendToAddr(addr, payload)
	if err != nil {
		s.logger.Printf("error while sending UnAuthenticated record to the client: %s", err)
		return
	}
}

// composeRecordBytes composes record bytes, prepend the record header (type & protocol version) to the body
func composeRecordBytes(typ byte, version [2]byte, payload []byte) []byte {
	return append([]byte{typ, version[0], version[1]}, payload...)
}

// parseRecord parses received bytes to the record struct
func parseRecord(rec []byte) (*record, error) {
	if rec[0] != HandshakeClientHelloRecordType {
		if len(rec) < 3 {
			return nil, ErrInvalidRecordType
		}

		return &record{
			Type:       rec[0],
			ProtoMajor: rec[1],
			ProtoMinor: rec[2],
			Body:       rec[3:],
		}, nil
	}

	if len(rec) < 5 {
		return nil, ErrInvalidRecordType
	}

	bodySize := 256*int(rec[3]) + int(rec[4])
	if bodySize < 0 || bodySize+5 > len(rec) {
		return nil, ErrInvalidPayloadBodySize
	}

	return &record{
		Type:       rec[0],
		ProtoMajor: rec[1],
		ProtoMinor: rec[2],
		Body:       rec[5 : bodySize+5],
		Extra:      rec[bodySize+5:],
	}, nil
}

// ValidateSessionID compares the client session ID with the given one
func (c *Client) ValidateSessionID(sessionID []byte) bool {
	if bytes.Equal(c.sessionID, sessionID) {
		return true
	}
	return false
}

// WithProtocolVersion sets the server protocol version
func WithProtocolVersion(major, minor uint8) Option {
	return func(s *Server) {
		s.protocolVersion = [2]byte{major, minor}
	}
}

// WithHeartbeatExpiration sets the server heartbeat expiration option
func WithHeartbeatExpiration(t time.Duration) Option {
	return func(s *Server) {
		s.heartbeatExpiration = t
	}
}

// WithMinimumPayloadSize sets the minimum payload size option
func WithMinimumPayloadSize(i int) Option {
	return func(s *Server) {
		s.minimumPayloadSize = i
	}
}

// WithReadBufferSize sets the read buffer size option
func WithReadBufferSize(i int) Option {
	return func(s *Server) {
		s.readBufferSize = i
	}
}

// WithSymmetricCrypto sets the symmetric cryptography implementation
func WithSymmetricCrypto(sc crypto.Symmetric) Option {
	return func(s *Server) {
		s.symmCrypto = sc
	}
}

// WithAsymmetricCrypto sets the asymmetric cryptography implementation
func WithAsymmetricCrypto(ac crypto.Asymmetric) Option {
	return func(s *Server) {
		s.asymmCrypto = ac
	}
}

// WithTranscoder sets the transcoder implementation
func WithTranscoder(t encoding.Transcoder) Option {
	return func(s *Server) {
		s.transcoder = t
	}
}

// WithAuthClient sets the auth client implementation
func WithAuthClient(ac AuthClient) Option {
	return func(s *Server) {
		s.authClient = ac
	}
}

// WithLogger sets the logger
func WithLogger(l *log.Logger) Option {
	return func(s *Server) {
		s.logger = l
	}
}

// WithHandshakeIdempotencyTTL sets the short window in which identical
// cookie-bearing handshakes (same address, cookie & random) are deduplicated.
func WithHandshakeIdempotencyTTL(d time.Duration) Option {
	return func(s *Server) {
		s.handshakeTTL = d
	}
}
