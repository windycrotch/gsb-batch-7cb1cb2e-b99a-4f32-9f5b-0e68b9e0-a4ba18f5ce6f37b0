package srt

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gosrt "github.com/datarhei/gosrt"
	"github.com/datarhei/gosrt/packet"
	"github.com/voc/srtrelay/mpegts"
	"github.com/voc/srtrelay/relay"
	"github.com/voc/srtrelay/stream"
)

// fakeConn is an in-memory gosrt.Conn replacement. Reads are served from a
// channel of chunks and writes are recorded, so a test can play both ends of a
// relayed stream without any real sockets.
type fakeConn struct {
	streamID string
	readCh   chan []byte
	done     chan struct{}
	doneOnce sync.Once
	read     atomic.Int64
	writeMu  sync.Mutex
	written  bytes.Buffer
	closed   bool
}

func newFakeConn(t *testing.T, name string, mode stream.Mode) *fakeConn {
	t.Helper()
	id, err := stream.NewStreamID(name, "", mode)
	if err != nil {
		t.Fatal(err)
	}
	return &fakeConn{
		streamID: id.String(),
		readCh:   make(chan []byte, 64),
		done:     make(chan struct{}),
	}
}

// disconnect ends the input side, emulating the peer going away. It is safe to
// call concurrently with send.
func (c *fakeConn) disconnect() {
	c.doneOnce.Do(func() { close(c.done) })
}

// send queues a chunk for the connection to read. It reports false when the
// peer has already disconnected.
func (c *fakeConn) send(chunk []byte) bool {
	select {
	case c.readCh <- chunk:
		return true
	case <-c.done:
		return false
	}
}

// Read returns queued chunks and reports io.EOF once the peer disconnected and
// the queue is drained. Every served chunk increments the read counter, which
// lets tests order publisher progress against other events.
func (c *fakeConn) Read(b []byte) (int, error) {
	chunk, err := c.nextChunk()
	if err != nil {
		return 0, err
	}
	c.read.Add(1)
	return copy(b, chunk), nil
}

func (c *fakeConn) nextChunk() ([]byte, error) {
	select {
	case chunk := <-c.readCh:
		return chunk, nil
	default:
	}
	select {
	case chunk := <-c.readCh:
		return chunk, nil
	case <-c.done:
		return nil, io.EOF
	}
}

// Reads returns the number of chunks the reader has consumed.
func (c *fakeConn) Reads() int64 {
	return c.read.Load()
}

func (c *fakeConn) ReadPacket() (packet.Packet, error) {
	return nil, io.EOF
}

func (c *fakeConn) Write(b []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.closed {
		return 0, io.ErrClosedPipe
	}
	c.written.Write(b)
	return len(b), nil
}

func (c *fakeConn) WritePacket(packet.Packet) error {
	return nil
}

func (c *fakeConn) Close() error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.closed = true
	return nil
}

func (c *fakeConn) LocalAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}
}

func (c *fakeConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 2}
}

func (c *fakeConn) SetDeadline(time.Time) error      { return nil }
func (c *fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (c *fakeConn) SetWriteDeadline(time.Time) error { return nil }
func (c *fakeConn) SocketId() uint32                 { return 1 }
func (c *fakeConn) PeerSocketId() uint32             { return 2 }
func (c *fakeConn) StreamId() string                 { return c.streamID }
func (c *fakeConn) Version() uint32                  { return 5 }
func (c *fakeConn) Stats(*gosrt.Statistics)          {}

func (c *fakeConn) Bytes() []byte {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return append([]byte(nil), c.written.Bytes()...)
}

func (c *fakeConn) Len() int {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.written.Len()
}

func (c *fakeConn) isClosed() bool {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.closed
}

var _ gosrt.Conn = (*fakeConn)(nil)

func newTestServer(t *testing.T, relayConfig relay.RelayConfig) *Server {
	t.Helper()
	return NewServer(&Config{
		Server: ServerConfig{PacketSize: 1316},
		Relay:  relayConfig,
	})
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// handleAsync runs the full connection handling path in the background and
// reports when it returned.
func handleAsync(s *Server, conn *fakeConn) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Handle(context.Background(), conn)
	}()
	return done
}

// connSession is one tracked connection of the in-memory relay: the fake
// socket, the handler completion signal and the stream mode.
type connSession struct {
	conn *fakeConn
	done <-chan struct{}
}

// session tracks the connections of a test and shuts them down in the correct
// order: publishers first (so subscribers are released), then waits for every
// handler to return.
type session struct {
	t       *testing.T
	s       *Server
	pubs    []connSession
	plays   []connSession
	cleaned bool
}

func newSession(t *testing.T, s *Server) *session {
	sess := &session{t: t, s: s}
	t.Cleanup(sess.shutdown)
	return sess
}

// publish starts a publisher and waits until it has claimed the name.
func (sess *session) publish(name string) connSession {
	sess.t.Helper()
	conn := newFakeConn(sess.t, name, stream.ModePublish)
	cs := connSession{conn: conn, done: handleAsync(sess.s, conn)}
	if !waitForStream(sess.t, sess.s, name) {
		sess.t.Fatalf("publisher did not claim stream %q", name)
	}
	sess.pubs = append(sess.pubs, cs)
	return cs
}

// play starts a subscriber for an already published name and waits until it is
// registered as a client.
func (sess *session) play(name string, expectedClients int) connSession {
	sess.t.Helper()
	conn := newFakeConn(sess.t, name, stream.ModePlay)
	cs := connSession{conn: conn, done: handleAsync(sess.s, conn)}
	sess.plays = append(sess.plays, cs)
	waitClients(sess.t, sess.s, name, expectedClients)
	return cs
}

// disconnect stops a publisher and waits for its handler to return.
func (sess *session) disconnect(cs connSession) {
	sess.t.Helper()
	cs.conn.disconnect()
	waitHandle(sess.t, cs.done, "publisher")
}

// shutdown disconnects all publishers, then waits for all connections.
func (sess *session) shutdown() {
	if sess.cleaned {
		return
	}
	sess.cleaned = true
	for _, cs := range sess.pubs {
		cs.conn.disconnect()
	}
	for _, cs := range sess.pubs {
		waitHandle(sess.t, cs.done, "publisher")
	}
	for _, cs := range sess.plays {
		waitHandle(sess.t, cs.done, "subscriber")
	}
}

// waitHandle waits for a connection handler to return.
func waitHandle(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("timeout waiting for %s to finish", what)
	}
}

// waitClients blocks until the relay reports the given number of clients for
// stream name.
func waitClients(t *testing.T, s *Server, name string, clients int) {
	t.Helper()
	waitUntil(t, "expected client count", func() bool {
		stats := s.relay.GetStatistics()
		return len(stats) == 1 && stats[0].Name == name && stats[0].Clients == clients
	})
}

// TestServer_PlayEndsWhenPublisherDisconnects covers the disconnect/read race:
// a subscriber that is blocked in Read has to be released as soon as the
// publisher disconnects, and its connection has to be closed.
func TestServer_PlayEndsWhenPublisherDisconnects(t *testing.T) {
	s := newTestServer(t, relay.RelayConfig{BufferSize: 64 * 1316, PacketSize: 1316})
	sess := newSession(t, s)

	pub := sess.publish("test")
	play := sess.play("test", 1)

	// Send data and make sure it arrives: the normal path still works.
	pub.conn.send([]byte{1, 2, 3, 4})
	waitUntil(t, "data to reach subscriber", func() bool { return play.conn.Len() == 4 })

	// Publisher disconnects (Read returns EOF).
	sess.disconnect(pub)

	// The subscriber must terminate on its own, without any further input.
	waitHandle(t, play.done, "subscriber connection")
	if !play.conn.isClosed() {
		t.Error("subscriber connection was not closed")
	}

	// Stream table and buffer pool must be clean right after the publisher
	// returned, without relying on background cleanup.
	if s.relay.ChannelExists("test") {
		t.Error("stream name still registered after publisher disconnect")
	}
	if stats := s.GetStatistics(); len(stats) != 0 {
		t.Errorf("statistics report %d streams after disconnect", len(stats))
	}
	if got := s.relay.BuffersInUse(); got != 0 {
		t.Errorf("%d buffers still in use after disconnect", got)
	}
}

// TestServer_RepublishAfterDisconnect covers the takeover path at the SRT
// level: the same name is published again right after the old publisher
// disconnected, and only the new epoch's data reaches a new subscriber.
func TestServer_RepublishAfterDisconnect(t *testing.T) {
	s := newTestServer(t, relay.RelayConfig{BufferSize: 64 * 1316, PacketSize: 1316})
	sess := newSession(t, s)

	// First publisher and an old subscriber.
	oldPub := sess.publish("test")
	oldPlay := sess.play("test", 1)

	oldPub.conn.send([]byte{0xAA, 0xAA, 0xAA, 0xAA})
	waitUntil(t, "old subscriber data", func() bool { return oldPlay.conn.Len() == 4 })

	// Old publisher disconnects; the old subscriber ends with it.
	sess.disconnect(oldPub)
	waitHandle(t, oldPlay.done, "old subscriber")
	if got := oldPlay.conn.Bytes(); !bytes.Equal(got, []byte{0xAA, 0xAA, 0xAA, 0xAA}) {
		t.Errorf("old subscriber saw foreign data: %x", got)
	}

	// Immediate republish without any wait: the name must be free.
	newPub := sess.publish("test")
	newPlay := sess.play("test", 1)

	newPub.conn.send([]byte{0xBB, 0xBB, 0xBB, 0xBB})
	waitUntil(t, "new subscriber data", func() bool { return newPlay.conn.Len() == 4 })

	if got := newPlay.conn.Bytes(); !bytes.Equal(got, []byte{0xBB, 0xBB, 0xBB, 0xBB}) {
		t.Errorf("new subscriber got %x, want new epoch data", got)
	}
	if stats := s.GetStatistics(); len(stats) != 1 || stats[0].Clients != 1 {
		t.Errorf("unexpected statistics after republish: %+v", stats)
	}
}

// TestServer_DuplicatePublisherRejected checks that a second publisher for a
// live stream is rejected instead of silently hijacking the stream.
func TestServer_DuplicatePublisherRejected(t *testing.T) {
	s := newTestServer(t, relay.RelayConfig{BufferSize: 64 * 1316, PacketSize: 1316})
	sess := newSession(t, s)
	sess.publish("test")

	second := newFakeConn(t, "test", stream.ModePublish)
	waitHandle(t, handleAsync(s, second), "duplicate publisher")
	if !second.isClosed() {
		t.Error("duplicate publisher connection was not closed")
	}
	if got := len(s.GetStatistics()); got != 1 {
		t.Errorf("duplicate publisher changed the stream table: %d streams", got)
	}
}

// TestServer_StatisticsReflectLiveStream verifies the /streams view for the
// regular path: one stream, accurate client count, stable across churn.
func TestServer_StatisticsReflectLiveStream(t *testing.T) {
	s := newTestServer(t, relay.RelayConfig{BufferSize: 64 * 1316, PacketSize: 1316})
	sess := newSession(t, s)

	pub := sess.publish("live")

	stats := s.GetStatistics()
	if len(stats) != 1 || stats[0].Name != "live" || stats[0].Clients != 0 {
		t.Fatalf("unexpected statistics: %+v", stats)
	}
	if stats[0].URL == "" {
		t.Error("stream statistics should contain a play URL")
	}

	plays := make([]connSession, 0, 3)
	for i := 1; i <= 3; i++ {
		plays = append(plays, sess.play("live", i))
		if got := len(s.GetStatistics()); got != 1 {
			t.Fatalf("expected 1 stream, got %d", got)
		}
	}

	// Disconnecting the publisher has to end all subscribers and clear the
	// stream table.
	sess.disconnect(pub)
	for _, play := range plays {
		waitHandle(t, play.done, "subscriber")
	}
	if stats := s.GetStatistics(); len(stats) != 0 {
		t.Errorf("statistics still report streams: %+v", stats)
	}
	if got := s.relay.BuffersInUse(); got != 0 {
		t.Errorf("%d buffers still in use after shutdown", got)
	}
}

// mpegTsFixture loads the MPEG-TS sample from the mpegts package.
func mpegTsFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("../mpegts/h264.ts")
	if err != nil {
		t.Fatalf("cannot read MPEG-TS fixture: %v", err)
	}
	if len(data)%mpegts.PacketLen != 0 {
		t.Fatalf("fixture is not a whole number of MPEG-TS packets: %d", len(data))
	}
	return data
}

// feedPackets writes data to a connection in MPEG-TS packet sized chunks.
func feedPackets(t *testing.T, conn *fakeConn, data []byte) {
	t.Helper()
	for offset := 0; offset < len(data); offset += mpegts.PacketLen {
		if !conn.send(data[offset : offset+mpegts.PacketLen]) {
			t.Fatalf("connection disconnected while feeding MPEG-TS at offset %d", offset)
		}
	}
}

// TestServer_PublishPlayMpegTs verifies the normal push/pull path with real
// MPEG-TS data: a subscriber receives every packet the publisher sent, and the
// received stream is parseable and therefore decodable.
func TestServer_PublishPlayMpegTs(t *testing.T) {
	data := mpegTsFixture(t)
	s := newTestServer(t, relay.RelayConfig{BufferSize: 512 * mpegts.PacketLen, PacketSize: mpegts.PacketLen})
	sess := newSession(t, s)

	pub := sess.publish("ts")
	play := sess.play("ts", 1)

	feedPackets(t, pub.conn, data)
	waitUntil(t, "all MPEG-TS packets to arrive", func() bool { return play.conn.Len() == len(data) })

	if got := play.conn.Bytes(); !bytes.Equal(got, data) {
		t.Fatal("relayed MPEG-TS stream differs from what the publisher sent")
	}
	if stats := s.GetStatistics(); len(stats) != 1 || stats[0].Clients != 1 {
		t.Fatalf("unexpected statistics: %+v", stats)
	}

	// The relayed bytes must still parse as MPEG-TS with complete decoder init.
	parser := mpegts.NewParser()
	if err := parser.Parse(play.conn.Bytes()); err != nil {
		t.Fatalf("relayed stream is not parseable: %v", err)
	}
	init, err := parser.InitData()
	if err != nil {
		t.Fatalf("relayed stream init: %v", err)
	}
	if len(init) == 0 {
		t.Fatal("relayed stream does not contain decoder initialization data")
	}
}

// TestServer_SyncClientsWithMpegTs covers the syncClients path with real
// MPEG-TS data: a late subscriber only starts receiving at a decoder
// synchronization point, and the bytes it receives are a valid MPEG-TS stream.
func TestServer_SyncClientsWithMpegTs(t *testing.T) {
	data := mpegTsFixture(t)
	s := NewServer(&Config{
		Server: ServerConfig{PacketSize: mpegts.PacketLen, SyncClients: true},
		Relay:  relay.RelayConfig{BufferSize: 512 * mpegts.PacketLen, PacketSize: mpegts.PacketLen},
	})
	sess := newSession(t, s)

	// The sample file is a single short GOP with only one decoder init point at
	// its start, so replay it: the subscriber joins after the first copy and has
	// to synchronize on the second one.
	const copies = 4
	stream := bytes.Repeat(data, copies)

	firstCopy := int64(len(data) / mpegts.PacketLen)
	pub := sess.publish("ts")
	feedPackets(t, pub.conn, stream[:len(data)])
	// Wait until the publisher consumed the whole first copy, so every packet of
	// it has reached the relay (and was dropped, there was no subscriber yet).
	waitReads(t, pub.conn, firstCopy)
	waitRelayDrained(t, s)

	play := sess.play("ts", 1)
	feedPackets(t, pub.conn, stream[len(data):])

	// The subscriber starts forwarding only once it found the decoder init
	// data. That must happen, and it must happen within the given data.
	waitUntil(t, "subscriber to receive synchronized stream", func() bool {
		return play.conn.Len() > 0
	})
	waitRelayDrained(t, s)

	out := play.conn.Bytes()
	if len(out)%mpegts.PacketLen != 0 {
		t.Fatalf("subscriber received a partial packet: %d bytes", len(out))
	}
	parser := mpegts.NewParser()
	if err := parser.Parse(out); err != nil {
		t.Fatalf("synchronized output is not parseable: %v", err)
	}
	init, err := parser.InitData()
	if err != nil || len(init) == 0 {
		t.Fatalf("synchronized output has no decoder init: init=%d err=%v", len(init), err)
	}
	// The subscriber must not have received the first copy, which was published
	// before it joined: without sync handling it would have been handed the raw
	// mid-GOP data instead of waiting for a decoder init point.
	if len(out) > len(stream)-len(data) {
		t.Fatalf("subscriber received %d bytes, more than the data published after it joined (%d)",
			len(out), len(stream)-len(data))
	}
}

// TestServer_RapidRepublishKeepsStatisticsConsistent exercises short
// publish/disconnect cycles at the SRT level with a puller attached and checks
// that streams, clients and buffers converge back to zero every time.
func TestServer_RapidRepublishKeepsStatisticsConsistent(t *testing.T) {
	s := newTestServer(t, relay.RelayConfig{BufferSize: 64 * 1316, PacketSize: 1316})

	for i := 0; i < 50; i++ {
		sess := newSession(t, s)
		pub := sess.publish("test")
		play := sess.play("test", 1)

		pub.conn.send([]byte{byte(i), 1, 2, 3})
		waitUntil(t, "packet to arrive", func() bool { return play.conn.Len() == 4 })

		sess.disconnect(pub)
		waitHandle(t, play.done, "subscriber")

		if stats := s.GetStatistics(); len(stats) != 0 {
			t.Fatalf("cycle %d: statistics did not converge: %+v", i, stats)
		}
		if got := s.relay.BuffersInUse(); got != 0 {
			t.Fatalf("cycle %d: %d buffers leaked", i, got)
		}
	}
}

// TestServer_PublisherDisconnectWhileSubscribersRead races the publisher that
// disconnects against subscribers that are reading. No double release, no leak,
// no blocked reader.
func TestServer_PublisherDisconnectWhileSubscribersRead(t *testing.T) {
	s := newTestServer(t, relay.RelayConfig{BufferSize: 16 * 1316, PacketSize: 1316})

	for iter := 0; iter < 40; iter++ {
		sess := newSession(t, s)
		pub := sess.publish("race")

		const subscribers = 4
		plays := make([]connSession, 0, subscribers)
		for i := 0; i < subscribers; i++ {
			plays = append(plays, sess.play("race", i+1))
		}

		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 8; i++ {
				if !pub.conn.send([]byte{byte(i), 1, 2, 3}) {
					return
				}
			}
		}()

		time.Sleep(time.Duration(iter%4) * 100 * time.Microsecond)
		sess.disconnect(pub)

		wg.Wait()
		for _, play := range plays {
			waitHandle(t, play.done, "subscriber")
			if !play.conn.isClosed() {
				t.Fatalf("iteration %d: subscriber connection left open", iter)
			}
		}
		if stats := s.GetStatistics(); len(stats) != 0 {
			t.Fatalf("iteration %d: statistics did not converge: %+v", iter, stats)
		}
		if got := s.relay.BuffersInUse(); got != 0 {
			t.Fatalf("iteration %d: %d buffers leaked", iter, got)
		}
	}
}

// waitRelayDrained waits until every buffer published so far has been released.
// A publisher blocked in Read legitimately holds one buffer, so up to one
// buffer may remain in use.
func waitRelayDrained(t *testing.T, s *Server) {
	t.Helper()
	waitUntil(t, "relay to drain", func() bool { return s.relay.BuffersInUse() <= 1 })
}

// waitReads waits until the connection's reader consumed n chunks. The publish
// loop reads, then fans out, then reads again, so once the n-th chunk has been
// read, all n chunks have been handed to the relay before the next one.
func waitReads(t *testing.T, conn *fakeConn, n int64) {
	t.Helper()
	waitUntil(t, "publisher to consume input", func() bool { return conn.Reads() >= n })
}

// waitForStream waits until name is registered as a live stream.
func waitForStream(t *testing.T, s *Server, name string) bool {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !s.relay.ChannelExists(name) {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Millisecond)
	}
	return true
}
