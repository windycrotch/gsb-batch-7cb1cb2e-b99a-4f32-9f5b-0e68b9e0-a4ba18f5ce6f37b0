package particle

import (
	"image/color"
	"testing"

	"github.com/hajimehoshi/ebiten/v2"
)

// This file uses a locally created small texture plus locally built
// templates to exercise the emitter <-> renderer lifecycle contracts:
//
//   - stopping emission only blocks new particles, old ones drain
//   - hidden emitters keep aging and restore without lifetime rollback
//   - disposed emitters are fully removed and re-adding them is safe
//   - shared buckets are isolated between emitters and get recycled
//   - disposed renderers stop drawing while emitter state stays queryable
//
// Pixel readback is unavailable outside of a running ebiten game,
// so the DrawTriangles call is spied on through drawTriangles to count
// the batches and the particles that were actually submitted for draw.

type drawCall struct {
	img       *ebiten.Image
	particles int
}

type drawSpy struct {
	calls []drawCall
}

func installDrawSpy(t *testing.T) *drawSpy {
	t.Helper()
	spy := &drawSpy{}
	old := drawTriangles
	drawTriangles = func(dst *ebiten.Image, vertices []ebiten.Vertex, indices []uint16, img *ebiten.Image, opts *ebiten.DrawTrianglesOptions) {
		spy.calls = append(spy.calls, drawCall{
			img:       img,
			particles: len(vertices) / 4,
		})
	}
	t.Cleanup(func() { drawTriangles = old })
	return spy
}

func (s *drawSpy) totalParticles() int {
	total := 0
	for _, c := range s.calls {
		total += c.particles
	}
	return total
}

func (s *drawSpy) reset() {
	s.calls = s.calls[:0]
}

// newTestImage creates a small self-made texture entirely in-process.
func newTestImage(size int) *ebiten.Image {
	img := ebiten.NewImage(size, size)
	img.Fill(color.RGBA{R: 255, G: 128, B: 64, A: 255})
	return img
}

func newTestTemplate(img *ebiten.Image, emitInterval, lifetime float64) *Template {
	tmpl := NewTemplate()
	tmpl.SetImage(img)
	tmpl.SetEmitInterval(emitInterval)
	tmpl.SetParticleLifetime(lifetime)
	tmpl.SetEmitBurst(1, 1)
	tmpl.SetParticleSpeed(0)
	return tmpl
}

// emitParticles turns the emitter on, runs exactly n update ticks of
// emitInterval seconds each (one particle per tick) and stops it again.
func emitParticles(e *Emitter, n int, emitInterval float64) {
	e.SetEmitting(true)
	for i := 0; i < n; i++ {
		e.UpdateWithDelta(emitInterval)
	}
	e.SetEmitting(false)
}

func bucketFor(r *Renderer, img *ebiten.Image) *rendererBucket {
	id, ok := r.bucketIDByImage[img]
	if !ok {
		return nil
	}
	return r.bucketList[id]
}

func bucketMembershipCount(b *rendererBucket, e *Emitter) int {
	count := 0
	for _, member := range b.emitters {
		if member == e {
			count++
		}
	}
	return count
}

func newDstImage() *ebiten.Image {
	return ebiten.NewImage(64, 64)
}

// Stopping emission must not append new particles, while the already
// spawned ones keep aging and drain naturally down to zero. The drawn
// particle count must follow the live count the whole time.
func TestEmitterStopEmittingDrains(t *testing.T) {
	img := newTestImage(4)
	tmpl := newTestTemplate(img, 0.1, 0.3)
	r := NewRenderer()
	dst := newDstImage()
	spy := installDrawSpy(t)

	e := NewEmitter(tmpl)
	r.AddEmitter(e)

	emitParticles(e, 3, 0.1)
	if got := e.NumParticles(); got != 3 {
		t.Fatalf("after 3 emit ticks: NumParticles=%d, want 3", got)
	}

	// Existing particles are still drawn immediately after stopping.
	r.Draw(dst)
	if got := spy.totalParticles(); got != 3 {
		t.Fatalf("draw right after stop submitted %d particles, want 3", got)
	}
	spy.reset()

	stoppedAt := e.NumParticles()
	for tick := 0; tick < 20; tick++ {
		e.UpdateWithDelta(0.1)

		// No new particles may be appended after emission stopped.
		if got := e.NumParticles(); got > stoppedAt {
			t.Fatalf("tick %d: particle count grew to %d after emission stopped", tick, got)
		}

		r.Draw(dst)
		// Everything still alive must be drawn exactly once.
		if got := spy.totalParticles(); got != e.NumParticles() {
			t.Fatalf("tick %d: drew %d particles but emitter has %d live", tick, got, e.NumParticles())
		}
		spy.reset()

		if e.NumParticles() == 0 {
			break
		}
	}

	if e.NumParticles() != 0 {
		t.Fatalf("particles never drained: %d remain", e.NumParticles())
	}

	// Nothing left to draw: no batch should be submitted at all.
	r.Draw(dst)
	if len(spy.calls) != 0 {
		t.Fatalf("drained emitter produced %d batch draws, want 0", len(spy.calls))
	}
}

// A hidden emitter must keep aging while invisible and come back with
// monotonically advanced lifetimes - no rollback, no double counting.
func TestEmitterHiddenAgesAndRestores(t *testing.T) {
	img := newTestImage(4)
	tmpl := newTestTemplate(img, 0.1, 10)
	r := NewRenderer()
	dst := newDstImage()
	spy := installDrawSpy(t)

	e := NewEmitter(tmpl)
	r.AddEmitter(e)
	emitParticles(e, 3, 0.1)

	before := make([]uint16, len(e.particles))
	for i, p := range e.particles {
		before[i] = p.counter
	}

	// While hidden the emitter stays in its bucket but draws nothing.
	e.SetVisibility(false)
	r.Draw(dst)
	if len(spy.calls) != 0 {
		t.Fatalf("hidden emitter produced %d batch draws, want 0", len(spy.calls))
	}
	b := bucketFor(r, img)
	if b == nil {
		t.Fatal("bucket was removed while emitter was only hidden")
	}
	if got := bucketMembershipCount(b, e); got != 1 {
		t.Fatalf("hidden emitter membership count=%d, want 1", got)
	}

	// Aging continues normally during two hidden update ticks.
	e.UpdateWithDelta(0.1)
	e.UpdateWithDelta(0.1)
	if got := e.NumParticles(); got != 3 {
		t.Fatalf("hidden update changed particle count to %d, want 3", got)
	}
	for i, p := range e.particles {
		want := before[i] + 200
		if p.counter != want {
			t.Fatalf("particle %d counter after hidden updates=%d, want %d (lifetime must keep advancing)", i, p.counter, want)
		}
		if p.counter < before[i] {
			t.Fatalf("particle %d lifetime rolled back after becoming visible again", i)
		}
	}

	// Becoming visible again draws each live particle exactly once.
	e.SetVisibility(true)
	r.Draw(dst)
	if got := spy.totalParticles(); got != e.NumParticles() {
		t.Fatalf("after restore drew %d particles, want %d", got, e.NumParticles())
	}
	if got := bucketMembershipCount(b, e); got != 1 {
		t.Fatalf("restored emitter membership count=%d, want 1", got)
	}
}

// A disposed emitter must disappear from its renderer bucket on the
// next draw, stay queryable afterwards, and never come back when
// re-added. A fresh emitter using the same image joins the same bucket.
func TestEmitterDisposeLifecycleAndReadd(t *testing.T) {
	img := newTestImage(4)
	tmpl1 := newTestTemplate(img, 0.1, 10)
	tmpl2 := newTestTemplate(img, 0.1, 10)
	r := NewRenderer()
	dst := newDstImage()
	spy := installDrawSpy(t)

	e1 := NewEmitter(tmpl1)
	e2 := NewEmitter(tmpl2)
	r.AddEmitter(e1)
	r.AddEmitter(e2)
	emitParticles(e1, 3, 0.1)
	emitParticles(e2, 2, 0.1)

	r.Draw(dst)
	if got := spy.totalParticles(); got != 5 {
		t.Fatalf("initial draw submitted %d particles, want 5", got)
	}
	spy.reset()

	// Disposing e1 removes it from the shared bucket; e2 is unaffected.
	e1.Dispose()
	if !e1.IsDisposed() {
		t.Fatal("IsDisposed()=false after Dispose()")
	}
	frozenCount := e1.NumParticles()
	frozenCounters := make([]uint16, len(e1.particles))
	for i, p := range e1.particles {
		frozenCounters[i] = p.counter
	}
	e1.UpdateWithDelta(0.1)
	if got := e1.NumParticles(); got != frozenCount {
		t.Fatalf("disposed emitter changed particle count %d -> %d, state must stay queryable/frozen", frozenCount, got)
	}
	for i, p := range e1.particles {
		if p.counter != frozenCounters[i] {
			t.Fatalf("disposed emitter particle %d counter advanced %d -> %d, aging must be frozen", i, frozenCounters[i], p.counter)
		}
	}

	r.Draw(dst)
	if got := spy.totalParticles(); got != 2 {
		t.Fatalf("after disposing e1 drew %d particles, want 2 (only e2)", got)
	}
	if e1.renderer != nil {
		t.Fatal("disposed emitter still holds a renderer backpointer")
	}
	b := bucketFor(r, img)
	if b == nil || len(b.emitters) != 1 || b.emitters[0] != e2 {
		t.Fatalf("bucket after dispose=%v, want only e2", b)
	}

	// Re-adding the disposed emitter is a no-op: no duplicate draw,
	// no stale batch entry.
	r.AddEmitter(e1)
	spy.reset()
	r.Draw(dst)
	if got := spy.totalParticles(); got != 2 {
		t.Fatalf("re-adding disposed emitter changed draw count to %d, want 2", got)
	}
	if got := bucketMembershipCount(b, e1); got != 0 {
		t.Fatalf("disposed emitter found in bucket %d times, want 0", got)
	}

	// A brand new emitter for the same image joins the existing bucket.
	e3 := NewEmitter(newTestTemplate(img, 0.1, 10))
	r.AddEmitter(e3)
	emitParticles(e3, 4, 0.1)
	if got := len(b.emitters); got != 2 {
		t.Fatalf("fresh emitter join: bucket size=%d, want 2", got)
	}
	spy.reset()
	r.Draw(dst)
	if got := spy.totalParticles(); got != 6 {
		t.Fatalf("with fresh emitter drew %d particles, want 6 (e2=2 + e3=4)", got)
	}

	// Once every emitter of the image is gone, the bucket must retire.
	e2.Dispose()
	e3.Dispose()
	r.Draw(dst)
	if bucketFor(r, img) != nil {
		t.Fatal("bucket still mapped after all its emitters were disposed")
	}
	if len(r.freeList) != 1 {
		t.Fatalf("freeList size=%d, want 1 retired bucket", len(r.freeList))
	}
}

// Adding a live emitter twice must not duplicate it in the batch or
// make it draw twice.
func TestAddEmitterIdempotent(t *testing.T) {
	img := newTestImage(4)
	tmpl := newTestTemplate(img, 0.1, 10)
	r := NewRenderer()
	dst := newDstImage()
	spy := installDrawSpy(t)

	e := NewEmitter(tmpl)
	r.AddEmitter(e)
	r.AddEmitter(e)

	b := bucketFor(r, img)
	if got := bucketMembershipCount(b, e); got != 1 {
		t.Fatalf("double AddEmitter membership count=%d, want 1", got)
	}

	emitParticles(e, 3, 0.1)
	r.Draw(dst)
	if got := spy.totalParticles(); got != 3 {
		t.Fatalf("double-registered emitter drew %d particles, want 3", got)
	}
}

// Emitters sharing one image share one bucket; disposing or clearing
// one of them must not affect the particle counts or draw output of
// the others.
func TestSharedBucketIsolation(t *testing.T) {
	img := newTestImage(4)
	r := NewRenderer()
	dst := newDstImage()
	spy := installDrawSpy(t)

	e1 := NewEmitter(newTestTemplate(img, 0.1, 10))
	e2 := NewEmitter(newTestTemplate(img, 0.1, 10))
	e3 := NewEmitter(newTestTemplate(img, 0.1, 10))
	for _, e := range []*Emitter{e1, e2, e3} {
		r.AddEmitter(e)
	}
	emitParticles(e1, 2, 0.1)
	emitParticles(e2, 3, 0.1)
	emitParticles(e3, 4, 0.1)

	if got := len(bucketFor(r, img).emitters); got != 3 {
		t.Fatalf("shared bucket size=%d, want 3", got)
	}

	e1.Clear()
	if got := e1.NumParticles(); got != 0 {
		t.Fatalf("cleared emitter still has %d particles", got)
	}
	e2.Dispose()

	r.Draw(dst)
	if got := spy.totalParticles(); got != 4 {
		t.Fatalf("after clear+dispose drew %d particles, want 4 (only e3)", got)
	}

	// The surviving emitter is fully untouched.
	if got := e3.NumParticles(); got != 4 {
		t.Fatalf("surviving emitter particle count=%d, want 4", got)
	}
	b := bucketFor(r, img)
	if got := bucketMembershipCount(b, e3); got != 1 {
		t.Fatalf("surviving emitter membership=%d, want 1", got)
	}
	if bucketMembershipCount(b, e1) != 1 {
		t.Fatal("cleared (but live) emitter must stay registered in the bucket")
	}
	if bucketMembershipCount(b, e2) != 0 {
		t.Fatal("disposed emitter must have been removed from the bucket")
	}

	// Clearing the last live emitter empties its draws but does not
	// retire the bucket: the emitter is still an active member.
	e3.Clear()
	spy.reset()
	r.Draw(dst)
	if len(spy.calls) != 0 {
		t.Fatalf("fully cleared bucket produced %d batch draws, want 0", len(spy.calls))
	}
	if bucketFor(r, img) == nil {
		t.Fatal("bucket with live (but empty) emitters must not be retired")
	}
}

// When a bucket is retired because all of its emitters became invalid,
// a later emitter using the same image must reuse that bucket instead
// of growing the bucket list forever.
func TestBucketRecycleReuse(t *testing.T) {
	img := newTestImage(4)
	r := NewRenderer()
	dst := newDstImage()
	installDrawSpy(t)

	for cycle := 0; cycle < 5; cycle++ {
		e := NewEmitter(newTestTemplate(img, 0.1, 10))
		r.AddEmitter(e)
		emitParticles(e, 2, 0.1)

		if got := len(r.bucketList); got != 1 {
			t.Fatalf("cycle %d: bucketList size=%d, want 1 (retired buckets must be reused)", cycle, got)
		}
		r.Draw(dst)

		e.Dispose()
		r.Draw(dst)
		if bucketFor(r, img) != nil {
			t.Fatalf("cycle %d: bucket not retired after sole emitter disposal", cycle)
		}
		if len(r.freeList) != 1 {
			t.Fatalf("cycle %d: freeList size=%d, want 1", cycle, len(r.freeList))
		}
	}

	if got := len(r.bucketList); got != 1 {
		t.Fatalf("bucketList grew to %d after recycle cycles, want 1", got)
	}

	// The recycled bucket still works end-to-end for a new emitter.
	e := NewEmitter(newTestTemplate(img, 0.1, 10))
	r.AddEmitter(e)
	emitParticles(e, 3, 0.1)
	b := bucketFor(r, img)
	if b == nil {
		t.Fatal("recycled bucket was not re-mapped for the new emitter")
	}
	if len(r.freeList) != 0 {
		t.Fatalf("freeList size after reuse=%d, want 0", len(r.freeList))
	}
}

// A disposed renderer draws nothing ever again, but the emitters it
// owned remain alive, updatable and queryable, and its buckets stay
// intact for inspection.
func TestRendererDisposeStopsDraw(t *testing.T) {
	img := newTestImage(4)
	r := NewRenderer()
	dst := newDstImage()
	spy := installDrawSpy(t)

	e1 := NewEmitter(newTestTemplate(img, 0.1, 10))
	e2 := NewEmitter(newTestTemplate(img, 0.1, 10))
	r.AddEmitter(e1)
	r.AddEmitter(e2)
	emitParticles(e1, 2, 0.1)
	emitParticles(e2, 3, 0.1)

	r.Draw(dst)
	if spy.totalParticles() != 5 {
		t.Fatalf("initial draw submitted %d particles, want 5", spy.totalParticles())
	}
	spy.reset()

	r.Dispose()
	if !r.IsDisposed() {
		t.Fatal("renderer IsDisposed()=false after Dispose()")
	}
	r.Draw(dst)
	r.Draw(dst)
	if len(spy.calls) != 0 {
		t.Fatalf("disposed renderer produced %d batch draws, want 0", len(spy.calls))
	}

	// Emitter state is still queryable and updates still age particles.
	if e1.IsDisposed() || e2.IsDisposed() {
		t.Fatal("renderer disposal must not dispose its emitters")
	}
	if e1.NumParticles() != 2 || e2.NumParticles() != 3 {
		t.Fatalf("emitter counts changed: e1=%d e2=%d", e1.NumParticles(), e2.NumParticles())
	}
	counterBefore := e1.particles[0].counter
	e1.UpdateWithDelta(0.1)
	if e1.particles[0].counter <= counterBefore {
		t.Fatal("emitter stopped aging after its renderer was disposed")
	}

	// Buckets are retained for inspection rather than torn down.
	if got := len(r.bucketList); got != 1 || r.bucketList[0].img == nil {
		t.Fatalf("disposed renderer buckets=%v, want the original bucket retained", r.bucketList)
	}

	// A destroyed renderer does not accept new memberships either.
	extra := NewEmitter(newTestTemplate(img, 0.1, 10))
	r.AddEmitter(extra)
	if got := len(r.bucketList[0].emitters); got != 2 {
		t.Fatalf("AddEmitter on disposed renderer changed membership to %d, want 2", got)
	}
	if extra.renderer != nil {
		t.Fatal("emitter added to a disposed renderer must not take ownership")
	}
}

// An emitter moved from one renderer to another must belong to exactly
// one batch at a time and be drawn only by its current renderer.
func TestEmitterMovesBetweenRenderers(t *testing.T) {
	img := newTestImage(4)
	tmpl := newTestTemplate(img, 0.1, 10)
	r1 := NewRenderer()
	r2 := NewRenderer()
	dst := newDstImage()
	spy1 := installDrawSpy(t)

	e := NewEmitter(tmpl)
	r1.AddEmitter(e)
	emitParticles(e, 3, 0.1)

	// Moving the emitter to another renderer drops it from r1's batch.
	r2.AddEmitter(e)

	if e.renderer != r2 {
		t.Fatal("emitter backpointer does not point at the new renderer")
	}
	r1.Draw(dst)
	if got := spy1.totalParticles(); got != 0 {
		t.Fatalf("old renderer still drew %d particles after the emitter moved", got)
	}
	if b := bucketFor(r1, img); b != nil && len(b.emitters) != 0 {
		t.Fatalf("old renderer bucket still holds %d emitters", len(b.emitters))
	}

	r2.Draw(dst)
	if got := spy1.totalParticles(); got != 3 {
		t.Fatalf("new renderer drew %d particles, want 3", got)
	}
	if got := bucketMembershipCount(bucketFor(r2, img), e); got != 1 {
		t.Fatalf("emitter membership in new renderer=%d, want 1", got)
	}
}
