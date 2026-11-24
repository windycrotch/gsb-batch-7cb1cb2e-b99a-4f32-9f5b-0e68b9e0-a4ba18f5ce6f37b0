package diskcache_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/SladkyCitron/gotau/cache"
	"github.com/SladkyCitron/gotau/cache/diskcache"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func keyFunc(s string) cache.KeyFunc {
	return func(w io.Writer) { _, _ = io.WriteString(w, s) }
}

func newCache(t *testing.T) (*diskcache.Cache, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "cache")
	return diskcache.New(dir, ".wav"), dir
}

func writeBlob(t *testing.T, c *diskcache.Cache, key string, data []byte) {
	t.Helper()
	w, err := c.Create(context.Background(), keyFunc(key))
	require.NoError(t, err)
	_, err = w.Write(data)
	require.NoError(t, err)
	require.NoError(t, w.Close())
}

func listFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			files = append(files, path)
		}
		return nil
	})
	require.NoError(t, err)
	return files
}

func TestDiskCacheRoundTrip(t *testing.T) {
	c, _ := newCache(t)
	writeBlob(t, c, "note", []byte("cached note data"))

	rc, err := c.Open(context.Background(), keyFunc("note"))
	require.NoError(t, err)
	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	assert.Equal(t, []byte("cached note data"), data)
}

func TestDiskCacheOpenNotFound(t *testing.T) {
	c, _ := newCache(t)
	_, err := c.Open(context.Background(), keyFunc("missing"))
	assert.ErrorIs(t, err, cache.ErrNotFound)
}

// TestDiskCacheAbortLeavesNoResidue verifies that aborting a write discards
// the entry and leaves no files behind.
func TestDiskCacheAbortLeavesNoResidue(t *testing.T) {
	c, dir := newCache(t)

	w, err := c.Create(context.Background(), keyFunc("aborted"))
	require.NoError(t, err)
	_, err = w.Write([]byte("partial data"))
	require.NoError(t, err)
	require.NoError(t, w.Abort())

	_, err = c.Open(context.Background(), keyFunc("aborted"))
	assert.ErrorIs(t, err, cache.ErrNotFound)
	assert.Empty(t, listFiles(t, dir))
}

// TestDiskCacheInterruptedWriteIsReplaced verifies that a write that was
// interrupted halfway (neither committed nor aborted, e.g. due to
// cancellation or a crash) is never served as a valid cache entry and is
// replaced by the next successful write for the same key.
func TestDiskCacheInterruptedWriteIsReplaced(t *testing.T) {
	c, _ := newCache(t)
	ctx := context.Background()

	w, err := c.Create(ctx, keyFunc("note"))
	require.NoError(t, err)
	_, err = w.Write([]byte("half-written"))
	require.NoError(t, err)
	// simulate cancellation/crash: neither Close nor Abort is called
	// (abort only during cleanup to release the file handle)
	t.Cleanup(func() { _ = w.Abort() })

	// the half-written entry must not be visible as valid cache
	_, err = c.Open(ctx, keyFunc("note"))
	assert.ErrorIs(t, err, cache.ErrNotFound)

	// re-rendering the same note replaces the interrupted write
	writeBlob(t, c, "note", []byte("complete"))

	rc, err := c.Open(ctx, keyFunc("note"))
	require.NoError(t, err)
	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	assert.Equal(t, []byte("complete"), data)
}

// TestDiskCacheOverwrite verifies that creating an entry that already exists
// atomically replaces it.
func TestDiskCacheOverwrite(t *testing.T) {
	c, _ := newCache(t)
	writeBlob(t, c, "note", []byte("first"))
	writeBlob(t, c, "note", []byte("second"))

	rc, err := c.Open(context.Background(), keyFunc("note"))
	require.NoError(t, err)
	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	assert.Equal(t, []byte("second"), data)
}

// TestDiskCacheConcurrentSameKey verifies that concurrent renders of the same
// note never let a reader observe a half-written cache entry: readers see
// either no entry at all or a complete one.
func TestDiskCacheConcurrentSameKey(t *testing.T) {
	c, _ := newCache(t)
	ctx := context.Background()
	key := keyFunc("concurrent-note")
	payload := []byte("complete cached note data")

	const writers = 4
	const readers = 4
	const iterations = 50

	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				w, err := c.Create(ctx, key)
				if !assert.NoError(t, err) {
					return
				}
				if _, err := w.Write(payload); !assert.NoError(t, err) {
					return
				}
				if err := w.Close(); err != nil {
					// On Windows, the atomic rename can fail if a concurrent
					// reader still has the previous entry open. This surfaces
					// as a cache write error to the caller (which keeps the
					// rendered audio), but never as a corrupt entry.
					if runtime.GOOS != "windows" {
						t.Errorf("unexpected close error: %v", err)
					}
					return
				}
			}
		}()
	}
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				rc, err := c.Open(ctx, key)
				if err != nil {
					if errors.Is(err, cache.ErrNotFound) {
						continue
					}
					if runtime.GOOS == "windows" {
						// a concurrent atomic rename can transiently lock the
						// entry on Windows; callers treat any Open error as a
						// cache miss, so this is harmless in practice
						continue
					}
					if !assert.NoError(t, err) {
						return
					}
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

// TestDiskCacheConcurrentDifferentKeys verifies that concurrent renders of
// different notes don't interfere with each other's cache entries.
func TestDiskCacheConcurrentDifferentKeys(t *testing.T) {
	c, _ := newCache(t)
	ctx := context.Background()

	const workers = 8
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		key := fmt.Sprintf("note-%d", i)
		payload := []byte("payload-" + key)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				w, err := c.Create(ctx, keyFunc(key))
				if !assert.NoError(t, err) {
					return
				}
				if _, err := w.Write(payload); !assert.NoError(t, err) {
					return
				}
				if !assert.NoError(t, w.Close()) {
					return
				}

				rc, err := c.Open(ctx, keyFunc(key))
				if !assert.NoError(t, err) {
					return
				}
				data, err := io.ReadAll(rc)
				_ = rc.Close()
				if !assert.NoError(t, err) {
					return
				}
				assert.Equal(t, payload, data)
			}
		}()
	}
	wg.Wait()
}
