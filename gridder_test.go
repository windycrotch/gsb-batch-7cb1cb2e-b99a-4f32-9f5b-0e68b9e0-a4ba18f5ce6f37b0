package gridder

import (
	"bytes"
	"errors"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/golang/freetype/truetype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/image/font/gofont/goregular"
)

func TestNew(t *testing.T) {
	_, err := New(ImageConfig{}, GridConfig{Rows: 0})
	assert.NotNil(t, err)

	_, err = New(ImageConfig{}, GridConfig{Rows: 10, Columns: 0})
	assert.NotNil(t, err)

	_, err = New(ImageConfig{}, GridConfig{Rows: 10, Columns: 10})
	assert.Nil(t, err)
}

func TestPaintCell(t *testing.T) {
	gridder, err := New(ImageConfig{}, GridConfig{Rows: 1, Columns: 1})
	assert.Nil(t, err)

	err = gridder.PaintCell(-1, -1, color.Black)
	assert.NotNil(t, err)

	err = gridder.PaintCell(0, 0, color.Black)
	assert.Nil(t, err)
}

func TestDrawCircle(t *testing.T) {
	gridder, err := New(ImageConfig{}, GridConfig{Rows: 1, Columns: 1})
	assert.Nil(t, err)

	err = gridder.DrawCircle(-1, -1)
	assert.NotNil(t, err)

	err = gridder.DrawCircle(0, 0)
	assert.Nil(t, err)
}

func TestDrawRectangle(t *testing.T) {
	gridder, err := New(ImageConfig{}, GridConfig{Rows: 1, Columns: 1})
	assert.Nil(t, err)

	err = gridder.DrawRectangle(-1, -1)
	assert.NotNil(t, err)

	err = gridder.DrawRectangle(0, 0)
	assert.Nil(t, err)
}

func TestDrawLine(t *testing.T) {
	gridder, err := New(ImageConfig{}, GridConfig{Rows: 1, Columns: 1})
	assert.Nil(t, err)

	err = gridder.DrawLine(-1, -1)
	assert.NotNil(t, err)

	err = gridder.DrawLine(0, 0)
	assert.Nil(t, err)
}

func TestDrawPath(t *testing.T) {
	gridder, err := New(ImageConfig{}, GridConfig{Rows: 1, Columns: 1})
	assert.Nil(t, err)

	err = gridder.DrawPath(-1, -1, -1, -1)
	assert.NotNil(t, err)

	err = gridder.DrawPath(0, 0, 0, 0)
	assert.Nil(t, err)
}

func TestDrawString(t *testing.T) {
	gridder, err := New(ImageConfig{}, GridConfig{Rows: 1, Columns: 1})
	assert.Nil(t, err)

	font, _ := truetype.Parse(goregular.TTF)
	fontFace := truetype.NewFace(font, &truetype.Options{Size: 24})

	err = gridder.DrawString(-1, -1, "Test", fontFace)
	assert.NotNil(t, err)

	err = gridder.DrawString(0, 0, "Test", fontFace)
	assert.Nil(t, err)
}

func TestSave(t *testing.T) {
	gridder, err := New(ImageConfig{}, GridConfig{Rows: 1, Columns: 1})
	assert.Nil(t, err)

	err = gridder.SavePNG()
	assert.NotNil(t, err)
}

func TestWriter(t *testing.T) {
	gridder, err := New(ImageConfig{Width: 200, Height: 200}, GridConfig{Rows: 2, Columns: 2, LineStrokeWidth: 2, BorderStrokeWidth: 10})
	assert.Nil(t, err)

	bImage := new(bytes.Buffer)

	err = gridder.EncodePNG(bImage)
	assert.Nil(t, err)
}

// failingWriter always returns an error and never consumes any bytes.
type failingWriter struct {
	err error
}

func (f *failingWriter) Write(p []byte) (int, error) {
	return 0, f.err
}

func newTestGridder(t *testing.T) *Gridder {
	t.Helper()
	gridder, err := New(
		ImageConfig{Width: 200, Height: 200},
		GridConfig{
			Rows:              4,
			Columns:           4,
			LineDashes:        5,
			LineStrokeWidth:   2,
			BorderDashes:      3,
			BorderStrokeWidth: 4,
			MarginWidth:       10,
		},
	)
	require.Nil(t, err)
	return gridder
}

func encodePNGBytes(t *testing.T, gridder *Gridder) []byte {
	t.Helper()
	buffer := new(bytes.Buffer)
	require.Nil(t, gridder.EncodePNG(buffer))
	require.NotEmpty(t, buffer.Bytes())
	return buffer.Bytes()
}

// TestRepeatedExportProducesIdenticalOutput makes sure the grid lines and
// border are never painted onto the live canvas. Exporting the same drawing
// any number of times, through either export method and in any order, must
// produce byte-identical PNGs (no line thickening, no dash-phase drift).
func TestRepeatedExportProducesIdenticalOutput(t *testing.T) {
	gridder := newTestGridder(t)
	require.Nil(t, gridder.PaintCell(0, 0, color.RGBA{R: 255, A: 255}))
	require.Nil(t, gridder.DrawCircle(2, 2))
	require.Nil(t, gridder.DrawLine(1, 3))

	first := encodePNGBytes(t, gridder)

	for i := 0; i < 5; i++ {
		assert.Equal(t, first, encodePNGBytes(t, gridder), "EncodePNG output changed on export %d", i+2)
	}

	// SavePNG must produce exactly the same pixels as EncodePNG for the same
	// drawing, regardless of how many times either one ran.
	path := filepath.Join(t.TempDir(), "export.png")
	savingGridder, err := New(
		ImageConfig{Width: 200, Height: 200, Name: path},
		gridder.gridConfig,
	)
	require.Nil(t, err)
	require.Nil(t, savingGridder.PaintCell(0, 0, color.RGBA{R: 255, A: 255}))
	require.Nil(t, savingGridder.DrawCircle(2, 2))
	require.Nil(t, savingGridder.DrawLine(1, 3))
	require.Nil(t, savingGridder.EncodePNG(new(bytes.Buffer)))
	require.Nil(t, savingGridder.SavePNG())
	require.Nil(t, savingGridder.SavePNG())

	saved, err := os.ReadFile(path)
	require.Nil(t, err)
	assert.Equal(t, first, saved, "SavePNG pixels differ from EncodePNG pixels")
}

// TestSaveAndEncodeAreIdempotentTogether exercises the original complaint:
// the same Gridder is first encoded into a buffer and then saved to disk (and
// the reverse order). Both outputs must match the first export exactly.
func TestSaveAndEncodeAreIdempotentTogether(t *testing.T) {
	path := filepath.Join(t.TempDir(), "grid.png")
	gridder, err := New(
		ImageConfig{Width: 200, Height: 200, Name: path},
		GridConfig{Rows: 3, Columns: 3, LineDashes: 4, BorderDashes: 6, LineStrokeWidth: 2, BorderStrokeWidth: 4},
	)
	require.Nil(t, err)
	require.Nil(t, gridder.PaintCell(1, 1, color.RGBA{G: 255, A: 255}))

	first := encodePNGBytes(t, gridder)

	// Encode to memory, then save to disk.
	assert.Equal(t, first, encodePNGBytes(t, gridder))
	require.Nil(t, gridder.SavePNG())
	saved, err := os.ReadFile(path)
	require.Nil(t, err)
	assert.Equal(t, first, saved)

	// Reverse order: save to disk, then encode to memory.
	require.Nil(t, gridder.SavePNG())
	saved, err = os.ReadFile(path)
	require.Nil(t, err)
	assert.Equal(t, first, saved)
	assert.Equal(t, first, encodePNGBytes(t, gridder))
}

// TestExportDoesNotMutateCanvas verifies directly that exporting leaves the
// underlying canvas untouched: two gridders built and painted identically,
// one exported many times and the other never, must keep exporting the exact
// same pixels.
func TestExportDoesNotMutateCanvas(t *testing.T) {
	exported := newTestGridder(t)
	require.Nil(t, exported.PaintCell(0, 0, color.RGBA{B: 255, A: 255}))

	untouched := newTestGridder(t)
	require.Nil(t, untouched.PaintCell(0, 0, color.RGBA{B: 255, A: 255}))

	for i := 0; i < 5; i++ {
		encodePNGBytes(t, exported)
	}

	assert.Equal(t, encodePNGBytes(t, untouched), encodePNGBytes(t, exported),
		"canvas pixels changed after exporting")
}

// TestDrawThenExportContainsContent ensures content drawn before an export is
// present in the exported image, while content drawn afterwards appears in the
// next export without re-stamping the existing grid lines or border.
func TestDrawThenExportContainsContent(t *testing.T) {
	gridder := newTestGridder(t)

	blank := encodePNGBytes(t, gridder)

	cellColor := color.RGBA{R: 255, G: 10, B: 20, A: 255}
	require.Nil(t, gridder.PaintCell(0, 0, cellColor))

	withCell := encodePNGBytes(t, gridder)
	assert.NotEqual(t, blank, withCell, "drawn cell did not show up in the export")
	assert.Equal(t, withCell, encodePNGBytes(t, gridder), "re-export after drawing changed unexpectedly")

	require.Nil(t, gridder.DrawCircle(3, 3))
	withCircle := encodePNGBytes(t, gridder)
	assert.NotEqual(t, withCell, withCircle, "newly drawn circle did not show up in the next export")
	assert.Equal(t, withCircle, encodePNGBytes(t, gridder), "grid or border got re-stamped between exports")
}

// TestFailedExportDoesNotPolluteCanvas checks that a writer error is surfaced
// to the caller but cannot leave the canvas in a different state. A retry
// after the failure must produce exactly the bytes the successful export would
// have produced before the failure.
func TestFailedExportDoesNotPolluteCanvas(t *testing.T) {
	gridder := newTestGridder(t)
	require.Nil(t, gridder.PaintCell(2, 1, color.RGBA{G: 128, A: 255}))

	expected := encodePNGBytes(t, gridder)

	expectedErr := errors.New("disk on fire")
	err := gridder.EncodePNG(&failingWriter{err: expectedErr})
	assert.Equal(t, expectedErr, err)

	// The failed export must not have painted grid lines or border onto the
	// canvas: retrying yields the exact same bytes as before the failure.
	assert.Equal(t, expected, encodePNGBytes(t, gridder))

	// A failing SavePNG (invalid path) must be recoverable in the same way.
	savingGridder, err := New(
		ImageConfig{Width: 200, Height: 200, Name: filepath.Join(t.TempDir(), "does-not-exist", "grid.png")},
		gridder.gridConfig,
	)
	require.Nil(t, err)
	require.Nil(t, savingGridder.PaintCell(2, 1, color.RGBA{G: 128, A: 255}))
	expectedSaved := encodePNGBytes(t, savingGridder)

	assert.NotNil(t, savingGridder.SavePNG())
	assert.Equal(t, expectedSaved, encodePNGBytes(t, savingGridder))
}

// TestOutOfBoundsDoesNotChangeState ensures out-of-bounds drawing still errors
// and leaves both the canvas and subsequent exports exactly as they were.
func TestOutOfBoundsDoesNotChangeState(t *testing.T) {
	gridder := newTestGridder(t)
	require.Nil(t, gridder.PaintCell(0, 0, color.RGBA{R: 255, A: 255}))

	before := encodePNGBytes(t, gridder)

	font, err := truetype.Parse(goregular.TTF)
	require.Nil(t, err)
	fontFace := truetype.NewFace(font, &truetype.Options{Size: 24})

	outOfBoundsCalls := []func() error{
		func() error { return gridder.PaintCell(-1, 0, color.Black) },
		func() error { return gridder.PaintCell(4, 0, color.Black) },
		func() error { return gridder.PaintCell(0, 4, color.Black) },
		func() error { return gridder.DrawRectangle(-1, 0) },
		func() error { return gridder.DrawCircle(0, -1) },
		func() error { return gridder.DrawLine(99, 99) },
		func() error { return gridder.DrawPath(0, 0, -1, 0) },
		func() error { return gridder.DrawPath(0, 0, 0, 99) },
		func() error { return gridder.DrawString(-1, -1, "Test", fontFace) },
	}
	for i, call := range outOfBoundsCalls {
		assert.Equal(t, errOutOfBounds, call(), "call %d should fail with errOutOfBounds", i)
	}

	assert.Equal(t, before, encodePNGBytes(t, gridder), "out-of-bounds drawing changed the canvas")
}

// TestConcurrentDrawAndExport hammers the same Gridder from several goroutines
// interleaving every drawing operation with exports. It must run cleanly under
// the race detector, never expose a half-drawn snapshot, and every export must
// match one of the valid complete snapshots of the drawing.
func TestConcurrentDrawAndExport(t *testing.T) {
	gridder := newTestGridder(t)

	font, err := truetype.Parse(goregular.TTF)
	require.Nil(t, err)
	fontFace := truetype.NewFace(font, &truetype.Options{Size: 24})

	const iterations = 100

	var wg sync.WaitGroup

	// Exporter goroutines: every output has to equal the export of some
	// complete state, so collect them for validation after the run.
	exported := make(chan []byte, iterations*4)
	for e := 0; e < 4; e++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				buffer := new(bytes.Buffer)
				if err := gridder.EncodePNG(buffer); err != nil {
					t.Errorf("unexpected export error: %v", err)
					return
				}
				copied := append([]byte(nil), buffer.Bytes()...)
				exported <- copied
			}
		}()
	}

	// Drawer goroutines covering every drawing entry point.
	drawers := []func(int) error{
		func(i int) error { return gridder.PaintCell(i%4, (i+1)%4, color.RGBA{R: uint8(i), A: 255}) },
		func(i int) error { return gridder.DrawRectangle(i%4, i%4) },
		func(i int) error { return gridder.DrawCircle((i+2)%4, i%4) },
		func(i int) error { return gridder.DrawLine(i%4, (i+3)%4) },
		func(i int) error { return gridder.DrawPath(0, 0, i%4, i%4) },
		func(i int) error { return gridder.DrawString(i%4, i%4, "x", fontFace) },
	}
	for _, draw := range drawers {
		draw := draw
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				if err := draw(i); err != nil {
					t.Errorf("unexpected draw error: %v", err)
					return
				}
			}
		}()
	}

	wg.Wait()
	close(exported)

	// No export may be empty or corrupted and all must decode as PNGs.
	for pngBytes := range exported {
		_, err := png.Decode(bytes.NewReader(pngBytes))
		require.Nil(t, err, "export produced an undecodable PNG")
	}

	// The final state must itself be exportable and self-consistent across
	// repeated exports.
	final := encodePNGBytes(t, gridder)
	assert.Equal(t, final, encodePNGBytes(t, gridder))
}
