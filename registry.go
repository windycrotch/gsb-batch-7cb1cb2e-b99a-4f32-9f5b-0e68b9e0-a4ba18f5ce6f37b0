package udpsocket

import (
	"fmt"
	"net"
	"time"
)

// defaultHandshakeReplayTTL is the short window in which an identical
// cookie-bearing handshake (same address, cookie & client random) is
// considered a retransmission and must not register a second client or
// invalidate the established session.
const defaultHandshakeReplayTTL = 1500 * time.Millisecond

// handshakeLease records the result of the single authenticated registration
// produced for an idempotency fingerprint.
type handshakeLease struct {
	client *Client
	at     time.Time
}

// addrKey returns the canonical map key for a client UDP address.
// The historical ip_port format is preserved so existing callers/tests that
// build the key themselves keep working.
func addrKey(addr *net.UDPAddr) string {
	return fmt.Sprintf("%s_%d", addr.IP.String(), addr.Port)
}

// handshakeFingerprint identifies retransmissions of the same authenticated
// handshake: same source address, same cookie and same client random.
func handshakeFingerprint(addr *net.UDPAddr, cookie, random []byte) string {
	return addrKey(addr) + "|" + string(cookie) + "|" + string(random)
}

// registerClient generates a new session ID & registers an address with token
// ID & encryption key as a Client.
//
// It is the unconditional upsert used by tests/tooling and by fresh
// (non-retransmission) handshakes. All map mutations happen under s.mu.
func (s *Server) registerClient(addr *net.UDPAddr, ID string, eKey []byte) (*Client, error) {
	sessionID, err := s.sessionManager.GenerateSessionID(addr, ID)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	cl := &Client{
		ID:            ID,
		sessionID:     sessionID,
		addr:          addr,
		eKey:          eKey,
		lastHeartbeat: &now,
	}

	s.mu.Lock()
	s.clients[ID] = cl
	s.sessions[addrKey(addr)] = cl
	s.mu.Unlock()

	return cl, nil
}

// findRecentHandshake returns the client produced by an identical handshake
// inside the idempotency window. Expired entries behave as misses.
func (s *Server) findRecentHandshake(fp string) (*Client, bool) {
	if fp == "" {
		return nil, false
	}

	s.mu.RLock()
	lease, ok := s.handshakeCache[fp]
	ttl := s.handshakeTTL
	s.mu.RUnlock()
	if !ok || lease == nil {
		return nil, false
	}
	if ttl <= 0 {
		ttl = defaultHandshakeReplayTTL
	}
	if time.Since(lease.at) > ttl {
		return nil, false
	}
	return lease.client, true
}

// registerAuthenticatedClient implements atomic exactly-once registration for
// an authenticated handshake fingerprint.
//
// If a fresh registration already exists for fp it is returned with
// duplicate=true and no map is mutated. Otherwise a new client is created and
// published together with the idempotency lease in one critical section.
func (s *Server) registerAuthenticatedClient(addr *net.UDPAddr, ID string, eKey []byte, fp string) (cl *Client, duplicate bool, err error) {
	if existing, ok := s.findRecentHandshake(fp); ok {
		return existing, true, nil
	}

	sessionID, err := s.sessionManager.GenerateSessionID(addr, ID)
	if err != nil {
		return nil, false, err
	}

	now := time.Now()
	fresh := &Client{
		ID:            ID,
		sessionID:     sessionID,
		addr:          addr,
		eKey:          eKey,
		lastHeartbeat: &now,
	}

	ttl := s.handshakeTTL
	if ttl <= 0 {
		ttl = defaultHandshakeReplayTTL
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Re-check under the write lock: two identical requests may have raced
	// between the read-only lookup and registration.
	if fp != "" {
		if lease, ok := s.handshakeCache[fp]; ok && lease != nil && time.Since(lease.at) <= ttl {
			return lease.client, true, nil
		}
	}

	s.clients[ID] = fresh
	s.sessions[addrKey(addr)] = fresh
	if fp != "" {
		s.handshakeCache[fp] = &handshakeLease{client: fresh, at: now}
		s.sweepHandshakeCacheLocked(now, ttl)
	}

	return fresh, false, nil
}

// sweepHandshakeCacheLocked opportunistically drops expired idempotency entries
// so the cache cannot grow without bound. The caller must hold s.mu (write).
func (s *Server) sweepHandshakeCacheLocked(now time.Time, ttl time.Duration) {
	if len(s.handshakeCache) <= 1024 {
		return
	}
	for fp, lease := range s.handshakeCache {
		if lease == nil || now.Sub(lease.at) > ttl {
			delete(s.handshakeCache, fp)
		}
	}
}

// findClientBySessionID returns the Client by the Session ID.
func (s *Server) findClientBySessionID(sessionID []byte) (*Client, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, client := range s.clients {
		if string(client.sessionID) == string(sessionID) {
			return client, true
		}
	}
	return nil, false
}

// returns the Client ID by the session ID
func (s *Server) findClientIDBySessionID(sessionID []byte) (string, bool) {
	cl, ok := s.findClientBySessionID(sessionID)
	if !ok {
		return "", ok
	}
	return cl.ID, true
}

// findClientByAddr returns the Client by IP & Port.
func (s *Server) findClientByAddr(addr *net.UDPAddr) (*Client, bool) {
	s.mu.RLock()
	cl, ok := s.sessions[addrKey(addr)]
	s.mu.RUnlock()
	return cl, ok
}

// findClientByID returns the Client by its application ID.
func (s *Server) findClientByID(clientID string) (*Client, bool) {
	s.mu.RLock()
	cl, ok := s.clients[clientID]
	s.mu.RUnlock()
	return cl, ok
}

// snapshotClients returns a stable copy of the currently registered clients.
// Iterating the returned slice is safe without holding s.mu.
func (s *Server) snapshotClients() []*Client {
	s.mu.RLock()
	clients := make([]*Client, 0, len(s.clients))
	for _, cl := range s.clients {
		clients = append(clients, cl)
	}
	s.mu.RUnlock()
	return clients
}

// removeClient deletes the victim from both indexes, but only when the maps
// still point at that exact instance. A freshly re-registered client sharing
// the same address/ID is never removed by cleaning up its predecessor.
//
// The victim is marked removed first so broadcasts that already snapshotted it
// can skip the write instead of sending to a deleted client.
func (s *Server) removeClient(victim *Client) {
	if victim == nil {
		return
	}
	victim.markRemoved()
	s.deleteClientMaps(victim)
}

// deleteClientMaps performs the identity-conditional index deletion.
func (s *Server) deleteClientMaps(victim *Client) {
	key := addrKey(victim.addr)
	s.mu.Lock()
	if current, ok := s.sessions[key]; ok && current == victim {
		delete(s.sessions, key)
	}
	if current, ok := s.clients[victim.ID]; ok && current == victim {
		delete(s.clients, victim.ID)
	}
	s.mu.Unlock()
}

// reapExpiredClient atomically checks expiration and marks/removes the client.
// Re-checking the heartbeat while holding the client lock avoids deleting a
// client whose heartbeat was refreshed concurrently. It returns true only when
// this call actually reaped the client.
func (s *Server) reapExpiredClient(victim *Client) bool {
	if victim == nil || s.heartbeatExpiration <= 0 {
		return false
	}

	victim.Lock()
	if victim.removed {
		victim.Unlock()
		return false
	}
	if victim.lastHeartbeat != nil && !time.Now().After(victim.lastHeartbeat.Add(s.heartbeatExpiration)) {
		victim.Unlock()
		return false
	}
	victim.removed = true
	victim.Unlock()

	s.deleteClientMaps(victim)
	return true
}

// expireClientIfNeeded lazily enforces heartbeat expiration on the request
// path. It returns true when the client was expired and has been removed.
func (s *Server) expireClientIfNeeded(cl *Client) bool {
	return s.reapExpiredClient(cl)
}

// resetSessionState clears every client/index/idempotency entry and rotates
// the cookie/session signing secrets. It gives a (re)started Serve epoch a
// zero state so sessions and cookies from a previous epoch are rejected.
func (s *Server) resetSessionState() {
	s.mu.Lock()
	s.clients = make(map[string]*Client)
	s.sessions = make(map[string]*Client)
	s.handshakeCache = make(map[string]*handshakeLease)
	s.mu.Unlock()

	if s.sessionManager != nil {
		if err := s.sessionManager.rotateKeys(); err != nil && s.logger != nil {
			s.logger.Printf("error while rotating session secrets: %s", err)
		}
	}
}

// touchHeartbeat records the latest activity time for the client.
func (c *Client) touchHeartbeat() {
	c.Lock()
	now := time.Now()
	c.lastHeartbeat = &now
	c.Unlock()
}

// heartbeatTime returns the last heartbeat time, or the zero time when unset.
func (c *Client) heartbeatTime() time.Time {
	c.Lock()
	defer c.Unlock()
	if c.lastHeartbeat == nil {
		return time.Time{}
	}
	return *c.lastHeartbeat
}

// isExpired reports whether the client's heartbeat is older than ttl.
func (c *Client) isExpired(ttl time.Duration) bool {
	if ttl <= 0 {
		return false
	}
	last := c.heartbeatTime()
	if last.IsZero() {
		return false
	}
	return time.Now().After(last.Add(ttl))
}

// markRemoved flags the client as deleted.
func (c *Client) markRemoved() {
	c.Lock()
	c.removed = true
	c.Unlock()
}

// isRemoved reports whether the client has been deleted from the registry.
func (c *Client) isRemoved() bool {
	c.Lock()
	defer c.Unlock()
	return c.removed
}
