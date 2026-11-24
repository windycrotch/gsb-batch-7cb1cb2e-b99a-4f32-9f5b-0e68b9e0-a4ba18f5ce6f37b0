// Copyright 2026 The gogpu Authors
// SPDX-License-Identifier: MIT

package gpucontext

import (
	"errors"
	"sync"
)

// Input session coordination.
//
// A frameless-window canvas receives two otherwise independent input
// chains: IME composition (EventSource's OnIMEComposition* callbacks plus
// the IMEController that positions the candidate window) and touchpad
// gestures (PointerEventSource and the per-frame GestureEventSource).
// Without a shared state axis, a pinch that pans or zooms the canvas while
// the user is composing CJK text leaves the candidate window at the caret's
// old position, and composition byte ranges can reference stale text.
//
// InputSessionCoordinator puts both chains on one state axis — the current
// CanvasTransform — and owns the composition lifecycle:
//
//   - At composition start it records the transform and the caret rect in
//     canvas content coordinates.
//   - Every gesture frame (or external transform update) during a
//     composition re-maps the caret to window logical DIP with the latest
//     transform and pushes it through IMEController.SetIMECursorArea, so
//     the candidate window tracks the canvas.
//   - Composition Cursor/Selection ranges are validated as UTF-8 byte
//     ranges in CompositionText; an invalid frame is dropped whole, never
//     truncated or clamped (see IMETextRange.IsValid).
//   - OnIMECanceled and OnIMEDisabled clear the preedit without committing
//     it; PointerCancel and PointerLeave invalidate the gesture baseline so
//     the next frame cannot compute a zoom jump from a stale centroid.
//
// All state is guarded by one mutex and every IMEController call happens
// outside the lock. The coordinator starts no goroutines and never blocks a
// caller waiting for a platform thread; it is safe to drive from a draw
// callback goroutine as well as the event-loop thread.
var (
	// ErrCoordinatorAlreadyBound is returned by Bind when the coordinator
	// already registered on an EventSource.
	ErrCoordinatorAlreadyBound = errors.New("gpucontext: input session coordinator is already bound to an event source")

	// ErrNoIMEController is returned when a coordinator is constructed or
	// bound without the IMEController it must push cursor areas through.
	ErrNoIMEController = errors.New("gpucontext: input session coordinator requires a non-nil IMEController")

	// ErrNoEventSource is returned by Bind when no EventSource is supplied.
	ErrNoEventSource = errors.New("gpucontext: input session coordinator requires a non-nil EventSource")
)

// gestureBaseline is the per-gesture reference used to detect that the set
// of active pointers changed between frames. The recognizer emits per-frame
// deltas, so the very first frame is applied as-is; afterwards a pointer
// cancel, leave, down or up marks the baseline broken, and the next frame
// re-establishes it instead of applying a delta computed against a centroid
// that no longer exists.
type gestureBaseline struct {
	// hasFrame says whether a predecessor gesture frame exists.
	hasFrame bool

	// broken says the predecessor was invalidated by a pointer event.
	broken bool

	// numPointers is the active pointer count of the predecessor frame.
	numPointers int
}

// InputSessionCoordinator synchronizes IME composition with canvas
// zoom-and-pan gestures. Create one with NewInputSessionCoordinator,
// register its handlers with Bind (or call the Handle* methods directly),
// and feed canvas geometry through SetCaretArea, HandleGesture and
// UpdateTransform.
//
// InputSessionCoordinator is safe for concurrent use. The zero value is not
// usable; use the constructor.
type InputSessionCoordinator struct {
	mu  sync.RWMutex
	ime IMEController

	// bound guards against registering the handlers twice.
	bound bool

	// transform is the shared state axis: the latest canvas transform,
	// updated by gestures and external transform publishers.
	transform CanvasTransform

	// baseline tracks whether the next gesture frame has a matching
	// predecessor. Its zero value means "no frame seen yet", which is why
	// the constructor does not initialize it.
	baseline gestureBaseline

	// Session state. A session runs from composition start to end,
	// cancellation or disable.
	composing bool
	preedit   IMEState

	// caret is the caret rectangle in canvas content coordinates recorded
	// for the active (or next) composition; haveCaret says whether one has
	// ever been supplied. It is cleared when a session terminates so the
	// next composition starts from a clean baseline.
	caret     IMECursorArea
	haveCaret bool

	// Optional consumer hooks, invoked outside the mutex after state has
	// been updated.
	onStart    func()
	onUpdate   func(IMEState)
	onCommit   func(string)
	onCancel   func()
	onDisabled func()
}

// NewInputSessionCoordinator creates a coordinator that pushes candidate
// window placement and IME configuration through ime. The coordinator
// starts with the identity canvas transform.
func NewInputSessionCoordinator(ime IMEController) *InputSessionCoordinator {
	if ime == nil {
		panic(ErrNoIMEController)
	}
	return &InputSessionCoordinator{
		ime:       ime,
		transform: IdentityCanvasTransform(),
	}
}

// Bind registers the coordinator's handlers on source and, when source also
// implements the optional pointer, gesture and IME-lifecycle extensions, on
// those as well. The composition callbacks of EventSource are always
// registered.
//
// Bind returns ErrCoordinatorAlreadyBound on a second call; construct a new
// coordinator instead of rebinding. Handler registration itself does not
// invoke any callback, so Bind does not take the platform thread.
func (c *InputSessionCoordinator) Bind(source EventSource) error {
	if source == nil {
		return ErrNoEventSource
	}
	c.mu.Lock()
	if c.bound {
		c.mu.Unlock()
		return ErrCoordinatorAlreadyBound
	}
	c.bound = true
	c.mu.Unlock()

	source.OnIMECompositionStart(c.HandleCompositionStart)
	source.OnIMECompositionUpdate(c.HandleCompositionUpdate)
	source.OnIMECompositionEnd(c.HandleCompositionEnd)

	if pointers, ok := source.(PointerEventSource); ok {
		pointers.OnPointer(c.HandlePointer)
	}
	if gestures, ok := source.(GestureEventSource); ok {
		gestures.OnGesture(c.HandleGesture)
	}
	if imeEvents, ok := source.(IMEEventSource); ok {
		imeEvents.OnIMECanceled(c.HandleIMECanceled)
		imeEvents.OnIMEDisabled(c.HandleIMEDisabled)
	}
	return nil
}

// SetCaretArea records the caret rectangle in canvas content coordinates
// for the current or next composition. During an active composition the
// rectangle is immediately re-mapped with the current transform and pushed
// to the IME as a logical-DIP cursor area.
//
// Widgets call this when the text field gains focus or the caret moves,
// including while composing.
func (c *InputSessionCoordinator) SetCaretArea(rect IMECursorArea) {
	area, push := c.setCaretArea(rect)
	if push {
		c.ime.SetIMECursorArea(area)
	}
}

func (c *InputSessionCoordinator) setCaretArea(rect IMECursorArea) (IMECursorArea, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.caret = rect
	c.haveCaret = true
	if !c.composing {
		return IMECursorArea{}, false
	}
	return c.transform.MapRect(c.caret), true
}

// UpdateTransform replaces the canvas transform from an authoritative
// source outside the gesture stream (for example the render state after an
// animated or programmatic zoom). During an active composition the caret is
// re-mapped and pushed immediately so the candidate window stays attached.
func (c *InputSessionCoordinator) UpdateTransform(transform CanvasTransform) {
	area, push := c.updateTransform(transform)
	if push {
		c.ime.SetIMECursorArea(area)
	}
}

func (c *InputSessionCoordinator) updateTransform(transform CanvasTransform) (IMECursorArea, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.transform = transform
	if !c.composing || !c.haveCaret {
		return IMECursorArea{}, false
	}
	return c.transform.MapRect(c.caret), true
}

// CurrentTransform returns a snapshot of the canvas transform.
func (c *InputSessionCoordinator) CurrentTransform() CanvasTransform {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.transform
}

// IsComposing reports whether an IME composition is active.
func (c *InputSessionCoordinator) IsComposing() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.composing
}

// CompositionState returns the last validated composition state and whether
// a session is active. The returned state is a copy; its Cursor and
// Selection are guaranteed to be valid UTF-8 byte ranges in
// CompositionText because invalid update frames are dropped on arrival.
func (c *InputSessionCoordinator) CompositionState() (IMEState, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.composing {
		return IMEState{}, false
	}
	return c.preedit, true
}

// SetIMEEnabled forwards to the underlying IMEController and is safe to
// call from any goroutine, including a draw callback.
func (c *InputSessionCoordinator) SetIMEEnabled(enabled bool) {
	c.ime.SetIMEEnabled(enabled)
}

// SetIMEContentType forwards to the underlying IMEController and is safe to
// call from any goroutine, including a draw callback.
func (c *InputSessionCoordinator) SetIMEContentType(purpose ContentPurpose, hints ContentHint) {
	c.ime.SetIMEContentType(purpose, hints)
}

// CancelIME forwards to the underlying IMEController without touching the
// coordinator's session state; platform cancellation is reported back
// through OnIMECanceled and handled there.
func (c *InputSessionCoordinator) CancelIME() {
	c.ime.CancelIME()
}

// HandleCompositionStart begins a composition session: the current
// transform and caret rect become the session baseline, and the caret is
// pushed in logical DIP so the candidate window appears at once.
//
// It is registered by Bind and may also be driven manually from a custom
// EventSource adapter.
func (c *InputSessionCoordinator) HandleCompositionStart() {
	area, push, hook := c.compositionStart()
	if push {
		c.ime.SetIMECursorArea(area)
	}
	if hook != nil {
		hook()
	}
}

func (c *InputSessionCoordinator) compositionStart() (IMECursorArea, bool, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.composing = true
	c.preedit = IMEState{Composing: true}
	hook := c.onStart
	if !c.haveCaret {
		return IMECursorArea{}, false, hook
	}
	return c.transform.MapRect(c.caret), true, hook
}

// HandleCompositionUpdate applies one composition update frame. A frame
// whose Cursor or Selection is not a valid UTF-8 byte range in
// CompositionText (see IMEState.IsValid) is discarded whole: the stored
// preedit and candidate placement stay at the previous good frame rather
// than being truncated or clamped. Updates received while no session is
// active are ignored.
func (c *InputSessionCoordinator) HandleCompositionUpdate(state IMEState) {
	area, push, hook := c.compositionUpdate(state)
	if !push && hook == nil {
		// Either no session was active or the frame was dropped as invalid:
		// nothing to publish, nothing to report.
		return
	}
	if push {
		c.ime.SetIMECursorArea(area)
	}
	if hook != nil {
		hook(state)
	}
}

func (c *InputSessionCoordinator) compositionUpdate(state IMEState) (IMECursorArea, bool, func(IMEState)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.composing || !state.IsValid() {
		return IMECursorArea{}, false, nil
	}
	state.Composing = true
	c.preedit = state
	hook := c.onUpdate
	if !c.haveCaret {
		return IMECursorArea{}, false, hook
	}
	return c.transform.MapRect(c.caret), true, hook
}

// HandleCompositionEnd finishes the active session and reports the committed
// text through the commit hook. The preedit is cleared without being
// committed separately — the platform's committed string is the only text
// the consumer inserts. An end event without an active session is ignored,
// so a cancellation followed by a stray end cannot double-commit.
func (c *InputSessionCoordinator) HandleCompositionEnd(committed string) {
	hook := c.compositionEnd()
	if hook != nil {
		hook(committed)
	}
}

func (c *InputSessionCoordinator) compositionEnd() func(string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	hook := c.onCommit
	if !c.composing {
		return nil
	}
	c.resetSessionLocked()
	return hook
}

// HandleIMECanceled abandons the active session without committing the
// preedit. It is registered on IMEEventSource.OnIMECanceled by Bind.
func (c *InputSessionCoordinator) HandleIMECanceled() {
	if hook := c.abortSession(true); hook != nil {
		hook()
	}
}

// HandleIMEDisabled clears the active session after the platform disables
// text input (for example on focus loss). Nothing is committed and the
// candidate placement is not touched again until the next session. It is
// registered on IMEEventSource.OnIMEDisabled by Bind.
func (c *InputSessionCoordinator) HandleIMEDisabled() {
	if hook := c.abortSession(false); hook != nil {
		hook()
	}
}

// abortSession ends a session the way cancellation and disable require:
// preedit dropped, no commit, caret baseline cleared so later gestures
// cannot reference the old caret. canceled selects the cancel hook versus
// the disabled hook. It returns nil when no session was active.
func (c *InputSessionCoordinator) abortSession(canceled bool) func() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.composing {
		return nil
	}
	c.resetSessionLocked()
	if canceled {
		return c.onCancel
	}
	return c.onDisabled
}

// resetSessionLocked returns the coordinator to the idle state. The
// transform, gesture baseline and pointer set are gesture-owned and stay
// valid; only the text session and its caret baseline are cleared.
func (c *InputSessionCoordinator) resetSessionLocked() {
	c.composing = false
	c.preedit = IMEState{}
	c.haveCaret = false
	c.caret = IMECursorArea{}
}

// HandlePointer keeps the gesture baseline honest across the pointer
// lifecycle. PointerCancel and PointerLeave — including while composing —
// invalidate the baseline so the next GestureEvent re-anchors on its own
// centroid instead of deriving a zoom jump from a stale one. Down and up
// events invalidate it too because they change the active pointer set.
func (c *InputSessionCoordinator) HandlePointer(ev PointerEvent) {
	c.mu.Lock()
	switch ev.Type {
	case PointerDown, PointerUp, PointerCancel, PointerLeave:
		// Any change to the active pointer set, or an explicit
		// cancel/leave, breaks the anchored gesture baseline.
		c.baseline.broken = true
	}
	c.mu.Unlock()
}

// HandleGesture applies one frame of gesture deltas to the shared transform
// and, while a composition is active, re-maps the caret with the resulting
// transform and pushes it to the IME. A frame whose pointer count differs
// from the anchored baseline, or whose baseline was invalidated by a
// cancel/leave pointer event, only re-anchors: its deltas are discarded so
// no jump is produced from a stale centroid.
//
// Whether a frame delivers the composition update before or after the
// gesture, the last pushed cursor area always reflects this frame's
// transform snapshot because both events update the same state under the
// same mutex.
func (c *InputSessionCoordinator) HandleGesture(ev GestureEvent) {
	area, push := c.gesture(ev)
	if push {
		c.ime.SetIMECursorArea(area)
	}
}

func (c *InputSessionCoordinator) gesture(ev GestureEvent) (IMECursorArea, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case !c.baseline.hasFrame:
		// First frame: its deltas already describe this frame only.
		c.transform = c.transform.ApplyGesture(ev)
		c.baseline = gestureBaseline{hasFrame: true, numPointers: ev.NumPointers}
	case c.baseline.broken || c.baseline.numPointers != ev.NumPointers:
		// Re-anchor and discard this frame's deltas: the pointer set
		// changed or a cancel/leave invalidated the predecessor.
		c.baseline = gestureBaseline{hasFrame: true, numPointers: ev.NumPointers}
	default:
		c.transform = c.transform.ApplyGesture(ev)
	}
	if !c.composing || !c.haveCaret {
		return IMECursorArea{}, false
	}
	return c.transform.MapRect(c.caret), true
}

// SetOnCompositionStart registers a hook invoked after a composition
// session starts. Hooks run outside the coordinator mutex; keep them
// non-blocking.
func (c *InputSessionCoordinator) SetOnCompositionStart(fn func()) {
	c.mu.Lock()
	c.onStart = fn
	c.mu.Unlock()
}

// SetOnCompositionUpdate registers a hook invoked with each validated
// composition frame. Frames dropped for invalid UTF-8 ranges never reach
// the hook.
func (c *InputSessionCoordinator) SetOnCompositionUpdate(fn func(IMEState)) {
	c.mu.Lock()
	c.onUpdate = fn
	c.mu.Unlock()
}

// SetOnCommit registers a hook invoked with the committed text when a
// composition ends normally.
func (c *InputSessionCoordinator) SetOnCommit(fn func(string)) {
	c.mu.Lock()
	c.onCommit = fn
	c.mu.Unlock()
}

// SetOnCancel registers a hook invoked after a composition is abandoned
// through OnIMECanceled. No text is committed.
func (c *InputSessionCoordinator) SetOnCancel(fn func()) {
	c.mu.Lock()
	c.onCancel = fn
	c.mu.Unlock()
}

// SetOnDisabled registers a hook invoked after a composition is abandoned
// through OnIMEDisabled. No text is committed.
func (c *InputSessionCoordinator) SetOnDisabled(fn func()) {
	c.mu.Lock()
	c.onDisabled = fn
	c.mu.Unlock()
}
