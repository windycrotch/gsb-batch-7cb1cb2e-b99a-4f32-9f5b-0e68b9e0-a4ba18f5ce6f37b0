package pinhole

import (
	"bytes"
	"image"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// snap is a value-level snapshot of everything that defines a rendered scene:
// per-line coordinates, color, text scale and the circle link relationships.
type lineSnap struct {
	x1, y1, z1 float64
	x2, y2, z2 float64
	nocaps     bool
	color      uint64
	str        string
	scale      float64
	circle     bool
	firstIdx   int // index of cfirst in p.lines, -1 if nil
	prevIdx    int
	nextIdx    int
}

func colorKey(c interface{}) uint64 {
	if c == nil {
		return 0
	}
	r, g, b, a := c.(interface {
		RGBA() (r, g, b, a uint32)
	}).RGBA()
	return uint64(r)<<48 | uint64(g)<<32 | uint64(b)<<16 | uint64(a)
}

func snapshot(p *Pinhole) []lineSnap {
	idx := map[*line]int{}
	for i, l := range p.lines {
		idx[l] = i
	}
	snaps := make([]lineSnap, len(p.lines))
	for i, l := range p.lines {
		s := lineSnap{
			x1: l.x1, y1: l.y1, z1: l.z1,
			x2: l.x2, y2: l.y2, z2: l.z2,
			nocaps:   l.nocaps,
			color:    colorKey(l.color),
			str:      l.str,
			scale:    l.scale,
			circle:   l.circle,
			firstIdx: -1, prevIdx: -1, nextIdx: -1,
		}
		if l.cfirst != nil {
			s.firstIdx = idx[l.cfirst]
		}
		if l.cprev != nil {
			s.prevIdx = idx[l.cprev]
		}
		if l.cnext != nil {
			s.nextIdx = idx[l.cnext]
		}
		snaps[i] = s
	}
	return snaps
}

func pixelsEqual(a, b *image.RGBA) bool {
	if a == nil || b == nil || !a.Rect.Eq(b.Rect) {
		return false
	}
	return bytes.Equal(a.Pix, b.Pix)
}

// buildAcceptanceScene constructs the canonical acceptance scene: a cube, a
// circle and a text label. The circle is rotated/translated inside a Begin/End
// block, exactly like the README example.
func buildAcceptanceScene(rot float64) *Pinhole {
	p := New()
	p.DrawCube(-0.3, -0.3, -0.3, 0.3, 0.3, 0.3)
	p.Rotate(math.Pi/3, math.Pi/6, 0)
	p.Begin()
	p.DrawCircle(0, 0, 0, 0.2)
	p.Rotate(0, rot, 0)
	p.Translate(-0.6, -0.4, 0)
	p.End()
	p.DrawString(0.6, 0.6, 0, "pinhole")
	p.Scale(1.2, 1.2, 1.2)
	return p
}

// TestRepeatedImageIsStable covers requirement 2: calling Image repeatedly on
// the same Pinhole must return identical pixels and must not mutate any line
// coordinate, color, text scale or circle link relationship.
func TestRepeatedImageIsStable(t *testing.T) {
	p := buildAcceptanceScene(math.Pi / 2)
	before := snapshot(p)
	first := p.Image(300, 300, nil)
	for n := 0; n < 4; n++ {
		img := p.Image(300, 300, nil)
		if !pixelsEqual(first, img) {
			t.Fatalf("frame %d differs from the first frame", n+1)
		}
	}
	after := snapshot(p)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("Image() mutated scene state\nbefore: %+v\nafter:  %+v", before, after)
	}
}

// buildFrameScene replays one deterministic construction sequence, including
// f global rotation steps applied after the scene (with all blocks) is built.
// The incremental test performs the same calls in the same order on one
// persistent Pinhole; identical operation order keeps every floating-point
// result bit-identical between the two paths.
func buildFrameScene(steps int) *Pinhole {
	p := New()
	p.DrawCube(-0.3, -0.3, -0.3, 0.3, 0.3, 0.3)
	p.Rotate(math.Pi/3, math.Pi/6, 0)
	p.Begin()
	p.DrawCircle(0, 0, 0, 0.2)
	p.Rotate(0, math.Pi/2, 0)
	p.Translate(-0.6, -0.4, 0)
	p.End()
	p.DrawString(0.6, 0.6, 0, "pinhole")
	p.Scale(1.2, 1.2, 1.2)
	step := math.Pi / 7
	for k := 0; k < steps; k++ {
		p.Rotate(0, step, 0)
	}
	return p
}

// TestIncrementalFramesMatchRebuiltScene is the headline acceptance test: one
// persistent Pinhole is rotated and rendered frame after frame (incremental
// frames). Every frame must be pixel-identical to a scene rebuilt from scratch
// by replaying the same operations.
func TestIncrementalFramesMatchRebuiltScene(t *testing.T) {
	persistent := buildFrameScene(0)
	const frames = 8
	step := math.Pi / 7
	for f := 1; f <= frames; f++ {
		persistent.Rotate(0, step, 0)
		got := persistent.Image(300, 300, nil)
		want := buildFrameScene(f).Image(300, 300, nil)
		if !pixelsEqual(got, want) {
			t.Fatalf("incremental frame %d does not match rebuilt scene", f)
		}
	}
}

// TestIncrementalFramesInsideOpenBlock verifies the trickier incremental path:
// rendering repeatedly while a Begin block is still open, then rotating inside
// that block between frames. The reference scenes are rebuilt with the block
// still open and the same number of rotations, so the histories match exactly.
func TestIncrementalFramesInsideOpenBlock(t *testing.T) {
	openScene := func(steps int) *Pinhole {
		p := New()
		p.DrawCube(-0.3, -0.3, -0.3, 0.3, 0.3, 0.3)
		p.Begin()
		p.DrawCircle(0, 0, 0, 0.2)
		step := math.Pi / 9
		for k := 0; k < steps; k++ {
			p.Rotate(0, step, 0)
		}
		return p
	}
	persistent := openScene(0)
	// render with the block open before any rotation: must equal the rebuilt
	// zero-step scene, proving rendering doesn't disturb the open block
	if got, want := persistent.Image(240, 240, nil), openScene(0).Image(240, 240, nil); !pixelsEqual(got, want) {
		t.Fatal("open-block baseline render does not match rebuilt scene")
	}
	const frames = 6
	step := math.Pi / 9
	for f := 1; f <= frames; f++ {
		persistent.Rotate(0, step, 0)
		got := persistent.Image(240, 240, nil)
		want := openScene(f).Image(240, 240, nil)
		if !pixelsEqual(got, want) {
			t.Fatalf("open-block incremental frame %d does not match rebuilt scene", f)
		}
	}
	// close the block; the cube outside it must be exactly where it started
	persistent.End()
	ref := openScene(frames)
	ref.End()
	if !reflect.DeepEqual(snapshot(persistent), snapshot(ref)) {
		t.Fatal("scene state after closing block differs from rebuilt equivalent")
	}
}

// TestBlockTransformsNeverTouchOuterLines covers requirement 1 and part of
// requirement 4: once a Begin/End block closes (or while one is open across a
// render), transforms scoped to the block must never move lines outside it.
func TestBlockTransformsNeverTouchOuterLines(t *testing.T) {
	p := New()
	p.DrawLine(1, 2, 3, 4, 5, 6) // outer line, index 0
	outerBefore := snapshot(p)[0]

	p.Begin()
	p.DrawCube(-0.1, -0.1, -0.1, 0.1, 0.1, 0.1)
	// render with the block open, then keep working in the same block
	p.Image(200, 200, nil)
	p.Rotate(0.4, 0.5, 0.6)
	p.Translate(10, 20, 30)
	p.Scale(2, 3, 4)
	p.Image(200, 200, nil)
	p.End()

	outerAfter := snapshot(p)[0]
	if outerAfter != outerBefore {
		t.Fatalf("outer line mutated by in-block transform\nbefore: %+v\nafter:  %+v", outerBefore, outerAfter)
	}

	// after End(), the block is closed and a new Begin block must again only
	// affect the newly added lines.
	p.Begin()
	p.DrawDot(0, 0, 0, 0.05)
	p.Translate(-7, -8, -9)
	p.End()
	outerAfter2 := snapshot(p)[0]
	if outerAfter2 != outerBefore {
		t.Fatalf("outer line mutated by second block transform: %+v", outerAfter2)
	}
}

// TestUnmatchedEndAndNestedBlocks checks the Begin/End pairing rules: an End
// without a Begin is a no-op, and nested blocks keep transforms scoped to the
// currently open block without panicking.
func TestUnmatchedEndAndNestedBlocks(t *testing.T) {
	p := New()
	p.End() // unmatched End must be a no-op, not a panic
	p.End()

	p.DrawLine(0, 0, 0, 1, 0, 0) // index 0: outside every block
	base := snapshot(p)[0]
	p.Begin()
	p.DrawLine(0, 0, 0, 0, 1, 0) // index 1: outer block only
	p.Begin()                    // nested block without closing the outer one
	p.DrawLine(0, 0, 0, 0, 0, 1) // index 2: inner block
	p.Translate(5, 0, 0)         // must move only index 2
	p.Image(100, 100, nil)       // render while nested blocks are open
	p.End()
	// outer block now spans indices 1..2
	p.Translate(0, 7, 0) // must move indices 1 and 2, never index 0
	p.End()
	p.Translate(0, 0, 9) // no open block: applies globally, index 0 moves too

	snaps := snapshot(p)
	if snaps[0].x1 != base.x1 || snaps[0].y1 != base.y1 || snaps[0].z1 != 9 {
		t.Fatalf("outermost line scoping wrong: %+v", snaps[0])
	}
	// index 1 belongs to the outer block (which includes the nested range), so
	// it gets the outer translate; the final translate runs with no open block
	// and applies to every line, hence the z=9 component
	if snaps[1].x1 != 0 || snaps[1].y1 != 7 || snaps[1].z1 != 9 {
		t.Fatalf("outer-block line scoping wrong: %+v", snaps[1])
	}
	// index 2 is inside the nested block: both block translates plus the final
	// global one
	if snaps[2].x1 != 5 || snaps[2].y1 != 7 || snaps[2].z1 != 9 {
		t.Fatalf("inner-block line scoping wrong: %+v", snaps[2])
	}
}

// TestCircleClosureSurvivesTransforms covers requirement 3: after a circle is
// transformed inside a block, adjacent segments still share endpoints, the
// chain links stay intact, and the final segment closes back onto the first.
func TestCircleClosureSurvivesTransforms(t *testing.T) {
	p := New()
	p.Begin()
	p.DrawCircle(0.1, -0.2, 0.3, 0.25)
	p.Rotate(0.7, 1.1, 0.4)
	p.Translate(0.5, -0.5, 0.2)
	p.Scale(1.3, 1.3, 1.3)
	p.Image(250, 250, nil) // render inside the block, then transform again
	p.Rotate(0.2, 0.3, 0.1)
	p.End()

	var first, last *line
	count := 0
	for _, l := range p.lines {
		if !l.circle {
			t.Fatal("non-circle line found in circle-only scene")
		}
		if first == nil {
			first = l
		}
		last = l
		count++
	}
	if count != circleSteps {
		t.Fatalf("expected %d circle segments, got %d", circleSteps, count)
	}
	// link chain integrity
	if first.cfirst != first || first.cprev != nil {
		t.Fatal("first segment chain anchors wrong")
	}
	if last.cnext != nil || last.cfirst != first {
		t.Fatal("last segment chain anchors wrong")
	}
	// geometric closure: every segment starts where the previous one ended,
	// and the ring closes end-to-start.
	seg := first
	for seg != nil {
		if seg.cnext != nil {
			n := seg.cnext
			if seg.x2 != n.x1 || seg.y2 != n.y1 || seg.z2 != n.z1 {
				t.Fatalf("gap between circle segments: (%.12f,%.12f,%.12f) != (%.12f,%.12f,%.12f)",
					seg.x2, seg.y2, seg.z2, n.x1, n.y1, n.z1)
			}
		}
		seg = seg.cnext
	}
	if last.x2 != first.x1 || last.y2 != first.y1 || last.z2 != first.z1 {
		t.Fatalf("circle does not close: last end (%.12f,%.12f,%.12f) != first start (%.12f,%.12f,%.12f)",
			last.x2, last.y2, last.z2, first.x1, first.y1, first.z1)
	}
}

// TestCenterEmptySceneNoPanic covers the failure/recovery requirement:
// centering an empty scene (or an empty open block) must be a harmless no-op.
func TestCenterEmptySceneNoPanic(t *testing.T) {
	p := New()
	p.Center()
	p.Begin()
	p.Center() // empty open block
	p.End()
	p.Image(64, 64, nil) // must still render (blank image)
	p.DrawDot(0, 0, 0, 0.1)
	p.Center()
	before := snapshot(p)
	p.Image(64, 64, nil)
	if !reflect.DeepEqual(before, snapshot(p)) {
		t.Fatal("rendering after empty-scene Center changed state")
	}
}

// TestBeginAfterForgottenEndRecovers covers the recovery requirement: a caller
// who renders inside a block, forgets End, then calls Begin again must not
// panic, and later frames must not inherit any corrupt state.
func TestBeginAfterForgottenEndRecovers(t *testing.T) {
	p := New()
	p.DrawLine(9, 9, 9, 9, 9, 9) // outer line
	outer := snapshot(p)[0]
	p.Begin()
	p.DrawCube(-0.2, -0.2, -0.2, 0.2, 0.2, 0.2)
	p.Image(120, 120, nil)
	p.Begin() // forgotten End: must not panic or corrupt scoping
	p.Translate(1, 2, 3)
	p.Image(120, 120, nil)
	p.End()
	p.End()
	if snapshot(p)[0] != outer {
		t.Fatal("outer line drifted after forgotten End recovery")
	}
	img1 := p.Image(120, 120, nil)
	img2 := p.Image(120, 120, nil)
	if !pixelsEqual(img1, img2) {
		t.Fatal("frames after recovery are not deterministic")
	}
}

// TestDegenerateCircleDoesNotPanic guards the render path that previously
// panicked ("nil!") when a circle segment projected to a single point. The
// render must complete and the scene must stay usable afterwards.
func TestDegenerateCircleDoesNotPanic(t *testing.T) {
	p := New()
	// a circle rotated edge-on and pushed far away so some segments collapse
	// to a single projected point or leave the screen
	p.Begin()
	p.DrawCircle(0, 0, 0, 0.9)
	p.Rotate(0, math.Pi/2, 0)
	p.Translate(0, 0, 50)
	p.End()
	img1 := p.Image(128, 128, nil)
	img2 := p.Image(128, 128, nil)
	if !pixelsEqual(img1, img2) {
		t.Fatal("degenerate-circle renders are not deterministic")
	}
}

// TestSavePNGFailureIsRecoverable: a failed render (unwritable path) must
// leave the Pinhole fully usable, with subsequent images unchanged.
func TestSavePNGFailureIsRecoverable(t *testing.T) {
	p := buildAcceptanceScene(math.Pi / 4)
	want := p.Image(300, 300, nil)
	before := snapshot(p)

	badDir := filepath.Join(os.TempDir(), "pinhole-no-such-dir-11357")
	badPath := filepath.Join(badDir, "cannot-create.png")
	if err := p.SavePNG(badPath, 300, 300, nil); err == nil {
		t.Fatal("expected SavePNG to fail on an unwritable path")
	}
	if !reflect.DeepEqual(before, snapshot(p)) {
		t.Fatal("failed SavePNG mutated scene state")
	}
	if got := p.Image(300, 300, nil); !pixelsEqual(got, want) {
		t.Fatal("image after failed SavePNG differs from image before it")
	}

	// and a subsequent successful save still works
	goodPath := filepath.Join(t.TempDir(), "recovered.png")
	if err := p.SavePNG(goodPath, 300, 300, nil); err != nil {
		t.Fatalf("SavePNG after recovery failed: %v", err)
	}
}

// TestLoadObjBlockIsolation covers requirement 4: a LoadObj model scaled and
// centered at scene level, then partially transformed inside a Begin/End
// block, must leave the lines outside the block untouched.
func TestLoadObjBlockIsolation(t *testing.T) {
	obj := `
v -1 -1 -1
v  1 -1 -1
v  1  1 -1
v -1  1 -1
f 1 2 3 4
`
	p := New()
	if err := p.LoadObj(bytes.NewBufferString(obj)); err != nil {
		t.Fatalf("LoadObj: %v", err)
	}
	nObj := len(p.lines)
	if nObj == 0 {
		t.Fatal("LoadObj produced no lines")
	}
	p.Scale(0.5, 0.5, 0.5)
	p.Center()
	modelBefore := snapshot(p)

	p.Begin()
	p.DrawDot(0.5, 0.5, 0.5, 0.05)
	p.Rotate(0.9, 0.2, 0.5)
	p.Translate(3, -3, 1)
	p.Image(200, 200, nil)
	p.End()

	after := snapshot(p)
	for i := 0; i < nObj; i++ {
		if after[i] != modelBefore[i] {
			t.Fatalf("obj line %d mutated by in-block transform\nbefore: %+v\nafter:  %+v", i, modelBefore[i], after[i])
		}
	}
}
