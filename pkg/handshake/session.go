package handshake

import (
	"io"
	"sync"
	"sync/atomic"
)

// State is the lifecycle state of a Conn handshake session.
type State uint32

const (
	// StateIdle means no handshake has been attempted yet, or the session was
	// reset and is ready for a fresh attempt.
	StateIdle State = iota

	// StateInProgress means a handshake is currently running.
	StateInProgress

	// StateComplete means the last handshake completed successfully and keys
	// (if any) are available.
	StateComplete

	// StateFailed means the last handshake failed. No keys are retained;
	// calling ClientHandshake or ServerHandshake again starts from a clean
	// state.
	StateFailed
)

// String returns a textual representation of the state.
func (s State) String() string {
	switch s {
	case StateIdle:
		return "idle"
	case StateInProgress:
		return "in-progress"
	case StateComplete:
		return "complete"
	case StateFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// Conn wraps an io.ReadWriter with handshake lifecycle state.
//
// Conn makes the "one handshake at a time per connection" rule explicit and
// guarantees that every attempt starts from a clean state: random material,
// digests and derived keys from a failed attempt are never reused.
//
// The package-level DoClient and DoServer functions already enforce the same
// concurrency rule and clean local state; Conn additionally exposes the
// lifecycle State and the retained keys to callers that want to observe them.
type Conn struct {
	rw       io.ReadWriter
	randRead func([]byte) (int, error)

	// attemptMu is held for the whole duration of a handshake attempt.
	// Callers that cannot TryLock it receive ErrHandshakeInProgress.
	attemptMu sync.Mutex

	// state is read without taking attemptMu, so State stays observable
	// while a handshake is in progress.
	state atomic.Uint32

	// resultMu protects err, keyIn and keyOut. It is never held across IO.
	resultMu sync.RWMutex
	err      error
	keyIn    []byte
	keyOut   []byte
}

// NewConn creates a handshake Conn around rw.
//
// It does not perform any IO yet; call ClientHandshake or ServerHandshake.
func NewConn(rw io.ReadWriter) *Conn {
	return NewConnWithRand(rw, defaultRandRead)
}

// NewConnWithRand is NewConn with a custom random source (useful in tests).
func NewConnWithRand(rw io.ReadWriter, randRead func([]byte) (int, error)) *Conn {
	if randRead == nil {
		randRead = defaultRandRead
	}
	c := &Conn{
		rw:       rw,
		randRead: randRead,
	}
	c.state.Store(uint32(StateIdle))
	return c
}

// State returns the current handshake state. It never blocks, even while a
// handshake is running.
func (c *Conn) State() State {
	return State(c.state.Load())
}

// Err returns the error from the last attempt, if any.
func (c *Conn) Err() error {
	c.resultMu.RLock()
	defer c.resultMu.RUnlock()
	return c.err
}

// Keys returns the receive and send keys derived by the last successful
// encrypted handshake. They are nil after a plaintext handshake or a failure.
// The returned slices must not be modified.
func (c *Conn) Keys() (keyIn []byte, keyOut []byte) {
	c.resultMu.RLock()
	defer c.resultMu.RUnlock()
	return c.keyIn, c.keyOut
}

// Reset clears all per-attempt state and returns the session to StateIdle.
//
// Reset blocks if a handshake is currently in progress; callers should not
// reset a connection whose handshake may still be writing. It is normally
// unnecessary: every ClientHandshake/ServerHandshake call already resets
// stale state before starting.
func (c *Conn) Reset() {
	c.attemptMu.Lock()
	defer c.attemptMu.Unlock()

	c.resultMu.Lock()
	c.err = nil
	c.keyIn = nil
	c.keyOut = nil
	c.resultMu.Unlock()

	c.state.Store(uint32(StateIdle))
}

// beginAttempt tries to start a new handshake attempt.
//
// It rejects concurrent callers with ErrHandshakeInProgress instead of
// letting their writes interleave. On success it has reset all stale material
// and marked the session StateInProgress. The caller must invoke
// finishAttempt (which releases attemptMu) when done.
func (c *Conn) beginAttempt() error {
	if !c.attemptMu.TryLock() {
		return ErrHandshakeInProgress
	}

	c.resultMu.Lock()
	c.err = nil
	c.keyIn = nil
	c.keyOut = nil
	c.resultMu.Unlock()

	c.state.Store(uint32(StateInProgress))
	return nil
}

// finishAttempt records the outcome and releases the attempt lock.
func (c *Conn) finishAttempt(keyIn, keyOut []byte, err error) ([]byte, []byte, error) {
	defer c.attemptMu.Unlock()

	c.resultMu.Lock()
	if err != nil {
		// Discard every piece of key material so the connection cannot be
		// mistaken for an established encrypted channel after a failure.
		c.err = err
		c.keyIn = nil
		c.keyOut = nil
		c.resultMu.Unlock()

		c.state.Store(uint32(StateFailed))
		return nil, nil, err
	}

	c.err = nil
	c.keyIn = keyIn
	c.keyOut = keyOut
	c.resultMu.Unlock()

	c.state.Store(uint32(StateComplete))
	return keyIn, keyOut, nil
}

// ClientHandshake performs a client-side handshake.
//
// If encrypted is true it performs the encrypted (DH/RC4) handshake and
// returns the derived receive/send keys; otherwise it performs the plaintext
// handshake and returns nil keys.
//
// A second call while a handshake is already running returns
// ErrHandshakeInProgress. After a failure, calling this method again starts a
// brand-new attempt with no reused random data, digests or keys.
func (c *Conn) ClientHandshake(encrypted bool, strict bool) ([]byte, []byte, error) {
	if err := c.beginAttempt(); err != nil {
		return nil, nil, err
	}

	keyIn, keyOut, err := DoClientWithRand(c.rw, encrypted, strict, c.randRead)
	return c.finishAttempt(keyIn, keyOut, err)
}

// ServerHandshake performs a server-side handshake.
//
// Encryption is negotiated from the client's C0/C1. Keys are returned for
// encrypted handshakes; plaintext handshakes return nil keys.
//
// Concurrency and retry semantics match ClientHandshake.
func (c *Conn) ServerHandshake(strict bool) ([]byte, []byte, error) {
	if err := c.beginAttempt(); err != nil {
		return nil, nil, err
	}

	keyIn, keyOut, err := DoServerWithRand(c.rw, strict, c.randRead)
	return c.finishAttempt(keyIn, keyOut, err)
}

// Client is an alias for ClientHandshake.
func (c *Conn) Client(encrypted bool, strict bool) ([]byte, []byte, error) {
	return c.ClientHandshake(encrypted, strict)
}

// Server is an alias for ServerHandshake.
func (c *Conn) Server(strict bool) ([]byte, []byte, error) {
	return c.ServerHandshake(strict)
}

// Handshaker is an alternative name for Conn.
type Handshaker = Conn

// Session is an alternative name for Conn.
type Session = Conn

// NewHandshaker creates a new Handshaker around rw.
func NewHandshaker(rw io.ReadWriter) *Handshaker {
	return NewConn(rw)
}

// NewSession creates a new Session around rw.
func NewSession(rw io.ReadWriter) *Session {
	return NewConn(rw)
}
