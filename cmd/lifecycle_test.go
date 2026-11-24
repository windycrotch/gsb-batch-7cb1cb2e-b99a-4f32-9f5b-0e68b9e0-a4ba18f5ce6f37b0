package main

// Lifecycle tests for POST /v1/process.
//
// The suite is hermetic: instead of requiring an ffmpeg installation it
// re-executes the test binary as a fake ffmpeg process
// (TestLifecycleFakeFFmpegHelper). Cancellation therefore goes through the
// real os/exec CommandContext path, including process kill. Downloads run
// against real httptest HTTP servers and disconnect tests use a real TCP
// httptest.Server plus a cancelable HTTP client, so the production
// r.Context() disconnect propagation is exercised end to end.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ffmpeg-api/api"
	"ffmpeg-api/internal"
)

const (
	lifecycleTestTimeout  = 15 * time.Second
	lifecyclePollInterval = 10 * time.Millisecond
)

// ---------------------------------------------------------------------------
// Fake ffmpeg helper (re-executed test binary)
// ---------------------------------------------------------------------------

// TestLifecycleFakeFFmpegHelper behaves like a controllable ffmpeg binary
// when LIFECYCLE_FAKE_HELPER=1 is set in its environment. In a normal "go
// test" invocation the env var is absent and the test simply passes.
func TestLifecycleFakeFFmpegHelper(t *testing.T) {
	if os.Getenv("LIFECYCLE_FAKE_HELPER") != "1" {
		return
	}

	mode := os.Getenv("LIFECYCLE_FAKE_MODE")
	out := os.Getenv("LIFECYCLE_FAKE_OUT")
	content := os.Getenv("LIFECYCLE_FAKE_CONTENT")
	ready := os.Getenv("LIFECYCLE_FAKE_READY")

	signalReady := func() {
		if ready == "" {
			return
		}
		if err := os.MkdirAll(filepath.Dir(ready), 0o755); err != nil {
			fmt.Fprintln(os.Stderr, "ready mkdir failed:", err)
		}
		if err := os.WriteFile(ready, []byte("ready"), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "ready signal failed:", err)
		}
	}

	switch mode {
	case "ok":
		if err := os.WriteFile(out, []byte(content), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "write failed:", err)
			os.Exit(2)
		}
		os.Exit(0)
	case "slowok":
		signalReady()
		time.Sleep(1500 * time.Millisecond)
		if err := os.WriteFile(out, []byte(content), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "write failed:", err)
			os.Exit(2)
		}
		os.Exit(0)
	case "partial":
		if err := os.WriteFile(out, []byte("PARTIAL-"+content), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "write failed:", err)
			os.Exit(2)
		}
		signalReady()
		lifecycleFakeBlockUntilKilled()
	case "slow":
		signalReady()
		lifecycleFakeBlockUntilKilled()
	case "stdout":
		if _, err := io.WriteString(os.Stdout, content); err != nil {
			os.Exit(2)
		}
		os.Exit(0)
	case "fail":
		fmt.Fprintln(os.Stderr, "lifecycle fake ffmpeg failure")
		os.Exit(7)
	default:
		fmt.Fprintln(os.Stderr, "unknown fake ffmpeg mode:", mode)
		os.Exit(2)
	}
}

// lifecycleFakeBlockUntilKilled keeps the fake ffmpeg process alive until the
// request context terminates it. A bare select{} would trigger the test
// binary's "all goroutines are asleep - deadlock!" fatal exit, so wait on a
// real runtime ticker instead.
func lifecycleFakeBlockUntilKilled() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for range ticker.C {
	}
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

type lifecycleRun struct {
	mode     string
	out      string
	ready    string
	cmd      *exec.Cmd
	started  chan struct{}
	finished chan error
}

type lifecycleHarness struct {
	t    *testing.T
	root string
	obs  string
	p    *Processor

	mu   sync.Mutex
	runs []*lifecycleRun
}

func newLifecycleHarness(t *testing.T) *lifecycleHarness {
	t.Helper()
	root := t.TempDir()
	obs := t.TempDir()
	h := &lifecycleHarness{t: t, root: root, obs: obs}
	h.p = &Processor{
		TempRoot:         root,
		NewFFmpegCommand: h.newCommand,
	}
	h.p.OnCommandStart = h.onCommandStart
	h.p.OnCommandFinish = h.onCommandFinish
	return h
}

// newCommand is a Processor command factory returning the re-executed test
// binary in fake-ffmpeg mode.
func (h *lifecycleHarness) newCommand(ctx context.Context, args []string) *exec.Cmd {
	mode, out := parseLifecycleFakeArgs(args)

	h.mu.Lock()
	ready := filepath.Join(h.obs, "ready-"+strconv.Itoa(len(h.runs))+"-"+mode)
	run := &lifecycleRun{
		mode:     mode,
		out:      out,
		ready:    ready,
		started:  make(chan struct{}),
		finished: make(chan error, 1),
	}
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLifecycleFakeFFmpegHelper$")
	cmd.Env = append(os.Environ(),
		"LIFECYCLE_FAKE_HELPER=1",
		"LIFECYCLE_FAKE_MODE="+mode,
		"LIFECYCLE_FAKE_OUT="+out,
		"LIFECYCLE_FAKE_CONTENT="+lifecycleFakeContent(out),
		"LIFECYCLE_FAKE_READY="+ready,
	)
	run.cmd = cmd
	h.runs = append(h.runs, run)
	h.mu.Unlock()
	return cmd
}

func (h *lifecycleHarness) findRun(cmd *exec.Cmd) *lifecycleRun {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.runs {
		if r.cmd == cmd {
			return r
		}
	}
	return nil
}

func (h *lifecycleHarness) onCommandStart(cmd *exec.Cmd) {
	if r := h.findRun(cmd); r != nil {
		close(r.started)
	}
}

func (h *lifecycleHarness) onCommandFinish(cmd *exec.Cmd, runErr error) {
	if r := h.findRun(cmd); r != nil {
		select {
		case r.finished <- runErr:
		default:
		}
	}
}

func (h *lifecycleHarness) waitForMode(mode string) *lifecycleRun {
	h.t.Helper()
	deadline := time.Now().Add(lifecycleTestTimeout)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		for _, r := range h.runs {
			if r.mode == mode {
				h.mu.Unlock()
				return r
			}
		}
		h.mu.Unlock()
		time.Sleep(lifecyclePollInterval)
	}
	h.t.Fatalf("timed out waiting for fake ffmpeg run %q (runs=%d)", mode, h.runCount())
	return nil
}

func (h *lifecycleHarness) runCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.runs)
}

func (h *lifecycleHarness) waitStarted(r *lifecycleRun) {
	h.t.Helper()
	select {
	case <-r.started:
	case <-time.After(lifecycleTestTimeout):
		h.t.Fatal("timed out waiting for ffmpeg start hook")
	}
}

func (h *lifecycleHarness) waitFinished(r *lifecycleRun) error {
	h.t.Helper()
	select {
	case err := <-r.finished:
		return err
	case <-time.After(lifecycleTestTimeout):
		h.t.Fatal("timed out waiting for ffmpeg process to finish")
		return nil
	}
}

func parseLifecycleFakeArgs(args []string) (mode, out string) {
	var positional []string
	for _, arg := range args {
		if arg == "-hide_banner" || arg == "lifecycle-fake" {
			continue
		}
		positional = append(positional, arg)
	}
	if len(positional) == 0 {
		return "fail", ""
	}
	mode = positional[0]
	if len(positional) > 1 {
		out = positional[1]
	}
	return mode, out
}

func lifecycleFakeContent(out string) string {
	if out == "" {
		return "lifecycle-fake-stdout"
	}
	return "lifecycle-output:" + out
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

func lifecycleMarshalRequest(t *testing.T, req api.ProcessRequest) []byte {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return body
}

func lifecycleNewRequest(t *testing.T, ctx context.Context, target string, body []byte) *http.Request {
	t.Helper()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	r := httptest.NewRequest(http.MethodPost, target, reader)
	r.Header.Set("Content-Type", "application/json")
	if ctx != nil {
		r = r.WithContext(ctx)
	}
	return r
}

func lifecycleBase64Input(content string) api.Input {
	return api.Input{Base64: base64.StdEncoding.EncodeToString([]byte(content))}
}

// lifecycleWatchWriter records whether the job directory was already gone when the
// terminal response header was written.
type lifecycleWatchWriter struct {
	rec             *httptest.ResponseRecorder
	root            string
	headerWritten   bool
	code            int
	dirGoneAtHeader bool
}

func newLifecycleWatchWriter(rec *httptest.ResponseRecorder, root string) *lifecycleWatchWriter {
	return &lifecycleWatchWriter{rec: rec, root: root}
}

func (w *lifecycleWatchWriter) Header() http.Header { return w.rec.Header() }

func (w *lifecycleWatchWriter) WriteHeader(code int) {
	if !w.headerWritten {
		w.headerWritten = true
		w.code = code
		w.dirGoneAtHeader = len(lifecycleJobDirs(w.root)) == 0
	}
	w.rec.WriteHeader(code)
}

func (w *lifecycleWatchWriter) Write(p []byte) (int, error) {
	if !w.headerWritten {
		w.WriteHeader(http.StatusOK)
	}
	return w.rec.Write(p)
}

func (w *lifecycleWatchWriter) Flush() {
	w.rec.Flush()
}

func (w *lifecycleWatchWriter) assertCleanupBeforeHeader(t *testing.T) {
	t.Helper()
	if !w.headerWritten {
		t.Fatal("response had no header written")
	}
	if !w.dirGoneAtHeader {
		t.Fatalf("job directory still present when terminal response %d header was written", w.code)
	}
}

func lifecycleJobDirs(root string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var dirs []string
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), "ffmpeg-api-") {
			dirs = append(dirs, filepath.Join(root, entry.Name()))
		}
	}
	return dirs
}

func lifecycleWaitNoJobDirs(t *testing.T, root string) {
	t.Helper()
	deadline := time.Now().Add(lifecycleTestTimeout)
	var remaining []string
	for time.Now().Before(deadline) {
		remaining = lifecycleJobDirs(root)
		if len(remaining) == 0 {
			return
		}
		time.Sleep(lifecyclePollInterval)
	}
	t.Fatalf("job directories survived cleanup: %v", remaining)
}

func lifecycleWaitPath(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(lifecycleTestTimeout)
	for time.Now().Before(deadline) {
		info, err := os.Stat(path)
		if err == nil && !info.IsDir() {
			return
		}
		time.Sleep(lifecyclePollInterval)
	}
	t.Fatalf("timed out waiting for file %s", path)
}

func lifecycleWaitDirGone(t *testing.T, dir string) {
	t.Helper()
	deadline := time.Now().Add(lifecycleTestTimeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
			return
		}
		time.Sleep(lifecyclePollInterval)
	}
	t.Fatalf("directory %s was not removed", dir)
}

func lifecycleDirExists(dir string) bool {
	info, err := os.Stat(dir)
	return err == nil && info.IsDir()
}

type lifecycleHTTPResult struct {
	resp *http.Response
	err  error
}

func lifecycleDoPost(ctx context.Context, serverURL string, body []byte) <-chan lifecycleHTTPResult {
	ch := make(chan lifecycleHTTPResult, 1)
	go func() {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, serverURL+"/v1/process", bytes.NewReader(body))
		if err != nil {
			ch <- lifecycleHTTPResult{err: err}
			return
		}
		req.Header.Set("Content-Type", "application/json")
		transport := &http.Transport{}
		client := &http.Client{Transport: transport}
		defer transport.CloseIdleConnections()
		resp, err := client.Do(req)
		ch <- lifecycleHTTPResult{resp: resp, err: err}
	}()
	return ch
}

func lifecycleAwait[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(lifecycleTestTimeout):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

// ---------------------------------------------------------------------------
// A1 + A7 + A6: successful request
// ---------------------------------------------------------------------------

func TestLifecycleSuccessCollectsOnlyOutputsAndCleansDir(t *testing.T) {
	h := newLifecycleHarness(t)

	inputSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.WriteString(w, "downloaded-input-bytes"); err != nil {
			t.Errorf("input server write: %v", err)
		}
	}))
	defer inputSrv.Close()

	req := api.ProcessRequest{
		Inputs: map[string]api.Input{
			"input.txt":   {HTTP: inputSrv.URL + "/input.txt"},
			"scratch.tmp": {Temporary: true},
		},
		Commands: [][]string{
			{"lifecycle-fake", "ok", "output.bin"},
		},
		Output: api.Output{Base64: true},
	}
	body := lifecycleMarshalRequest(t, req)
	rec := httptest.NewRecorder()
	watch := newLifecycleWatchWriter(rec, h.root)

	h.p.ServeHTTP(watch, lifecycleNewRequest(t, context.Background(), "/v1/process", body))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	watch.assertCleanupBeforeHeader(t)

	var resp api.ProcessResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v\nbody=%s", err, rec.Body.String())
	}
	if len(resp.Results) != 1 {
		t.Fatalf("expected exactly one result, got %#v", resp.Results)
	}
	result, ok := resp.Results["output.bin"]
	if !ok {
		t.Fatalf("output.bin missing from results: %#v", resp.Results)
	}
	want := base64.StdEncoding.EncodeToString([]byte(lifecycleFakeContent("output.bin")))
	if result.Base64 != want {
		t.Fatalf("base64 mismatch: got %q want %q", result.Base64, want)
	}
	if result.URL != "" {
		t.Fatalf("unexpected URL result: %q", result.URL)
	}
	lifecycleWaitNoJobDirs(t, h.root)
}

// Inline output keeps its baseline content type and bytes, and the job
// directory is removed once ServeFile has finished sending the file.
func TestLifecycleInlineOutputServedAfterCleanup(t *testing.T) {
	h := newLifecycleHarness(t)
	req := api.ProcessRequest{
		Inputs:   map[string]api.Input{"input.txt": lifecycleBase64Input("hello")},
		Commands: [][]string{{"lifecycle-fake", "ok", "clip.dat"}},
		Output:   api.Output{InlineContentType: "application/octet-stream"},
	}
	body := lifecycleMarshalRequest(t, req)
	rec := httptest.NewRecorder()
	watch := newLifecycleWatchWriter(rec, h.root)

	h.p.ServeHTTP(watch, lifecycleNewRequest(t, context.Background(), "/v1/process", body))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("content type = %q", ct)
	}
	if rec.Body.String() != lifecycleFakeContent("clip.dat") {
		t.Fatalf("inline body = %q", rec.Body.String())
	}
	// Inline serving finishes (and closes) the file before cleanup, so the
	// directory is gone by the time the handler returns.
	lifecycleWaitNoJobDirs(t, h.root)
}

// Streaming mode keeps its baseline body behavior and still cleans the job
// directory before the handler finishes (trailers terminate the response).
func TestLifecycleStreamingOutputAndCleanup(t *testing.T) {
	h := newLifecycleHarness(t)
	req := api.ProcessRequest{
		Inputs:   map[string]api.Input{"input.txt": lifecycleBase64Input("hello")},
		Commands: [][]string{{"lifecycle-fake", "stdout", "pipe:1"}},
		Output: api.Output{
			InlineContentType: "application/octet-stream",
			Stream:            true,
		},
	}
	body := lifecycleMarshalRequest(t, req)
	rec := httptest.NewRecorder()

	h.p.ServeHTTP(rec, lifecycleNewRequest(t, context.Background(), "/v1/process", body))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != lifecycleFakeContent("pipe:1") {
		t.Fatalf("streamed body = %q", rec.Body.String())
	}
	lifecycleWaitNoJobDirs(t, h.root)
}

// ---------------------------------------------------------------------------
// A2 + A8 + A6: download failure
// ---------------------------------------------------------------------------

func TestLifecycleDownloadFailureCancelsSiblingsNoFFmpegNoUploadCleanup(t *testing.T) {
	h := newLifecycleHarness(t)

	var ffmpegCalls atomic.Int32
	h.p.NewFFmpegCommand = func(ctx context.Context, args []string) *exec.Cmd {
		ffmpegCalls.Add(1)
		// Even if wrongly invoked, return a prompt failing fake command.
		return h.newCommand(ctx, []string{"lifecycle-fake", "fail"})
	}

	slowStarted := make(chan struct{})
	var startOnce sync.Once
	siblingCanceled := make(chan struct{}, 1)

	downloadSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/slow":
			startOnce.Do(func() { close(slowStarted) })
			<-r.Context().Done()
			select {
			case siblingCanceled <- struct{}{}:
			default:
			}
		case "/missing":
			// Make sure the sibling download is actually in flight so the
			// assertion proves cancellation rather than a never-started fetch.
			select {
			case <-slowStarted:
			case <-time.After(lifecycleTestTimeout):
				t.Error("slow download never started")
			}
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer downloadSrv.Close()

	var s3Hits atomic.Int64
	s3Srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s3Hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer s3Srv.Close()

	useSSL := false
	req := api.ProcessRequest{
		S3Config: api.S3Config{
			Endpoint:  strings.TrimPrefix(s3Srv.URL, "http://"),
			Region:    "us-east-1",
			AccessKey: "AKIA-TEST",
			SecretKey: "secret-test",
			UseSSL:    &useSSL,
		},
		Inputs: map[string]api.Input{
			"missing.bin": {HTTP: downloadSrv.URL + "/missing"},
			"slow.bin":    {HTTP: downloadSrv.URL + "/slow"},
		},
		Commands: [][]string{{"lifecycle-fake", "ok", "output.bin"}},
		Output:   api.Output{S3: "s3://bucket/out/", Base64: true},
	}
	body := lifecycleMarshalRequest(t, req)
	rec := httptest.NewRecorder()
	watch := newLifecycleWatchWriter(rec, h.root)

	h.p.ServeHTTP(watch, lifecycleNewRequest(t, context.Background(), "/v1/process", body))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"results"`) {
		t.Fatalf("error response must not contain results: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"base64"`) {
		t.Fatalf("error response must not contain base64 output: %s", rec.Body.String())
	}
	watch.assertCleanupBeforeHeader(t)

	select {
	case <-siblingCanceled:
	case <-time.After(lifecycleTestTimeout):
		t.Fatal("sibling download was not canceled after the other input failed")
	}
	if calls := ffmpegCalls.Load(); calls != 0 {
		t.Fatalf("ffmpeg was invoked %d times after download failure", calls)
	}
	if hits := s3Hits.Load(); hits != 0 {
		t.Fatalf("S3 endpoint received %d requests after download failure", hits)
	}
	lifecycleWaitNoJobDirs(t, h.root)
}

// ---------------------------------------------------------------------------
// A3: real client disconnect while FFmpeg is running
// ---------------------------------------------------------------------------

func TestLifecycleClientDisconnectStopsFFmpegAndCleansDir(t *testing.T) {
	h := newLifecycleHarness(t)
	srv := httptest.NewServer(http.HandlerFunc(h.p.ServeHTTP))
	defer srv.Close()

	req := api.ProcessRequest{
		Inputs:   map[string]api.Input{"input.txt": lifecycleBase64Input("hello")},
		Commands: [][]string{{"lifecycle-fake", "partial", "victim.bin"}},
		Output:   api.Output{Base64: true},
	}
	body := lifecycleMarshalRequest(t, req)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resultCh := lifecycleDoPost(ctx, srv.URL, body)

	run := h.waitForMode("partial")
	h.waitStarted(run)
	lifecycleWaitPath(t, run.ready)
	lifecycleWaitPath(t, filepath.Join(run.cmd.Dir, "victim.bin"))
	victimDir := run.cmd.Dir

	cancel()

	if err := h.waitFinished(run); err == nil {
		t.Fatal("canceled ffmpeg process exited successfully; expected kill error")
	}
	lifecycleWaitDirGone(t, victimDir)
	lifecycleWaitNoJobDirs(t, h.root)

	result := lifecycleAwait(t, resultCh, "disconnected HTTP call to return")
	if result.err == nil {
		if result.resp == nil {
			t.Fatal("client Do returned nil response and nil error")
		}
		if result.resp.StatusCode == http.StatusOK {
			data, _ := io.ReadAll(result.resp.Body)
			t.Fatalf("canceled request returned 200 with results: %s", data)
		}
		result.resp.Body.Close()
	}
}

// ---------------------------------------------------------------------------
// A5: cancellation at the collection boundary must not return half products
// ---------------------------------------------------------------------------

func TestLifecycleCancelBeforeCollectionRejectsPartialOutput(t *testing.T) {
	h := newLifecycleHarness(t)

	parentCtx, cancel := context.WithCancel(context.Background())
	// Simulate the client going away exactly as FFmpeg finishes: cancel after
	// the last command but before result collection.
	h.p.AfterFFmpeg = cancel

	req := api.ProcessRequest{
		Inputs:   map[string]api.Input{"input.txt": lifecycleBase64Input("hello")},
		Commands: [][]string{{"lifecycle-fake", "ok", "half.bin"}},
		Output:   api.Output{Base64: true},
	}
	body := lifecycleMarshalRequest(t, req)
	rec := httptest.NewRecorder()
	watch := newLifecycleWatchWriter(rec, h.root)

	h.p.ServeHTTP(watch, lifecycleNewRequest(t, parentCtx, "/v1/process", body))

	if rec.Code == http.StatusOK {
		t.Fatalf("canceled request must not return 200, body=%s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "half.bin") || strings.Contains(rec.Body.String(), `"results"`) {
		t.Fatalf("partial output leaked into response: %s", rec.Body.String())
	}
	watch.assertCleanupBeforeHeader(t)
	lifecycleWaitNoJobDirs(t, h.root)
}

// Direct guard: an already canceled context makes collectResults fail before
// reading/encoding/uploading anything.
func TestLifecycleCollectResultsCanceledStartsNoUpload(t *testing.T) {
	h := newLifecycleHarness(t)

	var s3Hits atomic.Int64
	s3Srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s3Hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer s3Srv.Close()
	useSSL := false
	client := internal.GetS3Client(&api.S3Config{
		Endpoint:  strings.TrimPrefix(s3Srv.URL, "http://"),
		Region:    "us-east-1",
		AccessKey: "a",
		SecretKey: "b",
		UseSSL:    &useSSL,
	})

	// Place an output-looking file in a job directory.
	jobDir, cleanup, err := createJobPathIn(h.root)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if err := os.WriteFile(filepath.Join(jobDir, "half.bin"), []byte("half"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := api.ProcessRequest{Output: api.Output{S3: "s3://bucket/out/", Base64: true}}
	results, err := collectResults(ctx, client, req, jobDir)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if results != nil {
		t.Fatalf("expected nil results, got %#v", results)
	}
	if hits := s3Hits.Load(); hits != 0 {
		t.Fatalf("upload started despite cancellation: %d hits", hits)
	}
}

// ---------------------------------------------------------------------------
// A4: two concurrent requests, one canceled
// ---------------------------------------------------------------------------

func TestLifecycleConcurrentRequestsOneCanceledOtherSurvives(t *testing.T) {
	h := newLifecycleHarness(t)
	srv := httptest.NewServer(http.HandlerFunc(h.p.ServeHTTP))
	defer srv.Close()

	victimReq := api.ProcessRequest{
		Inputs:   map[string]api.Input{"in.txt": lifecycleBase64Input("victim")},
		Commands: [][]string{{"lifecycle-fake", "partial", "victim.bin"}},
		Output:   api.Output{Base64: true},
	}
	otherReq := api.ProcessRequest{
		Inputs:   map[string]api.Input{"in.txt": lifecycleBase64Input("other")},
		Commands: [][]string{{"lifecycle-fake", "slowok", "other.bin"}},
		Output:   api.Output{Base64: true},
	}
	victimBody := lifecycleMarshalRequest(t, victimReq)
	otherBody := lifecycleMarshalRequest(t, otherReq)

	victimCtx, cancelVictim := context.WithCancel(context.Background())
	defer cancelVictim()

	// Launch both close together.
	victimCh := lifecycleDoPost(victimCtx, srv.URL, victimBody)
	otherCh := lifecycleDoPost(context.Background(), srv.URL, otherBody)

	victimRun := h.waitForMode("partial")
	otherRun := h.waitForMode("slowok")
	h.waitStarted(victimRun)
	h.waitStarted(otherRun)

	victimDir := victimRun.cmd.Dir
	otherDir := otherRun.cmd.Dir
	if victimDir == otherDir {
		t.Fatalf("requests share job directory %s", victimDir)
	}

	lifecycleWaitPath(t, victimRun.ready)
	lifecycleWaitPath(t, otherRun.ready)
	lifecycleWaitPath(t, filepath.Join(victimDir, "victim.bin"))
	if !lifecycleDirExists(otherDir) {
		t.Fatalf("other request dir %s missing before cancellation", otherDir)
	}

	// Cancel only the victim.
	cancelVictim()

	if err := h.waitFinished(victimRun); err == nil {
		t.Fatal("victim ffmpeg should have been killed")
	}
	lifecycleWaitDirGone(t, victimDir)

	// The other FFmpeg must still be running and its directory untouched.
	select {
	case err := <-otherRun.finished:
		t.Fatalf("other request ffmpeg was disturbed by sibling cancellation (err=%v)", err)
	case <-time.After(300 * time.Millisecond):
	}
	if !lifecycleDirExists(otherDir) {
		t.Fatal("other request job directory was deleted early")
	}

	// The survivor completes normally with its own output.
	otherResult := lifecycleAwait(t, otherCh, "surviving request response")
	if otherResult.err != nil {
		t.Fatalf("surviving request failed: %v", otherResult.err)
	}
	if otherResult.resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(otherResult.resp.Body)
		t.Fatalf("surviving status = %d body=%s", otherResult.resp.StatusCode, data)
	}
	var resp api.ProcessResponse
	if err := json.NewDecoder(otherResult.resp.Body).Decode(&resp); err != nil {
		t.Fatalf("decode survivor response: %v", err)
	}
	otherResult.resp.Body.Close()
	result, ok := resp.Results["other.bin"]
	if !ok {
		t.Fatalf("other.bin missing: %#v", resp.Results)
	}
	if want := base64.StdEncoding.EncodeToString([]byte(lifecycleFakeContent("other.bin"))); result.Base64 != want {
		t.Fatalf("survivor output mismatch: %q", result.Base64)
	}

	if err := h.waitFinished(otherRun); err != nil {
		t.Fatalf("surviving ffmpeg exited with error: %v", err)
	}
	lifecycleWaitDirGone(t, otherDir)
	lifecycleWaitNoJobDirs(t, h.root)

	// The victim's client call must also have returned, unsuccessfully.
	victimResult := lifecycleAwait(t, victimCh, "victim client return")
	if victimResult.err == nil {
		if victimResult.resp != nil && victimResult.resp.StatusCode == http.StatusOK {
			data, _ := io.ReadAll(victimResult.resp.Body)
			t.Fatalf("canceled victim returned 200: %s", data)
		}
		if victimResult.resp != nil {
			victimResult.resp.Body.Close()
		}
	}
}

// ---------------------------------------------------------------------------
// Package-level wiring (handleProcess / defaultProcessor / /v1/process)
// ---------------------------------------------------------------------------

func TestLifecyclePackageHandleProcessWiring(t *testing.T) {
	h := newLifecycleHarness(t)
	old := defaultProcessor
	defaultProcessor = h.p
	t.Cleanup(func() { defaultProcessor = old })

	srv := httptest.NewServer(http.HandlerFunc(handleProcess))
	defer srv.Close()

	req := api.ProcessRequest{
		Inputs:   map[string]api.Input{"input.txt": lifecycleBase64Input("hello")},
		Commands: [][]string{{"lifecycle-fake", "ok", "wired.bin"}},
		Output:   api.Output{Base64: true},
	}
	body := lifecycleMarshalRequest(t, req)
	ch := lifecycleDoPost(context.Background(), srv.URL, body)
	result := lifecycleAwait(t, ch, "wired request")
	if result.err != nil {
		t.Fatalf("post: %v", result.err)
	}
	if result.resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(result.resp.Body)
		t.Fatalf("status %d body=%s", result.resp.StatusCode, data)
	}
	result.resp.Body.Close()
	lifecycleWaitNoJobDirs(t, h.root)
}

// ---------------------------------------------------------------------------
// Optional real-ffmpeg smoke test (skipped when no ffmpeg is available)
// ---------------------------------------------------------------------------

func TestLifecycleRealFFmpegSmoke(t *testing.T) {
	bin := os.Getenv("LIFECYCLE_FFMPEG_BIN")
	if bin == "" {
		if found, err := exec.LookPath("ffmpeg"); err == nil {
			bin = found
		}
	}
	if bin == "" {
		t.Skip("no ffmpeg binary on PATH and LIFECYCLE_FFMPEG_BIN unset")
	}

	p := &Processor{TempRoot: t.TempDir(), FFmpegPath: bin}
	req := api.ProcessRequest{
		Commands: [][]string{{
			"-f", "lavfi", "-i", "color=c=red:s=64x64:d=0.1",
			"-frames:v", "1", "real-out.png",
		}},
		Output: api.Output{Base64: true},
	}
	body := lifecycleMarshalRequest(t, req)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, lifecycleNewRequest(t, context.Background(), "/v1/process", body))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp api.ProcessResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	result, ok := resp.Results["real-out.png"]
	if !ok {
		t.Fatalf("real-out.png missing: %#v", resp.Results)
	}
	data, err := base64.StdEncoding.DecodeString(result.Base64)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 8 || string(data[1:4]) != "PNG" {
		t.Fatalf("decoded output is not a PNG (%d bytes)", len(data))
	}
	if dirs := lifecycleJobDirs(p.TempRoot); len(dirs) != 0 {
		t.Fatalf("job dirs remain: %v", dirs)
	}
}
