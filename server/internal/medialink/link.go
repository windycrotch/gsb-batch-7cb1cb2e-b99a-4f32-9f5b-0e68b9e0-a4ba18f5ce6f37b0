// Package medialink keeps a multiplexed connection to the media engine alive.
package medialink

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/whispem/ambivox/server/internal/protocol"
)

const (
	outputQueue    = 4096
	dialTimeout    = 2 * time.Second
	writeTimeout   = 5 * time.Second
	minBackoff     = 100 * time.Millisecond
	maxBackoff     = 3 * time.Second
	handshakeLimit = 5 * time.Second
)

var ErrDisconnected = errors.New("medialink: media engine is not connected")

// Handler receives everything the engine produces for one stream. Calls come
// from a single goroutine and must not block.
type Handler interface {
	OnSpeech(packet []byte)
	OnCaption(kind protocol.CaptionKind, utterance uint32, text string)
	OnError(message string)
	OnClosed()
}

type Link struct {
	path string
	log  *slog.Logger

	mu       sync.Mutex
	session  *session
	onChange []func(connected bool)

	nextID  atomic.Uint32
	stats   atomic.Pointer[map[string]float64]
	dropped atomic.Uint64
	done    chan struct{}
}

type session struct {
	conn    net.Conn
	out     chan protocol.Frame
	backend string
	streams map[uint32]Handler
	closed  chan struct{}
}

type Stream struct {
	id      uint32
	link    *Link
	session *session
	once    sync.Once
}

func Dial(ctx context.Context, path string, log *slog.Logger) *Link {
	l := &Link{path: path, log: log, done: make(chan struct{})}
	go l.run(ctx)
	return l
}

func (l *Link) Done() <-chan struct{} { return l.done }

func (l *Link) OnChange(fn func(connected bool)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.onChange = append(l.onChange, fn)
}

func (l *Link) Connected() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.session != nil
}

func (l *Link) Backend() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.session == nil {
		return ""
	}
	return l.session.backend
}

func (l *Link) Stats() map[string]float64 {
	if stats := l.stats.Load(); stats != nil {
		return *stats
	}
	return nil
}

func (l *Link) Dropped() uint64 { return l.dropped.Load() }

func (l *Link) Open(source, target string, early bool, handler Handler) (*Stream, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.session
	if s == nil {
		return nil, ErrDisconnected
	}
	id := l.nextID.Add(1)
	s.streams[id] = handler
	frame := protocol.Frame{Type: protocol.TypeOpen, Stream: id, Source: source, Target: target, Early: early}
	select {
	case s.out <- frame:
	default:
		delete(s.streams, id)
		return nil, fmt.Errorf("medialink: output queue is full")
	}
	return &Stream{id: id, link: l, session: s}, nil
}

func (s *Stream) Write(sequence uint16, packet []byte) {
	frame := protocol.Frame{Type: protocol.TypeAudio, Stream: s.id, Sequence: sequence, Packet: packet}
	select {
	case s.session.out <- frame:
	case <-s.session.closed:
	default:
		s.link.dropped.Add(1)
	}
}

// Close asks the engine to finish the stream; OnClosed follows once the
// remaining speech has been delivered.
func (s *Stream) Close() {
	s.once.Do(func() {
		select {
		case s.session.out <- protocol.Frame{Type: protocol.TypeClose, Stream: s.id}:
		case <-s.session.closed:
		case <-time.After(writeTimeout):
		}
	})
}

func (l *Link) run(ctx context.Context) {
	defer close(l.done)
	backoff := minBackoff
	for ctx.Err() == nil {
		started := time.Now()
		err := l.connect(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) > maxBackoff {
			backoff = minBackoff
		}
		l.log.Warn("media engine unavailable", "error", err, "retry_in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

func (l *Link) connect(ctx context.Context) error {
	dialer := net.Dialer{Timeout: dialTimeout}
	conn, err := dialer.DialContext(ctx, "unix", l.path)
	if err != nil {
		return err
	}
	defer conn.Close()
	reader := bufio.NewReaderSize(conn, 64*1024)

	_ = conn.SetReadDeadline(time.Now().Add(handshakeLimit))
	hello, err := protocol.Read(reader)
	if err != nil {
		return fmt.Errorf("handshake: %w", err)
	}
	if hello.Type != protocol.TypeHello || hello.Version != protocol.Version {
		return fmt.Errorf("handshake: unexpected frame type 0x%02x version %d", uint8(hello.Type), hello.Version)
	}
	_ = conn.SetReadDeadline(time.Time{})

	s := &session{
		conn:    conn,
		out:     make(chan protocol.Frame, outputQueue),
		backend: hello.Text,
		streams: make(map[uint32]Handler),
		closed:  make(chan struct{}),
	}
	l.mu.Lock()
	l.session = s
	listeners := append([]func(bool){}, l.onChange...)
	l.mu.Unlock()
	l.log.Info("media engine connected", "backend", s.backend)
	for _, fn := range listeners {
		fn(true)
	}

	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	writeErr := make(chan error, 1)
	go func() { writeErr <- l.write(s) }()

	err = l.read(s, reader)

	l.mu.Lock()
	l.session = nil
	orphans := s.streams
	s.streams = nil
	listeners = append([]func(bool){}, l.onChange...)
	l.mu.Unlock()
	close(s.closed)
	conn.Close()
	if werr := <-writeErr; err == nil {
		err = werr
	}
	for _, handler := range orphans {
		handler.OnClosed()
	}
	for _, fn := range listeners {
		fn(false)
	}
	return err
}

func (l *Link) write(s *session) error {
	writer := bufio.NewWriterSize(s.conn, 64*1024)
	buf := make([]byte, 0, 2048)
	for {
		var frame protocol.Frame
		select {
		case frame = <-s.out:
		case <-s.closed:
			return nil
		}
		for {
			var err error
			if buf, err = frame.Append(buf[:0]); err != nil {
				l.log.Warn("dropping unencodable frame", "error", err)
			} else {
				_ = s.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
				if _, err = writer.Write(buf); err != nil {
					s.conn.Close()
					return err
				}
			}
			select {
			case frame = <-s.out:
				continue
			default:
			}
			break
		}
		if err := writer.Flush(); err != nil {
			s.conn.Close()
			return err
		}
	}
}

func (l *Link) read(s *session, reader *bufio.Reader) error {
	for {
		frame, err := protocol.Read(reader)
		if err != nil {
			return err
		}
		if frame.Type == protocol.TypeStats {
			var stats map[string]float64
			if json.Unmarshal([]byte(frame.Text), &stats) == nil {
				l.stats.Store(&stats)
			}
			continue
		}

		l.mu.Lock()
		handler := s.streams[frame.Stream]
		if frame.Type == protocol.TypeClosed {
			delete(s.streams, frame.Stream)
		}
		l.mu.Unlock()
		if handler == nil {
			continue
		}

		switch frame.Type {
		case protocol.TypeSpeech:
			handler.OnSpeech(frame.Packet)
		case protocol.TypeCaption:
			handler.OnCaption(frame.Kind, frame.Utterance, frame.Text)
		case protocol.TypeError:
			handler.OnError(frame.Text)
		case protocol.TypeClosed:
			handler.OnClosed()
		default:
			return fmt.Errorf("medialink: unexpected frame type 0x%02x from engine", uint8(frame.Type))
		}
	}
}
