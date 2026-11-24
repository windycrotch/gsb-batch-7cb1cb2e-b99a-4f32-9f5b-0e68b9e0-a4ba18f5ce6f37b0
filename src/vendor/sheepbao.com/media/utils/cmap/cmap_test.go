package cmap

import (
	"sync"
	"testing"
)

func TestRemoveIfOnlyRemovesMatchingValue(t *testing.T) {
	m := New()
	old := &struct{ n int }{1}
	cur := &struct{ n int }{2}

	m.Set("k", old)
	if !m.RemoveIf("k", old) {
		t.Fatal("RemoveIf should remove the matching value")
	}
	if m.Has("k") {
		t.Fatal("key still present after RemoveIf")
	}

	// A stale reference must not delete a newer value.
	m.Set("k", cur)
	if m.RemoveIf("k", old) {
		t.Fatal("RemoveIf removed a value it does not own")
	}
	if v, ok := m.Get("k"); !ok || v != cur {
		t.Fatal("newer value was disturbed by stale RemoveIf")
	}

	// Missing key is a no-op.
	if m.RemoveIf("absent", old) {
		t.Fatal("RemoveIf reported removal for an absent key")
	}
}

func TestRemoveIfConcurrent(t *testing.T) {
	m := New()
	values := make([]*struct{ n int }, 32)
	for i := range values {
		values[i] = &struct{ n int }{i}
	}
	m.Set("k", values[len(values)-1])

	var wg sync.WaitGroup
	for _, v := range values {
		wg.Add(1)
		go func(v *struct{ n int }) {
			defer wg.Done()
			m.RemoveIf("k", v)
		}(v)
	}
	wg.Wait()

	// Only the newest value was removable, so it is gone and nothing races.
	if m.Has("k") {
		t.Fatal("expected newest value to be removed exactly once")
	}
}
