package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"github.com/dionysia-dev/dionysia/internal/config"
	"github.com/dionysia-dev/dionysia/internal/db"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"
)

const (
	TypeStreamPackage = "stream:package"
)

type StreamPayload struct {
	ID          uuid.UUID `json:"id"`
	Input       Input     `json:"input"`
	Address     string    `json:"address"`
	PlayoutPath string    `json:"playout_path"`
}

func NewPackageTask(id uuid.UUID, input Input, cfg *config.Config) (*asynq.Task, error) {
	payload, err := json.Marshal(StreamPayload{
		ID:          id,
		Input:       input,
		Address:     "rtmp://media-server:1935",
		PlayoutPath: cfg.PlayoutPath,
	})
	if err != nil {
		return nil, err
	}

	return asynq.NewTask(TypeStreamPackage, payload), nil
}

// StreamPackager contains the dependency-aware packaging logic. It
// re-validates the input before/after running gpac, coordinates with deletion
// and persists the resulting lifecycle status.
type StreamPackager struct {
	store       db.InputStore
	coordinator *Coordinator
	playoutPath string
	gpacBin     string

	// runCommand overrides real gpac execution in tests. When nil the command
	// runs as a context-bound subprocess (honoring GPACBin / cancellation).
	runCommand func(ctx context.Context, cmd *GPACCommand) error

	statusTimeout time.Duration
	// watchInterval is how often the running task re-checks persistence for a
	// cross-process delete while gpac is running.
	watchInterval time.Duration
}

// WithStatusWatchInterval configures the cross-process deletion watch poll.
func WithStatusWatchInterval(d time.Duration) PackagerOption {
	return func(h *StreamPackager) {
		h.watchInterval = d
	}
}

// PackagerOption customizes a StreamPackager.
type PackagerOption func(*StreamPackager)

func WithPackagerCoordinator(c *Coordinator) PackagerOption {
	return func(h *StreamPackager) {
		if c != nil {
			h.coordinator = c
		}
	}
}

func WithPackagerPlayoutPath(path string) PackagerOption {
	return func(h *StreamPackager) {
		if path != "" {
			h.playoutPath = path
		}
	}
}

func WithPackagerGPACBin(bin string) PackagerOption {
	return func(h *StreamPackager) {
		if bin != "" {
			h.gpacBin = bin
		}
	}
}

// WithCommandRunner replaces gpac execution. Intended for fast in-process
// tests that do not want a real subprocess.
func WithCommandRunner(run func(ctx context.Context, cmd *GPACCommand) error) PackagerOption {
	return func(h *StreamPackager) {
		if run != nil {
			h.runCommand = run
		}
	}
}

func NewStreamPackager(store db.InputStore, cfg *config.Config, opts ...PackagerOption) *StreamPackager {
	h := &StreamPackager{
		store:         store,
		coordinator:   DefaultCoordinator(),
		statusTimeout: 5 * time.Second,
		watchInterval: 200 * time.Millisecond,
	}

	if cfg != nil {
		h.playoutPath = cfg.PlayoutPath
		h.gpacBin = cfg.GPACBin
	}

	for _, opt := range opts {
		opt(h)
	}

	return h
}

// HandleTask is the asynq.HandlerFunc-compatible entry point.
func (h *StreamPackager) HandleTask(ctx context.Context, t *asynq.Task) error {
	var payload StreamPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		return fmt.Errorf("failed to unmarshal json: %v: %w", err, asynq.SkipRetry)
	}

	return h.HandlePayload(ctx, payload)
}

// HandlePayload runs the lifecycle-aware packaging flow for one payload.
func (h *StreamPackager) HandlePayload(ctx context.Context, payload StreamPayload) error {
	id := payload.ID
	log.Printf("Packaging stream %s", id.String())

	outputBase := payload.PlayoutPath
	if outputBase == "" {
		outputBase = h.playoutPath
	}

	abandon := func(reason string) error {
		// Never leave a playlist/segments of a deleted input behind,
		// including stale files written by a previous worker generation.
		if err := RemoveInputArtifacts(outputBase, id.String()); err != nil {
			log.Printf("failed removing artifacts of abandoned input %s: %v", id, err)
		}

		return fmt.Errorf("input %s %s, abandoning packaging: %w", id, reason, asynq.SkipRetry)
	}

	// 1) In-process deletion barrier. If delete already won, do not touch
	// gpac or the output directory.
	runCtx := ctx

	var execution *Execution

	if h.coordinator != nil {
		var allowed bool

		execution, allowed = h.coordinator.Begin(ctx, id)
		if !allowed {
			log.Printf("Input %s was deleted before packaging started", id)
			return abandon("already deleted")
		}

		runCtx = execution.Context()

		defer execution.Finish()
	}

	// 2) Persistent re-validation. This is the restart-safe / cross-process
	// guard: a task carrying a stale input snapshot must discover that the
	// record is gone instead of blindly running gpac.
	if h.store != nil {
		_, err := h.store.GetInput(runCtx, id)

		switch {
		case err == nil:
			// still present
		case IsInputNotFound(err):
			log.Printf("Input %s no longer exists in store; skipping gpac", id)
			return abandon("no longer exists")
		case runCtx.Err() != nil:
			return abandon("packaging canceled by deletion")
		default:
			// Transient store failure: do not pretend success and do not
			// write artifacts based on a stale snapshot.
			return fmt.Errorf("failed to re-load input %s before packaging: %w", id, err)
		}

		if runCtx.Err() != nil {
			return abandon("packaging canceled by deletion")
		}

		h.persistStatus(id, StatusPackaging, "")
	}

	// 3) Build and execute the gpac command under a context that both the
	// in-process deletion barrier and the cross-process persistence watcher
	// can cancel.
	commandCtx, stopWatcher := context.WithCancel(runCtx)
	defer stopWatcher()

	if h.store != nil && h.watchInterval > 0 {
		go h.watchDeletion(commandCtx, id, stopWatcher)
	}

	commandConfig := NewDefaultCommandConfig()
	builder := NewGPACCommandBuilder(commandConfig)

	command := builder.Build(id.String(), payload.Address, outputBase, payload.Input)
	command.Ctx = commandCtx

	if h.gpacBin != "" {
		command.Bin = h.gpacBin
	}

	var runErr error
	if h.runCommand != nil {
		runErr = h.runCommand(commandCtx, command)
	} else {
		runErr = command.Execute()
	}

	// NOTE: stopWatcher is intentionally only invoked by the deferred call
	// below, so commandCtx.Err() still reflects a deletion-triggered cancel
	// at the outcome classification right after this.

	// 4) Re-check persistence after gpac returns. Covers a delete handled by
	// a different API/worker process that removed the row while gpac ran.
	deletedInProcess := h.coordinator != nil && h.coordinator.IsDeleted(id)
	existsInStore := h.inputStillExists(id)
	canceledForDeletion := commandCtx.Err() != nil || runCtx.Err() != nil

	if deletedInProcess || !existsInStore || canceledForDeletion {
		log.Printf("Input %s deleted while packaging was running; discarding artifacts", id)
		return abandon("deleted during packaging")
	}

	// 5) gpac failed but the input still exists: retain the record and expose
	// the failure through its status. Never delete here.
	if runErr != nil {
		log.Printf("Failed to execute gpac command for input %s: %v", id, runErr)
		h.persistStatus(id, StatusFailed, runErr.Error())

		return fmt.Errorf("failed to execute command: %v: %w", runErr, asynq.SkipRetry)
	}

	h.persistStatus(id, StatusActive, "")
	log.Printf("Stream %s command executed successfully\n", id.String())

	return nil
}

// watchDeletion polls persistence while gpac is running so a delete handled
// by another process (the API and worker run as separate containers in
// production) terminates the subprocess. It cancels the command context when
// the input row disappears or is flagged as deleting.
func (h *StreamPackager) watchDeletion(ctx context.Context, id uuid.UUID, cancel context.CancelFunc) {
	interval := h.watchInterval
	if interval <= 0 {
		interval = 200 * time.Millisecond
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			checkCtx, checkCancel := context.WithTimeout(context.Background(), h.statusTimeout)
			in, err := h.store.GetInput(checkCtx, id)
			checkCancel()

			switch {
			case IsInputNotFound(err):
				log.Printf("Cross-process delete detected for input %s; stopping gpac", id)
				cancel()

				return
			case err == nil && in.Status == StatusDeleting:
				log.Printf("Input %s flagged deleting; stopping gpac", id)
				cancel()

				return
			}
		}
	}
}

// inputStillExists performs a detached existence check so an already-canceled
// run context cannot mask a missing record. A store error is treated as
// "exists" to avoid deleting/discarding work on a transient outage; the
// in-process coordinator handles explicit deletion authoritatively.
func (h *StreamPackager) inputStillExists(id uuid.UUID) bool {
	if h.store == nil {
		return true
	}

	ctx, cancel := context.WithTimeout(context.Background(), h.statusTimeout)
	defer cancel()

	_, err := h.store.GetInput(ctx, id)
	if err == nil {
		return true
	}

	if IsInputNotFound(err) {
		return false
	}

	log.Printf("post-packaging input re-check failed for %s: %v", id, err)

	return true
}

func (h *StreamPackager) persistStatus(id uuid.UUID, status, message string) {
	if h.store == nil {
		return
	}

	updater, ok := h.store.(db.InputStatusUpdater)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), h.statusTimeout)
	defer cancel()

	if err := updater.UpdateInputStatus(ctx, id, status, message); err != nil {
		log.Printf("failed to persist status %q for input %s: %v", status, id, err)
	}
}

// defaultPackager backs the package-level HandleStreamPackageTask used by the
// asynq mux. Production worker wiring installs a dependency-aware packager;
// tests/embedders may do the same via SetDefaultStreamPackager.
var defaultPackager atomic.Pointer[StreamPackager]

// SetDefaultStreamPackager installs the packager used by the package-level
// HandleStreamPackageTask entry point.
func SetDefaultStreamPackager(h *StreamPackager) {
	defaultPackager.Store(h)
}

// HandleStreamPackageTask is the original package-level asynq handler. It is
// preserved for compatibility; after unmarshalling it delegates to the
// installed default packager, falling back to the legacy direct gpac run when
// none is configured.
func HandleStreamPackageTask(ctx context.Context, t *asynq.Task) error {
	var payload StreamPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		return fmt.Errorf("failed to unmarshal json: %v: %w", err, asynq.SkipRetry)
	}

	if h := defaultPackager.Load(); h != nil {
		return h.HandlePayload(ctx, payload)
	}

	return legacyHandleStreamPackage(payload)
}

// legacyHandleStreamPackage reproduces the original behavior when no
// dependency-aware packager has been installed.
func legacyHandleStreamPackage(payload StreamPayload) error {
	log.Printf("Packaging stream %s", payload.ID.String())

	cfg := NewDefaultCommandConfig()
	builder := NewGPACCommandBuilder(cfg)
	cmd := builder.Build(payload.ID.String(), payload.Address, payload.PlayoutPath, payload.Input)

	if err := cmd.Execute(); err != nil {
		log.Printf("Failed to execute command: %v", err)

		return fmt.Errorf("failed to execute command: %v: %w", err, asynq.SkipRetry)
	}

	log.Printf("Stream %s command executed successfully\n", payload.ID.String())

	return nil
}
