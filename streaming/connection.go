/* Copyright (c) 2016-2017 Gregor Riepl
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

package streaming

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/onitake/restreamer/protocol"
)

// connectionKickDeadline is the maximum time a kicked connection is given to
// finish an in-flight write before its write deadline is force-expired.
// It bounds shutdown even for clients that are not reading.
const connectionKickDeadline = 1 * time.Second

// Connection is a single active client connection.
//
// This is meant to be called directly from a ServeHTTP handler.
// No separate thread is created.
type Connection struct {
	// Queue is the per-connection packet queue
	Queue chan protocol.MpegTsPacket
	// ClientAddress is the remote client address
	ClientAddress string
	// the destination socket
	writer http.ResponseWriter
	// Closed is true if Serve was ended because of a closed channel.
	// This is simply there to avoid a double close.
	//
	// Deprecated: all queue closes are now idempotent through CloseQueue,
	// so this field is only retained for API/observability compatibility.
	Closed bool
	// context contains the cached context object for this connection
	context context.Context

	// shutdown is closed when the connection is kicked (stream offline).
	// It may be nil for zero-value Connection structs used in unit tests.
	shutdown chan struct{}
	// closeOnce makes closing Queue idempotent and race-free.
	closeOnce sync.Once
	// kickOnce makes closing shutdown idempotent.
	kickOnce sync.Once
	// kicked is an atomic bool (int32) set before shutdown is closed.
	kicked int32
	// controller is the per-connection response controller, used to
	// unblock stuck writes when a connection is kicked.
	controller *http.ResponseController
}

// NewConnection creates a new connection object.
// To start sending data to a client, call Serve().
//
// clientaddr should point to the remote address of the connecting client
// and will be used for logging.
func NewConnection(destination http.ResponseWriter, qsize int, clientaddr string, ctx context.Context) *Connection {
	if ctx == nil {
		ctx = context.Background()
	}
	conn := &Connection{
		Queue:         make(chan protocol.MpegTsPacket, qsize),
		ClientAddress: clientaddr,
		writer:        destination,
		context:       ctx,
		shutdown:      make(chan struct{}),
		controller:    http.NewResponseController(destination),
	}
	return conn
}

// CloseQueue closes the packet queue exactly once.
//
// It is safe to call concurrently and repeatedly. Note that closing a queue
// that the streamer still distributes packets to would panic; the streamer
// removes a connection from its pool before calling this.
func (conn *Connection) CloseQueue() {
	if conn == nil || conn.Queue == nil {
		return
	}
	conn.closeOnce.Do(func() {
		close(conn.Queue)
	})
}

// Shutdown kicks the connection: Serve stops writing promptly and both the
// shutdown signal and the packet queue are closed exactly once.
//
// It is safe to call repeatedly and concurrently.
func (conn *Connection) Shutdown() {
	if conn == nil {
		return
	}
	conn.kickOnce.Do(func() {
		atomic.StoreInt32(&conn.kicked, 1)
		if conn.shutdown != nil {
			close(conn.shutdown)
		}
	})
	// Best effort: unblock a writer that is stuck sending to a slow/dead
	// client. Unsupported ResponseWriters simply ignore this.
	if conn.controller != nil {
		_ = conn.controller.SetWriteDeadline(time.Now().Add(connectionKickDeadline))
	}
	conn.CloseQueue()
}

// Kicked reports whether Shutdown was called on this connection.
func (conn *Connection) Kicked() bool {
	return conn != nil && atomic.LoadInt32(&conn.kicked) != 0
}

// shutdownChannel returns the kick channel, tolerating zero-value connections.
func (conn *Connection) shutdownChannel() <-chan struct{} {
	if conn.shutdown == nil {
		return nil
	}
	return conn.shutdown
}

// Serve starts serving data to a client, continuously feeding packets from the queue.
// An optional preamble buffer can be passed that will be sent before streaming the live payload
// (but after the HTTP response headers).
func (conn *Connection) Serve(preamble []byte) {
	defer logger.Logkv(
		"event", eventConnectionDone,
		"message", "Streaming finished",
	)
	// Clear any kick write deadline when this invocation ends, so it cannot
	// leak onto a potentially reused keep-alive connection.
	defer func() {
		if conn.controller != nil {
			_ = conn.controller.SetWriteDeadline(time.Time{})
		}
	}()
	// set the content type (important)
	conn.writer.Header().Set("Content-Type", "video/mpeg")
	// a stream is always current
	conn.writer.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
	// other headers to comply with the specs
	conn.writer.Header().Set("Accept-Range", "none")
	// suppress caching by intermediate proxies
	conn.writer.Header().Set("Cache-Control", "no-cache,no-store,no-transform")
	// use Add and Set to set more headers here
	conn.writer.WriteHeader(http.StatusOK)
	// try to flush the header
	flusher, ok := conn.writer.(http.Flusher)
	if ok {
		flusher.Flush()
	} else {
		logger.Logkv(
			"event", eventConnectionError,
			"error", errorConnectionNotFlushable,
			"message", "ResponseWriter is not flushable!",
		)
	}
	logger.Logkv(
		"event", eventHeaderSent,
		"message", "Sent header",
	)

	// send the preamble
	if len(preamble) > 0 {
		_, err := conn.writer.Write(preamble)
		if err != nil {
			logger.Logkv(
				"event", eventConnectionClosed,
				"message", "Downstream connection closed during preamble",
			)
			return
		}
	}

	ctx := conn.context
	if ctx == nil {
		ctx = context.Background()
	}
	shutdown := conn.shutdownChannel()

	for {
		// A kick always takes precedence over packets that are still
		// buffered in the queue, so an offline kick is prompt.
		select {
		case <-shutdown:
			conn.Closed = true
			logger.Logkv(
				"event", eventConnectionShutdown,
				"message", "Kicking client connection (offline)",
			)
			return
		default:
		}

		// start reading packets
		select {
		case <-shutdown:
			// kicked, stop immediately
			conn.Closed = true
			logger.Logkv(
				"event", eventConnectionShutdown,
				"message", "Shutting down client connection",
			)
			return
		case <-ctx.Done():
			// connection closed while we were waiting for more data
			conn.Closed = true
			logger.Logkv(
				"event", eventConnectionClosedWait,
				"message", "Downstream connection closed (while waiting)",
				"error", fmt.Sprintf("%v", ctx.Err()),
			)
			return
		case packet, ok := <-conn.Queue:
			if !ok {
				// channel closed, exit
				logger.Logkv(
					"event", eventConnectionShutdown,
					"message", "Shutting down client connection",
				)
				conn.Closed = true
				return
			}
			if conn.Kicked() {
				// a kick raced with a buffered packet; do not write stale data
				return
			}
			// packet received, send the packet out
			_, err := conn.writer.Write(packet)
			// NOTE we shouldn't flush here, to avoid swamping the kernel with syscalls.
			// see https://golang.org/pkg/net/http/?m=all#response.Write for details
			// on how Go buffers HTTP responses (hint: a 2KiB bufio and a 4KiB bufio)
			if err != nil {
				logger.Logkv(
					"event", eventConnectionClosed,
					"message", "Downstream connection closed",
				)
				return
			}
		}
	}

	// we cannot drain the channel here, as it might not be closed yet.
	// better let our caller handle closure and draining.
}

// ServeStreamError returns an appropriate error response to the client.
func ServeStreamError(writer http.ResponseWriter, status int) {
	// set the content type (important)
	writer.Header().Set("Content-Type", "video/mpeg")
	// a stream is always current
	writer.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
	// other headers to comply with the specs
	writer.Header().Set("Accept-Range", "none")
	// suppress caching by intermediate proxies
	writer.Header().Set("Cache-Control", "no-cache,no-store,no-transform")
	// ...and the application-supplied status code
	if status == 0 {
		status = http.StatusNotFound
	}
	writer.WriteHeader(status)
}
