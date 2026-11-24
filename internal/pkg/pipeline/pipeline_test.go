package pipeline_test

import (
	"io/ioutil"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/sfluor/musig/internal/pkg/db"
	"github.com/sfluor/musig/internal/pkg/model"
	"github.com/sfluor/musig/internal/pkg/pipeline"
	"github.com/sfluor/musig/pkg/dsp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countFingerprints opens the raw fingerprint bucket through the Database
// interface helpers so the tests do not depend on bbolt internals.
func fingerprintCount(t *testing.T, p *pipeline.Pipeline) int {
	t.Helper()
	bdb, ok := p.DB.(*db.BoltDB)
	require.True(t, ok, "expected a *db.BoltDB")
	n, err := bdb.FingerprintCount()
	require.NoError(t, err)
	return n
}

// queryScore runs the same matching logic as the read command for the given
// sample file against the songs currently stored in the pipeline's database.
func queryScore(t *testing.T, p *pipeline.Pipeline, samplePath string, songID uint32) float64 {
	t.Helper()

	res, err := p.Process(samplePath)
	require.NoError(t, err)

	keys := make([]model.EncodedKey, 0, len(res.Fingerprint))
	for k := range res.Fingerprint {
		keys = append(keys, k)
	}

	stored, err := p.DB.Get(keys)
	require.NoError(t, err)

	points := map[model.EncodedKey]model.TableValue{}
	for key, values := range stored {
		for _, v := range values {
			if v.SongID == songID {
				points[key] = v
			}
		}
	}
	// With no overlapping key the correlation is undefined: a zero score is
	// the meaningful "no match" answer.
	if len(points) == 0 {
		return 0
	}
	return dsp.MatchScore(res.Fingerprint, points)
}

func TestSyntheticWAVProducesFingerprints(t *testing.T) {
	dir := mustTempDir(t, "musig-smoke-")
	defer os.RemoveAll(dir)

	path440 := join(dir, "tone440.wav")
	pathMix := join(dir, "tonemix.wav")
	writeTestWAV(t, path440, 440, 660)
	writeTestWAV(t, pathMix, 440, 880)

	fpr440 := fingerprintWAV(t, path440)
	fprMix := fingerprintWAV(t, pathMix)

	require.NotEmpty(t, fpr440, "the synthesized WAV must produce a non empty fingerprint")
	require.NotEmpty(t, fprMix)
	assert.NotEqual(t, len(fpr440), 0)
}

func TestProcessAndStoreRepeatedLoadIsIdempotent(t *testing.T) {
	dir := mustTempDir(t, "musig-idem-")
	defer os.RemoveAll(dir)

	dbFile := join(dir, "musig.db")
	wavPath := join(dir, "tone440.wav")
	writeTestWAV(t, wavPath, 440, 880)

	p, err := pipeline.NewDefaultPipeline(dbFile)
	require.NoError(t, err)
	defer p.Close()

	res1, err := p.ProcessAndStore(wavPath)
	require.NoError(t, err)
	require.NotZero(t, res1.SongID)
	firstCount := fingerprintCount(t, p)
	require.NotZero(t, firstCount)

	// Load the exact same file several more times.
	for i := 0; i < 3; i++ {
		res, err := p.ProcessAndStore(wavPath)
		require.NoError(t, err)
		assert.Equal(t, res1.SongID, res.SongID, "song name to ID mapping must stay stable")
		assert.Equal(t, firstCount, fingerprintCount(t, p), "reload #%d added fingerprint entries", i)
	}

	bdb := p.DB.(*db.BoltDB)
	songCount, err := bdb.SongFingerprintCount(res1.SongID)
	require.NoError(t, err)
	assert.Equal(t, firstCount, songCount)

	// Queries still match the song.
	score := queryScore(t, p, wavPath, res1.SongID)
	assert.True(t, score > 0, "the stored song must still be queryable")
}

func TestProcessAndStoreContentChangeKeepsIdentityAndRetiresOld(t *testing.T) {
	dir := mustTempDir(t, "musig-change-")
	defer os.RemoveAll(dir)

	dbFile := join(dir, "musig.db")
	// Same file name, two different contents over time.
	wavPath := join(dir, "changing.wav")
	writeTestWAV(t, wavPath, 440, 880)

	p, err := pipeline.NewDefaultPipeline(dbFile)
	require.NoError(t, err)
	defer p.Close()

	resV1, err := p.ProcessAndStore(wavPath)
	require.NoError(t, err)
	id := resV1.SongID
	countV1 := fingerprintCount(t, p)
	require.NotZero(t, countV1)

	// A second, distinct song to make sure its fingerprints are untouched.
	otherPath := join(dir, "other.wav")
	writeTestWAV(t, otherPath, 1200, 2400)
	resOther, err := p.ProcessAndStore(otherPath)
	require.NoError(t, err)
	require.NotEqual(t, id, resOther.SongID)
	countAfterOther := fingerprintCount(t, p)

	// Replace the content behind the same file name.
	writeTestWAV(t, wavPath, 1800, 3200)
	resV2, err := p.ProcessAndStore(wavPath)
	require.NoError(t, err)

	assert.Equal(t, id, resV2.SongID, "the song name must keep its identity across content changes")

	bdb := p.DB.(*db.BoltDB)

	// Old fingerprints no longer participate in matching.
	oldCount, err := bdb.SongFingerprintCount(id)
	require.NoError(t, err)
	assert.Equal(t, len(resV2.Fingerprint), oldCount)

	// The other song is fully preserved.
	otherCount, err := bdb.SongFingerprintCount(resOther.SongID)
	require.NoError(t, err)
	assert.Equal(t, countAfterOther-countV1, otherCount, "the unrelated song must not be modified")

	// Every stored value for the changed song belongs to the new version:
	// querying with the new content matches, querying with the old content
	// yields no points for the song.
	newScore := queryScore(t, p, wavPath, id)
	assert.True(t, newScore > 0, "the new content must match the song")

	oldContentPath := join(dir, "old-version.wav")
	writeTestWAV(t, oldContentPath, 440, 880)
	oldScore := queryScore(t, p, oldContentPath, id)
	assert.Zero(t, oldScore, "the retired fingerprint version must not match anymore")

	// The stored content hash reflects the latest successful load.
	hashNow, err := bdb.SongContentHash(id)
	require.NoError(t, err)
	require.NotEmpty(t, hashNow)

	// Reloading the new content again is a no-op.
	countBeforeReload := fingerprintCount(t, p)
	resV3, err := p.ProcessAndStore(wavPath)
	require.NoError(t, err)
	assert.Equal(t, id, resV3.SongID)
	assert.Equal(t, countBeforeReload, fingerprintCount(t, p))
}

// failingDB wraps a real database and makes StoreSong fail on demand,
// simulating a fingerprint write failure at the pipeline level.
type failingDB struct {
	db.Database
	failNext bool
	mu       sync.Mutex
}

func (f *failingDB) StoreSong(name string, hash []byte, fpr map[model.EncodedKey]model.TableValue) (uint32, bool, error) {
	f.mu.Lock()
	shouldFail := f.failNext
	f.failNext = false
	f.mu.Unlock()

	if shouldFail {
		return 0, false, assert.AnError
	}
	return f.Database.StoreSong(name, hash, fpr)
}

func TestProcessAndStoreWriteFailureRecovery(t *testing.T) {
	dir := mustTempDir(t, "musig-fail-")
	defer os.RemoveAll(dir)

	dbFile := join(dir, "musig.db")
	wavPath := join(dir, "tone.wav")
	writeTestWAV(t, wavPath, 660, 990)

	bdb, err := db.NewBoltDB(dbFile)
	require.NoError(t, err)

	wrapped := &failingDB{Database: bdb}
	p := pipeline.NewPipeline(
		newTestSpectrogrammer(),
		wrapped,
		newTestFingerprinter(),
	)
	defer p.Close()

	// First load fails: no song name may point at an ID without fingerprints.
	wrapped.failNext = true
	_, err = p.ProcessAndStore(wavPath)
	require.Error(t, err)

	count, err := bdb.FingerprintCount()
	require.NoError(t, err)
	assert.Zero(t, count, "a failed load must not leave partial fingerprints")

	_, err = bdb.GetSongID("tone")
	assert.Error(t, err, "a failed load must not register the song name")

	// Reloading the same file after the failure recovers to a consistent state.
	res, err := p.ProcessAndStore(wavPath)
	require.NoError(t, err)
	require.NotZero(t, res.SongID)

	count, err = bdb.FingerprintCount()
	require.NoError(t, err)
	assert.Equal(t, len(res.Fingerprint), count)

	name, err := bdb.GetSong(res.SongID)
	require.NoError(t, err)
	assert.Equal(t, "tone", name)

	// And another failure on an *update* (content change) rolls back as well.
	writeTestWAV(t, wavPath, 990, 1480)
	wrapped.failNext = true
	_, err = p.ProcessAndStore(wavPath)
	require.Error(t, err)

	countAfterFailedUpdate, err := bdb.FingerprintCount()
	require.NoError(t, err)
	assert.Equal(t, count, countAfterFailedUpdate, "the previous fingerprint set must survive a failed update")

	// Recovery after the failed update.
	res2, err := p.ProcessAndStore(wavPath)
	require.NoError(t, err)
	assert.Equal(t, res.SongID, res2.SongID, "identity must be preserved after recovery")

	score := queryScore(t, p, wavPath, res2.SongID)
	assert.True(t, score > 0)
}

func TestProcessAndStoreInvariantsSurviveReopen(t *testing.T) {
	dir := mustTempDir(t, "musig-reopen-")
	defer os.RemoveAll(dir)

	dbFile := join(dir, "musig.db")
	wavPath := join(dir, "reopen.wav")
	writeTestWAV(t, wavPath, 440, 880)

	p, err := pipeline.NewDefaultPipeline(dbFile)
	require.NoError(t, err)

	res, err := p.ProcessAndStore(wavPath)
	require.NoError(t, err)
	id := res.SongID
	countBefore := fingerprintCount(t, p)
	p.Close()

	// Reopen the database: every invariant must still hold.
	p2, err := pipeline.NewDefaultPipeline(dbFile)
	require.NoError(t, err)
	defer p2.Close()

	gotID, err := p2.DB.GetSongID("reopen")
	require.NoError(t, err)
	assert.Equal(t, id, gotID)

	assert.Equal(t, countBefore, fingerprintCount(t, p2))

	// Idempotent reload across processes/handles.
	res2, err := p2.ProcessAndStore(wavPath)
	require.NoError(t, err)
	assert.Equal(t, id, res2.SongID)
	assert.Equal(t, countBefore, fingerprintCount(t, p2))

	// Content change after reopen still retires the old version.
	writeTestWAV(t, wavPath, 1500, 2700)
	res3, err := p2.ProcessAndStore(wavPath)
	require.NoError(t, err)
	assert.Equal(t, id, res3.SongID)
	assert.Equal(t, len(res3.Fingerprint), fingerprintCount(t, p2))

	score := queryScore(t, p2, wavPath, id)
	assert.True(t, score > 0)
}

func TestProcessAndStoreConcurrentLoads(t *testing.T) {
	dir := mustTempDir(t, "musig-concurrent-")
	defer os.RemoveAll(dir)

	dbFile := join(dir, "musig.db")

	// Several distinct songs...
	files := map[string][]float64{
		"alpha.wav":   {330, 660},
		"bravo.wav":   {770, 1540},
		"charlie.wav": {1320, 2640},
	}
	paths := make([]string, 0, len(files))
	for name, freqs := range files {
		path := join(dir, name)
		writeTestWAV(t, path, freqs...)
		paths = append(paths, path)
	}

	p, err := pipeline.NewDefaultPipeline(dbFile)
	require.NoError(t, err)
	defer p.Close()

	const goroutinesPerFile = 6
	var wg sync.WaitGroup
	idsMu := sync.Mutex{}
	idsByName := map[string]map[uint32]bool{}

	for _, path := range paths {
		idsByName[filepath.Base(path)] = map[uint32]bool{}
		for i := 0; i < goroutinesPerFile; i++ {
			wg.Add(1)
			go func(path string) {
				defer wg.Done()
				res, err := p.ProcessAndStore(path)
				assert.NoError(t, err)
				idsMu.Lock()
				idsByName[filepath.Base(path)][res.SongID] = true
				idsMu.Unlock()
			}(path)
		}
	}
	wg.Wait()

	bdb := p.DB.(*db.BoltDB)

	// Each name maps to exactly one ID and all IDs are distinct.
	allIDs := map[uint32]string{}
	for name, ids := range idsByName {
		require.Len(t, ids, 1, "song %s got several IDs under concurrent loads", name)
		for id := range ids {
			if previous, ok := allIDs[id]; ok {
				t.Fatalf("ID %d shared by %s and %s", id, previous, name)
			}
			allIDs[id] = name
		}
	}
	assert.Len(t, allIDs, len(files))

	// No fingerprint entry was overwritten by another song: every stored
	// TableValue points at the song that owns its key.
	total := 0
	for id, name := range allIDs {
		c, err := bdb.SongFingerprintCount(id)
		require.NoError(t, err)
		assert.NotZero(t, c, "song %s has no fingerprint left", name)
		total += c

		storedName, err := bdb.GetSong(id)
		require.NoError(t, err)
		assert.Equal(t, model.SongNameFromPath(join(dir, name)), storedName)
	}
	count, err := bdb.FingerprintCount()
	require.NoError(t, err)
	assert.Equal(t, total, count)

	// Each song is still queryable and matches its own content.
	for id, name := range allIDs {
		path := join(dir, name)
		score := queryScore(t, p, path, id)
		assert.True(t, score > 0, "song %s is not matchable after concurrent loads", name)
	}
}

func TestProcessAndStoreConcurrentSameFileSingleID(t *testing.T) {
	dir := mustTempDir(t, "musig-concurrent-same-")
	defer os.RemoveAll(dir)

	dbFile := join(dir, "musig.db")
	wavPath := join(dir, "same.wav")
	writeTestWAV(t, wavPath, 520, 1040)

	p, err := pipeline.NewDefaultPipeline(dbFile)
	require.NoError(t, err)
	defer p.Close()

	const n = 12
	ids := make(chan uint32, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := p.ProcessAndStore(wavPath)
			assert.NoError(t, err)
			ids <- res.SongID
		}()
	}
	wg.Wait()
	close(ids)

	first := <-ids
	for id := range ids {
		assert.Equal(t, first, id)
	}

	bdb := p.DB.(*db.BoltDB)
	c, err := bdb.SongFingerprintCount(first)
	require.NoError(t, err)
	assert.NotZero(t, c)

	total, err := bdb.FingerprintCount()
	require.NoError(t, err)
	assert.Equal(t, c, total, "repeated concurrent loads must not duplicate entries")

	// Sanity check on temp dir usage helpers.
	_, err = ioutil.ReadDir(dir)
	require.NoError(t, err)
}
