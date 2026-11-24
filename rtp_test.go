package main

import (
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// failingWriter is a stand-in track whose Write always fails, simulating a
// closed PeerConnection or a track with no viewer. The receive loop must keep
// calling it instead of dying.
type failingWriter struct {
	calls atomic.Int64
}

func (f *failingWriter) Write([]byte) (int, error) {
	f.calls.Add(1)
	return 0, errors.New("simulated track write failure: no viewer")
}

// countingWriter records successful writes.
type countingWriter struct {
	calls atomic.Int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	c.calls.Add(1)
	return len(p), nil
}

func freeUDPPort(t *testing.T) (*net.UDPConn, *net.UDPAddr) {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("bind test udp: %v", err)
	}
	addr := conn.LocalAddr().(*net.UDPAddr)
	return conn, addr
}

func sendUDP(t *testing.T, addr *net.UDPAddr, count int) {
	t.Helper()
	sender, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		t.Fatalf("dial udp: %v", err)
	}
	defer sender.Close()
	pkt := []byte{0x80, 0x60, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0xde, 0xad, 0xbe, 0xef}
	for i := 0; i < count; i++ {
		if _, err := sender.Write(pkt); err != nil {
			t.Fatalf("send packet %d: %v", i, err)
		}
	}
}

func waitForAtomic(t *testing.T, counter *atomic.Int64, want int64, timeout time.Duration) int64 {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if got := counter.Load(); got >= want {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	return counter.Load()
}

// TestRtpLoopSurvivesTrackWriteFailures models "no viewer / connection
// closed": every track write fails, yet the receive loop must consume every
// incoming packet and keep running.
func TestRtpLoopSurvivesTrackWriteFailures(t *testing.T) {
	conn, addr := freeUDPPort(t)
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })

	fw := &failingWriter{}
	go listenRtpConn(conn, "video", fw, stop)

	const packets = 40
	sendUDP(t, addr, packets)

	if got := waitForAtomic(t, &fw.calls, packets, 5*time.Second); got < packets {
		t.Fatalf("failing writer received %d packets, want %d (loop died on write error?)", got, packets)
	}

	// Loop must still be alive and processing after the first batch.
	sendUDP(t, addr, packets)
	if got := waitForAtomic(t, &fw.calls, 2*packets, 5*time.Second); got < 2*packets {
		t.Fatalf("after recovery only %d writes, want %d", got, 2*packets)
	}
}

// TestRtpLoopSurvivesSourceInterruption makes the read side sit idle (read
// deadlines produce repeated read errors with no source), then verifies it
// picks packets back up automatically once the "source" returns.
func TestRtpLoopSurvivesSourceInterruption(t *testing.T) {
	conn, addr := freeUDPPort(t)
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })

	cw := &countingWriter{}
	go listenRtpConn(conn, "audio", cw, stop)

	// No sender for longer than several read deadlines: read errors fire,
	// the loop must not exit.
	time.Sleep(2500 * time.Millisecond)

	sendUDP(t, addr, 20)
	if got := waitForAtomic(t, &cw.calls, 20, 5*time.Second); got < 20 {
		t.Fatalf("after source interruption only %d packets received, want 20", got)
	}
}

// TestRtpLoopWithNilTrackDropsPackets ensures a disabled media kind never
// forwards data to a nil track and never crashes.
func TestRtpLoopWithNilTrackDropsPackets(t *testing.T) {
	conn, addr := freeUDPPort(t)
	stop := make(chan struct{})
	var exited sync.WaitGroup
	exited.Add(1)
	go func() {
		defer exited.Done()
		listenRtpConn(conn, "video", nil, stop)
	}()

	sendUDP(t, addr, 15)
	// Give the loop time to process all packets; if it panicked the test
	// runner would report it.
	time.Sleep(300 * time.Millisecond)

	// Stopping must end the goroutine cleanly.
	close(stop)
	done := make(chan struct{})
	go func() { exited.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("RTP loop did not stop when stop channel was closed")
	}
}

// TestRtpLoopHealthyWriterDeliversEverything is the normal-path check: when
// the track accepts writes, every packet reaches it.
func TestRtpLoopHealthyWriterDeliversEverything(t *testing.T) {
	conn, addr := freeUDPPort(t)
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })

	cw := &countingWriter{}
	go listenRtpConn(conn, "video", cw, stop)

	sendUDP(t, addr, 50)
	if got := waitForAtomic(t, &cw.calls, 50, 5*time.Second); got != 50 {
		t.Fatalf("healthy writer got %d packets, want 50", got)
	}
}

// TestRtpLoopIndependentSessions verifies two loops (video + audio) run
// independently: failure on one never affects the other.
func TestRtpLoopIndependentSessions(t *testing.T) {
	videoConn, videoAddr := freeUDPPort(t)
	audioConn, audioAddr := freeUDPPort(t)
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })

	failing := &failingWriter{}
	healthy := &countingWriter{}
	go listenRtpConn(videoConn, "video", failing, stop)
	go listenRtpConn(audioConn, "audio", healthy, stop)

	sendUDP(t, videoAddr, 25) // all video writes fail
	sendUDP(t, audioAddr, 25) // audio must be unaffected

	if got := waitForAtomic(t, &failing.calls, 25, 5*time.Second); got < 25 {
		t.Fatalf("video loop got %d, want 25", got)
	}
	if got := waitForAtomic(t, &healthy.calls, 25, 5*time.Second); got < 25 {
		t.Fatalf("audio loop got %d, want 25 (affected by video failures?)", got)
	}
}

// TestListenRtpBindFailureReturnsNotPanics ensures an unbindable address is
// reported and the function returns instead of crashing the process.
func TestListenRtpBindFailureReturnsNotPanics(t *testing.T) {
	// Occupy a port first, then ask the RTP listener to bind the same one.
	holder, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	defer holder.Close()
	port := holder.LocalAddr().(*net.UDPAddr).Port

	stop := make(chan struct{})
	defer close(stop)
	listenRtp("127.0.0.1", port, "video", &countingWriter{}, stop)
	// Reaching here means no panic and the goroutine returned.
}
