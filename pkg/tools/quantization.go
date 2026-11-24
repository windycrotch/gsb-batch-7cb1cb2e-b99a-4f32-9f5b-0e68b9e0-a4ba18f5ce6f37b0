package tools

import (
	"context"
	"fmt"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/willibrandon/mtlog/core"
	"github.com/willibrandon/pixel-mcp/pkg/aseprite"
	"github.com/willibrandon/pixel-mcp/pkg/config"
)

// QuantizePaletteInput defines the input parameters for the quantize_palette tool.
type QuantizePaletteInput struct {
	SpritePath           string `json:"sprite_path" jsonschema:"Path to source .aseprite file"`
	TargetColors         int    `json:"target_colors" jsonschema:"Target palette size (2-256)"`
	Algorithm            string `json:"algorithm" jsonschema:"Quantization algorithm: median_cut (default), kmeans, or octree"`
	Dither               bool   `json:"dither" jsonschema:"Apply Floyd-Steinberg dithering during quantization (default: false)"`
	PreserveTransparency *bool  `json:"preserve_transparency,omitempty" jsonschema:"Keep transparent pixels transparent (default: true)"`
	ConvertToIndexed     *bool  `json:"convert_to_indexed,omitempty" jsonschema:"Convert sprite to indexed color mode (default: true)"`
}

// QuantizePaletteOutput defines the output for the quantize_palette tool.
type QuantizePaletteOutput struct {
	Success         bool     `json:"success" jsonschema:"Whether the operation succeeded"`
	OriginalColors  int      `json:"original_colors" jsonschema:"Number of unique colors in original sprite"`
	QuantizedColors int      `json:"quantized_colors" jsonschema:"Number of colors in quantized palette"`
	ColorMode       string   `json:"color_mode" jsonschema:"Color mode after quantization (indexed or rgb)"`
	Palette         []string `json:"palette" jsonschema:"Array of hex colors in the quantized palette"`
	AlgorithmUsed   string   `json:"algorithm_used" jsonschema:"Quantization algorithm that was used"`
}

// quantizationLocks serializes quantize_palette operations per sprite path so that
// two concurrent calls can never interleave their writes and leave a sprite whose
// palette and pixels come from different batches.
var quantizationLocks = struct {
	sync.Mutex
	entries map[string]*quantizationLockEntry
}{entries: make(map[string]*quantizationLockEntry)}

type quantizationLockEntry struct {
	mu   sync.Mutex
	refs int
}

// lockSpritePath acquires an exclusive lock for spritePath and returns the
// matching unlock function. Paths are normalized (absolute, cleaned, and
// case-folded on Windows) so different spellings of the same file share a lock.
//
// Concurrent calls for the same path wait here and therefore run strictly one
// after another; the last call to finish removes the entry from the map.
func lockSpritePath(spritePath string) func() {
	key := filepath.Clean(spritePath)
	if abs, err := filepath.Abs(key); err == nil {
		key = abs
	}
	if runtime.GOOS == "windows" {
		key = strings.ToLower(key)
	}

	quantizationLocks.Lock()
	entry, ok := quantizationLocks.entries[key]
	if !ok {
		entry = &quantizationLockEntry{}
		quantizationLocks.entries[key] = entry
	}
	entry.refs++
	quantizationLocks.Unlock()

	entry.mu.Lock()

	var once sync.Once
	return func() {
		once.Do(func() {
			entry.mu.Unlock()

			quantizationLocks.Lock()
			entry.refs--
			if entry.refs == 0 {
				delete(quantizationLocks.entries, key)
			}
			quantizationLocks.Unlock()
		})
	}
}

// RegisterQuantizationTools registers the quantize_palette tool with the MCP server.
func RegisterQuantizationTools(server *mcp.Server, client *aseprite.Client, gen *aseprite.LuaGenerator, cfg *config.Config, logger core.Logger) {
	mcp.AddTool(
		server,
		&mcp.Tool{
			Name:        "quantize_palette",
			Description: "Automatically reduce sprite colors using industry-standard quantization algorithms. Supports three algorithms: median_cut (fast, balanced quality), kmeans (highest quality, slower), octree (very fast, good for photos). Can apply Floyd-Steinberg dithering for smoother gradients. Optionally converts to indexed color mode for true palette constraint or keeps RGB mode for flexible multi-pass workflows. The palette and the pixels are written in a single atomic step: on any failure the .aseprite file is left exactly as it was.",
		},
		maybeWrapWithTiming("quantize_palette", logger, cfg.EnableTiming, func(ctx context.Context, req *mcp.CallToolRequest, input QuantizePaletteInput) (*mcp.CallToolResult, *QuantizePaletteOutput, error) {
			opLogger := logger.WithContext(ctx)
			opLogger.Debug("quantize_palette tool called",
				"sprite", input.SpritePath,
				"target_colors", input.TargetColors,
				"algorithm", input.Algorithm)

			// Set defaults
			if input.Algorithm == "" {
				input.Algorithm = "median_cut"
			}
			if input.PreserveTransparency == nil {
				defaultTrue := true
				input.PreserveTransparency = &defaultTrue
			}
			if input.ConvertToIndexed == nil {
				defaultTrue := true
				input.ConvertToIndexed = &defaultTrue
			}

			// Validate inputs
			if input.TargetColors < 2 || input.TargetColors > aseprite.MaxPaletteColors {
				return nil, nil, fmt.Errorf("target_colors must be between 2 and %d, got %d", aseprite.MaxPaletteColors, input.TargetColors)
			}

			validAlgorithms := map[string]bool{
				"median_cut": true,
				"kmeans":     true,
				"octree":     true,
			}
			if !validAlgorithms[input.Algorithm] {
				return nil, nil, fmt.Errorf("invalid algorithm: %s (must be median_cut, kmeans, or octree)", input.Algorithm)
			}

			// Check sprite file exists
			if _, err := os.Stat(input.SpritePath); os.IsNotExist(err) {
				return nil, nil, fmt.Errorf("sprite file not found: %s", input.SpritePath)
			}

			// Serialize all quantization of the same sprite path. The lock is held
			// for the whole export/analyze/write cycle so concurrent calls cannot
			// produce a palette from one batch and pixels from another; the second
			// caller waits and then operates on the completed first result.
			unlock := lockSpritePath(input.SpritePath)
			defer unlock()

			// Step 1: Export sprite to temporary PNG for analysis. This only reads
			// the sprite; the temp directory is always removed, on success and
			// failure alike.
			tempDir, err := os.MkdirTemp("", "pixel-mcp-quantize-*")
			if err != nil {
				return nil, nil, fmt.Errorf("failed to create temp directory: %w", err)
			}
			defer os.RemoveAll(tempDir)

			// Snapshot the sprite so it can be byte-for-byte restored if any later
			// step fails (for example a crash or truncated write inside Aseprite).
			backupPath := filepath.Join(tempDir, "original.aseprite")
			if err := copyFile(input.SpritePath, backupPath); err != nil {
				return nil, nil, fmt.Errorf("failed to back up sprite before quantization: %w", err)
			}

			tempPNG := filepath.Join(tempDir, "sprite.png")

			// Export sprite to PNG using gen.ExportSprite for consistency
			exportScript := gen.ExportSprite(tempPNG, 0)

			if _, err := client.ExecuteLua(ctx, exportScript, input.SpritePath); err != nil {
				return nil, nil, fmt.Errorf("failed to export sprite to PNG: %w", err)
			}

			// Step 2: Load PNG and perform quantization in Go
			img, err := decodeImageFile(tempPNG)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to decode exported PNG: %w", err)
			}

			// Perform quantization
			palette, originalColors, err := aseprite.QuantizePalette(
				img,
				input.TargetColors,
				input.Algorithm,
				*input.PreserveTransparency,
			)
			if err != nil {
				return nil, nil, fmt.Errorf("quantization failed: %w", err)
			}

			// Fail before writing if the palette cannot be stored as-is. This keeps
			// the tool from silently truncating colors and reporting success.
			if len(palette) == 0 {
				return nil, nil, fmt.Errorf("quantization produced an empty palette")
			}
			if len(palette) > aseprite.MaxPaletteColors {
				return nil, nil, fmt.Errorf("quantized palette has %d colors, exceeding the maximum of %d", len(palette), aseprite.MaxPaletteColors)
			}

			opLogger.Information("Quantization completed",
				"original_colors", originalColors,
				"quantized_colors", len(palette),
				"algorithm", input.Algorithm)

			// Step 3: Apply palette, pixel remapping (with optional dithering) and
			// color-mode conversion as one atomic operation inside Aseprite. If this
			// fails, restore the snapshot so the sprite on disk is untouched.
			applyScript := gen.ApplyQuantizedPalette(
				palette,
				originalColors,
				input.Algorithm,
				*input.ConvertToIndexed,
				input.Dither,
			)

			output, applyErr := client.ExecuteLua(ctx, applyScript, input.SpritePath)
			if applyErr != nil {
				opLogger.Error("Failed to apply quantized palette", "error", applyErr)
				return nil, nil, restoreAndFail(opLogger, input.SpritePath, backupPath,
					fmt.Errorf("failed to apply quantized palette: %w", applyErr))
			}

			// Parse JSON output from Lua
			var result QuantizePaletteOutput
			if err := parseJSON(output, &result); err != nil {
				return nil, nil, restoreAndFail(opLogger, input.SpritePath, backupPath,
					fmt.Errorf("failed to parse quantization output: %w", err))
			}

			// Validate that the reported state matches what we asked Aseprite to
			// write, so a partially applied result can never be reported as success.
			if err := validateQuantizeResult(&result, palette, *input.ConvertToIndexed); err != nil {
				return nil, nil, restoreAndFail(opLogger, input.SpritePath, backupPath, err)
			}

			// Re-open the saved file in a fresh Aseprite process and compare its
			// palette and color mode against what we are about to report. This
			// catches a save that silently diverged from the in-memory result, and
			// the returned values are taken from this on-disk read so they describe
			// the file that actually exists.
			savedPalette, savedMode, err := verifySavedSprite(ctx, client, gen, input.SpritePath, &result, palette)
			if err != nil {
				return nil, nil, restoreAndFail(opLogger, input.SpritePath, backupPath, err)
			}

			result.Success = true
			result.OriginalColors = originalColors
			result.AlgorithmUsed = input.Algorithm
			result.Palette = savedPalette
			result.QuantizedColors = len(savedPalette)
			result.ColorMode = savedMode

			opLogger.Information("Palette quantized and applied successfully",
				"sprite", input.SpritePath,
				"original_colors", result.OriginalColors,
				"quantized_colors", result.QuantizedColors,
				"color_mode", result.ColorMode,
				"algorithm", result.AlgorithmUsed)

			return nil, &result, nil
		}),
	)
}

// verifySavedSprite re-opens spritePath in a fresh Aseprite process and checks
// that the palette and color mode on disk match the in-memory result. It returns
// the palette and color mode read from disk so callers can report exactly what
// the file contains. A non-nil error means the saved file diverged from the
// requested quantization.
func verifySavedSprite(ctx context.Context, client *aseprite.Client, gen *aseprite.LuaGenerator, spritePath string, result *QuantizePaletteOutput, requested []string) ([]string, string, error) {
	output, err := client.ExecuteLua(ctx, gen.GetQuantizedState(), spritePath)
	if err != nil {
		return nil, "", fmt.Errorf("failed to verify saved sprite: %w", err)
	}

	var saved struct {
		ColorMode       string   `json:"color_mode"`
		QuantizedColors int      `json:"quantized_colors"`
		Palette         []string `json:"palette"`
	}
	if err := parseJSON(output, &saved); err != nil {
		return nil, "", fmt.Errorf("failed to parse saved sprite state: %w", err)
	}

	if saved.ColorMode != result.ColorMode {
		return nil, "", fmt.Errorf("saved sprite color mode is %q but %q was reported", saved.ColorMode, result.ColorMode)
	}
	if saved.QuantizedColors != len(requested) || len(saved.Palette) != len(requested) {
		return nil, "", fmt.Errorf("saved sprite has %d colors but %d were written", saved.QuantizedColors, len(requested))
	}
	if !palettesEqual(saved.Palette, requested) {
		return nil, "", fmt.Errorf("saved sprite palette differs from the quantized palette")
	}

	return saved.Palette, saved.ColorMode, nil
}

// validateQuantizeResult checks that the state reported by Aseprite is consistent
// with the palette we requested and the requested color-mode conversion.
func validateQuantizeResult(result *QuantizePaletteOutput, requested []string, convertToIndexed bool) error {
	if !result.Success {
		return fmt.Errorf("quantization reported failure")
	}
	if result.QuantizedColors != len(requested) {
		return fmt.Errorf("quantization wrote %d colors but %d were requested", result.QuantizedColors, len(requested))
	}
	if len(result.Palette) != result.QuantizedColors {
		return fmt.Errorf("quantization reported %d palette entries but %d colors", len(result.Palette), result.QuantizedColors)
	}
	if len(result.Palette) > aseprite.MaxPaletteColors {
		return fmt.Errorf("quantization reported %d colors, exceeding the maximum of %d", len(result.Palette), aseprite.MaxPaletteColors)
	}
	if !palettesEqual(result.Palette, requested) {
		return fmt.Errorf("palette written to sprite differs from the quantized palette")
	}

	switch result.ColorMode {
	case "indexed":
		// Indexed is always valid: requested conversion, an already-indexed
		// sprite, or a source that was indexed and is kept indexed.
	case "rgb", "grayscale":
		if convertToIndexed {
			return fmt.Errorf("expected indexed color mode after conversion, got %s", result.ColorMode)
		}
	default:
		return fmt.Errorf("unexpected color mode after quantization: %q", result.ColorMode)
	}

	return nil
}

// palettesEqual reports whether two hex palettes contain the same colors in the
// same order. Hex strings are normalized so "#RRGGBB" and "#RRGGBBAA" with full
// alpha compare equal.
func palettesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if normalizeHexColor(a[i]) != normalizeHexColor(b[i]) {
			return false
		}
	}
	return true
}

// normalizeHexColor uppercases a hex color and drops a fully opaque alpha
// suffix, so "#RRGGBB" and "#RRGGBBAA" (alpha 255) compare equal.
func normalizeHexColor(hex string) string {
	if len(hex) == 9 && strings.HasPrefix(hex, "#") {
		if strings.EqualFold(hex[7:9], "FF") {
			return strings.ToUpper(hex[:7])
		}
		return strings.ToUpper(hex)
	}
	return strings.ToUpper(hex)
}

// restoreAndFail copies the pre-quantization snapshot back over spritePath and
// returns an error that includes any restore failure.
func restoreAndFail(logger core.Logger, spritePath, backupPath string, cause error) error {
	if err := copyFile(backupPath, spritePath); err != nil {
		logger.Error("Failed to restore sprite after quantization failure",
			"sprite", spritePath, "error", err)
		return fmt.Errorf("%w (additionally failed to restore original sprite: %v)", cause, err)
	}
	return cause
}

// copyFile copies src to dst, replacing dst if it already exists. If dst exists,
// its file mode is preserved.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	mode := os.FileMode(0600)
	if info, statErr := os.Stat(dst); statErr == nil {
		mode = info.Mode().Perm()
	}

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// decodeImageFile decodes an image file, falling back to generic format detection.
func decodeImageFile(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	img, err := png.Decode(f)
	if err == nil {
		return img, nil
	}

	if _, seekErr := f.Seek(0, 0); seekErr != nil {
		return nil, err
	}
	img, _, err = image.Decode(f)
	if err != nil {
		return nil, err
	}
	return img, nil
}
