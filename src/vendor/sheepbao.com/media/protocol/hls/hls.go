package hls

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"sheepbao.com/glog"
	"sheepbao.com/media/av"
	"sheepbao.com/media/container/flv"
	"sheepbao.com/media/container/ts"
	"sheepbao.com/media/parser"
	"sheepbao.com/media/utils/cmap"
	"sheepbao.com/media/utils/uid"
)

const (
	duration = 3000
)

var (
	ErrNoPublisher         = errors.New("No publisher")
	ErrInvalidReq          = errors.New("invalid req url path")
	ErrNoSupportVideoCodec = errors.New("no support video codec")
	ErrNoSupportAudioCodec = errors.New("no support audio codec")
)

var crossdomainxml = []byte(`<?xml version="1.0" ?>
<cross-domain-policy>
	<allow-access-from domain="*" />
	<allow-http-request-headers-from domain="*" headers="*"/>
</cross-domain-policy>`)

// Server serves HLS for stream names keyed by app/stream.
//
// Segment caches (caches) are owned by the Server and keyed by stream name, so
// they survive the per-publisher Source sessions. A re-publish on the same name
// starts a new session inside the same Source while keeping the cache: old
// segments stay fetchable and the playlist's media sequence continues instead
// of restarting or 403ing.
type Server struct {
	mu     sync.Mutex
	l      net.Listener
	conns  cmap.ConcurrentMap // key -> *Source (current session)
	caches map[string]*TSCacheItem
}

func NewServer() *Server {
	ret := &Server{
		conns:  cmap.New(),
		caches: make(map[string]*TSCacheItem),
	}
	go ret.checkStop()
	return ret
}

func (self *Server) Serve(l net.Listener) error {
	mux := http.NewServeMux()
	mux.Handle("/", self)
	self.l = l
	http.Serve(l, mux)
	return nil
}

// ServeHTTP makes Server an http.Handler so it can be mounted directly (and so
// the request path is exercisable in tests without binding a port).
func (self *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	self.handle(w, r)
}

// GetWriter returns the Source that consumes the publisher for info.Key.
//
// The same *Source is reused across publisher takeovers. This matters because
// the RTMP layer migrates its writers (including this one) onto a new stream
// object on re-publish; keeping the writer identity stable means the migrated
// writer is the very object that will receive the new publisher's packets.
//
// A takeover resets the session through OnMigrate, which the RTMP stream calls
// at the exact handover point (after the old publisher stopped, before the new
// one is read). GetWriter therefore does not reset the session itself: on a
// takeover it only refreshes the publisher info on the already-reset Source.
func (self *Server) GetWriter(info av.Info) av.WriteCloser {
	self.mu.Lock()
	defer self.mu.Unlock()

	cache, ok := self.caches[info.Key]
	if !ok {
		cache = NewTSCacheItem(info.Key)
		self.caches[info.Key] = cache
	}

	if v, ok := self.conns.Get(info.Key); ok {
		s := v.(*Source)
		if !s.isClosed() {
			// Reuse the Source that was (or is about to be) reset by OnMigrate.
			s.adoptPublisher(info)
			return s
		}
		// The previous Source was fully closed (e.g. by the liveness sweep).
		// Fall through and build a fresh one below with a new generation.
	}

	gen := cache.StartSession()
	info.UID = uid.NEWID()
	info.Inter = false
	info.Type = "hls"
	glog.Infoln("new hls source: ", info, "session", gen)
	s := NewSource(info, cache, gen)
	self.conns.Set(info.Key, s)
	return s
}

func (self *Server) getConn(key string) *Source {
	v, ok := self.conns.Get(key)
	if !ok {
		return nil
	}
	return v.(*Source)
}

// getCache returns the persistent segment cache for key. It is independent of
// whether a publisher is currently connected.
func (self *Server) getCache(key string) *TSCacheItem {
	self.mu.Lock()
	defer self.mu.Unlock()
	return self.caches[key]
}

func (self *Server) checkStop() {
	for {
		<-time.After(5 * time.Second)
		self.mu.Lock()
		for item := range self.conns.IterBuffered() {
			v := item.Val.(*Source)
			if !v.Alive() {
				glog.Infoln("check stop and remove: ", v.Info())
				// Only drop the entry if it still points at the Source we
				// checked; a takeover may have installed a newer session.
				if self.conns.RemoveIf(item.Key, v) {
					// Stop this Source's sender, but keep the per-name segment
					// cache: it outlives the session so old segments stay
					// servable and the next publisher continues the playlist.
					v.Close(errors.New("check alive"))
				}
			}
		}
		self.mu.Unlock()
	}
}

func (self *Server) handle(w http.ResponseWriter, r *http.Request) {
	if path.Base(r.URL.Path) == "crossdomain.xml" {
		w.Header().Set("Content-Type", "application/xml")
		w.Write(crossdomainxml)
		return
	}
	switch path.Ext(r.URL.Path) {
	case ".m3u8":
		key, err := self.parseM3u8(r.URL.Path)
		if err != nil {
			glog.Errorln("parse url error: ", key)
			http.Error(w, ErrNoPublisher.Error(), http.StatusForbidden)
			return
		}
		tsCache := self.getCache(key)
		if tsCache == nil {
			glog.Errorln("can't get conn: ", key)
			http.Error(w, ErrNoPublisher.Error(), http.StatusForbidden)
			return
		}
		body, err := tsCache.GenM3U8PlayList()
		if err != nil {
			glog.Errorln("GenM3U8PlayList error: ", err)
			if err == ErrNoSegment {
				// Publisher is connected but the current session has not
				// produced a key frame / segment yet. Say so explicitly instead
				// of serving an empty or previous-session playlist.
				http.Error(w, err.Error(), http.StatusServiceUnavailable)
				return
			}
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Content-Type", "application/x-mpegURL")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Write(body)
	case ".ts":
		key, _ := self.parseTs(r.URL.Path)
		tsCache := self.getCache(key)
		if tsCache == nil {
			http.Error(w, ErrNoPublisher.Error(), http.StatusForbidden)
			return
		}
		item, err := tsCache.GetItem(r.URL.Path)
		if err != nil {
			glog.Errorln("GetItem error: ", err)
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Content-Type", "video/mp2t")
		w.Header().Set("Content-Length", strconv.Itoa(len(item.Data)))
		w.Write(item.Data)
	}
}

func (self *Server) parseM3u8(pathstr string) (key string, err error) {
	pathstr = strings.TrimLeft(pathstr, "/")
	paths := strings.SplitN(pathstr, ".", 2)
	if len(paths) != 2 {
		err = ErrInvalidReq
		return
	}
	key = paths[0]
	if paths[1] != "m3u8" {
		err = errors.New("wrong url")
		return
	}
	return
}

func (self *Server) parseTs(pathstr string) (key string, err error) {
	pathstr = strings.TrimLeft(pathstr, "/")
	paths := strings.SplitN(pathstr, "/", 3)
	if len(paths) != 3 {
		err = fmt.Errorf("invalid path=%s", pathstr)
		return
	}
	key = paths[0] + "/" + paths[1]

	return
}

const (
	videoHZ      = 90000
	aacSampleLen = 1024
	maxQueueNum  = 512

	h264_default_hz uint64 = 90
)

// Source consumes the packets of one stream name and muxes them into TS
// segments of the shared TSCacheItem. Its identity is stable across publisher
// takeovers (the RTMP layer migrates it as a writer); the per-session muxing
// state lives in sessionState and is replaced on takeover.
type Source struct {
	av.RWBaser

	mu      sync.Mutex
	info    av.Info
	tsCache *TSCacheItem
	session uint64
	// st is the current session's muxing state. Packets carry the generation
	// they were queued under, so a packet left over from a retired session is
	// dropped rather than contaminating the new session.
	st          *sessionState
	closed      bool
	done        chan struct{}
	closeOnce   sync.Once
	packetQueue chan queuedPacket
}

type queuedPacket struct {
	gen uint64
	p   av.Packet
}

// sessionState is the mutable muxing state of a single publisher session.
type sessionState struct {
	key       string // stream key (app/stream); constant for a Source's lifetime
	seq       int
	bwriter   *bytes.Buffer
	btswriter *bytes.Buffer
	demuxer   *flv.Demuxer
	muxer     *ts.Muxer
	pts, dts  uint64
	stat      *status
	align     *align
	cache     *audioCache
	tsparser  *parser.CodecParser
}

func newSessionState(key string) *sessionState {
	return &sessionState{
		key:      key,
		align:    &align{},
		stat:     newStatus(),
		cache:    newAudioCache(),
		demuxer:  flv.NewDemuxer(),
		muxer:    ts.NewMuxer(),
		tsparser: parser.NewCodecParser(),
		bwriter:  bytes.NewBuffer(make([]byte, 100*1024)),
	}
}

func NewSource(info av.Info, tsCache *TSCacheItem, session uint64) *Source {
	s := &Source{
		info:        info,
		tsCache:     tsCache,
		session:     session,
		st:          newSessionState(info.Key),
		RWBaser:     av.NewRWBaser(time.Second * 10),
		done:        make(chan struct{}),
		packetQueue: make(chan queuedPacket, maxQueueNum),
	}
	go s.SendPacket()
	return s
}

// OnMigrate implements av.Migrator. The RTMP stream calls it when this writer
// is moved from a retired stream generation to the new one, with no packet in
// flight from either publisher. Advancing the session here guarantees the new
// publisher's first packets are muxed into a clean session instead of the old
// one's state, at exactly the handover point.
func (self *Source) OnMigrate() {
	gen := self.tsCache.StartSession()
	self.mu.Lock()
	self.session = gen
	self.st = newSessionState(self.info.Key)
	self.closed = false
	self.mu.Unlock()
	self.SetPreTime()
}

// adoptPublisher refreshes the publisher identity on a reused Source. Session
// advancement/reset is owned by OnMigrate (RTMP handover) or by fresh Source
// creation, never by this method, so a takeover cannot reset twice.
func (self *Source) adoptPublisher(info av.Info) {
	self.mu.Lock()
	if !self.closed {
		self.info = info
	}
	self.mu.Unlock()
	self.SetPreTime()
}

func (self *Source) GetCacheInc() *TSCacheItem {
	return self.tsCache
}

func (self *Source) isClosed() bool {
	self.mu.Lock()
	defer self.mu.Unlock()
	return self.closed
}

func (self *Source) DropPacket(pktQue chan queuedPacket, info av.Info) {
	glog.Errorf("[%v] packet queue max!!!", info)
	for i := 0; i < maxQueueNum-84; i++ {
		var qp queuedPacket
		ok := false
		select {
		case qp, ok = <-pktQue:
		default:
		}
		// try to don't drop audio
		if ok && qp.p.IsAudio {
			select {
			case pktQue <- qp:
			default:
			}
		}

		if ok && qp.p.IsVideo {
			videoPkt, ok := qp.p.Header.(av.VideoPacketHeader)
			// dont't drop sps config and dont't drop key frame
			if ok && (videoPkt.IsSeq() || videoPkt.IsKeyFrame()) {
				select {
				case pktQue <- qp:
				default:
				}
			}
			if len(pktQue) > maxQueueNum-10 {
				select {
				case <-pktQue:
				default:
				}
			}
		}

	}
	glog.Infoln("packet queue len: ", len(pktQue))
}

func (self *Source) Write(p av.Packet) error {
	self.SetPreTime()
	self.mu.Lock()
	if self.closed {
		self.mu.Unlock()
		return errors.New("closed")
	}
	qp := queuedPacket{gen: self.session, p: p}
	info := self.info
	self.mu.Unlock()

	if len(self.packetQueue) >= maxQueueNum-24 {
		self.DropPacket(self.packetQueue, info)
	}
	select {
	case self.packetQueue <- qp:
	case <-self.done:
		return errors.New("closed")
	default:
		// Queue full even after dropping: skip this packet rather than block
		// the stream's delivery loop.
	}
	return nil
}

func (self *Source) SendPacket() error {
	self.mu.Lock()
	info := self.info
	self.mu.Unlock()
	defer func() {
		glog.Infof("[%v] hls sender stop", info)
		if r := recover(); r != nil {
			glog.Errorln("hls SendPacket panic: ", r)
		}
	}()
	glog.Infof("[%v] hls sender start", info)
	for {
		select {
		case <-self.done:
			return errors.New("closed")
		case qp := <-self.packetQueue:
			self.mu.Lock()
			if self.closed {
				self.mu.Unlock()
				return errors.New("closed")
			}
			// Drop packets queued under a retired session generation.
			if qp.gen != self.session {
				self.mu.Unlock()
				continue
			}
			st := self.st
			gen := self.session
			self.mu.Unlock()

			self.handlePacket(st, gen, qp.p)
		}
	}
}

func (self *Source) handlePacket(st *sessionState, gen uint64, p av.Packet) {
	if p.IsMetadata {
		return
	}
	err := st.demuxer.Demux(&p)
	if err == flv.ErrAvcEndSEQ {
		glog.Errorln(err)
		return
	} else if err != nil {
		glog.Errorln(err)
		return
	}
	compositionTime, isSeq, err := st.parse(self, gen, &p)
	if err != nil {
		glog.Errorln(err)
	}
	if err != nil || isSeq {
		return
	}
	if st.btswriter != nil {
		st.stat.update(p.IsVideo, p.TimeStamp)
		st.calcPtsDts(p.IsVideo, p.TimeStamp, uint32(compositionTime))
		st.tsMux(&p)
	}
}

func (self *Source) Info() (ret av.Info) {
	self.mu.Lock()
	defer self.mu.Unlock()
	return self.info
}

// Close retires the whole Source (used when the server drops it). It is
// idempotent and never touches the shared TSCacheItem: segments already
// published remain servable to viewers and to the next session on the same name.
func (self *Source) Close(err error) {
	glog.Infoln("hls source closed: ", self.Info())
	self.closeOnce.Do(func() {
		self.mu.Lock()
		self.closed = true
		close(self.done)
		self.mu.Unlock()
	})
}

// ---- per-session muxing -------------------------------------------------
// These methods take the session state explicitly so they never run against a
// stale session after a takeover.

func (st *sessionState) cut(src *Source, gen uint64) {
	newf := true
	if st.btswriter == nil {
		st.btswriter = bytes.NewBuffer(nil)
	} else if st.stat.durationMs() >= duration {
		st.flushAudio()

		st.seq++
		// Include the session generation and sequence in the name so a fast
		// re-publish cannot overwrite a segment from the previous session.
		filename := fmt.Sprintf("/%s/%d_%d_%d.ts", st.key, gen, time.Now().Unix(), st.seq)
		item := NewTSItem(filename, int(st.stat.durationMs()), src.tsCache.NextSeq(), st.btswriter.Bytes())
		item.Session = gen
		src.tsCache.SetItem(filename, item)

		st.btswriter.Reset()
		st.stat.resetAndNew()
	} else {
		newf = false
	}
	if newf {
		st.btswriter.Write(st.muxer.PAT())
		st.btswriter.Write(st.muxer.PMT(av.SOUND_AAC, true))
	}
}

func (st *sessionState) parse(src *Source, gen uint64, p *av.Packet) (int32, bool, error) {
	var compositionTime int32
	var ah av.AudioPacketHeader
	var vh av.VideoPacketHeader
	if p.IsVideo {
		vh = p.Header.(av.VideoPacketHeader)
		if vh.CodecID() != av.VIDEO_H264 {
			return compositionTime, false, ErrNoSupportVideoCodec
		}
		compositionTime = vh.CompositionTime()
		if vh.IsKeyFrame() && vh.IsSeq() {
			return compositionTime, true, st.tsparser.Parse(p, st.bwriter)
		}
	} else {
		ah = p.Header.(av.AudioPacketHeader)
		if ah.SoundFormat() != av.SOUND_AAC {
			return compositionTime, false, ErrNoSupportAudioCodec
		}
		if ah.AACPacketType() == av.AAC_SEQHDR {
			return compositionTime, true, st.tsparser.Parse(p, st.bwriter)
		}
	}
	st.bwriter.Reset()
	if err := st.tsparser.Parse(p, st.bwriter); err != nil {
		return compositionTime, false, err
	}
	p.Data = st.bwriter.Bytes()

	if p.IsVideo && vh.IsKeyFrame() {
		st.cut(src, gen)
	}
	return compositionTime, false, nil
}

func (st *sessionState) calcPtsDts(isVideo bool, ts, compositionTs uint32) {
	st.dts = uint64(ts) * h264_default_hz
	if isVideo {
		st.pts = st.dts + uint64(compositionTs)*h264_default_hz
	} else {
		sampleRate, _ := st.tsparser.SampleRate()
		st.align.align(&st.dts, uint32(videoHZ*aacSampleLen/sampleRate))
		st.pts = st.dts
	}
}

func (st *sessionState) flushAudio() error {
	return st.muxAudio(1)
}

func (st *sessionState) muxAudio(limit byte) error {
	if st.cache.CacheNum() < limit {
		return nil
	}
	var p av.Packet
	_, pts, buf := st.cache.GetFrame()
	p.Data = buf
	p.TimeStamp = uint32(pts / h264_default_hz)
	return st.muxer.Mux(&p, st.btswriter)
}

func (st *sessionState) tsMux(p *av.Packet) error {
	if p.IsVideo {
		return st.muxer.Mux(p, st.btswriter)
	}
	st.cache.Cache(p.Data, st.pts)
	return st.muxAudio(cache_max_frames)
}
