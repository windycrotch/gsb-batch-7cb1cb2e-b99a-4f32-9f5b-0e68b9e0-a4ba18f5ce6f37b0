// Copyright 2026 The gogpu Authors
// SPDX-License-Identifier: MIT

package gpucontext

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

// recordingIMEHost is an IMEController plus IMEEventSource that records
// every call. A mutex makes the recorded order observable under the race
// detector when handlers run from multiple goroutines.
type recordingIMEHost struct {
	mu sync.Mutex

	enabledCalls []bool
	cursorAreas  []IMECursorArea
	contentTypes []contentTypeCall
	cancelCalls  int
}

type contentTypeCall struct {
	purpose ContentPurpose
	hints   ContentHint
}

func (h *recordingIMEHost) SetIMEEnabled(enabled bool) {
	h.mu.Lock()
	h.enabledCalls = append(h.enabledCalls, enabled)
	h.mu.Unlock()
}

func (h *recordingIMEHost) SetIMECursorArea(area IMECursorArea) {
	h.mu.Lock()
	h.cursorAreas = append(h.cursorAreas, area)
	h.mu.Unlock()
}

func (h *recordingIMEHost) SetIMEContentType(purpose ContentPurpose, hints ContentHint) {
	h.mu.Lock()
	h.contentTypes = append(h.contentTypes, contentTypeCall{purpose, hints})
	h.mu.Unlock()
}

func (h *recordingIMEHost) SetIMESurroundingText(IMESurroundingText) {}

func (h *recordingIMEHost) CancelIME() {
	h.mu.Lock()
	h.cancelCalls++
	h.mu.Unlock()
}

func (h *recordingIMEHost) IMECapabilities() IMECapabilities {
	return IMECapabilityCursorArea | IMECapabilityContentType
}

// The mock host also speaks the optional IMEEventSource; Bind is exercised
// with mockEventSource instead, so these stay no-ops.
func (*recordingIMEHost) OnIMECanceled(func())                                   {}
func (*recordingIMEHost) OnIMEDisabled(func())                                   {}
func (*recordingIMEHost) OnIMEDeleteSurrounding(func(IMEDeleteSurroundingEvent)) {}

var (
	_ IMEController  = (*recordingIMEHost)(nil)
	_ IMEEventSource = (*recordingIMEHost)(nil)
)

func (h *recordingIMEHost) areas() []IMECursorArea {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]IMECursorArea, len(h.cursorAreas))
	copy(out, h.cursorAreas)
	return out
}

func (h *recordingIMEHost) lastArea() (IMECursorArea, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.cursorAreas) == 0 {
		return IMECursorArea{}, false
	}
	return h.cursorAreas[len(h.cursorAreas)-1], true
}

func (h *recordingIMEHost) areaCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.cursorAreas)
}

// mockEventSource collects every callback registration and implements all
// optional event-source extensions.
type mockEventSource struct {
	startFn    func()
	updateFn   func(IMEState)
	endFn      func(string)
	pointerFn  func(PointerEvent)
	gestureFn  func(GestureEvent)
	cancelFn   func()
	disabledFn func()
}

func (m *mockEventSource) OnKeyPress(func(Key, Modifiers))    {}
func (m *mockEventSource) OnKeyRelease(func(Key, Modifiers))  {}
func (m *mockEventSource) OnTextInput(func(string))           {}
func (m *mockEventSource) OnMouseMove(func(float64, float64)) {}
func (m *mockEventSource) OnMousePress(func(MouseButton, float64, float64)) {
}
func (m *mockEventSource) OnMouseRelease(func(MouseButton, float64, float64)) {
}
func (m *mockEventSource) OnScroll(func(float64, float64)) {}
func (m *mockEventSource) OnResize(func(int, int))         {}
func (m *mockEventSource) OnFocus(func(bool))              {}
func (m *mockEventSource) OnIMECompositionStart(fn func()) { m.startFn = fn }
func (m *mockEventSource) OnIMECompositionUpdate(fn func(IMEState)) {
	m.updateFn = fn
}
func (m *mockEventSource) OnIMECompositionEnd(fn func(string))                    { m.endFn = fn }
func (m *mockEventSource) OnPointer(fn func(PointerEvent))                        { m.pointerFn = fn }
func (m *mockEventSource) OnGesture(fn func(GestureEvent))                        { m.gestureFn = fn }
func (m *mockEventSource) OnIMECanceled(fn func())                                { m.cancelFn = fn }
func (m *mockEventSource) OnIMEDisabled(fn func())                                { m.disabledFn = fn }
func (m *mockEventSource) OnIMEDeleteSurrounding(func(IMEDeleteSurroundingEvent)) {}

var (
	_ EventSource        = (*mockEventSource)(nil)
	_ PointerEventSource = (*mockEventSource)(nil)
	_ GestureEventSource = (*mockEventSource)(nil)
	_ IMEEventSource     = (*mockEventSource)(nil)
)

func approx(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}

func areaEqual(got, want IMECursorArea) bool {
	return approx(got.X, want.X) && approx(got.Y, want.Y) &&
		approx(got.Width, want.Width) && approx(got.Height, want.Height)
}

// validCJKState builds a composition state whose ranges are valid UTF-8
// byte offsets. "にほんご" is four three-byte runes, 12 bytes total.
func validCJKState() IMEState {
	return IMEState{
		Composing:       true,
		CompositionText: "にほんご",
		Cursor:          IMETextRange{Start: 12, End: 12},
		Selection:       IMETextRange{Start: 9, End: 12},
	}
}

// TestCoordinatorHappyPathRepositionsCandidateWindow covers the main path:
// composition starts with a recorded baseline, and every gesture frame
// during composition re-maps the caret through the latest transform and
// pushes it as logical DIP, while valid composition frames are retained.
func TestCoordinatorHappyPathRepositionsCandidateWindow(t *testing.T) {
	host := &recordingIMEHost{}
	c := NewInputSessionCoordinator(host)

	// Caret in canvas content coordinates: 10x20 size at (30, 40).
	c.SetCaretArea(IMECursorArea{X: 30, Y: 40, Width: 2, Height: 16})

	c.HandleCompositionStart()
	if !c.IsComposing() {
		t.Fatal("composition should be active after start")
	}
	// Identity transform: content DIP == window DIP.
	area, ok := host.lastArea()
	if !ok || !areaEqual(area, IMECursorArea{X: 30, Y: 40, Width: 2, Height: 16}) {
		t.Fatalf("start push = %+v ok=%v, want identity-mapped caret", area, ok)
	}

	c.HandleCompositionUpdate(validCJKState())
	state, active := c.CompositionState()
	if !active || state.CompositionText != "にほんご" {
		t.Fatalf("valid composition frame must be retained, got %+v active=%v", state, active)
	}

	// Pan by (5, -7): translation only.
	c.HandleGesture(GestureEvent{
		NumPointers:      2,
		ZoomDelta:        1,
		TranslationDelta: Point{X: 5, Y: -7},
		Center:           Point{X: 100, Y: 100},
	})
	area, _ = host.lastArea()
	if want := (IMECursorArea{X: 35, Y: 33, Width: 2, Height: 16}); !areaEqual(area, want) {
		t.Fatalf("after pan push = %+v, want %+v", area, want)
	}

	// Zoom 2x anchored at the gesture center (200, 150). After the pan the
	// transform offset is (5, -7); zooming keeps the content point under
	// the center fixed: offset' = center - (center-offset)*2.
	c.HandleGesture(GestureEvent{
		NumPointers:      2,
		ZoomDelta:        2,
		TranslationDelta: Point{},
		Center:           Point{X: 200, Y: 150},
	})
	tr := c.CurrentTransform()
	wantTransform := CanvasTransform{
		ScaleX:  2,
		ScaleY:  2,
		OffsetX: 200 - (200-5)*2, // -190
		OffsetY: 150 - (150+7)*2, // -164
	}
	if !approx(tr.ScaleX, 2) || !approx(tr.ScaleY, 2) ||
		!approx(tr.OffsetX, wantTransform.OffsetX) || !approx(tr.OffsetY, wantTransform.OffsetY) {
		t.Fatalf("transform after zoom = %+v, want %+v", tr, wantTransform)
	}
	area, _ = host.lastArea()
	wantArea := IMECursorArea{
		X:      wantTransform.OffsetX + 30*2,
		Y:      wantTransform.OffsetY + 40*2,
		Width:  2 * 2,
		Height: 16 * 2,
	}
	if !areaEqual(area, wantArea) {
		t.Fatalf("after zoom push = %+v, want %+v", area, wantArea)
	}

	// Commit: back to idle, committed text reported, no preedit retained.
	var committed string
	c.SetOnCommit(func(s string) { committed = s })
	c.HandleCompositionEnd("日本語")
	if committed != "日本語" {
		t.Fatalf("commit hook got %q, want 日本語", committed)
	}
	if c.IsComposing() {
		t.Fatal("coordinator must return to idle after commit")
	}
	if _, active := c.CompositionState(); active {
		t.Fatal("no composition state may survive a commit")
	}
}

// TestCoordinatorInvalidRangeDropsWholeFrame: illegal Cursor/Selection
// ranges discard the entire frame instead of clamping or truncating.
func TestCoordinatorInvalidRangeDropsWholeFrame(t *testing.T) {
	host := &recordingIMEHost{}
	c := NewInputSessionCoordinator(host)
	c.SetCaretArea(IMECursorArea{X: 1, Y: 2, Width: 1, Height: 10})
	c.HandleCompositionStart()

	good := validCJKState()
	c.HandleCompositionUpdate(good)
	areasAfterGood := host.areaCount()

	// Offset 10 is inside the final three-byte rune of "にほんご".
	bad := good
	bad.Cursor = IMETextRange{Start: 10, End: 12}
	if bad.IsValid() {
		t.Fatal("test setup: mid-rune cursor must be invalid")
	}
	c.HandleCompositionUpdate(bad)

	state, active := c.CompositionState()
	if !active || state.CompositionText != good.CompositionText ||
		state.Cursor != good.Cursor || state.Selection != good.Selection {
		t.Fatalf("invalid frame must be dropped whole; retained state = %+v", state)
	}
	if host.areaCount() != areasAfterGood {
		t.Fatal("dropped frame must not push a cursor area")
	}

	// An invalid selection is rejected the same way.
	badSelection := good
	badSelection.Selection = IMETextRange{Start: 0, End: 2}
	c.HandleCompositionUpdate(badSelection)
	state, _ = c.CompositionState()
	if state.Selection != good.Selection {
		t.Fatalf("invalid selection frame must be dropped; got %+v", state)
	}

	// A later valid frame is accepted again.
	c.HandleCompositionUpdate(good)
	if state, active = c.CompositionState(); !active || state.Cursor != good.Cursor {
		t.Fatalf("recovery frame must be accepted; got %+v", state)
	}

	// Hidden-cursor states remain legal: {-1,-1} is not an illegal range.
	hidden := good
	hidden.Cursor = IMETextRange{Start: -1, End: -1}
	c.HandleCompositionUpdate(hidden)
	if state, _ = c.CompositionState(); state.HasCursor() {
		t.Fatal("hidden-cursor frame should be accepted as valid")
	}
}

// TestCoordinatorCancelAndDisabledNeverCommit covers both lifecycle exits.
func TestCoordinatorCancelAndDisabledNeverCommit(t *testing.T) {
	for _, canceled := range []bool{true, false} {
		t.Run(map[bool]string{true: "canceled", false: "disabled"}[canceled], func(t *testing.T) {
			host := &recordingIMEHost{}
			c := NewInputSessionCoordinator(host)
			c.SetCaretArea(IMECursorArea{X: 7, Y: 8, Width: 1, Height: 9})

			var commits int
			var aborted int
			c.SetOnCommit(func(string) { commits++ })
			c.SetOnCancel(func() { aborted++ })
			c.SetOnDisabled(func() { aborted++ })

			c.HandleCompositionStart()
			c.HandleCompositionUpdate(validCJKState())
			areasBefore := host.areaCount()

			if canceled {
				c.HandleIMECanceled()
			} else {
				c.HandleIMEDisabled()
			}

			if commits != 0 {
				t.Fatal("cancellation and disable must never commit preedit text")
			}
			if aborted != 1 {
				t.Fatalf("lifecycle hook fired %d times, want 1", aborted)
			}
			if c.IsComposing() {
				t.Fatal("session state must be cleared")
			}
			if _, active := c.CompositionState(); active {
				t.Fatal("preedit must be cleared")
			}
			if host.areaCount() != areasBefore {
				t.Fatal("abort must not push another cursor area")
			}

			// A stray composition end after the abort cannot double-commit.
			c.HandleCompositionEnd("日本語")
			if commits != 0 {
				t.Fatal("end event after abort must be ignored")
			}

			// Later gestures must not reference the old caret: no push even
			// though the transform keeps updating.
			c.HandleGesture(GestureEvent{
				NumPointers:      2,
				ZoomDelta:        3,
				TranslationDelta: Point{X: 1, Y: 1},
				Center:           Point{X: 50, Y: 60},
			})
			if host.areaCount() != areasBefore {
				t.Fatal("gesture after abort must not re-push the stale caret")
			}

			// A brand-new composition starts from a clean caret baseline:
			// the previous caret is gone, so nothing is pushed at start.
			c.HandleCompositionStart()
			if host.areaCount() != areasBefore {
				t.Fatal("new session must not inherit the previous caret")
			}
			fresh := IMECursorArea{X: 100, Y: 200, Width: 4, Height: 12}
			c.SetCaretArea(fresh)
			// The transform persists across sessions (it describes the
			// canvas, not the text), so the fresh caret is mapped through
			// the current transform rather than treated as raw DIP.
			wantFresh := c.CurrentTransform().MapRect(fresh)
			area, ok := host.lastArea()
			if !ok || !areaEqual(area, wantFresh) {
				t.Fatalf("fresh caret on new session = %+v ok=%v, want %+v", area, ok, wantFresh)
			}
			if areaEqual(area, c.CurrentTransform().MapRect(IMECursorArea{X: 7, Y: 8, Width: 1, Height: 9})) {
				t.Fatal("the fresh session must not reuse the aborted caret")
			}
		})
	}
}

// TestCoordinatorPointerCancelAndLeaveReanchorGesture verifies that a
// pointer interruption during composition terminates the gesture baseline,
// so the following frame re-anchors rather than producing a zoom jump.
func TestCoordinatorPointerCancelAndLeaveReanchorGesture(t *testing.T) {
	for _, evType := range []PointerEventType{PointerCancel, PointerLeave} {
		t.Run(evType.String(), func(t *testing.T) {
			host := &recordingIMEHost{}
			c := NewInputSessionCoordinator(host)
			c.SetCaretArea(IMECursorArea{X: 10, Y: 10, Width: 2, Height: 8})
			c.HandleCompositionStart()

			// Establish a two-pointer baseline and move once.
			c.HandleGesture(GestureEvent{
				NumPointers:      2,
				ZoomDelta:        1,
				TranslationDelta: Point{X: 3, Y: 4},
				Center:           Point{X: 100, Y: 100},
			})
			transformBefore := c.CurrentTransform()
			areasBefore := host.areaCount()

			c.HandlePointer(PointerEvent{Type: evType, PointerID: 2})

			// A large, stale-looking delta must be ignored: the frame only
			// re-anchors, so the transform is unchanged.
			c.HandleGesture(GestureEvent{
				NumPointers:      2,
				ZoomDelta:        8,
				TranslationDelta: Point{X: 500, Y: 500},
				Center:           Point{X: 10, Y: 10},
			})
			if got := c.CurrentTransform(); got != transformBefore {
				t.Fatalf("re-anchor frame must not apply deltas: got %+v, want %+v", got, transformBefore)
			}
			// Re-anchoring still pushes the caret area for the frame, and it
			// must match the unchanged transform.
			if host.areaCount() != areasBefore+1 {
				t.Fatal("re-anchor frame during composition must still publish a caret area")
			}
			area, _ := host.lastArea()
			if want := transformBefore.MapRect(IMECursorArea{X: 10, Y: 10, Width: 2, Height: 8}); !areaEqual(area, want) {
				t.Fatalf("re-anchor push = %+v, want %+v", area, want)
			}

			// The frame after re-anchoring applies deltas normally again.
			c.HandleGesture(GestureEvent{
				NumPointers:      2,
				ZoomDelta:        1.25,
				TranslationDelta: Point{},
				Center:           Point{X: 10, Y: 10},
			})
			if got := c.CurrentTransform().ScaleX; !approx(got, 1.25) {
				t.Fatalf("post-reanchor zoom = %v, want 1.25", got)
			}
		})
	}
}

// TestCoordinatorSameFrameOrderingConverges: within one frame, whether the
// composition update or the gesture arrives first, the final pushed cursor
// area must correspond to the same transform snapshot.
func TestCoordinatorSameFrameOrderingConverges(t *testing.T) {
	gesture := GestureEvent{
		NumPointers:      2,
		ZoomDelta:        1.5,
		TranslationDelta: Point{X: 4, Y: 6},
		Center:           Point{X: 80, Y: 90},
	}
	state := validCJKState()
	caret := IMECursorArea{X: 20, Y: 24, Width: 3, Height: 11}

	run := func(gestureFirst bool) IMECursorArea {
		host := &recordingIMEHost{}
		c := NewInputSessionCoordinator(host)
		c.SetCaretArea(caret)
		c.HandleCompositionStart()
		if gestureFirst {
			c.HandleGesture(gesture)
			c.HandleCompositionUpdate(state)
		} else {
			c.HandleCompositionUpdate(state)
			c.HandleGesture(gesture)
		}
		area, _ := host.lastArea()
		return area
	}

	gestureFirst := run(true)
	updateFirst := run(false)
	if !areaEqual(gestureFirst, updateFirst) {
		t.Fatalf("frame ordering changed the pushed area: gesture-first %+v vs update-first %+v",
			gestureFirst, updateFirst)
	}

	// And the converged area must be the caret mapped through the frame's
	// transform, not the pre-gesture position.
	host := &recordingIMEHost{}
	c := NewInputSessionCoordinator(host)
	c.SetCaretArea(caret)
	c.HandleCompositionStart()
	c.HandleGesture(gesture)
	want := c.CurrentTransform().MapRect(caret)
	if !areaEqual(gestureFirst, want) {
		t.Fatalf("pushed area %+v does not match the frame transform %+v", gestureFirst, want)
	}
	if areaEqual(gestureFirst, caret) {
		t.Fatal("the pushed area must move with the gesture, not stay at the old caret")
	}
}

// TestCoordinatorConcurrentAccess is run under -race: composition, gesture,
// pointer and transform traffic from several goroutines must not race or
// block, and every observed state must stay internally consistent.
func TestCoordinatorConcurrentAccess(t *testing.T) {
	host := &recordingIMEHost{}
	c := NewInputSessionCoordinator(host)
	c.SetCaretArea(IMECursorArea{X: 5, Y: 6, Width: 1, Height: 7})

	const goroutines = 8
	const iterations = 200
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for g := 0; g < goroutines; g++ {
		g := g
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				switch g % 5 {
				case 0:
					c.HandleCompositionStart()
					c.HandleCompositionUpdate(validCJKState())
				case 1:
					c.HandleGesture(GestureEvent{
						NumPointers:      2,
						ZoomDelta:        1.01,
						TranslationDelta: Point{X: 1, Y: -1},
						Center:           Point{X: 40, Y: 40},
					})
				case 2:
					c.HandlePointer(PointerEvent{Type: PointerCancel, PointerID: i})
				case 3:
					// Draw-callback goroutine: only reads plus the
					// thread-safe IMEController forwards.
					_, _ = c.CurrentTransform(), c.IsComposing()
					if state, active := c.CompositionState(); active && !state.IsValid() {
						t.Errorf("observed an invalid retained composition state: %+v", state)
					}
					c.SetIMEEnabled(true)
					c.SetIMEContentType(ContentPurposeChat, ContentHintMultiline)
				case 4:
					c.UpdateTransform(CanvasTransform{ScaleX: 1, ScaleY: 1, OffsetX: 2, OffsetY: 3})
				}
			}
		}()
	}
	wg.Wait()

	// Every retained composition state, if one is active, must be valid.
	if state, active := c.CompositionState(); active && !state.IsValid() {
		t.Fatalf("final composition state invalid: %+v", state)
	}
	// All forwarded calls landed.
	host.mu.Lock()
	gotPurpose := len(host.contentTypes) > 0
	gotEnabled := len(host.enabledCalls) > 0
	host.mu.Unlock()
	if !gotPurpose || !gotEnabled {
		t.Fatal("cross-goroutine IMEController forwards were lost")
	}
}

// TestCoordinatorBindWiresEveryEventSource verifies the one-call
// registration across EventSource and all optional extensions, and the
// second-bind error.
func TestCoordinatorBindWiresEveryEventSource(t *testing.T) {
	host := &recordingIMEHost{}
	c := NewInputSessionCoordinator(host)
	source := &mockEventSource{}

	if err := c.Bind(source); err != nil {
		t.Fatalf("Bind returned %v", err)
	}
	if err := c.Bind(source); !errors.Is(err, ErrCoordinatorAlreadyBound) {
		t.Fatalf("second Bind = %v, want ErrCoordinatorAlreadyBound", err)
	}
	if err := c.Bind(nil); !errors.Is(err, ErrNoEventSource) {
		t.Fatalf("Bind(nil) = %v, want ErrNoEventSource", err)
	}
	if source.startFn == nil || source.updateFn == nil || source.endFn == nil ||
		source.pointerFn == nil || source.gestureFn == nil ||
		source.cancelFn == nil || source.disabledFn == nil {
		t.Fatal("Bind must register every available callback")
	}

	// Drive the coordinator purely through the registered callbacks.
	source.pointerFn(PointerEvent{Type: PointerLeave})
	source.startFn()
	source.updateFn(validCJKState())
	source.gestureFn(GestureEvent{
		NumPointers:      2,
		ZoomDelta:        1,
		TranslationDelta: Point{X: 9, Y: 0},
	})
	if !c.IsComposing() {
		t.Fatal("bound start callback must open a session")
	}
	source.cancelFn()
	if c.IsComposing() {
		t.Fatal("bound cancel callback must close the session")
	}
	source.disabledFn() // must be a no-op without an active session
}

// TestCoordinatorBindToleratesPlainEventSource: a source without the
// optional extensions still binds for composition.
func TestCoordinatorBindToleratesPlainEventSource(t *testing.T) {
	host := &recordingIMEHost{}
	c := NewInputSessionCoordinator(host)
	if err := c.Bind(NullEventSource{}); err != nil {
		t.Fatalf("binding a plain EventSource must succeed, got %v", err)
	}
	c.HandleCompositionStart()
	if !c.IsComposing() {
		t.Fatal("composition still works without pointer/gesture extensions")
	}
}

// TestCoordinatorUpdateTransformPushesDuringComposition: an authoritative
// transform from outside the gesture stream re-maps the caret immediately.
func TestCoordinatorUpdateTransformPushesDuringComposition(t *testing.T) {
	host := &recordingIMEHost{}
	c := NewInputSessionCoordinator(host)
	caret := IMECursorArea{X: 10, Y: 20, Width: 2, Height: 4}
	c.SetCaretArea(caret)
	c.HandleCompositionStart()
	areasBefore := host.areaCount()

	tr := CanvasTransform{ScaleX: 3, ScaleY: 3, OffsetX: 100, OffsetY: 200}
	c.UpdateTransform(tr)
	if got := c.CurrentTransform(); got != tr {
		t.Fatalf("CurrentTransform = %+v, want %+v", got, tr)
	}
	area, ok := host.lastArea()
	if !ok || !areaEqual(area, tr.MapRect(caret)) {
		t.Fatalf("external transform push = %+v ok=%v, want %+v", area, ok, tr.MapRect(caret))
	}

	// Outside a composition, transform updates stay silent.
	c.HandleCompositionEnd("")
	hostAreas := host.areaCount()
	c.UpdateTransform(CanvasTransform{ScaleX: 5, ScaleY: 5})
	if host.areaCount() != hostAreas {
		t.Fatal("transform updates while idle must not push cursor areas")
	}
	if areasBefore == 0 {
		t.Fatal("expected at least the composition-start push")
	}
}

// TestCoordinatorForwardsContentTypeAndEnabled covers the IMEController
// pass-through surface, including calls that never start a composition.
func TestCoordinatorForwardsContentTypeAndEnabled(t *testing.T) {
	host := &recordingIMEHost{}
	c := NewInputSessionCoordinator(host)

	c.SetIMEEnabled(true)
	c.SetIMEContentType(ContentPurposeEmail, ContentHintAutoCapitalization|ContentHintLatin)
	c.CancelIME()

	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.enabledCalls) != 1 || !host.enabledCalls[0] {
		t.Fatalf("enabled calls = %v, want [true]", host.enabledCalls)
	}
	if len(host.contentTypes) != 1 {
		t.Fatalf("content type calls = %d, want 1", len(host.contentTypes))
	}
	got := host.contentTypes[0]
	if got.purpose != ContentPurposeEmail || got.hints != (ContentHintAutoCapitalization|ContentHintLatin) {
		t.Fatalf("content type = %+v", got)
	}
	if host.cancelCalls != 1 {
		t.Fatalf("cancel calls = %d, want 1", host.cancelCalls)
	}
}

// TestCoordinatorNewRequiresController documents the fail-fast constructor.
func TestCoordinatorNewRequiresController(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("NewInputSessionCoordinator(nil) must panic")
		} else if !errors.Is(r.(error), ErrNoIMEController) {
			t.Fatalf("panic = %v, want ErrNoIMEController", r)
		}
	}()
	NewInputSessionCoordinator(nil)
}

// TestCoordinatorHooksFireOutsideStateTransitions checks hook delivery and
// ordering across the full lifecycle.
func TestCoordinatorHooksFireOutsideStateTransitions(t *testing.T) {
	host := &recordingIMEHost{}
	c := NewInputSessionCoordinator(host)

	var events []string
	c.SetOnCompositionStart(func() { events = append(events, "start") })
	c.SetOnCompositionUpdate(func(IMEState) { events = append(events, "update") })
	c.SetOnCommit(func(string) { events = append(events, "commit") })
	c.SetOnCancel(func() { events = append(events, "cancel") })
	c.SetOnDisabled(func() { events = append(events, "disabled") })

	c.HandleCompositionStart()
	c.HandleCompositionUpdate(validCJKState())
	// Invalid frames must not reach the update hook.
	bad := validCJKState()
	bad.Cursor = IMETextRange{Start: 11, End: 12}
	c.HandleCompositionUpdate(bad)
	c.HandleCompositionEnd("日本語")

	// Cancel and disabled hooks on their own sessions.
	c.HandleCompositionStart()
	c.HandleIMECanceled()
	c.HandleCompositionStart()
	c.HandleIMEDisabled()

	want := []string{"start", "update", "commit", "start", "cancel", "start", "disabled"}
	if len(events) != len(want) {
		t.Fatalf("hook events = %v, want %v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("hook events = %v, want %v", events, want)
		}
	}
}

// TestCoordinatorRejectsUpdatesWhenIdle: an update without a session is
// ignored rather than creating one.
func TestCoordinatorRejectsUpdatesWhenIdle(t *testing.T) {
	host := &recordingIMEHost{}
	c := NewInputSessionCoordinator(host)
	c.HandleCompositionUpdate(validCJKState())
	if c.IsComposing() {
		t.Fatal("an update while idle must not open a session")
	}
	if host.areaCount() != 0 {
		t.Fatal("an update while idle must not push a cursor area")
	}
}

// TestCanvasTransformMath checks the transform building blocks directly.
func TestCanvasTransformMath(t *testing.T) {
	identity := IdentityCanvasTransform()
	if identity.ScaleX != 1 || identity.ScaleY != 1 ||
		identity.OffsetX != 0 || identity.OffsetY != 0 {
		t.Fatalf("identity = %+v", identity)
	}

	// Pan.
	panned := identity.ApplyGesture(GestureEvent{
		TranslationDelta: Point{X: 10, Y: 20},
	})
	if want := (CanvasTransform{ScaleX: 1, ScaleY: 1, OffsetX: 10, OffsetY: 20}); panned != want {
		t.Fatalf("panned = %+v, want %+v", panned, want)
	}

	// Zoom anchored at (100, 100) keeps that content point fixed.
	zoomed := panned.ApplyGesture(GestureEvent{
		ZoomDelta: 2,
		Center:    Point{X: 100, Y: 100},
	})
	if !approx(zoomed.ScaleX, 2) || !approx(zoomed.ScaleY, 2) {
		t.Fatalf("zoom scale = %+v", zoomed)
	}
	fixed := zoomed.MapRect(IMECursorArea{X: 90, Y: 80, Width: 0, Height: 0})
	// Content point (90,80) under panned was window (100,100).
	if !approx(fixed.X, 100) || !approx(fixed.Y, 100) {
		t.Fatalf("anchored point moved to %+v, want (100,100)", fixed)
	}

	// Non-positive zoom deltas are ignored, panning still applies.
	guarded := zoomed.ApplyGesture(GestureEvent{
		ZoomDelta:        -1,
		TranslationDelta: Point{X: 1, Y: 1},
	})
	if !approx(guarded.ScaleX, zoomed.ScaleX) || !approx(guarded.OffsetX, zoomed.OffsetX+1) {
		t.Fatalf("invalid zoom was not ignored: %+v -> %+v", zoomed, guarded)
	}

	// Rect mapping scales size as well as position.
	rect := CanvasTransform{ScaleX: 2, ScaleY: 3, OffsetX: 10, OffsetY: 40}.
		MapRect(IMECursorArea{X: 5, Y: 6, Width: 7, Height: 8})
	want := IMECursorArea{X: 20, Y: 58, Width: 14, Height: 24}
	if !areaEqual(rect, want) {
		t.Fatalf("MapRect = %+v, want %+v", rect, want)
	}

	// Negative scales flip the rect; the mapped result keeps a
	// non-negative width and height with the flipped top-left corner.
	flipped := CanvasTransform{ScaleX: -2, ScaleY: -3, OffsetX: 100, OffsetY: 200}.
		MapRect(IMECursorArea{X: 5, Y: 6, Width: 7, Height: 8})
	wantFlipped := IMECursorArea{X: 76, Y: 158, Width: 14, Height: 24}
	if !areaEqual(flipped, wantFlipped) {
		t.Fatalf("flipped MapRect = %+v, want %+v", flipped, wantFlipped)
	}
}

// TestCoordinatorAtomicCounterSanity keeps the race test's atomic import
// honest by also exercising a high-volume session loop.
func TestCoordinatorHighVolumeSessionLoop(t *testing.T) {
	host := &recordingIMEHost{}
	c := NewInputSessionCoordinator(host)
	c.SetCaretArea(IMECursorArea{X: 1, Y: 1, Width: 1, Height: 1})

	var commits atomic.Int64
	c.SetOnCommit(func(string) { commits.Add(1) })

	const rounds = 500
	for i := 0; i < rounds; i++ {
		c.HandleCompositionStart()
		c.HandleCompositionUpdate(validCJKState())
		c.HandleGesture(GestureEvent{
			NumPointers:      2,
			ZoomDelta:        1.001,
			TranslationDelta: Point{X: 0.1, Y: 0},
			Center:           Point{X: 50, Y: 50},
		})
		c.HandleCompositionEnd("x")
		if c.IsComposing() {
			t.Fatalf("round %d did not return to idle", i)
		}
	}
	if commits.Load() != rounds {
		t.Fatalf("commits = %d, want %d", commits.Load(), rounds)
	}
}
