package handshake_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/bluenviron/gortmplib/pkg/handshake"
)

// fixedPairTranscripts runs a deterministic paired handshake and records the
// raw bytes written by each side, together with the derived keys.
func fixedPairTranscripts(t *testing.T, encrypted bool) (
	clientBytes []byte,
	serverBytes []byte,
	clientIn []byte,
	clientOut []byte,
	serverIn []byte,
	serverOut []byte,
) {
	t.Helper()

	base := &testReadWriter{ch: make(chan []byte)}
	clientRec := &recordingReadWriter{ReadWriter: base}
	serverRec := &recordingReadWriter{ReadWriter: base}

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		var err error
		serverIn, serverOut, err = handshake.DoServerWithRand(serverRec, true, testCryptoRandRead)
		require.NoError(t, err)
	}()

	var err error
	clientIn, clientOut, err = handshake.DoClientWithRand(clientRec, encrypted, true, testCryptoRandRead)
	require.NoError(t, err)
	<-serverDone

	return clientRec.writtenBytes(), serverRec.writtenBytes(),
		clientIn, clientOut, serverIn, serverOut
}

// A7: IO failures injected at every handshake stage are observable, return no
// keys and leave the session in StateFailed (never "seemingly encrypted").
func TestHandshakeFailuresAtEveryStage(t *testing.T) {
	const packet1 = 1
	const packet2 = 1 + 1536

	for _, encrypted := range []bool{false, true} {
		clientBytes, serverBytes, _, _, _, _ := fixedPairTranscripts(t, encrypted)

		side := "plain"
		if encrypted {
			side = "encrypted"
		}

		t.Run(side+" client", func(t *testing.T) {
			cases := []struct {
				name        string
				failReadAt  int64
				failWriteAt int64
			}{
				{name: "c0 write", failWriteAt: 0},
				{name: "c1 write", failWriteAt: packet1},
				{name: "c2 write", failWriteAt: packet2},
				{name: "s0 read", failReadAt: 0},
				{name: "s1 read", failReadAt: packet1},
				{name: "s2 read", failReadAt: packet2},
			}

			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					rw := newScriptRW(serverBytes)
					rw.failReadAt = tc.failReadAt
					rw.failWriteAt = tc.failWriteAt

					hc := handshake.NewConnWithRand(rw, testCryptoRandRead)
					in, out, err := hc.ClientHandshake(encrypted, true)

					require.ErrorIs(t, err, errInjected)
					require.Nil(t, in)
					require.Nil(t, out)
					require.Equal(t, handshake.StateFailed, hc.State())

					storedIn, storedOut := hc.Keys()
					require.Nil(t, storedIn)
					require.Nil(t, storedOut)
				})
			}
		})

		t.Run(side+" server", func(t *testing.T) {
			cases := []struct {
				name        string
				failReadAt  int64
				failWriteAt int64
			}{
				{name: "c0 read", failReadAt: 0},
				{name: "c1 read", failReadAt: packet1},
				{name: "c2 read", failReadAt: packet2},
				{name: "s0 write", failWriteAt: 0},
				{name: "s1 write", failWriteAt: packet1},
				{name: "s2 write", failWriteAt: packet2},
			}

			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					rw := newScriptRW(clientBytes)
					rw.failReadAt = tc.failReadAt
					rw.failWriteAt = tc.failWriteAt

					hc := handshake.NewConnWithRand(rw, testCryptoRandRead)
					in, out, err := hc.ServerHandshake(true)

					require.ErrorIs(t, err, errInjected)
					require.Nil(t, in)
					require.Nil(t, out)
					require.Equal(t, handshake.StateFailed, hc.State())

					storedIn, storedOut := hc.Keys()
					require.Nil(t, storedIn)
					require.Nil(t, storedOut)
				})
			}
		})
	}
}

// A7/A3: after a late-stage failure and a clean replay on the same
// connection/session, the retry succeeds with the expected keys rather than
// reusing material derived during the failed epoch.
func TestServerLateFailureThenCleanRetry(t *testing.T) {
	clientBytes, _, cIn, cOut, sIn, sOut := fixedPairTranscripts(t, true)
	require.Equal(t, cOut, sIn)
	require.Equal(t, cIn, sOut)

	rw := newScriptRW(clientBytes)
	rw.failWriteAt = 1 + 1536 // fail while writing S2

	hc := handshake.NewConnWithRand(rw, testCryptoRandRead)
	_, _, err := hc.ServerHandshake(true)
	require.ErrorIs(t, err, errInjected)
	require.Equal(t, handshake.StateFailed, hc.State())

	// Replay a clean client transcript on the same ReadWriter and session.
	rw.reset(clientBytes)

	in, out, err := hc.ServerHandshake(true)
	require.NoError(t, err)
	require.Equal(t, sIn, in)
	require.Equal(t, sOut, out)
	require.Equal(t, handshake.StateComplete, hc.State())
}
