package session

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// This file covers the frame-ack -> pacer connection and its lifecycle:
// healthy links stay open (A1), real queueing sheds (A2), old-stream acks do
// not cross a rebuild (A3), a never-acking viewer is unchanged (A4), duplicate
// and out-of-order acks count once (A5), the in-flight table is bounded (A6),
// the ack baseline follows a path change without adopting a burst (A7), and
// Ack/Sent/Reset/Admit are safe concurrently (A8).

var ackT0 = time.Unix(1_700_000_000, 0)

// ackOne records one frame at sentAt and acks it at sentAt+rtt, feeding the
// matched measurement to p exactly as Session.ViewerAckFrame does.
func ackOne(t *testing.T, f *FlowEst, p *Pacer, pts uint64, sentAt time.Time, rtt time.Duration) AckOutcome {
	t.Helper()
	f.Sent(pts, 1000, sentAt)
	out := f.Ack(pts, sentAt.Add(rtt))
	if !out.Matched {
		t.Fatalf("pts %d did not match the frame just sent", pts)
	}
	p.ObserveAck(out.RTT, out.Base)
	return out
}

// drainAcks applies matched outcomes without a *testing.T, for hot loops.
func drainAcks(f *FlowEst, p *Pacer, pts uint64, sentAt time.Time, rtt time.Duration) AckOutcome {
	f.Sent(pts, 1000, sentAt)
	out := f.Ack(pts, sentAt.Add(rtt))
	if out.Matched {
		p.ObserveAck(out.RTT, out.Base)
	}
	return out
}

func admitFraction(p *Pacer, n int) float64 {
	sent := 0
	for i := 0; i < n; i++ {
		if p.Admit() {
			sent++
		}
	}
	return float64(sent) / float64(n)
}

// A1: stable, low send-to-ack time keeps the send fraction at one and admits
// essentially every droppable frame.
func TestAckThrottleHealthyKeepsFull(t *testing.T) {
	var f FlowEst
	p := NewPacer()
	at := ackT0
	for i := 0; i < 120; i++ {
		ackOne(t, &f, p, uint64(i+1), at, 20*time.Millisecond)
		at = at.Add(25 * time.Millisecond)
	}
	if k := p.AckKeep(); k != 1 {
		t.Fatalf("ackKeep = %.3f on a healthy acking link, want 1", k)
	}
	if k := p.Keep(); k != 1 {
		t.Fatalf("keep = %.3f on a healthy acking link, want 1", k)
	}
	if rate := admitFraction(p, 2000); rate < 0.99 {
		t.Fatalf("admitted %.3f of droppable frames, want ~1", rate)
	}
}

// A2 + recovery: a real excess over the ack baseline collapses the fraction;
// once queueing clears, healthy acks reopen it gradually rather than in one
// step.
func TestAckThrottleQueuesDropsAndHealthyAcksRecover(t *testing.T) {
	var f FlowEst
	p := NewPacer()
	at := ackT0

	// Establish a 20 ms ack baseline.
	for i := 0; i < 30; i++ {
		ackOne(t, &f, p, uint64(i+1), at, 20*time.Millisecond)
		at = at.Add(30 * time.Millisecond)
	}
	if p.AckKeep() != 1 {
		t.Fatalf("baseline phase moved ackKeep to %.3f", p.AckKeep())
	}

	// Sustained 800 ms send-to-ack against a 20 ms floor: real queueing.
	for i := 0; i < 25; i++ {
		drainAcks(&f, p, uint64(100+i), at, 800*time.Millisecond)
		at = at.Add(30 * time.Millisecond)
	}
	if k := p.AckKeep(); k > 0.25 {
		t.Fatalf("ackKeep = %.3f after sustained queueing, want a clear drop (<=0.25)", k)
	}
	if rate := admitFraction(p, 1000); rate > 0.3 {
		t.Fatalf("admitted %.3f under real queueing, want <=0.30", rate)
	}

	// Queue drains. Track the recovery curve.
	var curve []float64
	for i := 0; i < 80; i++ {
		ackOne(t, &f, p, uint64(200+i), at, 20*time.Millisecond)
		at = at.Add(30 * time.Millisecond)
		if i == 0 || i == 4 || i == 9 || i == 44 || i == 79 {
			curve = append(curve, p.AckKeep())
		}
	}
	for i := 1; i < len(curve); i++ {
		if curve[i]+1e-9 < curve[i-1] {
			t.Fatalf("recovery went backwards: %v", curve)
		}
	}
	if curve[1] > 0.5 {
		t.Fatalf("recovery was instant: keep after five good acks = %.3f, want gradual", curve[1])
	}
	if last := curve[len(curve)-1]; last != 1 {
		t.Fatalf("ackKeep = %.3f after sustained healthy acks, want 1 (curve %v)", last, curve)
	}
}

// A5/H2: replaying the same ack, and acking pts that were popped by an earlier
// cumulative ack, produces no second measurement: the fraction and baseline
// move exactly once and the ignored counter advances.
func TestAckDuplicateOutOfOrderAndDroppedCountOnce(t *testing.T) {
	var f FlowEst
	p := NewPacer()

	// A healthy sample to set a low baseline.
	ackOne(t, &f, p, 1, ackT0, 20*time.Millisecond)

	// Frames 10..14 in flight; ack 12 cumulatively pops 10, 11, 12. Only frame
	// 12 is measured, at 800 ms, which is one backoff step.
	t1 := ackT0.Add(time.Second)
	for i, pts := range []uint64{10, 11, 12, 13, 14} {
		f.Sent(pts, 1000, t1.Add(time.Duration(i)*time.Millisecond))
	}
	out := f.Ack(12, t1.Add(800*time.Millisecond))
	if !out.Matched {
		t.Fatal("ack 12 should match")
	}
	p.ObserveAck(out.RTT, out.Base)

	keepOnce := p.AckKeep()
	if keepOnce >= 1 || keepOnce <= minKeep {
		t.Fatalf("one congested ack moved keep to %.3f; expected one mid step", keepOnce)
	}
	baseBefore := f.Snapshot(t1.Add(810 * time.Millisecond)).BaseMs
	backlogBefore := f.Snapshot(t1.Add(810 * time.Millisecond)).Backlog
	ignoredBefore := f.Snapshot(t1.Add(810 * time.Millisecond)).IgnoredAcks

	// Duplicate 12, the already-popped 10/11 arriving "out of order", an
	// unknown pts, and an ack for a shed frame that was never Sent.
	for _, pts := range []uint64{12, 12, 10, 11, 12, 999, 777} {
		out := f.Ack(pts, t1.Add(900*time.Millisecond))
		if out.Matched {
			t.Fatalf("replayed/out-of-order pts %d matched a second time", pts)
		}
		// Production only feeds Matched outcomes; do so unconditionally here to
		// prove the guard the caller relies on.
		if out.Matched {
			p.ObserveAck(out.RTT, out.Base)
		}
	}
	if got := p.AckKeep(); got != keepOnce {
		t.Fatalf("ignored acks moved ackKeep %.17g -> %.17g", keepOnce, got)
	}
	s := f.Snapshot(t1.Add(time.Second))
	if s.BaseMs != baseBefore {
		t.Fatalf("baseline jumped %v -> %v on ignored acks", baseBefore, s.BaseMs)
	}
	if s.Backlog != backlogBefore {
		t.Fatalf("backlog jumped %d -> %d on ignored acks", backlogBefore, s.Backlog)
	}
	if s.IgnoredAcks != ignoredBefore+7 {
		t.Fatalf("ignored = %d, want %d", s.IgnoredAcks, ignoredBefore+7)
	}
}

// A3 via the real Session coordinator: after a rebuild, old-stream acks cannot
// match, cannot lower the fraction and cannot jump backlog; the new pts epoch
// starts healthy and reopens the throttle.
func TestViewerRebuildOldAcksDoNotPolluteNewStream(t *testing.T) {
	s, v, p := newAckCoordSession(t)
	f := &v.flow

	// Healthy baseline then enough congestion to sit on the floor.
	at := ackT0
	for i := 0; i < 20; i++ {
		f.Sent(uint64(i+1), 1000, at)
		s.ViewerAckFrame(1, uint64(i+1), at.Add(20*time.Millisecond))
		at = at.Add(30 * time.Millisecond)
	}
	for i := 0; i < 30; i++ {
		pts := uint64(100 + i)
		f.Sent(pts, 1000, at)
		s.ViewerAckFrame(1, pts, at.Add(900*time.Millisecond))
		at = at.Add(30 * time.Millisecond)
	}
	if k := p.AckKeep(); k > minKeep+1e-9 {
		t.Fatalf("test setup: ackKeep = %.3f, want floor %.2f", k, minKeep)
	}
	if ep := f.Epoch(); ep != 0 {
		t.Fatalf("epoch before reset = %d, want 0", ep)
	}

	// Encoder rebuilt: config frame goes through the coordinated reset.
	s.ViewerResetThrottle(1)
	if ep := f.Epoch(); ep != 1 {
		t.Fatalf("epoch after reset = %d, want 1", ep)
	}
	afterReset := p.AckKeep()

	// A storm of OLD stream acks, including duplicates and the pts values the
	// new stream is about to reuse. None may match or move the pacer.
	for i := 0; i < 50; i++ {
		s.ViewerAckFrame(1, uint64(i%7), at.Add(time.Duration(i)*time.Millisecond))
		s.ViewerAckFrame(1, uint64(100+(i%30)), at.Add(time.Duration(i)*time.Millisecond))
	}
	if got := p.AckKeep(); got != afterReset {
		t.Fatalf("old-stream ack storm moved ackKeep %.3f -> %.3f", afterReset, got)
	}
	if snap := f.Snapshot(at.Add(time.Second)); snap.Backlog != 0 {
		t.Fatalf("backlog after reset + old acks = %d, want 0", snap.Backlog)
	} else if snap.IgnoredAcks < 100 {
		t.Fatalf("old acks ignored = %d, want all 100", snap.IgnoredAcks)
	}

	// The new stream restarts pts at 1. Healthy acks must be read as healthy,
	// not as the old congestion, and reopen the throttle.
	at = at.Add(2 * time.Second)
	for i := 0; i < 90; i++ {
		pts := uint64((i % 8) + 1) // pts reuse immediately
		f.Sent(pts, 1000, at)
		s.ViewerAckFrame(1, pts, at.Add(20*time.Millisecond))
		at = at.Add(25 * time.Millisecond)
	}
	if k := p.AckKeep(); k != 1 {
		t.Fatalf("new healthy stream left ackKeep = %.3f, want 1", k)
	}
	if k := p.Keep(); k != 1 {
		t.Fatalf("new healthy stream left effective keep = %.3f, want 1", k)
	}
}

// Rebuild with no subsequent acks: the throttle must converge back to full on
// elapsed time rather than sit at the floor forever.
func TestAckThrottleRebuildSilenceRecovers(t *testing.T) {
	p, advance := testPacer()
	for i := 0; i < 40; i++ {
		p.ObserveQueue(900 * time.Millisecond)
	}
	if k := p.AckKeep(); k > minKeep+1e-9 {
		t.Fatalf("setup ackKeep = %.3f, want floor", k)
	}

	// A rebuild arms immediate unpinning (no extra grace for a discarded
	// stream backlog).
	p.StreamReset()
	p.Recover() // cursor starts at "now": no elapsed time yet
	advance(250 * time.Millisecond)
	p.Recover()
	if k := p.AckKeep(); k <= minKeep+1e-9 {
		t.Fatalf("after rebuild + 250ms of silence ackKeep still %.3f (floor); want recovery to start", k)
	}

	prev := p.AckKeep()
	recovered := false
	for i := 0; i < 40; i++ {
		advance(250 * time.Millisecond)
		p.Recover()
		k := p.AckKeep()
		if k+1e-9 < prev {
			t.Fatalf("recovery went backwards %.3f -> %.3f", prev, k)
		}
		prev = k
		if k == 1 {
			recovered = true
			break
		}
	}
	if !recovered {
		t.Fatalf("ackKeep stuck at %.3f after rebuild and 10s of silence", prev)
	}
	if k := p.Keep(); k != 1 {
		t.Fatalf("effective keep = %.3f, want 1 after silence recovery", k)
	}
}

// Without any rebuild, acks simply stopping after congestion must also unpin
// after the staleness delay.
func TestAckThrottleAckSilenceUnpins(t *testing.T) {
	p, advance := testPacer()
	for i := 0; i < 40; i++ {
		p.ObserveQueue(900 * time.Millisecond)
	}
	low := p.AckKeep()

	// Inside the unpin grace nothing is time-recovered.
	advance(pacerUnpinDelay - 250*time.Millisecond)
	p.Recover()
	if k := p.AckKeep(); k != low {
		t.Fatalf("recovery started before the staleness delay: %.3f -> %.3f", low, k)
	}

	prev := low
	recovered := false
	for i := 0; i < 40; i++ {
		advance(250 * time.Millisecond)
		p.Recover()
		k := p.AckKeep()
		if k+1e-9 < prev {
			t.Fatalf("recovery went backwards")
		}
		prev = k
		if k == 1 {
			recovered = true
			break
		}
	}
	if !recovered {
		t.Fatalf("ackKeep pinned at %.3f with no further acks", prev)
	}
}

// A4: a client that never sends acks never activates the ack axis: ackKeep is
// always one, the effective fraction equals the rtt fraction, missing (zero)
// measurements do nothing, and arbitrary elapsed time does not slow anything.
func TestAckThrottleNeverAckingClientUnaffected(t *testing.T) {
	p, advance := testPacer()
	for total := 0; total < 60; total++ {
		advance(time.Second)
		p.Observe(0) // no client report either
		if k := p.AckKeep(); k != 1 {
			t.Fatalf("missing ack changed ackKeep to %.3f", k)
		}
		if k := p.Keep(); k != 1 {
			t.Fatalf("missing measurement slowed the pacer to %.3f", k)
		}
	}
	if rate := admitFraction(p, 2000); rate < 0.99 {
		t.Fatalf("never-acking client admitted only %.3f, want ~1", rate)
	}

	// Even when rtt control moves, the ack axis stays fully open and the
	// effective fraction is exactly the rtt one.
	p.Observe(20 * time.Millisecond)
	for i := 0; i < 60; i++ {
		p.Observe(3 * time.Second)
		if got, want := p.Keep(), p.RTTKeep(); got != want {
			t.Fatalf("Keep %.3f != RTTKeep %.3f with no acks", got, want)
		}
	}
	if p.AckKeep() != 1 {
		t.Fatal("ackKeep left 1 without any ack input")
	}
}

// A6: sustained sending with no acks caps the in-flight table and its backing
// array; memory does not grow with viewing duration.
func TestFlowLongSilenceInflightBounded(t *testing.T) {
	var f FlowEst
	for i := uint64(0); i < 200_000; i++ {
		f.Sent(i, 100, ackT0)
	}
	s := f.Snapshot(ackT0)
	if s.Backlog != flowMaxInflight {
		t.Fatalf("backlog = %d, want cap %d", s.Backlog, flowMaxInflight)
	}
	if c := cap(f.inflight); c > 2*flowMaxInflight {
		t.Fatalf("backing array cap = %d, want bounded (<= %d)", c, 2*flowMaxInflight)
	}
}

// A7: the ack baseline adopts a genuinely slower stable path within the
// 10 second bucket window, after which a no-excess reading is healthy again.
func TestAckBaselineFollowsAPathSwitch(t *testing.T) {
	var f FlowEst
	p := NewPacer()
	at := ackT0

	// Wifi-like 20 ms floor.
	pts := uint64(1)
	for i := 0; i < 40; i++ {
		drainAcks(&f, p, pts, at, 20*time.Millisecond)
		pts++
		at = at.Add(30 * time.Millisecond)
	}

	// Handover to a stable 300 ms path. For the first window the old floor
	// reads as excess and sheds; keep feeding no-queue 300 ms samples for
	// >flowBaseMaxAge and the baseline must adopt 300, making them healthy.
	for i := 0; i < 500; i++ {
		sentAt := at
		f.Sent(pts, 1000, sentAt)
		out := f.Ack(pts, sentAt.Add(300*time.Millisecond))
		if out.Matched {
			p.ObserveAck(out.RTT, out.Base)
		}
		pts++
		at = at.Add(40 * time.Millisecond)
	}
	last := at.Add(300 * time.Millisecond)
	base := f.Snapshot(last).BaseMs
	if base < 290 || base > 310 {
		t.Fatalf("ack baseline after handover = %.1f ms, want ~300", base)
	}
	if k := p.AckKeep(); k != 1 {
		t.Fatalf("ackKeep = %.3f after the new floor was learned, want 1", k)
	}
}

// A7 mirror: a short burst of huge send-to-ack must not become the baseline,
// and once it ends healthy acks reopen the throttle.
func TestAckBaselineSurvivesAQueuingBurst(t *testing.T) {
	var f FlowEst
	p := NewPacer()
	at := ackT0
	pts := uint64(1)
	for i := 0; i < 40; i++ {
		drainAcks(&f, p, pts, at, 20*time.Millisecond)
		pts++
		at = at.Add(40 * time.Millisecond)
	}
	// Two seconds of queueing.
	for i := 0; i < 50; i++ {
		drainAcks(&f, p, pts, at, 800*time.Millisecond)
		pts++
		at = at.Add(40 * time.Millisecond)
	}
	if base := f.Snapshot(at.Add(800 * time.Millisecond)).BaseMs; base > 60 {
		t.Fatalf("two-second burst raised baseline to %.1f ms, want it near 20", base)
	}
	if k := p.AckKeep(); k > 0.5 {
		t.Fatalf("ackKeep = %.3f after a queueing burst, want real shedding", k)
	}
	// Burst ends.
	for i := 0; i < 100; i++ {
		drainAcks(&f, p, pts, at, 20*time.Millisecond)
		pts++
		at = at.Add(30 * time.Millisecond)
	}
	if k := p.AckKeep(); k != 1 {
		t.Fatalf("ackKeep = %.3f after the burst cleared, want 1", k)
	}
}

// A8: concurrent ack processing, sending, snapshotting, admission and rebuild
// must be race-free (run with -race), keep the in-flight table capped and the
// fraction in range.
func TestViewerAckConcurrentWithSendAdmitAndReset(t *testing.T) {
	s, v, p := newAckCoordSession(t)

	var sentPTS atomic.Uint64
	var wg sync.WaitGroup

	// Writer: records delivered frames and admits droppable ones.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 3000; i++ {
			pts := sentPTS.Add(1)
			v.flow.Sent(pts, 256, time.Now())
			p.Admit()
			if i%97 == 0 {
				s.ViewerResetThrottle(1)
			}
		}
	}()

	// Stats reader.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 3000; i++ {
			_ = v.flow.Snapshot(time.Now())
			_ = p.Keep()
		}
	}()

	// Four ack loops: recent pts, fixed duplicates and unknown pts.
	for g := 0; g < 4; g++ {
		g := g
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 1500; i++ {
				var pts uint64
				switch i % 3 {
				case 0:
					pts = sentPTS.Load() // newest (or not sent yet)
				case 1:
					pts = uint64(1 + (i+g)%5) // small, usually popped/duplicate
				default:
					pts = uint64(100000 + i) // unknown / shed
				}
				s.ViewerAckFrame(1, pts, time.Now())
			}
		}()
	}

	wg.Wait()

	k := p.Keep()
	if k < minKeep-1e-9 || k > 1+1e-9 {
		t.Fatalf("keep %.3f out of [%.2f,1]", k, minKeep)
	}
	if b := v.flow.Snapshot(time.Now()).Backlog; b > flowMaxInflight {
		t.Fatalf("backlog %d exceeds cap %d", b, flowMaxInflight)
	}
}

// FlowEst alone under the same hammer, including Reset and Epoch.
func TestFlowEstConcurrentAckSentResetSnapshot(t *testing.T) {
	var f FlowEst
	var pts atomic.Uint64
	var wg sync.WaitGroup
	for g := 0; g < 3; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				n := pts.Add(1)
				f.Sent(n, 64, time.Now())
				if i%50 == 0 {
					f.Ack(n, time.Now())
				}
				if i%500 == 0 {
					f.Reset()
					_ = f.Epoch()
				}
				_ = f.Snapshot(time.Now())
			}
		}(g)
	}
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 3000; i++ {
				f.Ack(pts.Load(), time.Now()) // often a duplicate/unknown
				_ = f.Snapshot(time.Now())
			}
		}()
	}
	wg.Wait()
	if b := f.Snapshot(time.Now()).Backlog; b > flowMaxInflight {
		t.Fatalf("backlog %d exceeds cap", b)
	}
}

func newAckCoordSession(t *testing.T) (*Session, *viewer, *Pacer) {
	t.Helper()
	p := NewPacer()
	v := &viewer{id: 1, pacer: p}
	s := &Session{viewers: map[uint64]*viewer{1: v}}
	return s, v, p
}
