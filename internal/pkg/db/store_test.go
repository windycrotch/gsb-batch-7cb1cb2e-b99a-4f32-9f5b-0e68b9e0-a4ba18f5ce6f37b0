package db

import (
	"io/ioutil"
	"path/filepath"
	"sync"
	"testing"

	"github.com/sfluor/musig/internal/pkg/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestBoltDB(t *testing.T) (*BoltDB, string) {
	t.Helper()

	dir, err := ioutil.TempDir("", "musig-db-test-")
	require.NoError(t, err)

	path := filepath.Join(dir, "test.db")
	database, err := NewBoltDB(path)
	require.NoError(t, err)
	return database, path
}

func genFingerprint(songID uint32, n int) map[model.EncodedKey]model.TableValue {
	res := make(map[model.EncodedKey]model.TableValue, n)
	for i := 0; i < n; i++ {
		// Keys are unique per fingerprint, anchor times make the values
		// distinguishable between two fingerprints of the same song.
		key := model.EncodedKey(uint32(i+1) * 7)
		res[key] = model.TableValue{AnchorTimeMs: uint32(1000 + i), SongID: songID}
	}
	return res
}

func TestStoreSongIdempotentReload(t *testing.T) {
	database, _ := newTestBoltDB(t)
	defer database.Close()

	hash := []byte("hash-v1")
	fpr := genFingerprint(0, 8)

	id, changed, err := database.StoreSong("song-a", hash, fpr)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.NotZero(t, id)

	count, err := database.FingerprintCount()
	require.NoError(t, err)
	assert.Equal(t, len(fpr), count)

	songCount, err := database.SongFingerprintCount(id)
	require.NoError(t, err)
	assert.Equal(t, len(fpr), songCount)

	// Reloading the exact same content must not add any entry and must keep
	// the same song ID.
	id2, changed2, err := database.StoreSong("song-a", append([]byte(nil), hash...), fpr)
	require.NoError(t, err)
	assert.False(t, changed2)
	assert.Equal(t, id, id2)

	count2, err := database.FingerprintCount()
	require.NoError(t, err)
	assert.Equal(t, count, count2)

	songCount2, err := database.SongFingerprintCount(id)
	require.NoError(t, err)
	assert.Equal(t, songCount, songCount2)

	// Name -> ID mapping stays stable and survives a reload cycle.
	gotID, err := database.GetSongID("song-a")
	require.NoError(t, err)
	assert.Equal(t, id, gotID)

	name, err := database.GetSong(id)
	require.NoError(t, err)
	assert.Equal(t, "song-a", name)
}

func TestStoreSongContentChangeRetiresOldFingerprints(t *testing.T) {
	database, _ := newTestBoltDB(t)
	defer database.Close()

	oldFpr := genFingerprint(0, 8)
	id, changed, err := database.StoreSong("song-b", []byte("hash-v1"), oldFpr)
	require.NoError(t, err)
	assert.True(t, changed)

	// New content: completely different keys and anchor times.
	newFpr := genFingerprint(0, 5)
	for k, v := range newFpr {
		v.AnchorTimeMs += 5000
		newFpr[k] = v
	}
	// Use a disjoint key range so old and new fingerprints cannot share a key.
	disjoint := make(map[model.EncodedKey]model.TableValue, len(newFpr))
	i := 0
	for _, v := range newFpr {
		disjoint[model.EncodedKey(uint32(1000+i))] = v
		i++
	}

	id2, changed2, err := database.StoreSong("song-b", []byte("hash-v2"), disjoint)
	require.NoError(t, err)
	assert.True(t, changed2)
	assert.Equal(t, id, id2, "the song name must keep its identity")

	// Only the new fingerprint set must remain.
	count, err := database.FingerprintCount()
	require.NoError(t, err)
	assert.Equal(t, len(disjoint), count)

	songCount, err := database.SongFingerprintCount(id)
	require.NoError(t, err)
	assert.Equal(t, len(disjoint), songCount)

	storedHash, err := database.SongContentHash(id)
	require.NoError(t, err)
	assert.Equal(t, []byte("hash-v2"), storedHash)

	// Every stored value must point at the same song and no old anchor time
	// may survive.
	for key := range oldFpr {
		got, err := database.Get([]model.EncodedKey{key})
		require.NoError(t, err)
		assert.NotContains(t, got, key, "old fingerprint key should have been removed")
	}
	for key, expected := range disjoint {
		got, err := database.Get([]model.EncodedKey{key})
		require.NoError(t, err)
		require.Contains(t, got, key)
		require.Len(t, got[key], 1)
		assert.Equal(t, id, got[key][0].SongID)
		assert.Equal(t, expected.AnchorTimeMs, got[key][0].AnchorTimeMs)
	}

	// Reloading the current content again is still a no-op.
	id3, changed3, err := database.StoreSong("song-b", []byte("hash-v2"), disjoint)
	require.NoError(t, err)
	assert.False(t, changed3)
	assert.Equal(t, id, id3)
	count3, err := database.FingerprintCount()
	require.NoError(t, err)
	assert.Equal(t, len(disjoint), count3)
}

func TestStoreSongSharedKeysKeepOtherSongs(t *testing.T) {
	database, _ := newTestBoltDB(t)
	defer database.Close()

	// Two songs sharing some of their keys.
	fprA := map[model.EncodedKey]model.TableValue{
		1: {AnchorTimeMs: 10, SongID: 0},
		2: {AnchorTimeMs: 20, SongID: 0},
		3: {AnchorTimeMs: 30, SongID: 0},
	}
	fprB := map[model.EncodedKey]model.TableValue{
		2: {AnchorTimeMs: 40, SongID: 0},
		3: {AnchorTimeMs: 50, SongID: 0},
		4: {AnchorTimeMs: 60, SongID: 0},
	}

	idA, _, err := database.StoreSong("song-a", []byte("a1"), fprA)
	require.NoError(t, err)
	idB, _, err := database.StoreSong("song-b", []byte("b1"), fprB)
	require.NoError(t, err)
	require.NotEqual(t, idA, idB)

	count, err := database.FingerprintCount()
	require.NoError(t, err)
	assert.Equal(t, 6, count)

	// Replace song A: the shared keys must lose A's values but keep B's.
	newA := map[model.EncodedKey]model.TableValue{
		5: {AnchorTimeMs: 70, SongID: 0},
	}
	idA2, _, err := database.StoreSong("song-a", []byte("a2"), newA)
	require.NoError(t, err)
	assert.Equal(t, idA, idA2)

	got, err := database.Get([]model.EncodedKey{2, 3, 4, 5})
	require.NoError(t, err)
	for _, key := range []model.EncodedKey{2, 3, 4} {
		require.Contains(t, got, key)
		for _, v := range got[key] {
			assert.Equal(t, idB, v.SongID, "song B values must survive song A replacement")
		}
	}
	assert.Len(t, got[2], 1)
	assert.Len(t, got[3], 1)
	assert.Len(t, got[4], 1)
	assert.Len(t, got[5], 1)
	assert.Equal(t, idA, got[5][0].SongID)

	count, err = database.FingerprintCount()
	require.NoError(t, err)
	assert.Equal(t, 4, count)
}

func TestStoreSongFailedTransactionRollsBack(t *testing.T) {
	database, _ := newTestBoltDB(t)
	defer database.Close()

	fpr := genFingerprint(0, 6)
	id, changed, err := database.StoreSong("song-c", []byte("h1"), fpr)
	require.NoError(t, err)
	require.True(t, changed)

	snapshotCount, err := database.FingerprintCount()
	require.NoError(t, err)
	require.Equal(t, len(fpr), snapshotCount)

	// Force the replacement transaction to fail right before it would commit.
	injected := assert.AnError
	database.failHook = func(failingID uint32) error {
		require.Equal(t, id, failingID)
		return injected
	}

	newFpr := genFingerprint(0, 3)
	_, _, err = database.StoreSong("song-c", []byte("h2"), newFpr)
	require.Error(t, err)

	database.failHook = nil

	// The old, consistent state must be fully preserved.
	count, err := database.FingerprintCount()
	require.NoError(t, err)
	assert.Equal(t, snapshotCount, count)

	songCount, err := database.SongFingerprintCount(id)
	require.NoError(t, err)
	assert.Equal(t, len(fpr), songCount)

	storedHash, err := database.SongContentHash(id)
	require.NoError(t, err)
	assert.Equal(t, []byte("h1"), storedHash)

	// A new attempt without the hook succeeds and reaches a consistent state.
	id2, changed2, err := database.StoreSong("song-c", []byte("h2"), newFpr)
	require.NoError(t, err)
	assert.True(t, changed2)
	assert.Equal(t, id, id2)

	count, err = database.FingerprintCount()
	require.NoError(t, err)
	assert.Equal(t, len(newFpr), count)

	storedHash, err = database.SongContentHash(id)
	require.NoError(t, err)
	assert.Equal(t, []byte("h2"), storedHash)
}

func TestStoreSongFailedTransactionForNewSongLeavesNoTrace(t *testing.T) {
	database, _ := newTestBoltDB(t)
	defer database.Close()

	database.failHook = func(uint32) error { return assert.AnError }

	fpr := genFingerprint(0, 4)
	_, _, err := database.StoreSong("ghost", []byte("g1"), fpr)
	require.Error(t, err)
	database.failHook = nil

	// No song name may point at an ID without fingerprints.
	_, err = database.GetSongID("ghost")
	require.Error(t, err)

	count, err := database.FingerprintCount()
	require.NoError(t, err)
	assert.Zero(t, count)

	// Recovering on the next load works.
	id, changed, err := database.StoreSong("ghost", []byte("g1"), fpr)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.NotZero(t, id)
	name, err := database.GetSong(id)
	require.NoError(t, err)
	assert.Equal(t, "ghost", name)
}

func TestStoreSongInvariantsSurviveReopen(t *testing.T) {
	database, path := newTestBoltDB(t)

	fprA := genFingerprint(0, 6)
	idA, _, err := database.StoreSong("song-a", []byte("a1"), fprA)
	require.NoError(t, err)

	fprB := genFingerprint(0, 4)
	idB, _, err := database.StoreSong("song-b", []byte("b1"), fprB)
	require.NoError(t, err)

	require.NoError(t, database.Close())

	reopened, err := NewBoltDB(path)
	require.NoError(t, err)
	defer reopened.Close()

	// Name -> ID mapping is stable.
	gotA, err := reopened.GetSongID("song-a")
	require.NoError(t, err)
	assert.Equal(t, idA, gotA)
	gotB, err := reopened.GetSongID("song-b")
	require.NoError(t, err)
	assert.Equal(t, idB, gotB)

	count, err := reopened.FingerprintCount()
	require.NoError(t, err)
	assert.Equal(t, len(fprA)+len(fprB), count)

	// Idempotent reload still holds after reopening.
	idA2, changed, err := reopened.StoreSong("song-a", []byte("a1"), fprA)
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Equal(t, idA, idA2)

	count2, err := reopened.FingerprintCount()
	require.NoError(t, err)
	assert.Equal(t, count, count2)

	// Content replacement still works after reopening and only touches A.
	newA := genFingerprint(0, 2)
	idA3, changed, err := reopened.StoreSong("song-a", []byte("a2"), newA)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, idA, idA3)

	count3, err := reopened.FingerprintCount()
	require.NoError(t, err)
	assert.Equal(t, len(newA)+len(fprB), count3)

	songBCount, err := reopened.SongFingerprintCount(idB)
	require.NoError(t, err)
	assert.Equal(t, len(fprB), songBCount)
}

func TestStoreSongConcurrentSameNameGetsSingleID(t *testing.T) {
	database, _ := newTestBoltDB(t)
	defer database.Close()

	const goroutines = 16
	ids := make(chan uint32, goroutines)
	var wg sync.WaitGroup

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Same name, same content hash: at most one writer actually
			// changes the fingerprint set.
			id, _, err := database.StoreSong("concurrent", []byte("same"), genFingerprint(0, 5))
			assert.NoError(t, err)
			ids <- id
		}()
	}
	wg.Wait()
	close(ids)

	first := <-ids
	for id := range ids {
		assert.Equal(t, first, id, "the same song name must map to a single ID")
	}

	count, err := database.FingerprintCount()
	require.NoError(t, err)
	assert.Equal(t, 5, count, "no duplicate fingerprint entries may be written")

	songCount, err := database.SongFingerprintCount(first)
	require.NoError(t, err)
	assert.Equal(t, 5, songCount)
}

func TestStoreSongConcurrentDistinctSongsDoNotOverwrite(t *testing.T) {
	database, _ := newTestBoltDB(t)
	defer database.Close()

	const n = 8
	var wg sync.WaitGroup
	idMu := sync.Mutex{}
	assigned := map[string]uint32{}

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := "song-" + string(rune('a'+i))
			id, _, err := database.StoreSong(name, []byte{byte(i)}, genFingerprint(0, 4))
			assert.NoError(t, err)
			idMu.Lock()
			assigned[name] = id
			idMu.Unlock()
		}(i)
	}
	wg.Wait()

	require.Len(t, assigned, n)

	// Every song got a unique ID and a full, untouched fingerprint set.
	seenIDs := map[uint32]bool{}
	total := 0
	for name, id := range assigned {
		assert.False(t, seenIDs[id], "ID %d assigned twice", id)
		seenIDs[id] = true

		gotName, err := database.GetSong(id)
		require.NoError(t, err)
		assert.Equal(t, name, gotName)

		c, err := database.SongFingerprintCount(id)
		require.NoError(t, err)
		assert.Equal(t, 4, c)
		total += c
	}

	count, err := database.FingerprintCount()
	require.NoError(t, err)
	assert.Equal(t, total, count)
	assert.Equal(t, 4*n, count)
}
