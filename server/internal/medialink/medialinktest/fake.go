// Package medialinktest provides an in-process stand-in for the media engine.
package medialinktest

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"

	"github.com/whispem/ambivox/server/internal/protocol"
)

// Engine echoes every audio packet back as speech, captions the first packet
// of each stream (a live draft, then the final pair) and refuses streams whose
// target language is "zz" or has been refused with Refuse.
//
// By default it acknowledges CLOSE at once. HoldClose makes it leave closed
// streams in place instead, so a test can replay speech and captions from a
// stream the server has already replaced.
type Engine struct {
	Path string

	mu       sync.Mutex
	listener net.Listener
	conns    map[net.Conn]struct{}
	opened   int
	early    int
	closed   int
	hold     bool
	refuse   map[string]bool
	active   map[uint32][2]string
	zombies  []zombie
	wg       sync.WaitGroup
}

type zombie struct {
	conn           *conn
	id             uint32
	source, target string
}

// conn serialises writes to one engine connection: the reader answers frames
// while a test may replay from another goroutine.
type conn struct {
	net.Conn
	mu sync.Mutex
}

func (c *conn) send(frames ...protocol.Frame) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, frame := range frames {
		if protocol.Write(c.Conn, frame) != nil {
			return false
		}
	}
	return true
}

func Start(t testing.TB) *Engine {
	t.Helper()
	dir, err := os.MkdirTemp("", "ambivox")
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{
		Path:   filepath.Join(dir, "media.sock"),
		conns:  make(map[net.Conn]struct{}),
		refuse: make(map[string]bool),
		active: make(map[uint32][2]string),
	}
	if err := e.Listen(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		e.Stop()
		os.RemoveAll(dir)
	})
	return e
}

func (e *Engine) Listen() error {
	listener, err := net.Listen("unix", e.Path)
	if err != nil {
		return err
	}
	e.mu.Lock()
	e.listener = listener
	e.mu.Unlock()
	e.wg.Go(func() {
		for {
			raw, err := listener.Accept()
			if err != nil {
				return
			}
			c := &conn{Conn: raw}
			e.mu.Lock()
			e.conns[raw] = struct{}{}
			e.mu.Unlock()
			e.wg.Go(func() {
				e.serve(c)
			})
		}
	})
	return nil
}

func (e *Engine) Stop() {
	e.mu.Lock()
	if e.listener != nil {
		e.listener.Close()
		e.listener = nil
	}
	for conn := range e.conns {
		conn.Close()
	}
	e.mu.Unlock()
	e.wg.Wait()
	os.Remove(e.Path)
}

func (e *Engine) Opened() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.opened
}

func (e *Engine) OpenedEarly() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.early
}

// Closed counts the streams the server has asked to finish.
func (e *Engine) Closed() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.closed
}

// HoldClose makes the engine keep closed streams alive instead of answering
// CLOSED, so their late speech and captions can be replayed.
func (e *Engine) HoldClose(on bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.hold = on
}

// Refuse makes the engine refuse new streams translating into target.
func (e *Engine) Refuse(target string, on bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.refuse[target] = on
}

// Pairs lists the language pairs of the streams the engine still holds open,
// sorted, so a test can tell exactly which streams are in effect.
func (e *Engine) Pairs() [][2]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	pairs := make([][2]string, 0, len(e.active))
	for _, pair := range e.active {
		pairs = append(pairs, pair)
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i][0] != pairs[j][0] {
			return pairs[i][0] < pairs[j][0]
		}
		return pairs[i][1] < pairs[j][1]
	})
	return pairs
}

// ReplaySpeech sends a SPEECH frame as if the most recently closed stream that
// matched the pair were still speaking. With no pair it uses the most recently
// closed stream of all. It reports whether there was such a stream.
func (e *Engine) ReplaySpeech(packet []byte, pair ...string) bool {
	z, ok := e.zombieFor(pair)
	if !ok {
		return false
	}
	return z.conn.send(protocol.Frame{Type: protocol.TypeSpeech, Stream: z.id, Packet: packet})
}

// ReplayCaption sends a final caption pair from the most recently closed stream
// that matched the pair, as ReplaySpeech picks it.
func (e *Engine) ReplayCaption(from, to string, pair ...string) bool {
	z, ok := e.zombieFor(pair)
	if !ok {
		return false
	}
	return z.conn.send(
		protocol.Frame{Type: protocol.TypeCaption, Stream: z.id, Kind: protocol.CaptionSource, Text: from},
		protocol.Frame{Type: protocol.TypeCaption, Stream: z.id, Kind: protocol.CaptionTranslation, Text: to},
	)
}

func (e *Engine) zombieFor(pair []string) (zombie, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := len(e.zombies) - 1; i >= 0; i-- {
		z := e.zombies[i]
		if len(pair) < 2 || (z.source == pair[0] && z.target == pair[1]) {
			return z, true
		}
	}
	return zombie{}, false
}

func (e *Engine) serve(c *conn) {
	var ids []uint32
	defer func() {
		c.Close()
		e.mu.Lock()
		delete(e.conns, c.Conn)
		for _, id := range ids {
			delete(e.active, id)
		}
		e.mu.Unlock()
	}()

	type stream struct {
		source, target string
		captioned      bool
	}
	streams := make(map[uint32]*stream)
	reader := bufio.NewReader(c)

	if !c.send(protocol.Frame{Type: protocol.TypeHello, Version: protocol.Version, Text: "fake"}) {
		return
	}
	for {
		frame, err := protocol.Read(reader)
		if err != nil {
			return
		}
		id := frame.Stream
		ok := true
		switch frame.Type {
		case protocol.TypeOpen:
			e.mu.Lock()
			refused := frame.Target == "zz" || e.refuse[frame.Target]
			e.mu.Unlock()
			if refused {
				ok = c.send(
					protocol.Frame{Type: protocol.TypeError, Stream: id, Text: "unsupported language: " + frame.Target},
					protocol.Frame{Type: protocol.TypeClosed, Stream: id},
				)
				break
			}
			streams[id] = &stream{source: frame.Source, target: frame.Target}
			ids = append(ids, id)
			e.mu.Lock()
			e.opened++
			e.active[id] = [2]string{frame.Source, frame.Target}
			if frame.Early {
				e.early++
			}
			e.mu.Unlock()
			ok = c.send(protocol.Frame{Type: protocol.TypeStats, Text: fmt.Sprintf(`{"streams":%d}`, len(streams))})
		case protocol.TypeAudio:
			s := streams[id]
			if s == nil {
				break
			}
			if !s.captioned {
				s.captioned = true
				ok = c.send(
					protocol.Frame{Type: protocol.TypeCaption, Stream: id, Kind: protocol.CaptionSourcePartial, Text: "heard"},
					protocol.Frame{Type: protocol.TypeCaption, Stream: id, Kind: protocol.CaptionSource, Text: "heard " + s.source},
					protocol.Frame{Type: protocol.TypeCaption, Stream: id, Kind: protocol.CaptionTranslation, Text: "said " + s.target},
				)
			}
			ok = ok && c.send(protocol.Frame{Type: protocol.TypeSpeech, Stream: id, Packet: frame.Packet})
		case protocol.TypeClose:
			if _, found := streams[id]; found {
				e.mu.Lock()
				e.closed++
				holding := e.hold
				delete(e.active, id)
				if holding {
					e.zombies = append(e.zombies, zombie{conn: c, id: id, source: streams[id].source, target: streams[id].target})
				}
				e.mu.Unlock()
				if !holding {
					delete(streams, id)
					ok = c.send(protocol.Frame{Type: protocol.TypeClosed, Stream: id})
				}
			}
		}
		if !ok {
			return
		}
	}
}
