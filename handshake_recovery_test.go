package gortmplib_test

import (
	"bytes"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bluenviron/gortmplib"
	"github.com/bluenviron/gortmplib/pkg/amf0"
	"github.com/bluenviron/gortmplib/pkg/bytecounter"
	"github.com/bluenviron/gortmplib/pkg/handshake"
	"github.com/bluenviron/gortmplib/pkg/message"
	"github.com/bluenviron/gortmplib/pkg/rc4rw"
)

// A6 end to end: an encrypted handshake followed by RC4-framed RTMP messages
// in both directions. A failure to derive corresponding keys or to wrap the
// connection correctly makes the connect command unreadable.
func TestServerConnEncryptedHandshakeMessageRoundTrip(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	initErr := make(chan error, 1)

	go func() {
		nconn, aerr := ln.Accept()
		if aerr != nil {
			initErr <- aerr
			return
		}
		defer nconn.Close()

		sc := &gortmplib.ServerConn{RW: nconn}
		if ierr := sc.Initialize(); ierr != nil {
			initErr <- ierr
			return
		}
		initErr <- nil

		// Writes the window ack / bandwidth / chunk size / _result sequence
		// and then blocks waiting for createStream; its error after the test
		// closes the socket is expected and discarded.
		_ = sc.AcceptConn()
	}()

	cconn, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	defer cconn.Close()

	bc := bytecounter.NewReadWriter(cconn)

	keyIn, keyOut, err := handshake.DoClient(bc, true, true)
	require.NoError(t, err)
	require.Len(t, keyIn, 16)
	require.Len(t, keyOut, 16)

	rc, err := rc4rw.New(bc, keyIn, keyOut)
	require.NoError(t, err)

	mrw := message.NewReadWriter(rc, bc, true)

	err = mrw.Write(&message.CommandAMF0{
		ChunkStreamID: 3,
		Name:          "connect",
		CommandID:     1,
		Arguments: []any{
			amf0.Object{
				{Key: "app", Value: "stream"},
				{Key: "tcUrl", Value: "rtmp://localhost/stream"},
			},
		},
	})
	require.NoError(t, err)

	select {
	case ierr := <-initErr:
		require.NoError(t, ierr)
	case <-time.After(10 * time.Second):
		t.Fatal("server Initialize did not complete")
	}

	_ = cconn.SetReadDeadline(time.Now().Add(10 * time.Second))

	var gotConnectResult bool
	for range 4 {
		msg, rerr := mrw.Read()
		require.NoError(t, rerr)

		if cmd, ok := msg.(*message.CommandAMF0); ok && cmd.Name == "_result" {
			gotConnectResult = true
		}
	}
	require.True(t, gotConnectResult, "expected connect _result among server messages")
}

// A4 at the gortmplib surface: ServerConn.Initialize surfaces the
// distinguishable version mismatch sentinel instead of downgrading.
func TestServerConnInitializeVersionMismatch(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteByte(handshake.C0VersionEncrypted) // C0 declares encryption
	c1 := make([]byte, 1536)
	// C1 Version field (offset 4..8) stays zero: inconsistent with C0.
	_, err := buf.Write(c1)
	require.NoError(t, err)

	sc := &gortmplib.ServerConn{RW: &buf}
	err = sc.Initialize()
	require.Error(t, err)
	require.ErrorIs(t, err, handshake.ErrVersionMismatch)
	require.ErrorIs(t, err, gortmplib.ErrHandshakeVersionMismatch)
}

var errRootBlockedRead = errors.New("blocking reader released with failure")

type blockReadWriter struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockReadWriter) Read(_ []byte) (int, error) {
	b.once.Do(func() {
		close(b.entered)
		<-b.release
	})
	return 0, errRootBlockedRead
}

func (b *blockReadWriter) Write(p []byte) (int, error) {
	return len(p), nil
}

// A5 at the gortmplib surface: concurrent Initialize calls on the same
// ServerConn are rejected with ErrHandshakeInProgress.
func TestServerConnConcurrentInitializeRejected(t *testing.T) {
	brw := &blockReadWriter{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}

	sc := &gortmplib.ServerConn{RW: brw}

	firstErr := make(chan error, 1)
	go func() {
		firstErr <- sc.Initialize()
	}()

	<-brw.entered

	err := sc.Initialize()
	require.Error(t, err)
	require.ErrorIs(t, err, gortmplib.ErrHandshakeInProgress)
	require.ErrorIs(t, err, handshake.ErrHandshakeInProgress)

	close(brw.release)

	select {
	case ferr := <-firstErr:
		require.ErrorIs(t, ferr, errRootBlockedRead)
	case <-time.After(5 * time.Second):
		t.Fatal("first Initialize did not return after release")
	}
}

// Ensure io.ReadWriter is implemented by the blocking test double.
var _ io.ReadWriter = (*blockReadWriter)(nil)
