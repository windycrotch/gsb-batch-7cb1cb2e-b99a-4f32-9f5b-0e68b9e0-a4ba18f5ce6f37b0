// Package medialinktest provides an in-process stand-in for the media engine.
package medialinktest

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/whispem/ambivox/server/internal/protocol"
)

// Engine echoes every audio packet back as speech, captions the first packet
// of each stream (a live draft, then the final pair) and refuses streams whose
// target language is "zz".
type Engine struct {
	Path string

	mu       sync.Mutex
	listener net.Listener
	conns    map[net.Conn]struct{}
	opened   int
	early    int
	wg       sync.WaitGroup
}

func Start(t testing.TB) *Engine {
	t.Helper()
	dir, err := os.MkdirTemp("", "ambivox")
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{Path: filepath.Join(dir, "media.sock"), conns: make(map[net.Conn]struct{})}
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
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			e.mu.Lock()
			e.conns[conn] = struct{}{}
			e.mu.Unlock()
			e.wg.Go(func() {
				e.serve(conn)
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

func (e *Engine) serve(conn net.Conn) {
	defer func() {
		conn.Close()
		e.mu.Lock()
		delete(e.conns, conn)
		e.mu.Unlock()
	}()

	type stream struct {
		source, target string
		captioned      bool
	}
	streams := make(map[uint32]*stream)
	reader := bufio.NewReader(conn)
	send := func(frames ...protocol.Frame) bool {
		for _, frame := range frames {
			if protocol.Write(conn, frame) != nil {
				return false
			}
		}
		return true
	}

	if !send(protocol.Frame{Type: protocol.TypeHello, Version: protocol.Version, Text: "fake"}) {
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
			if frame.Target == "zz" {
				ok = send(
					protocol.Frame{Type: protocol.TypeError, Stream: id, Text: "unsupported language: zz"},
					protocol.Frame{Type: protocol.TypeClosed, Stream: id},
				)
				break
			}
			streams[id] = &stream{source: frame.Source, target: frame.Target}
			e.mu.Lock()
			e.opened++
			if frame.Early {
				e.early++
			}
			e.mu.Unlock()
			ok = send(protocol.Frame{Type: protocol.TypeStats, Text: fmt.Sprintf(`{"streams":%d}`, len(streams))})
		case protocol.TypeAudio:
			s := streams[id]
			if s == nil {
				break
			}
			if !s.captioned {
				s.captioned = true
				ok = send(
					protocol.Frame{Type: protocol.TypeCaption, Stream: id, Kind: protocol.CaptionSourcePartial, Text: "heard"},
					protocol.Frame{Type: protocol.TypeCaption, Stream: id, Kind: protocol.CaptionSource, Text: "heard " + s.source},
					protocol.Frame{Type: protocol.TypeCaption, Stream: id, Kind: protocol.CaptionTranslation, Text: "said " + s.target},
				)
			}
			ok = ok && send(protocol.Frame{Type: protocol.TypeSpeech, Stream: id, Packet: frame.Packet})
		case protocol.TypeClose:
			if _, found := streams[id]; found {
				delete(streams, id)
				ok = send(protocol.Frame{Type: protocol.TypeClosed, Stream: id})
			}
		}
		if !ok {
			return
		}
	}
}
