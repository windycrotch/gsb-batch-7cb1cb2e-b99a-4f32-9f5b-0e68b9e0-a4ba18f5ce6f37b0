package particle

import (
	"math"

	"github.com/quasilyte/ebitengine-graphics/internal/cache"
	"github.com/quasilyte/gmath"
)

type Emitter struct {
	tmpl *Template

	particles []particle

	Pos gmath.Pos

	PivotOffset gmath.Vec

	Rotation *gmath.Rad

	// dtError is a fractional part that accumulates between the updates.
	// When it becomes 1.0, it will be consumed to correct the s->ms conversion
	// we do (the error happens due to the uint16 truncation).
	// It helps to avoid the infinitely growing error.
	dtError float64

	lifetimeMultiplier float32

	// emitDelay is a time (in seconds) until the next emission step.
	emitDelay float32

	idSeq      uint32
	generation uint16

	// renderer is the renderer this emitter currently belongs to.
	// An emitter can be registered with at most one renderer at a time.
	// It's used to keep AddEmitter idempotent so a re-added emitter
	// never gets drawn twice or leaves stale batch entries.
	renderer *Renderer

	emitting bool
	visible  bool
	disposed bool
}

type particle struct {
	counter  uint16
	lifetime uint16

	scalingSeed  uint8
	speedSeed    uint8
	angleSeed    uint8
	origAngle    uint8
	paletteIndex uint8
	userData     uint8
	_            [2]uint8 // unused (reserved for future use)

	// Would use {uint16, uint16} here to save 4 bytes,
	// but it can be desirable to support negative coords
	// and values larger than MaxUint16.
	// The 32-bit float will grant us a precision area of about
	// 8_388_608 pixels (as opposed to 65_536 of uint16).
	// A world larger than that would make particles less accurate.
	origPos gmath.Vec32
}

func NewEmitter(tmpl *Template) *Emitter {
	e := &Emitter{
		tmpl:               tmpl,
		particles:          make([]particle, 0, 8),
		lifetimeMultiplier: 1,
		visible:            true,
	}
	return e
}

func (e *Emitter) IsDisposed() bool {
	return e.disposed
}

// Dispose marks the emitter as disposed.
//
// A disposed emitter is removed from its renderer buckets on the next
// Draw call and never emits or renders again. Disposal is permanent:
// re-adding the same disposed emitter to a renderer is a no-op.
// The emitter itself is not mutated otherwise, so its state remains
// queryable (for example, via [Emitter.NumParticles]).
func (e *Emitter) Dispose() {
	e.disposed = true
}

// Clear removes all live particles from the emitter without changing
// its emitting, visibility or disposal state.
//
// It's useful when an emitter has to be emptied immediately rather
// than waiting for all of its particles to reach the end of their
// lifetimes. Already rendered batches are not affected: the cleared
// particles simply stop being drawn on the next Draw call.
func (e *Emitter) Clear() {
	if e.disposed {
		return
	}
	e.particles = e.particles[:0]
}

// SetVisibility toggles the emitter visibility.
//
// A hidden emitter keeps updating normally: its particles age while
// hidden, so making it visible again never rolls their lifetimes back
// or double-counts them. It simply stops contributing to the drawn
// batches until it becomes visible again.
func (e *Emitter) SetVisibility(visible bool) { e.visible = visible }

func (e *Emitter) SetLifetimeMultiplier(multiplier float64) {
	e.lifetimeMultiplier = float32(multiplier)
}

func (e *Emitter) SetEmitting(emitting bool) {
	if e.emitting == emitting {
		return
	}

	e.emitDelay = 0
	e.emitting = emitting
}

func (e *Emitter) NumParticles() int {
	return len(e.particles)
}

func (e *Emitter) Update() {
	e.UpdateWithDelta(1.0 / 60.0)
}

func (e *Emitter) UpdateWithDelta(delta float64) {
	// A disposed emitter is frozen: it neither emits new particles
	// nor ages the existing ones. Its state stays queryable until
	// the renderer drops it from its batches.
	if e.disposed {
		return
	}

	if e.emitting {
		// With a very low emit interval, it's possible to have
		// delta > emitInterval.
		// If delta is 0.1 and emitInterval is 0.04, then we need
		// to emit twice and have the delay set to 0.02.
		// We should also create the second particles emitted
		// with t=0.04 instead of t=0.00.
		e.emitDelay -= float32(delta)
		t := float32(0.0)
		for e.emitDelay < 0 {
			e.emit(t)
			t += e.tmpl.emitInterval
			e.emitDelay += e.tmpl.emitInterval
		}
	}

	deltaMS := delta * 1000
	// This loop should execute either 0 or 1 time in most cases,
	// since error gets only a fractional part per tick.
	// But just to be safe, make it a loop.
	// Should still be more efficient than computing another modf.
	for e.dtError >= 1 {
		deltaMS++
		e.dtError--
	}
	_, fract := math.Modf(deltaMS)
	e.dtError += fract

	live := e.particles[:0]
	dt := uint16(deltaMS)
	for _, p := range e.particles {
		p.counter += dt
		if p.counter > p.lifetime {
			continue
		}
		live = append(live, p)
	}
	e.particles = live
}

func (e *Emitter) emit(t float32) {
	tmpl := e.tmpl
	e.generation++

	w, h := float64(e.tmpl.img.Bounds().Dx()), float64(e.tmpl.img.Bounds().Dy())
	pos := e.Pos.Resolve().Sub(gmath.Vec{X: w * 0.5, Y: h * 0.5})
	if !e.PivotOffset.IsZero() {
		offset := rotatedVec(e.PivotOffset, e.Rotation)
		pos = pos.Add(offset)
	}

	// Compute up to 64 random bits only once.
	// Then use the fastrand to generate more.
	// If system doesn't need any rand bits, don't bother generating it.
	randBits := uint64(0)
	randSeq := uint64(0)
	if e.tmpl.needsRandBits != 0 {
		randBits = cache.Global.Rand.Uint64()
	}

	numParticles := 1
	if e.tmpl.emitBurstRangeSize != 0 {
		// Calculate the range using the 8 rand bits.
		x := uint16(fastrand(randBits, randSeq) & 0xff)
		randSeq++
		numParticles = int(e.tmpl.minEmitBurst + uint8(x*e.tmpl.emitBurstRangeSize/256))
	}

	ctx := SpawnContext{emitter: e}
	for i := 0; i < numParticles; i++ {
		ctx.id = e.idSeq
		e.idSeq++
		particlePos := pos
		ctx.userData = 0
		if tmpl.spawnUserDataFunc != nil {
			ctx.userData = tmpl.spawnUserDataFunc(ctx)
		}
		if tmpl.spawnOffsetFunc != nil {
			offset := rotatedVec(tmpl.spawnOffsetFunc(ctx), e.Rotation)
			particlePos = particlePos.Add(offset)
		}

		paletteIndex := uint8(0)
		if tmpl.spawnColorFunc != nil {
			paletteIndex = uint8(tmpl.spawnColorFunc(ctx))
		}

		lifetime := uint16(0)
		{
			minLifetime := e.tmpl.particleMinLifetime
			v := minLifetime
			if e.tmpl.needsRandBits&lifetimeRandBit != 0 {
				roll := float32(fastrandFloat(randBits, randSeq))
				maxLifetime := e.tmpl.particleMaxLifetime
				v = minLifetime + roll*(maxLifetime-minLifetime)
				randSeq++
			}
			v *= e.lifetimeMultiplier
			lifetime = uint16(v * 1000)
		}

		scalingSeed := uint8(0)
		if e.tmpl.needsRandBits&scalingRandBit != 0 {
			scalingSeed = uint8(fastrand(randBits, randSeq))
			randSeq++
		}

		origAngle := uint8(0)
		if e.Rotation != nil {
			origAngle = uint8((e.Rotation.Normalized() / (2 * math.Pi)) * 256)
		}

		p := particle{
			counter:      uint16(t * 1000),
			lifetime:     lifetime,
			paletteIndex: paletteIndex,
			scalingSeed:  scalingSeed,
			origAngle:    origAngle,
			userData:     ctx.userData,
			origPos:      particlePos.AsVec32(),
		}

		if e.tmpl.needsRandBits&speedRandBit != 0 {
			x := uint8(fastrand(randBits, randSeq))
			randSeq++
			p.speedSeed = x
		}

		if e.tmpl.needsRandBits&angleRandBit != 0 {
			x := uint8(fastrand(randBits, randSeq))
			randSeq++
			p.angleSeed = x
		}

		e.particles = append(e.particles, p)
	}
}
