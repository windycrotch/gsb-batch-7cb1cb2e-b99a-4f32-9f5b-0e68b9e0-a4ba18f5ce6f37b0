package gridder

import (
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"sync"

	"github.com/fogleman/gg"
	"golang.org/x/image/font"
)

var (
	errNoRows      = errors.New("no rows provided")
	errNoColumns   = errors.New("no columns provided")
	errOutOfBounds = errors.New("out of bounds")
)

// New creates a new gridder and sets it up with its configuration
func New(imageConfig ImageConfig, gridConfig GridConfig) (*Gridder, error) {
	rows := gridConfig.GetRows()
	if rows == 0 {
		return nil, errNoRows
	}

	columns := gridConfig.GetColumns()
	if columns == 0 {
		return nil, errNoColumns
	}

	gridder := Gridder{
		imageConfig: imageConfig,
		gridConfig:  gridConfig,
		ctx:         gg.NewContext(imageConfig.GetWidth(), imageConfig.GetHeight()),
	}
	gridder.paintBackground()
	return &gridder, nil
}

// Gridder gridder structure
//
// The mutex guards the underlying drawing context. All drawing operations and
// exports hold it for the whole time they touch the canvas, so concurrent
// callers never observe a half-drawn state and cannot trigger a data race.
type Gridder struct {
	mu          sync.Mutex
	imageConfig ImageConfig
	gridConfig  GridConfig
	ctx         *gg.Context
}

// SavePNG saves to PNG
//
// The grid lines and border are painted onto a throwaway snapshot of the
// canvas, never onto the canvas itself, so exporting any number of times
// always yields the same pixels and never thickens the lines or shifts the
// dash phase.
func (g *Gridder) SavePNG() error {
	image := g.renderImage()
	return gg.SavePNG(g.imageConfig.GetName(), image)
}

// EncodePNG encodes the image as a PNG and writes it to the provided io.Writer.
//
// As with SavePNG, the grid lines and border are composited onto a snapshot,
// so the canvas is left untouched. A failing writer cannot pollute the canvas:
// the snapshot is built before anything is written, and a later export retries
// from the very same canvas state.
func (g *Gridder) EncodePNG(w io.Writer) error {
	image := g.renderImage()
	return png.Encode(w, image)
}

// renderImage produces a complete, consistent snapshot of the canvas with the
// grid lines and border composited on top. The caller-visible canvas is never
// modified, which makes repeated, interleaved and failing exports idempotent.
func (g *Gridder) renderImage() image.Image {
	g.mu.Lock()
	defer g.mu.Unlock()

	// NewContextForImage copies the pixels into a fresh image.RGBA, so the
	// grid and border strokes below cannot leak back into the canvas.
	exportCtx := gg.NewContextForImage(g.ctx.Image())
	margin := float64(g.gridConfig.GetMarginWidth())
	exportCtx.Translate(margin, margin)
	g.paintGrid(exportCtx)
	g.paintBorder(exportCtx)
	return exportCtx.Image()
}

// PaintCell paints Cell
func (g *Gridder) PaintCell(row int, column int, c color.Color) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.paintCell(row, column, c)
}

func (g *Gridder) paintCell(row int, column int, c color.Color) error {
	err := g.verifyInBounds(row, column)
	if err != nil {
		return err
	}

	cellWidth, cellHeight := g.getCellDimensions()
	paintWidth := cellWidth - g.gridConfig.GetLineStrokeWidth()
	paintHeight := cellHeight - g.gridConfig.GetLineStrokeWidth()
	return g.drawRectangle(row, column, RectangleConfig{Width: paintWidth, Height: paintHeight, Color: c})
}

// DrawRectangle draws a rectangle in a cell
func (g *Gridder) DrawRectangle(row int, column int, rectangleConfigs ...RectangleConfig) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.drawRectangle(row, column, rectangleConfigs...)
}

func (g *Gridder) drawRectangle(row int, column int, rectangleConfigs ...RectangleConfig) error {
	err := g.verifyInBounds(row, column)
	if err != nil {
		return err
	}

	center := g.getCellCenter(row, column)
	rectangleConfig := getFirstRectangleConfig(rectangleConfigs...)
	rectangleWidth := rectangleConfig.GetWidth()
	rectangleHeight := rectangleConfig.GetHeight()

	x := center.X - rectangleWidth/2
	y := center.Y - rectangleHeight/2

	g.ctx.Push()
	dashes := rectangleConfig.GetDashes()
	if dashes > 0 {
		g.ctx.SetDash(dashes)
	} else {
		g.ctx.SetDash()
	}
	g.ctx.RotateAbout(gg.Radians(rectangleConfig.GetRotate()), center.X, center.Y)
	g.ctx.DrawRectangle(x, y, rectangleWidth, rectangleHeight)
	g.ctx.SetLineWidth(rectangleConfig.GetStrokeWidth())
	g.ctx.SetColor(rectangleConfig.GetColor())
	if rectangleConfig.IsStroke() {
		g.ctx.Stroke()
	} else {
		g.ctx.Fill()
	}
	g.ctx.Pop()
	return nil
}

// DrawCircle draws a circle in a cell
func (g *Gridder) DrawCircle(row int, column int, circleConfigs ...CircleConfig) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.drawCircle(row, column, circleConfigs...)
}

func (g *Gridder) drawCircle(row int, column int, circleConfigs ...CircleConfig) error {
	err := g.verifyInBounds(row, column)
	if err != nil {
		return err
	}

	center := g.getCellCenter(row, column)
	circleConfig := getFirstCircleConfig(circleConfigs...)

	g.ctx.Push()
	dashes := circleConfig.GetDashes()
	if dashes > 0 {
		g.ctx.SetDash(dashes)
	} else {
		g.ctx.SetDash()
	}
	g.ctx.DrawPoint(center.X, center.Y, circleConfig.GetRadius())
	g.ctx.SetLineWidth(circleConfig.GetStrokeWidth())
	g.ctx.SetColor(circleConfig.GetColor())
	if circleConfig.IsStroke() {
		g.ctx.Stroke()
	} else {
		g.ctx.Fill()
	}
	g.ctx.Pop()
	return nil
}

// DrawPath draws a path between two cells
func (g *Gridder) DrawPath(row1 int, column1 int, row2 int, column2 int, pathConfigs ...PathConfig) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.drawPath(row1, column1, row2, column2, pathConfigs...)
}

func (g *Gridder) drawPath(row1 int, column1 int, row2 int, column2 int, pathConfigs ...PathConfig) error {
	err := g.verifyInBounds(row1, column1)
	if err != nil {
		return err
	}

	err = g.verifyInBounds(row2, column2)
	if err != nil {
		return err
	}

	center1 := g.getCellCenter(row1, column1)
	center2 := g.getCellCenter(row2, column2)
	pathConfig := getFirstPathConfig(pathConfigs...)

	g.ctx.Push()
	dashes := pathConfig.GetDashes()
	if dashes > 0 {
		g.ctx.SetDash(dashes)
	} else {
		g.ctx.SetDash()
	}
	g.ctx.SetColor(pathConfig.GetColor())
	g.ctx.SetLineWidth(pathConfig.GetStrokeWidth())
	g.ctx.DrawLine(center1.X, center1.Y, center2.X, center2.Y)
	g.ctx.Stroke()
	g.ctx.Pop()
	return nil
}

// DrawLine draws a line in a cell
func (g *Gridder) DrawLine(row int, column int, lineConfigs ...LineConfig) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.drawLine(row, column, lineConfigs...)
}

func (g *Gridder) drawLine(row int, column int, lineConfigs ...LineConfig) error {
	err := g.verifyInBounds(row, column)
	if err != nil {
		return err
	}

	center := g.getCellCenter(row, column)
	lineConfig := getFirstLineConfig(lineConfigs...)
	length := lineConfig.GetLength()

	x1 := center.X - length/2
	x2 := center.X + length/2
	y := center.Y

	g.ctx.Push()
	dashes := lineConfig.GetDashes()
	if dashes > 0 {
		g.ctx.SetDash(dashes)
	} else {
		g.ctx.SetDash()
	}
	g.ctx.RotateAbout(gg.Radians(lineConfig.GetRotate()), center.X, center.Y)
	g.ctx.DrawLine(x1, y, x2, y)
	g.ctx.SetLineWidth(lineConfig.GetStrokeWidth())
	g.ctx.SetColor(lineConfig.GetColor())
	g.ctx.Stroke()
	g.ctx.Pop()
	return nil
}

// DrawString draws a string in a cell
func (g *Gridder) DrawString(row int, column int, text string, fontFace font.Face, stringConfigs ...StringConfig) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.drawString(row, column, text, fontFace, stringConfigs...)
}

func (g *Gridder) drawString(row int, column int, text string, fontFace font.Face, stringConfigs ...StringConfig) error {
	err := g.verifyInBounds(row, column)
	if err != nil {
		return err
	}

	center := g.getCellCenter(row, column)
	stringConfig := getFirstStringConfig(stringConfigs...)
	g.ctx.Push()
	g.ctx.SetFontFace(fontFace)
	g.ctx.SetColor(stringConfig.GetColor())
	g.ctx.RotateAbout(gg.Radians(stringConfig.GetRotate()), center.X, center.Y)
	g.ctx.DrawStringAnchored(text, center.X, center.Y, 0.5, 0.35)
	g.ctx.Pop()
	return nil
}

func (g *Gridder) paintBackground() {
	margin := float64(g.gridConfig.GetMarginWidth())
	g.ctx.Translate(margin, margin)
	g.ctx.SetColor(g.gridConfig.GetBackgroundColor())
	g.ctx.Clear()
}

func (g *Gridder) paintGrid(ctx *gg.Context) {
	canvasWidth, canvasHeight := g.getGridDimensions()
	cellWidth, cellHeight := g.getCellDimensions()

	columns := float64(g.gridConfig.GetColumns())

	ctx.Push()
	for i := 1.0; i < columns; i++ {
		x := i * cellWidth
		ctx.MoveTo(x, 0)
		ctx.LineTo(x, canvasHeight)
	}

	rows := float64(g.gridConfig.GetRows())
	for i := 1.0; i < rows; i++ {
		y := i * cellHeight
		ctx.MoveTo(0, y)
		ctx.LineTo(canvasWidth, y)
	}

	dashes := g.gridConfig.GetLineDashes()
	if dashes > 0 {
		ctx.SetDash(dashes)
	} else {
		ctx.SetDash()
	}
	ctx.SetColor(g.gridConfig.GetLineColor())
	ctx.SetLineWidth(g.gridConfig.GetLineStrokeWidth())
	ctx.Stroke()
	ctx.Pop()
}

func (g *Gridder) paintBorder(ctx *gg.Context) {
	canvasWidth, canvasHeight := g.getGridDimensions()
	cellWidth, cellHeight := g.getCellDimensions()

	columns := float64(g.gridConfig.GetColumns())
	ctx.Push()
	ctx.MoveTo(0, 0)
	ctx.LineTo(0, canvasHeight)
	ctx.MoveTo(cellWidth*columns, 0)
	ctx.LineTo(cellWidth*columns, canvasHeight)

	rows := float64(g.gridConfig.GetRows())
	ctx.MoveTo(0, 0)
	ctx.LineTo(canvasWidth, 0)
	ctx.MoveTo(0, cellHeight*rows)
	ctx.LineTo(canvasWidth, cellHeight*rows)

	dashes := g.gridConfig.GetBorderDashes()
	if dashes > 0 {
		ctx.SetDash(dashes)
	} else {
		ctx.SetDash()
	}
	ctx.SetLineWidth(g.gridConfig.GetBorderStrokeWidth())
	ctx.SetColor(g.gridConfig.GetBorderColor())
	ctx.Stroke()
	ctx.Pop()
}

func (g *Gridder) getCellDimensions() (float64, float64) {
	gridWidth, gridHeight := g.getGridDimensions()
	cellWidth := gridWidth / float64(g.gridConfig.GetColumns())
	cellHeight := gridHeight / float64(g.gridConfig.GetRows())
	return cellWidth, cellHeight
}

func (g *Gridder) getGridDimensions() (float64, float64) {
	imageWidth := g.imageConfig.GetWidth()
	imageHeight := g.imageConfig.GetHeight()

	gridWidth := float64(g.gridConfig.GetWidth(imageWidth))
	gridHeight := float64(g.gridConfig.GetHeight(imageHeight))
	return gridWidth, gridHeight
}

func (g *Gridder) getCellCenter(row, column int) *gg.Point {
	columns := float64(g.gridConfig.GetColumns())
	rows := float64(g.gridConfig.GetRows())

	cellWidth, cellHeight := g.getCellDimensions()
	gridWidth, gridHeight := g.getGridDimensions()

	x := float64(column)*(gridWidth/columns) + cellWidth/2
	y := float64(row)*(gridHeight/rows) + cellHeight/2
	return &gg.Point{X: x, Y: y}
}

func (g *Gridder) verifyInBounds(row, column int) error {
	columns := g.gridConfig.GetColumns()
	rows := g.gridConfig.GetRows()
	if row < 0 || row >= rows || column < 0 || column >= columns {
		return errOutOfBounds
	}
	return nil
}
