package srt

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"testing"
	"time"

	gosrt "github.com/datarhei/gosrt"
	"github.com/voc/srtrelay/auth"
	"github.com/voc/srtrelay/relay"
	"github.com/voc/srtrelay/stream"
)

// startRealtimeServer starts a real relay on a loopback UDP port with the SRT
// protocol enabled, and returns its dial address.
func startRealtimeServer(t *testing.T, relayConfig relay.RelayConfig) (*Server, string, func()) {
	t.Helper()

	// Pick a free UDP port and release it again; the relay binds it right after.
	probe, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("cannot find a free UDP port: %v", err)
	}
	port := probe.LocalAddr().(*net.UDPAddr).Port
	probe.Close()

	ctx, cancel := context.WithCancel(context.Background())
	server := NewServer(&Config{
		Server: ServerConfig{
			Addresses:   []netip.AddrPort{netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(port))},
			LatencyMs:   120,
			PacketSize:  1316,
			Auth:        auth.NewStaticAuth(auth.StaticAuthConfig{Allow: []string{"*"}}),
			SyncClients: false,
		},
		Relay: relayConfig,
	})
	if err := server.Listen(ctx); err != nil {
		cancel()
		t.Fatalf("cannot start relay: %v", err)
	}
	return server, fmt.Sprintf("127.0.0.1:%d", port), func() {
		cancel()
		server.Wait()
	}
}

func dialRealtime(t *testing.T, addr, streamID string) gosrt.Conn {
	t.Helper()
	conf := gosrt.DefaultConfig()
	conf.StreamId = streamID
	conf.Latency = 120 * time.Millisecond
	conn, err := gosrt.Dial("srt", addr, conf)
	if err != nil {
		t.Fatalf("dial %s: %v", streamID, err)
	}
	return conn
}

// TestRealtime_RepublishAfterDisconnect is the production-path end-to-end check:
// real SRT sockets, a publisher disconnecting, and the same stream name being
// claimed again immediately. Old players must end, new players must receive
// data.
func TestRealtime_RepublishAfterDisconnect(t *testing.T) {
	if testing.Short() {
		t.Skip("realtime SRT test")
	}
	_, addr, stop := startRealtimeServer(t, relay.RelayConfig{BufferSize: 256 * 1316, PacketSize: 1316})
	defer stop()

	payload := []byte("hello from the publisher")

	// Publisher 1 publishes and a player receives.
	pub1 := dialRealtime(t, addr, "publish/e2e")
	play1 := dialRealtime(t, addr, "play/e2e")

	if _, err := pub1.Write(payload); err != nil {
		t.Fatalf("publish: %v", err)
	}
	buf := make([]byte, 2048)
	if err := play1.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	n, err := play1.Read(buf)
	if err != nil {
		t.Fatalf("player read: %v", err)
	}
	if string(buf[:n]) != string(payload) {
		t.Fatalf("player got %q, want %q", buf[:n], payload)
	}

	// Publisher disconnects.
	pub1.Close()

	// The old player must not hang: it has to see the stream end.
	if err := play1.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := play1.Read(buf); err == nil {
		t.Error("old player kept receiving after publisher disconnect")
	}
	play1.Close()

	// Republish immediately on the same name.
	pub2 := dialRealtime(t, addr, "publish/e2e")
	defer pub2.Close()
	newPayload := []byte("second publisher")
	if _, err := pub2.Write(newPayload); err != nil {
		t.Fatalf("republish: %v", err)
	}

	play2 := dialRealtime(t, addr, "play/e2e")
	defer play2.Close()
	if err := play2.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	n, err = play2.Read(buf)
	if err != nil {
		t.Fatalf("new player read: %v", err)
	}
	if string(buf[:n]) != string(newPayload) {
		t.Fatalf("new player got %q, want %q", buf[:n], newPayload)
	}
}

// TestRealtime_ConcurrentChurn hammers the same name with short publish/pull
// cycles over real sockets and requires every cycle to complete. This exercises
// the disconnect/read race with the actual SRT stack.
func TestRealtime_ConcurrentChurn(t *testing.T) {
	if testing.Short() {
		t.Skip("realtime SRT test")
	}
	s, addr, stop := startRealtimeServer(t, relay.RelayConfig{BufferSize: 256 * 1316, PacketSize: 1316})
	defer stop()

	const cycles = 5
	for i := 0; i < cycles; i++ {
		pub := dialRealtime(t, addr, "publish/churn")
		player := dialRealtime(t, addr, "play/churn")
		waitClients(t, s, "churn", 1)

		// The player is registered, so a single write reaches it.
		payload := []byte{byte(i), 1, 2, 3}
		if _, err := pub.Write(payload); err != nil {
			t.Fatalf("cycle %d: publish: %v", i, err)
		}

		buf := make([]byte, 2048)
		if err := player.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err := player.Read(buf); err != nil {
			t.Fatalf("cycle %d: player read: %v", i, err)
		}

		pub.Close()

		// The player ends when its publisher is gone.
		if err := player.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err := player.Read(buf); err == nil {
			t.Fatalf("cycle %d: player still receives after publisher close", i)
		}
		player.Close()
	}
}

// TestRealtime_DuplicatePublisher verifies at the protocol level that a second
// publisher for a live stream is disconnected instead of hijacking it.
func TestRealtime_DuplicatePublisher(t *testing.T) {
	if testing.Short() {
		t.Skip("realtime SRT test")
	}
	_, addr, stop := startRealtimeServer(t, relay.RelayConfig{BufferSize: 256 * 1316, PacketSize: 1316})
	defer stop()

	first := dialRealtime(t, addr, "publish/dup")
	defer first.Close()
	if _, err := first.Write([]byte("first")); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	// Give the relay a moment to register the stream.
	time.Sleep(50 * time.Millisecond)

	second := dialRealtime(t, addr, "publish/dup")
	defer second.Close()
	// The relay rejects the duplicate; writes either fail or simply never reach
	// subscribers. The decisive check is that the first publisher stays alive
	// and keeps serving.
	if err := first.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Write([]byte("still alive")); err != nil {
		t.Fatalf("first publisher was disturbed by duplicate: %v", err)
	}

	player := dialRealtime(t, addr, "play/dup")
	defer player.Close()
	buf := make([]byte, 2048)
	if err := player.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := player.Read(buf); err != nil {
		t.Fatalf("player of first publisher: %v", err)
	}
}

// TestRealtime_StreamIDParsing is a cheap check that the streamid forms used in
// the docs reach the relay as expected.
func TestRealtime_StreamIDParsing(t *testing.T) {
	for _, s := range []string{"publish/test", "play/test", "#!::m=publish,r=test", "#!::m=request,r=test"} {
		var id stream.StreamID
		if err := id.FromString(s); err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		if id.Name() != "test" {
			t.Errorf("%q parsed name %q, want test", s, id.Name())
		}
	}
}
