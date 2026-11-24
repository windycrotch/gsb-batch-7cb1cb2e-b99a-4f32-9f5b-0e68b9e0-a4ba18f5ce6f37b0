package relay

import (
	"log"

	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/voc/srtrelay/internal/metrics"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const relaySubsystem = "relay"

var (
	activeClients = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: prometheus.BuildFQName(metrics.Namespace, relaySubsystem, "active_clients"),
			Help: "The number of active clients per channel",
		},
		[]string{"channel_name"},
	)
	channelCreatedTimestamp = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: prometheus.BuildFQName(metrics.Namespace, relaySubsystem, "created_timestamp_seconds"),
			Help: "The UNIX timestamp when the channel was created",
		},
		[]string{"channel_name"},
	)
)

type UnsubscribeFunc func()

// Channel is one epoch of a stream name. It fans out buffers from a single
// publisher to its subscribers. A channel is closed exactly once, after which
// no subscriber can be added and every existing subscriber observes a closed
// upstream.
//
// All state transitions (Pub, Sub, unsubscribe and Close) are serialised by
// mutex. Because buffers are only ever handed over while holding that lock, and
// Close drains and recycles what is still queued, a buffer can neither be
// recycled while a reader still uses it nor leak when its subscriber goes away.
type Channel struct {
	name       string
	mutex      sync.Mutex
	subs       Subs
	maxPackets uint
	closed     chan struct{}
	closeOnce  sync.Once

	// statistics
	clients atomic.Value
	created time.Time

	// Prometheus metrics.
	activeClients    prometheus.Gauge
	createdTimestamp prometheus.Gauge
}
type Subs []*Subscriber

type Stats struct {
	clients int
	created time.Time
}

// Remove removes a single subscriber from the slice and returns the result.
// It is the backwards-compatible wrapper around remove, which additionally
// reports whether the subscriber was actually present.
func (subs Subs) Remove(sub chan *Buffer) Subs {
	remaining, _ := subs.remove(sub)
	return remaining
}

// remove removes a single subscriber.
//
// The subscriber channel is drained and recycled before it is closed, so no
// buffer stays referenced by a subscriber that is going away. The second return
// value reports whether the subscriber was registered at all, which lets callers
// keep client accounting exact when they call remove more than once. The caller
// has to hold the channel mutex.
func (subs Subs) remove(sub chan *Buffer) (Subs, bool) {
	idx := -1
	for i := range subs {
		if subs[i].ch == sub {
			idx = i
			break
		}
	}

	// subscriber was already removed
	if idx < 0 {
		return subs, false
	}

	for {
		select {
		case buf, ok := <-sub:
			if !ok {
				// Already closed, the subscriber is just no longer registered.
				return slices.Delete(subs, idx, idx+1), true
			}
			if buf != nil {
				buf.Release()
			}
		default:
			close(sub)
			return slices.Delete(subs, idx, idx+1), true
		}
	}
}

func NewChannel(name string, maxPackets uint) *Channel {
	channelActiveClients := activeClients.WithLabelValues(name)
	ch := &Channel{
		name:          name,
		subs:          make(Subs, 0, 10),
		maxPackets:    maxPackets,
		created:       time.Now(),
		activeClients: channelActiveClients,
		closed:        make(chan struct{}),
	}
	ch.clients.Store(0)
	ch.createdTimestamp = channelCreatedTimestamp.WithLabelValues(name)
	ch.createdTimestamp.Set(float64(ch.created.UnixNano()) / 1000000.0)
	return ch
}

// Sub subscribes to a channel. Subscribing to an already closed channel yields
// a subscriber that immediately reports a closed upstream.
func (ch *Channel) Sub() (*Subscriber, UnsubscribeFunc) {
	ch.mutex.Lock()
	defer ch.mutex.Unlock()

	sub := &Subscriber{
		ch:         make(chan *Buffer, ch.maxPackets),
		chanClosed: ch.closed,
	}
	if ch.subs == nil {
		// Channel is already closed, hand out an already finished subscriber.
		close(sub.ch)
		return sub, func() {}
	}

	ch.subs = append(ch.subs, sub)
	ch.clients.Store(len(ch.subs))
	ch.activeClients.Inc()

	var unsub UnsubscribeFunc = func() {
		ch.mutex.Lock()
		defer ch.mutex.Unlock()

		// Channel already closed, sub was removed and counted already.
		if ch.subs == nil {
			return
		}

		var removed bool
		ch.subs, removed = ch.subs.remove(sub.ch)
		if !removed {
			return
		}
		ch.clients.Store(len(ch.subs))
		ch.activeClients.Dec()
	}
	return sub, unsub
}

// Pub publishes a packet to a channel. The buffer reference passed in is
// consumed: it is released once every subscriber has released it, or
// immediately if there are no subscribers.
func (ch *Channel) Pub(b *Buffer) {
	if b == nil {
		return
	}

	ch.mutex.Lock()
	defer ch.mutex.Unlock()

	toRemove := make(Subs, 0, 5)
	for i := range ch.subs {
		b.Retain()
		select {
		case ch.subs[i].ch <- b:
			continue

		// Remember overflowed chans for drop
		default:
			b.Release()
			toRemove = append(toRemove, ch.subs[i])
			log.Println("dropping overflowing client", i)
		}
	}
	for _, sub := range toRemove {
		var removed bool
		ch.subs, removed = ch.subs.remove(sub.ch)
		if removed {
			ch.activeClients.Dec()
		}
	}
	ch.clients.Store(len(ch.subs))
	b.Release()
}

// Close closes a channel. It is idempotent.
func (ch *Channel) Close() {
	ch.closeOnce.Do(func() {
		ch.mutex.Lock()
		defer ch.mutex.Unlock()

		close(ch.closed)
		// Remove subscribers explicitly to return queued buffers to the pool.
		// The active client gauge is deleted below, so per-subscriber decrements
		// are not needed here.
		for len(ch.subs) > 0 {
			ch.subs, _ = ch.subs.remove(ch.subs[0].ch)
		}
		ch.subs = nil
		ch.clients.Store(0)
		activeClients.DeleteLabelValues(ch.name)
		channelCreatedTimestamp.DeleteLabelValues(ch.name)
	})
}

func (ch *Channel) Stats() Stats {
	return Stats{
		clients: ch.clients.Load().(int),
		created: ch.created,
	}
}
