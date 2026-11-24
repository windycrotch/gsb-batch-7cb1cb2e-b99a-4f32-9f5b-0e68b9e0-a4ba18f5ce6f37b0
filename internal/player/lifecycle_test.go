package player

import (
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/greed/cadence/internal/audio"
)

// gatedPlayer builds a player whose resolver blocks on "slow" identifiers until
// the gate is released, so preload resolution races can be driven deterministically.
func gatedPlayer(lengths map[string]float64, g gate) (*Player, *recorder) {
	rec := &recorder{}
	p := New(Config{
		GuildID:      "guild-1",
		BufferFrames: 200,
		Open:         opener(lengths),
		Resolve:      g,
		OnEvent:      rec.add,
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return p, rec
}

func eventsOf(rec *recorder, typ EventType, inst uint64) []Event {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	var out []Event
	for _, e := range rec.events {
		if e.Type == typ && e.Instance == inst {
			out = append(out, e)
		}
	}
	return out
}

func indexOf(rec *recorder, typ EventType, inst uint64) int {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for i, e := range rec.events {
		if e.Type == typ && e.Instance == inst {
			return i
		}
	}
	return -1
}

// waitFor blocks until an event of the given type and instance is dispatched.
func waitFor(t *testing.T, rec *recorder, typ EventType, inst uint64) Event {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		rec.mu.Lock()
		for _, e := range rec.events {
			if e.Type == typ && e.Instance == inst {
				rec.mu.Unlock()
				return e
			}
		}
		rec.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no %s event for instance %d", typ, inst)
	return Event{}
}

// drainFrames pumps the player until no frame is produced for two consecutive
// polls or the deadline elapses, returning the number of frames produced.
func drainFrames(p *Player, out *audio.Frame, deadline time.Time) int {
	empty := 0
	produced := 0
	for time.Now().Before(deadline) && empty < 2 {
		if info, ok := p.NextFrame(out); ok {
			produced++
			empty = 0
			_ = info
		} else {
			empty++
			time.Sleep(2 * time.Millisecond)
		}
	}
	return produced
}

// A natural handoff to the preloaded track must emit exactly one end event for
// the old instance, and the new instance's TrackStart and first TrackAudible must
// both be ordered after that end event.
func TestGaplessHandoffKeepsInstancesBoundToDecks(t *testing.T) {
	p, rec := newTestPlayer(map[string]float64{"a": 0.4, "b": 3})
	defer p.Destroy()
	aInst := p.Play(track("a"), 0, false)
	rec.wait(t, EventTrackStart)
	bInst, err := p.SetNext(track("b"))
	if err != nil {
		t.Fatal(err)
	}
	if aInst == bInst {
		t.Fatal("preloaded track reused the active instance number")
	}
	time.Sleep(120 * time.Millisecond)
	out := &audio.Frame{}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		info, ok := p.NextFrame(out)
		if ok && info.Instance == bInst {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("never reached the preloaded track")
		}
	}

	aEnds := eventsOf(rec, EventTrackEnd, aInst)
	if len(aEnds) != 1 {
		t.Fatalf("old track produced %d end events, want exactly 1", len(aEnds))
	}
	if aEnds[0].Reason != EndFinished || aEnds[0].Track.Identifier != "a" {
		t.Fatalf("old end event %+v", aEnds[0])
	}
	endIdx := indexOf(rec, EventTrackEnd, aInst)
	if endIdx < 0 {
		t.Fatal("old track end event missing")
	}
	if len(eventsOf(rec, EventTrackStart, aInst)) != 1 {
		t.Fatal("old instance was re-announced after handoff")
	}

	// The client adopts the new track; its TrackStart and first TrackAudible must
	// both be ordered after the old track's end event.
	p.Play(track("b"), 0, false)
	startWait := time.Now().Add(2 * time.Second)
	for time.Now().Before(startWait) && indexOf(rec, EventTrackStart, bInst) < 0 {
		time.Sleep(5 * time.Millisecond)
	}
	startIdx := indexOf(rec, EventTrackStart, bInst)
	if startIdx < 0 {
		t.Fatal("new track never announced")
	}
	if startIdx <= endIdx {
		t.Fatalf("new TrackStart at %d must follow the old TrackEnd at %d", startIdx, endIdx)
	}
	for i := 0; i < 5; i++ {
		info, ok := p.NextFrame(out)
		if !ok {
			time.Sleep(2 * time.Millisecond)
			continue
		}
		p.Sent(info, time.Now())
	}
	audibleWait := time.Now().Add(2 * time.Second)
	for time.Now().Before(audibleWait) && len(eventsOf(rec, EventTrackAudible, bInst)) == 0 {
		info, ok := p.NextFrame(out)
		if ok {
			p.Sent(info, time.Now())
		}
		time.Sleep(2 * time.Millisecond)
	}
	audibles := eventsOf(rec, EventTrackAudible, bInst)
	if len(audibles) != 1 {
		t.Fatalf("new track produced %d audible events, want exactly 1", len(audibles))
	}
	audIdx := indexOf(rec, EventTrackAudible, bInst)
	if audIdx <= endIdx || audIdx <= startIdx {
		t.Fatalf("audible at %d must follow end at %d and start at %d", audIdx, endIdx, startIdx)
	}
	if len(eventsOf(rec, EventTrackEnd, bInst)) != 0 {
		t.Fatal("the surviving deck received an end event")
	}
}

// Every event that leaves the player must name the guild it belongs to.
func TestEveryEventCarriesGuildAndInstance(t *testing.T) {
	p, rec := newTestPlayer(map[string]float64{"a": 0.3, "b": 3})
	defer p.Destroy()
	aInst := p.Play(track("a"), 0, false)
	bInst, err := p.SetNext(track("b"))
	if err != nil {
		t.Fatal(err)
	}
	out := &audio.Frame{}
	drainFrames(p, out, time.Now().Add(2*time.Second))
	rec.mu.Lock()
	events := append([]Event(nil), rec.events...)
	rec.mu.Unlock()
	if len(events) == 0 {
		t.Fatal("no events were emitted")
	}
	for _, e := range events {
		if e.GuildID != "1" {
			t.Fatalf("%s event has guild %q", e.Type, e.GuildID)
		}
		if e.Instance != aInst && e.Instance != bInst {
			t.Fatalf("%s event has instance %d, want %d or %d", e.Type, e.Instance, aInst, bInst)
		}
	}
}

// A play request landing in the middle of a running fade must close the replaced
// Deck B and never let it promote into the current track.
func TestPlayDuringFadeClosesReplacedDeckB(t *testing.T) {
	p, rec := newTestPlayer(map[string]float64{"a": 8, "b": 8, "c": 8})
	defer p.Destroy()
	aInst := p.Play(track("a"), 0, false)
	rec.wait(t, EventTrackStart)
	bInst, err := p.LoadDeckB(track("b"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.StartCrossfade(CrossfadeRequest{DurationMs: 3000, Curve: "LINEAR"}); err != nil {
		t.Fatal(err)
	}
	rec.wait(t, EventMixStarted)
	out := &audio.Frame{}
	for i := 0; i < 8; i++ {
		frame(t, p, out)
	}
	cInst := p.Play(track("c"), 0, false)
	if cInst == bInst || cInst == aInst {
		t.Fatalf("replacement kept a stale instance: %d", cInst)
	}
	rec.wait(t, EventTrackLoading)

	// Pump hard: the replaced Deck B must never promote even if the fade timer
	// would otherwise have finished now.
	for i := 0; i < 200; i++ {
		p.NextFrame(out)
	}
	if got := eventsOf(rec, EventMixPromoted, bInst); len(got) != 0 {
		t.Fatalf("replaced Deck B promoted %d times", len(got))
	}
	waitFor(t, rec, EventMixDropped, bInst)
	if dropped := eventsOf(rec, EventMixDropped, bInst); len(dropped) != 1 {
		t.Fatalf("replaced Deck B produced %d drop events, want 1", len(dropped))
	}
	waitFor(t, rec, EventTrackEnd, aInst)
	if ends := eventsOf(rec, EventTrackEnd, aInst); len(ends) != 1 || ends[0].Reason != EndReplaced {
		t.Fatalf("old deck end events: %+v", ends)
	}
	rec.wait(t, EventTrackStart)
	s := p.State()
	if s.Track == nil || s.Track.Identifier != "c" || s.Instance != cInst {
		t.Fatalf("state after replacing the fade %+v", s)
	}
	if s.Mixing || s.MixInto != nil {
		t.Fatalf("state still reports a dead mix: %+v", s)
	}
	if s.NextTrack != nil {
		t.Fatalf("a dead preload survived: %+v", s.NextTrack)
	}
}

// A seek during a running fade promotes the incoming deck (existing behaviour)
// and state afterwards must describe the surviving deck only.
func TestStateAfterFadeCompletionMatchesSurvivingDeck(t *testing.T) {
	p, rec := newTestPlayer(map[string]float64{"a": 6, "b": 6})
	defer p.Destroy()
	aInst := p.Play(track("a"), 0, false)
	rec.wait(t, EventTrackStart)
	bInst, err := p.LoadDeckB(track("b"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.StartCrossfade(CrossfadeRequest{DurationMs: 200, Curve: "LINEAR"}); err != nil {
		t.Fatal(err)
	}
	out := &audio.Frame{}
	promotedWait := time.Now().Add(3 * time.Second)
	for time.Now().Before(promotedWait) && indexOf(rec, EventMixPromoted, aInst) < 0 {
		frame(t, p, out)
	}
	if got := eventsOf(rec, EventMixPromoted, aInst); len(got) != 1 {
		t.Fatalf("exactly one promotion event expected, got %d", len(got))
	}
	// The old deck's end event stays bound to the old deck's own instance.
	waitFor(t, rec, EventTrackEnd, aInst)
	if got := eventsOf(rec, EventTrackEnd, aInst); len(got) != 1 || got[0].Reason != EndReplaced {
		t.Fatalf("faded-out deck end events: %+v", got)
	}
	info := frame(t, p, out)
	if info.Instance != bInst {
		t.Fatalf("frame after promotion carries instance %d, want %d", info.Instance, bInst)
	}
	s := p.State()
	if s.Track == nil || s.Track.Identifier != "b" || s.Instance != bInst {
		t.Fatalf("current track state %+v", s)
	}
	if s.Mixing || s.MixInto != nil || s.MixProgress != 0 {
		t.Fatalf("state still describes a finished fade: %+v", s)
	}
	if s.NextTrack != nil || s.DeckBLoaded {
		t.Fatalf("state references a deck that no longer exists: %+v", s)
	}
}

// A slow SetNext preload that resolves after the current track was replaced must
// neither reappear as "next" nor be promoted onto the new current track.
func TestSlowPreloadAfterPlayIsDiscarded(t *testing.T) {
	g := gate{release: make(chan struct{})}
	p, rec := gatedPlayer(map[string]float64{"a": 5, "slow": 5, "c": 5}, g)
	defer p.Destroy()
	p.Play(track("a"), 0, false)
	rec.wait(t, EventTrackStart)
	done := make(chan error, 1)
	go func() {
		_, err := p.SetNext(track("slow"))
		done <- err
	}()
	time.Sleep(30 * time.Millisecond)
	p.Play(track("c"), 0, false)
	rec.wait(t, EventTrackLoading)
	close(g.release)
	if err := <-done; !errors.Is(err, ErrNextSuperseded) {
		t.Fatalf("stale preload returned %v, want ErrNextSuperseded", err)
	}
	time.Sleep(30 * time.Millisecond)
	if st := p.State(); st.NextTrack != nil {
		t.Fatalf("stale preload reappeared: %+v", st.NextTrack)
	}
	out := &audio.Frame{}
	frame(t, p, out)
	if st := p.State(); st.Track == nil || st.Track.Identifier != "c" {
		t.Fatalf("stale preload displaced the current track: %+v", st.Track)
	}
}

// Stopping while a preload is still resolving must leave the player empty.
func TestSlowPreloadAfterStopIsDiscarded(t *testing.T) {
	g := gate{release: make(chan struct{})}
	p, rec := gatedPlayer(map[string]float64{"a": 5, "slow": 5}, g)
	defer p.Destroy()
	p.Play(track("a"), 0, false)
	rec.wait(t, EventTrackStart)
	done := make(chan error, 1)
	go func() {
		_, err := p.SetNext(track("slow"))
		done <- err
	}()
	time.Sleep(30 * time.Millisecond)
	p.Stop()
	close(g.release)
	if err := <-done; !errors.Is(err, ErrNextSuperseded) {
		t.Fatalf("stale preload after stop returned %v", err)
	}
	time.Sleep(30 * time.Millisecond)
	if st := p.State(); st.Track != nil || st.NextTrack != nil {
		t.Fatalf("player not empty after stop: %+v", st)
	}
}

// Destroying the player while a preload is resolving must swallow the preload and
// emit no events after destruction.
func TestDestroyDuringSlowPreloadEmitsNothing(t *testing.T) {
	g := gate{release: make(chan struct{})}
	p, rec := gatedPlayer(map[string]float64{"a": 5, "slow": 5}, g)
	p.Play(track("a"), 0, false)
	rec.wait(t, EventTrackStart)
	done := make(chan error, 1)
	go func() {
		_, err := p.SetNext(track("slow"))
		done <- err
	}()
	time.Sleep(30 * time.Millisecond)
	p.Destroy()
	// Wait for the dispatcher to flush Destroy's own cleanup event.
	flushWait := time.Now().Add(2 * time.Second)
	for time.Now().Before(flushWait) && len(eventsOf(rec, EventTrackEnd, 1)) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	before := len(rec.events)
	close(g.release)
	if err := <-done; err == nil {
		t.Fatal("preload after destroy must fail")
	}
	time.Sleep(50 * time.Millisecond)
	if len(rec.events) != before {
		rec.mu.Lock()
		later := rec.events[before:]
		rec.mu.Unlock()
		t.Fatalf("destroy leaked events: %+v", later)
	}
}

// LoadDeckB has the same race: a slow resolution must not arm a Deck B after the
// player moved on to another track.
func TestSlowDeckBAfterPlayIsDiscarded(t *testing.T) {
	g := gate{release: make(chan struct{})}
	p, rec := gatedPlayer(map[string]float64{"a": 5, "slow": 5, "c": 5}, g)
	defer p.Destroy()
	p.Play(track("a"), 0, false)
	rec.wait(t, EventTrackStart)
	done := make(chan error, 1)
	go func() {
		_, err := p.LoadDeckB(track("slow"), 0)
		done <- err
	}()
	time.Sleep(30 * time.Millisecond)
	p.Play(track("c"), 0, false)
	rec.wait(t, EventTrackLoading)
	close(g.release)
	if err := <-done; !errors.Is(err, ErrNextSuperseded) {
		t.Fatalf("stale Deck B returned %v, want ErrNextSuperseded", err)
	}
	time.Sleep(30 * time.Millisecond)
	if st := p.State(); st.DeckBLoaded || st.MixArmedAtMs != nil {
		t.Fatalf("stale Deck B became armed: %+v", st)
	}
	if err := p.StartCrossfade(CrossfadeRequest{DurationMs: 100, Curve: "LINEAR"}); !errors.Is(err, ErrNoDeckB) {
		t.Fatalf("ghost Deck B accepted a crossfade: %v", err)
	}
}

// A slow Deck B whose resolution finishes after it was armed into a fade must not
// re-arm itself behind the running mix.
func TestSlowDeckBAfterCrossfadeIsDiscarded(t *testing.T) {
	g := gate{release: make(chan struct{})}
	p, rec := gatedPlayer(map[string]float64{"a": 5, "slow": 5, "b": 5}, g)
	defer p.Destroy()
	p.Play(track("a"), 0, false)
	rec.wait(t, EventTrackStart)
	done := make(chan error, 1)
	go func() {
		_, err := p.LoadDeckB(track("slow"), 0)
		done <- err
	}()
	time.Sleep(30 * time.Millisecond)
	// A fresh, fast Deck B replaces the slow one and starts a fade.
	if _, err := p.LoadDeckB(track("b"), 0); err != nil {
		t.Fatal(err)
	}
	if err := p.StartCrossfade(CrossfadeRequest{DurationMs: 3000, Curve: "LINEAR"}); err != nil {
		t.Fatal(err)
	}
	close(g.release)
	if err := <-done; !errors.Is(err, ErrNextSuperseded) {
		t.Fatalf("superseded Deck B returned %v", err)
	}
	if st := p.State(); !st.Mixing || st.MixInto == nil || st.MixInto.Identifier != "b" {
		t.Fatalf("stale Deck B disturbed the running mix: %+v", st)
	}
}

// Replacing a track whose load has not finished must end the still-loading track
// under its own instance number, not the previous deck's.
func TestReplacedLoadingTrackEndsUnderItsOwnInstance(t *testing.T) {
	g := gate{release: make(chan struct{})}
	p, rec := gatedPlayer(map[string]float64{"slow": 5, "c": 5}, g)
	defer p.Destroy()
	slowInst := p.Play(track("slow"), 0, false)
	loading := rec.wait(t, EventTrackLoading)
	if loading.Instance != slowInst {
		t.Fatalf("loading event instance %d, want %d", loading.Instance, slowInst)
	}
	cInst := p.Play(track("c"), 0, false)
	if cInst == slowInst {
		t.Fatal("replacement reused the loading instance number")
	}
	close(g.release)
	rec.wait(t, EventTrackStart)
	ends := eventsOf(rec, EventTrackEnd, slowInst)
	if len(ends) != 1 {
		t.Fatalf("loading track produced %d end events, want 1", len(ends))
	}
	if ends[0].Track == nil || ends[0].Track.Identifier != "slow" || ends[0].Reason != EndReplaced {
		t.Fatalf("loading end event %+v", ends[0])
	}
	for _, e := range eventsOf(rec, EventTrackEnd, cInst) {
		t.Fatalf("new track received an end event it never earned: %+v", e)
	}
}

// Stop and Destroy on a still-loading track likewise bind the end event to the
// loading instance.
func TestStopAndDestroyLoadingTrackUseLoadingInstance(t *testing.T) {
	g := gate{release: make(chan struct{})}
	p, rec := gatedPlayer(map[string]float64{"slow": 5}, g)
	slowInst := p.Play(track("slow"), 0, false)
	rec.wait(t, EventTrackLoading)
	p.Stop()
	waitFor(t, rec, EventTrackEnd, slowInst)
	ends := eventsOf(rec, EventTrackEnd, slowInst)
	if len(ends) != 1 || ends[0].Reason != EndStopped {
		t.Fatalf("stop end events: %+v", ends)
	}
	close(g.release)
	time.Sleep(50 * time.Millisecond)
	if extra := eventsOf(rec, EventTrackEnd, slowInst); len(extra) != 1 {
		t.Fatalf("late resolve produced another end event: %d total", len(extra))
	}
	// The late resolution must not resurrect the stopped player.
	if got := eventsOf(rec, EventTrackStart, slowInst); len(got) != 0 {
		t.Fatalf("stopped track came back to life: %d start events", len(got))
	}
	if st := p.State(); st.Track != nil || st.Instance != 0 {
		t.Fatalf("stopped player was repopulated by a stale load: %+v", st)
	}
	p.Destroy()

	g2 := gate{release: make(chan struct{})}
	p2, rec2 := gatedPlayer(map[string]float64{"slow": 5}, g2)
	inst2 := p2.Play(track("slow"), 0, false)
	rec2.wait(t, EventTrackLoading)
	p2.Destroy()
	waitFor(t, rec2, EventTrackEnd, inst2)
	close(g2.release)
	time.Sleep(30 * time.Millisecond)
	ends2 := eventsOf(rec2, EventTrackEnd, inst2)
	if len(ends2) != 1 || ends2[0].Reason != EndCleanup {
		t.Fatalf("destroy end events: %+v", ends2)
	}
}

// After Stop, state must not advertise a live deck, and a later Play must get a
// fresh instance number whose events can never be confused with the stopped one.
func TestPlayAfterStopAllocatesFreshInstanceAndCleanState(t *testing.T) {
	p, rec := newTestPlayer(map[string]float64{"a": 5, "c": 5})
	defer p.Destroy()
	aInst := p.Play(track("a"), 0, false)
	rec.wait(t, EventTrackStart)
	p.Stop()
	waitFor(t, rec, EventTrackEnd, aInst)
	if st := p.State(); st.Track != nil || st.Instance != 0 {
		t.Fatalf("state after stop still advertises a deck: %+v", st)
	}
	cInst := p.Play(track("c"), 0, false)
	if cInst == aInst {
		t.Fatalf("new play reused stopped instance %d", aInst)
	}
	rec.wait(t, EventTrackStart)
	if st := p.State(); st.Instance != cInst || st.Track == nil || st.Track.Identifier != "c" {
		t.Fatalf("state after restart: %+v", st)
	}
	if len(eventsOf(rec, EventTrackEnd, aInst)) != 1 {
		t.Fatal("stopped instance received additional end events")
	}
}

// A seek while a fade is running lands on the incoming deck; every event of the
// handoff stays bound to the deck it describes and the promotion happens once.
func TestSeekDuringFadeKeepsEventInstancesBound(t *testing.T) {
	p, rec := newTestPlayer(map[string]float64{"a": 6, "b": 6})
	defer p.Destroy()
	aInst := p.Play(track("a"), 0, false)
	rec.wait(t, EventTrackStart)
	bInst, err := p.LoadDeckB(track("b"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.StartCrossfade(CrossfadeRequest{DurationMs: 3000, Curve: "LINEAR"}); err != nil {
		t.Fatal(err)
	}
	out := &audio.Frame{}
	for i := 0; i < 8; i++ {
		frame(t, p, out)
	}
	rec.wait(t, EventMixStarted)
	if err := p.SeekTo(2500); err != nil {
		t.Fatal(err)
	}
	waitFor(t, rec, EventMixPromoted, aInst)
	waitFor(t, rec, EventTrackEnd, aInst)
	if got := eventsOf(rec, EventMixPromoted, aInst); len(got) != 1 {
		t.Fatalf("promotion fired %d times", len(got))
	}
	if got := eventsOf(rec, EventTrackEnd, aInst); len(got) != 1 {
		t.Fatalf("old deck end fired %d times", len(got))
	}
	if got := eventsOf(rec, EventMixPromoted, bInst); len(got) != 0 {
		t.Fatal("the surviving deck was itself promoted")
	}
	jump := waitFor(t, rec, EventPositionJump, bInst)
	if jump.PositionMs != 2500 {
		t.Fatalf("position jump %+v", jump)
	}
	info := frame(t, p, out)
	if info.Instance != bInst {
		t.Fatalf("frame after seek carries instance %d, want %d", info.Instance, bInst)
	}
}

// Full ordering contract of a crossfade handoff driven to natural completion:
// start/mix events use the old instance, promotion and end stay on the old
// instance, and TrackStart for the incoming deck comes last, once.
func TestCrossfadeHandoffEventOrderAndInstances(t *testing.T) {
	p, rec := newTestPlayer(map[string]float64{"a": 4, "b": 4})
	defer p.Destroy()
	aInst := p.Play(track("a"), 0, false)
	rec.wait(t, EventTrackStart)
	bInst, err := p.LoadDeckB(track("b"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.StartCrossfade(CrossfadeRequest{DurationMs: 200, Curve: "LINEAR"}); err != nil {
		t.Fatal(err)
	}
	out := &audio.Frame{}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && indexOf(rec, EventMixPromoted, aInst) < 0 {
		p.NextFrame(out)
		time.Sleep(time.Millisecond)
	}
	waitFor(t, rec, EventTrackEnd, aInst)
	waitFor(t, rec, EventTrackStart, bInst)

	started := waitFor(t, rec, EventMixStarted, aInst)
	if started.Into == nil || started.Into.Identifier != "b" {
		t.Fatalf("mix started event %+v", started)
	}
	idxStart := indexOf(rec, EventMixStarted, aInst)
	idxPromo := indexOf(rec, EventMixPromoted, aInst)
	idxEnd := indexOf(rec, EventTrackEnd, aInst)
	idxNewStart := indexOf(rec, EventTrackStart, bInst)
	if !(idxStart < idxPromo && idxPromo < idxEnd && idxEnd < idxNewStart) {
		t.Fatalf("bad ordering: started=%d promoted=%d end=%d newStart=%d", idxStart, idxPromo, idxEnd, idxNewStart)
	}
	if len(eventsOf(rec, EventTrackStart, aInst)) != 1 {
		t.Fatal("old deck was re-announced")
	}
	if len(eventsOf(rec, EventTrackStart, bInst)) != 1 {
		t.Fatalf("new deck announced %d times", len(eventsOf(rec, EventTrackStart, bInst)))
	}
	if len(eventsOf(rec, EventMixPromoted, aInst)) != 1 || len(eventsOf(rec, EventTrackEnd, aInst)) != 1 {
		t.Fatal("duplicate promotion or end events")
	}
	// No mix/next state may survive the handoff.
	s := p.State()
	if s.Mixing || s.MixInto != nil || s.NextTrack != nil || s.DeckBLoaded {
		t.Fatalf("state after handoff references dead decks: %+v", s)
	}
	if s.Instance != bInst {
		t.Fatalf("state instance %d, want %d", s.Instance, bInst)
	}
}

// Repeating SetNext while an earlier preload is still resolving must never let
// the older track win: the older request is superseded and only the newest deck
// remains installed.
func TestConcurrentSetNextOnlyNewestSurvives(t *testing.T) {
	g := gate{release: make(chan struct{})}
	p, rec := gatedPlayer(map[string]float64{"a": 5, "slow": 5, "fast": 5}, g)
	defer p.Destroy()
	p.Play(track("a"), 0, false)
	rec.wait(t, EventTrackStart)
	slowDone := make(chan error, 1)
	go func() {
		_, err := p.SetNext(track("slow"))
		slowDone <- err
	}()
	time.Sleep(30 * time.Millisecond)
	// The second request supersedes the first; it resolves once the gate opens.
	fastDone := make(chan error, 1)
	go func() {
		_, err := p.SetNext(track("fast"))
		fastDone <- err
	}()
	time.Sleep(30 * time.Millisecond)
	close(g.release)
	if err := <-slowDone; !errors.Is(err, ErrNextSuperseded) {
		t.Fatalf("older preload: %v", err)
	}
	// The newest request is the last to bump nextGen, so it installs cleanly.
	if err := <-fastDone; err != nil {
		t.Fatalf("newest preload should install: %v", err)
	}
	time.Sleep(30 * time.Millisecond)
	if st := p.State(); st.NextTrack == nil || st.NextTrack.Identifier != "fast" {
		t.Fatalf("next track state: %+v", st.NextTrack)
	}
}

// A preload resolving after the current deck handed over via a crossfade must not
// attach to the new current deck: the active instance changed without the
// next-generation counter necessarily moving, so the instance snapshot matters.
func TestSlowPreloadAfterCrossfadePromotionIsDiscarded(t *testing.T) {
	g := gate{release: make(chan struct{})}
	p, rec := gatedPlayer(map[string]float64{"a": 4, "b": 4, "slow": 4}, g)
	defer p.Destroy()
	aInst := p.Play(track("a"), 0, false)
	rec.wait(t, EventTrackStart)
	done := make(chan error, 1)
	go func() {
		_, err := p.SetNext(track("slow"))
		done <- err
	}()
	time.Sleep(30 * time.Millisecond)
	// Crossfade to Deck B and complete it: the active instance changes while the
	// preload is still resolving.
	if _, err := p.LoadDeckB(track("b"), 0); err != nil {
		t.Fatal(err)
	}
	if err := p.StartCrossfade(CrossfadeRequest{DurationMs: 200, Curve: "LINEAR"}); err != nil {
		t.Fatal(err)
	}
	out := &audio.Frame{}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && indexOf(rec, EventMixPromoted, aInst) < 0 {
		p.NextFrame(out)
		time.Sleep(time.Millisecond)
	}
	bInst := p.State().Instance
	if bInst == 0 || bInst == aInst {
		t.Fatalf("crossfade did not promote: state instance %d", bInst)
	}
	close(g.release)
	if err := <-done; !errors.Is(err, ErrNextSuperseded) {
		t.Fatalf("preload across promotion returned %v, want ErrNextSuperseded", err)
	}
	time.Sleep(30 * time.Millisecond)
	if st := p.State(); st.NextTrack != nil {
		t.Fatalf("stale preload attached to the promoted deck: %+v", st.NextTrack)
	}
	if st := p.State(); st.Track == nil || st.Track.Identifier != "b" {
		t.Fatalf("promoted track was disturbed: %+v", st.Track)
	}
}
