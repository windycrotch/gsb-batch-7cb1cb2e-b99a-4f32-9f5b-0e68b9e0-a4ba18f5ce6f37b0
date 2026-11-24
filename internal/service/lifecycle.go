package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/dionysia-dev/dionysia/internal/db"
	"github.com/google/uuid"
)

// Lifecycle statuses exposed to operators. They mirror the persistence
// statuses defined in internal/db/model but are part of the service contract.
const (
	StatusActive    = "active"
	StatusPackaging = "packaging"
	StatusFailed    = "failed"
	StatusDeleting  = "deleting"
)

// Sentinel errors that let transport layers distinguish lifecycle outcomes
// from generic infrastructure failures (and therefore avoid a blanket 500).
var (
	// ErrInputNotFound is returned when an input does not exist (anymore).
	ErrInputNotFound = errors.New("input not found")
	// ErrInputDeleting is returned when an input deletion is in progress and
	// new packaging work can no longer be accepted for it.
	ErrInputDeleting = errors.New("input is being deleted")
)

// IsInputNotFound reports whether err denotes a missing input, covering both
// the persistence sentinel and the service-level sentinel.
func IsInputNotFound(err error) bool {
	return errors.Is(err, ErrInputNotFound) || errors.Is(err, db.ErrNotFound)
}

// IsFailedStatus reports whether a persisted status represents a packaging
// failure. It accepts the canonical "failed" value as well as the common
// "error" spelling.
func IsFailedStatus(status string) bool {
	return status == StatusFailed || status == "error"
}

// inputGuard holds the deletion barrier and the currently active executions
// for a single input ID.
type inputGuard struct {
	mu sync.Mutex

	// deleted is set as soon as a deletion is requested. Once set, new
	// executions can no longer begin and packaging requests are rejected.
	deleted bool

	// wg tracks all packaging executions that began before deleted was set.
	wg      sync.WaitGroup
	seq     uint64
	cancels map[uint64]context.CancelFunc
}

// Execution is a registered, in-progress packaging run. The worker uses its
// Context to run gpac so deletion can terminate it, and MUST call Finish when
// the run is over to release the deletion barrier.
type Execution struct {
	id  uuid.UUID
	seq uint64
	g   *inputGuard

	ctx    context.Context
	cancel context.CancelFunc
	once   sync.Once
}

// Context is the context the packaging work (and gpac subprocess) should be
// bound to. It is canceled when the input is deleted.
func (e *Execution) Context() context.Context {
	return e.ctx
}

// Finish unregisters the execution and unblocks a waiting delete.
func (e *Execution) Finish() {
	e.once.Do(func() {
		e.cancel()

		e.g.mu.Lock()
		delete(e.g.cancels, e.seq)
		e.g.mu.Unlock()

		e.g.wg.Done()
	})
}

// Coordinator serializes input deletion against packaging executions inside a
// process. It is the in-process half of the lifecycle alignment; persistent
// re-validation in the task handler covers separate processes and restarts.
//
// A zero Coordinator is not usable; obtain one with NewCoordinator or use
// DefaultCoordinator.
type Coordinator struct {
	mu     sync.Mutex
	guards map[uuid.UUID]*inputGuard
}

// NewCoordinator creates an isolated coordinator. Tests that need
// deterministic isolation should create one and share it between the input,
// notification and packaging handlers.
func NewCoordinator() *Coordinator {
	return &Coordinator{guards: make(map[uuid.UUID]*inputGuard)}
}

var defaultCoordinator = NewCoordinator()

// DefaultCoordinator is the process-wide coordinator used by the zero-wiring
// constructors and the production fx setup.
func DefaultCoordinator() *Coordinator {
	return defaultCoordinator
}

func (c *Coordinator) guardFor(id uuid.UUID) *inputGuard {
	c.mu.Lock()
	defer c.mu.Unlock()

	g, ok := c.guards[id]
	if !ok {
		g = &inputGuard{cancels: make(map[uint64]context.CancelFunc)}
		c.guards[id] = g
	}

	return g
}

// Reset clears any deletion state recorded for id. It is mainly useful for
// tests that reuse a fixed identifier across independent scenarios; normal
// request flows never need it.
func (c *Coordinator) Reset(id uuid.UUID) {
	c.mu.Lock()
	delete(c.guards, id)
	c.mu.Unlock()
}

// Begin attempts to register a packaging execution. It returns false when a
// deletion already won the race; in that case the caller must abandon the
// task without invoking gpac. The returned Execution MUST be Finished.
func (c *Coordinator) Begin(parent context.Context, id uuid.UUID) (*Execution, bool) {
	g := c.guardFor(id)

	g.mu.Lock()
	if g.deleted {
		g.mu.Unlock()
		return nil, false
	}

	g.seq++
	seq := g.seq
	ctx, cancel := context.WithCancel(parent)
	g.cancels[seq] = cancel
	g.wg.Add(1)
	g.mu.Unlock()

	return &Execution{id: id, seq: seq, g: g, ctx: ctx, cancel: cancel}, true
}

// IsDeleted reports whether deletion was requested/completed for id.
func (c *Coordinator) IsDeleted(id uuid.UUID) bool {
	c.mu.Lock()
	g, ok := c.guards[id]
	c.mu.Unlock()

	if !ok {
		return false
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	return g.deleted
}

// Delete runs fn behind the deletion barrier for id:
//
//  1. it marks the input as deleted so new executions are rejected;
//  2. it cancels every running execution (killing its gpac subprocess);
//  3. it waits for those executions to finish their own cleanup;
//  4. only then it invokes fn, which removes the persistence record and the
//     output artifacts.
//
// Every Delete call invokes fn exactly once. Repeated or concurrent deletes
// are safe because fn is expected to be idempotent (the SQL delete and the
// artifact removal both are) and the barrier can be waited on by many callers.
func (c *Coordinator) Delete(id uuid.UUID, fn func() error) error {
	g := c.guardFor(id)

	g.mu.Lock()
	g.deleted = true

	cancels := make([]context.CancelFunc, 0, len(g.cancels))
	for _, cancel := range g.cancels {
		cancels = append(cancels, cancel)
	}
	g.mu.Unlock()

	// Terminate any gpac processes bound to the executions, then wait until
	// all executions have fully stopped and cleaned their artifacts. Calling
	// an already-canceled CancelFunc is a no-op, so concurrent deletes are
	// safe to issue their own snapshots.
	for _, cancel := range cancels {
		cancel()
	}

	g.wg.Wait()

	return fn()
}

// defaultPlayoutPath is the artifact root used by handlers constructed
// without an explicit path. Production wiring sets it from config.
var (
	playoutPathMu      sync.RWMutex
	defaultPlayoutPath = filepath.Join(os.TempDir(), "dionysia")
)

// SetDefaultPlayoutPath configures the process-wide artifact root.
func SetDefaultPlayoutPath(path string) {
	if path == "" {
		return
	}

	playoutPathMu.Lock()
	defaultPlayoutPath = path
	playoutPathMu.Unlock()
}

// DefaultPlayoutPath returns the process-wide artifact root.
func DefaultPlayoutPath() string {
	playoutPathMu.RLock()
	defer playoutPathMu.RUnlock()

	return defaultPlayoutPath
}

// artifactDirFor returns the per-input HLS output directory.
func artifactDirFor(base, id string) string {
	if base == "" {
		base = DefaultPlayoutPath()
	}

	return filepath.Join(base, id)
}

// RemoveInputArtifacts deletes the HLS directory of a single input. It is
// best-effort and retried briefly because on Windows a just-killed subprocess
// may release its file handles a few milliseconds later.
func RemoveInputArtifacts(base, id string) error {
	dir := artifactDirFor(base, id)

	var lastErr error
	for attempt := 0; attempt < 10; attempt++ {
		if err := os.RemoveAll(dir); err == nil {
			return nil
		} else { //nolint:revive // keep last error and retry
			lastErr = err
		}

		// Bounded backoff; on Windows a just-killed subprocess may release
		// its file handles a few milliseconds later.
		time.Sleep(time.Duration(20*(attempt+1)) * time.Millisecond)
	}

	return lastErr
}
