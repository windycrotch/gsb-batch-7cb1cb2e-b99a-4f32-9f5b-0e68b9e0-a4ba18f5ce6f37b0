package rc4rw_test

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bluenviron/gortmplib/pkg/rc4rw"
)

// A6: two RC4 ReadWriters keyed with corresponding (read/write swapped) keys
// exchange a message in both directions and recover the exact plaintext.
func TestReadWriterCrossRoundTrip(t *testing.T) {
	clientKeyIn := bytes.Repeat([]byte{0x11}, 16)
	clientKeyOut := bytes.Repeat([]byte{0x22}, 16)
	// The server derives the swapped directions.
	serverKeyIn := clientKeyOut
	serverKeyOut := clientKeyIn

	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	client, err := rc4rw.New(a, clientKeyIn, clientKeyOut)
	require.NoError(t, err)

	server, err := rc4rw.New(b, serverKeyIn, serverKeyOut)
	require.NoError(t, err)

	want := bytes.Repeat([]byte("rtmp-encrypted-message-"), 64)

	serverErrs := make(chan error, 2)
	go func() {
		_, werr := server.Write(bytes.Clone(want))
		serverErrs <- werr

		buf := make([]byte, len(want))
		_, rerr := io.ReadFull(server, buf)
		if rerr == nil && !bytes.Equal(buf, want) {
			rerr = io.ErrShortBuffer
		}
		serverErrs <- rerr
	}()

	// First receive the server -> client message.
	clientBuf := make([]byte, len(want))
	readDone := make(chan error, 1)
	go func() {
		_ = a.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, rerr := io.ReadFull(client, clientBuf)
		readDone <- rerr
	}()

	select {
	case readErr := <-readDone:
		require.NoError(t, readErr)
	case <-time.After(6 * time.Second):
		t.Fatal("timeout reading server message")
	}
	require.Equal(t, want, clientBuf)
	require.NoError(t, <-serverErrs) // server write result

	// Then send client -> server. Write XORs the slice in place.
	_, err = client.Write(bytes.Clone(want))
	require.NoError(t, err)

	require.NoError(t, <-serverErrs) // server read/compare result

	// Writing through RC4 must actually obfuscate the bytes on the wire.
	raw, rawPeer := net.Pipe()
	defer raw.Close()
	defer rawPeer.Close()

	rw1, err := rc4rw.New(raw, clientKeyIn, clientKeyOut)
	require.NoError(t, err)

	wantWire := bytes.Repeat([]byte("obfuscate-me-"), 8)

	wire := make(chan []byte, 1)
	go func() {
		one := make([]byte, len(wantWire))
		_, rerr := io.ReadFull(rawPeer, one)
		if rerr != nil {
			wire <- nil
			return
		}
		wire <- one
	}()

	_, err = rw1.Write(bytes.Clone(wantWire))
	require.NoError(t, err)

	select {
	case got := <-wire:
		require.NotNil(t, got)
		require.Len(t, got, len(wantWire))
		require.NotEqual(t, wantWire, got, "wire bytes must be ciphertext")
	case <-time.After(5 * time.Second):
		t.Fatal("timeout reading ciphertext")
	}
}
