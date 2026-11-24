package cmd

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var (
	errDuplicateID   = errors.New("a task with this id already exists")
	errUnknownTask   = errors.New("unknown task id")
	errNotOwner      = errors.New("only the client that submitted this task may cancel it")
	errTaskNotCancel = errors.New("task is already finished")
	errQueueFull     = errors.New("server queue is full, try again later")
)

// client wraps one websocket connection with the stable identity the browser
// keeps across reconnects. A fresh connection that never identifies itself is
// still a client, with an ephemeral server-side ID; legacy jobs are owned by
// such anonymous clients and are cleaned up when the socket drops.
//
// All field access is guarded because the read goroutine mutates identity and
// protocol mode while worker goroutines broadcast events.
type client struct {
	conn *websocket.Conn

	mu       sync.Mutex
	id       string
	identify bool // ID came from the browser (query param or hello), not generated.
	taskMode bool // Sends task IDs, so expects typed event envelopes.
	closed   bool
}

// newClient wraps an upgraded connection. identified reports whether the
// browser supplied the ID itself; generated IDs are not advertised back as a
// reconnect identity the browser could rely on.
func newClient(conn *websocket.Conn, id string, identified bool) *client {
	return &client{conn: conn, id: id, identify: identified}
}

// ID returns the connection's stable identity.
func (c *client) ID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.id
}

func (c *client) identified() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.identify
}

// enableTaskMode switches the connection to typed event envelopes. It stays
// on for the life of the connection once a task-aware message is seen.
func (c *client) enableTaskMode() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.taskMode = true
}

func (c *client) isTaskMode() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.taskMode
}

func (c *client) send(v any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if err := c.conn.WriteJSON(v); err != nil {
		c.closed = true
		c.conn.Close()
	}
}

func (c *client) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		c.conn.Close()
	}
}

// hub tracks live connections. gorilla/websocket allows one writer at a time
// per connection; each client serialises its own writes, so the hub only needs
// to protect its maps.
type hub struct {
	mu    sync.RWMutex
	conns map[*client]bool
	byID  map[string]map[*client]bool // Identity -> its live connections.
}

func newHub() *hub {
	return &hub{
		conns: make(map[*client]bool),
		byID:  make(map[string]map[*client]bool),
	}
}

func (h *hub) add(c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.conns[c] = true
	id := c.ID()
	set, ok := h.byID[id]
	if !ok {
		set = make(map[*client]bool)
		h.byID[id] = set
	}
	set[c] = true
}

func (h *hub) remove(c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.conns, c)
	id := c.ID()
	if set, ok := h.byID[id]; ok {
		delete(set, c)
		if len(set) == 0 {
			delete(h.byID, id)
		}
	}
	c.close()
}

// online reports whether at least one connection with this identity is open.
func (h *hub) online(clientID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.byID[clientID]) > 0
}

// broadcastTo sends an event to every live connection sharing an identity.
// The same browser may hold more than one socket (extra tabs, a reconnect
// before the old socket noticed it dropped), so fan out rather than pick one.
func (h *hub) broadcastTo(clientID string, v any) {
	h.mu.RLock()
	conns := make([]*client, 0, len(h.byID[clientID]))
	for c := range h.byID[clientID] {
		conns = append(conns, c)
	}
	h.mu.RUnlock()
	for _, c := range conns {
		c.send(v)
	}
}

// broadcastLegacy sends an old-style Status to every connected legacy client.
// New clients are addressed per task and never receive these.
func (h *hub) broadcastLegacy(v any) {
	h.mu.RLock()
	conns := make([]*client, 0, len(h.conns))
	for c := range h.conns {
		conns = append(conns, c)
	}
	h.mu.RUnlock()
	for _, c := range conns {
		if !c.isTaskMode() {
			c.send(v)
		}
	}
}

// rebind moves a connection from one identity to another. The hub lock is
// held and the client lock is taken in the same order everywhere to avoid
// lock-ordering deadlocks.
func (h *hub) rebind(c *client, newID string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	c.mu.Lock()
	oldID := c.id
	c.id = newID
	c.identify = true
	c.mu.Unlock()

	if set := h.byID[oldID]; set != nil {
		delete(set, c)
		if len(set) == 0 {
			delete(h.byID, oldID)
		}
	}
	set, ok := h.byID[newID]
	if !ok {
		set = make(map[*client]bool)
		h.byID[newID] = set
	}
	set[c] = true
}

// validClientID limits identity tokens to a safe, opaque shape: no spaces or
// characters that would make logs or URLs ambiguous.
func validClientID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-' || r == '_' || r == '.':
		default:
			return false
		}
	}
	return true
}

// newClientID generates an identity token for an anonymous connection.
func newClientID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// rand.Read failing is not recoverable; a time-based fallback still
		// keeps identities unique within one process.
		return "c-" + time.Now().Format("20060102150405.000000000")
	}
	return "c-" + hex.EncodeToString(b)
}
