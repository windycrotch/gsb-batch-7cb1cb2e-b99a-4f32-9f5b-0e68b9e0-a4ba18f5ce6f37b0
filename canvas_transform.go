// Copyright 2026 The gogpu Authors
// SPDX-License-Identifier: MIT

package gpucontext

// CanvasTransform is the zoom-and-pan transform of a borderless-window
// canvas, the single state axis shared by gesture handling and IME
// composition.
//
// The transform maps canvas content coordinates to window logical DIP
// (device-independent pixels), the coordinate space of PointerEvent,
// GestureEvent.Center and IMECursorArea:
//
//	window = content*Scale + Offset
//
// Content coordinates are stable while the user pans and zooms: a text
// caret rect expressed in content coordinates stays attached to the same
// glyph, and MapRect re-expresses it in window DIP after every gesture
// frame so the IME candidate window follows the canvas instead of staying
// where the caret used to be.
//
// CanvasTransform is a value type. Its methods return a new value instead
// of mutating the receiver, so a transform can be snapshotted per frame
// and shared between goroutines through InputSessionCoordinator without
// aliasing.
type CanvasTransform struct {
	// ScaleX is the horizontal zoom factor; 1 is no zoom.
	ScaleX float64

	// ScaleY is the vertical zoom factor; 1 is no zoom.
	ScaleY float64

	// OffsetX is the horizontal translation in window logical DIP.
	OffsetX float64

	// OffsetY is the vertical translation in window logical DIP.
	OffsetY float64
}

// IdentityCanvasTransform returns the transform with no zoom and no
// translation, where content and window coordinates coincide.
func IdentityCanvasTransform() CanvasTransform {
	return CanvasTransform{ScaleX: 1, ScaleY: 1}
}

// ApplyGesture returns the transform after one frame of gesture deltas.
//
// ZoomDelta is applied as a uniform scale anchored at ev.Center, so the
// content point under the gesture centroid stays under the finger while
// pinching. TranslationDelta is then added in window logical DIP.
//
// A non-positive ZoomDelta is treated as "no zoom" because a real gesture
// recognizer never emits one; the frame is still panned. RotationDelta and
// ZoomDelta2D are deliberately not applied — a canvas that supports
// rotation or non-uniform stretch owns its own transform and publishes it
// through InputSessionCoordinator.UpdateTransform.
func (t CanvasTransform) ApplyGesture(ev GestureEvent) CanvasTransform {
	if z := ev.ZoomDelta; z > 0 && z != 1 {
		// Keep the content point under the centroid fixed:
		//	offset' = center - (center-offset)*z
		t.OffsetX = ev.Center.X - (ev.Center.X-t.OffsetX)*z
		t.OffsetY = ev.Center.Y - (ev.Center.Y-t.OffsetY)*z
		t.ScaleX *= z
		t.ScaleY *= z
	}
	t.OffsetX += ev.TranslationDelta.X
	t.OffsetY += ev.TranslationDelta.Y
	return t
}

// MapRect expresses a rectangle in canvas content coordinates as a
// rectangle in window logical DIP. It is how a caret rect stored at
// composition start is repositioned for SetIMECursorArea after the canvas
// has been panned or zoomed. A zero-width or zero-height rect stays a
// point or a line.
func (t CanvasTransform) MapRect(rect IMECursorArea) IMECursorArea {
	left := t.OffsetX + rect.X*t.ScaleX
	right := t.OffsetX + (rect.X+rect.Width)*t.ScaleX
	top := t.OffsetY + rect.Y*t.ScaleY
	bottom := t.OffsetY + (rect.Y+rect.Height)*t.ScaleY
	if right < left {
		left, right = right, left
	}
	if bottom < top {
		top, bottom = bottom, top
	}
	return IMECursorArea{
		X:      left,
		Y:      top,
		Width:  right - left,
		Height: bottom - top,
	}
}
