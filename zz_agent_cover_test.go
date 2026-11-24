package main

// Additional acceptance tests authored for the cover-fallback feature.
// Every top-level identifier carries the "agent" prefix to avoid colliding
// with helper names used by any externally supplied test files.

import (
	"context"
	"fmt"
	"net/http/httptest"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// agentTestS3Config is a self-contained copy of the fake-S3 harness so the
// agent-authored tests do not depend on helpers in the baseline test files.
func agentTestS3Config() (S3Config, func()) {
	backend := s3mem.New()
	faker := gofakes3.New(backend)
	ts := httptest.NewServer(faker.Server())
	return S3Config{
		Region:         "agent-test",
		Endpoint:       ts.URL,
		Bucket:         "agent-bucket",
		ForcePathStyle: true,
		Credentials: &S3Credentials{
			ID:     "id1",
			Secret: "secret1",
		},
		RequestPresignExpiry: Duration(time.Minute),
	}, ts.Close
}

func agentSeededLibrary(t *testing.T, basePrefix string, keys ...string) (*MediaLibrary, func()) {
	t.Helper()
	cfg, closeS3 := agentTestS3Config()
	cfg.BasePrefix = basePrefix
	store := NewS3Storage(cfg)

	ctx := context.Background()
	_, err := store.s3.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String(cfg.Bucket),
	})
	require.NoError(t, err)

	for _, key := range keys {
		fullKey := key
		if cfg.BasePrefix != "" {
			fullKey = path.Join(cfg.BasePrefix, key)
		}
		_, err := store.s3.PutObject(ctx, &s3.PutObjectInput{
			Body:   strings.NewReader("non-empty-content"),
			Bucket: aws.String(cfg.Bucket),
			Key:    aws.String(fullKey),
		})
		require.NoError(t, err)
	}
	return NewMediaLibrary(store), closeS3
}

func TestAgent_FindCoverTieDeterministicAcrossPermutations(t *testing.T) {
	// "cover" and "folder" both produce the maximum score of 2.
	candidates := []*StorageFile{
		NewStorageFile("zebra/cover.jpg", 1),
		NewStorageFile("zebra/folder.jpg", 1),
	}
	ml := &MediaLibrary{}

	want := "zebra/cover.jpg"
	assert.Equal(t, want, ml.findCover(candidates).path)

	for _, perm := range agentPermutations(len(candidates)) {
		var reordered []*StorageFile
		for _, idx := range perm {
			reordered = append(reordered, candidates[idx])
		}
		cover := ml.findCover(reordered)
		if assert.NotNil(t, cover) {
			assert.Equal(t, want, cover.path, "permutation %v selected %s", perm, cover.path)
		}
	}
}

func TestAgent_FindCoverHigherScoreBeatsOrder(t *testing.T) {
	ml := &MediaLibrary{}
	in := []*StorageFile{
		NewStorageFile("aaa.jpg", 1),
		NewStorageFile("zzz.png", 1),
		NewStorageFile("Cover.JPG", 1),
	}
	cover := ml.findCover(in)
	require.NotNil(t, cover)
	assert.Equal(t, "Cover.JPG", cover.Name())
}

func TestAgent_CoverFallsBackToImmediateParent(t *testing.T) {
	keys := []string{
		"fallback/Artist/cover.jpg",
		"fallback/Artist/parent-notes.txt",
		"fallback/Artist/Album/01 Track.mp3",
		"fallback/Artist/Album/notes.txt",
	}
	ml, closeS3 := agentSeededLibrary(t, "music/", keys...)
	defer closeS3()

	listing, err := ml.List(context.Background(), "fallback/Artist/Album")
	assert.NoError(t, err)
	require.NotNil(t, listing.Cover, "expected cover inherited from parent directory")
	assert.Equal(t, "fallback/Artist/cover.jpg", listing.Cover.path)

	assert.Len(t, listing.AudioTracks, 1)
	assert.Equal(t, "fallback/Artist/Album/01 Track.mp3", listing.AudioTracks[0].path)

	require.Len(t, listing.Files, 1)
	assert.Equal(t, "fallback/Artist/Album/notes.txt", listing.Files[0].path)
	for _, f := range listing.Files {
		assert.NotEqual(t, listing.Cover.path, f.path)
		assert.NotContains(t, f.path, "parent-notes.txt")
	}
}

func TestAgent_CoverFallsBackSeveralLevelsNearestWins(t *testing.T) {
	keys := []string{
		"multi/Artist/Cover.JPG",
		"multi/Artist/Album/folder.jpeg",
		"multi/Artist/Album/CD1/01.mp3",
	}
	ml, closeS3 := agentSeededLibrary(t, "music/", keys...)
	defer closeS3()

	listing, err := ml.List(context.Background(), "multi/Artist/Album/CD1")
	require.NoError(t, err)
	require.NotNil(t, listing.Cover)
	assert.Equal(t, "multi/Artist/Album/folder.jpeg", listing.Cover.path)

	keys2 := []string{
		"multi2/Artist/Cover.JPG",
		"multi2/Artist/Album/CD1/01.mp3",
	}
	ml2, close2 := agentSeededLibrary(t, "music/", keys2...)
	defer close2()

	listing2, err := ml2.List(context.Background(), "multi2/Artist/Album/CD1")
	require.NoError(t, err)
	require.NotNil(t, listing2.Cover)
	assert.Equal(t, "multi2/Artist/Cover.JPG", listing2.Cover.path)
}

func TestAgent_ArtworkDirectoryNamesCaseInsensitive(t *testing.T) {
	keys := []string{
		"case/Album/SCANS/BigScan.PNG",
		"case/Album/01.mp3",
	}
	ml, closeS3 := agentSeededLibrary(t, "music/", keys...)
	defer closeS3()

	listing, err := ml.List(context.Background(), "case/Album")
	require.NoError(t, err)
	require.NotNil(t, listing.Cover)
	assert.Equal(t, "case/Album/SCANS/BigScan.PNG", listing.Cover.path)
}

func TestAgent_EqualScoreCoverStableConcurrently(t *testing.T) {
	keys := []string{
		"tie/Album/Cover.jpg",
		"tie/Album/Folder.jpg",
		"tie/Album/01.mp3",
		"tie/Album/02.mp3",
	}
	ml, closeS3 := agentSeededLibrary(t, "music/", keys...)
	defer closeS3()

	ctx := context.Background()
	const n = 32
	var wg sync.WaitGroup
	covers := make(chan string, n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := ml.List(ctx, "tie/Album")
			if err != nil {
				errs <- err
				return
			}
			if l.Cover == nil {
				errs <- fmt.Errorf("nil cover")
				return
			}
			covers <- l.Cover.path
		}()
	}
	wg.Wait()
	close(covers)
	close(errs)

	for err := range errs {
		assert.NoError(t, err)
	}
	winner := ""
	count := 0
	for c := range covers {
		if winner == "" {
			winner = c
		}
		assert.Equal(t, winner, c, "cover selection jittered between requests")
		count++
	}
	assert.Equal(t, n, count)
	assert.Equal(t, "tie/Album/Cover.jpg", winner)
}

func TestAgent_NotFoundIsClassified(t *testing.T) {
	ml, closeS3 := agentSeededLibrary(t, "music/")
	defer closeS3()

	_, err := ml.List(context.Background(), "missing/album")
	require.Error(t, err)
	assert.True(t, IsNotFoundError(err))
	assert.ErrorIs(t, err, ErrNotFound)
}

func agentPermutations(n int) [][]int {
	if n <= 0 || n > 4 {
		return [][]int{agentIdentity(n)}
	}
	var out [][]int
	idx := agentIdentity(n)
	var generate func(k int)
	generate = func(k int) {
		if k == 1 {
			cp := append([]int(nil), idx...)
			out = append(out, cp)
			return
		}
		generate(k - 1)
		for i := 0; i < k-1; i++ {
			if k%2 == 0 {
				idx[i], idx[k-1] = idx[k-1], idx[i]
			} else {
				idx[0], idx[k-1] = idx[k-1], idx[0]
			}
			generate(k - 1)
		}
	}
	generate(n)
	return out
}

func agentIdentity(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}
