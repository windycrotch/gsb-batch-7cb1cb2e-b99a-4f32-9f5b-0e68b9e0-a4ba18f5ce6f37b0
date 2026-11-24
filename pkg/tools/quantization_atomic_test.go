package tools

import (
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/willibrandon/mtlog"
	"github.com/willibrandon/mtlog/core"
	"github.com/willibrandon/pixel-mcp/pkg/aseprite"
)

// discardLogger returns a logger that emits nothing below error level.
func discardLogger() core.Logger {
	return mtlog.New(mtlog.WithMinimumLevel(core.ErrorLevel))
}

func TestNormalizeHexColor(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"#ff0000", "#FF0000"},
		{"#FF0000", "#FF0000"},
		{"#FF0000FF", "#FF0000"},
		{"#00000000", "#00000000"},
		{"#AABBCC80", "#AABBCC80"},
	}
	for _, tt := range tests {
		if got := normalizeHexColor(tt.in); got != tt.want {
			t.Errorf("normalizeHexColor(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestPalettesEqual(t *testing.T) {
	tests := []struct {
		name string
		a    []string
		b    []string
		want bool
	}{
		{"identical", []string{"#FF0000", "#00FF00"}, []string{"#FF0000", "#00FF00"}, true},
		{"case insensitive", []string{"#ff0000"}, []string{"#FF0000"}, true},
		{"opaque alpha normalized", []string{"#FF0000FF"}, []string{"#FF0000"}, true},
		{"transparent preserved", []string{"#00000000"}, []string{"#00000000"}, true},
		{"different length", []string{"#FF0000"}, []string{"#FF0000", "#00FF00"}, false},
		{"different color", []string{"#FF0000"}, []string{"#00FF00"}, false},
		{"different order", []string{"#FF0000", "#00FF00"}, []string{"#00FF00", "#FF0000"}, false},
		{"semi-transparent differs", []string{"#FF000080"}, []string{"#FF0000"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := palettesEqual(tt.a, tt.b); got != tt.want {
				t.Errorf("palettesEqual(%v, %v) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestValidateQuantizeResult(t *testing.T) {
	requested := []string{"#FF0000", "#00FF00", "#0000FF"}

	tests := []struct {
		name         string
		result       QuantizePaletteOutput
		indexed      bool
		wantErr      bool
		errSubstring string
	}{
		{
			name: "success indexed",
			result: QuantizePaletteOutput{
				Success: true, QuantizedColors: 3, ColorMode: "indexed",
				Palette: []string{"#FF0000", "#00FF00", "#0000FF"},
			},
			indexed: true,
		},
		{
			name: "success rgb without conversion",
			result: QuantizePaletteOutput{
				Success: true, QuantizedColors: 3, ColorMode: "rgb",
				Palette: []string{"#FF0000", "#00FF00", "#0000FF"},
			},
			indexed: false,
		},
		{
			// An indexed source with convert_to_indexed=false stays indexed.
			name: "indexed source kept indexed",
			result: QuantizePaletteOutput{
				Success: true, QuantizedColors: 3, ColorMode: "indexed",
				Palette: []string{"#FF0000", "#00FF00", "#0000FF"},
			},
			indexed: false,
		},
		{
			name: "not successful",
			result: QuantizePaletteOutput{
				Success: false, QuantizedColors: 3, ColorMode: "indexed",
				Palette: []string{"#FF0000", "#00FF00", "#0000FF"},
			},
			indexed: true, wantErr: true, errSubstring: "reported failure",
		},
		{
			name: "color count mismatch",
			result: QuantizePaletteOutput{
				Success: true, QuantizedColors: 2, ColorMode: "indexed",
				Palette: []string{"#FF0000", "#00FF00"},
			},
			indexed: true, wantErr: true, errSubstring: "colors but",
		},
		{
			name: "palette length mismatch",
			result: QuantizePaletteOutput{
				Success: true, QuantizedColors: 3, ColorMode: "indexed",
				Palette: []string{"#FF0000", "#00FF00"},
			},
			indexed: true, wantErr: true, errSubstring: "palette entries",
		},
		{
			name: "palette content mismatch",
			result: QuantizePaletteOutput{
				Success: true, QuantizedColors: 3, ColorMode: "indexed",
				Palette: []string{"#FF0000", "#00FF00", "#123456"},
			},
			indexed: true, wantErr: true, errSubstring: "differs from the quantized palette",
		},
		{
			name: "expected indexed got rgb",
			result: QuantizePaletteOutput{
				Success: true, QuantizedColors: 3, ColorMode: "rgb",
				Palette: []string{"#FF0000", "#00FF00", "#0000FF"},
			},
			indexed: true, wantErr: true, errSubstring: "expected indexed",
		},
		{
			name: "unknown color mode",
			result: QuantizePaletteOutput{
				Success: true, QuantizedColors: 3, ColorMode: "cmyk",
				Palette: []string{"#FF0000", "#00FF00", "#0000FF"},
			},
			indexed: true, wantErr: true, errSubstring: "unexpected color mode",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateQuantizeResult(&tt.result, requested, tt.indexed)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if tt.errSubstring != "" && !containsSubstring(err.Error(), tt.errSubstring) {
					t.Errorf("error %q does not contain %q", err.Error(), tt.errSubstring)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func containsSubstring(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}

func TestLockSpritePath_SerializesSamePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sprite.aseprite")

	var inside int32
	var maxInside int32
	var wg sync.WaitGroup
	var mu sync.Mutex

	const goroutines = 8
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock := lockSpritePath(path)
			mu.Lock()
			inside++
			if inside > maxInside {
				maxInside = inside
			}
			mu.Unlock()

			time.Sleep(2 * time.Millisecond)

			mu.Lock()
			inside--
			mu.Unlock()
			unlock()
		}()
	}
	wg.Wait()

	if maxInside != 1 {
		t.Errorf("max concurrent holders = %d, want 1 (lock did not serialize)", maxInside)
	}

	// After all releases the entry map must be empty (no leak).
	quantizationLocks.Lock()
	remaining := len(quantizationLocks.entries)
	quantizationLocks.Unlock()
	if remaining != 0 {
		t.Errorf("lock entries leaked: %d remaining", remaining)
	}
}

func TestLockSpritePath_DifferentPathsConcurrent(t *testing.T) {
	dir := t.TempDir()
	p1 := filepath.Join(dir, "a.aseprite")
	p2 := filepath.Join(dir, "b.aseprite")

	u1 := lockSpritePath(p1)
	// Acquiring a different path must not block.
	done := make(chan struct{})
	go func() {
		u2 := lockSpritePath(p2)
		u2()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("locking a different path blocked")
	}
	u1()
}

func TestLockSpritePath_UnlockIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sprite.aseprite")
	unlock := lockSpritePath(path)
	unlock()
	// Calling again must not panic or corrupt the ref count.
	unlock()

	quantizationLocks.Lock()
	remaining := len(quantizationLocks.entries)
	quantizationLocks.Unlock()
	if remaining != 0 {
		t.Errorf("lock entries leaked after double unlock: %d", remaining)
	}

	// A subsequent lock must still work.
	u := lockSpritePath(path)
	u()
}

func TestCopyFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	dst := filepath.Join(dir, "dst.bin")

	content := []byte("hello aseprite")
	if err := os.WriteFile(src, content, 0644); err != nil {
		t.Fatal(err)
	}

	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copyFile() error: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) {
		t.Errorf("copied content = %q, want %q", got, content)
	}
}

func TestRestoreAndFail_RestoresContent(t *testing.T) {
	dir := t.TempDir()
	spritePath := filepath.Join(dir, "sprite.aseprite")
	backupPath := filepath.Join(dir, "backup.aseprite")

	original := []byte("ORIGINAL SPRITE BYTES")
	if err := os.WriteFile(backupPath, original, 0600); err != nil {
		t.Fatal(err)
	}
	// Simulate a corrupted/partial write.
	if err := os.WriteFile(spritePath, []byte("CORRUPT"), 0600); err != nil {
		t.Fatal(err)
	}

	cause := context.DeadlineExceeded
	err := restoreAndFail(discardLogger(), spritePath, backupPath, cause)
	if err == nil {
		t.Fatal("restoreAndFail should return an error")
	}

	got, readErr := os.ReadFile(spritePath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != string(original) {
		t.Errorf("restored content = %q, want %q", got, original)
	}
}

func TestRestoreAndFail_ReportsRestoreFailure(t *testing.T) {
	dir := t.TempDir()
	spritePath := filepath.Join(dir, "missing-dir", "sprite.aseprite")
	backupPath := filepath.Join(dir, "backup.aseprite")
	if err := os.WriteFile(backupPath, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}

	err := restoreAndFail(discardLogger(), spritePath, backupPath, context.Canceled)
	if err == nil {
		t.Fatal("expected error")
	}
	if !containsSubstring(err.Error(), "failed to restore") {
		t.Errorf("expected restore failure to be reported, got: %v", err)
	}
}

func TestDecodeImageFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "img.png")

	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 60), G: uint8(y * 60), B: 0, A: 255})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, img); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	decoded, err := decodeImageFile(path)
	if err != nil {
		t.Fatalf("decodeImageFile() error: %v", err)
	}
	if decoded.Bounds().Dx() != 4 || decoded.Bounds().Dy() != 4 {
		t.Errorf("decoded image size = %dx%d, want 4x4", decoded.Bounds().Dx(), decoded.Bounds().Dy())
	}
}

func TestQuantizePaletteInputDefaults(t *testing.T) {
	// Guard against accidental changes to the tool's schema defaults.
	input := QuantizePaletteInput{}
	if input.PreserveTransparency != nil {
		t.Error("PreserveTransparency should default to nil (set by handler)")
	}
	if input.ConvertToIndexed != nil {
		t.Error("ConvertToIndexed should default to nil (set by handler)")
	}
}

func TestMaxPaletteColorsConstant(t *testing.T) {
	if aseprite.MaxPaletteColors != 256 {
		t.Errorf("MaxPaletteColors = %d, want 256", aseprite.MaxPaletteColors)
	}
}
