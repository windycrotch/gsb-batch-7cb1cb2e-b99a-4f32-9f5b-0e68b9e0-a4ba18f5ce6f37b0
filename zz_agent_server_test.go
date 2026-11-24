package main

// HTTP-level acceptance tests: 404 vs 5xx classification, cancellation,
// traversal/empty boundaries, concurrency isolation and real mux routing.
// All top-level identifiers carry the "agent" prefix to avoid collisions
// with externally supplied test files in package main.

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type agentRoundTripper struct {
	calls    atomic.Int64
	mu       sync.Mutex
	down     bool
	hook     func(req *http.Request) (*http.Response, bool)
	delegate http.RoundTripper
}

func (rt *agentRoundTripper) setDown(v bool) {
	rt.mu.Lock()
	rt.down = v
	rt.mu.Unlock()
}

func (rt *agentRoundTripper) setHook(h func(req *http.Request) (*http.Response, bool)) {
	rt.mu.Lock()
	rt.hook = h
	rt.mu.Unlock()
}

func (rt *agentRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.calls.Add(1)
	rt.mu.Lock()
	down := rt.down
	hook := rt.hook
	rt.mu.Unlock()

	if hook != nil {
		if resp, handled := hook(req); handled {
			return resp, nil
		}
	}
	if down {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return agentSyntheticResponse(req, http.StatusInternalServerError), nil
	}
	return rt.delegate.RoundTrip(req)
}

func agentSyntheticResponse(req *http.Request, code int) *http.Response {
	body := `<?xml version="1.0" encoding="UTF-8"?><Error><Code>InternalError</Code><Message>storage unavailable</Message></Error>`
	return &http.Response{
		Status:        http.StatusText(code),
		StatusCode:    code,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        make(http.Header),
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}

func agentInstallRT(t *testing.T, store *S3Storage, rt http.RoundTripper) {
	t.Helper()
	client := s3.NewFromConfig(aws.Config{Region: store.cfg.Region}, func(o *s3.Options) {
		if store.cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(store.cfg.Endpoint)
		}
		o.UsePathStyle = store.cfg.ForcePathStyle
		o.HTTPClient = &http.Client{Transport: rt}
		o.Retryer = retry.NewStandard(func(o *retry.StandardOptions) {
			o.MaxAttempts = 1
		})
		if store.cfg.Credentials != nil {
			o.Credentials = credentials.NewStaticCredentialsProvider(
				store.cfg.Credentials.ID,
				store.cfg.Credentials.Secret,
				store.cfg.Credentials.Token,
			)
		}
	})
	store.s3 = client
	store.presigner = s3.NewPresignClient(client)
}

func agentScriptedLibrary(t *testing.T, basePrefix string) (*MediaLibrary, *agentRoundTripper, func()) {
	t.Helper()
	cfg, closeS3 := agentTestS3Config()
	cfg.BasePrefix = basePrefix
	store := NewS3Storage(cfg)
	rt := &agentRoundTripper{delegate: http.DefaultTransport}
	agentInstallRT(t, store, rt)

	ctx := context.Background()
	_, err := store.s3.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(cfg.Bucket)})
	require.NoError(t, err)
	rt.calls.Store(0)
	return NewMediaLibrary(store), rt, closeS3
}

func agentSeed(t *testing.T, ml *MediaLibrary, key, content string) {
	t.Helper()
	fullKey := key
	if ml.store.cfg.BasePrefix != "" {
		fullKey = ml.store.cfg.BasePrefix + key
	}
	_, err := ml.store.s3.PutObject(context.Background(), &s3.PutObjectInput{
		Body:   strings.NewReader(content),
		Bucket: aws.String(ml.store.cfg.Bucket),
		Key:    aws.String(fullKey),
	})
	require.NoError(t, err)
}

type agentRecordingWriter struct {
	*httptest.ResponseRecorder
	headerWritten atomic.Bool
	writes        atomic.Int32
}

func agentNewRecordingWriter() *agentRecordingWriter {
	return &agentRecordingWriter{ResponseRecorder: httptest.NewRecorder()}
}

func (w *agentRecordingWriter) WriteHeader(code int) {
	w.headerWritten.Store(true)
	w.ResponseRecorder.WriteHeader(code)
}

func (w *agentRecordingWriter) Write(b []byte) (int, error) {
	w.writes.Add(1)
	return w.ResponseRecorder.Write(b)
}

func agentApp(t *testing.T, ml *MediaLibrary) http.Handler {
	t.Helper()
	app, err := NewHandler(ml)
	require.NoError(t, err)
	return app
}

func agentErrf(format string, args ...any) error {
	return fmt.Errorf(format, args...)
}

func TestAgent_HTTPListingMissingIs404(t *testing.T) {
	ml, closeS3 := agentSeededLibrary(t, "music/", "exists/cover.jpg")
	defer closeS3()
	app := agentApp(t, ml)

	req := httptest.NewRequest(http.MethodGet, "/library/no/such/album", nil)
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, strings.ToLower(rec.Body.String()), "not found")
}

func TestAgent_HTTPStreamMissingObjectIs404(t *testing.T) {
	ml, closeS3 := agentSeededLibrary(t, "music/")
	defer closeS3()
	app := agentApp(t, ml)

	req := httptest.NewRequest(http.MethodGet, "/stream/missing.mp3", nil)
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Empty(t, rec.Header().Get("Location"))
}

func TestAgent_EmptyFileDoesNotRedirect(t *testing.T) {
	cfg, closeS3 := agentTestS3Config()
	defer closeS3()
	cfg.BasePrefix = "music/"
	store := NewS3Storage(cfg)
	ctx := context.Background()
	_, err := store.s3.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(cfg.Bucket)})
	require.NoError(t, err)
	_, err = store.s3.PutObject(ctx, &s3.PutObjectInput{
		Body:   strings.NewReader(""),
		Bucket: aws.String(cfg.Bucket),
		Key:    aws.String("music/empty.mp3"),
	})
	require.NoError(t, err)

	app := agentApp(t, NewMediaLibrary(store))
	req := httptest.NewRequest(http.MethodGet, "/stream/empty.mp3", nil)
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, req)

	assert.GreaterOrEqual(t, rec.Code, 400)
	assert.Empty(t, rec.Header().Get("Location"))
	assert.Contains(t, strings.ToLower(rec.Body.String()), "empty")
}

func TestAgent_TraversalRejectedWithoutRedirect(t *testing.T) {
	ml, closeS3 := agentSeededLibrary(t, "music/")
	defer closeS3()
	app := agentApp(t, ml)

	for _, target := range []string{
		"/stream/../etc/passwd",
		"/stream/%2e%2e/etc/passwd",
		"/library/../../etc",
	} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		rec := httptest.NewRecorder()
		app.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "target %s", target)
		assert.Empty(t, rec.Header().Get("Location"), "target %s", target)
	}

	srv, err := NewServer(ml)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodGet, "/../etc/passwd", nil)
	rec := httptest.NewRecorder()
	srv.StreamHandler(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Empty(t, rec.Header().Get("Location"))
}

func TestAgent_StorageFailure5xxAndRecovery(t *testing.T) {
	ml, rt, closeS3 := agentScriptedLibrary(t, "music/")
	defer closeS3()
	agentSeed(t, ml, "ok.txt", "hello")
	app := agentApp(t, ml)

	get := func(target string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		rec := httptest.NewRecorder()
		app.ServeHTTP(rec, req)
		return rec
	}

	rt.setDown(true)
	for i := 0; i < 3; i++ {
		rec := get("/library/")
		assert.GreaterOrEqual(t, rec.Code, 500, "attempt %d", i)
		assert.Less(t, rec.Code, 600)
		assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
		assert.NotEmpty(t, rec.Body.String())
	}
	streamRec := get("/stream/ok.txt")
	assert.GreaterOrEqual(t, streamRec.Code, 500)
	assert.Empty(t, streamRec.Header().Get("Location"))

	rt.setDown(false)
	require.Eventually(t, func() bool {
		return get("/library/").Code == http.StatusOK
	}, 2*time.Second, 20*time.Millisecond)
}

func TestAgent_FallbackStorageErrorNotMasked(t *testing.T) {
	ml, rt, closeS3 := agentScriptedLibrary(t, "music/")
	defer closeS3()
	agentSeed(t, ml, "fb/Artist/cover.jpg", "img")
	agentSeed(t, ml, "fb/Artist/Album/track.mp3", "audio")

	rt.setHook(func(req *http.Request) (*http.Response, bool) {
		if req.Method == http.MethodGet && req.URL.Query().Has("list-type") {
			if req.URL.Query().Get("prefix") == "music/fb/Artist/" {
				return agentSyntheticResponse(req, http.StatusServiceUnavailable), true
			}
		}
		return nil, false
	})

	_, err := ml.List(context.Background(), "fb/Artist/Album")
	require.Error(t, err)
	assert.False(t, IsNotFoundError(err))

	app := agentApp(t, ml)
	req := httptest.NewRequest(http.MethodGet, "/library/fb/Artist/Album", nil)
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, req)
	assert.GreaterOrEqual(t, rec.Code, 500)

	rt.setHook(nil)
	listing, err := ml.List(context.Background(), "fb/Artist/Album")
	require.NoError(t, err)
	require.NotNil(t, listing.Cover)
	assert.Equal(t, "fb/Artist/cover.jpg", listing.Cover.path)
}

func TestAgent_CanceledStreamDoesNotSignOrWrite(t *testing.T) {
	ml, rt, closeS3 := agentScriptedLibrary(t, "music/")
	defer closeS3()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	srv := &Server{mediaLib: ml}
	req := httptest.NewRequest(http.MethodGet, "/stream/track.mp3", nil).WithContext(ctx)
	w := agentNewRecordingWriter()
	srv.StreamHandler(w, req)

	assert.Equal(t, int64(0), rt.calls.Load())
	assert.False(t, w.headerWritten.Load())
	assert.Equal(t, int32(0), w.writes.Load())
	assert.Empty(t, w.Header().Get("Location"))

	app := agentApp(t, ml)
	rec := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/stream/track.mp3", nil).WithContext(ctx)
	app.ServeHTTP(rec, req2)
	assert.NotEqual(t, http.StatusFound, rec.Code)
	assert.Empty(t, rec.Header().Get("Location"))
	assert.Equal(t, int64(0), rt.calls.Load())
}

func TestAgent_ConcurrentStreamsListingsCancellation(t *testing.T) {
	keys := []string{
		"conc/Album/cover.jpg",
		"conc/Album/track1.mp3",
		"conc/Album/track2.mp3",
		"conc/Album/track3.mp3",
	}
	ml, closeS3 := agentSeededLibrary(t, "music/", keys...)
	defer closeS3()

	ts := httptest.NewServer(agentApp(t, ml))
	defer ts.Close()

	noRedirect := &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	tracks := []string{"track1.mp3", "track2.mp3", "track3.mp3"}
	const perTrack = 12
	var wg sync.WaitGroup
	errCh := make(chan error, 256)

	for _, track := range tracks {
		for i := 0; i < perTrack; i++ {
			wg.Add(1)
			go func(track string) {
				defer wg.Done()
				resp, err := noRedirect.Get(ts.URL + "/stream/conc/Album/" + track)
				if err != nil {
					errCh <- err
					return
				}
				defer resp.Body.Close()
				_, _ = io.Copy(io.Discard, resp.Body)
				if resp.StatusCode != http.StatusFound {
					errCh <- agentErrf("track %s: expected 302, got %d", track, resp.StatusCode)
					return
				}
				loc := resp.Header.Get("Location")
				if !strings.Contains(loc, track) {
					errCh <- agentErrf("cross-talk: location %q does not belong to %s", loc, track)
					return
				}
				for _, other := range tracks {
					if other != track && strings.Contains(loc, other) {
						errCh <- agentErrf("cross-talk: %q for %s references %s", loc, track, other)
					}
				}
			}(track)
		}
	}

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := noRedirect.Get(ts.URL + "/library/conc/Album")
			if err != nil {
				errCh <- err
				return
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK {
				errCh <- agentErrf("listing status %d", resp.StatusCode)
				return
			}
			if !strings.Contains(string(body), "cover.jpg") {
				errCh <- agentErrf("listing missing cover")
			}
		}()
	}

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/stream/conc/Album/track1.mp3", nil)
			if err != nil {
				errCh <- err
				return
			}
			resp, err := noRedirect.Do(req)
			if err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusFound {
					errCh <- agentErrf("canceled request unexpectedly redirected")
				}
			}
		}()
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		assert.NoError(t, err)
	}

	for _, track := range tracks {
		resp, err := noRedirect.Get(ts.URL + "/stream/conc/Album/" + track)
		require.NoError(t, err)
		assert.Equal(t, http.StatusFound, resp.StatusCode)
		assert.Contains(t, resp.Header.Get("Location"), track)
		_ = resp.Body.Close()
	}
}

func TestAgent_StreamRedirectServesBytes(t *testing.T) {
	ml, closeS3 := agentSeededLibrary(t, "music/", "play/song.mp3")
	defer closeS3()
	// Seeded content is the shared non-empty placeholder; reseed distinctive bytes.
	agentSeed(t, ml, "play/song.mp3", "DISTINCT-AUDIO-BYTES")

	ts := httptest.NewServer(agentApp(t, ml))
	defer ts.Close()

	// Default client follows the 302 to the presigned S3 URL.
	resp, err := http.Get(ts.URL + "/stream/play/song.mp3")
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "DISTINCT-AUDIO-BYTES", string(body))
}

func TestAgent_LiteralServerLazyTemplate(t *testing.T) {
	ml, closeS3 := agentSeededLibrary(t, "music/", "lazy/Album/track.mp3", "lazy/Album/cover.jpg")
	defer closeS3()

	// Zero-value construction style a focused unit test might use.
	srv := &Server{mediaLib: ml}
	req := httptest.NewRequest(http.MethodGet, "/lazy/Album", nil)
	rec := httptest.NewRecorder()
	srv.ListingHandler(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "track")
	assert.Contains(t, rec.Body.String(), "cover.jpg")
}

func TestAgent_StaticAssetsServed(t *testing.T) {
	ml, closeS3 := agentSeededLibrary(t, "music/")
	defer closeS3()
	srv, err := NewServer(ml)
	require.NoError(t, err)
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "/static/"+srv.staticVersion+"/style.css", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.NotEmpty(t, rec.Body.Bytes())
}

func TestAgent_StartServerEndToEnd(t *testing.T) {
	keys := []string{
		"e2e/Album/cover.jpg",
		"e2e/Album/track1.mp3",
	}
	ml, closeS3 := agentSeededLibrary(t, "music/", keys...)
	defer closeS3()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())

	go func() { _ = StartServer(ml, addr) }()

	noRedirect := &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	base := "http://" + addr
	require.Eventually(t, func() bool {
		resp, err := http.Get(base + "/library/")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 5*time.Second, 50*time.Millisecond)

	resp, err := noRedirect.Get(base + "/")
	require.NoError(t, err)
	assert.Equal(t, http.StatusMovedPermanently, resp.StatusCode)
	assert.Equal(t, "/library/", resp.Header.Get("Location"))
	_ = resp.Body.Close()

	resp, err = http.Get(base + "/library/e2e/Album")
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, string(body), "cover.jpg")
	assert.Contains(t, string(body), "track1")

	resp, err = noRedirect.Get(base + "/stream/e2e/Album/track1.mp3")
	require.NoError(t, err)
	assert.Equal(t, http.StatusFound, resp.StatusCode)
	loc := resp.Header.Get("Location")
	assert.NotEmpty(t, loc)
	assert.Contains(t, loc, "track1.mp3")
	parsed, err := url.Parse(loc)
	require.NoError(t, err)
	assert.True(t, parsed.IsAbs())
	_ = resp.Body.Close()
}
