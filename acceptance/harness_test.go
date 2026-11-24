//go:build acceptance
// +build acceptance

// Package acceptance_test drives the udpsocket server over real loopback UDP
// connections. It is intentionally self-contained (random ports, unique
// identifiers) and lives in a separate package so it can never collide with
// grading tests added to the root package.
//
// It is gated behind the "acceptance" build tag so a default `go test ./...`
// does not run this deliberately heavy suite; run it with:
//
//	go test -tags acceptance -race ./acceptance/
package acceptance_test

import (
	"bytes"
	"context"
	"crypto/rsa"
	"io"
	"log"
	"net"
	"sync"
	"testing"
	"time"

	udpsocket "github.com/theredrad/udpsocket"
	"github.com/theredrad/udpsocket/crypto"
	"github.com/theredrad/udpsocket/encoding/pb"
)

const accRecordType byte = 13

var (
	accAESKey = []byte{113, 110, 25, 53, 11, 53, 68, 33, 17, 36, 22, 7, 125, 11, 35, 16, 83, 61, 59, 49, 31, 22, 69, 17, 24, 125, 11, 35, 16, 83, 61, 59}
	accRandom = []byte{12, 101, 30, 21, 8, 45, 86, 10, 125, 9, 49, 31, 22, 69, 36, 22, 7, 12, 83, 61, 5, 17, 24, 125, 11, 35, 15, 11, 35, 16, 6, 71}
)

// syncBuffer is a race-safe bytes buffer for log capture.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

var _ io.Writer = (*syncBuffer)(nil)

// countingAuth is a configurable AuthClient that counts Authenticate calls.
type countingAuth struct {
	mu    sync.Mutex
	calls int
}

func (a *countingAuth) Authenticate(_ context.Context, token []byte) (string, error) {
	a.mu.Lock()
	a.calls++
	a.mu.Unlock()
	return "uid-" + string(token), nil
}

func (a *countingAuth) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

type callback struct {
	id  string
	typ byte
	p   []byte
}

type accEnv struct {
	t      *testing.T
	server *udpsocket.Server
	conn   *net.UDPConn
	addr   *net.UDPAddr
	auth   *countingAuth
	logs   *syncBuffer
	pk     *rsa.PrivateKey
	cbCh   chan callback
}

func startEnv(t *testing.T, expiration time.Duration, opts ...udpsocket.Option) *accEnv {
	t.Helper()

	serverConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen server udp: %v", err)
	}

	pk, err := crypto.GenerateRSAKey(2048)
	if err != nil {
		serverConn.Close()
		t.Fatalf("generate rsa: %v", err)
	}

	auth := &countingAuth{}
	logBuf := &syncBuffer{}
	logger := log.New(logBuf, "acc ", 0)

	base := []udpsocket.Option{
		udpsocket.WithAuthClient(auth),
		udpsocket.WithTranscoder(&pb.Protobuf{}),
		udpsocket.WithSymmetricCrypto(crypto.NewAES(crypto.AES_CBC)),
		udpsocket.WithAsymmetricCrypto(crypto.NewRSAFromPK(pk)),
		udpsocket.WithReadBufferSize(4096),
		udpsocket.WithMinimumPayloadSize(3),
		udpsocket.WithProtocolVersion(1, 0),
		udpsocket.WithLogger(logger),
	}
	if expiration > 0 {
		base = append(base, udpsocket.WithHeartbeatExpiration(expiration))
	}

	srv, err := udpsocket.NewServer(serverConn, append(base, opts...)...)
	if err != nil {
		serverConn.Close()
		t.Fatalf("new server: %v", err)
	}

	env := &accEnv{
		t:      t,
		server: srv,
		conn:   serverConn,
		addr:   serverConn.LocalAddr().(*net.UDPAddr),
		auth:   auth,
		logs:   logBuf,
		pk:     pk,
		cbCh:   make(chan callback, 4096),
	}
	srv.SetHandler(func(id string, typ byte, p []byte) {
		cp := append([]byte(nil), p...)
		select {
		case env.cbCh <- callback{id: id, typ: typ, p: cp}:
		default:
		}
	})

	go srv.Serve()
	time.Sleep(120 * time.Millisecond) // let the Serve epoch start on slow/-race runs

	t.Cleanup(func() {
		srv.Stop()
		serverConn.Close()
	})

	return env
}

// newClient creates a real loopback UDP client with its own ephemeral address.
func (e *accEnv) newClient(token string) *accClient {
	e.t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		e.t.Fatalf("listen client udp: %v", err)
	}
	e.t.Cleanup(func() { conn.Close() })

	return &accClient{
		t:      e.t,
		conn:   conn,
		srv:    e.addr,
		token:  token,
		key:    append([]byte(nil), accAESKey...),
		random: append([]byte(nil), accRandom...),
		rsa:    crypto.NewRSAEncryptorFromPK(&e.pk.PublicKey),
		sym:    crypto.NewAES(crypto.AES_CBC),
		tr:     &pb.Protobuf{},
	}
}

type accClient struct {
	t         *testing.T
	conn      *net.UDPConn
	srv       *net.UDPAddr
	token     string
	key       []byte
	random    []byte
	rsa       *crypto.RSAEncryptor
	sym       crypto.Symmetric
	tr        *pb.Protobuf
	sessionID []byte
}

func (c *accClient) localAddr() *net.UDPAddr {
	return c.conn.LocalAddr().(*net.UDPAddr)
}

// frameHandshake RSA-encrypts and frames a ClientHello record.
func (c *accClient) buildHandshake(cookie []byte) []byte {
	h := &pb.Handshake{
		Key:           c.key,
		Random:        c.random,
		ClientVersion: "0.1",
		Cookie:        cookie,
	}
	p, err := c.tr.MarshalHandshake(h)
	if err != nil {
		c.t.Fatalf("marshal handshake: %v", err)
	}
	enc, err := c.rsa.Encrypt(p)
	if err != nil {
		c.t.Fatalf("rsa encrypt: %v", err)
	}
	l := len(enc)
	return append([]byte{udpsocket.HandshakeClientHelloRecordType, 1, 0, byte(l / 256), byte(l % 256)}, enc...)
}

// sendRaw writes a raw datagram to the server.
func (c *accClient) sendRaw(b []byte) {
	c.t.Helper()
	if _, err := c.conn.WriteToUDP(b, c.srv); err != nil {
		c.t.Fatalf("write udp: %v", err)
	}
}

// sendHello sends a ClientHello; when cookie is non-empty it also appends the
// AES-encrypted token extra. It returns the exact wire datagram sent.
func (c *accClient) sendHello(cookie []byte, withToken bool) []byte {
	frame := c.buildHandshake(cookie)
	if cookie != nil && withToken {
		encToken, err := c.sym.Encrypt([]byte(c.token), c.key)
		if err != nil {
			c.t.Fatalf("encrypt token: %v", err)
		}
		frame = append(frame, encToken...)
	}
	c.sendRaw(frame)
	return frame
}

type accPacket struct {
	Type      byte
	Plain     []byte
	Raw       []byte
	AuthError error
}

// readPacket reads one UDP packet. Encrypted bodies are AES-decrypted; an
// UnAuthenticated notification has an empty unencrypted body.
func (c *accClient) readPacket(timeout time.Duration) (accPacket, error) {
	if err := c.conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return accPacket{}, err
	}
	buf := make([]byte, 8192)
	n, _, err := c.conn.ReadFromUDP(buf)
	if err != nil {
		return accPacket{}, err
	}
	raw := append([]byte(nil), buf[:n]...)
	pkt := accPacket{Type: raw[0], Raw: raw}
	if pkt.Type == udpsocket.UnAuthenticated {
		return pkt, nil
	}
	if n >= 3 {
		d, derr := c.sym.Decrypt(raw[3:], c.key)
		if derr != nil {
			pkt.AuthError = derr
			return pkt, derr
		}
		pkt.Plain = d
	}
	return pkt, nil
}

// obtainCookie performs the cookie-less ClientHello / HelloVerify round trip.
func (c *accClient) obtainCookie() []byte {
	c.t.Helper()
	c.sendHello(nil, false)
	pkt, err := c.readPacket(2 * time.Second)
	if err != nil {
		c.t.Fatalf("read HelloVerify: %v", err)
	}
	if pkt.Type != udpsocket.HandshakeHelloVerifyRecordType {
		c.t.Fatalf("want HelloVerify type %d, got %d", udpsocket.HandshakeHelloVerifyRecordType, pkt.Type)
	}
	h, err := c.tr.UnmarshalHandshake(pkt.Plain)
	if err != nil {
		c.t.Fatalf("unmarshal HelloVerify: %v", err)
	}
	if len(h.GetCookie()) == 0 {
		c.t.Fatal("empty cookie")
	}
	return h.GetCookie()
}

// completeHandshake runs the full 1.5-RTT handshake and stores the session ID.
func (c *accClient) completeHandshake() []byte {
	c.t.Helper()
	cookie := c.obtainCookie()
	c.sendHello(cookie, true)
	pkt, err := c.readPacket(2 * time.Second)
	if err != nil {
		c.t.Fatalf("read ServerHello: %v", err)
	}
	if pkt.Type != udpsocket.HandshakeServerHelloRecordType {
		c.t.Fatalf("want ServerHello type %d, got %d", udpsocket.HandshakeServerHelloRecordType, pkt.Type)
	}
	h, err := c.tr.UnmarshalHandshake(pkt.Plain)
	if err != nil {
		c.t.Fatalf("unmarshal ServerHello: %v", err)
	}
	if len(h.GetSessionId()) == 0 {
		c.t.Fatal("empty session id")
	}
	c.sessionID = h.GetSessionId()
	return c.sessionID
}

// sendRecord sends an encrypted business/ping record carrying the session ID.
func (c *accClient) sendRecord(typ byte, sessionID, payload []byte) {
	c.t.Helper()
	if err := c.postRecord(typ, sessionID, payload); err != nil {
		c.t.Fatalf("write record: %v", err)
	}
}

// postRecord is the goroutine-safe, non-fatal variant of sendRecord.
func (c *accClient) postRecord(typ byte, sessionID, payload []byte) error {
	plain := make([]byte, 0, len(sessionID)+len(payload))
	plain = append(plain, sessionID...)
	plain = append(plain, payload...)
	enc, err := c.sym.Encrypt(plain, c.key)
	if err != nil {
		return err
	}
	frame := append([]byte{typ, 1, 0}, enc...)
	_, err = c.conn.WriteToUDP(frame, c.srv)
	return err
}

// collectPackets reads every packet arriving during window.
func (c *accClient) collectPackets(window time.Duration) []accPacket {
	deadline := time.Now().Add(window)
	var out []accPacket
	for time.Now().Before(deadline) {
		pkt, err := c.readPacket(100 * time.Millisecond)
		if err == nil {
			out = append(out, pkt)
		}
	}
	return out
}

// waitCallback waits for the handler callback and validates identity/type.
func (e *accEnv) waitCallback(timeout time.Duration) callback {
	e.t.Helper()
	select {
	case cb := <-e.cbCh:
		return cb
	case <-time.After(timeout):
		e.t.Fatalf("timed out waiting for handler callback")
		return callback{}
	}
}

// assertNoCallback fails if a callback arrives within the window.
func (e *accEnv) assertNoCallback(window time.Duration) {
	e.t.Helper()
	select {
	case cb := <-e.cbCh:
		e.t.Fatalf("unexpected callback: id=%s type=%d", cb.id, cb.typ)
	case <-time.After(window):
	}
}

// drainPackets reads and discards packets until the window elapses.
func (c *accClient) drainPackets(window time.Duration) {
	_ = c.conn.SetReadDeadline(time.Now().Add(window))
	buf := make([]byte, 8192)
	for {
		if _, _, err := c.conn.ReadFromUDP(buf); err != nil {
			return
		}
	}
}
