// Package finalizer provides an ordered, single-flight, idempotent shutdown
// sequence for a recording pipeline.
//
// The sequence is: stop the recording source first, then drain and commit
// the upload. A Finalizer guarantees that:
//
//   - concurrent Stop calls never run two finalizations: later callers wait
//     for the in-flight run and share its outcome;
//   - a failed run leaves nothing cached, so Stop can be retried with the
//     same Finalizer until it succeeds;
//   - a successful result is cached and returned to every subsequent caller,
//     so Stop is idempotent once it has succeeded.
package finalizer

import (
	"fmt"
	"sync"

	"github.com/OmGuptaIND/cloud"
)

// Finalizer runs an ordered, single-flight, idempotent stop sequence.
type Finalizer struct {
	// stopSource stops the recording process. It runs before finalize and
	// must be idempotent, because it runs again on every retry.
	stopSource func() error
	// finalize drains the remaining recording data into the upload and
	// commits the multipart upload, returning the recording URL.
	finalize func() (*cloud.CloudUploadPartCompleted, error)

	mu     sync.Mutex
	result *cloud.CloudUploadPartCompleted
}

// New creates a Finalizer. stopSource may be nil; finalize is required.
func New(stopSource func() error, finalize func() (*cloud.CloudUploadPartCompleted, error)) *Finalizer {
	return &Finalizer{
		stopSource: stopSource,
		finalize:   finalize,
	}
}

// Stop runs the stop sequence once and returns the recording URL.
//
// The recording source is always stopped before the upload is finalized.
// If any step fails, nothing is cached and Stop may be called again to retry.
// Once Stop has succeeded, every later call returns the same result without
// re-running the sequence.
func (f *Finalizer) Stop() (*cloud.CloudUploadPartCompleted, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.result != nil {
		return f.result, nil
	}

	if f.stopSource != nil {
		if err := f.stopSource(); err != nil {
			return nil, fmt.Errorf("failed to stop recording source: %w", err)
		}
	}

	if f.finalize == nil {
		return nil, fmt.Errorf("no finalize step configured")
	}

	result, err := f.finalize()

	if err != nil {
		return nil, err
	}

	if result == nil || result.Recording_Url == nil {
		return nil, fmt.Errorf("finalize completed without a recording url")
	}

	f.result = result

	return result, nil
}

// Done reports whether the stop sequence has completed successfully.
func (f *Finalizer) Done() bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.result != nil
}
