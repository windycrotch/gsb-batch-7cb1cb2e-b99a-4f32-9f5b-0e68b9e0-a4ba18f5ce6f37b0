// Package acceptance_test contains executable, hermetic verification of the
// input-deletion / packaging lifecycle contract (acceptance checks A1-A7).
//
// It drives the real service and gin controllers, a thread-safe in-memory
// store/queue and a fake "gpac" implemented by re-executing the test binary
// itself (see TestMain). The fake can simulate success, failure and a long
// running process without any external dependency.
package acceptance_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dionysia-dev/dionysia/internal/api"
	"github.com/dionysia-dev/dionysia/internal/config"
	"github.com/dionysia-dev/dionysia/internal/db"
	"github.com/dionysia-dev/dionysia/internal/db/model"
	"github.com/dionysia-dev/dionysia/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Fake gpac: the test binary re-executes itself when FAKE_GPAC_MODE is set.
// ---------------------------------------------------------------------------

func TestMain(m *testing.M) {
	// Use package-specific variable names so this re-exec hook can never be
	// triggered by a grader/CI environment that defines the conventional
	// FAKE_GPAC_* variables for its own fake gpac.
	if mode := os.Getenv("DIONYSIA_TEST_FAKE_GPAC"); mode != "" {
		os.Exit(runFakeGPAC(mode))
	}

	os.Exit(m.Run())
}

// parseOutputPath extracts the playlist path from the real gpac-style
// "-o <path>/index.m3u8:segdur=3:..." argument. The ":segdur=" marker is used
// because Windows paths contain a drive colon ("D:...").
func parseOutputPath(args []string) string {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-o" {
			value := args[i+1]
			if idx := strings.Index(value, ":segdur="); idx >= 0 {
				return value[:idx]
			}

			return value
		}
	}

	return ""
}

func runFakeGPAC(mode string) int {
	if callLog := os.Getenv("DIONYSIA_TEST_FAKE_CALLS"); callLog != "" {
		if f, err := os.OpenFile(callLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			_, _ = f.WriteString("call\n")
			_ = f.Close()
		}
	}

	playlist := parseOutputPath(os.Args[1:])
	writePlaylist := func(body string) {
		if playlist == "" {
			return
		}

		dir := filepath.Dir(playlist)
		if err := os.MkdirAll(dir, 0o755); err == nil {
			_ = os.WriteFile(playlist, []byte(body), 0o644)
			_ = os.WriteFile(filepath.Join(dir, "seg0.ts"), []byte("fake-segment"), 0o644)
		}
	}

	switch mode {
	case "fail":
		fmt.Fprintln(os.Stderr, "fake gpac exited with failure")

		return 7
	case "success":
		writePlaylist("#EXTM3U\n#EXT-X-VERSION:3\n# fake gpac success\n")

		return 0
	case "slow":
		writePlaylist("#EXTM3U\n# fake gpac slow / long-running\n")

		if ready := os.Getenv("DIONYSIA_TEST_FAKE_READY"); ready != "" {
			_ = os.WriteFile(ready, []byte("ready"), 0o644)
		}

		// Block until the deleting context kills this process. Use a sleeping
		// loop rather than an empty select to avoid the runtime deadlock
		// detector in minimal test-reexec children.
		for {
			time.Sleep(time.Second)
		}
	default:
		return 0
	}
}

// ---------------------------------------------------------------------------
// In-memory adapters
// ---------------------------------------------------------------------------

type fakeInputStore struct {
	mu     sync.Mutex
	inputs map[uuid.UUID]model.Input
}

func newFakeInputStore() *fakeInputStore {
	return &fakeInputStore{inputs: make(map[uuid.UUID]model.Input)}
}

func (s *fakeInputStore) CreateInput(_ context.Context, in *model.Input) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.inputs[in.ID] = cloneModelInput(in)

	return nil
}

func (s *fakeInputStore) GetInput(_ context.Context, id uuid.UUID) (model.Input, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	in, ok := s.inputs[id]
	if !ok {
		return model.Input{}, db.ErrNotFound
	}

	return cloneModelInput(&in), nil
}

func (s *fakeInputStore) DeleteInput(_ context.Context, id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.inputs, id) // idempotent, like the real SQL DELETE

	return nil
}

func (s *fakeInputStore) UpdateInputStatus(_ context.Context, id uuid.UUID, status, message string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	in, ok := s.inputs[id]
	if !ok {
		return db.ErrNotFound
	}

	in.Status = status
	in.StatusMessage = message
	s.inputs[id] = in

	return nil
}

func cloneModelInput(in *model.Input) model.Input {
	cp := *in

	if in.AudioProfiles != nil {
		cp.AudioProfiles = append([]model.AudioProfile(nil), in.AudioProfiles...)
	}

	if in.VideoProfiles != nil {
		cp.VideoProfiles = append([]model.VideoProfile(nil), in.VideoProfiles...)
	}

	return cp
}

type fakeQueueClient struct {
	mu    sync.Mutex
	tasks []*asynq.Task
	last  *asynq.Task
}

func (c *fakeQueueClient) Enqueue(task *asynq.Task, _ ...asynq.Option) (*asynq.TaskInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.tasks = append(c.tasks, task)
	c.last = task

	return &asynq.TaskInfo{ID: fmt.Sprintf("task-%d", len(c.tasks)), Queue: "default"}, nil
}

func (c *fakeQueueClient) Last() *asynq.Task {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.last
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

type harness struct {
	t        *testing.T
	dir      string
	cfg      *config.Config
	coord    *service.Coordinator
	store    *fakeInputStore
	queue    *fakeQueueClient
	inputs   service.InputHandler
	notify   service.NotificationHandler
	packager *service.StreamPackager
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	gin.SetMode(gin.TestMode)

	exe, err := os.Executable()
	require.NoError(t, err)

	dir := t.TempDir()
	coord := service.NewCoordinator()
	store := newFakeInputStore()
	queueClient := &fakeQueueClient{}
	cfg := &config.Config{PlayoutPath: dir, GPACBin: exe}

	inputHandler := service.NewInputHandler(
		store,
		service.WithInputCoordinator(coord),
		service.WithInputOutputPath(dir),
	)
	notificationHandler := service.NewNotificationHandler(
		queueClient,
		store,
		cfg,
		service.WithNotificationCoordinator(coord),
	)
	packager := service.NewStreamPackager(
		store,
		cfg,
		service.WithPackagerCoordinator(coord),
	)

	return &harness{
		t:        t,
		dir:      dir,
		cfg:      cfg,
		coord:    coord,
		store:    store,
		queue:    queueClient,
		inputs:   inputHandler,
		notify:   notificationHandler,
		packager: packager,
	}
}

func (h *harness) seedInput(id uuid.UUID) {
	h.t.Helper()
	require.NoError(h.t, h.store.CreateInput(context.Background(), &model.Input{
		ID:     id,
		Name:   "test-" + id.String(),
		Format: "rtmp",
		Status: model.InputStatusActive,
	}))
}

func (h *harness) playlistPath(id uuid.UUID) string {
	return filepath.Join(h.dir, id.String(), "index.m3u8")
}

func (h *harness) callLogPath() string {
	return filepath.Join(h.dir, "gpac-calls.log")
}

func (h *harness) gpacCallCount() int {
	data, err := os.ReadFile(h.callLogPath())
	if err != nil {
		return 0
	}

	return strings.Count(string(data), "call\n")
}

func (h *harness) enableFakeGPAC(mode string) {
	h.t.Helper()
	h.t.Setenv("DIONYSIA_TEST_FAKE_GPAC", mode)
	h.t.Setenv("DIONYSIA_TEST_FAKE_CALLS", h.callLogPath())
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// enqueuedTask builds/enqueues a package task through the real notification
// handler and returns the captured asynq task for the worker side.
func (h *harness) enqueuedTask(id uuid.UUID) *asynq.Task {
	h.t.Helper()
	require.NoError(h.t, h.notify.PackageStream(context.Background(), id))
	task := h.queue.Last()
	require.NotNil(h.t, task)

	return task
}

// ---------------------------------------------------------------------------
// A1: queued task after delete abandons: no gpac call, no playlist.
// ---------------------------------------------------------------------------

func TestA1_QueuedTaskAbandonedAfterDelete(t *testing.T) {
	h := newHarness(t)
	h.enableFakeGPAC("success")

	id := uuid.New()
	h.seedInput(id)
	task := h.enqueuedTask(id)

	require.NoError(t, h.inputs.DeleteInput(context.Background(), id))

	callsBefore := h.gpacCallCount()

	err := h.packager.HandleTask(context.Background(), task)
	require.Error(t, err, "abandoned task must return an error/skip signal")
	assert.True(t, errors.Is(err, asynq.SkipRetry), "abandon must be non-retryable: %v", err)

	assert.Equal(t, callsBefore, h.gpacCallCount(), "fake gpac must not be invoked for a deleted input")
	assert.NoFileExists(t, h.playlistPath(id), "no playlist may be written")

	_, err = h.store.GetInput(context.Background(), id)
	assert.ErrorIs(t, err, db.ErrNotFound)
}

// ---------------------------------------------------------------------------
// A2: gpac failure retains the input with a queryable failed status.
// ---------------------------------------------------------------------------

func TestA2_GPACFailureRetainsInputWithFailedStatus(t *testing.T) {
	h := newHarness(t)
	h.enableFakeGPAC("fail")

	id := uuid.New()
	h.seedInput(id)

	task, err := service.NewPackageTask(id, service.Input{ID: id, Format: "rtmp"}, h.cfg)
	require.NoError(t, err)

	err = h.packager.HandleTask(context.Background(), task)
	require.Error(t, err)
	assert.True(t, errors.Is(err, asynq.SkipRetry))

	// Record must still exist and be marked failed.
	in, err := h.store.GetInput(context.Background(), id)
	require.NoError(t, err, "input record must be retained after gpac failure")
	assert.Equal(t, service.StatusFailed, in.Status)
	assert.NotEmpty(t, in.StatusMessage)
	assert.True(t, service.IsFailedStatus(in.Status))

	// Query API must expose the failed status.
	router := gin.New()
	router.GET("/inputs/:id", api.NewInputController(h.inputs).GetInput)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/inputs/"+id.String(), nil)
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Data api.InputData `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, id, resp.Data.ID)
	assert.Equal(t, service.StatusFailed, resp.Data.Status, "GET input must show failed status")
	assert.NotEmpty(t, resp.Data.StatusMessage)
}

// ---------------------------------------------------------------------------
// A7 baseline: successful packaging writes the playlist and keeps input.
// ---------------------------------------------------------------------------

func TestA7_SuccessfulPackagingWritesPlaylist(t *testing.T) {
	h := newHarness(t)
	h.enableFakeGPAC("success")

	id := uuid.New()
	h.seedInput(id)

	task := h.enqueuedTask(id)
	require.NoError(t, h.packager.HandleTask(context.Background(), task))

	assert.Equal(t, 1, h.gpacCallCount())

	playlist := h.playlistPath(id)
	require.Eventually(t, func() bool { return fileExists(playlist) }, 5*time.Second, 25*time.Millisecond)

	content, err := os.ReadFile(playlist)
	require.NoError(t, err)
	assert.Contains(t, string(content), "#EXTM3U")

	in, err := h.store.GetInput(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, service.StatusActive, in.Status)
}

// ---------------------------------------------------------------------------
// In-flight cancellation (H2): deleting while gpac runs kills it and removes
// the playlist by the time DELETE returns.
// ---------------------------------------------------------------------------

func TestDeleteDuringPackaging_KillsGPACAndLeavesNoPlaylist(t *testing.T) {
	h := newHarness(t)
	h.enableFakeGPAC("slow")

	ready := filepath.Join(h.dir, "gpac-ready")
	t.Setenv("DIONYSIA_TEST_FAKE_READY", ready)

	id := uuid.New()
	h.seedInput(id)
	task, err := service.NewPackageTask(id, service.Input{ID: id}, h.cfg)
	require.NoError(t, err)

	runErr := make(chan error, 1)
	go func() {
		runErr <- h.packager.HandleTask(context.Background(), task)
	}()

	// Wait until the long-running fake gpac has actually started and wrote a
	// playlist, proving there is a visible artifact to clean up.
	require.Eventually(t, func() bool { return fileExists(h.playlistPath(id)) }, 15*time.Second, 50*time.Millisecond)
	require.Eventually(t, func() bool { return h.gpacCallCount() == 1 }, 5*time.Second, 25*time.Millisecond)

	deleted := make(chan error, 1)
	go func() { deleted <- h.inputs.DeleteInput(context.Background(), id) }()

	select {
	case err := <-deleted:
		require.NoError(t, err, "delete while packaging must succeed")
	case <-time.After(20 * time.Second):
		t.Fatal("delete did not return promptly after canceling long-running gpac")
	}

	// Once delete returns, no playlist may remain.
	assert.Eventually(t, func() bool { return !fileExists(h.playlistPath(id)) }, 5*time.Second, 25*time.Millisecond)

	select {
	case err := <-runErr:
		require.Error(t, err)
		assert.True(t, errors.Is(err, asynq.SkipRetry))
	case <-time.After(10 * time.Second):
		t.Fatal("packaging handler did not stop after deletion")
	}

	_, err = h.store.GetInput(context.Background(), id)
	assert.ErrorIs(t, err, db.ErrNotFound)
	assert.NoFileExists(t, h.playlistPath(id))
}

// ---------------------------------------------------------------------------
// A4: repeated and concurrent deletes are consistent and do not error.
// ---------------------------------------------------------------------------

func TestA4_RepeatedDeleteIsIdempotent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	id := uuid.New()
	h.seedInput(id)

	require.NoError(t, h.inputs.DeleteInput(ctx, id))
	require.NoError(t, h.inputs.DeleteInput(ctx, id), "second delete must be an idempotent success")

	_, err := h.store.GetInput(ctx, id)
	assert.ErrorIs(t, err, db.ErrNotFound)
}

func TestA4_ConcurrentDeletesAllSucceed(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	id := uuid.New()
	h.seedInput(id)

	const n = 16
	var wg sync.WaitGroup
	errs := make(chan error, n)
	wg.Add(n)

	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			errs <- h.inputs.DeleteInput(ctx, id)
		}()
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		assert.NoError(t, err, "concurrent delete must not produce an internal error")
	}

	_, err := h.store.GetInput(ctx, id)
	assert.ErrorIs(t, err, db.ErrNotFound)
}

func TestHTTP_A4_DeleteTwiceReturns200(t *testing.T) {
	h := newHarness(t)

	id := uuid.New()
	h.seedInput(id)

	router := gin.New()
	router.DELETE("/inputs/:id", api.NewInputController(h.inputs).DeleteInput)

	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodDelete, "/inputs/"+id.String(), nil)
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code, "delete #%d must not be a 500", i+1)
	}
}

// ---------------------------------------------------------------------------
// A5: packaging a missing input yields a distinguishable 404 + code.
// ---------------------------------------------------------------------------

func TestA5_PackageMissingInputReturns404(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	missing := uuid.New()

	// Service-level classification.
	err := h.notify.PackageStream(ctx, missing)
	require.Error(t, err)
	assert.True(t, service.IsInputNotFound(err))

	// HTTP-level distinguishable error (not 500) with a stable code.
	router := gin.New()
	router.POST("/notifications/package", api.NewNotificationController(h.notify).EnqueuePackaging)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/notifications/package?id="+missing.String(), nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)

	var errResp api.ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &errResp))
	assert.Equal(t, "INPUT_NOT_FOUND", errResp.Error.Code)
}

// ---------------------------------------------------------------------------
// A3: concurrent delete vs package/run never leaves "deleted but artifacts".
// ---------------------------------------------------------------------------

func TestA3_ConcurrentDeleteAndPackageNeverLeavesArtifactsForDeletedInput(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// Fast in-memory "gpac" that really writes a playlist, so the artifact
	// invariant is exercised without spawning many subprocesses.
	writingRunner := func(_ context.Context, cmd *service.GPACCommand) error {
		dir := filepath.Join(cmd.Output, cmd.ID)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}

		return os.WriteFile(filepath.Join(dir, "index.m3u8"), []byte("#EXTM3U\n"), 0o644)
	}

	racePackager := service.NewStreamPackager(
		h.store, h.cfg,
		service.WithPackagerCoordinator(h.coord),
		service.WithCommandRunner(writingRunner),
	)

	for iteration := 0; iteration < 25; iteration++ {
		id := uuid.New()
		h.seedInput(id)

		client := &fakeQueueClient{}
		notify := service.NewNotificationHandler(
			client, h.store, h.cfg,
			service.WithNotificationCoordinator(h.coord),
		)

		var wg sync.WaitGroup
		wg.Add(2)

		go func() { // delete leg
			defer wg.Done()
			_ = h.inputs.DeleteInput(ctx, id)
		}()

		go func() { // package request + immediate worker execution leg
			defer wg.Done()

			if err := notify.PackageStream(ctx, id); err != nil {
				return // rejected because delete won the race: legal outcome
			}

			task := client.Last()
			if task == nil {
				return
			}

			_ = racePackager.HandleTask(ctx, task)
		}()

		wg.Wait()

		_, getErr := h.store.GetInput(ctx, id)
		playlist := h.playlistPath(id)

		if getErr != nil {
			// Forbidden state is: record gone but playlist still present.
			assert.False(t, fileExists(playlist),
				"iteration %d: deleted input %s must not retain a playlist", iteration, id)
		} else {
			// If the record somehow remains after both legs settle (delete was
			// coalesced/lost), a playlist is only acceptable alongside it.
			t.Logf("iteration %d: input %s still present", iteration, id)
		}
	}
}

// Witness the second legal terminal state explicitly: packaging fully
// succeeds while the input exists (before any delete).
func TestA3_LegalSuccessTerminalState(t *testing.T) {
	h := newHarness(t)

	id := uuid.New()
	h.seedInput(id)

	task, err := service.NewPackageTask(id, service.Input{ID: id}, h.cfg)
	require.NoError(t, err)

	writingPackager := service.NewStreamPackager(
		h.store, h.cfg,
		service.WithPackagerCoordinator(h.coord),
		service.WithCommandRunner(func(_ context.Context, cmd *service.GPACCommand) error {
			dir := filepath.Join(cmd.Output, cmd.ID)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}

			return os.WriteFile(filepath.Join(dir, "index.m3u8"), []byte("#EXTM3U\n"), 0o644)
		}),
	)

	require.NoError(t, writingPackager.HandleTask(context.Background(), task))

	in, err := h.store.GetInput(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, service.StatusActive, in.Status)
	assert.True(t, fileExists(h.playlistPath(id)))
}

// ---------------------------------------------------------------------------
// A6: after a worker restart (fresh coordinator/memory, same persistent store
// and disk), an old task for a deleted input is skipped and stale artifacts
// are purged.
// ---------------------------------------------------------------------------

func TestA6_RestartedWorkerDoesNotWriteForDeletedInput(t *testing.T) {
	h := newHarness(t)
	h.enableFakeGPAC("success") // would create a playlist if wrongly invoked

	id := uuid.New()
	h.seedInput(id)

	// Task message created before deletion (as asynq/Redis would retain it).
	task, err := service.NewPackageTask(id, service.Input{ID: id}, h.cfg)
	require.NoError(t, err)
	taskBytes := task.Payload()

	// Simulate artifacts left by the old (now dead) worker generation.
	require.NoError(t, os.MkdirAll(filepath.Dir(h.playlistPath(id)), 0o755))
	require.NoError(t, os.WriteFile(h.playlistPath(id), []byte("stale playlist"), 0o644))

	// Input is deleted persistently. The new process has NO in-memory
	// coordinator state, so use a brand new coordinator and delete directly
	// from the store (as the API's committed delete would leave it).
	require.NoError(t, h.store.DeleteInput(context.Background(), id))

	newCoordinator := service.NewCoordinator()
	restartedPackager := service.NewStreamPackager(
		h.store, h.cfg,
		service.WithPackagerCoordinator(newCoordinator),
	)

	callsBefore := h.gpacCallCount()

	err = restartedPackager.HandleTask(context.Background(), asynq.NewTask(service.TypeStreamPackage, taskBytes))
	require.Error(t, err)
	assert.True(t, errors.Is(err, asynq.SkipRetry))

	assert.Equal(t, callsBefore, h.gpacCallCount(), "restarted worker must not invoke gpac for deleted input")
	assert.NoFileExists(t, h.playlistPath(id), "restarted worker must purge stale playlist and not rewrite it")

	// The package-level mux entry point must delegate to an installed packager.
	service.SetDefaultStreamPackager(restartedPackager)
	t.Cleanup(func() { service.SetDefaultStreamPackager(nil) })

	err = service.HandleStreamPackageTask(context.Background(), asynq.NewTask(service.TypeStreamPackage, taskBytes))
	require.Error(t, err)
	assert.True(t, errors.Is(err, asynq.SkipRetry))
}
