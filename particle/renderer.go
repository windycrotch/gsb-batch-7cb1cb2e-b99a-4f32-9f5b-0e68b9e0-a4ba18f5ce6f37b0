package particle

import (
	"math"

	"github.com/hajimehoshi/ebiten/v2"
	graphics "github.com/quasilyte/ebitengine-graphics"
	"github.com/quasilyte/ebitengine-graphics/internal/cache"
	"github.com/quasilyte/ebitengine-graphics/internal/xmath"
	"github.com/quasilyte/gmath"
)

// Renderer implements batch particles rendering.
//
// It can be used as a layer-like object that renders all
// of its emitters on a single layer.
//
// [Renderer] is intended to be a part of Ebitengine Draw tree.
// You add [Emitter] to the [Renderer] in order for it to be drawn.
//
// The order in which particles are drawn is unspecified,
// but stable: this order is consistent between the frames.
// If different layers are needed, several renderers should be used.
type Renderer struct {
	bucketIDByImage map[*ebiten.Image]int
	bucketList      []*rendererBucket
	freeList        []*rendererBucket
	disposed        bool
}

type rendererBucket struct {
	img      *ebiten.Image
	id       int
	emitters []*Emitter
}

// drawTriangles is a thin wrapper over [ebiten.Image.DrawTriangles].
// It's a package-level variable so tests can replace it with a spy
// that counts batch draws without needing a running game.
var drawTriangles = func(dst *ebiten.Image, vertices []ebiten.Vertex, indices []uint16, img *ebiten.Image, opts *ebiten.DrawTrianglesOptions) {
	dst.DrawTriangles(vertices, indices, img, opts)
}

func NewRenderer() *Renderer {
	return &Renderer{
		bucketIDByImage: make(map[*ebiten.Image]int, 8),
		bucketList:      make([]*rendererBucket, 0, 8),
		freeList:        make([]*rendererBucket, 0, 2),
	}
}

func (r *Renderer) IsDisposed() bool {
	return r.disposed
}

// Dispose marks the renderer to be removed from the scene.
// It does not dispose any of its associated emitters.
// This is mostly relevant if you're using some scene framework.
//
// After disposal the renderer stops drawing any of its batches,
// but the registered emitters are left untouched and their state
// (particle counts, visibility, disposal flags) stays queryable.
func (r *Renderer) Dispose() {
	r.disposed = true
}

// AddEmitter registers e with the renderer so its particles get drawn
// in the batch keyed by the emitter's template image.
//
// AddEmitter is idempotent for live emitters: adding an emitter that
// already belongs to this renderer is a no-op, so a re-added emitter
// can never be drawn twice or leave a stale duplicate in a batch.
//
// If e already belongs to another renderer, it's removed from that
// renderer's batch first: an emitter is always registered with at
// most one renderer at a time.
//
// Adding a disposed emitter is a no-op as well; disposal is permanent,
// so a fresh emitter has to be created instead of reusing the old one.
// Adding emitters to a disposed renderer is also a no-op.
func (r *Renderer) AddEmitter(e *Emitter) {
	if e.IsDisposed() || r.disposed {
		return
	}
	if e.renderer == r {
		// Already registered with this renderer.
		return
	}
	if e.renderer != nil {
		// Move the emitter out of its previous renderer's batch.
		e.renderer.removeEmitter(e)
	}

	bucketID, ok := r.bucketIDByImage[e.tmpl.img]
	var bucket *rendererBucket

	if ok {
		// Found an active bucket match.
		bucket = r.bucketList[bucketID]
	} else {
		var id int
		if len(r.freeList) > 0 {
			// Re-use a previously retired bucket.
			bucket = r.freeList[len(r.freeList)-1]
			r.freeList = r.freeList[:len(r.freeList)-1]
			id = bucket.id
			bucket.img = e.tmpl.img
		} else {
			// Allocate a new bucket.
			id = len(r.bucketList)
			bucket = &rendererBucket{
				id:       id,
				img:      e.tmpl.img,
				emitters: make([]*Emitter, 0, 8),
			}
			r.bucketList = append(r.bucketList, bucket)
		}
		r.bucketIDByImage[e.tmpl.img] = id
	}

	e.renderer = r
	bucket.emitters = append(bucket.emitters, e)
}

// removeEmitter drops e from its bucket in this renderer without
// retiring the bucket; the bucket cleanup happens on the next Draw.
func (r *Renderer) removeEmitter(e *Emitter) {
	bucketID, ok := r.bucketIDByImage[e.tmpl.img]
	if !ok {
		return
	}
	bucket := r.bucketList[bucketID]
	emitters := bucket.emitters[:0]
	for _, member := range bucket.emitters {
		if member != e {
			emitters = append(emitters, member)
		}
	}
	bucket.emitters = emitters
}

func (r *Renderer) Draw(dst *ebiten.Image) {
	r.DrawWithOptions(dst, graphics.DrawOptions{})
}

func (r *Renderer) DrawWithOptions(dst *ebiten.Image, opts graphics.DrawOptions) {
	if r.disposed {
		// A disposed renderer keeps its emitters and buckets intact
		// (their state must stay queryable), it just never draws.
		return
	}

	for _, bucket := range r.bucketList {
		if bucket.img == nil {
			continue // This bucket was retired, it's in the free list
		}
		activeEmitters, removedEmitters := r.drawBucket(dst, bucket.img, opts, bucket.emitters)
		for _, e := range removedEmitters {
			// These emitters were disposed and dropped from the batch;
			// clear the ownership link so a fresh state can be tracked.
			if e.renderer == r {
				e.renderer = nil
			}
		}
		if len(activeEmitters) == 0 {
			delete(r.bucketIDByImage, bucket.img)
			bucket.emitters = bucket.emitters[:0]
			bucket.img = nil
			r.freeList = append(r.freeList, bucket)
			continue
		}
		bucket.emitters = activeEmitters
	}
}

func (r *Renderer) drawBucket(dst *ebiten.Image, img *ebiten.Image, opts graphics.DrawOptions, emitters []*Emitter) (active []*Emitter, removed []*Emitter) {
	const batchThreshold = math.MaxUint16 / 24 // Doesn't have to be bigger
	batchParticles := 0

	batch := sharedResources.batchSlice[:0]
	defer func() {
		sharedResources.batchSlice = batch[:0]
	}()

	activeEmitters := emitters[:0]

	for _, e := range emitters {
		if e.IsDisposed() {
			removed = append(removed, e)
			continue
		}
		activeEmitters = append(activeEmitters, e)

		if !e.visible {
			continue
		}
		n := e.NumParticles()
		if n == 0 {
			continue
		}

		if batchParticles+n > batchThreshold {
			r.drawBatch(dst, img, opts, batch)
			batch = batch[:0]
			batchParticles = 0
		}

		// If this emitter caused a batch flush above,
		// this append is still needed and will be rendered
		// as a part of a pre-return flush (see below).
		batch = append(batch, e)
		batchParticles += n
	}

	if len(batch) != 0 {
		r.drawBatch(dst, img, opts, batch)
	}

	return activeEmitters, removed
}

func (r *Renderer) drawBatch(dst, img *ebiten.Image, opts graphics.DrawOptions, emitters []*Emitter) {
	// Use pre-allocated slices.
	vertices := cache.Global.ScratchVertices[:0]
	indices := cache.Global.ScratchIndices[:0]
	defer func() {
		cache.Global.ScratchVertices = vertices[:0]
		cache.Global.ScratchIndices = indices[:0]
	}()

	idx := uint16(0)
	offset32 := opts.Offset.AsVec32()

	for _, e := range emitters {
		tmpl := e.tmpl

		w, h := float32(tmpl.img.Bounds().Dx()), float32(tmpl.img.Bounds().Dy())
		halfWidth := w * 0.5
		halfHeight := h * 0.5
		palette := tmpl.palette

		needScaling := tmpl.needsRandBits&scalingRandBit != 0 ||
			tmpl.particleMinScaling.X != 1 ||
			tmpl.particleMinScaling.Y != 1
		needAngle := tmpl.particleMinAngle != 0 ||
			tmpl.particleMaxAngle != 0 ||
			e.Rotation != nil

		updateColorScaleFunc := tmpl.updateColorScaleFunc
		updateScalingFunc := tmpl.updateScalingFunc

		ctx := UpdateContext{emitter: e}
		minSpeed := tmpl.particleMinSpeed
		speedStep := tmpl.particleSpeedStep
		minScaling := tmpl.particleMinScaling
		scalingStep := tmpl.particleScalingStep
		for _, p := range e.particles {
			var pos xmath.Geom32
			var angle float64
			{
				origPos := p.origPos
				fcounter := float32(p.counter)
				progress := fcounter / float32(p.lifetime)
				ctx.t = progress
				ctx.userData = p.userData

				dir := gmath.Vec32{X: 1, Y: 0}
				if needAngle {
					angle = tmpl.particleMinAngle + tmpl.particleAngleStep*float64(p.angleSeed)
					angle += float64(p.origAngle) * ((2 * math.Pi) / 255)
					dir = dir.Rotated(gmath.Rad(angle))
				}

				speed := minSpeed + (speedStep * float32(p.speedSeed))
				currentPos := origPos.Add(dir.Mulf(speed).Mulf(fcounter * 0.001))

				scaling := gmath.Vec32{X: 1, Y: 1}
				if needScaling {
					scaling = minScaling.Add(scalingStep.Mulf(float32(p.scalingSeed)))
				}
				if updateScalingFunc != nil {
					scaling = scaling.Mul(updateScalingFunc(ctx))
				}

				pos.Translate(-halfWidth, -halfHeight)
				if scaling.X != 1 || scaling.Y != 1 {
					pos.Scale(scaling.X, scaling.Y)
				}
				if angle != 0 {
					pos.Rotate(angle)
				}
				pos.Translate(halfWidth, halfHeight)
				pos.Translate(offset32.X+currentPos.X, offset32.Y+currentPos.Y)
			}

			clr := palette[p.paletteIndex]
			if updateColorScaleFunc != nil {
				clr = clr.Mul(updateColorScaleFunc(ctx))
			}

			x := pos.Tx
			y := pos.Ty
			vertices = append(vertices,
				ebiten.Vertex{DstX: x, DstY: y, SrcX: 0, SrcY: 0, ColorR: clr.R, ColorG: clr.G, ColorB: clr.B, ColorA: clr.A},
				ebiten.Vertex{DstX: (pos.A1+1)*w + x, DstY: pos.C*w + y, SrcX: w, SrcY: 0, ColorR: clr.R, ColorG: clr.G, ColorB: clr.B, ColorA: clr.A},
				ebiten.Vertex{DstX: pos.B*h + x, DstY: (pos.D1+1)*h + y, SrcX: 0, SrcY: h, ColorR: clr.R, ColorG: clr.G, ColorB: clr.B, ColorA: clr.A},
				ebiten.Vertex{DstX: pos.ApplyX(w, h), DstY: pos.ApplyY(w, h), SrcX: w, SrcY: h, ColorR: clr.R, ColorG: clr.G, ColorB: clr.B, ColorA: clr.A},
			)

			indices = append(indices, idx, idx+1, idx+2, idx+1, idx+2, idx+3)
			idx += 4
		}
	}

	var drawOptions ebiten.DrawTrianglesOptions
	drawTriangles(dst, vertices, indices, img, &drawOptions)
}
