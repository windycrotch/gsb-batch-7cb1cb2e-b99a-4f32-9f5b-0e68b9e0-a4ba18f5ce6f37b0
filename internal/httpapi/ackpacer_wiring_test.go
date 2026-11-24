package httpapi

import (
	"testing"
	"time"

	"github.com/bmcreations/loupe/internal/session"
	"github.com/bmcreations/loupe/internal/target"
)

// These tests drive frame acks through the REAL read loop (Recv -> answered
// inline -> dispatch -> Session.ViewerAckFrame) while the real write loop runs,
// so the ack -> FlowEst -> Pacer wiring is exercised end to end rather than
// through a same-layer fake.

type wiredAckViewer struct {
	t      *testing.T
	sess   *session.Session
	srv    *Server
	conn   *ackConn
	target *flowTarget
	done   chan struct{}
}

func startWiredAckViewer(t *testing.T) *wiredAckViewer {
	t.Helper()
	ft := &flowTarget{frames: make(chan target.Frame, 16)}
	m := NewManager(target.Options{})
	m.Register(ft)
	sess, err := m.Acquire(t.Context(), "flow")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	conn := newAckConn()
	srv := &Server{mgr: m}
	done := make(chan struct{})
	go func() { srv.serveViewer("flow", sess, conn); close(done) }()
	w := &wiredAckViewer{t: t, sess: sess, srv: srv, conn: conn, target: ft, done: done}
	t.Cleanup(func() {
		_ = conn.Close()
		sess.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("serveViewer still running 3s after cleanup")
		}
	})
	waitUntil(t, "viewer pacer", func() bool { return sess.ViewerPacer(1) != nil })
	return w
}

func (w *wiredAckViewer) pacer() *session.Pacer { return w.sess.ViewerPacer(1) }

func (w *wiredAckViewer) flow() *session.FlowEst {
	f, _ := w.sess.ViewerFlow(1)
	return f
}

func (w *wiredAckViewer) pushConfig() {
	w.target.frames <- target.Frame{Config: true, Data: []byte{0, 0, 0, 1, 0x67}}
}

func keyFrame(ms int) target.Frame {
	return target.Frame{Key: true, PTS: time.Duration(ms) * time.Millisecond, Data: []byte{0, 0, 0, 1, 0x65, byte(ms)}}
}

func deltaFrame(ms int) target.Frame {
	return target.Frame{PTS: time.Duration(ms) * time.Millisecond, Data: []byte{0, 0, 0, 1, 0x41, byte(ms)}}
}

// TestViewerAcksBackOffTheRealPacer sends a fast baseline ack then a batch of
// delayed acks through the real dispatch path and asserts the shared pacer
// sheds. This is the observable A1/A2 wiring proof.
func TestViewerAcksBackOffTheRealPacer(t *testing.T) {
	w := startWiredAckViewer(t)

	w.pushConfig()
	waitUntil(t, "config sent", func() bool { return len(w.conn.sentFrames()) >= 1 })

	// Ack the keyframe once FlowEst has recorded its Sent time (waiting on the
	// backlog, not the conn frame slice, which is appended before Sent). This
	// gives the ack baseline a small loopback value.
	w.target.frames <- keyFrame(1)
	waitUntil(t, "key recorded", func() bool { return w.flow().Snapshot(time.Now()).Backlog >= 1 })
	w.conn.recv <- frameAckPayload(1000)
	waitUntil(t, "baseline ack", func() bool { return w.flow().Snapshot(time.Now()).Acking })

	for ms := 2; ms <= 30; ms++ {
		w.target.frames <- deltaFrame(ms)
	}
	waitUntil(t, "deltas in flight", func() bool {
		return w.flow().Snapshot(time.Now()).Backlog >= 29
	})

	// Let every delta age ~300 ms, then ack pts 2..30 ascending in one burst.
	// Each names a distinct in-flight frame whose send-to-ack is ~300 ms
	// against the tiny keyframe baseline, so each is one fresh backoff sample.
	time.Sleep(300 * time.Millisecond)
	go func() {
		for ms := 2; ms <= 30; ms++ {
			select {
			case w.conn.recv <- frameAckPayload(uint64(ms) * 1000):
			case <-time.After(2 * time.Second):
				return
			}
		}
	}()

	waitUntil(t, "delayed acks back the pacer off", func() bool {
		return w.pacer().AckKeep() <= 0.25
	})
}

// Even at the floor, parameter sets and keyframes bypass admission and are
// delivered.
func TestViewerEssentialFramesBypassTheAckPacer(t *testing.T) {
	w := startWiredAckViewer(t)
	p := w.pacer()
	for i := 0; i < 40; i++ {
		p.ObserveQueue(3 * time.Second)
	}
	if p.AckKeep() > 0.1+1e-9 {
		t.Fatalf("setup keep %.3f, want floor", p.AckKeep())
	}

	before := len(w.conn.sentFrames())
	w.pushConfig()
	w.target.frames <- keyFrame(7)

	waitUntil(t, "config and key delivered", func() bool {
		return len(w.conn.sentFrames()) >= before+2
	})
	var sawCfg, sawKey bool
	for _, m := range w.conn.sentFrames()[before:] {
		if m.Config {
			sawCfg = true
		}
		if m.Key {
			sawKey = true
		}
	}
	if !sawCfg || !sawKey {
		t.Fatalf("config delivered=%v key delivered=%v at keep %.3f; essential frames must bypass",
			sawCfg, sawKey, p.Keep())
	}
}

// A client that never acks keeps a fully open ack pacer and loses no droppable
// frame to pacing.
func TestViewerNoAcksLeavePacerOpenAndFramesSent(t *testing.T) {
	w := startWiredAckViewer(t)
	w.pushConfig()
	w.target.frames <- keyFrame(1)
	const deltas = 40
	for ms := 2; ms <= 1+deltas; ms++ {
		w.target.frames <- deltaFrame(ms)
	}

	waitUntil(t, "all deltas sent", func() bool {
		n := 0
		for _, m := range w.conn.sentFrames() {
			if !m.Key && !m.Config {
				n++
			}
		}
		return n == deltas
	})

	if k := w.pacer().AckKeep(); k != 1 {
		t.Fatalf("ackKeep = %.3f for a no-ack client, want 1", k)
	}
	if k := w.pacer().Keep(); k != 1 {
		t.Fatalf("Keep = %.3f with no acks/rtt, want 1", k)
	}
	if got := w.sess.ShedBreakdown().Pacer; got != 0 {
		t.Fatalf("pacer sheds = %d for a no-ack client, want 0", got)
	}
}

// After a config-frame rebuild, acks naming the old pts cannot move the shared
// pacer through the real dispatch path.
func TestViewerPostRebuildOldAcksDoNotMovePacer(t *testing.T) {
	w := startWiredAckViewer(t)
	w.pushConfig()
	waitUntil(t, "config sent", func() bool { return len(w.conn.sentFrames()) >= 1 })

	// Lower the ack fraction directly, then let the config reset arm recovery
	// without changing the current value.
	p := w.pacer()
	for i := 0; i < 30; i++ {
		p.ObserveQueue(900 * time.Millisecond)
	}
	before := p.AckKeep()
	w.sess.ViewerResetThrottle(1)
	if got := p.AckKeep(); got != before {
		t.Fatalf("reset changed ackKeep %.3f -> %.3f; it should arm, not jump", before, got)
	}

	for _, pts := range []uint64{1000, 2000, 3000, 999000} {
		w.conn.recv <- frameAckPayload(pts)
	}
	time.Sleep(50 * time.Millisecond)
	if got := p.AckKeep(); got != before {
		t.Fatalf("old/unknown acks moved ackKeep %.3f -> %.3f", before, got)
	}
}
