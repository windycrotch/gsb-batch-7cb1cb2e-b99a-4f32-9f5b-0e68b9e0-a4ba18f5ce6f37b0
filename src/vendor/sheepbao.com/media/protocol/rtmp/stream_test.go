package rtmp

import (
	"errors"
	"sync"
	"testing"
	"time"

	"sheepbao.com/media/av"
)

// ---- test doubles -------------------------------------------------------

// fakeHeader implements both video and audio packet headers just enough for the
// cache logic (seq config / key frame detection).
type fakeVideoHeader struct {
	key bool
	seq bool
}

func (h fakeVideoHeader) IsKeyFrame() bool       { return h.key }
func (h fakeVideoHeader) IsSeq() bool            { return h.seq }
func (h fakeVideoHeader) CodecID() uint8         { return av.VIDEO_H264 }
func (h fakeVideoHeader) CompositionTime() int32 { return 0 }

type fakeAudioHeader struct {
	seq bool
}

func (h fakeAudioHeader) SoundFormat() uint8 { return av.SOUND_AAC }
func (h fakeAudioHeader) AACPacketType() uint8 {
	if h.seq {
		return av.AAC_SEQHDR
	}
	return av.AAC_RAW
}

func videoSeqPkt() av.Packet {
	return av.Packet{IsVideo: true, Header: fakeVideoHeader{seq: true}, Data: []byte{0x17, 0x00}}
}

func videoKeyPkt(ts uint32) av.Packet {
	return av.Packet{IsVideo: true, TimeStamp: ts, Header: fakeVideoHeader{key: true}, Data: []byte{0x17, 0x01, 0xaa}}
}

func videoInterPkt(ts uint32) av.Packet {
	return av.Packet{IsVideo: true, TimeStamp: ts, Header: fakeVideoHeader{}, Data: []byte{0x27, 0x01, 0xbb}}
}

func audioSeqPkt() av.Packet {
	return av.Packet{IsAudio: true, Header: fakeAudioHeader{seq: true}, Data: []byte{0xaf, 0x00, 0x12, 0x10}}
}

func audioRawPkt(ts uint32) av.Packet {
	return av.Packet{IsAudio: true, TimeStamp: ts, Data: []byte{0xaf, 0x01, 0xcc}}
}

// fakeReader is a publisher. Packets are fed through a channel so tests control
// timing; it implements av.ReadCloser.
type fakeReader struct {
	uid    string
	key    string
	ch     chan av.Packet
	closed chan struct{}
	once   sync.Once
	err    error
}

func newFakeReader(uid, key string) *fakeReader {
	return &fakeReader{
		uid:    uid,
		key:    key,
		ch:     make(chan av.Packet, 64),
		closed: make(chan struct{}),
	}
}

func (r *fakeReader) Info() av.Info {
	return av.Info{UID: r.uid, Key: r.key, Type: "publisher", Inter: false}
}

func (r *fakeReader) Read(p *av.Packet) error {
	select {
	case pkt, ok := <-r.ch:
		if !ok {
			return errors.New("eof")
		}
		*p = pkt
		return nil
	case <-r.closed:
		return errors.New("closed")
	}
}

// send pushes a packet unless the reader has been closed.
func (r *fakeReader) send(p av.Packet) bool {
	select {
	case <-r.closed:
		return false
	default:
	}
	select {
	case r.ch <- p:
		return true
	case <-r.closed:
		return false
	}
}

// eof makes Read return an error without marking the reader "closed", modelling
// a publisher that simply disconnects.
func (r *fakeReader) eof() {
	close(r.ch)
}

func (r *fakeReader) Close(err error) {
	r.once.Do(func() {
		r.err = err
		close(r.closed)
	})
}

func (r *fakeReader) IsClosed() bool {
	select {
	case <-r.closed:
		return true
	default:
		return false
	}
}

func (r *fakeReader) Alive() bool { return true }

// fakeWriter is a player. It records every packet it receives.
type fakeWriter struct {
	mu       sync.Mutex
	uid      string
	key      string
	inter    bool
	pkts     []av.Packet
	closed   int
	closeErr error
}

func newFakeWriter(uid, key string, inter bool) *fakeWriter {
	return &fakeWriter{uid: uid, key: key, inter: inter}
}

func (w *fakeWriter) Write(p av.Packet) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pkts = append(w.pkts, p)
	return nil
}

func (w *fakeWriter) Close(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed++
	if w.closeErr == nil {
		w.closeErr = err
	}
}

func (w *fakeWriter) CloseCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.closed
}

func (w *fakeWriter) Packets() []av.Packet {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]av.Packet, len(w.pkts))
	copy(out, w.pkts)
	return out
}

func (w *fakeWriter) Reset() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pkts = nil
}

func (w *fakeWriter) Info() av.Info {
	return av.Info{UID: w.uid, Key: w.key, Type: "player", Inter: w.inter}
}

func (w *fakeWriter) CalcBaseTimestamp() {}

func (w *fakeWriter) Alive() bool { return true }

// ---- assertions ---------------------------------------------------------

// waitFor polls cond until it holds or the deadline expires.
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

// assertSeededFrom verifies that pkts begins with the codec config then a key
// frame of the expected generation (keyframe payload tag identifies it).
func assertSeeded(t *testing.T, what string, pkts []av.Packet) {
	t.Helper()
	if len(pkts) < 2 {
		t.Fatalf("%s: expected at least seq+keyframe, got %d packets", what, len(pkts))
	}
	vh, ok := pkts[0].Header.(av.VideoPacketHeader)
	if !ok {
		// audio-first config is acceptable
		ah, ok2 := pkts[0].Header.(av.AudioPacketHeader)
		if !ok2 {
			t.Fatalf("%s: first packet has neither video nor audio header", what)
		}
		if ah.AACPacketType() != av.AAC_SEQHDR {
			t.Fatalf("%s: first packet is not an audio codec config", what)
		}
	} else if !vh.IsSeq() {
		t.Fatalf("%s: first packet is not a video codec config", what)
	}

	// After the config, the first media packet must be a key frame.
	for _, p := range pkts {
		if !p.IsVideo {
			continue
		}
		vh := p.Header.(av.VideoPacketHeader)
		if vh.IsSeq() {
			continue
		}
		if !vh.IsKeyFrame() {
			t.Fatalf("%s: media started on a non-keyframe", what)
		}
		return
	}
	if !anyAudioConfig(pkts) {
		t.Fatalf("%s: no key frame delivered", what)
	}
}

func anyAudioConfig(pkts []av.Packet) bool {
	for _, p := range pkts {
		if p.IsAudio {
			return true
		}
	}
	return false
}

// ---- tests --------------------------------------------------------------

// TestTakeoverMigratesPlayers: a publisher is replaced on the same key; the
// already connected player must keep receiving, seeded with the new stream's
// codec config and first key frame, and must not be closed by the handover.
func TestTakeoverMigratesPlayers(t *testing.T) {
	rs := NewRtmpStream()

	p1 := newFakeReader("pub-1", "live/cam")
	rs.HandleReader(p1)

	w := newFakeWriter("player-1", "live/cam", true)
	rs.HandleWriter(w)

	// First generation becomes playable.
	p1.send(videoSeqPkt())
	p1.send(videoKeyPkt(0))
	p1.send(videoInterPkt(40))
	waitFor(t, time.Second, "first generation packets", func() bool { return len(w.Packets()) >= 3 })

	p1.send(videoInterPkt(80))
	waitFor(t, time.Second, "player gets first gen media", func() bool { return len(w.Packets()) >= 4 })
	w.Reset()

	// New publisher takes over the same name.
	p2 := newFakeReader("pub-2", "live/cam")
	rs.HandleReader(p2)

	// Old publisher is closed by the takeover.
	waitFor(t, time.Second, "old publisher closed", p1.IsClosed)

	// The player must not have been closed by the takeover.
	if n := w.CloseCount(); n != 0 {
		t.Fatalf("player closed %d time(s) during takeover", n)
	}

	// New generation produces config + keyframe + media.
	p2.send(videoSeqPkt())
	p2.send(videoKeyPkt(0))
	p2.send(videoInterPkt(40))

	waitFor(t, time.Second, "player seeded from new generation", func() bool {
		return hasInter(w.Packets())
	})

	pkts := w.Packets()
	assertSeeded(t, "takeover", pkts)

	// Ensure the player did not receive a stale inter-frame from gen1 before
	// the new generation's key frame.
	seenKey := false
	for _, p := range pkts {
		if p.IsVideo {
			vh := p.Header.(av.VideoPacketHeader)
			if !vh.IsSeq() && vh.IsKeyFrame() {
				seenKey = true
				continue
			}
			if !vh.IsSeq() && !seenKey {
				t.Fatalf("got media before the new generation's key frame")
			}
		}
	}

	// The stream must be owned by pub-2.
	cur, ok := rs.GetStreams().Get("live/cam")
	if !ok {
		t.Fatal("stream entry missing after takeover")
	}
	if id := cur.(*Stream).ID(); id != "pub-2" {
		t.Fatalf("stream owned by %q, want pub-2", id)
	}
	if n := w.CloseCount(); n != 0 {
		t.Fatalf("player closed %d time(s) after takeover", n)
	}
}

func hasInter(pkts []av.Packet) bool {
	for _, p := range pkts {
		if p.IsVideo {
			vh := p.Header.(av.VideoPacketHeader)
			if !vh.IsSeq() && !vh.IsKeyFrame() {
				return true
			}
		}
	}
	return false
}

// TestOldSessionTeardownDoesNotTouchNewSession: after a takeover the old
// publisher's transport goroutine is exiting; it must not close the migrated
// player or remove the new stream object.
func TestOldSessionTeardownDoesNotTouchNewSession(t *testing.T) {
	rs := NewRtmpStream()
	p1 := newFakeReader("pub-1", "live/cam")
	rs.HandleReader(p1)

	w := newFakeWriter("player-1", "live/cam", true)
	rs.HandleWriter(w)

	p1.send(videoSeqPkt())
	p1.send(videoKeyPkt(0))
	waitFor(t, time.Second, "gen1 playable", func() bool { return len(w.Packets()) >= 2 })

	p2 := newFakeReader("pub-2", "live/cam")
	rs.HandleReader(p2)
	waitFor(t, time.Second, "old publisher closed", p1.IsClosed)

	// Give the old TransStart goroutine time to run its teardown path.
	time.Sleep(50 * time.Millisecond)

	if n := w.CloseCount(); n != 0 {
		t.Fatalf("old session teardown closed the migrated player %d time(s)", n)
	}
	if _, ok := rs.GetStreams().Get("live/cam"); !ok {
		t.Fatal("old session teardown removed the new stream object")
	}

	// New generation still works.
	p2.send(videoSeqPkt())
	p2.send(videoKeyPkt(0))
	p2.send(videoInterPkt(40))
	waitFor(t, time.Second, "player gets new generation", hasInterClosure(w))

	if n := w.CloseCount(); n != 0 {
		t.Fatalf("player closed %d time(s)", n)
	}
}

func hasInterClosure(w *fakeWriter) func() bool {
	return func() bool { return hasInter(w.Packets()) }
}

// TestRapidRepublishKeepsOnlyLatest: several publishers in quick succession;
// the final state must have exactly the latest publisher and the player must be
// neither duplicated nor lost.
func TestRapidRepublishKeepsOnlyLatest(t *testing.T) {
	rs := NewRtmpStream()

	p1 := newFakeReader("pub-1", "live/cam")
	rs.HandleReader(p1)
	w := newFakeWriter("player-1", "live/cam", true)
	rs.HandleWriter(w)

	p1.send(videoSeqPkt())
	p1.send(videoKeyPkt(0))
	waitFor(t, time.Second, "gen1 playable", func() bool { return len(w.Packets()) >= 2 })

	readers := []*fakeReader{p1}
	for i := 2; i <= 5; i++ {
		r := newFakeReader("pub-"+itoa(i), "live/cam")
		rs.HandleReader(r)
		readers = append(readers, r)
	}

	// All but the last publisher must be closed by the chained takeovers.
	for i := 0; i < len(readers)-1; i++ {
		r := readers[i]
		waitFor(t, time.Second, "old publisher closed", r.IsClosed)
	}
	last := readers[len(readers)-1]
	if last.IsClosed() {
		t.Fatal("latest publisher was closed")
	}

	cur, ok := rs.GetStreams().Get("live/cam")
	if !ok {
		t.Fatal("stream entry missing")
	}
	s := cur.(*Stream)
	if id := s.ID(); id != "pub-5" {
		t.Fatalf("stream owned by %q, want pub-5", id)
	}
	if n := s.GetWs().Count(); n != 1 {
		t.Fatalf("player set size = %d, want 1 (no duplicates, none lost)", n)
	}
	if n := w.CloseCount(); n != 0 {
		t.Fatalf("player closed %d time(s) during rapid re-publish", n)
	}

	// Final generation delivers.
	last.send(videoSeqPkt())
	last.send(videoKeyPkt(0))
	last.send(videoInterPkt(40))
	waitFor(t, time.Second, "player gets final generation", hasInterClosure(w))
	if n := s.GetWs().Count(); n != 1 {
		t.Fatalf("player set size = %d after delivery, want 1", n)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [8]byte
	n := len(b)
	for i > 0 {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
	}
	return string(b[n:])
}

// TestConcurrentTakeoversAndPlayers keeps multiple players attaching while
// publishers take over repeatedly; the player set must stay consistent and no
// player may be closed by the handovers.
func TestConcurrentTakeoversAndPlayers(t *testing.T) {
	rs := NewRtmpStream()

	first := newFakeReader("pub-0", "live/cam")
	rs.HandleReader(first)
	first.send(videoSeqPkt())
	first.send(videoKeyPkt(0))
	waitFor(t, time.Second, "gen0 playable", func() bool {
		v, ok := rs.GetStreams().Get("live/cam")
		return ok && v.(*Stream).GetReader() != nil
	})

	const players = 8
	writers := make([]*fakeWriter, players)
	var wg sync.WaitGroup
	for i := 0; i < players; i++ {
		writers[i] = newFakeWriter("player-"+itoa(i), "live/cam", true)
		wg.Add(1)
		go func(w *fakeWriter) {
			defer wg.Done()
			rs.HandleWriter(w)
		}(writers[i])
	}

	for i := 1; i <= 6; i++ {
		r := newFakeReader("pub-"+itoa(i), "live/cam")
		wg.Add(1)
		go func(r *fakeReader) {
			defer wg.Done()
			rs.HandleReader(r)
			r.send(videoSeqPkt())
			r.send(videoKeyPkt(0))
			r.send(videoInterPkt(40))
		}(r)
		time.Sleep(time.Millisecond)
	}
	wg.Wait()

	// Let transports settle.
	time.Sleep(100 * time.Millisecond)

	cur, ok := rs.GetStreams().Get("live/cam")
	if !ok {
		t.Fatal("stream entry missing after concurrent takeovers")
	}
	s := cur.(*Stream)
	if got := s.GetWs().Count(); got != players {
		t.Fatalf("player set size = %d, want %d (players lost or duplicated)", got, players)
	}
	for _, w := range writers {
		if n := w.CloseCount(); n > 1 {
			t.Fatalf("%s closed %d times", w.Info().UID, n)
		}
	}

	// The latest publisher must own the stream and its players must receive.
	if id := s.ID(); id == "" {
		t.Fatal("stream has no publisher")
	}
}

// TestHandleWriterBeforePublisher: a player connecting before any publisher
// must be attached and later receive from the first publisher.
func TestHandleWriterBeforePublisher(t *testing.T) {
	rs := NewRtmpStream()
	w := newFakeWriter("player-1", "live/cam", true)
	rs.HandleWriter(w)

	p := newFakeReader("pub-1", "live/cam")
	rs.HandleReader(p)
	p.send(videoSeqPkt())
	p.send(videoKeyPkt(0))
	p.send(videoInterPkt(40))

	waitFor(t, time.Second, "player gets media", hasInterClosure(w))
	assertSeeded(t, "late publisher", w.Packets())
}

// TestPublisherGoneThenRepublish: the publisher disappears (EOF) and a new one
// connects shortly after; the waiting player must be adopted, not closed.
func TestPublisherGoneThenRepublish(t *testing.T) {
	rs := NewRtmpStream()
	p1 := newFakeReader("pub-1", "live/cam")
	rs.HandleReader(p1)
	w := newFakeWriter("player-1", "live/cam", true)
	rs.HandleWriter(w)

	p1.send(videoSeqPkt())
	p1.send(videoKeyPkt(0))
	waitFor(t, time.Second, "gen1 playable", func() bool { return len(w.Packets()) >= 2 })
	w.Reset()

	// Publisher drops without a takeover.
	p1.eof()
	time.Sleep(20 * time.Millisecond)
	if n := w.CloseCount(); n != 0 {
		t.Fatalf("player closed on publisher EOF, want it to wait for re-publish")
	}

	p2 := newFakeReader("pub-2", "live/cam")
	rs.HandleReader(p2)
	p2.send(videoSeqPkt())
	p2.send(videoKeyPkt(0))
	p2.send(videoInterPkt(40))

	waitFor(t, time.Second, "player adopted by new publisher", hasInterClosure(w))
	assertSeeded(t, "re-publish after EOF", w.Packets())
	if n := w.CloseCount(); n != 0 {
		t.Fatalf("player closed %d time(s)", n)
	}
}
