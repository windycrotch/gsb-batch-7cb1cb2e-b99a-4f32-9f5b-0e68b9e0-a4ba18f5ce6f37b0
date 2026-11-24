package restreamer

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestDownloadWithTimeoutContext exercises the shared start() entry point the
// same way the download command does: a deadline context against a local
// origin that serves a live-style playlist. When the deadline expires the
// call must return without error and stop issuing upstream requests.
func TestDownloadWithTimeoutContext(t *testing.T) {
	var hits int64
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/live.m3u8":
			// Media playlist without ENDLIST so Get keeps reloading.
			fmt.Fprint(w, `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:1
#EXT-X-MEDIA-SEQUENCE:0
#EXTINF:1.0,
seg0.ts
`)
		case "/seg0.ts":
			hits++
			_, _ = w.Write(bytes.Repeat([]byte{0xAA}, 256))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(origin.Close)

	configureStreams(t, map[string]string{"live": origin.URL + "/live.m3u8"})

	var sink bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- start(ctx, &sink, "live") }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("timeout expiry should stop the download cleanly, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("download did not stop when the timeout context expired")
	}

	if hits == 0 {
		t.Fatal("expected at least one segment to be pulled before the deadline")
	}
}

// TestDownloadEncryptedStreamThroughStart verifies the full path used by both
// server and download command correctly decrypts a per-segment keyed stream.
func TestDownloadEncryptedStreamThroughStart(t *testing.T) {
	key := bytes.Repeat([]byte{0xF6}, 16)
	iv := bytes.Repeat([]byte{0x06}, aes.BlockSize)
	plaintext := bytes.Repeat([]byte{0xAB}, 1200)

	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	padded := make([]byte, len(plaintext)+aes.BlockSize-len(plaintext)%aes.BlockSize)
	copy(padded, plaintext)
	for i := len(plaintext); i < len(padded); i++ {
		padded[i] = byte(aes.BlockSize - len(plaintext)%aes.BlockSize)
	}
	encrypted := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(encrypted, padded)

	var originURL string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/enc.m3u8":
			fmt.Fprintf(w, `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:1
#EXT-X-MEDIA-SEQUENCE:0
#EXT-X-KEY:METHOD=AES-128,URI="%s/key.bin",IV=0x06060606060606060606060606060606
#EXTINF:1.0,
seg0.enc
#EXT-X-ENDLIST
`, originURL)
		case "/key.bin":
			_, _ = w.Write(key)
		case "/seg0.enc":
			_, _ = w.Write(encrypted)
		default:
			http.NotFound(w, r)
		}
	}))
	originURL = origin.URL
	t.Cleanup(origin.Close)

	configureStreams(t, map[string]string{"enc": origin.URL + "/enc.m3u8"})

	var sink bytes.Buffer
	if err := start(context.Background(), &sink, "enc"); err != nil {
		t.Fatalf("encrypted download failed: %v", err)
	}
	if !bytes.Equal(sink.Bytes(), plaintext) {
		t.Fatalf("decrypted output mismatch: got %d bytes, want %d", sink.Len(), len(plaintext))
	}
}
