package handshake

import (
	"errors"
	"fmt"
)

// Sentinel errors returned by handshake operations.
var (
	// ErrHandshakeInProgress is returned when a second handshake is attempted
	// on a connection while another handshake is still running.
	//
	// Callers may use errors.Is to detect this condition and must not write
	// handshake packets from the rejected call.
	ErrHandshakeInProgress = errors.New("rtmp handshake already in progress on this connection")

	// ErrVersionMismatch is returned when the RTMP version declared in C0/S0
	// is inconsistent with the version fields or digest carried by C1/S1,
	// for example C0 declaring the encrypted version while C1 does not use
	// the encrypted protocol (or vice versa).
	//
	// The server returns this error instead of silently downgrading an
	// encrypted negotiation to a plaintext handshake.
	ErrVersionMismatch = errors.New("rtmp handshake version declaration does not match the handshake payload")

	// ErrInvalidVersion is returned when an RTMP version byte or field uses a
	// value that is not supported at all (as opposed to contradicting another
	// part of the same handshake).
	ErrInvalidVersion = errors.New("invalid or unsupported rtmp handshake version")
)

// Compatibility aliases for the two primary sentinels. Code may refer to the
// condition using whichever name reads most naturally at the call site; all
// aliases preserve errors.Is identity.
var (
	// ErrHandshakeBusy is an alias for ErrHandshakeInProgress.
	ErrHandshakeBusy = ErrHandshakeInProgress

	// ErrConcurrentHandshake is an alias for ErrHandshakeInProgress.
	ErrConcurrentHandshake = ErrHandshakeInProgress

	// ErrVersionConflict is an alias for ErrVersionMismatch.
	ErrVersionConflict = ErrVersionMismatch
)

// VersionMismatchError provides details about a version inconsistency.
//
// It wraps ErrVersionMismatch, so callers can detect every version conflict
// with errors.Is(err, handshake.ErrVersionMismatch).
type VersionMismatchError struct {
	// C0Version is the version byte declared in C0 (or S0).
	C0Version byte

	// PacketVersion is the 32-bit Version field of C1/S1, when available.
	PacketVersion uint32

	// Reason describes which consistency rule was violated.
	Reason string
}

// Error implements error.
func (e *VersionMismatchError) Error() string {
	return fmt.Sprintf("rtmp handshake version mismatch (c0=%d, c1s1-version=0x%08x): %s",
		e.C0Version, e.PacketVersion, e.Reason)
}

// Unwrap exposes ErrVersionMismatch for errors.Is/errors.As.
func (e *VersionMismatchError) Unwrap() error {
	return ErrVersionMismatch
}
