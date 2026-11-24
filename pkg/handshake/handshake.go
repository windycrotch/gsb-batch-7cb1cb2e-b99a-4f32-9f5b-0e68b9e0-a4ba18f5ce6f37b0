// Package handshake contains the RTMP handshake mechanism.
package handshake

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"io"
)

const (
	// C0VersionPlain is the C0/S0 version byte used by the plaintext
	// handshake.
	C0VersionPlain byte = 3

	// C0VersionEncrypted is the C0/S0 version byte used by the encrypted
	// handshake.
	C0VersionEncrypted byte = 6

	// EncryptedVersion is the 32-bit version field carried by encrypted
	// C1/S1 packets (Flash Media Server 3.5.1.1).
	EncryptedVersion = 3<<24 | 5<<16 | 1<<8 | 1

	encryptedVersion = EncryptedVersion
)

// defaultRandRead is the system random source. It is a variable so tests can
// substitute it when needed.
var defaultRandRead = rand.Read

func doClientEncrypted(rw io.ReadWriter, randRead func([]byte) (int, error)) ([]byte, []byte, error) {
	var c0 C0S0

	c0.Version = 6

	err := c0.Write(rw)
	if err != nil {
		return nil, nil, err
	}

	localPrivateKey, localPublicKey, err := dhGenerateKeyPair(randRead)
	if err != nil {
		return nil, nil, err
	}

	var c1 C1S1

	// Declare the encrypted protocol version in C1 so the server can verify
	// that C0 and C1 describe the same (encrypted) negotiation. The digest is
	// computed over this field, so it must be set before fill.
	c1.Version = EncryptedVersion

	c1Digest, err := c1.fill(false, localPublicKey, randRead)
	if err != nil {
		return nil, nil, err
	}

	err = c1.Write(rw)
	if err != nil {
		return nil, nil, err
	}

	var s0 C0S0

	err = s0.Read(rw)
	if err != nil {
		return nil, nil, err
	}

	if s0.Version != 6 {
		return nil, nil, fmt.Errorf("server replied with unexpected version %d", s0.Version)
	}

	var s1 C1S1

	err = s1.Read(rw)
	if err != nil {
		return nil, nil, err
	}

	s1Digest, remotePublicKey, err := s1.validate(true)
	if err != nil {
		return nil, nil, err
	}

	var s2 C2S2

	err = s2.Read(rw)
	if err != nil {
		return nil, nil, err
	}

	err = s2.validate(true, c1Digest)
	if err != nil {
		return nil, nil, err
	}

	var c2 C2S2

	err = c2.fill(false, s1Digest, randRead)
	if err != nil {
		return nil, nil, err
	}

	err = c2.Write(rw)
	if err != nil {
		return nil, nil, err
	}

	sharedSecret := dhComputeSharedSecret(localPrivateKey, remotePublicKey)
	keyIn := hmacSha256(sharedSecret, localPublicKey)[:16]
	keyOut := hmacSha256(sharedSecret, remotePublicKey)[:16]
	return keyIn, keyOut, nil
}

func doClientPlain(rw io.ReadWriter, strict bool, randRead func([]byte) (int, error)) error {
	var c0 C0S0

	c0.Version = 3

	err := c0.Write(rw)
	if err != nil {
		return err
	}

	var c1 C1S1

	err = c1.fillPlain(randRead)
	if err != nil {
		return err
	}

	err = c1.Write(rw)
	if err != nil {
		return err
	}

	var s0 C0S0

	err = s0.Read(rw)
	if err != nil {
		return err
	}

	if s0.Version != 3 {
		return fmt.Errorf("server replied with unexpected version %d", s0.Version)
	}

	var s1 C1S1

	err = s1.Read(rw)
	if err != nil {
		return err
	}

	var s2 C2S2

	err = s2.Read(rw)
	if err != nil {
		return err
	}

	if strict && !bytes.Equal(s2.Data, c1.Data) {
		return fmt.Errorf("data in S2 does not correspond")
	}

	var c2 C2S2

	c2.Data = s1.Data

	return c2.Write(rw)
}

// DoClient performs a client-side handshake.
func DoClient(rw io.ReadWriter, encrypted bool, strict bool) ([]byte, []byte, error) {
	return DoClientWithRand(rw, encrypted, strict, rand.Read)
}

// DoClientWithRand performs a client-side handshake, using the given function
// to generate random data.
//
// Two concurrent client handshakes on the same io.ReadWriter are rejected:
// the second call returns an error wrapping ErrHandshakeInProgress instead of
// interleaving writes with the running handshake. Once a call returns
// (success or failure), the connection is released and a further handshake
// starts from clean local state.
func DoClientWithRand(
	rw io.ReadWriter,
	encrypted bool,
	strict bool,
	randRead func([]byte) (int, error),
) ([]byte, []byte, error) {
	release, err := acquireHandshake(rw, clientHandshake)
	if err != nil {
		return nil, nil, err
	}
	defer release()

	if randRead == nil {
		randRead = defaultRandRead
	}

	if encrypted {
		return doClientEncrypted(rw, randRead)
	}
	return nil, nil, doClientPlain(rw, strict, randRead)
}

func doServerEncrypted(rw io.ReadWriter, c1 *C1S1, randRead func([]byte) (int, error)) ([]byte, []byte, error) {
	c1Digest, remotePublicKey, err := c1.validate(false)
	if err != nil {
		return nil, nil, err
	}

	localPrivateKey, localPublicKey, err := dhGenerateKeyPair(randRead)
	if err != nil {
		return nil, nil, err
	}

	var s0 C0S0

	s0.Version = 6

	err = s0.Write(rw)
	if err != nil {
		return nil, nil, err
	}

	var s1 C1S1

	s1.Version = encryptedVersion

	s1Digest, err := s1.fill(true, localPublicKey, randRead)
	if err != nil {
		return nil, nil, err
	}

	err = s1.Write(rw)
	if err != nil {
		return nil, nil, err
	}

	var s2 C2S2

	s2.Time2 = encryptedVersion

	err = s2.fill(true, c1Digest, randRead)
	if err != nil {
		return nil, nil, err
	}

	err = s2.Write(rw)
	if err != nil {
		return nil, nil, err
	}

	var c2 C2S2

	err = c2.Read(rw)
	if err != nil {
		return nil, nil, err
	}

	err = c2.validate(false, s1Digest)
	if err != nil {
		return nil, nil, err
	}

	sharedSecret := dhComputeSharedSecret(localPrivateKey, remotePublicKey)
	keyIn := hmacSha256(sharedSecret, localPublicKey)[:16]
	keyOut := hmacSha256(sharedSecret, remotePublicKey)[:16]
	return keyIn, keyOut, nil
}

func doServerPlain(rw io.ReadWriter, c1 *C1S1, strict bool, randRead func([]byte) (int, error)) error {
	var s0 C0S0

	s0.Version = 3

	err := s0.Write(rw)
	if err != nil {
		return err
	}

	var s1 C1S1

	err = s1.fillPlain(randRead)
	if err != nil {
		return err
	}

	err = s1.Write(rw)
	if err != nil {
		return err
	}

	var s2 C2S2

	s2.Data = c1.Data

	err = s2.Write(rw)
	if err != nil {
		return err
	}

	var c2 C2S2

	err = c2.Read(rw)
	if err != nil {
		return err
	}

	if strict && !bytes.Equal(c2.Data, s1.Data) {
		return fmt.Errorf("data in C2 does not correspond")
	}

	return nil
}

// DoServer performs a server-side handshake.
func DoServer(rw io.ReadWriter, strict bool) ([]byte, []byte, error) {
	return DoServerWithRand(rw, strict, rand.Read)
}

// CheckVersionConsistency verifies that the version declared by C0 (or S0)
// agrees with the contents of C1 (or S1).
//
// Specifically:
//   - C0/S0 version 6 selects the encrypted handshake, so the C1/S1 Version
//     field must be EncryptedVersion.
//   - C0/S0 version 3 selects the plaintext handshake, so C1/S1 must not
//     advertise EncryptedVersion nor carry a valid encrypted digest.
//
// A violation returns an error wrapping ErrVersionMismatch. The check is
// performed independently of digest validation on purpose: a correctly
// digested packet whose version contradicts C0 must still be rejected.
func CheckVersionConsistency(c0 *C0S0, c1 *C1S1) error {
	switch c0.Version {
	case C0VersionEncrypted:
		if c1.Version != EncryptedVersion {
			return &VersionMismatchError{
				C0Version:     c0.Version,
				PacketVersion: c1.Version,
				Reason: fmt.Sprintf(
					"C0 declares encrypted version %d but C1/S1 version is 0x%08x, expected 0x%08x",
					C0VersionEncrypted, c1.Version, EncryptedVersion),
			}
		}

	case C0VersionPlain:
		if c1.Version == EncryptedVersion {
			return &VersionMismatchError{
				C0Version:     c0.Version,
				PacketVersion: c1.Version,
				Reason: fmt.Sprintf(
					"C0 declares plaintext version %d but C1/S1 advertises encrypted version 0x%08x",
					C0VersionPlain, EncryptedVersion),
			}
		}

		// C1.Version alone is not enough to detect every contradictory
		// client: an encrypted C1 produced with another (or zero) version
		// field still carries a valid genuine-client digest. Detect it
		// explicitly instead of silently processing it as random plaintext.
		if _, _, err := c1.validateDigest(false); err == nil {
			return &VersionMismatchError{
				C0Version:     c0.Version,
				PacketVersion: c1.Version,
				Reason:        "C0 declares plaintext version 3 but C1 carries a valid encrypted digest",
			}
		}

	default:
		return fmt.Errorf("%w: c0 version %d", ErrInvalidVersion, c0.Version)
	}

	return nil
}

// DoServerWithRand performs a server-side handshake, using the given function
// to generate random data.
//
// Two concurrent server handshakes on the same io.ReadWriter are rejected
// with an error wrapping ErrHandshakeInProgress. Once a call returns, the
// connection is released and a further handshake starts cleanly.
//
// The client's C0 version and C1 contents are checked for consistency: a
// client that declares one negotiation in C0 but carries a C1 from the other
// negotiation is rejected with an error wrapping ErrVersionMismatch instead
// of being silently handled as plaintext.
func DoServerWithRand(rw io.ReadWriter, strict bool, randRead func([]byte) (int, error)) ([]byte, []byte, error) {
	release, err := acquireHandshake(rw, serverHandshake)
	if err != nil {
		return nil, nil, err
	}
	defer release()

	if randRead == nil {
		randRead = defaultRandRead
	}

	var c0 C0S0

	err = c0.Read(rw)
	if err != nil {
		return nil, nil, err
	}

	// Read C1 before choosing a branch so the C0/C1 version consistency can
	// be verified and both branches can share the same packet.
	var c1 C1S1

	err = c1.Read(rw)
	if err != nil {
		return nil, nil, err
	}

	err = CheckVersionConsistency(&c0, &c1)
	if err != nil {
		return nil, nil, err
	}

	if c0.Version == C0VersionEncrypted {
		return doServerEncrypted(rw, &c1, randRead)
	}

	return nil, nil, doServerPlain(rw, &c1, strict, randRead)
}
