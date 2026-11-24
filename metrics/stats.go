/* Copyright (c) 2017 Gregor Riepl
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package metrics

import (
	"github.com/onitake/restreamer/protocol"
	"github.com/onitake/restreamer/util"
	"sync"
	"sync/atomic"
	"time"
)

// Collector is the public face of a statistics collector.
// It is implemented by the individual stream stats.
type Collector interface {
	// ConnectionAdded notifies that a new downstream client connected.
	ConnectionAdded()
	// ConnectionRemoved notifies that a downstream client disconnected.
	ConnectionRemoved()
	// PacketReceived notifies that a packet was received.
	// TODO pass the endpoint here
	PacketReceived()
	// PacketSent notifies that a packet was sent.
	// TODO pass the endpoint here
	PacketSent()
	// PacketDropped notifies that a packet was dropped.
	// TODO pass the endpoint here
	PacketDropped()
	// SourceConnected notifies that upstream is live.
	SourceConnected()
	// SourceDisconnected notifies that upstream is offline.
	SourceDisconnected()
	// IsUpstreamConnected tells you if upstream is connected.
	IsUpstreamConnected() bool
	// StreamDuration reports how long a downstream connection was up
	StreamDuration(duration time.Duration)
}

// realCollector represents per-stream state information
// and is continuously updated by the corresponding streamer.
// Use the provided accessor methods for this purpose.
type realCollector struct {
	// total number of connections
	connections int64
	// total number of received packets
	packetsReceived uint64
	// total number of sent packets
	packetsSent uint64
	// total number of dropped packets
	packetsDropped uint64
	// total streaming duration
	duration int64
	// upstream connection state
	// NOTE AtomicBool is a 32-bit type and must listed be after 64-bit fields
	// to avoid crashes due to misalignment!
	connected util.AtomicBool
}

func (stats *realCollector) ConnectionAdded() {
	atomic.AddInt64(&stats.connections, 1)
}

func (stats *realCollector) ConnectionRemoved() {
	atomic.AddInt64(&stats.connections, -1)
}

func (stats *realCollector) PacketReceived() {
	atomic.AddUint64(&stats.packetsReceived, 1)
}

func (stats *realCollector) PacketSent() {
	atomic.AddUint64(&stats.packetsSent, 1)
}

func (stats *realCollector) PacketDropped() {
	atomic.AddUint64(&stats.packetsDropped, 1)
}

func (stats *realCollector) SourceConnected() {
	util.StoreBool(&stats.connected, true)
}

func (stats *realCollector) SourceDisconnected() {
	util.StoreBool(&stats.connected, false)
}

func (stats *realCollector) IsUpstreamConnected() bool {
	return util.LoadBool(&stats.connected)
}

func (stats *realCollector) StreamDuration(duration time.Duration) {
	atomic.AddInt64(&stats.duration, int64(duration))
}

// clone creates a copy of the stats object - useful for
// storing state temporarily.
func (stats *realCollector) clone() *realCollector {
	return &realCollector{
		connections:     atomic.LoadInt64(&stats.connections),
		packetsReceived: atomic.LoadUint64(&stats.packetsReceived),
		packetsSent:     atomic.LoadUint64(&stats.packetsSent),
		packetsDropped:  atomic.LoadUint64(&stats.packetsDropped),
		connected:       util.ToAtomicBool(util.LoadBool(&stats.connected)),
		duration:        atomic.LoadInt64(&stats.duration),
	}
}

// invsub subtracts this stats object from another and sets each
// value to the difference. Note: Should not be used on atomic values
// directly. clone() first.
// "connected" is copied directly from "to".
// Useful if you want to calculate a delta, then replace the previous
// value with the current one:
// prev := realCollector{}
//
//	for {
//	  current := realCollector{...current values...}
//	  prev.invsub(current)
//	  doSomethingWithPrev(prev)
//	  prev = current
//	}
func (stats *realCollector) invsub(to *realCollector) {
	stats.connections = to.connections - stats.connections
	stats.packetsReceived = to.packetsReceived - stats.packetsReceived
	stats.packetsSent = to.packetsSent - stats.packetsSent
	stats.packetsDropped = to.packetsDropped - stats.packetsDropped
	stats.connected = to.connected
	stats.duration = to.duration - stats.duration
}

// StreamStatistics is the current state of a single stream
// or all streams combined.
type StreamStatistics struct {
	Connections              int64
	MaxConnections           int64
	FullConnections          int64
	TotalPacketsReceived     uint64
	TotalPacketsSent         uint64
	TotalPacketsDropped      uint64
	TotalBytesReceived       uint64
	TotalBytesSent           uint64
	TotalBytesDropped        uint64
	TotalStreamTime          int64
	PacketsPerSecondReceived uint64
	PacketsPerSecondSent     uint64
	PacketsPerSecondDropped  uint64
	BytesPerSecondReceived   uint64
	BytesPerSecondSent       uint64
	BytesPerSecondDropped    uint64
	Connected                bool
}

// Statistics is the access interface for a stat tracker.
// Streams update their state continuously, but data fields are only updated in periodic intervals.
// There is also an HTTP/JSON API facility available through the New...Api() methods in api.go.
type Statistics interface {
	// Start starts the updater thread.
	Start()
	// Stop stops the updater thread.
	Stop()
	// RegisterStream adds a new stream to the map.
	// The name will be used as the lookup key.
	RegisterStream(name string) Collector
	// RemoveStream removes a stream from the map.
	RemoveStream(name string)
	// GetStreamStatistics fetches the statistics for a stream.
	// The returned object is a copy does not need to be handled with care.
	GetStreamStatistics(name string) *StreamStatistics
	// GetAllStreamStatistics fetches the statistics for all streams.
	// The returned object is a copy does not need to be handled with care.
	GetAllStreamStatistics() map[string]*StreamStatistics
	// GetGlobalStatistics fetches the global statistics.
	// The returned object is a copy does not need to be handled with care.
	GetGlobalStatistics() *StreamStatistics
}

// realStatistics implements a full statistics collector and API endpoint generator.
type realStatistics struct {
	lock     sync.RWMutex
	lifeLock sync.Mutex
	stop     chan struct{}
	done     chan struct{}
	internal map[string]*realCollector
	streams  map[string]*StreamStatistics
	global   *StreamStatistics
}

// NewStatistics creates a new statistics container.
// You can start and stop the periodic updater using Start() and Stop().
// Register your streams with RegisterStream(), this will return an updateable
// statistics object. You should not write to the individual fields directly,
// instead access them using the Add...() methods.
// Snapshots of the aggregated statistics can then be fetched with the Get...() methods.
func NewStatistics(maxconns uint, fullcons uint) Statistics {
	stats := &realStatistics{
		stop:     make(chan struct{}),
		internal: make(map[string]*realCollector),
		streams:  make(map[string]*StreamStatistics),
		global: &StreamStatistics{
			MaxConnections:  int64(maxconns),
			FullConnections: int64(fullcons),
		},
	}
	return stats
}

// update updates the aggregated statistics from the current state of each stream.
func (stats *realStatistics) update(delta time.Duration, change map[string]*realCollector) {
	// acquire the global write lock
	stats.lock.Lock()

	// reset the global counters
	stats.global.Connections = 0
	stats.global.TotalPacketsReceived = 0
	stats.global.TotalPacketsSent = 0
	stats.global.TotalPacketsDropped = 0
	stats.global.TotalBytesReceived = 0
	stats.global.TotalBytesSent = 0
	stats.global.TotalBytesDropped = 0
	stats.global.TotalStreamTime = 0
	stats.global.PacketsPerSecondReceived = 0
	stats.global.PacketsPerSecondSent = 0
	stats.global.PacketsPerSecondDropped = 0
	stats.global.BytesPerSecondReceived = 0
	stats.global.BytesPerSecondSent = 0
	stats.global.BytesPerSecondDropped = 0
	stats.global.Connected = false

	// loop over all streams
	for name, stream := range stats.streams {
		diff := change[name]

		// update the stats
		stream.Connections += diff.connections
		stream.TotalPacketsReceived += diff.packetsReceived
		stream.TotalPacketsSent += diff.packetsSent
		stream.TotalPacketsDropped += diff.packetsDropped
		stream.TotalBytesReceived = stream.TotalPacketsReceived * protocol.MpegTsPacketSize
		stream.TotalBytesSent = stream.TotalPacketsSent * protocol.MpegTsPacketSize
		stream.TotalBytesDropped = stream.TotalPacketsDropped * protocol.MpegTsPacketSize
		stream.TotalStreamTime += diff.duration
		stream.PacketsPerSecondReceived = uint64(float64(diff.packetsReceived) / delta.Seconds())
		stream.PacketsPerSecondSent = uint64(float64(diff.packetsSent) / delta.Seconds())
		stream.PacketsPerSecondDropped = uint64(float64(diff.packetsDropped) / delta.Seconds())
		stream.BytesPerSecondReceived = stream.PacketsPerSecondReceived * protocol.MpegTsPacketSize
		stream.BytesPerSecondSent = stream.PacketsPerSecondSent * protocol.MpegTsPacketSize
		stream.BytesPerSecondDropped = stream.PacketsPerSecondDropped * protocol.MpegTsPacketSize
		stream.Connected = diff.connected != 0

		// update the global counters as well
		stats.global.Connections += stream.Connections
		stats.global.TotalPacketsReceived += stream.TotalPacketsReceived
		stats.global.TotalPacketsSent += stream.TotalPacketsSent
		stats.global.TotalPacketsDropped += stream.TotalPacketsDropped
		stats.global.TotalBytesReceived += stream.TotalBytesReceived
		stats.global.TotalBytesSent += stream.TotalBytesSent
		stats.global.TotalBytesDropped += stream.TotalBytesDropped
		stats.global.TotalStreamTime += stream.TotalStreamTime
		stats.global.PacketsPerSecondReceived += stream.PacketsPerSecondReceived
		stats.global.PacketsPerSecondSent += stream.PacketsPerSecondSent
		stats.global.PacketsPerSecondDropped += stream.PacketsPerSecondDropped
		stats.global.BytesPerSecondReceived += stream.BytesPerSecondReceived
		stats.global.BytesPerSecondSent += stream.BytesPerSecondSent
		stats.global.BytesPerSecondDropped += stream.BytesPerSecondDropped
		if stream.Connected {
			stats.global.Connected = true
		}
	}

	// and done
	stats.lock.Unlock()
}

// snapshotInternal clones all current per-stream collectors.
func (stats *realStatistics) snapshotInternal() map[string]*realCollector {
	stats.lock.RLock()
	previous := make(map[string]*realCollector, len(stats.internal))
	for name, stream := range stats.internal {
		previous[name] = stream.clone()
	}
	stats.lock.RUnlock()
	return previous
}

// delta calculates the difference between a previous state and the current
// state. It returns the per-stream diffs to accumulate and a fresh snapshot
// to use as the next previous state. Streams missing from the previous map
// (for example registered after Start) are treated as zero state, so this
// never dereferences a nil entry.
func (stats *realStatistics) delta(previous map[string]*realCollector) (map[string]*realCollector, map[string]*realCollector) {
	stats.lock.RLock()
	diffs := make(map[string]*realCollector, len(stats.internal))
	current := make(map[string]*realCollector, len(stats.internal))
	for name, stream := range stats.internal {
		update := stream.clone()
		current[name] = update
		old := previous[name]
		if old == nil {
			old = &realCollector{}
		}
		diffs[name] = &realCollector{
			connections:     update.connections - old.connections,
			packetsReceived: update.packetsReceived - old.packetsReceived,
			packetsSent:     update.packetsSent - old.packetsSent,
			packetsDropped:  update.packetsDropped - old.packetsDropped,
			connected:       update.connected,
			duration:        update.duration - old.duration,
		}
	}
	stats.lock.RUnlock()
	return diffs, current
}

// loop runs a ticker to update all statistics periodically.
func (stats *realStatistics) loop(stop <-chan struct{}, done chan struct{}) {
	defer close(done)

	// TODO make the interval configurable
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	// pre-init - store the current time and state
	before := time.Now()
	previous := stats.snapshotInternal()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			// calculate the elapsed time
			now := time.Now()
			// calculate the state delta and update the stored state
			diff, current := stats.delta(previous)
			// and update
			stats.update(now.Sub(before), diff)
			// stash the current time and state
			before = now
			previous = current
		}
	}
}

// Start starts the updater thread.
func (stats *realStatistics) Start() {
	stats.lifeLock.Lock()
	defer stats.lifeLock.Unlock()
	if stats.done != nil {
		return
	}
	// a fresh stop channel per generation also makes Stop/Start cycles work
	stop := make(chan struct{})
	done := make(chan struct{})
	stats.stop = stop
	stats.done = done
	go stats.loop(stop, done)
}

// Stop stops the updater thread. It is idempotent and waits for the loop to exit.
func (stats *realStatistics) Stop() {
	stats.lifeLock.Lock()
	stop := stats.stop
	done := stats.done
	stats.stop = nil
	stats.done = nil
	stats.lifeLock.Unlock()
	if done == nil {
		return
	}
	close(stop)
	<-done
}

// RegisterStream adds a new stream to the map.
// The name will be used as the lookup key.
func (stats *realStatistics) RegisterStream(name string) Collector {
	current := &realCollector{}
	stats.lock.Lock()
	stats.internal[name] = current
	stats.streams[name] = &StreamStatistics{}
	stats.lock.Unlock()
	return current
}

// RemoveStream removes a stream from the map.
func (stats *realStatistics) RemoveStream(name string) {
	stats.lock.Lock()
	delete(stats.internal, name)
	delete(stats.streams, name)
	stats.lock.Unlock()
}

// GetStreamStatistics fetches the statistics for a stream.
// The returned object is a copy does not need to be handled with care.
// A missing stream returns a zero-valued statistics object.
func (stats *realStatistics) GetStreamStatistics(name string) *StreamStatistics {
	stats.lock.RLock()
	var stream StreamStatistics
	if stored, ok := stats.streams[name]; ok && stored != nil {
		stream = *stored
	}
	collector := stats.internal[name]
	stats.lock.RUnlock()
	// report the authoritative live connection gauge immediately instead of
	// waiting for the next periodic aggregation tick
	if collector != nil {
		stream.Connections = atomic.LoadInt64(&collector.connections)
	}
	return &stream
}

// GetAllStreamStatistics fetches the statistics for all streams.
// The returned object is a copy does not need to be handled with care.
func (stats *realStatistics) GetAllStreamStatistics() map[string]*StreamStatistics {
	stats.lock.RLock()
	streams := make(map[string]*StreamStatistics, len(stats.streams))
	for name, stream := range stats.streams {
		scopy := *stream
		streams[name] = &scopy
	}
	stats.lock.RUnlock()
	return streams
}

// GetGlobalStatistics fetches the global statistics.
// The returned object is a copy does not need to be handled with care.
func (stats *realStatistics) GetGlobalStatistics() *StreamStatistics {
	stats.lock.RLock()
	global := *stats.global
	// Sum the authoritative live connection gauges so the health/stats APIs
	// reflect the real online count immediately, not only after the next
	// periodic aggregation tick.
	var connections int64
	for _, collector := range stats.internal {
		connections += atomic.LoadInt64(&collector.connections)
	}
	stats.lock.RUnlock()
	global.Connections = connections
	return &global
}

// DummyStatistics is placeholder for a real stats handler.
type DummyStatistics struct {
}

func (stats *DummyStatistics) Start() {
}

func (stats *DummyStatistics) Stop() {
}

func (stats *DummyStatistics) RegisterStream(name string) Collector {
	return &DummyCollector{}
}

func (stats *DummyStatistics) RemoveStream(name string) {
}

func (stats *DummyStatistics) GetStreamStatistics(name string) *StreamStatistics {
	return &StreamStatistics{}
}

func (stats *DummyStatistics) GetAllStreamStatistics() map[string]*StreamStatistics {
	return make(map[string]*StreamStatistics)
}

func (stats *DummyStatistics) GetGlobalStatistics() *StreamStatistics {
	return &StreamStatistics{}
}

// DummyCollector is placeholder for a real stats collector.
type DummyCollector struct {
}

func (stats *DummyCollector) ConnectionAdded() {
}

func (stats *DummyCollector) ConnectionRemoved() {
}

func (stats *DummyCollector) PacketReceived() {
}

func (stats *DummyCollector) PacketSent() {
}

func (stats *DummyCollector) PacketDropped() {
}

func (stats *DummyCollector) SourceConnected() {
}

func (stats *DummyCollector) SourceDisconnected() {
}

func (stats *DummyCollector) IsUpstreamConnected() bool {
	return false
}

func (stats *DummyCollector) StreamDuration(duration time.Duration) {
}
