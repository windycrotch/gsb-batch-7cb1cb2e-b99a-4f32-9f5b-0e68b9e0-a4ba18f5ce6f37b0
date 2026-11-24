package rtmp_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"sheepbao.com/media/av"
	"sheepbao.com/media/container/flv"
	"sheepbao.com/media/protocol/hls"
	"sheepbao.com/media/protocol/rtmp"
)

// This external test wires the real RtmpStream and HLS Server together the way
// main.go does: for each publisher, RtmpStream.HandleReader is called and then
// the HLS getter's writer is registered with RtmpStream.HandleWriter. On a
// re-publish the HLS writer is migrated to the new stream generation; it must
// keep receiving the new publisher's packets and produce a fetchable playlist.

// avcSeq is a minimal AVCDecoderConfigurationRecord (SPS+PPS).
var avcSeq = []byte{
	0x01, 0x4d, 0x00, 0x1e, 0xff, 0xe1, 0x00, 0x17, 0x67, 0x4d, 0x00,
	0x1e, 0xab, 0x40, 0x5a, 0x12, 0x6c, 0x09, 0x28, 0x28, 0x28, 0x2f,
	0x80, 0x00, 0x01, 0xf4, 0x00, 0x00, 0x61, 0xa8, 0x4a, 0x01, 0x00,
	0x04, 0x68, 0xde, 0x31, 0x12,
}

// demuxHeader populates p.Header from its FLV tag body, exactly as VirReader
// does for a real publisher. The RTMP cache inspects Header to detect codec
// config and key frames, so a packet without one is treated as unplayable.
func demuxHeader(p av.Packet) av.Packet {
	d := flv.NewDemuxer()
	_ = d.DemuxH(&p)
	return p
}

func vSeq() av.Packet {
	d := append([]byte{0x17, 0x00, 0, 0, 0}, avcSeq...)
	return demuxHeader(av.Packet{IsVideo: true, Data: d})
}
func aSeq() av.Packet {
	return demuxHeader(av.Packet{IsAudio: true, Data: []byte{0xaf, 0x00, 0x12, 0x10}})
}
func vKey(ts uint32) av.Packet {
	return demuxHeader(av.Packet{IsVideo: true, TimeStamp: ts, Data: []byte{0x17, 0x01, 0, 0, 0, 0, 0, 0, 2, 0x65, 0x23}})
}
func vInter(ts uint32) av.Packet {
	return demuxHeader(av.Packet{IsVideo: true, TimeStamp: ts, Data: []byte{0x27, 0x01, 0, 0, 0, 0, 0, 0, 2, 0x41, 0x23}})
}
func aRaw(ts uint32) av.Packet {
	return demuxHeader(av.Packet{IsAudio: true, TimeStamp: ts, Data: []byte{0xaf, 0x01, 0x21, 0x22, 0x23}})
}

// streamReader is a publisher feeding packets to the RTMP stream.
type streamReader struct {
	uid, key string
	ch       chan av.Packet
	closed   chan struct{}
}

func newStreamReader(uid, key string) *streamReader {
	return &streamReader{uid: uid, key: key, ch: make(chan av.Packet, 128), closed: make(chan struct{})}
}
func (r *streamReader) Info() av.Info { return av.Info{UID: r.uid, Key: r.key, Type: "publisher"} }
func (r *streamReader) Alive() bool   { return true }
func (r *streamReader) Read(p *av.Packet) error {
	select {
	case pkt := <-r.ch:
		*p = pkt
		return nil
	case <-r.closed:
		return errClosed
	}
}
func (r *streamReader) Close(error) {
	select {
	case <-r.closed:
	default:
		close(r.closed)
	}
}
func (r *streamReader) send(p av.Packet) {
	select {
	case r.ch <- p:
	case <-r.closed:
	}
}

type errString string

func (e errString) Error() string { return string(e) }

const errClosed = errString("closed")

// publish feeds a full playable chunk (~6s) to r.
func publish(r *streamReader) {
	for _, p := range []av.Packet{vSeq(), aSeq(), vKey(0)} {
		r.send(p)
	}
	for ts := uint32(1000); ts <= 6000; ts += 1000 {
		r.send(vInter(ts))
		r.send(aRaw(ts))
	}
	r.send(vKey(7000))
}

func get(t *testing.T, s *hls.Server, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", path, nil)
	// Drive the exported handler the same way the HTTP mux does.
	s.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// TestHLSSurvivesRTMPTakeover is the end-to-end scenario: HLS subscribes as a
// getter on the RTMP stream; after a re-publish the same HLS writer must still
// receive media and serve a continuous playlist.
func TestHLSSurvivesRTMPTakeover(t *testing.T) {
	rs := rtmp.NewRtmpStream()
	hlsServer := hls.NewServer()

	getter := hlsServer.GetWriter

	// First publisher.
	p1 := newStreamReader("pub-1", "live/cam")
	rs.HandleReader(p1)
	hlsWriter := getter(p1.Info())
	rs.HandleWriter(hlsWriter)
	publish(p1)

	waitStatus(t, hlsServer, "/live/cam.m3u8", http.StatusOK, 5*time.Second)

	code, body := get(t, hlsServer, "/live/cam.m3u8")
	if code != http.StatusOK {
		t.Fatalf("playlist after first publisher: %d\n%s", code, body)
	}
	firstSeg := firstSegment(body)
	if firstSeg == "" {
		t.Fatalf("no segment in first playlist:\n%s", body)
	}
	if code, _ := get(t, hlsServer, firstSeg); code != http.StatusOK {
		t.Fatalf("first segment %s: status %d", firstSeg, code)
	}

	// Re-publish on the same name: new publisher, HLS writer is reused as the
	// getter's writer (like main.go does on a new publisher connection).
	p2 := newStreamReader("pub-2", "live/cam")
	rs.HandleReader(p2)
	hlsWriter2 := getter(p2.Info())
	rs.HandleWriter(hlsWriter2)
	if hlsWriter2 != hlsWriter {
		t.Fatal("HLS getter returned a different writer on re-publish; the RTMP migration would strand it")
	}

	// Right after takeover (before the new session cuts a segment) the old
	// segment must still be fetchable, and the playlist must not 403.
	if code, _ := get(t, hlsServer, firstSeg); code != http.StatusOK {
		t.Fatalf("old segment after takeover: status %d, want 200", code)
	}

	publish(p2)
	waitStatus(t, hlsServer, "/live/cam.m3u8", http.StatusOK, 5*time.Second)

	// The new segment must be fetchable and the old one still present.
	code, body = get(t, hlsServer, "/live/cam.m3u8")
	if code != http.StatusOK {
		t.Fatalf("playlist after takeover: %d", code)
	}
	// The new segment must come from a *later* session generation: OnMigrate
	// advanced the session at handover, so pub-2's media is not stamped with
	// pub-1's generation (which would prove the reset never ran).
	newSeg := ""
	for _, seg := range splitLines(body) {
		if seg == firstSeg || len(seg) < 4 || seg[len(seg)-3:] != ".ts" {
			continue
		}
		newSeg = seg
	}
	if newSeg == "" {
		t.Fatalf("no new segment after takeover:\n%s", body)
	}
	firstGen, ok1 := segSession(firstSeg)
	newGen, ok2 := segSession(newSeg)
	if ok1 && ok2 && newGen <= firstGen {
		t.Fatalf("new segment session %d not newer than first session %d", newGen, firstGen)
	}
	if code, _ := get(t, hlsServer, newSeg); code != http.StatusOK {
		t.Fatalf("new segment %s: status %d", newSeg, code)
	}
	if firstSegment(body) == "" {
		t.Fatalf("playlist after takeover has no segments:\n%s", body)
	}
	if code, _ := get(t, hlsServer, firstSeg); code != http.StatusOK {
		t.Fatalf("old segment dropped after takeover: status %d", code)
	}
}

func firstSegment(body string) string {
	for _, line := range splitLines(body) {
		if len(line) > 3 && line[len(line)-3:] == ".ts" {
			return line
		}
	}
	return ""
}

func splitLines(s string) []string {
	var out []string
	cur := ""
	for _, c := range s {
		if c == '\n' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(c)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func waitStatus(t *testing.T, s *hls.Server, path string, want int, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	var last int
	for time.Now().Before(deadline) {
		last, _ = get(t, s, path)
		if last == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s status %d (last %d)", path, want, last)
}

// segSession extracts the session generation embedded in a segment name of the
// form "/app/stream/<session>_<unix>_<seq>.ts".
func segSession(seg string) (uint64, bool) {
	base := seg
	if i := lastSlash(base); i >= 0 {
		base = base[i+1:]
	}
	us := -1
	for i := 0; i < len(base); i++ {
		if base[i] == '_' {
			us = i
			break
		}
	}
	if us <= 0 {
		return 0, false
	}
	var v uint64
	for _, c := range base[:us] {
		if c < '0' || c > '9' {
			return 0, false
		}
		v = v*10 + uint64(c-'0')
	}
	return v, true
}

func lastSlash(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' {
			return i
		}
	}
	return -1
}
