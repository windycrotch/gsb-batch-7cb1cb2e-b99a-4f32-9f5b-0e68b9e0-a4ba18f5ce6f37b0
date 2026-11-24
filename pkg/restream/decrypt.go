package restream

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	io2 "io"
	"strings"

	"github.com/shaunschembri/restreamer/pkg/restream/provider"
	"github.com/shaunschembri/restreamer/pkg/restream/request"
)

// decrypter is scoped to a single segment. A new instance is created for
// every encrypted segment so that key material and IV state never leak
// across segments.
type decrypter interface {
	// decrypt transforms one chunk of the segment payload. final indicates
	// the last chunk of the segment, which is the only place PKCS#7 padding
	// may be stripped.
	decrypt(payload []byte, final bool) ([]byte, error)
	info() string
}

type aes128 struct {
	iv      string
	keyURL  string
	request request.Request
	mode    cipher.BlockMode
}

func (a aes128) info() string {
	return "AES128"
}

// newDecrypter builds the decrypter for a single segment. Plain segments
// (method NONE or empty) return a nil decrypter and the caller writes the
// payload untouched. Encrypted segments get a fresh AES-128 cipher built
// from the segment's own key URL and IV; a key change therefore can never
// reuse the previous segment's key. Any failure aborts the stream before
// undecrypted data is written.
func newDecrypter(ctx context.Context, segment provider.Segment, userAgent string) (decrypter, error) {
	switch segment.KeyMethod {
	case "", "NONE":
		return nil, nil
	case "AES-128":
	default:
		return nil, fmt.Errorf("key method %s is not supported", segment.KeyMethod)
	}

	a := &aes128{
		iv:      segment.IV,
		keyURL:  segment.KeyURL,
		request: request.New(userAgent),
	}
	if err := a.init(ctx, segment.Seq); err != nil {
		return nil, fmt.Errorf("error initiating decrypter %s: %w", a.info(), err)
	}

	return a, nil
}

func (a *aes128) init(ctx context.Context, mediaSequence uint64) error {
	keyFileResponse, err := a.request.Do(ctx, a.keyURL)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer keyFileResponse.Body.Close()

	key, err := io2.ReadAll(keyFileResponse.Body)
	if err != nil {
		return fmt.Errorf("cannot read key from %s: %w", a.keyURL, err)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return fmt.Errorf("cannot create new AES cipher: %w", err)
	}

	iv, err := resolveIV(a.iv, mediaSequence)
	if err != nil {
		return err
	}

	a.mode = cipher.NewCBCDecrypter(block, iv)

	return nil
}

// resolveIV returns the 16-byte IV for a segment. When the playlist does
// not carry an explicit IV, the media sequence number is used as a
// big-endian 128-bit integer, as required by the HLS specification.
func resolveIV(ivString string, mediaSequence uint64) ([]byte, error) {
	if ivString == "" {
		iv := make([]byte, aes.BlockSize)
		binary.BigEndian.PutUint64(iv[aes.BlockSize-8:], mediaSequence)
		return iv, nil
	}

	trimmed := strings.TrimPrefix(strings.TrimPrefix(ivString, "0x"), "0X")
	iv, err := hex.DecodeString(trimmed)
	if err != nil {
		return nil, fmt.Errorf("cannot decode IV %s: %w", ivString, err)
	}
	if len(iv) != aes.BlockSize {
		return nil, fmt.Errorf("IV %s must be %d bytes, got %d", ivString, aes.BlockSize, len(iv))
	}

	return iv, nil
}

func (a *aes128) decrypt(payload []byte, final bool) ([]byte, error) {
	payloadSize := len(payload)
	if payloadSize%aes.BlockSize != 0 {
		return nil, fmt.Errorf("payload size is not a multiple of %d", aes.BlockSize)
	}

	// Decrypt payload in place. The CBC mode keeps its own chaining state
	// between calls, so multi-chunk segments decrypt as one continuous stream.
	a.mode.CryptBlocks(payload, payload)

	if !final {
		return payload, nil
	}

	// PKCS#7 padding only ever lives in the final block of the whole segment,
	// which may arrive inside a short final read rather than a full buffer.
	return stripPKCS7Padding(payload), nil
}

// stripPKCS7Padding removes PKCS#7 padding when present and returns the
// payload unchanged otherwise.
func stripPKCS7Padding(payload []byte) []byte {
	payloadSize := len(payload)
	if payloadSize == 0 {
		return payload
	}

	// If last byte in payload is bigger then the block size (16) or the last byte
	// is not a multiple of 4, then there is no padding and the entire decrypted payload
	// is returned.
	lastByte := int(payload[payloadSize-1])
	if lastByte > aes.BlockSize || lastByte == 0 || lastByte%4 != 0 || lastByte > payloadSize {
		return payload
	}

	// As per PKCS#7 specification the last byte value will contain the number of bytes
	// that were added as padding and all these extra bytes will have the same value as the
	// last byte.
	for _, paddingByte := range payload[payloadSize-lastByte:] {
		if paddingByte != byte(lastByte) {
			return payload
		}
	}

	return payload[:payloadSize-lastByte]
}
