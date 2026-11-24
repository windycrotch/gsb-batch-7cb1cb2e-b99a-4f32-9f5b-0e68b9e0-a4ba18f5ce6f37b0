package rtmp

import (
	"errors"
	"sync"
	"time"

	"sheepbao.com/glog"
	"sheepbao.com/media/av"
	"sheepbao.com/media/protocol/rtmp/cache"
	"sheepbao.com/media/utils/cmap"
)

var (
	EmptyID = ""
)

// RtmpStream keeps one Stream per app/stream name.
//
// A new publisher on an existing name takes over: the old publisher is
// stopped, every already connected player is migrated to a fresh Stream
// generation (so they are re-seeded with the new stream's codec configuration
// and first key frame), and only then does the new generation become the
// current one. mu serializes generation changes with player attach/detach and
// the liveness sweep, so a player can never be left attached to a retired
// generation.
type RtmpStream struct {
	mu      sync.Mutex
	streams cmap.ConcurrentMap
}

func NewRtmpStream() *RtmpStream {
	ret := &RtmpStream{
		streams: cmap.New(),
	}
	go ret.CheckAlive()
	return ret
}

// HandleReader installs r as the publisher for its stream key. Any previous
// publisher is stopped and its players are moved onto the new generation.
func (rs *RtmpStream) HandleReader(r av.ReadCloser) {
	info := r.Info()

	rs.mu.Lock()
	defer rs.mu.Unlock()

	ns := NewStream()
	old, ok := rs.streams.Get(info.Key)
	// Publish the new generation first: from this point on, writers and the
	// liveness sweep see the new Stream, never the retired one.
	rs.streams.Set(info.Key, ns)

	if ok && old != nil {
		os := old.(*Stream)
		// Stop the old publisher. Players are *not* closed here; they are
		// migrated below and must survive into the new generation.
		os.TransStop()
		os.MigrateTo(ns)
	}

	ns.AddReader(r)
}

// HandleWriter attaches a player to the current generation for its key,
// creating an empty generation if no publisher has connected yet.
func (rs *RtmpStream) HandleWriter(w av.WriteCloser) {
	info := w.Info()

	rs.mu.Lock()
	defer rs.mu.Unlock()

	var s *Stream
	item, ok := rs.streams.Get(info.Key)
	if !ok || item == nil {
		s = NewStream()
		rs.streams.Set(info.Key, s)
	} else {
		s = item.(*Stream)
	}
	// AddWriter runs under rs.mu, so the generation cannot be retired between
	// the lookup and the attach: the player is never dropped on a takeover.
	s.AddWriter(w)
}

func (rs *RtmpStream) GetStreams() cmap.ConcurrentMap {
	return rs.streams
}

func (rs *RtmpStream) CheckAlive() {
	for {
		<-time.After(5 * time.Second)
		rs.mu.Lock()
		for item := range rs.streams.IterBuffered() {
			v := item.Val.(*Stream)
			if v.CheckAlive() == 0 {
				// Retire and close any remaining players before dropping the
				// entry, then remove it only if it is still the entry we
				// checked; a takeover may have installed a newer generation.
				v.closeInter()
				rs.streams.RemoveIf(item.Key, v)
				if r := v.GetReader(); r != nil {
					glog.Infof("check alive and remove: %v", r.Info())
				}
			}
		}
		rs.mu.Unlock()
	}
}

// Stream is one session generation of a named stream. All mutable state is
// guarded by mu so packet delivery, migration and teardown see a consistent
// view of the publisher and the player set.
type Stream struct {
	mu      sync.Mutex
	isStart bool
	closed  bool // generation retired: no new reader/writer, players belong elsewhere
	cache   *cache.Cache
	r       av.ReadCloser
	ws      cmap.ConcurrentMap
}

type PackWriterCloser struct {
	init bool // guarded by Stream.mu
	w    av.WriteCloser
}

func (p *PackWriterCloser) GetWriter() av.WriteCloser {
	return p.w
}

func NewStream() *Stream {
	return &Stream{
		cache: cache.NewCache(),
		ws:    cmap.New(),
	}
}

func (s *Stream) ID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.r != nil {
		return s.r.Info().UID
	}
	return EmptyID
}

func (s *Stream) GetReader() av.ReadCloser {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.r
}

func (s *Stream) GetWs() cmap.ConcurrentMap {
	return s.ws
}

// MigrateTo moves every player currently attached to s onto dst. Detaching
// happens under s.mu, so once migration returns s.teardown can no longer reach
// those players: it is safe for the old publisher to close afterwards.
//
// Writers that maintain per-publisher state (av.Migrator) are told to reset
// before they are re-attached, at a moment when neither the old nor the new
// publisher is being delivered to them.
func (s *Stream) MigrateTo(dst *Stream) {
	s.mu.Lock()
	if s.closed {
		// Already retired; whoever retired it moved the players.
		s.mu.Unlock()
		return
	}
	s.closed = true
	moved := make([]*PackWriterCloser, 0, s.ws.Count())
	for item := range s.ws.IterBuffered() {
		v := item.Val.(*PackWriterCloser)
		s.ws.Remove(item.Key)
		v.w.CalcBaseTimestamp()
		moved = append(moved, v)
	}
	s.mu.Unlock()

	for _, v := range moved {
		if m, ok := v.w.(av.Migrator); ok {
			m.OnMigrate()
		}
		dst.AddWriter(v.w)
	}
}

func (s *Stream) AddReader(r av.ReadCloser) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		glog.Infof("[%v] stream generation already retired, drop reader", r.Info())
		r.Close(errors.New("superseded"))
		return
	}
	s.r = r
	s.isStart = true
	s.mu.Unlock()
	go s.TransStart()
}

func (s *Stream) AddWriter(w av.WriteCloser) {
	info := w.Info()

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		glog.Infof("[%v] stream generation already retired, drop writer", info)
		w.Close(errors.New("superseded"))
		return
	}
	s.ws.Set(info.UID, &PackWriterCloser{w: w})
	// Seed the player immediately when the generation already has a key frame;
	// otherwise first delivery happens from TransStart once one arrives.
	s.tryInitLocked(w, info.UID)
	s.mu.Unlock()
}

// tryInitLocked seeds a freshly attached writer from the cache when the cache
// has enough data (codec config + key frame). s.mu must be held.
func (s *Stream) tryInitLocked(w av.WriteCloser, uid string) {
	if s.closed || !s.cache.Ready() {
		return
	}
	item, ok := s.ws.Get(uid)
	if !ok {
		return
	}
	pw := item.(*PackWriterCloser)
	if pw.init {
		return
	}
	if err := s.cache.Send(w); err != nil {
		glog.Errorf("[%s] send cache packet error: %v, remove", w.Info(), err)
		s.ws.Remove(uid)
		return
	}
	pw.init = true
}

func (s *Stream) TransStart() {
	s.mu.Lock()
	r := s.r
	s.mu.Unlock()
	defer func() {
		if r != nil {
			glog.Infof("[%v] Transport stop", r.Info())
		}
	}()

	var p av.Packet
	for {
		s.mu.Lock()
		start := s.isStart && !s.closed
		s.mu.Unlock()
		if !start || r == nil {
			// Retired by a takeover, or already stopped. Never touch players
			// here: on takeover they belong to the new generation, and on a
			// plain stop they are closed by CheckAlive once they go idle.
			return
		}

		err := r.Read(&p)
		if err != nil {
			s.mu.Lock()
			s.isStart = false
			retired := s.closed
			s.mu.Unlock()
			if !retired {
				// The publisher vanished without a takeover. Keep the players
				// attached so a fast re-publish adopts them (see HandleReader);
				// if none arrives, CheckAlive closes them after the write
				// timeout and then removes this generation.
				glog.Infof("[%v] publisher gone, waiting for takeover", r.Info())
			}
			return
		}

		s.mu.Lock()
		if s.closed || !s.isStart {
			// A takeover retired this generation while we were blocked in
			// Read. The players now belong to the new generation; do not
			// touch (or close) them.
			s.mu.Unlock()
			return
		}
		s.cache.Write(p)
		ready := s.cache.Ready()
		for item := range s.ws.IterBuffered() {
			v := item.Val.(*PackWriterCloser)
			if !v.init {
				if !ready {
					// New generation has not produced a key frame yet. Hold
					// the player rather than sending mid-GOP media.
					continue
				}
				if err = s.cache.Send(v.w); err != nil {
					glog.Errorf("[%s] send cache packet error: %v, remove", v.w.Info(), err)
					s.ws.Remove(item.Key)
					continue
				}
				v.init = true
			} else {
				if err = v.w.Write(p); err != nil {
					glog.Errorf("[%s] write packet error: %v, remove", v.w.Info(), err)
					s.ws.Remove(item.Key)
				}
			}
		}
		s.mu.Unlock()
	}
}

// TransStop stops reading from this generation's publisher. It does not close
// attached players: those are migrated to the successor generation, or closed
// by CheckAlive when the stream really ends.
func (s *Stream) TransStop() {
	s.mu.Lock()
	r := s.r
	start := s.isStart
	s.isStart = false
	s.mu.Unlock()

	if start && r != nil {
		// Closing the publisher unblocks TransStart's pending Read; that
		// goroutine then observes isStart==false / closed and returns.
		r.Close(errors.New("stop old"))
	}
}

func (s *Stream) CheckAlive() (n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0
	}
	if s.r != nil && s.isStart {
		if s.r.Alive() {
			n++
		} else {
			s.r.Close(errors.New("read timeout"))
		}
	}
	for item := range s.ws.IterBuffered() {
		v := item.Val.(*PackWriterCloser)
		if v.w != nil {
			if !v.w.Alive() {
				glog.Infof("[%v] player closed and remove\n", v.w.Info())
				s.ws.Remove(item.Key)
				v.w.Close(errors.New("write timeout"))
				continue
			}
			n++
		}
	}
	return
}

// closeInter closes this generation's managed (server-side) players. It only
// closes a player once: the writer is removed from the set before Close, and a
// retired generation (players migrated away) closes nothing.
func (s *Stream) closeInter() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	for item := range s.ws.IterBuffered() {
		v := item.Val.(*PackWriterCloser)
		if v.w != nil {
			if v.w.Info().IsInterval() {
				s.ws.Remove(item.Key)
				v.w.Close(errors.New("closed"))
				glog.Infof("[%v] player closed and remove\n", v.w.Info())
			}
		}
	}
}
