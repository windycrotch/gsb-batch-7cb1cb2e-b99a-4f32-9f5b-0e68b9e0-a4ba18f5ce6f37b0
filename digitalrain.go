package main

import (
	"github.com/gopherjs/gopherjs/js"
)

var (
	showBlueHeads       = true
	overlap             = 0                                    // 0-N, allowable drop overlaps for a column
	background          = "linear-gradient(#ccddee, #ffffff);" //"#000000"
	githubLinkColor     = "rgba(107,165,184,.5)"               //"rgba(59,128,109,.5)"
	githubLinkOverColor = "rgba(107,165,184,1)"                //"rgba(59,128,109,1)"
	githubLink          = "http://github.com/tidwall/digitalrain"
	level1Cols          = 40
	level2Cols          = 60
)

var lowGlyphCanvases []*GlyphCanvas
var highGlyphCanvases []*GlyphCanvas
var backgrounds []string
var index = 1

func main() {
	sheet := js.Global.Get("document").Call("createElement", "style")
	sheet.Set("innerHTML",
		`html, body { 
			padding:0; margin:0; border:0; width:100%; height:100%; overflow:hidden;
		}
		html{
			background: black;
		}`)
	js.Global.Get("document").Get("head").Call("appendChild", sheet)
	js.Global.Get("document").Set("title", "whoa")
	js.Global.Call("addEventListener", "load", func() {
		lowGlyphCanvases = []*GlyphCanvas{NewGlyphCanvas("#6ba5b8"), NewGlyphCanvas("#3b806d")}
		highGlyphCanvases = []*GlyphCanvas{NewGlyphCanvas("#5b95a8"), NewGlyphCanvas("#5b9b9b")}
		backgrounds = []string{"#dcedfe", "#000000"}
		rain1, err := NewDigitalRain(js.Global.Get("document").Get("body"), level2Cols, 2, 8, 0.25)
		if err != nil {
			println(err.Error())
			return
		}
		cover := js.Global.Get("document").Call("createElement", "div")
		cover.Get("style").Set("height", "100%")
		cover.Get("style").Set("width", "100%")
		cover.Get("style").Set("background-image", "radial-gradient(ellipse farthest-corner at 45px 45px , #00FFFF 0%, rgba(0, 0, 255, 0) 50%, #0000FF 95%)")
		cover.Get("style").Set("opacity", "0.18")
		cover.Get("style").Set("position", "absolute")
		js.Global.Get("document").Get("body").Call("appendChild", cover)

		js.Global.Call("addEventListener", "resize", func() {
			rain1.layout()
		})
		rain2, err := NewDigitalRain(js.Global.Get("document").Get("body"), level1Cols, 2, 12, 1.0)
		if err != nil {
			println(err.Error())
			return
		}
		js.Global.Call("addEventListener", "resize", func() {
			rain2.layout()
		})
		rain2.Clicked = func() {
			index++
			rain1.lowGlyphCanvas = lowGlyphCanvases[index%2]
			rain1.highGlyphCanvas = highGlyphCanvases[index%2]
			rain2.lowGlyphCanvas = lowGlyphCanvases[index%2]
			rain2.highGlyphCanvas = highGlyphCanvases[index%2]

			js.Global.Get("document").Get("body").Get("style").Set("background", backgrounds[index%2])
		}
		// Exposed for browser-side verification of rebuild behavior:
		// generation counts, canvas node counts, listener checks.
		js.Global.Set("digitalRain", map[string]interface{}{
			"rain1": rain1,
			"rain2": rain2,
		})
	})
}

type Duration float64

const Second Duration = 1

func itoa(i int) string {
	return js.Global.Get("String").New(i).String()
}
func ftoa(f float64) string {
	return js.Global.Get("String").New(f).String()
}
func randi() int {
	return int(js.Global.Get("Math").Call("random").Float() * 2147483647.0)
}

type DigitalRain struct {
	parent, canvas  *js.Object
	ctx             *js.Object
	width, height   float64
	ratio           float64
	timestamp       Duration
	lowGlyphCanvas  *GlyphCanvas
	highGlyphCanvas *GlyphCanvas
	drops           []*waterDrop
	linkover        bool
	screenCols      int
	minSpeed        int
	maxSpeed        int
	brightness      float64
	generation      int // incremented on every successful canvas rebuild
	Clicked         func()
}

// Generation returns how many times the canvas has been rebuilt.
// Exposed for browser-side verification (window.digitalRain).
func (r *DigitalRain) Generation() int {
	return r.generation
}

func NewDigitalRain(parent *js.Object, screenCols int, minSpeed int, maxSpeed int, brightness float64) (*DigitalRain, error) {
	rain := &DigitalRain{parent: parent}
	rain.screenCols = screenCols
	rain.minSpeed = minSpeed
	rain.maxSpeed = maxSpeed
	rain.brightness = brightness
	if err := rain.start(); err != nil {
		return nil, err
	}
	return rain, nil
}
func (r *DigitalRain) start() error {
	var raf string
	for _, s := range []string{"requestAnimationFrame", "webkitRequestAnimationFrame", "mozRequestAnimationFrame"} {
		if js.Global.Get(s) != js.Undefined {
			raf = s
			break
		}
	}
	if raf == "" {
		panic("requestAnimationFrame is not available")
	}
	defer r.layout()
	var f func(*js.Object)
	f = func(timestampJS *js.Object) {
		js.Global.Call(raf, f)
		r.loop(Duration(timestampJS.Float() / 1000))
	}
	js.Global.Call(raf, f)
	return nil
}

func (r *DigitalRain) layout() {
	ratio := js.Global.Get("devicePixelRatio").Float()
	if ratio <= 0 {
		ratio = 1
	}
	width := r.parent.Get("offsetWidth").Float() * ratio
	height := r.parent.Get("offsetHeight").Float() * ratio
	if width <= 0 || height <= 0 {
		// The parent has no measurable size (e.g. display:none or a
		// zero-sized container). Keep the previous canvas untouched so
		// the page never ends up blank or half-initialized.
		return
	}
	if r.canvas != nil && r.width == width && r.height == height && r.ratio == ratio {
		return
	}

	// Build the replacement canvas first. Only when it is fully usable
	// do we swap it in, so a failure here leaves the old canvas running.
	canvas := js.Global.Get("document").Call("createElement", "canvas")
	ctx := canvas.Call("getContext", "2d")
	if ctx == nil || !js.Global.Call("Boolean", ctx).Bool() {
		// No 2D context available; keep rendering to the old canvas.
		return
	}
	canvas.Set("width", width)
	canvas.Set("height", height)
	canvas.Get("style").Set("width", ftoa(width/ratio)+"px")
	canvas.Get("style").Set("height", ftoa(height/ratio)+"px")
	canvas.Get("style").Set("position", "absolute")
	canvas.Get("style").Set("left", "0")
	canvas.Get("style").Set("top", "0")

	// Swap atomically and preserve the stacking order: insert the new
	// canvas exactly where the old one was, then remove the old node.
	// The old node (and with it its listeners) leaves the DOM here, so
	// the number of canvas nodes never grows with rebuilds.
	if r.canvas != nil {
		r.parent.Call("insertBefore", canvas, r.canvas)
		r.parent.Call("removeChild", r.canvas)
	} else {
		r.parent.Call("appendChild", canvas)
	}
	r.canvas = canvas
	r.ctx = ctx
	r.width, r.height, r.ratio = width, height, ratio
	r.generation++

	if r.highGlyphCanvas == nil {
		r.highGlyphCanvas = highGlyphCanvases[index%2]
	}
	if r.lowGlyphCanvas == nil {
		r.lowGlyphCanvas = lowGlyphCanvases[index%2]
	}

	// The handlers capture the generation they were registered for.
	// A rebuild bumps r.generation, so even if a stale canvas somehow
	// still receives an event, its callbacks are inert.
	gen := r.generation
	r.canvas.Call("addEventListener", "click", func(ev *js.Object) {
		if gen != r.generation {
			return // stale canvas, ignore
		}
		if r.overLink(ev.Get("x").Int(), ev.Get("y").Int()) {
			js.Global.Set("location", githubLink)

		} else {
			if r.Clicked != nil {
				r.Clicked()
			}
		}
	})

	r.canvas.Call("addEventListener", "mousemove", func(ev *js.Object) {
		if gen != r.generation {
			return // stale canvas, ignore
		}
		if r.overLink(ev.Get("x").Int(), ev.Get("y").Int()) {
			r.canvas.Get("style").Set("cursor", "pointer")
			r.linkover = true
		} else {
			r.canvas.Get("style").Set("cursor", "default")
			r.linkover = false
		}
	})

	// Repaint the current state onto the new canvas without advancing
	// time and without spawning new drops: drops keep their column,
	// row, speed and glyph sequence, so the migration is visually
	// continuous. The next animation frame advances by the real
	// elapsed time only, because r.timestamp is left untouched here.
	r.render()
}

func (r *DigitalRain) overLink(x int, y int) bool {
	return x > int(r.width/r.ratio)-320 && y > int(r.height/r.ratio)-50
}

type waterDrop struct {
	col     int     // the column the drop it dropping
	row     float64 // the row of the bottom most cell
	start   float64 // the starting row
	speed   float64 // how many cells per second
	glyphs  []int   // random glyph table
	spedup  bool
	created Duration
}

func (r *DigitalRain) dropWaterAtCol(col int, speed float64, length int, start float64, created Duration) {
	wd := waterDrop{}
	wd.col = col
	wd.speed = speed
	wd.glyphs = make([]int, length)
	for i := 0; i < length; i++ {
		wd.glyphs[i] = randi() % glyphsCount
	}
	r.drops = append(r.drops, &wd)

	wd.row = start
	wd.start = wd.row
	wd.created = created
}

func (r *DigitalRain) dropRandomWaterDrop(timestamp Duration) {
	col := randi() % r.screenCols
	colcnt := 0
	for _, drop := range r.drops {
		if drop.col == col && int(drop.row)-len(drop.glyphs) < 0 {
			colcnt++
			if colcnt > overlap {
				return // no space
			}
		}
	}
	speed := float64(randi()%(r.maxSpeed-r.minSpeed) + r.minSpeed)
	length := randi()%30 + 10
	start := float64((randi() % r.maxRows()) - r.maxRows()/2)
	r.dropWaterAtCol(col, speed, length, start, timestamp)
}
func (r *DigitalRain) maxRows() int {
	cellSize := r.width / float64(r.screenCols)
	return int((r.height / cellSize) + 2)
}
func (r *DigitalRain) drawGlyphAt(nidx int, col int, row float64, brightness float64, head bool) {
	if col < 0 || col > r.screenCols || row < -1 || row > float64(r.maxRows()) {
		return
	}
	r.drawGlyphElAt(r.lowGlyphCanvas, nidx, col, row, brightness)
	if head {
		r.drawGlyphElAt(r.highGlyphCanvas, nidx, col, row, brightness)
	}
}
func (r *DigitalRain) drawGlyphElAt(glyphCanvas *GlyphCanvas, nidx int, col int, row float64, brightness float64) {
	if brightness <= 0.05 {
		return
	}
	if brightness > 1 {
		brightness = 1
	}
	cellSize := r.width / float64(r.screenCols)
	gy := int(nidx/glyphsCols) * glyphCellSize
	gx := int(nidx%glyphsCols) * glyphCellSize
	cx := cellSize*float64(col) + cellSize/2 - (cellSize*1.5)/2
	cy := cellSize * float64(row)
	glyph := glyphCanvas.Glyph(gx, gy)
	if glyph != nil {
		r.ctx.Call("save")
		r.ctx.Set("globalAlpha", brightness)
		r.ctx.Call("drawImage", glyph, cx, cy, cellSize*1.5, cellSize*1.5)
		r.ctx.Call("restore")
	}
}

func shortLink(link string) string {
	for i := 0; i < len(link); i++ {
		if link[i] == ':' && i+2 < len(link) && link[i+1] == '/' && link[i+2] == '/' {

			return link[i+3:]
		}
	}
	return link
}

func (r *DigitalRain) drawTitle(text string, color string, fontSize float64, y float64) float64 {
	ny := y + (fontSize * 1.5)
	pad := 15 * r.ratio
	x := r.width - pad
	y = r.height - pad - y
	r.ctx.Call("save")
	r.ctx.Set("font", itoa(int(fontSize))+"px Menlo, Consolas, Monospace, Helvetica, Arial, Sans-Serif")
	r.ctx.Set("textAlign", "right")
	r.ctx.Set("lineWidth", 0)
	r.ctx.Set("shadowColor", color)
	r.ctx.Set("shadowBlur", float64(fontSize))
	r.ctx.Set("fillStyle", color)
	r.ctx.Call("fillText", text, x, y)
	r.ctx.Call("restore")
	return ny
}

func (r *DigitalRain) drawTitles() {
	y := float64(0)
	if r.linkover {
		y = r.drawTitle(shortLink(githubLink), githubLinkOverColor, 15*r.ratio, y)
	} else {
		y = r.drawTitle(shortLink(githubLink), githubLinkColor, 15*r.ratio, y)
	}
}

func (r *DigitalRain) loop(timestamp Duration) {
	if timestamp == 0 || r.timestamp == 0 {
		r.timestamp = timestamp
		return
	}
	if r.canvas == nil || r.ctx == nil || r.width <= 0 || r.height <= 0 {
		// No usable canvas (e.g. the initial layout failed because the
		// parent had zero size). Keep the clock moving so that when a
		// canvas does appear, drops advance by real elapsed time only.
		r.timestamp = timestamp
		return
	}
	elapsed := timestamp - r.timestamp
	if elapsed < 0 {
		elapsed = 0
	}
	r.timestamp = timestamp

	r.dropRandomWaterDrop(timestamp)

	// Advance drops by the real elapsed time. A canvas rebuild never
	// touches r.timestamp, so a rebuild happening between two frames
	// can neither double-count nor lose any time span.
	var drops []*waterDrop
	for _, drop := range r.drops {
		var ri = randi()
		if !drop.spedup {
			if ri%250 == 0 {
				drop.speed *= float64(ri%3) + 0.8
				drop.spedup = true
			}
		}
		drop.row += float64(elapsed) / float64(Second) * drop.speed
		if int(drop.row)-len(drop.glyphs) > r.maxRows() {
			continue
		}
		drops = append(drops, drop)
	}
	r.drops = drops

	r.render()
}

// render draws the current drop state without advancing time, spawning
// drops, or otherwise mutating drop positions. It is called once per
// animation frame from loop, and directly after a canvas rebuild so the
// new canvas shows the exact same state the old one did.
func (r *DigitalRain) render() {
	if r.ctx == nil {
		return
	}
	r.ctx.Call("clearRect", 0, 0, r.width, r.height)
	defer r.drawTitles()

	for _, drop := range r.drops {
		gbrightness := r.brightness
		age := (r.timestamp - drop.created)
		if age < Second {
			gbrightness = float64(age / Second)
		}
		gcount := int(drop.row - drop.start)
		if gcount > len(drop.glyphs) {
			gcount = len(drop.glyphs)
		}
		for i := 0; i < gcount; i++ {
			var ri = randi()
			glyph := drop.glyphs[i]
			brightness := 1 - (float64(i) / float64(gcount))
			// one in N change that the glyph will change, just because
			if ri%50 == 0 {
				glyph = ri % glyphsCount
				drop.glyphs[i] = glyph
			}
			row := drop.row - float64(i)
			r.drawGlyphAt(glyph, drop.col, row, brightness*gbrightness, i == 0)
		}
	}
}

const (
	glyphs        = "02345789ABCEGIJMNPRVXYZ:>+*~｡､イエカクコシセタツトニハフホミメヤラハヒルرعلحودסצשאיดฟวㅏㅓㅗㅜ-ㅣŁ"
	glyphsCols    = 18
	glyphsCount   = 72
	glyphCellSize = 100
	glyphFontSize = 86
)

func NewGlyphCanvas(color string) *GlyphCanvas {
	glyphCanvas := &GlyphCanvas{
		jso: js.Global.Get("document").Call("createElement", "canvas"),
	}
	glyphCanvas.jso.Set("width", int(glyphCellSize*glyphsCols+glyphCellSize))
	glyphCanvas.jso.Set("height", int(glyphCellSize*glyphsCount/glyphsCols+glyphCellSize))
	ctx := glyphCanvas.jso.Call("getContext", "2d")
	col := 0
	row := 1
	for i, c := range glyphs {
		if col == 18 {
			row++
			col = 0
		}
		cellSize := float64(glyphCellSize)
		fontSize := float64(glyphFontSize)
		if i <= 36 {
			fontSize *= .87
		}
		ctx.Call("save")
		ctx.Set("textAlign", "center")
		ctx.Set("font", itoa(int(fontSize))+"px Monaco, Helvetica, Arial, Sans-Serif")
		ctx.Set("shadowColor", "rgba(255,255,255,0.1)")
		ctx.Set("shadowBlur", float64(fontSize)*.50)
		switch c {
		default:
			ctx.Call("translate", cellSize*float64(col)+cellSize/2, cellSize*float64(row)+(fontSize-cellSize))
		case '2', '4', '9':
			ctx.Call("scale", -1, 1)
			ctx.Call("translate", -(cellSize*float64(col) + cellSize/2), cellSize*float64(row)+(fontSize-cellSize))
		}
		for i := 0; i < 3; i++ {
			ctx.Set("fillStyle", color)
			ctx.Call("fillText", string(c), 0, 0)
		}
		ctx.Call("restore")
		col++
	}

	return glyphCanvas
}

type GlyphCanvas struct {
	jso    *js.Object
	glyphs map[int]map[int]*js.Object
}

func (gc *GlyphCanvas) Glyph(gx int, gy int) *js.Object {
	if gc.glyphs == nil {
		gc.glyphs = make(map[int]map[int]*js.Object)
	}
	mx := gc.glyphs[gx]
	if mx == nil {
		mx = make(map[int]*js.Object)
		gc.glyphs[gx] = mx
	}
	my := mx[gy]
	if my == nil {
		my = js.Global.Get("document").Call("createElement", "canvas")
		my.Set("width", glyphCellSize)
		my.Set("height", glyphCellSize)
		ctx := my.Call("getContext", "2d")
		ctx.Call("drawImage", gc.jso, gx, gy, glyphCellSize, glyphCellSize, 0, 0, glyphCellSize, glyphCellSize)
		mx[gy] = my
	}
	return my
}
