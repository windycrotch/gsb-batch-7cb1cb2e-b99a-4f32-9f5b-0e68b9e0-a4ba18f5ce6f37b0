package cmd

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alfg/ffmpegd/ffmpeg"
	"github.com/gorilla/websocket"
)

// fakeProc stands in for an ffmpeg process: Cancel closes a channel the fake
// runner selects on, mirroring ffmpeg.FFmpeg.Cancel killing the process.
type fakeProc struct {
	once      sync.Once
	cancelled chan struct{}
}

func newFakeProc() *fakeProc {
	return &fakeProc{cancelled: make(chan struct{})}
}

func (p *fakeProc) Cancel() {
	p.once.Do(func() { close(p.cancelled) })
}

// harness wires a manager to a controllable fake encoder so tests can decide
// when each task starts, fails, completes or gets cancelled.
type harness struct {
	m       *manager
	dir     string
	started chan string

	mu    sync.Mutex
	procs map[string]*fakeProc
	fail  map[string]bool
	runs  map[string]int

	release chan struct{}
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		dir:     t.TempDir(),
		started: make(chan string, 256),
		procs:   make(map[string]*fakeProc),
		fail:    make(map[string]bool),
		runs:    make(map[string]int),
		release: make(chan struct{}),
	}
	h.m = newManager(newHub(), newRegistry(1000))
	h.m.runner = func(task *Task) error {
		proc := newFakeProc()
		if !h.m.beginProc(task, proc) {
			return ffmpeg.ErrCancelled
		}

		h.mu.Lock()
		h.procs[task.ID] = proc
		h.runs[task.ID]++
		shouldFail := h.fail[task.ID]
		h.mu.Unlock()

		// Mirror runReal: once the encoder launches, an output may exist and
		// cancellation must clean it up.
		task.markStarted()
		if task.Output != "" {
			if err := os.WriteFile(task.Output, []byte("partial encode"), 0o644); err != nil {
				return err
			}
		}
		h.started <- task.ID

		select {
		case <-proc.cancelled:
			return ffmpeg.ErrCancelled
		case <-h.release:
			if shouldFail {
				return errors.New("synthetic encoder failure")
			}
			return nil
		}
	}
	h.m.start()
	return h
}

func (h *harness) outputFor(id string) string {
	return filepath.Join(h.dir, id+".mp4")
}

func (h *harness) releaseAll() {
	close(h.release)
}

func (h *harness) runCount(id string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.runs[id]
}

func (h *harness) waitStarted(t *testing.T, id string) {
	t.Helper()
	select {
	case got := <-h.started:
		if got != id {
			t.Fatalf("runner started %q, want %q", got, id)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("task %q never started", id)
	}
}

// startTestServer exposes handleConnections over a real websocket listener.
func startTestServer(t *testing.T, h *harness) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(handleConnections))
	t.Cleanup(srv.Close)

	u, _ := url.Parse(srv.URL)
	// The dialer sends an Origin matching the server host, so allow exactly
	// that origin for the lifetime of this test server.
	allowedOrigins = []string{"http://" + u.Host}
	mgr = h.m
	return "ws://" + u.Host + "/ws"
}

// testClient runs a dedicated reader goroutine that funnels every message
// into a buffered channel. Negative assertions then select on the channel
// instead of issuing short read deadlines, which on some platforms leave the
// websocket unable to deliver later messages.
type testClient struct {
	t    *testing.T
	ws   *websocket.Conn
	ch   chan map[string]any
	once sync.Once
}

func dialClient(t *testing.T, wsURL, clientID string) *testClient {
	t.Helper()
	u, err := url.Parse(wsURL)
	if err != nil {
		t.Fatal(err)
	}
	if clientID != "" {
		q := u.Query()
		q.Set("client_id", clientID)
		u.RawQuery = q.Encode()
	}
	headers := http.Header{}
	headers.Set("Origin", "http://"+u.Host)
	ws, _, err := websocket.DefaultDialer.Dial(u.String(), headers)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	c := &testClient{
		t:  t,
		ws: ws,
		ch: make(chan map[string]any, 64),
	}
	go c.readLoop()
	t.Cleanup(c.close)
	return c
}

func (c *testClient) readLoop() {
	for {
		var m map[string]any
		if err := c.ws.ReadJSON(&m); err != nil {
			return
		}
		select {
		case c.ch <- m:
		default:
			// Buffer full: drop rather than block the reader.
		}
	}
}

func (c *testClient) send(msg any) {
	c.t.Helper()
	if err := c.ws.WriteJSON(msg); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

// next waits for the next message, failing the test on timeout.
func (c *testClient) next() map[string]any {
	c.t.Helper()
	select {
	case m := <-c.ch:
		return m
	case <-time.After(5 * time.Second):
		c.t.Fatal("timed out waiting for a message")
		return nil
	}
}

// nextOf waits until a message with the given type (and task ID when non-empty)
// arrives, queuing any other messages back into a small pending slice.
func (c *testClient) nextOf(typ, id string) map[string]any {
	c.t.Helper()
	deadline := time.After(5 * time.Second)
	var pending []map[string]any
	for {
		select {
		case m := <-c.ch:
			if m["type"] == typ && (id == "" || m["id"] == id) {
				// Re-queue skipped messages so the caller can still see them.
				for _, p := range pending {
					select {
					case c.ch <- p:
					default:
					}
				}
				return m
			}
			pending = append(pending, m)
		case <-deadline:
			c.t.Fatalf("timed out waiting for type=%q id=%q (skipped %d)", typ, id, len(pending))
			return nil
		}
	}
}

// expectNone asserts no message arrives within the window.
func (c *testClient) expectNone() {
	c.t.Helper()
	select {
	case m := <-c.ch:
		c.t.Fatalf("unexpected message: %v", m)
	case <-time.After(200 * time.Millisecond):
	}
}

func (c *testClient) close() {
	c.once.Do(func() { c.ws.Close() })
}

func (c *testClient) closed() {
	c.ws.Close()
}

func eventually(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}

func encodeMsg(id, output string) Message {
	return Message{Type: "encode", ID: id, Input: "in.mp4", Output: output, Payload: "{}"}
}

// TestEncodeEmitsIdentifiedEventsAndCompletes covers the happy path: every
// event from queued through complete carries the submitted ID, exactly once.
func TestEncodeEmitsIdentifiedEventsAndCompletes(t *testing.T) {
	h := newHarness(t)
	wsURL := startTestServer(t, h)
	ws := dialClient(t, wsURL, "alice")

	if ev := ws.next(); ev["type"] != "welcome" || ev["client_id"] != "alice" {
		t.Fatalf("first event = %v, want welcome for alice", ev)
	}
	if ev := ws.next(); ev["type"] != "tasks" {
		t.Fatalf("second event = %v, want tasks snapshot", ev)
	}

	ws.send(encodeMsg("job-1", h.outputFor("job-1")))

	ev := ws.nextOf("queued", "job-1")
	if ev["state"] != "queued" {
		t.Fatalf("queued event = %v", ev)
	}
	ev = ws.nextOf("started", "job-1")
	if ev["state"] != "running" {
		t.Fatalf("started event = %v", ev)
	}

	h.waitStarted(t, "job-1")
	h.releaseAll()

	ev = ws.nextOf("complete", "job-1")
	if ev["state"] != "complete" {
		t.Fatalf("complete event = %v", ev)
	}
	if pct, _ := ev["percent"].(float64); pct != 100 {
		t.Fatalf("complete percent = %v, want 100", ev["percent"])
	}

	task, ok := h.m.reg.get("job-1")
	if !ok || task.State() != stateComplete {
		t.Fatalf("registry state = %v, want complete", ok)
	}
	if h.runCount("job-1") != 1 {
		t.Fatalf("encoder ran %d times, want exactly 1", h.runCount("job-1"))
	}
	if _, err := os.Stat(h.outputFor("job-1")); err != nil {
		t.Fatalf("completed output missing: %v", err)
	}

	ws.expectNone()
}

// TestCancelQueuedTaskNeverRuns: a queued task cancelled before its turn is
// removed, emits one cancelled event and never reaches the encoder, while the
// rest of the queue still drains.
func TestCancelQueuedTaskNeverRuns(t *testing.T) {
	h := newHarness(t)
	wsURL := startTestServer(t, h)
	ws := dialClient(t, wsURL, "alice")
	ws.next() // welcome
	ws.next() // tasks

	ws.send(encodeMsg("job-1", h.outputFor("job-1")))
	ws.nextOf("started", "job-1")
	h.waitStarted(t, "job-1")

	ws.send(encodeMsg("job-2", h.outputFor("job-2")))
	ws.nextOf("queued", "job-2")
	ws.send(encodeMsg("job-3", h.outputFor("job-3")))
	ws.nextOf("queued", "job-3")

	ws.send(Message{Type: "cancel", ID: "job-2"})
	ev := ws.nextOf("cancelled", "job-2")
	if ev["state"] != "cancelled" {
		t.Fatalf("cancel event = %v", ev)
	}

	h.releaseAll()

	// job-1 and job-3 complete; job-2 must not appear among them.
	completes := map[string]bool{}
	for len(completes) < 2 {
		ev := ws.nextOf("complete", "")
		completes[ev["id"].(string)] = true
	}
	if !completes["job-1"] || !completes["job-3"] {
		t.Fatalf("completes = %v, want job-1 and job-3", completes)
	}

	if h.runCount("job-2") != 0 {
		t.Fatalf("cancelled queued task ran %d times", h.runCount("job-2"))
	}
	if _, err := os.Stat(h.outputFor("job-2")); !os.IsNotExist(err) {
		t.Fatalf("cancelled queued task produced output: %v", err)
	}
	task, _ := h.m.reg.get("job-2")
	if task.State() != stateCancelled {
		t.Fatalf("job-2 state = %q, want cancelled", task.State())
	}
}

// TestCancelRunningContinuesQueue: cancelling the running task kills it,
// cleans its partial output and the worker immediately moves to the next job.
func TestCancelRunningContinuesQueue(t *testing.T) {
	h := newHarness(t)
	wsURL := startTestServer(t, h)
	ws := dialClient(t, wsURL, "alice")
	ws.next()
	ws.next()

	ws.send(encodeMsg("job-1", h.outputFor("job-1")))
	ws.nextOf("started", "job-1")
	h.waitStarted(t, "job-1")

	ws.send(encodeMsg("job-2", h.outputFor("job-2")))
	ws.nextOf("queued", "job-2")

	if _, err := os.Stat(h.outputFor("job-1")); err != nil {
		t.Fatalf("partial output not created: %v", err)
	}

	ws.send(Message{Type: "cancel", ID: "job-1"})
	ws.nextOf("cancelled", "job-1")
	eventually(t, func() bool {
		_, err := os.Stat(h.outputFor("job-1"))
		return os.IsNotExist(err)
	})

	// The queue advances: job-2 starts and completes.
	ws.nextOf("started", "job-2")
	h.waitStarted(t, "job-2")
	h.releaseAll()
	ws.nextOf("complete", "job-2")
}

// TestDisconnectBeforeEncoderRegistered: covers the narrow window where a
// task has been popped and marked running but its encoder handle does not yet
// exist. A disconnect landing there must still stop the task and never run it.
func TestDisconnectBeforeEncoderRegistered(t *testing.T) {
	m := newManager(newHub(), newRegistry(1000))
	gate := make(chan struct{})
	var ran atomic.Bool
	m.runner = func(task *Task) error {
		proc := newFakeProc()
		// Hold the runner before the owner-online check, then let the test
		// disconnect before releasing it.
		<-gate
		if !m.beginProc(task, proc) {
			return ffmpeg.ErrCancelled
		}
		task.markStarted()
		if task.Output != "" {
			os.WriteFile(task.Output, []byte("partial"), 0o644)
		}
		ran.Store(true)
		return nil
	}
	m.start()

	srv := httptest.NewServer(http.HandlerFunc(handleConnections))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	allowedOrigins = []string{"http://" + u.Host}
	mgr = m

	dir := t.TempDir()
	out := filepath.Join(dir, "raced.mp4")
	ws := dialClient(t, "ws://"+u.Host+"/ws", "racer")
	ws.next() // welcome
	ws.next() // tasks
	ws.send(encodeMsg("raced", out))
	ws.nextOf("queued", "raced")
	ws.nextOf("started", "raced")

	// The runner is now parked at the gate: running state, but the encoder is
	// not yet registered. Disconnect, then allow the runner to proceed.
	ws.closed()
	eventually(t, func() bool { return !m.hub.online("racer") })
	close(gate)

	eventually(t, func() bool {
		task, ok := m.reg.get("raced")
		return ok && task.State() == stateCancelled
	})
	if ran.Load() {
		t.Fatal("orphaned task ran after its owner disconnected")
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("orphaned task produced an output file")
	}
}

// TestCancelOnlyOwnTasks: a client can neither cancel another client's task
// nor reference IDs it does not own.
func TestCancelOnlyOwnTasks(t *testing.T) {
	h := newHarness(t)
	wsURL := startTestServer(t, h)
	alice := dialClient(t, wsURL, "alice")
	bob := dialClient(t, wsURL, "bob")
	alice.next()
	alice.next()
	bob.next()
	bob.next()

	alice.send(encodeMsg("job-1", h.outputFor("job-1")))
	alice.nextOf("started", "job-1")
	h.waitStarted(t, "job-1")

	// Bob cannot cancel Alice's task.
	bob.send(Message{Type: "cancel", ID: "job-1"})
	ev := bob.nextOf("error", "job-1")
	if msg, _ := ev["err"].(string); msg == "" {
		t.Fatal("expected an ownership error message")
	}

	// Unknown IDs are rejected too.
	bob.send(Message{Type: "cancel", ID: "ghost"})
	bob.nextOf("error", "ghost")

	// Alice's task is still running and only she can cancel it.
	if task, _ := h.m.reg.get("job-1"); task.State() != stateRunning {
		t.Fatalf("job-1 state = %q, want still running", task.State())
	}
	alice.send(Message{Type: "cancel", ID: "job-1"})
	alice.nextOf("cancelled", "job-1")
}

// TestDisconnectCleansUpClientTasks: dropping a connection cancels its queued
// and running tasks (removing partial output) without touching other clients.
func TestDisconnectCleansUpClientTasks(t *testing.T) {
	h := newHarness(t)
	wsURL := startTestServer(t, h)
	alice := dialClient(t, wsURL, "alice")
	bob := dialClient(t, wsURL, "bob")
	alice.next()
	alice.next()
	bob.next()
	bob.next()

	alice.send(encodeMsg("a-1", h.outputFor("a-1")))
	alice.nextOf("started", "a-1")
	h.waitStarted(t, "a-1")

	alice.send(encodeMsg("a-2", h.outputFor("a-2")))
	alice.nextOf("queued", "a-2")
	bob.send(encodeMsg("b-1", h.outputFor("b-1")))
	bob.nextOf("queued", "b-1")

	alice.closed()

	// Alice's running and queued tasks end cancelled; her partial output is
	// removed; Bob's queued task is untouched and then runs.
	eventually(t, func() bool {
		a1, ok1 := h.m.reg.get("a-1")
		a2, ok2 := h.m.reg.get("a-2")
		return ok1 && ok2 && a1.State() == stateCancelled && a2.State() == stateCancelled
	})
	eventually(t, func() bool {
		_, err := os.Stat(h.outputFor("a-1"))
		return os.IsNotExist(err)
	})
	if _, err := os.Stat(h.outputFor("a-2")); !os.IsNotExist(err) {
		t.Fatal("queued task of disconnected client produced an output")
	}
	if h.runCount("a-2") != 0 {
		t.Fatal("disconnected client's queued task executed")
	}

	h.waitStarted(t, "b-1")
	h.releaseAll()
	bob.nextOf("complete", "b-1")
}

// TestReconnectQueriesTerminalStates: a browser returning with the same
// client_id recovers completed, failed and cancelled results it missed.
func TestReconnectQueriesTerminalStates(t *testing.T) {
	h := newHarness(t)
	wsURL := startTestServer(t, h)
	ws := dialClient(t, wsURL, "alice")
	ws.next() // welcome
	ws.next() // tasks

	ws.send(encodeMsg("done", h.outputFor("done")))
	ws.nextOf("started", "done")
	h.waitStarted(t, "done")

	ws.send(encodeMsg("gone", h.outputFor("gone")))
	ws.nextOf("queued", "gone")
	ws.send(Message{Type: "cancel", ID: "gone"})
	ws.nextOf("cancelled", "gone")

	h.mu.Lock()
	h.fail["boom"] = true
	h.mu.Unlock()
	ws.send(encodeMsg("boom", h.outputFor("boom")))
	ws.nextOf("queued", "boom")

	h.releaseAll()
	ws.nextOf("complete", "done")
	ws.nextOf("started", "boom")
	h.waitStarted(t, "boom")
	ws.nextOf("error", "boom")
	h.m.waitIdle()
	ws.closed()

	// Reconnect with the same identity.
	ws2 := dialClient(t, wsURL, "alice")
	if ev := ws2.next(); ev["type"] != "welcome" || ev["client_id"] != "alice" {
		t.Fatalf("reconnect event = %v", ev)
	}
	ev := ws2.nextOf("tasks", "")
	states := map[string]string{}
	for _, item := range ev["tasks"].([]any) {
		task := item.(map[string]any)
		states[task["id"].(string)] = task["state"].(string)
	}
	want := map[string]string{"done": "complete", "gone": "cancelled", "boom": "failed"}
	for id, state := range want {
		if states[id] != state {
			t.Fatalf("reconnected state of %s = %q, want %q (all: %v)", id, states[id], state, states)
		}
	}

	// Direct status query for one task.
	ws2.send(Message{Type: "status", ID: "done"})
	ev = ws2.nextOf("status", "done")
	if ev["state"] != "complete" {
		t.Fatalf("status event = %v", ev)
	}

	// Someone else's task is invisible.
	ws3 := dialClient(t, wsURL, "bob")
	ws3.next()
	ws3.next()
	ws3.send(Message{Type: "status", ID: "done"})
	ev = ws3.nextOf("status", "done")
	if msg, _ := ev["err"].(string); msg == "" {
		t.Fatal("expected unknown-task error for another client's task")
	}
}

// TestLegacyClientKeepsOldProtocol: clients without IDs still get the
// original Status broadcasts and legacy cancel semantics.
func TestLegacyClientKeepsOldProtocol(t *testing.T) {
	h := newHarness(t)
	wsURL := startTestServer(t, h)

	legacyA := dialClient(t, wsURL, "")
	legacyB := dialClient(t, wsURL, "")
	modern := dialClient(t, wsURL, "alice")
	modern.next() // welcome
	modern.next() // tasks

	// No welcome/tasks events for legacy connections.
	legacyA.expectNone()

	legacyA.send(Message{Type: "encode", Input: "in.mp4", Output: h.outputFor("legacy"), Payload: "{}"})
	legacyA.expectNone() // No typed envelope for legacy clients.
	waitAnyStarted(t, h)

	// Bob's legacy cancel targets a job he does not own: rejected with a
	// legacy Status error, not a typed envelope.
	legacyB.send(Message{Type: "cancel"})
	ev := legacyB.next()
	if _, isTyped := ev["type"]; isTyped {
		t.Fatalf("legacy client received typed envelope: %v", ev)
	}
	if msg, _ := ev["err"].(string); msg == "" {
		t.Fatalf("legacy cancel error missing: %v", ev)
	}

	// The modern client must not receive legacy broadcasts.
	modern.expectNone()

	// Owner's legacy cancel succeeds and is delivered in the old shape.
	legacyA.send(Message{Type: "cancel"})
	ev = legacyA.next()
	if cancelled, _ := ev["cancelled"].(bool); !cancelled {
		t.Fatalf("legacy cancel status = %v, want cancelled=true", ev)
	}
	if _, isTyped := ev["type"]; isTyped {
		t.Fatalf("legacy cancel arrived as typed envelope: %v", ev)
	}
}

func waitAnyStarted(t *testing.T, h *harness) {
	t.Helper()
	select {
	case <-h.started:
	case <-time.After(5 * time.Second):
		t.Fatal("encode never started")
	}
}

// TestConcurrentClientsSingleOutcome: many clients cancel at once, one a
// running task and the rest while they are queued, then submit a second wave.
// Every task executes at most once, ends with exactly one terminal state and
// can never report both completion and cancellation.
func TestConcurrentClientsSingleOutcome(t *testing.T) {
	h := newHarness(t)
	wsURL := startTestServer(t, h)

	const clients = 5
	conns := make([]*testClient, clients)
	cids := make([]string, clients)
	for i := range clients {
		cids[i] = "client-" + string(rune('a'+i))
		conns[i] = dialClient(t, wsURL, cids[i])
		conns[i].next() // welcome
		conns[i].next() // tasks
	}

	// One task per client: the first runs and blocks, the other four queue.
	for i, ws := range conns {
		ws.send(encodeMsg(cids[i]+"-1", h.outputFor(cids[i]+"-1")))
	}
	h.waitStarted(t, cids[0]+"-1")

	// Concurrent cancels: one running task, four queued tasks.
	var wg sync.WaitGroup
	for i, ws := range conns {
		wg.Add(1)
		go func(i int, ws *testClient) {
			defer wg.Done()
			ws.send(Message{Type: "cancel", ID: cids[i] + "-1"})
		}(i, ws)
	}
	wg.Wait()

	// Every first-wave task must settle as cancelled, with no output.
	eventually(t, func() bool {
		for _, cid := range cids {
			task, ok := h.m.reg.get(cid + "-1")
			if !ok || task.State() != stateCancelled {
				return false
			}
		}
		return true
	})
	for i, cid := range cids {
		id := cid + "-1"
		if runs := h.runCount(id); runs > 1 {
			t.Errorf("%s executed %d times, want at most 1", id, runs)
		}
		if i > 0 && h.runCount(id) != 0 {
			t.Errorf("queued %s executed after cancellation", id)
		}
		if _, err := os.Stat(h.outputFor(id)); !os.IsNotExist(err) {
			t.Errorf("cancelled %s left output behind", id)
		}
	}

	// Second wave: new tasks from every client, all released to completion.
	for i, ws := range conns {
		ws.send(encodeMsg(cids[i]+"-2", h.outputFor(cids[i]+"-2")))
	}
	h.releaseAll()

	terminal := make([]map[string]string, clients)
	for i := range terminal {
		terminal[i] = map[string]string{}
	}
	// Drain every connection: expect one cancelled (-1) and one complete (-2)
	// terminal event per client, each exactly once.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for i, ws := range conns {
			select {
			case ev := <-ws.ch:
				id, _ := ev["id"].(string)
				typ, _ := ev["type"].(string)
				if typ == "complete" || typ == "cancelled" || typ == "error" {
					if _, exists := terminal[i][id]; exists {
						t.Errorf("task %s reported terminal twice", id)
					}
					terminal[i][id] = typ
				}
			default:
			}
		}
		count := 0
		for _, m := range terminal {
			count += len(m)
		}
		if count == 2*clients {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	count := 0
	for _, m := range terminal {
		count += len(m)
	}
	if count != 2*clients {
		t.Fatalf("collected %d terminal events, want %d: %v", count, 2*clients, terminal)
	}

	for i, cid := range cids {
		if terminal[i][cid+"-1"] != "cancelled" {
			t.Errorf("%s-1 wire outcome = %q, want cancelled", cid, terminal[i][cid+"-1"])
		}
		if terminal[i][cid+"-2"] != "complete" {
			t.Errorf("%s-2 wire outcome = %q, want complete", cid, terminal[i][cid+"-2"])
		}
		task, _ := h.m.reg.get(cid + "-2")
		if task.State() != stateComplete {
			t.Errorf("%s-2 registry state = %q, want complete", cid, task.State())
		}
		if h.runCount(cid+"-2") != 1 {
			t.Errorf("%s-2 executed %d times, want 1", cid, h.runCount(cid+"-2"))
		}
	}
}
