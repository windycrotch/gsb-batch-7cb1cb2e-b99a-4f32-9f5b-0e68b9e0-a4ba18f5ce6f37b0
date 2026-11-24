package hls

import (
	"bytes"
	"container/list"
	"errors"
	"fmt"
	"sync"
)

type TSCache struct {
	entrys map[string]*TSCacheItem
}

func NewTSCache() *TSCache {
	return &TSCache{
		entrys: make(map[string]*TSCacheItem),
	}
}

func (self *TSCache) Set(key string, e *TSCacheItem) {
	self.entrys[key] = e
}

func (self *TSCache) Get(key string) *TSCacheItem {
	v := self.entrys[key]
	return v
}

const (
	maxTSCacheNum = 3
)

var (
	ErrNoKey     = errors.New("No key for cache")
	ErrNoSegment = errors.New("stream not ready: no segment available yet")
)

// TSCacheItem is the sliding window of TS segments for one stream name. It is
// owned by the HLS Server and outlives individual Source sessions, so a
// re-publish on the same name keeps serving the tail of the previous session
// instead of 403ing or restarting the media sequence.
type TSCacheItem struct {
	mu      sync.Mutex
	id      string
	num     int
	ll      *list.List
	lm      map[string]TSItem
	lastSeq int
	// gen/started gate playlists on the current session having produced at
	// least one segment. Before that a playlist request is answered with an
	// explicit "not ready" instead of an empty/previous-session playlist.
	gen     uint64
	started bool
}

func NewTSCacheItem(id string) *TSCacheItem {
	return &TSCacheItem{
		id:  id,
		ll:  list.New(),
		num: maxTSCacheNum,
		lm:  make(map[string]TSItem),
	}
}

// StartSession begins a new session generation and returns it. Segments from
// previous sessions stay servable (so in-flight viewers can finish), but the
// playlist is considered "not ready" until the new session emits a segment.
// The cache is the single authority for the generation counter.
func (self *TSCacheItem) StartSession() uint64 {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.gen++
	self.started = false
	return self.gen
}

func (self *TSCacheItem) ID() string {
	return self.id
}

// NextSeq returns the next media sequence number. It never goes backwards,
// even after older segments have been evicted.
func (self *TSCacheItem) NextSeq() int {
	self.mu.Lock()
	defer self.mu.Unlock()
	return self.lastSeq + 1
}

// Len returns the number of cached segments.
func (self *TSCacheItem) Len() int {
	self.mu.Lock()
	defer self.mu.Unlock()
	return len(self.lm)
}

func (self *TSCacheItem) GenM3U8PlayList() ([]byte, error) {
	self.mu.Lock()
	defer self.mu.Unlock()

	if !self.started {
		return nil, ErrNoSegment
	}
	if len(self.lm) == 0 {
		return nil, ErrNoSegment
	}

	var seq int
	var getSeq bool
	var maxDuration int
	var lastSession uint64
	var haveSession bool
	m3u8body := bytes.NewBuffer(nil)
	for e := self.ll.Front(); e != nil; e = e.Next() {
		key := e.Value.(string)
		v, ok := self.lm[key]
		if ok {
			if v.Duration > maxDuration {
				maxDuration = v.Duration
			}
			if !getSeq {
				getSeq = true
				seq = v.SeqNum
			}
			if haveSession && v.Session != lastSession {
				m3u8body.WriteString("#EXT-X-DISCONTINUITY\n")
			}
			lastSession = v.Session
			haveSession = true
			fmt.Fprintf(m3u8body, "#EXTINF:%.3f,\n%s\n", float64(v.Duration)/float64(1000), v.Name)
		}
	}
	w := bytes.NewBuffer(nil)
	fmt.Fprintf(w,
		"#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-ALLOW-CACHE:NO\n#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:%d\n\n",
		maxDuration/1000+1, seq)
	w.Write(m3u8body.Bytes())
	return w.Bytes(), nil
}

func (self *TSCacheItem) SetItem(key string, item TSItem) {
	self.mu.Lock()
	defer self.mu.Unlock()
	// Ignore segments from a session that is no longer current: a late packet
	// from a retired session must not make the new session look ready or push
	// stale media ahead of it.
	if item.Session != 0 && item.Session < self.gen {
		return
	}
	if self.ll.Len() == self.num {
		e := self.ll.Front()
		self.ll.Remove(e)
		k := e.Value.(string)
		delete(self.lm, k)
	}
	self.lm[key] = item
	self.ll.PushBack(key)
	if item.SeqNum > self.lastSeq {
		self.lastSeq = item.SeqNum
	}
	if item.Session == self.gen {
		self.started = true
	}
}

func (self *TSCacheItem) GetItem(key string) (TSItem, error) {
	self.mu.Lock()
	defer self.mu.Unlock()
	item, ok := self.lm[key]
	if !ok {
		return item, ErrNoKey
	}
	return item, nil
}

type TSItem struct {
	Name     string
	SeqNum   int
	Duration int
	Session  uint64
	Data     []byte
}

func NewTSItem(name string, duration, seqNum int, b []byte) TSItem {
	var item TSItem
	item.Name = name
	item.SeqNum = seqNum
	item.Duration = duration
	item.Data = make([]byte, len(b))
	copy(item.Data, b)
	return item
}
