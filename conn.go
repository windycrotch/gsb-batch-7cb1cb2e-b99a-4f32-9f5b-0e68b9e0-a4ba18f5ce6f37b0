package gortmplib

import (
	"github.com/bluenviron/gortmplib/pkg/handshake"
	"github.com/bluenviron/gortmplib/pkg/message"
)

// ErrHandshakeInProgress is returned when Initialize is called concurrently
// on the same connection while another initialization/handshake is still in
// progress. It is an alias of the handshake package sentinel.
var ErrHandshakeInProgress = handshake.ErrHandshakeInProgress

// ErrHandshakeVersionMismatch is returned when the two peers cannot agree on
// whether encryption is used because C0 and C1/S1 contradict each other.
// It is an alias of the handshake package sentinel.
var ErrHandshakeVersionMismatch = handshake.ErrVersionMismatch

// Conn is implemented by Client and ServerConn.
type Conn interface {
	BytesReceived() uint64
	BytesSent() uint64
	Read() (message.Message, error)
	Write(msg message.Message) error
}
