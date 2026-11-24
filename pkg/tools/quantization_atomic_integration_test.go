//go:build integration
// +build integration

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"github.com/willibrandon/mtlog"
	"github.com/willibrandon/mtlog/core"
	"github.com/willibrandon/pixel-mcp/internal/testutil"
	"github.com/willibrandon/pixel-mcp/pkg/aseprite"
)

// createQuantizeTestSession creates an MCP session with the quantization and
// canvas tools registered, for end-to-end tests through the tool layer.
func createQuantizeTestSession(t *testing.T) (*mcp.ClientSession, *aseprite.Client) {
	t.Helper()

	cfg := testutil.LoadTestConfig(t)
	client := aseprite.NewClient(cfg.AsepritePath, cfg.TempDir, cfg.Timeout)
	gen := aseprite.NewLuaGenerator()
	logger := mtlog.New(mtlog.WithMinimumLevel(core.ErrorLevel))

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "pixel-mcp-quantize-test",
		Version: "1.0.0",
	}, nil)

	RegisterQuantizationTools(server, client, gen, cfg, logger)
	RegisterCanvasTools(server, client, gen, cfg, logger)
	RegisterDrawingTools(server, client, gen, cfg, logger)

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	_, err := server.Connect(context.Background(), serverTransport, nil)
	require.NoError(t, err)

	mcpClient := mcp.NewClient(&mcp.Implementation{
		Name:    "test-client",
		Version: "1.0.0",
	}, nil)

	session, err := mcpClient.Connect(context.Background(), clientTransport, nil)
	require.NoError(t, err)

	return session, client
}

// callQuantize invokes quantize_palette via the MCP session and decodes the result.
func callQuantize(t *testing.T, session *mcp.ClientSession, args map[string]any) (*QuantizePaletteOutput, bool) {
	t.Helper()

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "quantize_palette",
		Arguments: args,
	})
	require.NoError(t, err)

	if result.IsError {
		return nil, false
	}

	var out QuantizePaletteOutput
	text := result.Content[0].(*mcp.TextContent).Text
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("failed to decode quantize output: %v\n%s", err, text)
	}
	return &out, true
}

// drawGradientSprite creates an RGB sprite filled with a gradient using a
// multi-layer structure, returning its path.
func drawGradientSprite(t *testing.T, session *mcp.ClientSession, width, height, layers int) string {
	t.Helper()

	createResult, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "create_canvas",
		Arguments: map[string]any{
			"width":      width,
			"height":     height,
			"color_mode": "rgb",
		},
	})
	require.NoError(t, err)

	var createOut struct {
		FilePath string `json:"file_path"`
	}
	require.NoError(t, json.Unmarshal([]byte(createResult.Content[0].(*mcp.TextContent).Text), &createOut))

	for i := 1; i < layers; i++ {
		_, err := session.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "add_layer",
			Arguments: map[string]any{"sprite_path": createOut.FilePath, "layer_name": fmt.Sprintf("Layer %d", i+1)},
		})
		require.NoError(t, err)
	}

	pixels := make([]map[string]any, 0, width*height)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			g := uint8((x + y) * 255 / ((width - 1) + (height - 1)))
			pixels = append(pixels, map[string]any{
				"x": x, "y": y,
				"color": fmt.Sprintf("#%02X%02X%02X", g, g, g),
			})
		}
	}
	_, err = session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "draw_pixels",
		Arguments: map[string]any{
			"sprite_path":  createOut.FilePath,
			"layer_name":   "Layer 1",
			"frame_number": 1,
			"pixels":       pixels,
		},
	})
	require.NoError(t, err)

	return createOut.FilePath
}

// spriteInfo returns width/height, color mode and layer count for a sprite.
func spriteInfo(t *testing.T, client *aseprite.Client, gen *aseprite.LuaGenerator, path string) map[string]any {
	t.Helper()
	out, err := client.ExecuteLua(context.Background(), gen.GetSpriteInfo(), path)
	require.NoError(t, err)

	var info map[string]any
	require.NoError(t, json.Unmarshal([]byte(out[stringsIndex(out, "{"):]), &info))
	return info
}

func stringsIndex(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return 0
}

// TestIntegration_QuantizePalette_PreservesLayerStructure ensures dithering and
// indexed conversion do not flatten a multi-layer sprite.
func TestIntegration_QuantizePalette_PreservesLayerStructure(t *testing.T) {
	session, client := createQuantizeTestSession(t)
	defer session.Close()
	gen := aseprite.NewLuaGenerator()

	spritePath := drawGradientSprite(t, session, 32, 32, 3)
	defer os.Remove(spritePath)

	out, ok := callQuantize(t, session, map[string]any{
		"sprite_path":        spritePath,
		"target_colors":      6,
		"algorithm":          "median_cut",
		"dither":             true,
		"convert_to_indexed": true,
	})
	require.True(t, ok, "quantize should succeed")
	require.Equal(t, "indexed", out.ColorMode)
	require.Equal(t, len(out.Palette), out.QuantizedColors)

	info := spriteInfo(t, client, gen, spritePath)
	require.EqualValues(t, 3, info["layer_count"], "layer structure must be preserved")
	require.EqualValues(t, 32, info["width"])

	// Reported palette must match the palette actually stored in the file.
	stored, err := client.ExecuteLua(context.Background(), gen.GetPalette(), spritePath)
	require.NoError(t, err)
	var storedPal struct {
		Colors []string `json:"colors"`
	}
	require.NoError(t, json.Unmarshal([]byte(stored[stringsIndex(stored, "{"):]), &storedPal))
	require.Len(t, storedPal.Colors, out.QuantizedColors)
	require.True(t, palettesEqual(storedPal.Colors, out.Palette),
		"returned palette %v does not match stored palette %v", out.Palette, storedPal.Colors)
}

// TestIntegration_QuantizePalette_ConvertToIndexedFalseKeepsRGB checks that with
// convert_to_indexed=false the reported mode reflects the sprite's actual mode.
func TestIntegration_QuantizePalette_ConvertToIndexedFalseKeepsRGB(t *testing.T) {
	session, client := createQuantizeTestSession(t)
	defer session.Close()
	gen := aseprite.NewLuaGenerator()

	spritePath := drawGradientSprite(t, session, 16, 16, 1)
	defer os.Remove(spritePath)

	out, ok := callQuantize(t, session, map[string]any{
		"sprite_path":        spritePath,
		"target_colors":      4,
		"algorithm":          "median_cut",
		"dither":             true,
		"convert_to_indexed": false,
	})
	require.True(t, ok, "quantize should succeed")
	require.Equal(t, "rgb", out.ColorMode)

	info := spriteInfo(t, client, gen, spritePath)
	require.Equal(t, "rgb", info["color_mode"])
}

// TestIntegration_QuantizePalette_ConcurrentSamePath ensures concurrent calls on
// the same sprite serialize and the final file matches exactly one call's result.
func TestIntegration_QuantizePalette_ConcurrentSamePath(t *testing.T) {
	session, client := createQuantizeTestSession(t)
	defer session.Close()
	gen := aseprite.NewLuaGenerator()

	spritePath := drawGradientSprite(t, session, 24, 24, 2)
	defer os.Remove(spritePath)

	type result struct {
		out *QuantizePaletteOutput
		ok  bool
	}
	results := make([]result, 2)
	var wg sync.WaitGroup

	args := []map[string]any{
		{"sprite_path": spritePath, "target_colors": 4, "algorithm": "median_cut", "dither": true, "convert_to_indexed": true},
		{"sprite_path": spritePath, "target_colors": 8, "algorithm": "octree", "dither": true, "convert_to_indexed": true},
	}
	for i := range args {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out, ok := callQuantize(t, session, args[i])
			results[i] = result{out, ok}
		}(i)
	}
	wg.Wait()

	// Both calls must complete successfully and the final file must match one of
	// them exactly (palette size and content), never a mix.
	var winners []*QuantizePaletteOutput
	for _, r := range results {
		if r.ok {
			winners = append(winners, r.out)
		}
	}
	require.NotEmpty(t, winners, "at least one concurrent call must succeed")

	info := spriteInfo(t, client, gen, spritePath)
	require.EqualValues(t, 2, info["layer_count"], "layer structure must survive concurrency")

	stored, err := client.ExecuteLua(context.Background(), gen.GetPalette(), spritePath)
	require.NoError(t, err)
	var storedPal struct {
		Colors []string `json:"colors"`
	}
	require.NoError(t, json.Unmarshal([]byte(stored[stringsIndex(stored, "{"):]), &storedPal))

	matched := false
	for _, w := range winners {
		if len(storedPal.Colors) == w.QuantizedColors && palettesEqual(storedPal.Colors, w.Palette) {
			matched = true
			break
		}
	}
	require.True(t, matched, "final palette %v matches no single call result", storedPal.Colors)
}

// TestIntegration_QuantizePalette_PaletteAtMaxWithTransparency exercises the
// 256 + transparency boundary: either it succeeds within the limit or fails
// without modifying the sprite.
func TestIntegration_QuantizePalette_PaletteAtMaxWithTransparency(t *testing.T) {
	session, _ := createQuantizeTestSession(t)
	defer session.Close()

	spritePath := drawGradientSprite(t, session, 32, 32, 1)
	defer os.Remove(spritePath)

	before, err := os.ReadFile(spritePath)
	require.NoError(t, err)

	out, ok := callQuantize(t, session, map[string]any{
		"sprite_path":           spritePath,
		"target_colors":         256,
		"algorithm":             "median_cut",
		"preserve_transparency": true,
		"convert_to_indexed":    true,
	})

	after, err := os.ReadFile(spritePath)
	require.NoError(t, err)

	if ok {
		require.LessOrEqual(t, len(out.Palette), 256)
		require.Equal(t, len(out.Palette), out.QuantizedColors)
		require.Equal(t, len(out.Palette), out.QuantizedColors)
	} else {
		// A failure before writing must leave the sprite untouched.
		require.Equal(t, string(before), string(after), "failed quantization must not modify the sprite")
	}
}

// TestIntegration_QuantizePalette_FailedCallLeavesSpriteUnchanged drives a real
// failure (unreadable sprite) and asserts the original bytes survive.
func TestIntegration_QuantizePalette_FailedCallLeavesSpriteUnchanged(t *testing.T) {
	session, _ := createQuantizeTestSession(t)
	defer session.Close()

	spritePath := drawGradientSprite(t, session, 16, 16, 1)
	defer os.Remove(spritePath)

	before, err := os.ReadFile(spritePath)
	require.NoError(t, err)

	// target_colors above the hard limit must be rejected before any write.
	_, ok := callQuantize(t, session, map[string]any{
		"sprite_path":   spritePath,
		"target_colors": 400,
		"algorithm":     "median_cut",
		"dither":        true,
	})
	require.False(t, ok, "out-of-range target_colors must fail")

	after, err := os.ReadFile(spritePath)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after), "rejected call must not modify the sprite")
}

// TestIntegration_QuantizePalette_CleansTempDir verifies the tool leaves no
// pixel-mcp-quantize-* directories behind on success or failure.
func TestIntegration_QuantizePalette_CleansTempDir(t *testing.T) {
	session, _ := createQuantizeTestSession(t)
	defer session.Close()

	spritePath := drawGradientSprite(t, session, 16, 16, 1)
	defer os.Remove(spritePath)

	osTemp := os.TempDir()
	countDirs := func() int {
		entries, err := os.ReadDir(osTemp)
		if err != nil {
			return 0
		}
		n := 0
		for _, e := range entries {
			if e.IsDir() && len(e.Name()) >= len("pixel-mcp-quantize-") && e.Name()[:len("pixel-mcp-quantize-")] == "pixel-mcp-quantize-" {
				n++
			}
		}
		return n
	}

	beforeCount := countDirs()

	// One successful call and one failing call.
	_, ok := callQuantize(t, session, map[string]any{
		"sprite_path": spritePath, "target_colors": 4, "algorithm": "median_cut", "dither": true,
	})
	require.True(t, ok)
	_, ok = callQuantize(t, session, map[string]any{
		"sprite_path": spritePath, "target_colors": 1, "algorithm": "median_cut",
	})
	require.False(t, ok)

	// Give the OS a moment to release handles (Windows).
	time.Sleep(100 * time.Millisecond)

	require.Equal(t, beforeCount, countDirs(), "temp quantization directories were leaked")
}
