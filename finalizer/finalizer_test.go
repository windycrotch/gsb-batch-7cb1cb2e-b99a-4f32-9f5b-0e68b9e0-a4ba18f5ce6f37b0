package finalizer_test

import (
	"errors"
	"sync"
	"testing"

	"github.com/OmGuptaIND/cloud"
	"github.com/OmGuptaIND/finalizer"
)

func urlResult(url string) *cloud.CloudUploadPartCompleted {
	return &cloud.CloudUploadPartCompleted{Recording_Url: &url}
}

// TestStopRunsStepsInOrder verifies the shutdown sequence: the recording
// source is stopped before the upload is finalized.
func TestStopRunsStepsInOrder(t *testing.T) {
	var order []string

	f := finalizer.New(
		func() error {
			order = append(order, "stopSource")
			return nil
		},
		func() (*cloud.CloudUploadPartCompleted, error) {
			order = append(order, "finalize")
			return urlResult("https://bucket/recording.mp4"), nil
		},
	)

	resp, err := f.Stop()
	if err != nil {
		t.Fatalf("Stop failed: %v", err)
	}
	if *resp.Recording_Url != "https://bucket/recording.mp4" {
		t.Fatalf("unexpected url %q", *resp.Recording_Url)
	}

	if len(order) != 2 || order[0] != "stopSource" || order[1] != "finalize" {
		t.Fatalf("steps ran in wrong order: %v", order)
	}
	if !f.Done() {
		t.Fatal("finalizer should report done after a successful stop")
	}
}

// TestStopFailureDoesNotStopSourceOnly verifies that when the source cannot
// be stopped, the upload is never finalized.
func TestStopSourceFailureSkipsFinalize(t *testing.T) {
	finalizeRan := false

	f := finalizer.New(
		func() error { return errors.New("recorder would not stop") },
		func() (*cloud.CloudUploadPartCompleted, error) {
			finalizeRan = true
			return urlResult("https://bucket/recording.mp4"), nil
		},
	)

	if _, err := f.Stop(); err == nil {
		t.Fatal("Stop should fail when the source cannot be stopped")
	}
	if finalizeRan {
		t.Fatal("finalize must not run when stopping the source failed")
	}
	if f.Done() {
		t.Fatal("finalizer must not report done after a failed stop")
	}
}

// TestStopRetryAfterFailure covers a failed finalization followed by a
// successful retry: the failure is not cached, the retry returns the url,
// and later calls return the cached result without re-running anything.
func TestStopRetryAfterFailure(t *testing.T) {
	finalizeCalls := 0
	stopSourceCalls := 0

	f := finalizer.New(
		func() error {
			stopSourceCalls++
			return nil
		},
		func() (*cloud.CloudUploadPartCompleted, error) {
			finalizeCalls++
			if finalizeCalls == 1 {
				return nil, errors.New("transient commit failure")
			}
			return urlResult("https://bucket/recording.mp4"), nil
		},
	)

	if _, err := f.Stop(); err == nil {
		t.Fatal("first Stop should have failed")
	}
	if f.Done() {
		t.Fatal("finalizer must not report done after a failed stop")
	}

	resp, err := f.Stop()
	if err != nil {
		t.Fatalf("retried Stop failed: %v", err)
	}
	if *resp.Recording_Url != "https://bucket/recording.mp4" {
		t.Fatalf("unexpected url %q", *resp.Recording_Url)
	}

	// Third call: cached result, no re-run.
	again, err := f.Stop()
	if err != nil {
		t.Fatalf("third Stop failed: %v", err)
	}
	if *again.Recording_Url != *resp.Recording_Url {
		t.Fatal("idempotent Stop returned a different result")
	}
	if finalizeCalls != 2 {
		t.Fatalf("finalize ran %d times, want 2 (failure + retry)", finalizeCalls)
	}
	if stopSourceCalls != 2 {
		t.Fatalf("stopSource ran %d times, want 2 (failure + retry)", stopSourceCalls)
	}
}

// TestConcurrentStopRunsFinalizationOnce covers concurrent stop requests:
// exactly one finalization executes and every caller shares its result.
func TestConcurrentStopRunsFinalizationOnce(t *testing.T) {
	var mu sync.Mutex
	finalizeCalls := 0

	release := make(chan struct{})

	f := finalizer.New(
		func() error { return nil },
		func() (*cloud.CloudUploadPartCompleted, error) {
			mu.Lock()
			finalizeCalls++
			mu.Unlock()
			<-release // hold the finalization so callers pile up
			return urlResult("https://bucket/recording.mp4"), nil
		},
	)

	const callers = 8

	results := make([]*cloud.CloudUploadPartCompleted, callers)
	errs := make([]error, callers)

	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = f.Stop()
		}(i)
	}

	close(release)
	wg.Wait()

	for i := 0; i < callers; i++ {
		if errs[i] != nil {
			t.Fatalf("concurrent Stop %d failed: %v", i, errs[i])
		}
		if *results[i].Recording_Url != "https://bucket/recording.mp4" {
			t.Fatalf("concurrent Stop %d got unexpected url %q", i, *results[i].Recording_Url)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if finalizeCalls != 1 {
		t.Fatalf("finalize ran %d times, want exactly 1", finalizeCalls)
	}
}

// TestConcurrentRetryAfterFailure covers a failure followed by concurrent
// retries: the retries converge on a single successful finalization.
func TestConcurrentRetryAfterFailure(t *testing.T) {
	var mu sync.Mutex
	finalizeCalls := 0

	f := finalizer.New(
		func() error { return nil },
		func() (*cloud.CloudUploadPartCompleted, error) {
			mu.Lock()
			defer mu.Unlock()
			finalizeCalls++
			if finalizeCalls == 1 {
				return nil, errors.New("transient commit failure")
			}
			return urlResult("https://bucket/recording.mp4"), nil
		},
	)

	if _, err := f.Stop(); err == nil {
		t.Fatal("first Stop should have failed")
	}

	const callers = 4
	errs := make([]error, callers)

	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = f.Stop()
		}(i)
	}
	wg.Wait()

	for i := 0; i < callers; i++ {
		if errs[i] != nil {
			t.Fatalf("retry %d failed: %v", i, errs[i])
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if finalizeCalls != 2 {
		t.Fatalf("finalize ran %d times, want 2 (failure + one retried success)", finalizeCalls)
	}
}
