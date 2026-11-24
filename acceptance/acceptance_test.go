//go:build acceptance
// +build acceptance

package acceptance_test

import (
	"bytes"
	"errors"
	"sync"
	"testing"
	"time"

	udpsocket "github.com/theredrad/udpsocket"
)

// A1: after a normal handshake, an encrypted business record carrying the
// session ID is delivered to the Handler with the right client ID/type/body.
func TestAcc_A1_NormalHandshakeBusinessCallback(t *testing.T) {
	env := startEnv(t, 30*time.Second)
	cl := env.newClient("tok-a1")
	sid := cl.completeHandshake()

	payload := []byte{1, 2, 3, 4}
	cl.sendRecord(accRecordType, sid, payload)

	cb := env.waitCallback(2 * time.Second)
	if cb.id != "uid-tok-a1" {
		t.Fatalf("want handler id uid-tok-a1, got %q", cb.id)
	}
	if cb.typ != accRecordType {
		t.Fatalf("want record type %d, got %d", accRecordType, cb.typ)
	}
	if !bytes.Equal(cb.p, payload) {
		t.Fatalf("payload mismatch: want %v, got %v", payload, cb.p)
	}
}

// A2: two byte-identical cookie handshakes from the same address produce
// exactly one registration (one Authenticate call) and one ServerHello; the
// duplicate produces no second reply.
func TestAcc_A2_DuplicateHandshakeRegistersOnce(t *testing.T) {
	env := startEnv(t, 30*time.Second)
	cl := env.newClient("tok-a2")

	cookie := cl.obtainCookie()
	datagram := cl.sendHello(cookie, true)

	pkt, err := cl.readPacket(2 * time.Second)
	if err != nil {
		t.Fatalf("read first ServerHello: %v", err)
	}
	if pkt.Type != udpsocket.HandshakeServerHelloRecordType {
		t.Fatalf("want ServerHello, got type %d", pkt.Type)
	}
	h, err := cl.tr.UnmarshalHandshake(pkt.Plain)
	if err != nil {
		t.Fatalf("unmarshal ServerHello: %v", err)
	}
	cl.sessionID = h.GetSessionId()

	// Resend the exact same authenticated handshake twice, back to back.
	cl.sendRaw(datagram)
	cl.sendRaw(datagram)

	packets := cl.collectPackets(700 * time.Millisecond)
	for _, p := range packets {
		if p.Type == udpsocket.HandshakeServerHelloRecordType {
			t.Fatalf("duplicate handshake produced an extra ServerHello (%d extra packet(s) total)", len(packets))
		}
	}

	if got := env.auth.count(); got != 1 {
		t.Fatalf("want exactly 1 Authenticate call, got %d", got)
	}
}

// A8: after a duplicate handshake, the original session ID/key still works and
// records still reach the Handler.
func TestAcc_A8_OriginalSessionSurvivesDuplicate(t *testing.T) {
	env := startEnv(t, 30*time.Second)
	cl := env.newClient("tok-a8")

	cookie := cl.obtainCookie()
	datagram := cl.sendHello(cookie, true)
	pkt, err := cl.readPacket(2 * time.Second)
	if err != nil {
		t.Fatalf("read ServerHello: %v", err)
	}
	h, err := cl.tr.UnmarshalHandshake(pkt.Plain)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	sid := h.GetSessionId()
	cl.sessionID = sid

	// Duplicate arrives after establishment.
	cl.sendRaw(datagram)
	if dups := cl.collectPackets(400 * time.Millisecond); len(dups) != 0 {
		t.Fatalf("expected no reply to duplicate, got %d packets", len(dups))
	}

	cl.sendRecord(accRecordType, sid, []byte("still-alive"))
	cb := env.waitCallback(2 * time.Second)
	if cb.id != "uid-tok-a8" || !bytes.Equal(cb.p, []byte("still-alive")) {
		t.Fatalf("original session did not survive: %+v", cb)
	}
}

// A3: once the heartbeat has expired, business records are rejected with an
// UnAuthenticated notification and never invoke the Handler.
func TestAcc_A3_ExpiredRecordRejectedWithUnauth(t *testing.T) {
	env := startEnv(t, 140*time.Millisecond)
	cl := env.newClient("tok-a3")
	sid := cl.completeHandshake()

	// Sanity: session works before expiry.
	cl.sendRecord(accRecordType, sid, []byte("before"))
	env.waitCallback(2 * time.Second)

	// Let both the TTL and several GC ticks pass.
	time.Sleep(500 * time.Millisecond)

	cl.sendRecord(accRecordType, sid, []byte("after-expiry"))
	pkt, err := cl.readPacket(1500 * time.Millisecond)
	if err != nil {
		t.Fatalf("expected UnAuthenticated notification, read error: %v", err)
	}
	if pkt.Type != udpsocket.UnAuthenticated {
		t.Fatalf("want UnAuthenticated type %d, got %d", udpsocket.UnAuthenticated, pkt.Type)
	}

	env.assertNoCallback(300 * time.Millisecond)
}

// A4: after expiration cleanup, BroadcastToClients does not deliver to the
// removed client and SendToClientByID returns ErrClientNotFound, while another
// online client keeps receiving.
func TestAcc_A4_ExpiredClientGetsNoBroadcastOrDirectSend(t *testing.T) {
	expiration := 200 * time.Millisecond
	env := startEnv(t, expiration)

	dead := env.newClient("dead-token")
	dead.completeHandshake()
	deadID := "uid-dead-token"

	live := env.newClient("live-token")
	liveSID := live.completeHandshake()

	// Keep the live client's heartbeat fresh.
	stopHB := make(chan struct{})
	var hbWG sync.WaitGroup
	hbWG.Add(1)
	go func() {
		defer hbWG.Done()
		ticker := time.NewTicker(40 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopHB:
				return
			case <-ticker.C:
				_ = live.postRecord(accRecordType, liveSID, []byte("hb"))
			}
		}
	}()
	defer func() {
		close(stopHB)
		hbWG.Wait()
	}()

	// Wait for the dead client to expire and be garbage collected.
	time.Sleep(700 * time.Millisecond)

	if err := env.server.SendToClientByID(deadID, accRecordType, []byte("direct")); !errors.Is(err, udpsocket.ErrClientNotFound) {
		t.Fatalf("want ErrClientNotFound for expired client, got %v", err)
	}

	// Drain anything that arrived before/around cleanup.
	dead.drainPackets(200 * time.Millisecond)

	body := []byte("broadcast-body")
	for i := 0; i < 3; i++ {
		env.server.BroadcastToClients(16, body)
		time.Sleep(80 * time.Millisecond)
	}

	// Removed client must receive nothing.
	if pkt, err := dead.readPacket(500 * time.Millisecond); err == nil {
		t.Fatalf("removed client unexpectedly received packet type=%d", pkt.Type)
	}

	// Live client must receive the broadcast.
	deadline := time.Now().Add(1500 * time.Millisecond)
	var gotBroadcast bool
	for time.Now().Before(deadline) && !gotBroadcast {
		pkt, err := live.readPacket(250 * time.Millisecond)
		if err != nil {
			continue
		}
		if pkt.Type == 16 {
			if !bytes.Equal(pkt.Plain, body) {
				t.Fatalf("broadcast body mismatch: %v", pkt.Plain)
			}
			gotBroadcast = true
		}
	}
	if !gotBroadcast {
		t.Fatal("live client did not receive broadcast")
	}
}

// A5: after Stop + Serve, old session IDs and old cookies are invalid; a fresh
// handshake starts from zero and works.
func TestAcc_A5_StopServeInvalidatesSessionsAndCookies(t *testing.T) {
	env := startEnv(t, 30*time.Second)
	cl := env.newClient("tok-a5")

	oldCookie := cl.obtainCookie()
	oldDatagram := cl.sendHello(oldCookie, true)
	pkt, err := cl.readPacket(2 * time.Second)
	if err != nil {
		t.Fatalf("read ServerHello: %v", err)
	}
	h, err := cl.tr.UnmarshalHandshake(pkt.Plain)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	oldSID := append([]byte(nil), h.GetSessionId()...)

	cl.sendRecord(accRecordType, oldSID, []byte("before-restart"))
	env.waitCallback(2 * time.Second)

	// Restart the serving epoch on the same UDP connection.
	env.server.Stop()
	go env.server.Serve()
	waitAccReady(t, cl)
	cl.drainPackets(150 * time.Millisecond) // discard any duplicate HelloVerify probes

	// Old session ID is rejected and triggers an UnAuthenticated notification.
	cl.sendRecord(accRecordType, oldSID, []byte("after-restart"))
	pkt, err = cl.readPacket(1500 * time.Millisecond)
	if err != nil {
		t.Fatalf("expected unauthenticated for old session after restart: %v", err)
	}
	if pkt.Type != udpsocket.UnAuthenticated {
		t.Fatalf("want UnAuthenticated after restart, got type %d", pkt.Type)
	}

	// Old cookie can no longer pass validation and must not get a ServerHello.
	cl.sendRaw(oldDatagram)
	if pkt, err := cl.readPacket(600 * time.Millisecond); err == nil {
		t.Fatalf("old cookie unexpectedly accepted after restart (type %d)", pkt.Type)
	}

	// A brand new handshake succeeds and business records flow again.
	fresh := env.newClient("tok-a5-fresh")
	newSID := fresh.completeHandshake()
	fresh.sendRecord(accRecordType, newSID, []byte("fresh-start"))
	cb := env.waitCallback(2 * time.Second)
	if cb.id != "uid-tok-a5-fresh" {
		t.Fatalf("unexpected handler id after restart: %q", cb.id)
	}
	if !bytes.Equal(cb.p, []byte("fresh-start")) {
		t.Fatalf("fresh payload mismatch: %v", cb.p)
	}
}

// A6: concurrent broadcasts, direct sends and expiration cleanup must not
// race/panic; deleted clients go silent while kept-alive clients receive.
func TestAcc_A6_ConcurrentBroadcastGCNoPanic(t *testing.T) {
	const totalClients = 12
	expiration := 250 * time.Millisecond
	env := startEnv(t, expiration)

	type member struct {
		cl  *accClient
		sid []byte
		id  string
	}
	var dead, alive []member

	for i := 0; i < totalClients; i++ {
		tok := "tok-" + string(rune('a'+i))
		cl := env.newClient(tok)
		sid := cl.completeHandshake()
		m := member{cl: cl, sid: sid, id: "uid-" + tok}
		if i%2 == 0 {
			dead = append(dead, m)
		} else {
			alive = append(alive, m)
		}
	}

	stopHB := make(chan struct{})
	var hbWG sync.WaitGroup
	for _, m := range alive {
		hbWG.Add(1)
		go func(m member) {
			defer hbWG.Done()
			ticker := time.NewTicker(40 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-stopHB:
					return
				case <-ticker.C:
					_ = m.cl.postRecord(accRecordType, m.sid, []byte("hb"))
				}
			}
		}(m)
	}

	// Broadcast storm + concurrent direct sends for a while.
	stormDone := make(chan struct{})
	var stormWG sync.WaitGroup
	for b := 0; b < 6; b++ {
		stormWG.Add(1)
		go func(seed byte) {
			defer stormWG.Done()
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			deadline := time.Now().Add(500 * time.Millisecond)
			i := seed
			for {
				select {
				case <-ticker.C:
					body := []byte{i, i + 1, i + 2}
					env.server.BroadcastToClients(17, body)
					_ = env.server.SendToClientByID(alive[int(i)%len(alive)].id, 17, body)
					_ = env.server.SendToClientByID("does-not-exist", 17, body)
					i++
					if time.Now().After(deadline) {
						return
					}
				case <-stormDone:
					return
				}
			}
		}(byte(b))
	}
	stormWG.Wait()
	close(stormDone)

	// Let every idle (dead-set) client expire and be collected.
	time.Sleep(800 * time.Millisecond)

	// Drain stale storm packets on every client before the final assertion.
	var drainWG sync.WaitGroup
	for _, m := range dead {
		drainWG.Add(1)
		go func(c *accClient) { defer drainWG.Done(); c.drainPackets(180 * time.Millisecond) }(m.cl)
	}
	for _, m := range alive {
		drainWG.Add(1)
		go func(c *accClient) { defer drainWG.Done(); c.drainPackets(180 * time.Millisecond) }(m.cl)
	}
	drainWG.Wait()

	// Final broadcasts after cleanup.
	for i := 0; i < 4; i++ {
		env.server.BroadcastToClients(18, []byte("final"))
		time.Sleep(70 * time.Millisecond)
	}

	// At least one kept-alive client must receive a final broadcast. Read
	// first so the packets are observed before the silence check window.
	liveGot := false
	liveDeadline := time.Now().Add(1500 * time.Millisecond)
	for _, m := range alive {
		if liveGot {
			break
		}
		_ = m.cl.conn.SetReadDeadline(time.Now().Add(700 * time.Millisecond))
		buf := make([]byte, 8192)
		for time.Now().Before(liveDeadline) {
			n, _, err := m.cl.conn.ReadFromUDP(buf)
			if err != nil {
				break
			}
			if n > 0 && buf[0] == 18 {
				liveGot = true
				break
			}
		}
	}
	if !liveGot {
		t.Fatal("no kept-alive client received the final broadcast")
	}

	// Deleted clients must be silent. Read them concurrently to keep the
	// assertion wall-time short.
	var silentWG sync.WaitGroup
	var silentMu sync.Mutex
	var noisy []int
	for idx, m := range dead {
		silentWG.Add(1)
		go func(idx int, c *accClient) {
			defer silentWG.Done()
			if pkt, err := c.readPacket(450 * time.Millisecond); err == nil {
				silentMu.Lock()
				noisy = append(noisy, idx)
				t.Logf("deleted client %d received type=%d", idx, pkt.Type)
				silentMu.Unlock()
			}
		}(idx, m.cl)
	}
	silentWG.Wait()
	if len(noisy) != 0 {
		t.Fatalf("%d deleted client(s) received packets after cleanup", len(noisy))
	}

	close(stopHB)
	hbWG.Wait()
}

// A7: every rejection writes an observable log line containing the reason, and
// a second online client keeps being served normally.
func TestAcc_A7_RejectionLoggedAndOtherClientUnaffected(t *testing.T) {
	env := startEnv(t, 30*time.Second)

	bad := env.newClient("tok-bad")
	sid := bad.completeHandshake()
	wrong := make([]byte, len(sid))
	for i := range sid {
		wrong[i] = sid[i] ^ 0xFF
	}
	bad.sendRecord(accRecordType, wrong, []byte{1})
	pkt, err := bad.readPacket(1500 * time.Millisecond)
	if err != nil {
		t.Fatalf("expected unauthenticated packet: %v", err)
	}
	if pkt.Type != udpsocket.UnAuthenticated {
		t.Fatalf("want UnAuthenticated, got %d", pkt.Type)
	}

	if logs := env.logs.String(); !containsAny(logs, "client session not found", "invalid session") {
		t.Fatalf("expected session rejection reason in logs, got:\n%s", logs)
	}

	good := env.newClient("tok-good")
	goodSID := good.completeHandshake()
	good.sendRecord(accRecordType, goodSID, []byte("ok"))
	cb := env.waitCallback(2 * time.Second)
	if cb.id != "uid-tok-good" {
		t.Fatalf("other client should not be affected, got callback %q", cb.id)
	}
}

// waitAccReady polls the server with fresh ClientHellos until it replies, which
// synchronizes the test with a restarted Serve epoch.
func waitAccReady(t *testing.T, c *accClient) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c.sendHello(nil, false)
		pkt, err := c.readPacket(250 * time.Millisecond)
		if err == nil && pkt.Type == udpsocket.HandshakeHelloVerifyRecordType {
			return
		}
	}
	t.Fatal("server did not become ready after Serve")
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if bytes.Contains([]byte(s), []byte(sub)) {
			return true
		}
	}
	return false
}
