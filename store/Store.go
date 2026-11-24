package store

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/OmGuptaIND/cloud"
)

// ContextKey is the type of the context keys used by this package.
type ContextKey string

// StoreKey is the context key under which an AppStore is stored.
const StoreKey ContextKey = "store"

// ErrPipelineNotFound is returned when no pipeline exists for a given id.
var ErrPipelineNotFound = errors.New("pipeline not found")

// Pipeline is the minimal behavior the store needs to stop a pipeline.
type Pipeline interface {
	Stop() (*cloud.CloudUploadPartCompleted, error)
}

var store *AppStore

type AppStore struct {
	mu        sync.RWMutex
	Pipelines map[string]Pipeline
}

// GetStore retrieves the store from the context, if ctx is nil it returns the global store.
func GetStore(ctx *context.Context) *AppStore {
	if ctx == nil {
		return store
	}

	ctxStore, _ := (*ctx).Value(StoreKey).(*AppStore)

	return ctxStore
}

// NewStore creates a new store.
func NewStore() *AppStore {
	if store != nil {
		return store
	}

	store = &AppStore{
		Pipelines: make(map[string]Pipeline),
	}

	return store
}

// AddPipeLine adds a recording to the store.
func (s *AppStore) AddPipeLine(id string, p Pipeline) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.Pipelines[id] = p
}

// GetPipeline retrieves a recording from the store.
func (s *AppStore) GetPipeline(id string) (Pipeline, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	r, ok := s.Pipelines[id]
	return r, ok
}

// RemovePipeline removes a recording from the store.
func (s *AppStore) RemovePipeline(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.Pipelines, id)
}

// StopPipeline stops the pipeline with the given id and removes it from the
// store, but only after the stop has fully succeeded.
//
// If no pipeline exists for the id, ErrPipelineNotFound is returned. If the
// stop fails, the pipeline stays in the store so the caller can retry with
// the same id. A stop that arrives while another stop for the same id is
// still in progress finds the pipeline in the store and waits on the
// pipeline's own Stop, so it never sees ErrPipelineNotFound nor triggers a
// second finalization.
func (s *AppStore) StopPipeline(id string) (*cloud.CloudUploadPartCompleted, error) {
	p, ok := s.GetPipeline(id)

	if !ok {
		return nil, ErrPipelineNotFound
	}

	resp, err := p.Stop()

	if err != nil {
		return nil, err
	}

	if resp == nil || resp.Recording_Url == nil {
		return nil, fmt.Errorf("pipeline %s stopped without a recording url", id)
	}

	s.RemovePipeline(id)

	return resp, nil
}

// ListPipelines lists all recordings in the store.
func (s *AppStore) ListPipelines() map[string]Pipeline {
	s.mu.RLock()
	defer s.mu.RUnlock()

	pipelines := make(map[string]Pipeline, len(s.Pipelines))
	for k, v := range s.Pipelines {
		pipelines[k] = v
	}

	return pipelines
}
