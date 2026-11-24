// Command e2e-client drives a real running ffmpegd server through the task
// tracking scenarios from the requirements: identified progress/completion,
// cancelling queued and running tasks, disconnect cleanup, reconnect queries
// and legacy (ID-less) clients.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"time"

	"github.com/gorilla/websocket"
)

type msg map[string]any

type conn struct {
	id string
	ws *websocket.Conn
	ch chan msg
}

func dial(server, clientID string) *conn {
	u, err := url.Parse(server)
	if err != nil {
		fatal("parse url: %v", err)
	}
	if clientID != "" {
		q := u.Query()
		q.Set("client_id", clientID)
		u.RawQuery = q.Encode()
	}
	hdr := http.Header{}
	hdr.Set("Origin", "http://"+u.Host)
	ws, _, err := websocket.DefaultDialer.Dial(u.String(), hdr)
	if err != nil {
		fatal("dial %s: %v", clientID, err)
	}
	c := &conn{id: clientID, ws: ws, ch: make(chan msg, 256)}
	go func() {
		for {
			_, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			var m msg
			if err := json.Unmarshal(data, &m); err != nil {
				continue
			}
			c.ch <- m
		}
	}()
	return c
}

func (c *conn) send(m msg) {
	if err := c.ws.WriteJSON(m); err != nil {
		fatal("write: %v", err)
	}
}

func (c *conn) close() { c.ws.Close() }

func (c *conn) wait(typ, id string, timeout time.Duration) msg {
	deadline := time.After(timeout)
	for {
		select {
		case m := <-c.ch:
			if m["type"] == typ && (id == "" || m["id"] == id) {
				return m
			}
		case <-deadline:
			fatal("client %s timed out waiting for type=%q id=%q", c.id, typ, id)
		}
	}
}

var failures int

func check(cond bool, format string, args ...any) {
	if cond {
		fmt.Printf("  PASS: %s\n", fmt.Sprintf(format, args...))
	} else {
		failures++
		fmt.Printf("  FAIL: %s\n", fmt.Sprintf(format, args...))
	}
}

func fatal(format string, args ...any) {
	fmt.Printf("FATAL: %s\n", fmt.Sprintf(format, args...))
	os.Exit(1)
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// payload is a small, fast encode configuration accepted by NewEncode.
const payload = `{"format":{"container":"mp4"},"video":{"codec":"libx264","preset":"medium","crf":"30"},"audio":{"codec":"aac"}}`

// payloadSlow uses a slow preset so an 8s clip emits several progress ticks.
const payloadSlow = `{"format":{"container":"mp4"},"video":{"codec":"libx264","preset":"veryslow","crf":"28"},"audio":{"codec":"aac"}}`

func main() {
	server := flag.String("server", "ws://127.0.0.1:8099/ws", "ffmpegd websocket URL")
	dir := flag.String("dir", "media", "directory holding inputs/outputs")
	flag.Parse()

	// A per-run prefix keeps IDs stable across the reconnect within this run
	// while never colliding with records retained from an earlier run.
	runID := fmt.Sprintf("r%d", time.Now().UnixNano())
	cid := func(n string) string { return n + "-" + runID }
	tid := func(n string) string { return n + "-" + runID }

	inputFast := *dir + "/sample-8s.mp4"
	inputHeavy := *dir + "/heavy-20s.mp4"

	// ---- Scenario 1: identified submit reports progress and completion.
	fmt.Println("\n[1] Normal submit: identified progress + completion")
	a := dial(*server, cid("e2e-alice"))
	if w := a.wait("welcome", "", 3*time.Second); w["client_id"] != cid("e2e-alice") {
		fatal("bad welcome: %v", w)
	}
	a.wait("tasks", "", 3*time.Second)

	out1 := *dir + "/out-1.mp4"
	os.Remove(out1)
	a.send(msg{"type": "encode", "id": tid("task-1"), "input": inputFast, "output": out1, "payload": payloadSlow})
	a.wait("queued", tid("task-1"), 3*time.Second)
	a.wait("started", tid("task-1"), 3*time.Second)
	var sawProgress bool
	done := false
	deadline := time.Now().Add(40 * time.Second)
	for !done && time.Now().Before(deadline) {
		select {
		case m := <-a.ch:
			if m["type"] == "progress" && m["id"] == tid("task-1") {
				sawProgress = true
			}
			if m["type"] == "complete" && m["id"] == tid("task-1") {
				check(m["state"] == "complete", "complete event carries state=complete and same id")
				check(sawProgress, "received at least one identified progress event")
				done = true
			}
			if m["type"] == "error" {
				fatal("%s failed: %v", tid("task-1"), m)
			}
		case <-time.After(40 * time.Second):
			fatal("%s never completed", tid("task-1"))
		}
	}
	check(fileExists(out1), "output file %s created", out1)

	// ---- Scenario 2: cancel a queued task; it never runs, no output.
	fmt.Println("\n[2] Cancel queued task: never starts, no output, queue drains")
	b := dial(*server, cid("e2e-bob"))
	b.wait("welcome", "", 3*time.Second)
	b.wait("tasks", "", 3*time.Second)

	outSlow := *dir + "/out-slow.mp4"
	outQueued := *dir + "/out-queued.mp4"
	os.Remove(outSlow)
	os.Remove(outQueued)
	b.send(msg{"type": "encode", "id": tid("slow"), "input": inputHeavy, "output": outSlow, "payload": payload})
	b.wait("started", tid("slow"), 5*time.Second)
	b.send(msg{"type": "encode", "id": tid("queued-job"), "input": inputFast, "output": outQueued, "payload": payload})
	b.wait("queued", tid("queued-job"), 3*time.Second)
	b.send(msg{"type": "cancel", "id": tid("queued-job")})
	cancelEv := b.wait("cancelled", tid("queued-job"), 3*time.Second)
	check(cancelEv["state"] == "cancelled", "queued task reports cancelled with same id")
	time.Sleep(500 * time.Millisecond)
	check(!fileExists(outQueued), "cancelled queued task produced no output")

	// ---- Scenario 3: cancel the running task; cleanup, queue continues.
	fmt.Println("\n[3] Cancel running task: cleanup + worker continues")
	gotProgress := false
	dl := time.Now().Add(15 * time.Second)
	for !gotProgress && time.Now().Before(dl) {
		select {
		case m := <-b.ch:
			if m["type"] == "progress" && m["id"] == tid("slow") {
				gotProgress = true
			}
		case <-time.After(15 * time.Second):
		}
	}
	check(gotProgress, "slow task emitted progress before cancel")
	check(fileExists(outSlow), "partial output exists while running")
	b.send(msg{"type": "cancel", "id": tid("slow")})
	b.wait("cancelled", tid("slow"), 5*time.Second)
	eventually(func() bool { return !fileExists(outSlow) }, 5*time.Second)
	check(!fileExists(outSlow), "cancelled running task's partial output removed")

	outNext := *dir + "/out-next.mp4"
	os.Remove(outNext)
	b.send(msg{"type": "encode", "id": tid("next"), "input": inputFast, "output": outNext, "payload": payload})
	b.wait("started", tid("next"), 3*time.Second)
	b.wait("complete", tid("next"), 30*time.Second)
	check(fileExists(outNext), "queue continued and next task completed")

	// ---- Scenario 4: cancellation is scoped to the owning client.
	fmt.Println("\n[4] Cancellation is scoped to the owning client")
	outOwn := *dir + "/out-own.mp4"
	os.Remove(outOwn)
	a.send(msg{"type": "encode", "id": tid("owned"), "input": inputHeavy, "output": outOwn, "payload": payload})
	a.wait("started", tid("owned"), 5*time.Second)
	b.send(msg{"type": "cancel", "id": tid("owned")})
	errEv := b.wait("error", tid("owned"), 3*time.Second)
	check(fmt.Sprint(errEv["err"]) != "", "non-owner cancel rejected with an error")
	a.send(msg{"type": "cancel", "id": tid("owned")})
	a.wait("cancelled", tid("owned"), 5*time.Second)
	eventually(func() bool { return !fileExists(outOwn) }, 5*time.Second)

	// ---- Scenario 5: disconnect cleans up queued and running tasks.
	fmt.Println("\n[5] Disconnect removes queued tasks and kills the running one")
	x := dial(*server, cid("e2e-carol"))
	x.wait("welcome", "", 3*time.Second)
	x.wait("tasks", "", 3*time.Second)
	outRun := *dir + "/out-disc-run.mp4"
	outQ := *dir + "/out-disc-queued.mp4"
	os.Remove(outRun)
	os.Remove(outQ)
	x.send(msg{"type": "encode", "id": tid("disc-run"), "input": inputHeavy, "output": outRun, "payload": payload})
	x.wait("started", tid("disc-run"), 5*time.Second)
	x.send(msg{"type": "encode", "id": tid("disc-queued"), "input": inputFast, "output": outQ, "payload": payload})
	x.wait("queued", tid("disc-queued"), 3*time.Second)
	eventually(func() bool { return fileExists(outRun) }, 15*time.Second)
	x.close()
	outBob := *dir + "/out-bob-after-disc.mp4"
	os.Remove(outBob)
	b.send(msg{"type": "encode", "id": tid("bob-after"), "input": inputFast, "output": outBob, "payload": payload})
	b.wait("complete", tid("bob-after"), 30*time.Second)
	check(fileExists(outBob), "other client's task still runs after a disconnect")
	eventually(func() bool { return !fileExists(outRun) }, 5*time.Second)
	check(!fileExists(outRun), "disconnected client's running output cleaned")
	check(!fileExists(outQ), "disconnected client's queued task never produced output")

	// ---- Scenario 6: reconnect and query terminal states.
	fmt.Println("\n[6] Reconnected client queries its task history")
	a2 := dial(*server, cid("e2e-alice"))
	a2.wait("welcome", "", 3*time.Second)
	tasksEv := a2.wait("tasks", "", 3*time.Second)
	states := map[string]string{}
	for _, it := range tasksEv["tasks"].([]any) {
		tm := it.(map[string]any)
		states[fmt.Sprint(tm["id"])] = fmt.Sprint(tm["state"])
	}
	check(states[tid("task-1")] == "complete", "reconnect sees task-1=complete (got %q)", states[tid("task-1")])
	check(states[tid("owned")] == "cancelled", "reconnect sees owned=cancelled (got %q)", states[tid("owned")])
	a2.send(msg{"type": "status", "id": tid("task-1")})
	st := a2.wait("status", tid("task-1"), 3*time.Second)
	check(st["state"] == "complete", "explicit status query returns complete")
	a2.close()

	// ---- Scenario 7: legacy client without IDs keeps the old protocol.
	fmt.Println("\n[7] Legacy ID-less client uses the original Status shape")
	legacy := dial(*server, "")
	select {
	case m := <-legacy.ch:
		fatal("legacy client got unsolicited typed event: %v", m)
	case <-time.After(400 * time.Millisecond):
	}
	outLegacy := *dir + "/out-legacy.mp4"
	os.Remove(outLegacy)
	legacy.send(msg{"type": "encode", "input": inputFast, "output": outLegacy, "payload": payload})
	legacyCompleted := false
	dl = time.Now().Add(40 * time.Second)
	for !legacyCompleted && time.Now().Before(dl) {
		select {
		case m := <-legacy.ch:
			if _, typed := m["type"]; typed {
				fatal("legacy client received typed envelope: %v", m)
			}
			if pct, _ := m["percent"].(float64); pct == 100 {
				legacyCompleted = true
			}
		case <-time.After(40 * time.Second):
		}
	}
	check(legacyCompleted, "legacy client received old-style percent=100 completion")
	check(fileExists(outLegacy), "legacy encode produced output")

	// ---- Scenario 8: summary of single terminal states.
	fmt.Println("\n[8] Cross-client consistency summary")
	names := make([]string, 0, len(states))
	for k := range states {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Printf("  alice task %-24s -> %s\n", n, states[n])
	}

	a.close()
	b.close()
	legacy.close()

	if failures == 0 {
		fmt.Println("\nALL E2E CHECKS PASSED")
	} else {
		fmt.Printf("\n%d E2E CHECK(S) FAILED\n", failures)
		os.Exit(1)
	}
}

func eventually(fn func() bool, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}
