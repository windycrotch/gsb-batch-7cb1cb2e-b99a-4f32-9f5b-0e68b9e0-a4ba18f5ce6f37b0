package restream

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaunschembri/restreamer/pkg/restream/provider"
	"github.com/shaunschembri/restreamer/pkg/restream/provider/hls"
	"github.com/shaunschembri/restreamer/pkg/restream/request"
)

// ---- local test HLS origin -------------------------------------------------

type testOrigin struct {
	server      *httptest.Server
	mu          sync.Mutex
	requestHits map[string]int64
	slowBlock   chan struct{}
}

func newTestOrigin(t *testing.T) *testOrigin {
	t.Helper()

	o := &testOrigin{
		requestHits: make(map[string]int64),
		// Unblocked by default so slow segments return immediately unless a
		// test replaces the gate.
		slowBlock: make(chan struct{}),
	}
	close(o.slowBlock)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		o.mu.Lock()
		o.requestHits[r.URL.Path]++
		o.mu.Unlock()

		// Dedicated failure paths always answer 404 so retries stop
		// immediately (the request layer treats 404 as terminal).
		if strings.HasPrefix(r.URL.Path, "/missing-key/") {
			http.NotFound(w, r)
			return
		}

		if body, ok := originFiles[r.URL.Path]; ok {
			_, _ = w.Write([]byte(body))
			return
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("/slow/", func(w http.ResponseWriter, r *http.Request) {
		o.mu.Lock()
		o.requestHits[r.URL.Path]++
		o.mu.Unlock()

		select {
		case <-o.slowBlock:
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write([]byte(originFiles["/slow-seg0.ts"]))
	})

	o.server = httptest.NewServer(mux)
	t.Cleanup(o.server.Close)
	return o
}

func (o *testOrigin) hits(path string) int64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.requestHits[path]
}

// originFiles is populated per test through the helpers below and served by
// every test origin. Keys are URL paths.
var originFiles = map[string]string{}

func setOriginFile(path, content string) {
	originFiles[path] = content
}

// ---- crypto helpers --------------------------------------------------------

func pkcs7Pad(data []byte, blockSize int) []byte {
	padding := blockSize - len(data)%blockSize
	padText := bytes.Repeat([]byte{byte(padding)}, padding)
	return append(append([]byte{}, data...), padText...)
}

func encryptCBC(t *testing.T, key, iv, plaintext []byte) []byte {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("new cipher: %v", err)
	}
	padded := pkcs7Pad(plaintext, aes.BlockSize)
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, padded)
	return out
}

func segmentPlaintext(id, size int) []byte {
	out := make([]byte, size)
	for i := range out {
		out[i] = byte((id*7 + i) % 256)
	}
	return out
}

func hexIV(b byte) string {
	iv := make([]byte, aes.BlockSize)
	for i := range iv {
		iv[i] = b
	}
	return "0x" + hex.EncodeToString(iv)
}

// ---- writers ---------------------------------------------------------------

type trackingWriter struct {
	mu       sync.Mutex
	buf      bytes.Buffer
	writes   int64
	cancel   context.CancelFunc
	expected int
}

func (w *trackingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.buf.Write(p)
	atomic.AddInt64(&w.writes, 1)
	if w.cancel != nil && w.buf.Len() >= w.expected {
		w.cancel()
	}
	return n, err
}

func (w *trackingWriter) len() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Len()
}

func (w *trackingWriter) bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte{}, w.buf.Bytes()...)
}

func newRestreamer(playlistURL string, writer io.Writer) *Restream {
	req := request.New("test-agent")
	return &Restream{
		Writer:          writer,
		ReadBufferSize:  4096,
		SegmentProvider: hls.NewMedia(req).WithPlaylistURL(playlistURL),
	}
}

// ---- tests -----------------------------------------------------------------

// TestAlternatingPlainAndEncryptedSegments drives a playlist that alternates
// plaintext and AES-128 segments, switches the encryption key mid playlist,
// and returns to plaintext afterwards. The reassembled output must equal the
// concatenation of the original plaintext segments byte for byte.
func TestAlternatingPlainAndEncryptedSegments(t *testing.T) {
	origin := newTestOrigin(t)

	key1 := bytes.Repeat([]byte{0xA1}, 16)
	key2 := bytes.Repeat([]byte{0xB2}, 16)
	setOriginFile("/key1.bin", string(key1))
	setOriginFile("/key2.bin", string(key2))

	plain := [][]byte{
		segmentPlaintext(0, 5000),  // plain, small
		segmentPlaintext(1, 40000), // encrypted with key1, spans multiple decrypt chunks
		segmentPlaintext(2, 7000),  // plain again: previous key must not linger
		segmentPlaintext(3, 41000), // encrypted with key2 (key switch), multi chunk
		segmentPlaintext(4, 3001),  // plain again
		segmentPlaintext(5, 1600),  // encrypted with key1 again
	}

	setOriginFile("/seg0.ts", string(plain[0]))
	setOriginFile("/seg1.enc", string(encryptCBC(t, key1, bytes.Repeat([]byte{0x11}, 16), plain[1])))
	setOriginFile("/seg2.ts", string(plain[2]))
	setOriginFile("/seg3.enc", string(encryptCBC(t, key2, bytes.Repeat([]byte{0x22}, 16), plain[3])))
	setOriginFile("/seg4.ts", string(plain[4]))
	setOriginFile("/seg5.enc", string(encryptCBC(t, key1, bytes.Repeat([]byte{0x33}, 16), plain[5])))

	playlist := fmt.Sprintf(`#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:1
#EXT-X-MEDIA-SEQUENCE:0
#EXTINF:1.0,
seg0.ts
#EXT-X-KEY:METHOD=AES-128,URI="%s/key1.bin",IV=%s
#EXTINF:1.0,
seg1.enc
#EXT-X-KEY:METHOD=NONE
#EXTINF:1.0,
seg2.ts
#EXT-X-KEY:METHOD=AES-128,URI="%s/key2.bin",IV=%s
#EXTINF:1.0,
seg3.enc
#EXT-X-KEY:METHOD=NONE
#EXTINF:1.0,
seg4.ts
#EXT-X-KEY:METHOD=AES-128,URI="%s/key1.bin",IV=%s
#EXTINF:1.0,
seg5.enc
#EXT-X-ENDLIST
`, origin.server.URL, hexIV(0x11), origin.server.URL, hexIV(0x22), origin.server.URL, hexIV(0x33))
	setOriginFile("/mixed.m3u8", playlist)

	var expected bytes.Buffer
	for _, p := range plain {
		expected.Write(p)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	writer := &trackingWriter{cancel: cancel, expected: expected.Len()}
	r := newRestreamer(origin.server.URL+"/mixed.m3u8", writer)

	if err := r.Start(ctx, origin.server.URL+"/mixed.m3u8"); err != nil {
		t.Fatalf("restream failed: %v", err)
	}

	if !bytes.Equal(writer.bytes(), expected.Bytes()) {
		t.Fatalf("reassembled output does not match plaintext (got %d bytes, want %d)", writer.len(), expected.Len())
	}

	// Both keys had to be fetched and the plain segments were served too.
	if origin.hits("/key1.bin") == 0 || origin.hits("/key2.bin") == 0 {
		t.Fatalf("expected both keys to be fetched, got key1=%d key2=%d", origin.hits("/key1.bin"), origin.hits("/key2.bin"))
	}
}

// TestImplicitIVUsesMediaSequence verifies that a key tag without an IV
// attribute falls back to the segment media sequence number as the IV.
func TestImplicitIVUsesMediaSequence(t *testing.T) {
	origin := newTestOrigin(t)

	key := bytes.Repeat([]byte{0xC3}, 16)
	setOriginFile("/implicit-key.bin", string(key))

	// Media sequence starts at 10, so the three segments implicitly use
	// IVs 10, 11 and 12.
	plain := [][]byte{
		segmentPlaintext(10, 2000),
		segmentPlaintext(11, 33000),
		segmentPlaintext(12, 1200),
	}
	for i, p := range plain {
		iv := make([]byte, aes.BlockSize)
		iv[15] = byte(10 + i)
		setOriginFile(fmt.Sprintf("/iseg%d.enc", i), string(encryptCBC(t, key, iv, p)))
	}

	playlist := fmt.Sprintf(`#EXTM3U
#EXT-X-VERSION:2
#EXT-X-TARGETDURATION:1
#EXT-X-MEDIA-SEQUENCE:10
#EXT-X-KEY:METHOD=AES-128,URI="%s/implicit-key.bin"
#EXTINF:1.0,
iseg0.enc
#EXTINF:1.0,
iseg1.enc
#EXTINF:1.0,
iseg2.enc
#EXT-X-ENDLIST
`, origin.server.URL)
	setOriginFile("/implicit.m3u8", playlist)

	var expected bytes.Buffer
	for _, p := range plain {
		expected.Write(p)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	writer := &trackingWriter{cancel: cancel, expected: expected.Len()}
	r := newRestreamer(origin.server.URL+"/implicit.m3u8", writer)

	if err := r.Start(ctx, origin.server.URL+"/implicit.m3u8"); err != nil {
		t.Fatalf("restream failed: %v", err)
	}
	if !bytes.Equal(writer.bytes(), expected.Bytes()) {
		t.Fatalf("implicit-IV decryption mismatch (got %d bytes, want %d)", writer.len(), expected.Len())
	}
}

// TestDecrypterInitFailureAbortsStream ensures the stream stops with an
// error and never writes undecrypted bytes when the key cannot be fetched.
func TestDecrypterInitFailureAbortsStream(t *testing.T) {
	origin := newTestOrigin(t)

	key := bytes.Repeat([]byte{0xD4}, 16)
	plain := segmentPlaintext(0, 4000)
	// Key is served at a path the playlist does not reference: playlist asks
	// for /missing-key/key.bin which always returns 404.
	setOriginFile("/good-key.bin", string(key))
	setOriginFile("/dseg0.enc", string(encryptCBC(t, key, bytes.Repeat([]byte{0x44}, 16), plain)))

	playlist := fmt.Sprintf(`#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:1
#EXT-X-MEDIA-SEQUENCE:0
#EXT-X-KEY:METHOD=AES-128,URI="%s/missing-key/key.bin",IV=%s
#EXTINF:1.0,
dseg0.enc
#EXT-X-ENDLIST
`, origin.server.URL, hexIV(0x44))
	setOriginFile("/badkey.m3u8", playlist)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	writer := &trackingWriter{}
	r := newRestreamer(origin.server.URL+"/badkey.m3u8", writer)

	err := r.Start(ctx, origin.server.URL+"/badkey.m3u8")
	if err == nil {
		t.Fatal("expected an error when decrypter initialization fails, got nil")
	}
	if writer.len() != 0 {
		t.Fatalf("undecrypted data was written after key init failure: %d bytes", writer.len())
	}
}

// TestCancellationStopsPullingAndWriting verifies that after context
// cancellation no new segment requests are issued, in-flight writing returns
// promptly and no further bytes reach the writer.
func TestCancellationStopsPullingAndWriting(t *testing.T) {
	origin := newTestOrigin(t)

	gate := make(chan struct{})
	origin.slowBlock = gate

	plain0 := segmentPlaintext(0, 1000)
	plain1 := segmentPlaintext(1, 1000)
	setOriginFile("/slow-seg0.ts", string(plain0))
	setOriginFile("/slow-seg1.ts", string(plain1))

	playlist := `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:1
#EXT-X-MEDIA-SEQUENCE:0
#EXTINF:1.0,
slow/slow-seg0.ts
#EXTINF:1.0,
slow/slow-seg1.ts
#EXT-X-ENDLIST
`
	setOriginFile("/slow.m3u8", playlist)

	ctx, cancel := context.WithCancel(context.Background())
	writer := &trackingWriter{}
	r := newRestreamer(origin.server.URL+"/slow.m3u8", writer)

	done := make(chan error, 1)
	go func() { done <- r.Start(ctx, origin.server.URL+"/slow.m3u8") }()

	// Wait until the first segment request is in flight, then cancel while
	// the origin is still holding the response open.
	deadline := time.Now().Add(5 * time.Second)
	for origin.hits("/slow/slow-seg0.ts") == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if origin.hits("/slow/slow-seg0.ts") == 0 {
		t.Fatal("segment request never reached the origin")
	}

	hitsBefore := origin.hits("/slow/slow-seg1.ts")
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start should return cleanly on cancellation, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after context cancellation")
	}

	// Give any (buggy) late requests a chance to land, then assert the origin
	// saw no additional segment fetches and the writer received nothing more.
	time.Sleep(200 * time.Millisecond)
	if origin.hits("/slow/slow-seg1.ts") != hitsBefore {
		t.Fatalf("new segment request issued after cancellation: before=%d after=%d",
			hitsBefore, origin.hits("/slow/slow-seg1.ts"))
	}

	writtenAfterCancel := writer.len()
	time.Sleep(100 * time.Millisecond)
	if writer.len() != writtenAfterCancel {
		t.Fatal("writer kept receiving bytes after cancellation")
	}

	close(gate) // release any held connection; cleanup must not hang
}

// TestConcurrentRequestsAreIndependent runs several restreamers against the
// same stream id at once. Each must reassemble the full plaintext into its
// own writer without cross-talk, and cancelling one request must not affect
// the others.
func TestConcurrentRequestsAreIndependent(t *testing.T) {
	origin := newTestOrigin(t)

	key := bytes.Repeat([]byte{0xE5}, 16)
	setOriginFile("/conc-key.bin", string(key))

	plain := [][]byte{
		segmentPlaintext(0, 6000),
		segmentPlaintext(1, 35000),
		segmentPlaintext(2, 4000),
	}
	setOriginFile("/cseg0.ts", string(plain[0]))
	setOriginFile("/cseg1.enc", string(encryptCBC(t, key, bytes.Repeat([]byte{0x55}, 16), plain[1])))
	setOriginFile("/cseg2.ts", string(plain[2]))

	playlist := fmt.Sprintf(`#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:1
#EXT-X-MEDIA-SEQUENCE:0
#EXTINF:1.0,
cseg0.ts
#EXT-X-KEY:METHOD=AES-128,URI="%s/conc-key.bin",IV=%s
#EXTINF:1.0,
cseg1.enc
#EXT-X-KEY:METHOD=NONE
#EXTINF:1.0,
cseg2.ts
#EXT-X-ENDLIST
`, origin.server.URL, hexIV(0x55))
	playlistURL := origin.server.URL + "/conc.m3u8"
	setOriginFile("/conc.m3u8", playlist)

	var expected bytes.Buffer
	for _, p := range plain {
		expected.Write(p)
	}

	const clients = 4
	var wg sync.WaitGroup
	errs := make([]error, clients)
	writers := make([]*trackingWriter, clients)
	cancels := make([]context.CancelFunc, clients)
	rootCtx, rootCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer rootCancel()

	for i := 0; i < clients; i++ {
		// Client 0 is cancelled early to prove it does not disturb the rest.
		ctx, cancel := context.WithCancel(rootCtx)
		cancels[i] = cancel
		if i == 0 {
			writers[i] = &trackingWriter{}
		} else {
			writers[i] = &trackingWriter{cancel: cancel, expected: expected.Len()}
		}

		wg.Add(1)
		go func(idx int, c context.Context, w *trackingWriter) {
			defer wg.Done()
			r := newRestreamer(playlistURL, w)
			errs[idx] = r.Start(c, playlistURL)
		}(i, ctx, writers[i])
	}

	// Cancel the first client once it has streamed at least the first segment.
	deadline := time.Now().Add(5 * time.Second)
	for writers[0].len() < len(plain[0]) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancels[0]()

	wg.Wait()

	if errs[0] != nil {
		t.Fatalf("cancelled client returned error, expected clean stop: %v", errs[0])
	}
	if writers[0].len() < len(plain[0]) {
		t.Fatalf("cancelled client produced no data: %d bytes", writers[0].len())
	}
	for i := 1; i < clients; i++ {
		if errs[i] != nil {
			t.Fatalf("client %d failed: %v", i, errs[i])
		}
		if !bytes.Equal(writers[i].bytes(), expected.Bytes()) {
			t.Fatalf("client %d output corrupted or incomplete: got %d want %d",
				i, writers[i].len(), expected.Len())
		}
	}
}

// TestTimeoutContextStopsDownload mirrors the download command: a deadline
// context must end the stream cleanly and freeze the output at deadline.
func TestTimeoutContextStopsDownload(t *testing.T) {
	origin := newTestOrigin(t)

	gate := make(chan struct{})
	origin.slowBlock = gate

	plain := segmentPlaintext(0, 1000)
	setOriginFile("/t-seg0.ts", string(plain))
	playlist := `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:1
#EXT-X-MEDIA-SEQUENCE:0
#EXTINF:1.0,
slow/t-seg0.ts
#EXT-X-ENDLIST
`
	setOriginFile("/timeout.m3u8", playlist)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	writer := &trackingWriter{}
	r := newRestreamer(origin.server.URL+"/timeout.m3u8", writer)

	done := make(chan error, 1)
	go func() { done <- r.Start(ctx, origin.server.URL+"/timeout.m3u8") }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("deadline expiry should stop cleanly, got error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not honour the timeout context")
	}

	frozen := writer.len()
	time.Sleep(100 * time.Millisecond)
	if writer.len() != frozen {
		t.Fatal("output kept growing after the download deadline expired")
	}
	close(gate)
}

// ---- unit tests ------------------------------------------------------------

func TestResolveIV(t *testing.T) {
	seqIV := func(seq uint64) []byte {
		iv := make([]byte, aes.BlockSize)
		iv[15] = byte(seq)
		return iv
	}

	cases := []struct {
		name    string
		iv      string
		seq     uint64
		want    []byte
		wantErr bool
	}{
		{name: "empty falls back to sequence", iv: "", seq: 7, want: seqIV(7)},
		{name: "explicit 0x hex", iv: hexIV(0x66), want: bytes.Repeat([]byte{0x66}, 16)},
		{name: "explicit hex without prefix", iv: hex.EncodeToString(bytes.Repeat([]byte{0x77}, 16)), want: bytes.Repeat([]byte{0x77}, 16)},
		{name: "invalid hex", iv: "0xnothex", wantErr: true},
		{name: "wrong length", iv: "0x0102", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveIV(tc.iv, tc.seq)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("IV mismatch: got %x want %x", got, tc.want)
			}
		})
	}
}

func TestNewDecrypterRejectsUnsupportedMethod(t *testing.T) {
	if _, err := newDecrypter(context.Background(),
		provider.Segment{KeyMethod: "SAMPLE-AES"}, "agent"); err == nil {
		t.Fatal("expected unsupported key method error")
	}

	d, err := newDecrypter(context.Background(),
		provider.Segment{KeyMethod: "NONE"}, "agent")
	if err != nil {
		t.Fatalf("plain segment decrypter: %v", err)
	}
	if d != nil {
		t.Fatal("plain segments must produce a nil decrypter")
	}
}

func TestStreamWriterRefusesCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var sink bytes.Buffer
	w := NewStreamWriter(ctx, &sink)

	if _, err := w.Write([]byte("before")); err != nil {
		t.Fatalf("write before cancel: %v", err)
	}
	cancel()
	if _, err := w.Write([]byte("after")); err == nil {
		t.Fatal("write after cancellation must fail")
	}
	if string(sink.Bytes()) != "before" {
		t.Fatalf("unexpected sink content: %q", sink.Bytes())
	}
}

func TestMain(m *testing.M) {
	code := m.Run()
	os.Exit(code)
}
