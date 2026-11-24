package hls

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"sheepbao.com/media/av"
)

// ---- helpers ------------------------------------------------------------

var hlsTestKey = "live/cam"

func getPlaylist(s *Server, key string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.handle(rec, httptest.NewRequest("GET", "/"+key+".m3u8", nil))
	return rec
}

func getSegment(s *Server, name string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.handle(rec, httptest.NewRequest("GET", name, nil))
	return rec
}

// addSegment publishes a segment for key as if the given session produced it.
func addSegment(s *Server, key string, session uint64, seq int, name string) {
	s.mu.Lock()
	c := s.caches[key]
	s.mu.Unlock()
	item := NewTSItem(name, 3000, seq, []byte{0x47, 0x00, 0x01})
	item.Session = session
	c.SetItem(name, item)
}

// newSession subscribes a publisher via GetWriter and, to model the RTMP
// handover that actually advances the session, calls OnMigrate for any
// subsequent publisher. Returns the Source and its current session generation.
func newSession(s *Server, uid string) (*Source, uint64) {
	w := s.GetWriter(av.Info{UID: uid, Key: hlsTestKey, Type: "publisher"})
	src := w.(*Source)
	return src, src.session
}

// takeover models the RTMP re-publish path: the existing writer is reset at the
// handover, then GetWriter is called for the new publisher.
func takeover(s *Server, uid string) (*Source, uint64) {
	if v, ok := s.conns.Get(hlsTestKey); ok {
		v.(*Source).OnMigrate()
	}
	return newSession(s, uid)
}

// ---- tests --------------------------------------------------------------

// TestHLSPlaylistNotReadyBeforeKeyFrame: a fresh session with no segment yet
// must answer 503, not an empty or previous-session playlist.
func TestHLSPlaylistNotReadyBeforeKeyFrame(t *testing.T) {
	s := NewServer()
	newSession(s, "pub-1")

	rec := getPlaylist(s, hlsTestKey)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (%s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "#EXTM3U") {
		t.Fatal("served a playlist while no segment was available")
	}
}

// TestHLSTakeoverKeepsOldSegmentsServable: after a re-publish, a segment from
// the previous session must still be fetchable (no 403) and old segments must
// not be wiped.
func TestHLSTakeoverKeepsOldSegmentsServable(t *testing.T) {
	s := NewServer()
	_, gen1 := newSession(s, "pub-1")
	oldSeg := "/live/cam/old.ts"
	addSegment(s, hlsTestKey, gen1, 1, oldSeg)

	// Playlist should now be generated normally.
	rec := getPlaylist(s, hlsTestKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("playlist status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), oldSeg) {
		t.Fatalf("playlist missing old segment:\n%s", rec.Body.String())
	}

	// New publisher takes over the same name.
	_, gen2 := takeover(s, "pub-2")
	if gen2 <= gen1 {
		t.Fatalf("session generation did not advance: %d -> %d", gen1, gen2)
	}

	// Old segment must still be servable.
	seg := getSegment(s, oldSeg)
	if seg.Code != http.StatusOK {
		t.Fatalf("old segment after takeover: status = %d, want 200", seg.Code)
	}

	// But the playlist must not advertise the previous session as current
	// before the new session produces a segment.
	rec = getPlaylist(s, hlsTestKey)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("playlist status after takeover = %d, want 503", rec.Code)
	}
}

// TestHLSPlaylistContinuesAfterTakeover: once the new session produces a
// segment, the playlist is served again and its media sequence continues
// across the session boundary.
func TestHLSPlaylistContinuesAfterTakeover(t *testing.T) {
	s := NewServer()
	_, gen1 := newSession(s, "pub-1")
	addSegment(s, hlsTestKey, gen1, 1, "/live/cam/a.ts")

	rec := getPlaylist(s, hlsTestKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("pre-takeover playlist status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "#EXT-X-MEDIA-SEQUENCE:1") {
		t.Fatalf("unexpected pre-takeover playlist:\n%s", rec.Body.String())
	}

	_, gen2 := takeover(s, "pub-2")
	// New session's first segment gets the next sequence number.
	c := s.getCache(hlsTestKey)
	next := c.NextSeq()
	addSegment(s, hlsTestKey, gen2, next, "/live/cam/b.ts")

	rec = getPlaylist(s, hlsTestKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("post-takeover playlist status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "/live/cam/b.ts") {
		t.Fatalf("playlist missing new segment:\n%s", body)
	}
	if !strings.Contains(body, "#EXT-X-MEDIA-SEQUENCE:1") {
		t.Fatalf("media sequence did not continue:\n%s", body)
	}
	// Session boundary should be marked.
	if !strings.Contains(body, "#EXT-X-DISCONTINUITY") {
		t.Fatalf("missing discontinuity marker at session boundary:\n%s", body)
	}
	// Both sessions' segments are present.
	if !strings.Contains(body, "/live/cam/a.ts") {
		t.Fatalf("playlist dropped the previous session's segment:\n%s", body)
	}
}

// TestHLSOldSessionCleanupDoesNotDropNewCache: the cache used by a session is
// shared across takeovers, so a retired session cannot drop or reset segments
// the new session still needs.
func TestHLSOldSessionCleanupDoesNotDropNewCache(t *testing.T) {
	s := NewServer()
	_, gen1 := newSession(s, "pub-1")
	addSegment(s, hlsTestKey, gen1, 1, "/live/cam/a.ts")

	newSrc, gen2 := takeover(s, "pub-2")
	// The Source object is intentionally reused across takeovers; the cache is
	// shared, so segments published by session 1 are still present.
	c := s.getCache(hlsTestKey)
	if c == nil {
		t.Fatal("cache removed after takeover")
	}
	if _, err := c.GetItem("/live/cam/a.ts"); err != nil {
		t.Fatalf("old segment dropped from cache: %v", err)
	}
	if c != newSrc.GetCacheInc() {
		t.Fatal("new session does not use the preserved cache")
	}

	// A late segment from the retired session must not resurrect it as ready.
	addSegment(s, hlsTestKey, gen1, 2, "/live/cam/stale.ts")
	rec := getPlaylist(s, hlsTestKey)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("stale session segment made playlist ready: %d\n%s", rec.Code, rec.Body.String())
	}

	// A segment from the current session makes it ready.
	addSegment(s, hlsTestKey, gen2, c.NextSeq(), "/live/cam/new.ts")
	rec = getPlaylist(s, hlsTestKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("current session segment did not make playlist ready: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "/live/cam/new.ts") {
		t.Fatalf("playlist missing new session segment:\n%s", rec.Body.String())
	}
}

// TestHLSConcurrentRequestsDuringTakeover hits the server from many goroutines
// while sessions take over; every response must be a sane status code and the
// cache must keep serving old segments.
func TestHLSConcurrentRequestsDuringTakeover(t *testing.T) {
	s := NewServer()
	_, gen1 := newSession(s, "pub-1")
	addSegment(s, hlsTestKey, gen1, 1, "/live/cam/a.ts")

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Writers: repeated takeovers.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			src, gen := takeover(s, "pub-"+string(rune('a'+i)))
			addSegment(s, hlsTestKey, gen, s.getCache(hlsTestKey).NextSeq(), "/live/cam/s.ts")
			_ = src
		}
		close(stop)
	}()

	// Readers: playlists and segments.
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				rec := getPlaylist(s, hlsTestKey)
				if rec.Code != http.StatusOK && rec.Code != http.StatusServiceUnavailable {
					t.Errorf("playlist during takeover: unexpected status %d", rec.Code)
					return
				}
				// The stream name always has a cache during/after takeover, so a
				// segment request is never 403. It may legitimately be 404 once
				// the 3-segment window has rolled past it.
				seg := getSegment(s, "/live/cam/s.ts")
				if seg.Code != http.StatusOK && seg.Code != http.StatusNotFound {
					t.Errorf("segment during takeover: unexpected status %d", seg.Code)
					return
				}
			}
		}()
	}
	wg.Wait()

	// No panic / data race and final state is coherent: the last session is
	// current and owns the shared cache.
	cur := s.getConn(hlsTestKey)
	if cur == nil {
		t.Fatal("no current source after concurrent takeovers")
	}
	if !strings.Contains(getPlaylist(s, hlsTestKey).Body.String(), "#EXTM3U") {
		t.Fatal("playlist not servable after concurrent takeovers")
	}
}

// TestHLSTsCacheWindowBounds keeps the sliding window bounded while keeping
// sequence numbers monotonic.
func TestHLSTsCacheWindowBounds(t *testing.T) {
	c := NewTSCacheItem("live/cam")
	if gen := c.StartSession(); gen != 1 {
		t.Fatalf("first session generation = %d, want 1", gen)
	}
	for i := 1; i <= 10; i++ {
		name := "/live/cam/" + string(rune('a'+i)) + ".ts"
		item := NewTSItem(name, 3000, i, []byte{0x47})
		item.Session = 1
		c.SetItem(name, item)
	}
	if n := c.Len(); n != maxTSCacheNum {
		t.Fatalf("cache window = %d, want %d", n, maxTSCacheNum)
	}
	if seq := c.NextSeq(); seq != 11 {
		t.Fatalf("NextSeq = %d, want 11", seq)
	}
	// Evicted segments are gone, recent ones remain.
	if _, err := c.GetItem("/live/cam/a.ts"); err == nil {
		t.Fatal("evicted segment still present")
	}
	if _, err := c.GetItem("/live/cam/k.ts"); err != nil {
		t.Fatalf("recent segment missing: %v", err)
	}
}

// TestHLSSourceWriteAfterCloseIsRejected guards the session lifecycle.
func TestHLSSourceWriteAfterCloseIsRejected(t *testing.T) {
	s := NewServer()
	src, _ := newSession(s, "pub-1")
	src.Close(nil)
	if err := src.Write(av.Packet{IsAudio: true}); err == nil {
		t.Fatal("Write after Close should fail")
	}
	// Close is idempotent.
	src.Close(nil)
	src.Close(nil)
}

// TestHLSUnknownKeyIsForbidden keeps the existing 403 contract for unknown
// stream names.
func TestHLSUnknownKeyIsForbidden(t *testing.T) {
	s := NewServer()
	if rec := getPlaylist(s, "nobody/here"); rec.Code != http.StatusForbidden {
		t.Fatalf("unknown key playlist status = %d, want 403", rec.Code)
	}
	if rec := getSegment(s, "/nobody/here/x.ts"); rec.Code != http.StatusForbidden {
		t.Fatalf("unknown key segment status = %d, want 403", rec.Code)
	}
}

// TestHLSTakeoverIsAtomic checks that GetWriter never exposes a half-swapped
// state: the map always points at a live source for the key.
func TestHLSTakeoverIsAtomic(t *testing.T) {
	s := NewServer()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s.GetWriter(av.Info{UID: "pub", Key: hlsTestKey})
			time.Sleep(time.Millisecond)
		}(i)
	}
	wg.Wait()
	cur := s.getConn(hlsTestKey)
	if cur == nil || cur.isClosed() {
		t.Fatal("map points at a closed/absent source after concurrent GetWriter")
	}
}

// ---- end-to-end muxing integration ---------------------------------------

// avcSeq is a minimal but valid AVCDecoderConfigurationRecord (SPS+PPS).
var avcSeq = []byte{
	0x01, 0x4d, 0x00, 0x1e, 0xff, 0xe1, 0x00, 0x17, 0x67, 0x4d, 0x00,
	0x1e, 0xab, 0x40, 0x5a, 0x12, 0x6c, 0x09, 0x28, 0x28, 0x28, 0x2f,
	0x80, 0x00, 0x01, 0xf4, 0x00, 0x00, 0x61, 0xa8, 0x4a, 0x01, 0x00,
	0x04, 0x68, 0xde, 0x31, 0x12,
}

func flvVideoSeq() av.Packet {
	d := append([]byte{0x17, 0x00, 0, 0, 0}, avcSeq...)
	return av.Packet{IsVideo: true, Data: d}
}

func flvVideoKey(ts uint32) av.Packet {
	// AVC key frame, one length-prefixed IDR NALU.
	d := []byte{0x17, 0x01, 0, 0, 0, 0, 0, 0, 2, 0x65, 0x23}
	return av.Packet{IsVideo: true, TimeStamp: ts, Data: d}
}

func flvVideoInter(ts uint32) av.Packet {
	d := []byte{0x27, 0x01, 0, 0, 0, 0, 0, 0, 2, 0x41, 0x23}
	return av.Packet{IsVideo: true, TimeStamp: ts, Data: d}
}

func flvAudioSeq() av.Packet {
	return av.Packet{IsAudio: true, Data: []byte{0xaf, 0x00, 0x12, 0x10}}
}

func flvAudioRaw(ts uint32) av.Packet {
	return av.Packet{IsAudio: true, TimeStamp: ts, Data: []byte{0xaf, 0x01, 0x21, 0x22, 0x23}}
}

// feedPlayable writes a codec config + key frame + enough media to force at
// least one segment cut for the given source.
func feedPlayable(t *testing.T, src *Source) {
	t.Helper()
	ms := uint32(0)
	write := func(p av.Packet) {
		if err := src.Write(p); err != nil {
			t.Fatalf("source write: %v", err)
		}
	}
	write(flvVideoSeq())
	write(flvAudioSeq())
	write(flvVideoKey(ms))
	// A segment is cut when a key frame arrives and the segment has already
	// accumulated >= 3s of media, so feed ~6s of frames with a key at the end.
	for ms = 1000; ms <= 6000; ms += 1000 {
		write(flvVideoInter(ms))
		write(flvAudioRaw(ms))
	}
	write(flvVideoKey(ms))
	write(flvVideoInter(ms + 1000))
}

func playlistSegments(body string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasSuffix(line, ".ts") {
			out = append(out, line)
		}
	}
	return out
}

// TestHLSMuxingSurvivesTakeover drives the real FLV demuxer + TS muxer through a
// publisher takeover: the first session must produce a fetchable segment, the
// second must keep the cache continuous, and old segments must stay servable.
func TestHLSMuxingSurvivesTakeover(t *testing.T) {
	s := NewServer()
	src1 := s.GetWriter(av.Info{UID: "pub-1", Key: hlsTestKey}).(*Source)
	gen1 := src1.session

	feedPlayable(t, src1)

	waitFor(t, 3*time.Second, "first segment cut", func() bool {
		return s.getCache(hlsTestKey).Len() >= 1
	})

	rec := getPlaylist(s, hlsTestKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("playlist after first segment: %d\n%s", rec.Code, rec.Body.String())
	}
	segs := playlistSegments(rec.Body.String())
	if len(segs) == 0 {
		t.Fatalf("playlist has no segments:\n%s", rec.Body.String())
	}
	first := segs[0]
	if got := getSegment(s, first); got.Code != http.StatusOK {
		t.Fatalf("first segment %s: status %d", first, got.Code)
	}
	// Segment names embed the producing session: "/key/<session>_<unix>_<seq>.ts".
	if !strings.Contains(first, "/"+hlsTestKey+"/"+itoa64(gen1)+"_") {
		t.Logf("segment name %q does not embed session %d (informational)", first, gen1)
	}

	// Takeover by a second publisher. In production the RTMP stream calls
	// OnMigrate on the reused writer at the handover point; then GetWriter is
	// asked for the new publisher's writer. Model exactly that order.
	src1.OnMigrate()
	src2 := s.GetWriter(av.Info{UID: "pub-2", Key: hlsTestKey}).(*Source)
	if src2 != src1 {
		t.Fatal("GetWriter created a new Source; migrated writer would be stranded")
	}
	if src2.session <= gen1 {
		t.Fatalf("session did not advance: %d -> %d", gen1, src2.session)
	}

	// Old segment must still be servable after the takeover (no 403).
	if got := getSegment(s, first); got.Code != http.StatusOK {
		t.Fatalf("old segment after takeover: status %d, want 200", got.Code)
	}

	// Before the new session cuts a segment the playlist is explicitly 503.
	if rec := getPlaylist(s, hlsTestKey); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("playlist before new segment: status %d, want 503\n%s", rec.Code, rec.Body.String())
	}

	feedPlayable(t, src2)
	waitFor(t, 3*time.Second, "second session segment", func() bool {
		rec := getPlaylist(s, hlsTestKey)
		return rec.Code == http.StatusOK && len(playlistSegments(rec.Body.String())) >= 1
	})

	rec = getPlaylist(s, hlsTestKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("playlist after second segment: %d", rec.Code)
	}
	body := rec.Body.String()
	var newSeg string
	for _, seg := range playlistSegments(body) {
		if strings.Contains(seg, "/"+hlsTestKey+"/"+itoa64(src2.session)+"_") {
			newSeg = seg
		}
	}
	if newSeg == "" {
		t.Fatalf("playlist missing second-session segment:\n%s", body)
	}
	if got := getSegment(s, newSeg); got.Code != http.StatusOK {
		t.Fatalf("new segment %s: status %d", newSeg, got.Code)
	}
	// Both sessions' media are present and the discontinuity is marked.
	if !strings.Contains(body, "#EXT-X-DISCONTINUITY") {
		t.Fatalf("no discontinuity marker across sessions:\n%s", body)
	}
	if !strings.Contains(body, first) {
		t.Fatalf("previous session segment %s dropped from playlist:\n%s", first, body)
	}
}

func itoa64(v uint64) string {
	if v == 0 {
		return "0"
	}
	var b [24]byte
	n := len(b)
	for v > 0 {
		n--
		b[n] = byte('0' + v%10)
		v /= 10
	}
	return string(b[n:])
}

func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

// TestHLSRepublishAfterSourceClosed: if the liveness sweep fully closed and
// removed a Source, the next publisher must get a working Source (a fresh one),
// not the dead object, and the per-name cache must be preserved.
func TestHLSRepublishAfterSourceClosed(t *testing.T) {
	s := NewServer()
	src1, gen1 := newSession(s, "pub-1")
	addSegment(s, hlsTestKey, gen1, 1, "/live/cam/a.ts")

	// Simulate the liveness sweep: remove and fully close the Source.
	s.mu.Lock()
	if s.conns.RemoveIf(hlsTestKey, src1) {
		src1.Close(nil)
	}
	s.mu.Unlock()
	if !src1.isClosed() {
		t.Fatal("source not closed by sweep")
	}

	src2, gen2 := takeover(s, "pub-2")
	if src2 == src1 {
		t.Fatal("GetWriter returned the closed source")
	}
	if gen2 <= gen1 {
		t.Fatalf("generation did not advance: %d -> %d", gen1, gen2)
	}
	// The cache survives the Source being dropped.
	c := s.getCache(hlsTestKey)
	if c == nil {
		t.Fatal("cache dropped with the source")
	}
	if _, err := c.GetItem("/live/cam/a.ts"); err != nil {
		t.Fatalf("old segment lost after source drop: %v", err)
	}
	// And the new Source writes muxable packets without panicking.
	feedPlayable(t, src2)
	waitFor(t, 3*time.Second, "new source produces a segment", func() bool {
		rec := getPlaylist(s, hlsTestKey)
		return rec.Code == http.StatusOK && len(playlistSegments(rec.Body.String())) >= 1
	})
}
