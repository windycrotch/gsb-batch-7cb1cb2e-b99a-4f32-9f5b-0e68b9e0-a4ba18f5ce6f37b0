package relay

import (
	"errors"
	"sync"
	"time"
)

var (
	ErrStreamAlreadyExists = errors.New("stream already exists")
	ErrStreamNotExisting   = errors.New("stream does not exist")
)

type RelayConfig struct {
	BufferSize uint
	PacketSize uint
}

type StreamStatistics struct {
	Name    string    `json:"name"`
	URL     string    `json:"url"`
	Clients int       `json:"clients"`
	Created time.Time `json:"created"`
}

// Publisher represents an active publishing session for one stream name.
//
// A publisher owns exactly one Channel, i.e. one epoch of the stream name.
// Retiring a publisher is synchronous and happens under the relay lock: as
// soon as it returns, the name can be claimed by a new publisher and every
// subscriber of the retired epoch observes a closed upstream. Subscribers of a
// newer epoch can never receive data written by an older one.
type Publisher struct {
	relay   *Relay
	name    string
	channel *Channel

	// input is only set for publishers created through Relay.Publish. It is the
	// channel drained by the forwarding goroutine and allows a later Publish
	// call to detect that the previous publisher has gone away.
	input chan *Buffer

	// closeOnce guarantees that the underlying channel is closed exactly once,
	// no matter whether retirement happens through Close, a takeover by a new
	// publisher, or reaping of a closed input channel.
	closeOnce sync.Once
}

// Name returns the stream name of the publishing session.
func (p *Publisher) Name() string {
	return p.name
}

// Write publishes buf to all subscribers of the current epoch.
//
// Write takes ownership of buf: it is released once every subscriber has
// released its reference, or immediately if there are none. Writing to an
// already retired publisher is safe and just recycles the buffer.
func (p *Publisher) Write(buf *Buffer) {
	if buf == nil {
		return
	}
	p.channel.Pub(buf)
}

// Close ends the publishing session. It is idempotent and synchronous: after
// Close returns the stream name is free for a new publisher, and every
// subscriber of this epoch observes a closed upstream.
func (p *Publisher) Close() {
	p.relay.mutex.Lock()
	defer p.relay.mutex.Unlock()
	p.relay.retireLocked(p.name, p)
}

// Relay represents a multi-channel stream relay
type Relay struct {
	mutex    sync.Mutex
	channels map[string]*Publisher
	config   *RelayConfig
	pool     *BufferPool
}

// NewRelay creates a relay
func NewRelay(config *RelayConfig) *Relay {
	if config.BufferSize == 0 {
		config.BufferSize = 384000 // 1s @ 3Mbits/s
	}
	if config.PacketSize == 0 {
		config.PacketSize = 1316 // default packet size
	}
	return &Relay{
		channels: make(map[string]*Publisher),
		config:   config,
		pool:     NewBufferPool(config.PacketSize),
	}
}

func (s *Relay) AcquireBuffer() *Buffer {
	return s.pool.Get()
}

// BuffersInUse returns the number of acquired buffers that have not been
// released yet. It reaches zero once all publishers and subscribers are gone.
func (s *Relay) BuffersInUse() int64 {
	return s.pool.InUse()
}

// NewPublisher claims a stream name and returns a handle for publishing to it.
//
// If the name is still claimed by a publisher that has already been closed, the
// stale claim is retired first so that a new publisher can take the name over
// immediately. If the name is claimed by a live publisher,
// ErrStreamAlreadyExists is returned.
func (s *Relay) NewPublisher(name string) (*Publisher, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return s.acquireLocked(name)
}

// Publish claims a stream name for publishing and returns the channel that
// buffers have to be sent to. Closing the returned channel ends the session.
//
// This is a channel-based convenience wrapper around NewPublisher. Closing the
// input releases the name asynchronously, but a later Publish call detects the
// closed input synchronously and takes the name over right away, so callers
// never have to wait for the forwarding goroutine.
func (s *Relay) Publish(name string) (chan<- *Buffer, error) {
	s.mutex.Lock()
	publisher, err := s.acquireLocked(name)
	if err != nil {
		s.mutex.Unlock()
		return nil, err
	}
	input := make(chan *Buffer)
	publisher.input = input
	s.mutex.Unlock()

	go func() {
		for buf := range input {
			// nil is only used as a liveness probe by acquireLocked /
			// reapLocked and is never a real buffer.
			publisher.Write(buf)
		}
		publisher.Close()
	}()
	return input, nil
}

// acquireLocked claims name for a new publisher. The caller holds s.mutex.
//
// If the name is currently claimed, the existing publisher is inspected: when
// its input channel is already closed the stale epoch is retired and the name
// is handed over, otherwise ErrStreamAlreadyExists is returned.
func (s *Relay) acquireLocked(name string) (*Publisher, error) {
	if old, ok := s.channels[name]; ok {
		if old.input == nil || !publisherInputClosed(old.input) {
			return nil, ErrStreamAlreadyExists
		}
		s.retireLocked(name, old)
	}

	channel := NewChannel(name, s.config.BufferSize/s.config.PacketSize)
	publisher := &Publisher{
		relay:   s,
		name:    name,
		channel: channel,
	}
	s.channels[name] = publisher
	return publisher, nil
}

// retireLocked removes publisher from the stream table (if it is still the
// current owner of the name) and closes its channel. The caller holds s.mutex,
// which serialises retirement against Subscribe, so no subscriber can attach to
// a channel while it is being torn down.
func (s *Relay) retireLocked(name string, publisher *Publisher) {
	if s.channels[name] == publisher {
		delete(s.channels, name)
	}
	publisher.closeOnce.Do(func() {
		publisher.channel.Close()
	})
}

// reapLocked retires publishers whose input channel has been closed but whose
// forwarding goroutine has not retired them yet. This keeps statistics from
// reporting streams that have already ended. The caller holds s.mutex.
func (s *Relay) reapLocked() {
	for name, publisher := range s.channels {
		if publisher.input != nil && publisherInputClosed(publisher.input) {
			s.retireLocked(name, publisher)
		}
	}
}

// isStaleLocked reports whether the publisher of name has already closed its
// input channel, i.e. the stream is gone even though the forwarding goroutine
// has not retired it yet. The caller holds s.mutex.
func (s *Relay) isStaleLocked(name string, publisher *Publisher) bool {
	if publisher.input == nil || !publisherInputClosed(publisher.input) {
		return false
	}
	s.retireLocked(name, publisher)
	return true
}

// publisherInputClosed reports whether a publisher has closed its input
// channel. Sending nil is a no-op for the forwarding goroutine, while a send on
// a closed channel panics before the value can be delivered. This makes closure
// observable synchronously instead of racing the forwarding goroutine.
func publisherInputClosed(input chan<- *Buffer) (closed bool) {
	defer func() {
		if recover() != nil {
			closed = true
		}
	}()
	select {
	case input <- nil:
	default:
	}
	return false
}

type Subscriber struct {
	ch         chan *Buffer
	chanClosed <-chan struct{}
}

var (
	errUpstreamClosed = errors.New("upstream closed")
	errLateSubscriber = errors.New("late subscriber")
)

// Read returns the next buffer for this subscriber.
//
// A closed upstream is reported before any still queued buffer so that readers
// finish promptly when their stream ends; queued buffers are recycled by the
// channel on close and are never handed out.
func (s *Subscriber) Read() (*Buffer, error) {
	select {
	case <-s.chanClosed:
		return nil, errUpstreamClosed
	default:
	}
	select {
	case <-s.chanClosed:
		return nil, errUpstreamClosed
	case buf, ok := <-s.ch:
		if !ok {
			return nil, errLateSubscriber
		}
		return buf, nil
	}
}

// Subscribe subscribes to a stream by name
func (s *Relay) Subscribe(name string) (*Subscriber, UnsubscribeFunc, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	publisher, ok := s.channels[name]
	if !ok {
		return nil, nil, ErrStreamNotExisting
	}
	// The publisher has closed its input but the forwarding goroutine has not
	// retired the channel yet: the stream is already gone.
	if s.isStaleLocked(name, publisher) {
		return nil, nil, ErrStreamNotExisting
	}

	ch, unsub := publisher.channel.Sub()
	return ch, unsub, nil
}

func (s *Relay) GetStatistics() []*StreamStatistics {
	statistics := make([]*StreamStatistics, 0)

	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.reapLocked()

	for name, publisher := range s.channels {
		stats := publisher.channel.Stats()
		statistics = append(statistics, &StreamStatistics{
			Name:    name,
			Clients: stats.clients,
			Created: stats.created,
		})
	}
	return statistics
}

func (s *Relay) ChannelExists(name string) bool {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.reapLocked()
	_, exists := s.channels[name]
	return exists
}
