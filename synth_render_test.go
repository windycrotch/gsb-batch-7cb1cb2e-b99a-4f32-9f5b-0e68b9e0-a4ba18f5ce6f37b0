package gotau_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"

	"github.com/SladkyCitron/gotau"
	"github.com/SladkyCitron/gotau/cache"
	"github.com/SladkyCitron/gotau/cache/diskcache"
	"github.com/SladkyCitron/gotau/cache/memcache"
	"github.com/SladkyCitron/gotau/concat"
	"github.com/SladkyCitron/gotau/resampler"
	"github.com/SladkyCitron/gotau/sequence"
	"github.com/SladkyCitron/gotau/voicebank"
	"github.com/SladkyCitron/resona/afmt"
	"github.com/SladkyCitron/resona/aio"
	"github.com/SladkyCitron/resona/codec/wav"
	"github.com/SladkyCitron/resona/freq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testSampleRate = 44100

// testNoteSamples is the audio data the stub resampler "renders" for every note.
var testNoteSamples = func() []float32 {
	s := make([]float32, 24000)
	for i := range s {
		s[i] = float32(i%200) / 400
	}
	return s
}()

func encodeWavBytes(t *testing.T, samples []float32) []byte {
	t.Helper()
	// encode via a real file: wav encoders patch the header sizes on Close,
	// which requires a seekable writer
	f, err := os.CreateTemp(t.TempDir(), "samples-*.wav")
	require.NoError(t, err)
	defer f.Close()
	enc, err := wav.NewEncoder(
		f,
		afmt.Format{SampleRate: freq.Frequency(testSampleRate) * freq.Hertz, NumChannels: 1},
		afmt.SampleFormat{BitDepth: 32, Encoding: afmt.SampleEncodingFloat, Endian: binary.LittleEndian},
		wav.FormatFloat,
	)
	require.NoError(t, err)
	_, err = enc.WriteSamples(samples)
	require.NoError(t, err)
	require.NoError(t, enc.Close())
	data, err := os.ReadFile(f.Name())
	require.NoError(t, err)
	return data
}

// stubResampler is a resampler that always returns the same pre-rendered
// samples and counts how many times it was invoked.
type stubResampler struct {
	calls   atomic.Int32
	wavData []byte
}

func newStubResampler(t *testing.T, samples []float32) *stubResampler {
	t.Helper()
	return &stubResampler{wavData: encodeWavBytes(t, samples)}
}

func (r *stubResampler) ID() string { return "stub-resampler" }

func (r *stubResampler) Resample(_ aio.SampleReader, _ resampler.ResampleConfig) (aio.SampleReader, error) {
	r.calls.Add(1)
	return wav.NewDecoder(bytes.NewReader(r.wavData))
}

func testVoicebank(t *testing.T) *voicebank.Voicebank {
	t.Helper()
	vb, err := voicebank.Open(fstest.MapFS{
		"oto.ini": &fstest.MapFile{Data: []byte("a.wav=a,0,80,50,10,5\n")},
		"a.wav":   &fstest.MapFile{Data: encodeWavBytes(t, make([]float32, testSampleRate/2))},
	})
	require.NoError(t, err)
	return vb
}

var testNotes = []sequence.Note{
	{Position: 0, Duration: 480, Lyric: "a", Note: 60, Intensity: 1, Velocity: 100},
	{Position: 480, Duration: 240, Lyric: "a", Note: 62, Intensity: 1, Velocity: 100},
}

func newTestSynth(t *testing.T, res resampler.Resampler, c cache.Cache) *gotau.Synth {
	t.Helper()
	s := gotau.New(testSampleRate, testVoicebank(t), res, &concat.Append{})
	if c != nil {
		s.SetResamplerCache(c)
	}
	s.EnqueueSequence(sequence.Sequence{
		Metadata: sequence.Metadata{Resolution: 480, Tempo: 120},
		Notes:    testNotes,
	})
	return s
}

// readAllSamples reads the synth to completion. Non-EOF errors are recorded
// (the first one is returned) but reading continues: already-rendered audio
// must remain readable even after a rendering error is reported.
func readAllSamples(t *testing.T, s *gotau.Synth) ([]float32, error) {
	t.Helper()
	var out []float32
	var firstErr error
	buf := make([]float32, 4096)
	for i := 0; i < 10000; i++ {
		n, err := s.ReadSamples(buf)
		out = append(out, buf[:n]...)
		if err == nil {
			continue
		}
		if errors.Is(err, io.EOF) {
			return out, firstErr
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	t.Fatal("ReadSamples did not terminate")
	return nil, nil
}

// TestSynthCacheHitAndMissProduceSameSamples verifies that rendering the same
// score twice (cache miss, then cache hit) produces identical samples and
// that the resampler is not invoked again on a cache hit.
func TestSynthCacheHitAndMissProduceSameSamples(t *testing.T) {
	c := memcache.New()

	res1 := newStubResampler(t, testNoteSamples)
	samples1, err := readAllSamples(t, newTestSynth(t, res1, c))
	require.NoError(t, err)
	require.NotEmpty(t, samples1)
	assert.Equal(t, int32(len(testNotes)), res1.calls.Load())

	// render the same score again with a fresh synth sharing the cache:
	// everything must come from the cache and produce identical samples
	res2 := newStubResampler(t, testNoteSamples)
	samples2, err := readAllSamples(t, newTestSynth(t, res2, c))
	require.NoError(t, err)
	assert.Equal(t, int32(0), res2.calls.Load())
	assert.Equal(t, samples1, samples2)
}

// failCreateCache simulates a cache whose Create always fails.
type failCreateCache struct{ inner cache.Cache }

func (c *failCreateCache) Open(ctx context.Context, key cache.KeyFunc) (io.ReadCloser, error) {
	return c.inner.Open(ctx, key)
}

func (c *failCreateCache) Create(context.Context, cache.KeyFunc) (cache.BlobWriter, error) {
	return nil, errors.New("injected cache create failure")
}

// failWriteCache simulates a cache whose writes fail mid-way.
type failWriteCache struct{ inner cache.Cache }

func (c *failWriteCache) Open(ctx context.Context, key cache.KeyFunc) (io.ReadCloser, error) {
	return c.inner.Open(ctx, key)
}

func (c *failWriteCache) Create(context.Context, cache.KeyFunc) (cache.BlobWriter, error) {
	return &failBlobWriter{}, nil
}

type failBlobWriter struct{}

func (w *failBlobWriter) Write([]byte) (int, error)      { return 0, errors.New("injected cache write failure") }
func (w *failBlobWriter) Seek(int64, int) (int64, error) { return 0, nil }
func (w *failBlobWriter) Close() error                   { return nil }
func (w *failBlobWriter) Abort() error                   { return nil }

// TestSynthCacheCreateFailurePreservesAudio verifies that a failing cache
// Create neither crashes the render nor loses the concatenated note, while
// the caller still perceives the failure.
func TestSynthCacheCreateFailurePreservesAudio(t *testing.T) {
	testSynthCacheFailurePreservesAudio(t, &failCreateCache{inner: memcache.New()})
}

// TestSynthCacheWriteFailurePreservesAudio verifies the same for a cache
// whose writes fail mid-way.
func TestSynthCacheWriteFailurePreservesAudio(t *testing.T) {
	testSynthCacheFailurePreservesAudio(t, &failWriteCache{inner: memcache.New()})
}

func testSynthCacheFailurePreservesAudio(t *testing.T, c cache.Cache) {
	t.Helper()

	// reference render without cache failures
	refRes := newStubResampler(t, testNoteSamples)
	ref, err := readAllSamples(t, newTestSynth(t, refRes, memcache.New()))
	require.NoError(t, err)
	require.NotEmpty(t, ref)

	res := newStubResampler(t, testNoteSamples)
	s := newTestSynth(t, res, c)

	// the caller must perceive the cache failure in this very read...
	buf := make([]float32, 4096)
	_, err = s.ReadSamples(buf)
	require.Error(t, err)

	// ...but the already-concatenated notes must not be lost
	var out []float32
	for i := 0; i < 10000; i++ {
		n, err := s.ReadSamples(buf)
		out = append(out, buf[:n]...)
		if err == nil {
			continue
		}
		if errors.Is(err, io.EOF) {
			break
		}
	}
	assert.Equal(t, ref, out)
	// the notes must not be re-rendered just because caching failed
	assert.Equal(t, int32(len(testNotes)), res.calls.Load())
}

// corruptCache simulates a cache containing invalid (e.g. half-written)
// entries: Open always returns garbage data.
type corruptCache struct{ inner cache.Cache }

func (c *corruptCache) Open(context.Context, cache.KeyFunc) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("half-written cache entry")), nil
}

func (c *corruptCache) Create(ctx context.Context, key cache.KeyFunc) (cache.BlobWriter, error) {
	return c.inner.Create(ctx, key)
}

// TestSynthInvalidCacheEntryIsReplaced verifies that an invalid cached entry
// is not used as a cache hit: the note is re-rendered and the entry replaced,
// so subsequent renders can reuse it.
func TestSynthInvalidCacheEntryIsReplaced(t *testing.T) {
	inner := memcache.New()

	// first render: the invalid entry must not be used; the notes are
	// re-rendered and the entries replaced
	res1 := newStubResampler(t, testNoteSamples)
	samples1, err := readAllSamples(t, newTestSynth(t, res1, &corruptCache{inner: inner}))
	require.NoError(t, err)
	require.NotEmpty(t, samples1)
	assert.Equal(t, int32(len(testNotes)), res1.calls.Load())

	// second render: the replaced entries are valid now and must be reused,
	// producing identical samples
	res2 := newStubResampler(t, testNoteSamples)
	samples2, err := readAllSamples(t, newTestSynth(t, res2, inner))
	require.NoError(t, err)
	assert.Equal(t, int32(0), res2.calls.Load())
	assert.Equal(t, samples1, samples2)
}

// TestSynthDefaultCacheRenders verifies that rendering with the default
// (no-op) cache succeeds.
func TestSynthDefaultCacheRenders(t *testing.T) {
	res := newStubResampler(t, testNoteSamples)
	samples, err := readAllSamples(t, newTestSynth(t, res, nil))
	require.NoError(t, err)
	assert.NotEmpty(t, samples)
}

// TestSynthDiskCacheInterruptedWriteRecovery verifies the end-to-end recovery
// flow with an on-disk cache: an interrupted cache write leaves a half-written
// entry that must not be used as a valid cache hit; re-rendering the same
// score replaces it and produces the same samples as the first successful
// render.
func TestSynthDiskCacheInterruptedWriteRecovery(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")

	render := func(t *testing.T) (*stubResampler, []float32, error) {
		t.Helper()
		res := newStubResampler(t, testNoteSamples)
		s := newTestSynth(t, res, diskcache.New(dir, gotau.ResamplerDiskCacheExt))
		samples, err := readAllSamples(t, s)
		return res, samples, err
	}

	// first successful render populates the cache
	res1, ref, err := render(t)
	require.NoError(t, err)
	require.NotEmpty(t, ref)
	require.Equal(t, int32(len(testNotes)), res1.calls.Load())

	// simulate an interrupted cache write: every cached entry is left
	// half-written on disk
	var cacheFiles []string
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, gotau.ResamplerDiskCacheExt) {
			cacheFiles = append(cacheFiles, path)
		}
		return nil
	})
	require.NoError(t, err)
	require.NotEmpty(t, cacheFiles)
	for _, p := range cacheFiles {
		require.NoError(t, os.WriteFile(p, []byte("half-written"), 0666))
	}

	// re-render: the half-written entries must not be used as valid cache;
	// they get replaced by this render
	res2, samples2, err := render(t)
	require.NoError(t, err)
	assert.Equal(t, int32(len(testNotes)), res2.calls.Load())
	assert.Equal(t, ref, samples2)

	// re-render again: the replaced entries are complete now and reused
	res3, samples3, err := render(t)
	require.NoError(t, err)
	assert.Equal(t, int32(0), res3.calls.Load())
	assert.Equal(t, ref, samples3)
}
