package handshake

import (
	"io"
	"reflect"
	"sync"
)

// handshakeRole identifies which side of a connection owns a handshake gate.
//
// Client and server gates are tracked separately because in tests (and in
// in-memory transports) both roles are sometimes driven through a single
// shared io.ReadWriter. Two handshakes of the same role on the same endpoint
// must never run concurrently.
type handshakeRole byte

const (
	clientHandshake handshakeRole = iota
	serverHandshake
)

// handshakeGate tracks whether one handshake is currently running for a
// (connection, role) pair.
type handshakeGate struct {
	busy bool
}

var (
	handshakeRegistryMu sync.Mutex
	handshakeRegistry   = map[any]*handshakeGate{}
)

// gateKey returns a stable, comparable identity for rw.
//
// Nearly all real io.ReadWriter implementations are pointers (*net.TCPConn,
// tls.Conn wrappers, bytecounter.ReadWriter, test doubles, ...). Comparable
// value types are used directly. Non-comparable values cannot be used as map
// keys safely; for those a nil key is returned and concurrency tracking is
// skipped for the package-level functions (the stateful Conn type still
// serializes calls made through the same instance).
func gateKey(rw io.ReadWriter, role handshakeRole) any {
	if rw == nil {
		return nil
	}

	rv := reflect.ValueOf(rw)

	var identity any

	switch rv.Kind() {
	case reflect.Pointer, reflect.Chan, reflect.Func, reflect.Map, reflect.UnsafePointer:
		identity = pointerIdentity{typ: rv.Type(), ptr: rv.Pointer()}

	case reflect.Slice:
		// A slice-backed ReadWriter is unusual; track the backing array.
		identity = pointerIdentity{typ: rv.Type(), ptr: rv.Pointer()}

	default:
		if !rv.Type().Comparable() {
			return nil
		}
		identity = rw
	}

	return combinedGateKey{role: role, identity: identity}
}

type pointerIdentity struct {
	typ reflect.Type
	ptr uintptr
}

type combinedGateKey struct {
	role     handshakeRole
	identity any
}

// acquireHandshake attempts to mark the connection as handshaking for role.
//
// It returns a release function on success. If another handshake of the same
// role is already in progress, it returns ErrHandshakeInProgress and a nil
// release function; the rejected caller must not perform any IO.
func acquireHandshake(rw io.ReadWriter, role handshakeRole) (func(), error) {
	key := gateKey(rw, role)
	if key == nil {
		// Unidentifiable writer: do not risk a map-key panic. Callers using
		// NewConn still get instance-level mutual exclusion.
		return func() {}, nil
	}

	handshakeRegistryMu.Lock()
	defer handshakeRegistryMu.Unlock()

	g := handshakeRegistry[key]
	if g == nil {
		g = &handshakeGate{}
		handshakeRegistry[key] = g
	}

	if g.busy {
		return nil, ErrHandshakeInProgress
	}

	g.busy = true

	released := false
	return func() {
		handshakeRegistryMu.Lock()
		defer handshakeRegistryMu.Unlock()

		if released {
			return
		}
		released = true

		g.busy = false
		// Remove the entry only if the registry still points at this gate.
		// Every busy transition is serialized by handshakeRegistryMu, so no
		// concurrent owner can be deleted here.
		if handshakeRegistry[key] == g {
			delete(handshakeRegistry, key)
		}
	}, nil
}
