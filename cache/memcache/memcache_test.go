package memcache_test

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/SladkyCitron/gotau/cache"
	"github.com/SladkyCitron/gotau/cache/memcache"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func keyFunc(s string) cache.KeyFunc {
	return func(w io.Writer) { _, _ = io.WriteString(w, s) }
}

func TestMemCacheRoundTrip(t *testing.T) {
	c := memcache.New()
	ctx := context.Background()

	w, err := c.Create(ctx, keyFunc("note"))
	require.NoError(t, err)
	_, err = w.Write([]byte("cached note data"))
	require.NoError(t, err)
	require.NoError(t, w.Close())

	rc, err := c.Open(ctx, keyFunc("note"))
	require.NoError(t, err)
	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	assert.Equal(t, []byte("cached note data"), data)
}

func TestMemCacheOpenNotFound(t *testing.T) {
	c := memcache.New()
	_, err := c.Open(context.Background(), keyFunc("missing"))
	assert.ErrorIs(t, err, cache.ErrNotFound)
}

// TestMemCacheAbortDiscardsBlob verifies that aborting a write discards the
// blob instead of committing a half-written one.
func TestMemCacheAbortDiscardsBlob(t *testing.T) {
	c := memcache.New()
	ctx := context.Background()

	w, err := c.Create(ctx, keyFunc("note"))
	require.NoError(t, err)
	_, err = w.Write([]byte("half-written"))
	require.NoError(t, err)
	require.NoError(t, w.Abort())

	_, err = c.Open(ctx, keyFunc("note"))
	assert.ErrorIs(t, err, cache.ErrNotFound)
}

// TestMemCacheConcurrentSameKey verifies that concurrent renders of the same
// note never let a reader observe a half-written cache entry: readers see
// either no entry at all or a complete one.
func TestMemCacheConcurrentSameKey(t *testing.T) {
	c := memcache.New()
	ctx := context.Background()
	key := keyFunc("note")
	payload := []byte("complete cached note data")

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				w, err := c.Create(ctx, key)
				if !assert.NoError(t, err) {
					return
				}
				if _, err := w.Write(payload); !assert.NoError(t, err) {
					return
				}
				if !assert.NoError(t, w.Close()) {
					return
				}
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				rc, err := c.Open(ctx, key)
				if errors.Is(err, cache.ErrNotFound) {
					continue
				}
				if !assert.NoError(t, err) {
					return
				}
				data, err := io.ReadAll(rc)
				_ = rc.Close()
				if !assert.NoError(t, err) {
					return
				}
				// a concurrent reader must never observe a half-written entry
				assert.Equal(t, payload, data)
			}
		}()
	}
	wg.Wait()
}
