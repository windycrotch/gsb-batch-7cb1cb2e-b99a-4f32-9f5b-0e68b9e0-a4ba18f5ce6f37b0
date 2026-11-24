package sfu

import (
	"fmt"
	"sort"
	"time"
)

const (
	defaultServerRetryBaseDelay            = time.Second
	defaultServerRetryMaxDelay             = 30 * time.Second
	defaultServerMaxConnectAttempts        = 5
	defaultServerConnectTimeout            = 20 * time.Second
	defaultServerHealthyConnectionDuration = 10 * time.Second
)

// RemoteClientObserver receives connection lifecycle reports for clients which represent
// a relay server (trunk) connection. Implementations may be called from any goroutine
// and must not block.
type RemoteClientObserver interface {
	// ServerConnected is reported once a connection attempt has succeeded.
	ServerConnected(url string, client RemoteClient)
	// ServerConnectFailed is reported when an attempt has definitely failed. The client
	// is finished and will not be reused; a fresh client is created for the next attempt.
	ServerConnectFailed(url string, client RemoteClient, err error)
	// ServerDisconnected is reported when a previously established connection has ended
	// (read error, peer connection failure, remote hang up, ...).
	ServerDisconnected(url string, client RemoteClient, err error)
}

type serverConnectionEventKind int

const (
	serverEventConnected serverConnectionEventKind = iota
	serverEventConnectFailed
	serverEventDisconnected
)

type serverConnectionEvent struct {
	kind   serverConnectionEventKind
	url    string
	client RemoteClient
	err    error
}

// serverEntry is the authoritative state for one intended relay server address.
// It is only mutated by the SFU message handler goroutine.
//
// The submitted intent ("intendedServers") and the connections which exist to satisfy
// it ("servers") are deliberately separate. A serverEntry tracks how far the SFU has
// got towards making the intent true, so an address which is still connecting, or is
// waiting out a backoff retry, never masquerades as an established connection.
type serverEntry struct {
	url string

	state     ServerState
	attempts  int    // consecutive failed connection attempts, reset once connected
	lastError string // last connection error, kept while retrying and after giving up

	retryAt time.Time

	// connectedAt is when the current connection was established, used to distinguish
	// a healthy connection from a flapping one when deciding whether to reset attempts.
	connectedAt time.Time

	// token uniquely identifies the current attempt. Timers carry the token they were
	// scheduled with, so a stale retry or connect-timeout can never apply to a later
	// attempt (including one for the same address after remove/re-add).
	token uint64

	// client is the single in-flight or established connection for this address.
	// Exactly one client exists at a time, so repeated submissions cannot fan out
	// into parallel connections or duplicate media forwarding.
	client RemoteClient

	// timer is the pending connect-timeout or backoff-retry timer for this entry. It is
	// only touched on the SFU handler goroutine (the timer callback just enqueues a
	// command), so no lock is needed.
	timer *time.Timer
}

func (s *Sfu) nextServerToken() uint64 {
	s.serverTokenSeq++
	return s.serverTokenSeq
}

func (s *Sfu) serverRetryDelay(attempts int) time.Duration {
	delay := s.serverRetryBaseDelay
	if delay <= 0 {
		return 0
	}

	for i := 1; i < attempts; i++ {
		delay *= 2
		if s.serverRetryMaxDelay > 0 && delay >= s.serverRetryMaxDelay {
			return s.serverRetryMaxDelay
		}
	}

	if s.serverRetryMaxDelay > 0 && delay > s.serverRetryMaxDelay {
		return s.serverRetryMaxDelay
	}

	return delay
}

// snapshotServers returns a consistent view of the relay servers: the URLs with an
// actually established connection, plus the observable state of every intended address.
// It runs on the SFU message handler goroutine, so it always sees one serial order.
func (s *Sfu) snapshotServers() *CurrentServers {
	result := &CurrentServers{
		Servers:        make([]string, 0, len(s.servers)),
		ServerStatuses: make([]*ServerStatus, 0, len(s.servers)),
	}

	urls := make([]string, 0, len(s.servers))
	for url := range s.servers {
		urls = append(urls, url)
	}
	sort.Strings(urls)

	now := time.Now()

	for _, url := range urls {
		entry := s.servers[url]

		status := &ServerStatus{
			Url:       url,
			State:     entry.state,
			Attempts:  int32(entry.attempts),
			LastError: entry.lastError,
		}

		if entry.state == ServerState_Retrying && !entry.retryAt.IsZero() {
			remaining := entry.retryAt.Sub(now)
			if remaining < 0 {
				remaining = 0
			}
			status.NextRetryInMillis = int32(remaining.Milliseconds())
		}

		if entry.state == ServerState_Connected {
			result.Servers = append(result.Servers, url)
		}

		result.ServerStatuses = append(result.ServerStatuses, status)
	}

	return result
}

// evaluateServers reconciles the intended server list with the connections which are
// actually being attempted or are established.
func (s *Sfu) evaluateServers() {
	// Stop anything which is no longer intended, whether it is connected, connecting
	// or waiting for a retry.
	for url, entry := range s.servers {
		if !s.intendedServers[url] {
			s.stopServerEntry(entry)
			delete(s.servers, url)
		}
	}

	// Start anything intended which is not already being worked on. Duplicate entries
	// collapse in intendedServers, so a repeated address can never create two clients.
	for url := range s.intendedServers {
		entry, exists := s.servers[url]
		if !exists {
			entry = &serverEntry{url: url}
			s.servers[url] = entry
			s.startServerAttempt(entry)
			continue
		}

		// An explicit resubmission of an address which already gave up is a fresh
		// intent: start a new attempt with a new client.
		if entry.state == ServerState_Failed {
			entry.attempts = 0
			entry.lastError = ""
			s.startServerAttempt(entry)
		}
	}
}

// startServerAttempt creates a brand new client for one connection attempt. Retries
// never reuse a client which may have been stopped, which is what makes remove then
// re-add produce a genuinely new connection.
func (s *Sfu) startServerAttempt(entry *serverEntry) {
	entry.stopTimer()
	entry.token = s.nextServerToken()
	entry.state = ServerState_Connecting
	entry.retryAt = time.Time{}

	if s.remoteClientFactory == nil {
		entry.client = nil
		entry.lastError = "no remote client factory"
		s.serverAttemptFailed(entry)
		return
	}

	client := s.remoteClientFactory.NewClient(&RemoteClientParameters{
		logger:   s.logger,
		trunkurl: entry.url,
		s:        s,
		observer: s,
	})

	if client == nil {
		entry.client = nil
		entry.lastError = "failed to create remote client"
		s.serverAttemptFailed(entry)
		return
	}

	entry.client = client

	// Guard against a client which never reports either outcome. The timer is
	// independent of the command queue so a momentary queue-full condition cannot
	// silently drop the guard and leave the entry stuck in Connecting.
	s.scheduleServerTimer(entry, s.serverConnectTimeout, sfuServerConnectTimeout)
}

// scheduleServerTimer arranges for a retry or connect-timeout command for this entry to
// be delivered after delay. Timers are kept per entry so a new attempt supersedes the
// previous one, and they live outside the command queue so a momentarily full queue
// cannot silently drop them (which would leave an entry stuck in Connecting/Retrying).
// The delivered command still carries the entry token, so a stale timer is ignored if
// the attempt it belonged to has since been superseded or removed.
func (s *Sfu) scheduleServerTimer(entry *serverEntry, delay time.Duration, what sfuCommand) {
	entry.stopTimer()

	if delay <= 0 {
		return
	}

	url := entry.url
	token := entry.token

	timer := time.AfterFunc(delay, func() {
		s.handler.SendWait(what, &sfuCommandMessage{
			serverURL:        url,
			serverGeneration: token,
		}, 5*time.Second)
	})

	entry.timer = timer
}

// stopServerTimer cancels any outstanding retry / connect-timeout timer for the entry.
func (entry *serverEntry) stopTimer() {
	if entry.timer != nil {
		entry.timer.Stop()
		entry.timer = nil
	}
}

// serverAttemptFailed records a failed attempt and either schedules a backoff retry or
// gives up on this address until it is submitted again.
func (s *Sfu) serverAttemptFailed(entry *serverEntry) {
	entry.client = nil
	entry.attempts++

	// Invalidate the connect timeout guard belonging to the attempt which just failed.
	entry.stopTimer()
	entry.token = s.nextServerToken()

	if entry.attempts >= s.serverMaxConnectAttempts {
		entry.state = ServerState_Failed
		entry.retryAt = time.Time{}
		return
	}

	delay := s.serverRetryDelay(entry.attempts)
	entry.state = ServerState_Retrying
	entry.retryAt = time.Now().Add(delay)

	s.scheduleServerTimer(entry, delay, sfuServerRetry)
}

// stopServerEntry permanently stops the current connection for an entry. After this
// returns neither timers nor late events can resurrect the entry, and no track it
// contributed keeps being relayed.
func (s *Sfu) stopServerEntry(entry *serverEntry) {
	// Cancel any outstanding retry or connect-timeout timer and invalidate the token
	// which a timer that already fired would carry.
	entry.stopTimer()
	entry.token = s.nextServerToken()

	client := entry.client
	entry.client = nil
	entry.state = ServerState_ServerStateUnknown
	entry.retryAt = time.Time{}

	if client != nil {
		client.stop()
		s.dropTracksOwnedBy(client)
	}
}

// handleServerConnectionEvent applies a lifecycle report from a client. Events from
// clients which are no longer the current connection for their address are ignored,
// which is what keeps delete/re-add and failure/retry interleavings converging.
func (s *Sfu) handleServerConnectionEvent(event *serverConnectionEvent) {
	entry, exists := s.servers[event.url]
	if !exists || entry.client != event.client {
		return
	}

	switch event.kind {
	case serverEventConnected:
		entry.state = ServerState_Connected
		// A connection which stayed up for a while earns a clean slate, so a later
		// failure is treated as a fresh problem. One which drops quickly is treated as
		// a continuation of a flapping connection: its attempt count is kept so the
		// backoff grows and it eventually gives up instead of reconnecting forever.
		if time.Since(entry.connectedAt) >= s.serverHealthyConnectionDuration {
			entry.attempts = 0
		}
		entry.connectedAt = time.Now()
		entry.lastError = ""
		entry.retryAt = time.Time{}
		// Invalidate the connect timeout guard for this successful attempt.
		entry.stopTimer()
		entry.token = s.nextServerToken()
	case serverEventConnectFailed:
		entry.lastError = serverErrorString(event.err)
		s.serverAttemptFailed(entry)
	case serverEventDisconnected:
		entry.lastError = serverErrorString(event.err)
		s.serverAttemptFailed(entry)
	}
}

// handleServerRetry fires when a backoff delay has elapsed.
func (s *Sfu) handleServerRetry(url string, token uint64) {
	entry, exists := s.servers[url]
	if !exists || entry.token != token || entry.state != ServerState_Retrying {
		return
	}

	s.startServerAttempt(entry)
}

// handleServerConnectTimeout fires when a connection attempt never reached a verdict.
func (s *Sfu) handleServerConnectTimeout(url string, token uint64) {
	entry, exists := s.servers[url]
	if !exists || entry.token != token || entry.state != ServerState_Connecting {
		return
	}

	if entry.client != nil {
		entry.client.stop()
	}

	entry.lastError = fmt.Sprintf("timed out connecting after %s", s.serverConnectTimeout)
	s.serverAttemptFailed(entry)
}

func serverErrorString(err error) string {
	if err == nil {
		return ""
	}

	return err.Error()
}
