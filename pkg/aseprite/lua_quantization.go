package aseprite

import (
	"fmt"
)

// ApplyQuantizedPalette generates a Lua script that applies a quantized palette
// to the active sprite in a single, atomic operation.
//
// All mutations happen inside one app.transaction and the sprite is saved only
// after every step has succeeded. If any step fails, Aseprite rolls the sprite
// back and the file on disk is never touched (the caller additionally restores a
// backup as a safety net). Layer and frame structure is preserved: pixels are
// remapped with Aseprite's per-cel pixel-format conversion instead of flattening
// the sprite into a single layer.
//
// Parameters:
//   - palette: slice of hex color strings (#RRGGBB or #RRGGBBAA format) from quantization
//   - originalColors: number of unique colors in the original sprite
//   - algorithm: name of the quantization algorithm used
//   - convertToIndexed: whether to convert the sprite to indexed color mode
//   - dither: whether error-diffusion (Floyd-Steinberg) dithering is applied while
//     remapping pixels to the palette
//
// Color mode behavior:
//   - convertToIndexed=true: the sprite ends in indexed mode.
//   - convertToIndexed=false: the sprite keeps a full-color mode. RGB sprites
//     stay RGB; indexed sprites stay indexed (an indexed sprite cannot hold the
//     remapped colors without a palette); grayscale sprites become RGB because a
//     multi-color palette cannot be represented in grayscale. Pixels are always
//     remapped to the quantized palette and, when dither is set, dithered.
//
// The script prints JSON with the palette, quantized color count and color mode
// read back from the sprite after the operation, so the reported values always
// match the file that was saved.
func (g *LuaGenerator) ApplyQuantizedPalette(palette []string, originalColors int, algorithm string, convertToIndexed bool, dither bool) string {
	if len(palette) == 0 {
		return `error("No palette colors provided")`
	}
	if len(palette) > MaxPaletteColors {
		return fmt.Sprintf(`error("Palette has %d colors, which exceeds the maximum of %d")`, len(palette), MaxPaletteColors)
	}

	// Build palette color list. Every entry must parse exactly, otherwise the
	// written palette would silently differ from the requested one.
	colorList := "{\n"
	for i, hexColor := range palette {
		var c Color
		if err := c.FromHex(hexColor); err != nil {
			return fmt.Sprintf(`error("Invalid palette color: %s")`, EscapeString(hexColor))
		}
		colorList += fmt.Sprintf("\t\tColor{r=%d, g=%d, b=%d, a=%d}", c.R, c.G, c.B, c.A)
		if i < len(palette)-1 {
			colorList += ","
		}
		colorList += "\n"
	}
	colorList += "\t}"

	ditheringMode := "none"
	if dither {
		ditheringMode = "error-diffusion"
	}

	convertFlag := "false"
	if convertToIndexed {
		convertFlag = "true"
	}

	return fmt.Sprintf(`local spr = app.activeSprite
if not spr then
	error("No active sprite")
end

local expectedColorCount = %d
local colors = %s
if #colors ~= expectedColorCount then
	error("Palette color count mismatch: expected " .. expectedColorCount .. ", got " .. #colors)
end

local function modeName(mode)
	if mode == ColorMode.INDEXED then
		return "indexed"
	elseif mode == ColorMode.GRAYSCALE then
		return "grayscale"
	else
		return "rgb"
	end
end

local convertToIndexed = %s
local ditheringMode = "%s"
local originalMode = modeName(spr.colorMode)
-- An indexed sprite cannot hold RGB pixels without a palette, so it stays
-- indexed even when convert_to_indexed is false. Everything else ends in one
-- of the two honest targets below.
local finalIndexed = convertToIndexed or (originalMode == "indexed")

-- Aseprite reports a disabled/failed command by returning false rather than
-- raising an error, so every conversion result is checked explicitly.
local function changeFormat(format)
	if not app.command.ChangePixelFormat{format=format, dithering=ditheringMode} then
		error("ChangePixelFormat failed for format: " .. format)
	end
end

local newPalette = Palette(#colors)
for i, color in ipairs(colors) do
	newPalette:setColor(i - 1, color)
end

-- Single transaction: either every mutation succeeds together or none of them
-- are kept. The file is saved only after the transaction completes.
--
-- Layers and frames are never flattened: ChangePixelFormat remaps every cel
-- independently, so the sprite structure is preserved.
app.transaction(function()
	-- Indexed sprites store palette indices and grayscale sprites store gray
	-- values; neither carries enough information to remap against a full-color
	-- palette. Convert both to RGB first so each pixel has an explicit color.
	if spr.colorMode ~= ColorMode.RGB then
		changeFormat("rgb")
	end

	-- Install the quantized palette before remapping pixels.
	spr:setPalette(newPalette)

	-- Remap every pixel to the nearest palette entry. Aseprite applies its
	-- error-diffusion (Floyd-Steinberg) dithering here when requested.
	changeFormat("indexed")

	if not finalIndexed then
		-- Returning to RGB keeps the remapped pixel colors exactly as mapped to
		-- the quantized palette. A multi-color palette cannot be represented in
		-- grayscale, so grayscale sources also end up in RGB here; the mode is
		-- reported as "rgb" below, matching the saved file.
		changeFormat("rgb")
		spr:setPalette(newPalette)
	end

	-- Verify inside the transaction: any mismatch aborts and rolls back.
	local checkPalette = spr.palettes[1]
	if not checkPalette or #checkPalette ~= expectedColorCount then
		error("Palette size mismatch after conversion")
	end
end)

-- Save only after every mutation succeeded.
if not spr:saveAs(spr.filename) then
	error("Failed to save sprite")
end

local finalPalette = spr.palettes[1]
if not finalPalette then
	error("No palette found after quantization")
end

local paletteHex = {}
for i = 0, #finalPalette - 1 do
	local c = finalPalette:getColor(i)
	if c.alpha == 0 then
		table.insert(paletteHex, string.format("#%%02X%%02X%%02X%%02X", c.red, c.green, c.blue, c.alpha))
	else
		table.insert(paletteHex, string.format("#%%02X%%02X%%02X", c.red, c.green, c.blue))
	end
end

if #paletteHex ~= expectedColorCount then
	error("Palette was modified unexpectedly: expected " .. expectedColorCount .. " colors, found " .. #paletteHex)
end

local json = '{"success": true,' ..
	'"original_colors": %d,' ..
	'"quantized_colors": %d,' ..
	'"color_mode": "' .. modeName(spr.colorMode) .. '",' ..
	'"palette": ["' .. table.concat(paletteHex, '","') .. '"],' ..
	'"algorithm_used": "%s"}'

print(json)`,
		len(palette),            // expectedColorCount
		colorList,               // color list
		convertFlag,             // convertToIndexed
		ditheringMode,           // dithering mode
		originalColors,          // original_colors
		len(palette),            // quantized_colors
		EscapeString(algorithm)) // algorithm_used
}

// GetQuantizedState generates a Lua script that reports the sprite's current
// color mode, palette and palette size as JSON.
//
// It is intended to be run on a freshly opened sprite (a separate Aseprite
// process) so the reported values reflect the file on disk rather than the
// in-memory state of the process that wrote it. This lets callers verify that
// what they return to the user matches what was actually saved.
//
// JSON shape: {"color_mode":"indexed","quantized_colors":N,"palette":["#RRGGBB",...]}
func (g *LuaGenerator) GetQuantizedState() string {
	return `local spr = app.activeSprite
if not spr then
	error("No active sprite")
end

local function modeName(mode)
	if mode == ColorMode.INDEXED then
		return "indexed"
	elseif mode == ColorMode.GRAYSCALE then
		return "grayscale"
	else
		return "rgb"
	end
end

local palette = spr.palettes[1]
if not palette then
	error("No palette found")
end

local paletteHex = {}
for i = 0, #palette - 1 do
	local c = palette:getColor(i)
	if c.alpha == 0 then
		table.insert(paletteHex, string.format("#%02X%02X%02X%02X", c.red, c.green, c.blue, c.alpha))
	else
		table.insert(paletteHex, string.format("#%02X%02X%02X", c.red, c.green, c.blue))
	end
end

print('{"color_mode": "' .. modeName(spr.colorMode) .. '",' ..
	'"quantized_colors": ' .. #paletteHex .. ',' ..
	'"palette": ["' .. table.concat(paletteHex, '","') .. '"]}')`
}

// ReplaceWithImage generates a Lua script to replace sprite content with an external image.
// This flattens all layers and replaces the content with the provided image.
func (g *LuaGenerator) ReplaceWithImage(imagePath string) string {
	escapedPath := EscapeString(imagePath)

	return fmt.Sprintf(`local spr = app.activeSprite
if not spr then
	error("No active sprite")
end

-- Load external image
local newImg = Image{ fromFile = "%s" }
if not newImg then
	error("Failed to load image: %s")
end

app.transaction(function()
	-- Flatten all layers to a single layer
	spr:flatten()

	-- Get the flattened layer (should be only layer now)
	local layer = spr.layers[1]
	if not layer then
		error("No layer found after flattening")
	end

	-- Get the cel at frame 1
	local cel = layer:cel(1)
	if not cel then
		-- Create cel if it doesn't exist
		cel = spr:newCel(layer, 1)
	end

	-- Convert color mode if needed
	local finalImg = newImg
	if newImg.colorMode ~= spr.colorMode then
		finalImg = Image(newImg.width, newImg.height, spr.colorMode)
		finalImg:drawImage(newImg, Point(0, 0), 255, BlendMode.SRC)
	end

	-- Replace cel image
	local celX = cel.position.x
	local celY = cel.position.y
	spr:deleteCel(cel)
	spr:newCel(layer, 1, finalImg, celX, celY)
end)

spr:saveAs(spr.filename)
print("Sprite content replaced successfully")`, escapedPath, escapedPath)
}
