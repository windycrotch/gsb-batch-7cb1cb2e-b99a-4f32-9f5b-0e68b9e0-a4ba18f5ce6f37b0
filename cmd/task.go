package cmd

import (
	"sync"
	"time"
)

// Task lifecycle states. A task moves forward through queued -> running and
// ends in exactly one terminal state: complete, failed or cancelled.
const (
	stateQueued    = "queued"
	stateRunning   = "running"
	stateComplete  = "complete"
	stateFailed    = "failed"
	stateCancelled = "cancelled"
)

// Task is one encode request and everything tracked about it. The same Task
// pointer is held by the registry, the queue and the worker, so every message
// about an encode carries the same stable ID.
type Task struct {
	ID      string
	OwnerID string // Client that submitted the task; "" for legacy id-less jobs.
	Legacy  bool   // Submitted by a client without task IDs.

	Input   string
	Output  string
	Payload string

	mu        sync.Mutex
	state     string
	percent   float64
	speed     string
	fps       float64
	err       string
	started   bool // ffmpeg (or its stand-in) actually began, so an output may exist.
	createdAt time.Time
	updatedAt time.Time
}

func newTask(id, ownerID string, legacy bool, msg Message) *Task {
	now := time.Now()
	return &Task{
		ID:        id,
		OwnerID:   ownerID,
		Legacy:    legacy,
		Input:     msg.Input,
		Output:    msg.Output,
		Payload:   msg.Payload,
		state:     stateQueued,
		createdAt: now,
		updatedAt: now,
	}
}

// State returns the current lifecycle state.
func (t *Task) State() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state
}

func (t *Task) isTerminal() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return isTerminalState(t.state)
}

func isTerminalState(state string) bool {
	return state == stateComplete || state == stateFailed || state == stateCancelled
}

// markRunning transitions queued -> running. It returns false when the task
// was cancelled while queued, in which case the worker must skip it.
func (t *Task) markRunning() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state == stateCancelled {
		return false
	}
	t.state = stateRunning
	t.updatedAt = time.Now()
	return true
}

// markStarted records that the encoder process launched, so the output file
// may exist and need cleaning up on cancel.
func (t *Task) markStarted() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.started = true
}

// hasStarted reports whether the encoder process ever launched.
func (t *Task) hasStarted() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.started
}

// cancelQueued transitions queued -> cancelled. Only the winner performs the
// transition, so a queued task is cancelled exactly once even if the client,
// a disconnect handler and the worker race over it.
func (t *Task) cancelQueued() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state != stateQueued {
		return false
	}
	t.state = stateCancelled
	t.updatedAt = time.Now()
	return true
}

// finish transitions running -> a terminal state. It returns false if another
// path already terminated the task, which guarantees a single terminal event.
func (t *Task) finish(state, errMsg string, percent float64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if isTerminalState(t.state) {
		return false
	}
	t.state = state
	t.err = errMsg
	if state == stateComplete {
		t.percent = 100
	} else if percent > 0 {
		t.percent = percent
	}
	t.updatedAt = time.Now()
	return true
}

// setProgress stores a progress snapshot.
func (t *Task) setProgress(percent float64, speed string, fps float64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.percent = percent
	t.speed = speed
	t.fps = fps
	t.updatedAt = time.Now()
}

// snapshot copies the fields needed for status replies and events.
func (t *Task) snapshot() TaskSnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	return TaskSnapshot{
		ID:        t.ID,
		State:     t.state,
		Percent:   t.percent,
		Speed:     t.speed,
		FPS:       t.fps,
		Err:       t.err,
		Input:     t.Input,
		Output:    t.Output,
		OwnerID:   t.OwnerID,
		Legacy:    t.Legacy,
		Started:   t.started,
		CreatedAt: t.createdAt,
		UpdatedAt: t.updatedAt,
	}
}

// TaskSnapshot is the serialisable view of a task.
type TaskSnapshot struct {
	ID        string    `json:"id"`
	State     string    `json:"state"`
	Percent   float64   `json:"percent"`
	Speed     string    `json:"speed,omitempty"`
	FPS       float64   `json:"fps,omitempty"`
	Err       string    `json:"err,omitempty"`
	Input     string    `json:"input,omitempty"`
	Output    string    `json:"output,omitempty"`
	OwnerID   string    `json:"-"`
	Legacy    bool      `json:"-"`
	Started   bool      `json:"-"`
	CreatedAt time.Time `json:"-"`
	UpdatedAt time.Time `json:"-"`
}

// registry holds every task the server knows about, keyed by stable ID. It is
// purely in-memory: a server restart starts with no history, which the
// protocol explicitly allows.
type registry struct {
	mu          sync.RWMutex
	tasks       map[string]*Task
	order       []string // Insertion order, for pruning the oldest records.
	maxRetained int
}

func newRegistry(maxRetained int) *registry {
	return &registry{
		tasks:       make(map[string]*Task),
		maxRetained: maxRetained,
	}
}

// add registers a task. It rejects an ID the registry already holds so a
// client cannot make two jobs share an identity.
func (r *registry) add(t *Task) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.tasks[t.ID]; exists {
		return errDuplicateID
	}
	r.tasks[t.ID] = t
	r.order = append(r.order, t.ID)
	r.pruneLocked()
	return nil
}

func (r *registry) remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.tasks, id)
	for i, o := range r.order {
		if o == id {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
}

func (r *registry) get(id string) (*Task, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tasks[id]
	return t, ok
}

// ownedBy returns every task belonging to a client, oldest first. Terminal
// records are retained so a reconnecting client can query its final results.
func (r *registry) ownedBy(clientID string) []*Task {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Task, 0)
	for _, id := range r.order {
		if t := r.tasks[id]; t != nil && t.OwnerID == clientID {
			out = append(out, t)
		}
	}
	return out
}

// pruneLocked drops the oldest terminal records past the retention limit.
// Active tasks are never pruned.
func (r *registry) pruneLocked() {
	for len(r.order) > r.maxRetained {
		oldest := r.order[0]
		r.order = r.order[1:]
		if t := r.tasks[oldest]; t != nil && t.isTerminal() {
			delete(r.tasks, oldest)
		} else {
			// Keep active tasks; hold their slot at the back so a later prune
			// considers terminal records first.
			r.order = append(r.order, oldest)
		}
	}
}
