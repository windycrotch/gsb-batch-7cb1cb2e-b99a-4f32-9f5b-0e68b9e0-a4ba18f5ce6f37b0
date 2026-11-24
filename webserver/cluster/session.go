package cluster

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
)

// sessionDrainTimeout bounds how long a superseded session may keep serving
// streams that were opened before it was replaced. Once it elapses the
// underlying tunnel is closed even if some streams are still transferring.
const sessionDrainTimeout = 30 * time.Second

// sessionPingTimeout bounds the post-handshake liveness probe. A slave whose
// yamux session does not answer a ping is treated as a failed handshake so the
// previous usable session stays in service.
const sessionPingTimeout = 10 * time.Second

// managedSession wraps a yamux session with the lifecycle bookkeeping the
// master needs while several generations of the same slave overlap.
type managedSession struct {
	session *yamux.Session

	// retired is closed once the session has been superseded by a newer one.
	// A retired session keeps serving its already opened streams but must not
	// receive any new stream, and its underlying connection is torn down either
	// when the last stream ends or after the drain timeout.
	retired chan struct{}
	once    sync.Once
}

func newManagedSession(session *yamux.Session) *managedSession {
	return &managedSession{
		session: session,
		retired: make(chan struct{}),
	}
}

// retire marks the session as superseded. New callers stop selecting it while
// existing streams keep running. The yamux GoAway frame tells the slave not to
// offer further streams over this connection; the session itself is left open
// until its streams drain or the drain timeout fires.
func (ms *managedSession) retire() {
	ms.once.Do(func() {
		close(ms.retired)
		if err := ms.session.GoAway(); err != nil {
			slog.Debug("cluster: goaway on superseded session failed", "error", err)
		}
	})
}

func (ms *managedSession) isRetired() bool {
	select {
	case <-ms.retired:
		return true
	default:
		return false
	}
}

// waitDrained blocks until every stream opened on the session has finished or
// the drain timeout elapses. It does not close the session itself.
func (ms *managedSession) waitDrained(drainTimeout time.Duration) {
	if drainTimeout <= 0 {
		drainTimeout = sessionDrainTimeout
	}
	deadline := time.NewTimer(drainTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		// NumStreams drops to zero once the peer acknowledges all stream closes.
		if ms.session.IsClosed() || ms.session.NumStreams() == 0 {
			return
		}
		select {
		case <-ms.session.CloseChan():
			return
		case <-deadline.C:
			return
		case <-ticker.C:
		}
	}
}

// verifySession probes a freshly established yamux session before it is allowed
// to replace the current one. A WebSocket upgrade and hello exchange can
// succeed against a half-dead connection; requiring a round trip guarantees
// the new session is actually usable and keeps a failed reconnection from
// taking a healthy slave offline.
func verifySession(ctx context.Context, session *yamux.Session, pingTimeout time.Duration) error {
	if pingTimeout <= 0 {
		pingTimeout = sessionPingTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := session.Ping()
		done <- err
	}()

	select {
	case <-ctx.Done():
		_ = session.Close()
		<-done
		return ctx.Err()
	case err := <-done:
		if err != nil {
			_ = session.Close()
			return err
		}
		return nil
	}
}

// openSessionStream opens a stream while honoring the caller's context. yamux
// OpenStream itself does not take a context, so a canceled or timed-out
// request abandons the attempt instead of getting stuck on a session that is
// shutting down underneath it.
func openSessionStream(ctx context.Context, session *yamux.Session) (net.Conn, error) {
	type result struct {
		conn net.Conn
		err  error
	}
	opened := make(chan result, 1)
	go func() {
		stream, err := session.OpenStream()
		opened <- result{conn: stream, err: err}
	}()

	select {
	case <-ctx.Done():
		go func() {
			res := <-opened
			if res.err == nil && res.conn != nil {
				_ = res.conn.Close()
			}
		}()
		return nil, ctx.Err()
	case res := <-opened:
		if res.err != nil {
			return nil, res.err
		}
		return res.conn, nil
	}
}
