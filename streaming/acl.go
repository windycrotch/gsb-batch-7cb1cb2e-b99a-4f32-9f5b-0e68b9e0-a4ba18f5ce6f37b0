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
	"fmt"
	"sync"
)

// AccessController implements a connection broker that limits
// the maximum number of concurrent connections.
type AccessController struct {
	// maxconnections is a global limit on the number of connections.
	maxconnections uint
	// lock to protect the connection counter
	lock sync.Mutex
	// connections contains the number of active connections.
	// must be accessed atomically.
	connections uint
	// inhibit is a global connection inhibitor flag.
	inhibit bool
	// streamers tracks the streamers using this controller as a broker, so a
	// direct SetInhibit on the controller can kick established connections too.
	streamers map[*Streamer]struct{}
}

// NewAccessController creates a connection broker object that
// handles access control according to the number of connected clients.
func NewAccessController(maxconnections uint) *AccessController {
	return &AccessController{
		maxconnections: maxconnections,
		streamers:      make(map[*Streamer]struct{}),
	}
}

// SetInhibit allows setting and clearing the inhibit flag.
// If it is set, no further connections are accepted, irrespective of the
// maxconnections limit. The change is also propagated to every Streamer that
// uses this controller, so established connections are kicked on inhibit.
func (control *AccessController) SetInhibit(inhibit bool) {
	// protect concurrent access
	control.lock.Lock()
	control.inhibit = inhibit
	targets := make([]*Streamer, 0, len(control.streamers))
	for streamer := range control.streamers {
		targets = append(targets, streamer)
	}
	control.lock.Unlock()

	// Propagate outside the lock: Streamer.SetInhibit synchronizes with the
	// stream loop, which may call back into Accept and need this lock.
	for _, streamer := range targets {
		if streamer != nil {
			streamer.SetInhibit(inhibit)
		}
	}
}

// Accept accepts an incoming connection when the maximum number of open connections
// has not been reached yet.
func (control *AccessController) Accept(remoteaddr string, streamer *Streamer) bool {
	accept := false
	// protect concurrent access
	control.lock.Lock()
	if streamer != nil {
		if control.streamers == nil {
			control.streamers = make(map[*Streamer]struct{})
		}
		control.streamers[streamer] = struct{}{}
	}
	// check if the limit is disabled or unreached, and no inhibit is set
	if !control.inhibit && (control.maxconnections == 0 || control.connections < control.maxconnections) {
		// and increase the counter
		control.connections++
		accept = true
	}
	// snapshot under lock for race-free logging
	connections := control.connections
	maxconnections := control.maxconnections
	inhibit := control.inhibit
	control.lock.Unlock()
	// print some info
	if accept {
		logger.Logkv(
			"event", eventAclAccepted,
			"remote", remoteaddr,
			"connections", connections,
			"max", maxconnections,
			"message", fmt.Sprintf("Accepted connection from %s, active=%d, max=%d", remoteaddr, connections, maxconnections),
		)
	} else {
		logger.Logkv(
			"event", eventAclDenied,
			"remote", remoteaddr,
			"connections", connections,
			"max", maxconnections,
			"message", fmt.Sprintf("Denied connection from %s, active=%d, max=%d, inhibit=%t", remoteaddr, connections, maxconnections, inhibit),
		)
	}
	// return the result
	return accept
}

// Release decrements the open connections count.
func (control *AccessController) Release(streamer *Streamer) {
	remove := false
	// protect concurrent access
	control.lock.Lock()
	if control.connections > 0 {
		// and decrease the counter
		control.connections--
		remove = true
	}
	// snapshot under lock for race-free logging
	connections := control.connections
	maxconnections := control.maxconnections
	control.lock.Unlock()
	if remove {
		logger.Logkv(
			"event", eventAclRemoved,
			"connections", connections,
			"max", maxconnections,
			"message", fmt.Sprintf("Removed connection, active=%d, max=%d", connections, maxconnections),
		)
	} else {
		logger.Logkv(
			"event", eventAclError,
			"error", errorAclNoConnection,
			"message", fmt.Sprintf("Error, no connection to remove"),
		)
	}
}
