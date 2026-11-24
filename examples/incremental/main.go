// Command incremental is the reproducible acceptance sample for pinhole's
// incremental framing support. It builds one scene containing a cube, a ring
// (circle) and a text label, then renders the SAME *pinhole.Pinhole repeatedly
// while rotating it frame after frame. Each incremental frame is compared
// against a scene rebuilt from scratch with the identical operation history.
//
// It also verifies that transforms issued inside a Begin/End block never move
// lines that were added outside the block, and that a failed SavePNG leaves
// the scene fully usable.
//
// Run with:
//
//	go run ./examples/incremental
package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"

	"github.com/tidwall/pinhole"
)

const (
	width  = 400
	height = 400
	frames = 12
)

// buildScene replays the whole construction sequence. steps is the number of
// per-frame rotations applied at the very end; passing 0 yields the initial
// scene. Both the persistent scene and every reference scene are produced by
// this one function, so matching pixels prove there is no state drift.
func buildScene(steps int) *pinhole.Pinhole {
	p := pinhole.New()

	// cube, rotated globally
	p.DrawCube(-0.3, -0.3, -0.3, 0.3, 0.3, 0.3)
	p.Rotate(math.Pi/3, math.Pi/6, 0)

	// ring, transformed inside a Begin/End block
	p.Begin()
	p.DrawCircle(0, 0, 0, 0.2)
	p.Rotate(0, math.Pi/2, 0)
	p.Translate(-0.6, -0.4, 0)
	p.Colorize(color.RGBA{R: 255, A: 255})
	p.End()

	// text label and a scene-level scale
	p.DrawString(0.6, 0.6, 0, "pinhole")
	p.Scale(1.2, 1.2, 1.2)

	step := math.Pi / 9
	for k := 0; k < steps; k++ {
		p.Rotate(0, step, 0)
	}
	return p
}

func encode(img image.Image) []byte {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// linePixelsMaskEqual verifies that every non-background pixel drawn in the
// line-only image appears at the same position with the same color in the
// combined image. Extra pixels (the block-scoped dot) are allowed.
func linePixelsMaskEqual(lineOnly, combined *image.RGBA) bool {
	if !lineOnly.Rect.Eq(combined.Rect) {
		return false
	}
	bg := color.RGBA{R: 255, G: 255, B: 255, A: 255}
	for i := 0; i < len(lineOnly.Pix); i += 4 {
		r, g, b, a := lineOnly.Pix[i], lineOnly.Pix[i+1], lineOnly.Pix[i+2], lineOnly.Pix[i+3]
		if r == bg.R && g == bg.G && b == bg.B && a == bg.A {
			continue
		}
		if combined.Pix[i] != r || combined.Pix[i+1] != g ||
			combined.Pix[i+2] != b || combined.Pix[i+3] != a {
			return false
		}
	}
	return true
}

func main() {
	failed := false
	check := func(name string, ok bool) {
		status := "PASS"
		if !ok {
			status = "FAIL"
			failed = true
		}
		fmt.Printf("[%s] %s\n", status, name)
	}

	// ---- 1. incremental frames vs rebuilt scenes -----------------------
	persistent := buildScene(0)
	firstFrame := persistent.Image(width, height, nil)
	// requirement 2: repeated Image calls return identical pixels
	check("repeated Image() on the same Pinhole is identical",
		bytes.Equal(encode(firstFrame), encode(persistent.Image(width, height, nil))))
	// first frame equals a freshly rebuilt scene's first frame
	check("first frame equals rebuilt-scene first frame",
		bytes.Equal(encode(firstFrame), encode(buildScene(0).Image(width, height, nil))))

	step := math.Pi / 9
	allFramesMatch := true
	for f := 1; f <= frames; f++ {
		persistent.Rotate(0, step, 0)
		got := persistent.Image(width, height, nil)
		want := buildScene(f).Image(width, height, nil)
		if !bytes.Equal(encode(got), encode(want)) {
			fmt.Printf("  frame %d differs from rebuilt scene\n", f)
			allFramesMatch = false
		}
	}
	check(fmt.Sprintf("all %d incremental frames match rebuilt scenes", frames), allFramesMatch)

	// after all that rendering, the persistent scene must still equal a
	// freshly rebuilt equivalent at the same step count
	check("persistent scene renders identically to rebuilt scene after the run",
		bytes.Equal(encode(persistent.Image(width, height, nil)),
			encode(buildScene(frames).Image(width, height, nil))))

	// ---- 2. block scoping: outer lines are untouched --------------------
	// A line added before Begin() must render identically whether or not a
	// block-scoped shape is heavily transformed afterwards: the transform can
	// only affect the shape added inside the block.
	lineOnly := pinhole.New()
	lineOnly.DrawLine(-0.5, -0.5, 0, 0.5, 0.5, 0)

	withBlock := pinhole.New()
	withBlock.DrawLine(-0.5, -0.5, 0, 0.5, 0.5, 0)
	withBlock.Begin()
	withBlock.DrawDot(0, 0, 0, 0.08)
	withBlock.Image(width, height, nil) // render while the block is still open
	withBlock.Translate(5, 5, 5)        // must move only the dot, never the line
	withBlock.End()

	// mask out every non-background pixel of the combined scene and compare
	// them against the line-only render: every line pixel must be unchanged.
	check("outer-line pixels unchanged by in-block transform",
		linePixelsMaskEqual(lineOnly.Image(width, height, nil),
			withBlock.Image(width, height, nil)))

	// ---- 3. failure recovery: unwritable SavePNG ------------------------
	recoverScene := buildScene(2)
	goodBefore := recoverScene.Image(width, height, nil)
	badPath := filepath.Join(os.TempDir(), "pinhole-missing-dir-11357", "nope.png")
	saveErr := recoverScene.SavePNG(badPath, width, height, nil)
	check("SavePNG to an unwritable path returns an error", saveErr != nil)
	check("scene still renders identically after a failed SavePNG",
		bytes.Equal(encode(goodBefore), encode(recoverScene.Image(width, height, nil))))

	// write one real frame next to the binary as a visual artifact
	out := filepath.Join(os.TempDir(), "pinhole-incremental-frame.png")
	if err := persistent.SavePNG(out, width, height, nil); err != nil {
		fmt.Printf("[FAIL] could not write sample frame: %v\n", err)
		failed = true
	} else {
		fmt.Printf("[INFO] sample frame written to %s\n", out)
	}

	if failed {
		os.Exit(1)
	}
	fmt.Println("all incremental-framing checks passed")
}
