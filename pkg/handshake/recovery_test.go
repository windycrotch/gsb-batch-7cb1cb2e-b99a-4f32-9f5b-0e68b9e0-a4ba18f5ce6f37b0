package handshake_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/bluenviron/gortmplib/pkg/handshake"
)

var errInjected = errors.New("injected IO failure")

// scriptRW serves a preloaded byte stream for reads and records every write.
//
// failReadAt/failWriteAt are absolute stream offsets. When the next operation
// starts at that offset, errInjected is returned without touching the
// underlying buffer. -1 disables injection.
type scriptRW struct {
	reader *bytes.Reader

	writeBuf bytes.Buffer

	readBytes  int64
	writeBytes int64

	failReadAt  int64
	failWriteAt int64
}

func newScriptRW(input []byte) *scriptRW {
	return &scriptRW{
		reader:      bytes.NewReader(input),
		failReadAt:  -1,
		failWriteAt: -1,
	}
}

func (s *scriptRW) Read(p []byte) (int, error) {
	if s.failReadAt >= 0 && s.readBytes == s.failReadAt {
		return 0, errInjected
	}

	n, err := s.reader.Read(p)
	s.readBytes += int64(n)
	return n, err
}

func (s *scriptRW) Write(p []byte) (int, error) {
	if s.failWriteAt >= 0 && s.writeBytes == s.failWriteAt {
		return 0, errInjected
	}

	n, err := s.writeBuf.Write(p)
	s.writeBytes += int64(n)
	return n, err
}

func (s *scriptRW) written() []byte {
	return s.writeBuf.Bytes()
}

// reset reloads the read side with input and clears counters/faults. It is
// meant to simulate restarting a handshake on the same connection object.
func (s *scriptRW) reset(input []byte) {
	s.reader = bytes.NewReader(input)
	s.writeBuf.Reset()
	s.readBytes = 0
	s.writeBytes = 0
	s.failReadAt = -1
	s.failWriteAt = -1
}

// blockGateRW parks the first Read or Write until release is closed, while
// advertising that the operation started on entered. This makes concurrency
// tests deterministic: the first caller is known to hold the handshake gate.
type blockGateRW struct {
	io.ReadWriter

	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *blockGateRW) park() {
	g.once.Do(func() {
		close(g.entered)
		<-g.release
	})
}

func (g *blockGateRW) Read(p []byte) (int, error) {
	g.park()
	return g.ReadWriter.Read(p)
}

func (g *blockGateRW) Write(p []byte) (int, error) {
	g.park()
	return g.ReadWriter.Write(p)
}

func newBlockGateRW(inner io.ReadWriter) *blockGateRW {
	return &blockGateRW{
		ReadWriter: inner,
		entered:    make(chan struct{}),
		release:    make(chan struct{}),
	}
}

// runCleanPair runs a full paired handshake over the lockstep test transport
// and returns the four keys.
func runCleanPair(t *testing.T, encrypted bool) (clientIn, clientOut, serverIn, serverOut []byte) {
	t.Helper()

	rw := &testReadWriter{ch: make(chan []byte)}
	serverDone := make(chan struct{})

	go func() {
		defer close(serverDone)
		var err error
		serverIn, serverOut, err = handshake.DoServerWithRand(rw, true, testCryptoRandRead)
		require.NoError(t, err)
	}()

	var err error
	clientIn, clientOut, err = handshake.DoClientWithRand(rw, encrypted, true, testCryptoRandRead)
	require.NoError(t, err)

	<-serverDone
	return clientIn, clientOut, serverIn, serverOut
}

// A1/A2: plaintext returns no keys, encrypted returns corresponding keys.
func TestHandshakeKeysContract(t *testing.T) {
	t.Run("plain", func(t *testing.T) {
		cIn, cOut, sIn, sOut := runCleanPair(t, false)
		require.Nil(t, cIn)
		require.Nil(t, cOut)
		require.Nil(t, sIn)
		require.Nil(t, sOut)
	})

	t.Run("encrypted", func(t *testing.T) {
		cIn, cOut, sIn, sOut := runCleanPair(t, true)
		require.Len(t, cIn, 16)
		require.Len(t, cOut, 16)
		require.Len(t, sIn, 16)
		require.Len(t, sOut, 16)
		require.Equal(t, cOut, sIn)
		require.Equal(t, cIn, sOut)
	})
}

// A4: the server rejects C0/C1 version contradictions with a distinguishable
// error wrapping ErrVersionMismatch.
func TestDoServerRejectsVersionMismatch(t *testing.T) {
	cases := []struct {
		name  string
		c0    byte
		c1Ver uint32
	}{
		{name: "c0=6 with plain c1 version 3", c0: 6, c1Ver: 3},
		{name: "c0=6 with zero c1 version", c0: 6, c1Ver: 0},
		{name: "c0=3 with encrypted c1 version", c0: 3, c1Ver: handshake.EncryptedVersion},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c1 := make([]byte, 1536)
			binary.BigEndian.PutUint32(c1[4:8], tc.c1Ver)

			input := append([]byte{tc.c0}, c1...)
			rw := newSplitReadWriter(input)

			keyIn, keyOut, err := handshake.DoServer(rw, true)
			require.Error(t, err)
			require.ErrorIs(t, err, handshake.ErrVersionMismatch)
			require.Nil(t, keyIn)
			require.Nil(t, keyOut)

			var mismatch *handshake.VersionMismatchError
			require.ErrorAs(t, err, &mismatch)
			require.Equal(t, tc.c0, mismatch.C0Version)
		})
	}
}

// An unsupported C0 version yields the distinguishable ErrInvalidVersion.
func TestDoServerInvalidC0Version(t *testing.T) {
	rw := newSplitReadWriter([]byte{9, 0, 0, 0})
	_, _, err := handshake.DoServer(rw, true)
	require.Error(t, err)
	require.ErrorIs(t, err, handshake.ErrInvalidVersion)
	require.NotErrorIs(t, err, handshake.ErrVersionMismatch)
}

// A5: two concurrent client handshakes on one endpoint: exactly one runs, the
// other gets ErrHandshakeInProgress without writing anything.
func TestConcurrentDoClientRejected(t *testing.T) {
	base := &testReadWriter{ch: make(chan []byte)}
	gate := newBlockGateRW(base)

	serverDone := make(chan struct{})
	var serverErr error
	var serverIn, serverOut []byte
	go func() {
		defer close(serverDone)
		serverIn, serverOut, serverErr = handshake.DoServer(base, true)
	}()

	type clientResult struct {
		in, out []byte
		err     error
	}
	firstDone := make(chan clientResult, 1)
	go func() {
		in, out, err := handshake.DoClient(gate, true, true)
		firstDone <- clientResult{in, out, err}
	}()

	<-gate.entered

	// Second concurrent caller must be rejected immediately.
	in2, out2, err2 := handshake.DoClient(gate, true, true)
	require.ErrorIs(t, err2, handshake.ErrHandshakeInProgress)
	require.Nil(t, in2)
	require.Nil(t, out2)

	close(gate.release)

	first := <-firstDone
	require.NoError(t, first.err)

	<-serverDone
	require.NoError(t, serverErr)
	require.Equal(t, first.out, serverIn)
	require.Equal(t, first.in, serverOut)
}

// A5: same guarantee for two concurrent server handshakes.
func TestConcurrentDoServerRejected(t *testing.T) {
	base := &testReadWriter{ch: make(chan []byte)}
	gate := newBlockGateRW(base)

	type serverResult struct {
		in, out []byte
		err     error
	}
	firstDone := make(chan serverResult, 1)
	go func() {
		in, out, err := handshake.DoServer(gate, true)
		firstDone <- serverResult{in, out, err}
	}()

	<-gate.entered

	in2, out2, err2 := handshake.DoServer(gate, true)
	require.ErrorIs(t, err2, handshake.ErrHandshakeInProgress)
	require.Nil(t, in2)
	require.Nil(t, out2)

	close(gate.release)

	// A single client completes the winning server handshake.
	clientIn, clientOut, err := handshake.DoClient(base, true, true)
	require.NoError(t, err)

	first := <-firstDone
	require.NoError(t, first.err)
	require.Equal(t, clientOut, first.in)
	require.Equal(t, clientIn, first.out)
}

// A3: after a read failure at the S1 stage, calling the client handshake
// again on the same ReadWriter with a fresh transcript succeeds and derives
// exactly the same keys as an independently executed clean handshake.
func TestClientRetryAfterS1ReadFailure(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		name := "plain"
		if encrypted {
			name = "encrypted"
		}

		t.Run(name, func(t *testing.T) {
			cleanIn, cleanOut, _, _ := runCleanPair(t, encrypted)

			s0Version := handshake.C0VersionPlain
			if encrypted {
				s0Version = handshake.C0VersionEncrypted
			}

			// Preload a valid S0 byte so the client reaches the S1 read;
			// fail there (S1 starts at stream offset 1).
			rw := newScriptRW([]byte{s0Version})
			rw.failReadAt = 1

			randCalls := 0
			counterRand := func(p []byte) (int, error) {
				randCalls++
				return testCryptoRandRead(p)
			}

			in, out, err := handshake.DoClientWithRand(rw, encrypted, true, counterRand)
			require.ErrorIs(t, err, errInjected)
			require.Nil(t, in)
			require.Nil(t, out)
			failedCalls := randCalls
			require.Greater(t, failedCalls, 0)

			// Build a valid S0/S1/S2 transcript from the C0/C1 bytes that
			// the failed attempt wrote. The server run fails afterwards when
			// C2 is missing, but its writes contain the transcript.
			sent := bytes.Clone(rw.written())
			require.NotEmpty(t, sent)

			serverRW := newScriptRW(sent)
			_, _, srvErr := handshake.DoServerWithRand(serverRW, true, testCryptoRandRead)
			require.Error(t, srvErr) // expected: no C2 available

			transcript := bytes.Clone(serverRW.written())
			require.Len(t, transcript, 1+1536+1536)

			// Retry on the SAME connection object.
			rw.reset(transcript)

			retryIn, retryOut, err := handshake.DoClientWithRand(rw, encrypted, true, counterRand)
			require.NoError(t, err)
			require.Greater(t, randCalls, failedCalls,
				"retry must draw fresh random material instead of reusing the failed epoch")
			require.Equal(t, cleanIn, retryIn)
			require.Equal(t, cleanOut, retryOut)
		})
	}
}

// A3/A7: a stateful server session fails (here at C2 read), exposes the
// failed state with no keys, then a retry on the same session/connection
// starts cleanly and succeeds.
func TestConnServerFailureThenRetry(t *testing.T) {
	clientIn, clientOut, _, _ := runCleanPair(t, true)

	// Capture a full valid C0/C1/C2 client transcript.
	base := &testReadWriter{ch: make(chan []byte)}
	recorder := &recordingReadWriter{ReadWriter: base}
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		_, _, err := handshake.DoServerWithRand(base, true, testCryptoRandRead)
		require.NoError(t, err)
	}()
	_, _, err := handshake.DoClientWithRand(recorder, true, true, testCryptoRandRead)
	require.NoError(t, err)
	<-serverDone
	transcript := recorder.writtenBytes()
	require.Len(t, transcript, 1+1536+1536)

	rw := newScriptRW(transcript)
	// C0+C1 take 1537 bytes; fail when C2 starts.
	rw.failReadAt = 1 + 1536

	hc := handshake.NewConnWithRand(rw, testCryptoRandRead)
	require.Equal(t, handshake.StateIdle, hc.State())

	in, out, err := hc.ServerHandshake(true)
	require.ErrorIs(t, err, errInjected)
	require.Nil(t, in)
	require.Nil(t, out)
	require.Equal(t, handshake.StateFailed, hc.State())
	require.ErrorIs(t, hc.Err(), errInjected)

	gotIn, gotOut := hc.Keys()
	require.Nil(t, gotIn)
	require.Nil(t, gotOut)

	// Concurrent call while idle-after-failure is not blocked by the mutex;
	// exercise a successful clean retry.
	rw.reset(transcript)

	in, out, err = hc.ServerHandshake(true)
	require.NoError(t, err)
	require.Len(t, in, 16)
	require.Len(t, out, 16)
	require.Equal(t, handshake.StateComplete, hc.State())
	require.Equal(t, clientOut, in)
	require.Equal(t, clientIn, out)
}

// A3 at the package entrypoint: after a failing DoServerWithRand call, a
// second call on the same ReadWriter with a replayed valid transcript
// succeeds and yields the clean-handshake keys.
func TestDoServerWithRandRetryAfterFailure(t *testing.T) {
	clientBytes, _, _, _, srvIn, srvOut := fixedPairTranscripts(t, true)

	rw := newScriptRW(clientBytes)
	rw.failReadAt = 1 + 1536 // fail at C2 read

	in, out, err := handshake.DoServerWithRand(rw, true, testCryptoRandRead)
	require.ErrorIs(t, err, errInjected)
	require.Nil(t, in)
	require.Nil(t, out)

	rw.reset(clientBytes)

	in, out, err = handshake.DoServerWithRand(rw, true, testCryptoRandRead)
	require.NoError(t, err)
	require.Equal(t, srvIn, in)
	require.Equal(t, srvOut, out)
}

// A5 at the stateful Conn level: concurrent methods get the sentinel error.
func TestConnConcurrentHandshakeRejected(t *testing.T) {
	base := &testReadWriter{ch: make(chan []byte)}
	gate := newBlockGateRW(base)
	hc := handshake.NewConnWithRand(gate, testCryptoRandRead)

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		_, _, err := handshake.DoServer(base, true)
		require.NoError(t, err)
	}()

	firstDone := make(chan error, 1)
	go func() {
		_, _, err := hc.ClientHandshake(true, true)
		firstDone <- err
	}()

	<-gate.entered

	_, _, err := hc.ClientHandshake(true, true)
	require.ErrorIs(t, err, handshake.ErrHandshakeInProgress)
	require.Equal(t, handshake.StateInProgress, hc.State())

	close(gate.release)
	require.NoError(t, <-firstDone)
	<-serverDone
	require.Equal(t, handshake.StateComplete, hc.State())
}

// recordingReadWriter tees all writes into an internal buffer.
type recordingReadWriter struct {
	io.ReadWriter
	mu  sync.Mutex
	buf bytes.Buffer
}

func (r *recordingReadWriter) Write(p []byte) (int, error) {
	r.mu.Lock()
	_, _ = r.buf.Write(p)
	r.mu.Unlock()
	return r.ReadWriter.Write(p)
}

func (r *recordingReadWriter) writtenBytes() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return bytes.Clone(r.buf.Bytes())
}
