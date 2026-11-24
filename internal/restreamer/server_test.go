package restreamer

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/spf13/viper"
)

func configureStreams(t *testing.T, streams map[string]string) {
	t.Helper()
	viper.Reset()
	viper.Set("max-bandwidth", 10.0)
	viper.Set("read-buffer", 0.001)
	viper.Set("streams", streams)
	t.Cleanup(viper.Reset)
}

// TestUnknownStreamIDReturns404 verifies that a stream id missing from the
// configuration is reported as a distinct not-found status instead of the
// connection ending silently.
func TestUnknownStreamIDReturns404(t *testing.T) {
	configureStreams(t, map[string]string{"known": "http://example.invalid/playlist.m3u8"})

	req := httptest.NewRequest(http.MethodGet, "/unknown-id", nil)
	rec := httptest.NewRecorder()

	httpStream(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown stream id, got %d (body: %q)", rec.Code, rec.Body.String())
	}
}

// TestPullFailureReturns502 verifies that a known stream id whose upstream
// cannot be pulled produces a bad-gateway status, distinguishable from the
// not-found case above.
func TestPullFailureReturns502(t *testing.T) {
	// An origin that answers 404 for the playlist: the request layer returns
	// an error immediately instead of retrying forever.
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(origin.Close)

	configureStreams(t, map[string]string{"broken": origin.URL + "/missing.m3u8"})

	req := httptest.NewRequest(http.MethodGet, "/broken", nil)
	rec := httptest.NewRecorder()

	httpStream(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 for upstream pull failure, got %d (body: %q)", rec.Code, rec.Body.String())
	}
}

// TestClientDisconnectStopsCleanly ensures that when the request context is
// cancelled (as happens when an HTTP client hangs up) the stream stops
// without an error response and without panicking on the closed recorder.
func TestClientDisconnectStopsCleanly(t *testing.T) {
	// Origin holds every request open until the test ends.
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(origin.Close)

	configureStreams(t, map[string]string{"live": origin.URL + "/live.m3u8"})

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/live", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		httpStream(rec, req)
		close(done)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return after client disconnect")
	}

	// Nothing should have been written for a stream that never produced data.
	if rec.Body.Len() != 0 {
		t.Fatalf("unexpected body after disconnect: %q", rec.Body.String())
	}
}

// TestStartErrStreamNotFoundSentinel keeps the sentinel error contract stable
// for any other caller of the shared start logic.
func TestStartErrStreamNotFoundSentinel(t *testing.T) {
	configureStreams(t, map[string]string{})

	err := start(context.Background(), &discardWriter{}, "nope")
	if err == nil {
		t.Fatal("expected error for missing stream id")
	}
	if !errors.Is(err, ErrStreamNotFound) {
		t.Fatalf("expected ErrStreamNotFound, got %v", err)
	}
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
