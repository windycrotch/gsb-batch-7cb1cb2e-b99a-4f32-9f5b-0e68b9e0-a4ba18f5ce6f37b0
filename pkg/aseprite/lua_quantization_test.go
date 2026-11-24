package aseprite

import (
	"strings"
	"testing"
)

func TestApplyQuantizedPalette_AtomicSingleTransaction(t *testing.T) {
	gen := NewLuaGenerator()
	script := gen.ApplyQuantizedPalette([]string{"#FF0000", "#00FF00", "#0000FF"}, 42, "median_cut", true, false)

	// All mutation must happen inside a single transaction ...
	if strings.Count(script, "app.transaction(") != 1 {
		t.Errorf("expected exactly one app.transaction, found %d", strings.Count(script, "app.transaction("))
	}
	// ... and the sprite must not be flattened (layer/frame structure preserved).
	if strings.Contains(script, "spr:flatten()") {
		t.Error("quantization must not flatten the sprite")
	}
	if strings.Contains(script, "ReplaceWithImage") {
		t.Error("quantization must not use ReplaceWithImage")
	}
	// Saved only after the transaction block closes.
	transEnd := strings.Index(script, "end)\n\n-- Save only after")
	if transEnd == -1 {
		t.Fatal("could not locate transaction boundary followed by save")
	}
	if saveIdx := strings.Index(script, "spr:saveAs"); saveIdx < transEnd {
		t.Error("sprite must be saved after the transaction completes")
	}
	// Color mode is read back from the sprite rather than hardcoded.
	if !strings.Contains(script, "modeName(spr.colorMode)") {
		t.Error("color_mode must be read from the sprite after mutation")
	}
	// Conversion results are checked (Aseprite signals failure via return value).
	if !strings.Contains(script, "if not app.command.ChangePixelFormat") {
		t.Error("ChangePixelFormat result must be checked")
	}
}

func TestApplyQuantizedPalette_DitherMode(t *testing.T) {
	gen := NewLuaGenerator()

	withDither := gen.ApplyQuantizedPalette([]string{"#FF0000", "#00FF00"}, 8, "median_cut", true, true)
	if !strings.Contains(withDither, `dithering="error-diffusion"`) && !strings.Contains(withDither, `dithering=ditheringMode`) {
		t.Error("dither=true should request error-diffusion dithering")
	}
	if !strings.Contains(withDither, `local ditheringMode = "error-diffusion"`) {
		t.Error("dither=true should set ditheringMode to error-diffusion")
	}

	withoutDither := gen.ApplyQuantizedPalette([]string{"#FF0000", "#00FF00"}, 8, "median_cut", true, false)
	if !strings.Contains(withoutDither, `local ditheringMode = "none"`) {
		t.Error("dither=false should not enable dithering")
	}
}

func TestApplyQuantizedPalette_ConvertToIndexedFlag(t *testing.T) {
	gen := NewLuaGenerator()

	indexed := gen.ApplyQuantizedPalette([]string{"#FF0000", "#00FF00"}, 8, "median_cut", true, false)
	if !strings.Contains(indexed, "local convertToIndexed = true") {
		t.Error("convert_to_indexed=true should emit convertToIndexed=true")
	}

	keepMode := gen.ApplyQuantizedPalette([]string{"#FF0000", "#00FF00"}, 8, "median_cut", false, false)
	if !strings.Contains(keepMode, "local convertToIndexed = false") {
		t.Error("convert_to_indexed=false should emit convertToIndexed=false")
	}
	// When not converting to indexed, the sprite must end up back in a full-color mode.
	if !strings.Contains(keepMode, `changeFormat("rgb")`) {
		t.Error("convert_to_indexed=false should return the sprite to RGB")
	}
	// Color mode must still be derived from the sprite, never hard-coded.
	if !strings.Contains(keepMode, "modeName(spr.colorMode)") {
		t.Error("color mode must be read back from the sprite")
	}
}

func TestApplyQuantizedPalette_RejectsInvalidInput(t *testing.T) {
	gen := NewLuaGenerator()

	if script := gen.ApplyQuantizedPalette(nil, 0, "median_cut", true, false); !strings.Contains(script, "error(") {
		t.Error("empty palette should produce an error script")
	}

	if script := gen.ApplyQuantizedPalette([]string{"not-a-color"}, 1, "median_cut", true, false); !strings.Contains(script, "error(") {
		t.Error("invalid palette color should produce an error script")
	}

	tooMany := make([]string, MaxPaletteColors+1)
	for i := range tooMany {
		tooMany[i] = "#000000"
	}
	if script := gen.ApplyQuantizedPalette(tooMany, 1, "median_cut", true, false); !strings.Contains(script, "error(") {
		t.Error("palette above the maximum should produce an error script")
	}
}

func TestApplyQuantizedPalette_EmbedsExactPalette(t *testing.T) {
	gen := NewLuaGenerator()
	script := gen.ApplyQuantizedPalette([]string{"#112233", "#AABBCCDD"}, 2, "median_cut", true, false)

	if !strings.Contains(script, "Color{r=17, g=34, b=51, a=255}") {
		t.Error("expected #112233 to be embedded as opaque color")
	}
	if !strings.Contains(script, "Color{r=170, g=187, b=204, a=221}") {
		t.Error("expected #AABBCCDD to be embedded with alpha")
	}
	if !strings.Contains(script, "local expectedColorCount = 2") {
		t.Error("expected color count to be embedded")
	}
}

func TestGetQuantizedState(t *testing.T) {
	gen := NewLuaGenerator()
	script := gen.GetQuantizedState()

	if !strings.Contains(script, "modeName(spr.colorMode)") {
		t.Error("state script must report the sprite's color mode")
	}
	if !strings.Contains(script, "spr.palettes[1]") {
		t.Error("state script must read the sprite palette")
	}
	if !strings.Contains(script, "quantized_colors") || !strings.Contains(script, `"palette"`) {
		t.Error("state script must report palette and color count")
	}
}

func TestReplaceWithImage_StillFlattens(t *testing.T) {
	// The legacy replacement helper is kept for callers that want an explicit
	// flatten-to-image operation; quantization no longer uses it.
	gen := NewLuaGenerator()
	script := gen.ReplaceWithImage("/tmp/image.png")
	if !strings.Contains(script, "spr:flatten()") {
		t.Error("ReplaceWithImage should still flatten (used by explicit replacement flows)")
	}
}
