package cmd

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"sync"
	"time"

	"github.com/alfg/ffmpegd/ffmpeg"
)

// Event is the typed envelope sent to clients that use task IDs. Every event
// about an encode carries the same ID the client submitted, so progress,
// completion, failure and cancellation can always be correlated.
type Event struct {
	Type     string         `json:"type"`                // welcome|queued|started|progress|complete|error|cancelled|status|tasks
	ID       string         `json:"id,omitempty"`        // Task ID; empty for connection-level events.
	State    string         `json:"state,omitempty"`     // Lifecycle state of the task.
	ClientID string         `json:"client_id,omitempty"` // Connection identity, on welcome.
	Percent  float64        `json:"percent,omitempty"`
	Speed    string         `json:"speed,omitempty"`
	FPS      float64        `json:"fps,omitempty"`
	Err      string         `json:"err,omitempty"`
	Tasks    []TaskSnapshot `json:"tasks,omitempty"`
}

// manager owns the encode queue, the worker that drains it and the rules for
// cancellation and disconnect cleanup. All state transitions go through it
// and the Task methods, which is what keeps each task to a single outcome.
type manager struct {
	hub *hub
	reg *registry

	// qmu guards the queue and backs cond. The queue is an explicit slice,
	// rather than a channel, so individual queued tasks can be removed by ID.
	qmu   sync.Mutex
	cond  *sync.Cond
	queue []*Task

	// curMu guards the currently running task and its encoder handle.
	curMu       sync.Mutex
	current     *Task
	currentProc encoder

	// runner is indirected so tests can drive the state machine without
	// ffmpeg binaries.
	runner func(t *Task) error

	// active counts tasks that have been queued but not yet reached a
	// terminal state, for tests that need to wait for the queue to drain.
	active sync.WaitGroup
}

// encoder is the cancellable process behind a running task. *ffmpeg.FFmpeg
// satisfies it in production; tests substitute a fake.
type encoder interface {
	Cancel()
}

func newManager(h *hub, reg *registry) *manager {
	m := &manager{hub: h, reg: reg}
	m.cond = sync.NewCond(&m.qmu)
	m.runner = m.runReal
	return m
}

// start launches the single worker goroutine. Encodes run one at a time, in
// submission order.
func (m *manager) start() {
	go m.worker()
}

func (m *manager) worker() {
	for {
		m.qmu.Lock()
		for len(m.queue) == 0 {
			m.cond.Wait()
		}
		t := m.queue[0]
		m.queue = m.queue[1:]
		m.qmu.Unlock()
		m.run(t)
	}
}

// enqueue registers a task and puts it in line. A task without an ID is a
// legacy job: it gets a server-generated ID that is never exposed, and its
// events are bridged onto the old broadcast Status shape.
func (m *manager) enqueue(c *client, msg Message) error {
	legacy := msg.ID == ""
	id := msg.ID
	if legacy {
		id = newServerTaskID()
	} else if len(id) > 200 {
		return fmt.Errorf("task id is too long (max 200 characters)")
	}

	t := newTask(id, c.ID(), legacy, msg)
	if err := m.reg.add(t); err != nil {
		return err
	}

	m.qmu.Lock()
	if len(m.queue) >= maxQueuedJobs {
		m.qmu.Unlock()
		m.reg.remove(id)
		return errQueueFull
	}
	m.queue = append(m.queue, t)
	m.qmu.Unlock()
	m.cond.Signal()

	m.active.Add(1)
	m.emit(t, Event{Type: "queued", State: stateQueued})
	return nil
}

// cancel stops a task the caller owns. A queued task is removed and never
// runs; a running task is signalled and its terminal event comes from the
// worker when the encoder exits. An empty ID preserves the legacy "cancel the
// current encode" message, scoped to what that client owns.
func (m *manager) cancel(c *client, id string) error {
	if id == "" {
		m.curMu.Lock()
		cur := m.current
		m.curMu.Unlock()
		if cur == nil || cur.OwnerID != c.ID() {
			return errUnknownTask
		}
		return m.cancelTask(cur)
	}

	t, ok := m.reg.get(id)
	if !ok {
		return errUnknownTask
	}
	if t.OwnerID != c.ID() {
		return errNotOwner
	}
	return m.cancelTask(t)
}

func (m *manager) cancelTask(t *Task) error {
	// Still queued: pull it out so the worker never sees it.
	m.qmu.Lock()
	for i, qt := range m.queue {
		if qt == t {
			m.queue = append(m.queue[:i], m.queue[i+1:]...)
			m.qmu.Unlock()
			if t.cancelQueued() {
				m.active.Done()
				m.emit(t, Event{Type: "cancelled", State: stateCancelled})
			}
			return nil
		}
	}
	m.qmu.Unlock()

	// Running: kill the encoder process; the worker reports the terminal
	// state when it exits.
	m.curMu.Lock()
	cur, proc := m.current, m.currentProc
	m.curMu.Unlock()
	if cur == t && proc != nil {
		proc.Cancel()
		return nil
	}

	if t.isTerminal() {
		return nil
	}
	return errTaskNotCancel
}

// disconnect removes a connection and, when no other connection shares its
// identity, cleans up everything that client still has in flight: queued
// tasks are cancelled and a running task is killed so its partial output is
// not left behind.
func (m *manager) disconnect(c *client) {
	id := c.ID()
	m.hub.remove(c)
	if m.hub.online(id) {
		// The same browser identity is still attached (another tab, or a
		// reconnect that landed before this socket noticed the drop).
		return
	}

	m.qmu.Lock()
	kept := m.queue[:0]
	var removed []*Task
	for _, t := range m.queue {
		if t.OwnerID == id {
			removed = append(removed, t)
		} else {
			kept = append(kept, t)
		}
	}
	m.queue = kept
	m.qmu.Unlock()

	for _, t := range removed {
		if t.cancelQueued() {
			m.active.Done()
			m.emit(t, Event{Type: "cancelled", State: stateCancelled})
		}
	}

	m.curMu.Lock()
	cur, proc := m.current, m.currentProc
	m.curMu.Unlock()
	if cur != nil && cur.OwnerID == id && proc != nil {
		proc.Cancel()
	}
}

// hello attaches a stable identity to an anonymous connection, so a browser
// that did not pass client_id in the URL can still reconnect and query its
// tasks.
func (m *manager) hello(c *client, clientID string) error {
	if !validClientID(clientID) {
		return fmt.Errorf("invalid client_id")
	}
	if c.identified() && c.ID() != clientID {
		return fmt.Errorf("this connection already identified as %q", c.ID())
	}
	if c.ID() != clientID {
		m.hub.rebind(c, clientID)
	}
	c.enableTaskMode()
	c.send(Event{Type: "welcome", ClientID: clientID})
	return nil
}

// status replies with one task's latest state.
func (m *manager) status(c *client, id string) {
	t, ok := m.reg.get(id)
	if !ok || t.OwnerID != c.ID() {
		// No lifecycle state exists for a task this client does not own; the
		// error alone conveys that it is unknown/inaccessible.
		c.send(Event{Type: "status", ID: id, Err: errUnknownTask.Error()})
		return
	}
	s := t.snapshot()
	c.send(Event{
		Type:    "status",
		ID:      t.ID,
		State:   s.State,
		Percent: s.Percent,
		Speed:   s.Speed,
		FPS:     s.FPS,
		Err:     s.Err,
	})
}

// tasks replies with every task the connecting client owns, including
// finished, failed and cancelled ones, so a reconnecting browser can recover
// the results it missed while disconnected.
func (m *manager) tasks(c *client) {
	owned := m.reg.ownedBy(c.ID())
	snaps := make([]TaskSnapshot, 0, len(owned))
	for _, t := range owned {
		snaps = append(snaps, t.snapshot())
	}
	c.send(Event{Type: "tasks", Tasks: snaps})
}

// run executes one popped task and records exactly one terminal state.
func (m *manager) run(t *Task) {
	m.curMu.Lock()
	m.current = t
	m.curMu.Unlock()
	defer func() {
		m.curMu.Lock()
		m.current = nil
		m.currentProc = nil
		m.curMu.Unlock()
		m.active.Done()
	}()

	if !t.markRunning() {
		// Cancelled in the gap between pop and execution.
		m.emit(t, Event{Type: "cancelled", State: stateCancelled})
		return
	}
	// A disconnect can land in the window after the task left the queue but
	// before an encoder handle existed for the cleanup path to stop. Catch
	// it here so an ownerless task never starts encoding.
	if !m.hub.online(t.OwnerID) {
		if t.finish(stateCancelled, "", 0) {
			m.cleanupOutput(t)
			m.emit(t, Event{Type: "cancelled", State: stateCancelled})
		}
		return
	}
	m.emit(t, Event{Type: "started", State: stateRunning})

	err := m.runner(t)
	switch {
	case errors.Is(err, ffmpeg.ErrCancelled):
		if t.finish(stateCancelled, "", 0) {
			m.cleanupOutput(t)
			m.emit(t, Event{Type: "cancelled", State: stateCancelled})
		}
	case err != nil:
		if t.finish(stateFailed, err.Error(), 0) {
			fmt.Printf("\r\u001b[31mEncode failed [%s]:\u001b[0m %s%s\n", t.ID, err.Error(), clearLine)
			m.emit(t, Event{Type: "error", State: stateFailed, Err: err.Error()})
		}
	default:
		if t.finish(stateComplete, "", 100) {
			fmt.Printf("\rEncode finished [%s]: %s%s\n", t.ID, t.Output, clearLine)
			m.emit(t, Event{Type: "complete", State: stateComplete, Percent: 100})
		}
	}
	fmt.Printf("Waiting for next job...")
}

// runReal is the production encoder: probe, progress tracking and ffmpeg.
func (m *manager) runReal(t *Task) error {
	f := &ffmpeg.FFmpeg{}
	// Registering the encoder makes the disconnect cleanup path able to stop
	// it. Re-check that the owner is still connected in case the disconnect
	// landed in the window just before registration.
	if !m.beginProc(t, f) {
		return ffmpeg.ErrCancelled
	}

	fmt.Printf("\rEncoding [%s] %s -> %s%s\n", t.ID, t.Input, t.Output, clearLine)

	e, err := ffmpeg.NewEncode(t.Input, t.Output, t.Payload)
	if err != nil {
		return err
	}
	defer e.Close()

	probe, err := ffmpeg.FFProbe{}.Run(t.Input)
	if err != nil {
		return err
	}

	// From here on ffmpeg may create the output file, so cancellation must
	// clean it up.
	t.markStarted()

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		m.trackProgress(t, f, e.OutputDuration(probe.Duration()), totalFrames(probe), done)
	}()

	err = f.RunEncode(e)
	// Stop progress updates before the terminal event so none can follow it.
	close(done)
	wg.Wait()
	return err
}

// cleanupOutput removes a cancelled task's partial encode. Two-pass log files
// live in a temp directory and are removed by Encode.Close.
func (m *manager) cleanupOutput(t *Task) {
	if !t.hasStarted() || t.Output == "" {
		return
	}
	switch err := os.Remove(t.Output); {
	case err == nil:
		fmt.Printf("\rRemoved partial output %s%s\n", t.Output, clearLine)
	case os.IsNotExist(err):
	default:
		fmt.Printf("\r\u001b[31mCould not remove partial output %s:\u001b[0m %v%s\n", t.Output, err, clearLine)
	}
}

// emit sends a task event to its owner, or bridges it onto the legacy
// broadcast Status shape for id-less clients.
func (m *manager) emit(t *Task, ev Event) {
	ev.ID = t.ID
	if t.Legacy {
		switch ev.Type {
		case "queued", "started":
			// The old protocol had no acknowledgement messages.
			return
		case "cancelled":
			m.hub.broadcastLegacy(Status{Cancelled: true})
		case "error":
			m.hub.broadcastLegacy(Status{Err: ev.Err})
		case "complete":
			m.hub.broadcastLegacy(Status{Percent: 100})
		}
		return
	}
	m.hub.broadcastTo(t.OwnerID, ev)
}

// emitProgress is the per-tick path used by the progress tracker.
func (m *manager) emitProgress(t *Task, pct float64, speed string, fps float64) {
	t.setProgress(pct, speed, fps)
	if t.Legacy {
		m.hub.broadcastLegacy(Status{Percent: pct, Speed: speed, FPS: fps})
		return
	}
	m.hub.broadcastTo(t.OwnerID, Event{
		Type:    "progress",
		ID:      t.ID,
		State:   stateRunning,
		Percent: pct,
		Speed:   speed,
		FPS:     fps,
	})
}

// trackProgress reports progress until done is closed. Progress is measured
// against the output duration when it is known, and the frame count
// otherwise. Each pass of a two-pass encode is half the total.
func (m *manager) trackProgress(t *Task, f *ffmpeg.FFmpeg, duration float64, frames int, done <-chan struct{}) {
	ticker := time.NewTicker(progressInterval)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			p := f.Progress()
			if p.Passes == 0 {
				continue
			}

			frac := -1.0
			switch {
			case duration > 0:
				frac = float64(p.OutTimeUS) / 1e6 / duration
			case frames > 0:
				frac = float64(p.Frame) / float64(frames)
			}

			var pct float64
			if frac >= 0 {
				pct = (float64(p.Pass-1) + math.Min(frac, 1)) / float64(p.Passes) * 100
				// 100 tells the client the job is done, so hold just short of
				// it until ffmpeg exits.
				pct = math.Min(math.Round(pct*100)/100, 99.99)
			}

			pass := ""
			if p.Passes > 1 {
				pass = fmt.Sprintf(" pass %d/%d", p.Pass, p.Passes)
			}
			if frac >= 0 {
				fmt.Printf("\rEncoding [%s]...%s %0.2f%% %s @ %0.2f fps%s", t.ID, pass, pct, p.Speed, p.FPS, clearLine)
			} else {
				fmt.Printf("\rEncoding [%s]...%s %0.1fs done %s @ %0.2f fps%s", t.ID, pass, float64(p.OutTimeUS)/1e6, p.Speed, p.FPS, clearLine)
			}

			m.emitProgress(t, pct, p.Speed, p.FPS)
		}
	}
}

// waitIdle blocks until every accepted task has reached a terminal state.
// Test helper.
func (m *manager) waitIdle() {
	m.active.Wait()
}

// setProc registers the encoder of the running task so cancel messages and
// disconnect cleanup can stop it. Test runners use it with a fake encoder.
func (m *manager) setProc(e encoder) {
	m.curMu.Lock()
	defer m.curMu.Unlock()
	m.currentProc = e
}

// beginProc registers the encoder and confirms the task's owner is still
// connected. It returns false when the owner disconnected in the window
// before the encoder was registered, stopping that encoder immediately so an
// orphaned task never runs.
func (m *manager) beginProc(t *Task, e encoder) bool {
	m.setProc(e)
	if m.hub.online(t.OwnerID) {
		return true
	}
	e.Cancel()
	return false
}

func newServerTaskID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "t-" + time.Now().Format("20060102150405.000000000")
	}
	return "t-" + hex.EncodeToString(b)
}
