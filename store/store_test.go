package store_test

import (
	"errors"
	"sync"
	"testing"

	"github.com/OmGuptaIND/cloud"
	"github.com/OmGuptaIND/store"
)

// fakePipeline is a controllable store.Pipeline. Its Stop fails while errs
// is non-empty, then succeeds; like the real pipeline, a successful result
// is cached so concurrent and repeated stops share one finalization.
type fakePipeline struct {
	mu        sync.Mutex
	errs      []error
	stopCalls int
	result    *cloud.CloudUploadPartCompleted
}

func (f *fakePipeline) Stop() (*cloud.CloudUploadPartCompleted, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.result != nil {
		return f.result, nil
	}

	f.stopCalls++

	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		return nil, err
	}

	url := "https://bucket/recording.mp4"
	f.result = &cloud.CloudUploadPartCompleted{Recording_Url: &url}

	return f.result, nil
}

func (f *fakePipeline) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopCalls
}

func newTestStore() *store.AppStore {
	return &store.AppStore{Pipelines: make(map[string]store.Pipeline)}
}

// TestStopPipelineSuccessThenNotFound covers the happy path: the first stop
// succeeds and returns the recording url, the pipeline is removed, and a
// later stop with the same id reports not found.
func TestStopPipelineSuccessThenNotFound(t *testing.T) {
	s := newTestStore()
	p := &fakePipeline{}
	s.AddPipeLine("pipeline_1", p)

	resp, err := s.StopPipeline("pipeline_1")
	if err != nil {
		t.Fatalf("StopPipeline failed: %v", err)
	}
	if resp == nil || resp.Recording_Url == nil {
		t.Fatal("StopPipeline returned no recording url")
	}

	if _, ok := s.GetPipeline("pipeline_1"); ok {
		t.Fatal("pipeline must be removed from the store after a successful stop")
	}

	_, err = s.StopPipeline("pipeline_1")
	if !errors.Is(err, store.ErrPipelineNotFound) {
		t.Fatalf("expected ErrPipelineNotFound after removal, got %v", err)
	}
}

// TestStopPipelineUnknownID covers stopping an id that was never started.
func TestStopPipelineUnknownID(t *testing.T) {
	s := newTestStore()

	_, err := s.StopPipeline("pipeline_missing")
	if !errors.Is(err, store.ErrPipelineNotFound) {
		t.Fatalf("expected ErrPipelineNotFound, got %v", err)
	}
}

// TestStopPipelineFailureKeepsPipeline covers a failed shutdown: the
// pipeline stays queryable, the retry with the same id succeeds and returns
// the url, and only then is the id removed.
func TestStopPipelineFailureKeepsPipeline(t *testing.T) {
	s := newTestStore()
	p := &fakePipeline{errs: []error{errors.New("transient finalize failure")}}
	s.AddPipeLine("pipeline_1", p)

	if _, err := s.StopPipeline("pipeline_1"); err == nil {
		t.Fatal("first StopPipeline should have failed")
	}

	if _, ok := s.GetPipeline("pipeline_1"); !ok {
		t.Fatal("pipeline must stay in the store after a failed stop")
	}

	resp, err := s.StopPipeline("pipeline_1")
	if err != nil {
		t.Fatalf("retried StopPipeline failed: %v", err)
	}
	if resp == nil || resp.Recording_Url == nil {
		t.Fatal("retried StopPipeline returned no recording url")
	}

	if _, ok := s.GetPipeline("pipeline_1"); ok {
		t.Fatal("pipeline must be removed after the retried stop succeeds")
	}

	_, err = s.StopPipeline("pipeline_1")
	if !errors.Is(err, store.ErrPipelineNotFound) {
		t.Fatalf("expected ErrPipelineNotFound after removal, got %v", err)
	}
}

// gatedPipeline blocks its Stop until all expected callers have entered it,
// simulating a stop that is still in progress while more stop requests for
// the same id arrive.
type gatedPipeline struct {
	entered   chan struct{}
	release   chan struct{}
	finalizes int
	mu        sync.Mutex
}

func newGatedPipeline(callers int) *gatedPipeline {
	return &gatedPipeline{
		entered: make(chan struct{}, callers),
		release: make(chan struct{}),
	}
}

func (g *gatedPipeline) Stop() (*cloud.CloudUploadPartCompleted, error) {
	g.entered <- struct{}{}
	<-g.release

	g.mu.Lock()
	g.finalizes++
	g.mu.Unlock()

	url := "https://bucket/recording.mp4"
	return &cloud.CloudUploadPartCompleted{Recording_Url: &url}, nil
}

// TestStopPipelineConcurrent covers stop requests that arrive while a stop
// of the same id is still in progress: none of them may see "not found",
// they all observe the successful result, and the pipeline is removed
// afterwards.
func TestStopPipelineConcurrent(t *testing.T) {
	s := newTestStore()

	const callers = 8

	p := newGatedPipeline(callers)
	s.AddPipeLine("pipeline_1", p)

	results := make([]*cloud.CloudUploadPartCompleted, callers)
	errs := make([]error, callers)

	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = s.StopPipeline("pipeline_1")
		}(i)
	}

	// Wait until every caller has found the pipeline and is blocked inside
	// its in-progress stop, then let the stop complete.
	for i := 0; i < callers; i++ {
		<-p.entered
	}
	close(p.release)

	wg.Wait()

	for i := 0; i < callers; i++ {
		if errs[i] != nil {
			t.Fatalf("concurrent StopPipeline %d failed: %v", i, errs[i])
		}
		if results[i] == nil || results[i].Recording_Url == nil {
			t.Fatalf("concurrent StopPipeline %d returned no recording url", i)
		}
		if *results[i].Recording_Url != "https://bucket/recording.mp4" {
			t.Fatalf("concurrent StopPipeline %d got unexpected url %q", i, *results[i].Recording_Url)
		}
	}

	if _, ok := s.GetPipeline("pipeline_1"); ok {
		t.Fatal("pipeline must be removed after a successful stop")
	}

	_, err := s.StopPipeline("pipeline_1")
	if !errors.Is(err, store.ErrPipelineNotFound) {
		t.Fatalf("expected ErrPipelineNotFound after removal, got %v", err)
	}
}
