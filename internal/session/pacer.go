package session

import (
	"sync"
	"time"
)

// targetExcess is how much queuing delay above the path's own baseline this
// server will tolerate before shedding.
//
// The threshold has to be relative, not absolute. An absolute one silently
// assumes the path is fast: on a relayed link whose baseline round trip is
// already 250ms, a fixed 300ms target reads as permanently congested, the
// controller clamps to its floor and holds it there forever, and the stream is
// starved to a couple of frames per second while latency looks "controlled".
// What matters is delay this server is causing, which is the excess over the
// best round trip the path has recently managed.
const targetExcess = 150 * time.Millisecond

// recoverExcess is the excess the stream must fall below before the pacer opens
// back up. The gap from targetExcess is deliberate hysteresis: without it the
// controller oscillates between flooding and starving.
const recoverExcess = 60 * time.Millisecond

// The baseline is the minimum over a rolling set of short slots rather than
// over a fixed number of samples, because a sample-counted window has no fixed
// duration: it is fed once per frame, so on a link delivering three frames a
// second it spans a minute while on a healthy one it spans three seconds.
//
// A trace of a phone moving from wifi onto congested 5G shows what that costs.
// The baseline stayed at the wifi figure of 7ms while the client reported 500
// to 2000ms, so every sample read as excess, `keep` sat pinned at its 0.1 floor
// for thirteen straight seconds, and it could not recover because recovery
// needs rtt below 67ms on a path whose real floor was 233ms. That is trap 5
// again, reached from the other direction: there the threshold was absolute,
// here the threshold is relative to a baseline that has stopped tracking.
//
// The slot count sets how long a genuinely worse path takes to be believed,
// and the total duration sets how much sustained congestion it takes to poison
// the estimate. Five seconds is the compromise: short enough that a handover is
// learned before a viewer gives up, long enough that a two-second burst of
// queuing cannot convince the pacer that queuing is the new floor.
const (
	baseSlot  = 500 * time.Millisecond
	baseSlots = 10
)

// Pacer decides what fraction of non-essential frames to send, from the
// latency the client actually observes.
//
// This exists because socket-level backpressure is not trustworthy. Anything
// between this server and the viewer, a reverse proxy, a relay, a kernel send
// buffer sized for throughput, will accept data long after the link has
// stopped keeping up. Every one of those buffers is invisible from here and
// unbounded in practice. The phone's own round-trip number is the one
// measurement that includes all of them.
//
// The control law is deliberately AIMD, the same shape TCP congestion control
// uses: back off fast when latency climbs, recover slowly when it does not.
// Fast recovery re-fills the very buffers that caused the problem.
type Pacer struct {
	// mu serializes the write loop's Observe/Admit against the read loop's
	// ObserveAck on the same viewer connection. The control math is small, so a
	// single lock keeps the two send fractions and the accumulator coherent.
	mu sync.Mutex

	keep  float64 // fraction from client/transport rtt, in [minKeep, 1]
	accum float64 // fractional carry so the effective keep applies smoothly frame to frame

	// ackKeep is the fraction driven by matched frame acks alone. It starts
	// fully open and stays there for a viewer that never acks, which keeps such
	// a viewer on exactly the pre-ack behavior. The admitted fraction is
	// min(keep, ackKeep).
	ackKeep float64

	// badAt and ackBadAt are the p.now timestamps of the last FRESH over-target
	// rtt and ack-queue sample. Re-reads (ObserveRepeat) do not refresh badAt.
	// They arm the unpin recovery; the zero time means that axis has never
	// congested. recAt and ackRecAt are the elapsed-recovery cursors.
	badAt    time.Time
	ackBadAt time.Time
	recAt    time.Time
	ackRecAt time.Time

	// base is the path's recent minimum round trip: propagation and relay cost
	// this server cannot do anything about. Only delay above it is ours.
	base time.Duration
	// slots holds one minimum per baseSlot of wall clock, oldest first. The
	// baseline is the minimum across them, so a burst of queuing can spoil at
	// most one slot while a path that really did get slower expires every slot
	// it was ever measured on.
	slots     [baseSlots]time.Duration
	slotStart time.Time

	// now is the clock, swappable so the slot rotation can be tested without
	// sleeping through five seconds of it.
	now func() time.Time
}

const (
	minKeep      = 0.1  // never starve the stream completely
	backoffScale = 0.06 // multiplicative reduction per sample over target
	recoverStep  = 0.02 // additive recovery per sample under the low-water mark
)

// Unpinning is the guarantee that a stale congestion reading cannot hold the
// controller at its floor forever. Once no FRESH over-target sample has
// arrived for pacerUnpinDelay, time-based additive recovery reopens the send
// fraction even if the client never acks or reports again (an encoder rebuild
// whose new stream has not been acked yet, a hidden tab, a path change). It
// only ever raises the fraction, so a viewer that sends no measurement is
// never slowed for the missing measurement.
const (
	pacerUnpinDelay    = 1500 * time.Millisecond
	pacerRecoverPerSec = 0.6 // floor (0.1) to full in about 1.5 s once unpinning
)

// NewPacer returns a pacer that starts fully open.
func NewPacer() *Pacer { return &Pacer{keep: 1, ackKeep: 1, now: time.Now} }

// Keep reports the current effective send fraction, for metrics. Due time-based
// unpin recovery is applied first, so a poller sees a controller reopen after
// its measurements go stale.
func (p *Pacer) Keep() float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	p.recoverRTTLocked(now)
	p.recoverAckLocked(now)
	return p.effectiveLocked()
}

// RTTKeep reports the client/transport-rtt fraction alone.
func (p *Pacer) RTTKeep() float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.keep
}

// AckKeep reports the frame-ack-driven fraction alone. Unlike Keep it does not
// run elapsed recovery; call Recover/RecoverAt (or Admit/Keep) to advance it.
func (p *Pacer) AckKeep() float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ackKeep
}

// effectiveLocked is the single fraction Admit applies. Caller holds mu.
func (p *Pacer) effectiveLocked() float64 {
	if p.ackKeep < p.keep {
		return p.ackKeep
	}
	return p.keep
}

// Base reports the path's current baseline round trip, for metrics.
func (p *Pacer) Base() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.base
}

// observeBase folds a sample into the current slot and recomputes the baseline
// as the minimum across every live slot.
func (p *Pacer) observeBase(rtt time.Duration) {
	now := p.now()
	if p.slotStart.IsZero() {
		p.slotStart = now
	}
	// Rotate one slot per elapsed baseSlot. A gap longer than the whole window
	// clears it: nothing measured before then describes this path any more.
	for elapsed := now.Sub(p.slotStart); elapsed >= baseSlot; elapsed -= baseSlot {
		copy(p.slots[:], p.slots[1:])
		p.slots[len(p.slots)-1] = 0
		p.slotStart = p.slotStart.Add(baseSlot)
	}

	cur := &p.slots[len(p.slots)-1]
	if *cur == 0 || rtt < *cur {
		*cur = rtt
	}

	p.base = 0
	for _, v := range p.slots {
		if v == 0 {
			continue
		}
		if p.base == 0 || v < p.base {
			p.base = v
		}
	}

}

// Observe updates the send fraction from the latest client-measured latency.
// A zero rtt means the client has not reported yet; the pacer holds steady
// rather than guessing.
func (p *Pacer) Observe(rtt time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.observeRTTLocked(rtt, true)
}

// ObserveRepeat takes a sample the pacer has already been given, such as the
// client's last report re-read for the next frame. It still recovers, but it
// does not back off again: the client reports once a second and a viewer sends
// about 60 frames in that second, so backing off on each re-read turned one
// late report into 60 backoffs. At 12:08:28 on 4 November one 406ms report
// took keep to its floor, and the viewer shed for two seconds after it.
func (p *Pacer) ObserveRepeat(rtt time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.observeRTTLocked(rtt, false)
}

func (p *Pacer) observeRTTLocked(rtt time.Duration, fresh bool) {
	if rtt <= 0 {
		return
	}
	p.observeBase(rtt)
	excess := rtt - p.base

	switch {
	case excess > targetExcess:
		// Only a fresh report re-arms the unpin timer. Re-reading one high
		// report every frame is not new congestion, and must not postpone the
		// stale-signal recovery forever.
		if fresh {
			p.badAt = p.now()
			// Scale the backoff by how far over we are, so a 3s backlog collapses
			// the stream quickly while a slightly-over one barely moves.
			over := float64(excess-targetExcess) / float64(targetExcess)
			if over > 4 {
				over = 4
			}
			p.keep *= 1 - backoffScale*(1+over)
		}
	case excess < recoverExcess:
		p.keep += recoverStep
	}

	if p.keep < minKeep {
		p.keep = minKeep
	}
	if p.keep > 1 {
		p.keep = 1
	}
}

// ObserveAck feeds ONE matched frame-ack measurement: the frame's
// send-to-ack time and the ack-path baseline FlowEst computed from ack samples
// alone. Their difference is the queueing delay the acks reveal.
//
// It must be called exactly once per MATCHED AckOutcome. A duplicate,
// out-of-order or unknown ack yields Matched=false and must not reach here, or
// one late ack would be counted several times. Safe to call from the read
// goroutine concurrently with the write loop's Observe/Admit.
func (p *Pacer) ObserveAck(sendToAck, baseline time.Duration) {
	excess := sendToAck - baseline
	if excess < 0 {
		excess = 0
	}
	p.ObserveQueue(excess)
}

// ObserveQueue feeds one fresh ack-derived queueing-excess sample directly.
func (p *Pacer) ObserveQueue(excess time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if excess < 0 {
		excess = 0
	}
	now := p.now()
	if excess > targetExcess {
		p.ackBadAt = now
		// Scale the backoff exactly as the rtt law does. Every call here is a
		// distinct matched ack, so unlike an rtt re-read each one may back off.
		over := float64(excess-targetExcess) / float64(targetExcess)
		if over > 4 {
			over = 4
		}
		p.ackKeep *= 1 - backoffScale*(1+over)
	} else if excess < recoverExcess {
		p.ackKeep += recoverStep
	}
	if p.ackKeep < minKeep {
		p.ackKeep = minKeep
	}
	if p.ackKeep > 1 {
		p.ackKeep = 1
	}
}

// StreamReset marks an encoder rebuild (rotate, resize, restart): FlowEst.Reset
// has discarded the old stream's in-flight table and pts epoch. It does not
// snap the fraction open, because the path can still be slow; instead it drops
// any stale-congestion latch and arms immediate time-based recovery, so a new
// stream whose client is slow to ack converges back to full instead of sitting
// at the floor forever. The path baselines are left alone: they describe the
// link, and the slot/bucket windows adapt them to a new path in bounded time.
func (p *Pacer) StreamReset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	armed := now.Add(-pacerUnpinDelay) // already past the unpin grace
	p.badAt = armed
	p.ackBadAt = armed
	p.recAt = now
	p.ackRecAt = now
}

// Recover advances the stale-signal unpin recovery at p.now(). The write loop
// does not have to call it: Admit and Keep apply it themselves. It is here so a
// test (or a non-frame ticker) can drive the guarantee directly.
func (p *Pacer) Recover() {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	p.recoverRTTLocked(now)
	p.recoverAckLocked(now)
}

// RecoverAt is Recover against an explicit clock, for deterministic tests.
func (p *Pacer) RecoverAt(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.recoverRTTLocked(now)
	p.recoverAckLocked(now)
}

// recoverRTTLocked time-reopens the rtt fraction once its last fresh over-target
// sample is older than pacerUnpinDelay. Caller holds mu.
func (p *Pacer) recoverRTTLocked(now time.Time) {
	unpinKeep(&p.keep, &p.badAt, &p.recAt, now)
}

// recoverAckLocked is recoverRTTLocked for the ack fraction.
func (p *Pacer) recoverAckLocked(now time.Time) {
	unpinKeep(&p.ackKeep, &p.ackBadAt, &p.ackRecAt, now)
}

// unpinKeep raises *keep on an elapsed-time budget after *badAt goes stale. A
// zero badAt means that axis never congested and there is nothing to unpin.
// Only ever increases *keep. Caller holds p.mu (via the Pacer methods).
func unpinKeep(keep *float64, badAt, cursor *time.Time, now time.Time) {
	if *keep >= 1 {
		*cursor = now
		return
	}
	if badAt.IsZero() {
		return
	}
	if now.Before(*badAt) {
		// A hand-advanced test clock was moved backwards; re-base rather than
		// compute a negative elapsed recovery.
		*badAt = now
		*cursor = now
		return
	}
	if now.Sub(*badAt) < pacerUnpinDelay {
		return
	}
	start := badAt.Add(pacerUnpinDelay)
	if cursor.IsZero() || cursor.Before(start) {
		*cursor = start
	}
	if now.Before(*cursor) {
		return
	}
	if dt := now.Sub(*cursor); dt > 0 {
		*keep += pacerRecoverPerSec * dt.Seconds()
		if *keep > 1 {
			*keep = 1
		}
	}
	*cursor = now
}

// Admit reports whether the next sheddable frame should be sent. Essential
// frames (parameter sets, keyframes) must bypass this entirely: dropping them
// leaves the decoder with nothing to resynchronize against.
func (p *Pacer) Admit() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	// Every send attempt also advances the unpin guarantee, so a writer that
	// keeps issuing droppable frames cannot leave recovery unevaluated.
	p.recoverRTTLocked(now)
	p.recoverAckLocked(now)
	ratio := p.effectiveLocked()
	p.accum += ratio
	if p.accum >= 1 {
		p.accum -= 1
		return true
	}
	return false
}
