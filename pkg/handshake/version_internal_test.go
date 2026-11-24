package handshake

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

func internalRand(p []byte) (int, error) {
	for i := range p {
		p[i] = 0x01
	}
	return len(p), nil
}

func c0Byte(v byte) *C0S0 {
	return &C0S0{Version: v}
}

// A8: a correctly digested C1 whose Version contradicts C0 must still be
// rejected with ErrVersionMismatch. The check targets version consistency,
// not digest validity.
func TestVersionConsistencyLegalDigestButWrongVersion(t *testing.T) {
	pub := make([]byte, dhKeyLength)
	for i := range pub {
		pub[i] = 0x02
	}

	t.Run("c0=6 with legal digest but non-encrypted c1 version", func(t *testing.T) {
		var c1 C1S1
		c1.Version = 3 // contradicts C0=6
		_, err := c1.fill(false, pub, internalRand)
		require.NoError(t, err)

		// Precondition: the digest really is valid.
		_, _, err = c1.validateDigest(false)
		require.NoError(t, err)

		err = CheckVersionConsistency(c0Byte(6), &c1)
		require.Error(t, err)
		require.ErrorIs(t, err, ErrVersionMismatch)
	})

	t.Run("c0=3 with legal encrypted digest and zero c1 version", func(t *testing.T) {
		var c1 C1S1
		c1.Version = 0
		_, err := c1.fill(false, pub, internalRand)
		require.NoError(t, err)

		// Precondition: it really carries a valid genuine-client digest.
		_, _, err = c1.validateDigest(false)
		require.NoError(t, err)

		err = CheckVersionConsistency(c0Byte(3), &c1)
		require.Error(t, err)
		require.ErrorIs(t, err, ErrVersionMismatch)
	})

	t.Run("consistent encrypted pair passes", func(t *testing.T) {
		var c1 C1S1
		c1.Version = EncryptedVersion
		_, err := c1.fill(false, pub, internalRand)
		require.NoError(t, err)

		require.NoError(t, CheckVersionConsistency(c0Byte(6), &c1))
	})
}

// End-to-end through DoServerWithRand: C0=6 plus a legally digested C1 with a
// wrong version produces the distinguishable sentinel error.
func TestDoServerWithRandLegalDigestWrongVersion(t *testing.T) {
	var c1 C1S1
	c1.Version = 0x00000003
	_, err := c1.fill(false, make([]byte, dhKeyLength), internalRand)
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, buf.WriteByte(6))
	require.NoError(t, c1.Write(&buf))

	keyIn, keyOut, err := DoServerWithRand(&buf, true, internalRand)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrVersionMismatch)
	require.Nil(t, keyIn)
	require.Nil(t, keyOut)
}
