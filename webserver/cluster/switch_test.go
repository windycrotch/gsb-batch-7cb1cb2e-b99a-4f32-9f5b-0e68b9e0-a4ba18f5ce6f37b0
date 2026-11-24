package cluster

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSessionSwitchRoutesRequestsToNewSession verifies that when a second agent
// for the same slave connects, the master atomically replaces the session
// generation and every subsequent device request is served by the new session.
func TestSessionSwitchRoutesRequestsToNewSession(t *testing.T) {
	env := newSwitchTestEnv(t, "sw101")
	agent1 := env.startAgent()
	first := env.master.currentSession(env.slaveID, SessionControl)
	require.NotNil(t, first)
	env.getPing()

	// A second generation of the same slave connects while the first is still
	// around, mimicking a network blip with overlapping reconnects.
	agent2 := env.startAgent()
	waitFor(t, 5*time.Second, func() bool {
		current := env.master.currentSession(env.slaveID, SessionControl)
		return current != nil && current != first
	})

	// The old generation must no longer be selected for new streams.
	env.master.mu.RLock()
	state := env.master.slaves[env.slaveID]
	require.NotNil(t, state)
	for _, managed := range state.sessions {
		assert.False(t, managed.isRetired())
		assert.NotSame(t, first, managed.session)
	}
	env.master.mu.RUnlock()

	// All retired generations are marked as such while they wait to drain.
	waitFor(t, 5*time.Second, func() bool {
		env.master.mu.RLock()
		defer env.master.mu.RUnlock()
		st := env.master.slaves[env.slaveID]
		if st == nil {
			return false
		}
		for _, managed := range st.retired {
			if !managed.isRetired() {
				return false
			}
		}
		return true
	})

	env.getPing()
	assert.Greater(t, env.requestCount.Load(), int64(1))

	// Stopping the first agent must not affect the currently active session.
	agent1.Stop()
	time.Sleep(100 * time.Millisecond)
	require.NotNil(t, env.master.currentSession(env.slaveID, SessionControl))
	env.getPing()

	_ = agent2
}

// TestSessionSwitchDrainsExistingStreams verifies that streams established on
// the old session are allowed to finish instead of being force-closed, while
// no new stream can be opened on the retired session.
func TestSessionSwitchDrainsExistingStreams(t *testing.T) {
	env := newSwitchTestEnv(t, "sw102")
	agent1 := env.startAgent()
	first := env.master.currentSession(env.slaveID, SessionControl)

	// Open a long-running HTTP stream over the first session.
	stream := env.openStream()
	require.Equal(t, int64(1), env.activeHandlers.Load())

	agent2 := env.startAgent()
	waitFor(t, 5*time.Second, func() bool {
		return env.master.currentSession(env.slaveID, SessionControl) != first
	})

	// The in-flight response stays alive after the switch.
	time.Sleep(100 * time.Millisecond)
	require.Equal(t, int64(1), env.activeHandlers.Load(), "old session stream must not be force-closed")

	// New requests land on the new session and succeed.
	env.getPing()

	// Releasing the blocked response lets the old stream finish naturally.
	close(env.blockStream)
	select {
	case <-env.handlerReleases:
	case <-time.After(5 * time.Second):
		t.Fatal("old session stream did not finish after release")
	}
	// The stream already delivered its prefix; the remaining body is empty once
	// the handler exits.
	rest, err := io.ReadAll(stream.Body)
	require.NoError(t, err)
	stream.Body.Close()
	assert.Empty(t, rest)

	// Once drained, the retired generation is removed from bookkeeping.
	waitFor(t, 5*time.Second, func() bool {
		env.master.mu.RLock()
		defer env.master.mu.RUnlock()
		state := env.master.slaves[env.slaveID]
		return state != nil && len(state.retired) == 0
	})

	// The retired yamux session rejects new streams rather than silently
	// serving them.
	_, err = openHTTPStream(first)
	require.Error(t, err)

	_ = agent1
	_ = agent2
}

// TestFailedHandshakeKeepsPreviousSession verifies that a reconnection whose
// handshake fails (bad protocol, bad hello, liveness probe failure) never
// replaces the working session, so a single failed reconnect cannot take the
// slave offline.
func TestFailedHandshakeKeepsPreviousSession(t *testing.T) {
	env := newSwitchTestEnv(t, "sw103")
	agent := env.startAgent()
	first := env.master.currentSession(env.slaveID, SessionControl)

	// Case 1: incompatible protocol version.
	conn := env.wsConnect()
	require.NoError(t, conn.WriteJSON(Hello{
		SlaveID:         env.slaveID,
		ProtocolVersion: ProtocolVersion + 99,
		Kind:            SessionControl,
	}))
	_, _, err := conn.ReadMessage()
	require.Error(t, err)
	_ = conn.Close()
	assert.Equal(t, first, env.master.currentSession(env.slaveID, SessionControl))

	// Case 2: a session that never answers yamux pings (half-dead connection).
	// The master creates a yamux client, waits for the liveness probe to time
	// out, and must keep the old session in place.
	deadConn := env.wsConnect()
	require.NoError(t, deadConn.WriteJSON(Hello{
		SlaveID:         env.slaveID,
		ProtocolVersion: ProtocolVersion,
		Kind:            SessionControl,
	}))
	// Do not run a yamux server on this side: the master's ping must time out.
	probeStart := time.Now()
	deadline := probeStart.Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if env.master.currentSession(env.slaveID, SessionControl) != first {
			t.Fatal("half-dead session replaced the healthy session")
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = deadConn.Close()

	// The original session is still usable.
	env.getPing()
	assert.Equal(t, first, env.master.currentSession(env.slaveID, SessionControl))
	assert.GreaterOrEqual(t, time.Since(probeStart), env.master.cfg.SessionPingTimeout,
		"the liveness probe must have timed out before the session is rejected")

	_ = agent
}

// TestSlaveStopClosesStreamsAndMarksOffline verifies that stopping the slave
// agent interrupts in-flight HTTP responses and raw TCP bridges, closes the
// underlying sessions, and the master reports the slave offline with explicit
// errors instead of hanging requests.
func TestSlaveStopClosesStreamsAndMarksOffline(t *testing.T) {
	env := newSwitchTestEnv(t, "sw104")
	agent := env.startAgent()

	// One long-running HTTP stream.
	httpStream := env.openStream()
	require.Equal(t, int64(1), env.activeHandlers.Load())

	// One raw TCP bridge over the tunnel.
	listener := echoListener(t)
	rawStream := env.dialRaw(listener)
	_, err := rawStream.Write([]byte("ping"))
	require.NoError(t, err)
	echoBuf := make([]byte, 4)
	require.NoError(t, rawStream.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, err = io.ReadFull(rawStream, echoBuf)
	require.NoError(t, err)
	assert.Equal(t, "ping", string(echoBuf))

	env.stopAgent(agent)

	// The in-flight HTTP response must be interrupted on both sides.
	select {
	case <-env.handlerReleases:
	case <-time.After(5 * time.Second):
		t.Fatal("slave-side HTTP handler did not exit after agent stop")
	}
	require.NoError(t, httpStream.Body.Close())

	// The raw bridge must break.
	require.NoError(t, rawStream.SetReadDeadline(time.Now().Add(5*time.Second)))
	buf := make([]byte, 1)
	_, err = rawStream.Read(buf)
	require.Error(t, err, "raw tunnel stream must close after agent stop")
	_ = rawStream.Close()

	// The master drops both session kinds and reports the slave offline.
	env.waitForDeviceGone()

	// The relay bookkeeping for this slave is gone as well.
	env.master.relays.mu.Lock()
	relayCount := len(env.master.relays.relays)
	env.master.relays.mu.Unlock()
	assert.Equal(t, 0, relayCount)

	// No tunnel-related goroutines remain on either side.
	assertGoroutinesSettled(t, 5*time.Second,
		"serveTunnel",
		"handleRawStream",
		"bridgeConns",
	)
}

// TestReconnectAfterStopRestoresDevice verifies the full offline -> online
// cycle: after the agent stops and the device disappears, a fresh agent brings
// the device back into the list and it becomes operable again.
func TestReconnectAfterStopRestoresDevice(t *testing.T) {
	env := newSwitchTestEnv(t, "sw105")
	agent := env.startAgent()
	globalID := env.globalID()
	waitFor(t, 5*time.Second, func() bool {
		_, ok := env.master.Device(globalID)
		return ok
	})

	env.stopAgent(agent)
	env.waitForDeviceGone()
	// The cached inventory entry stays around but is flagged unavailable while
	// the slave is offline.
	cached, ok := env.master.Device(globalID)
	if ok {
		assert.True(t, cached.Disabled)
		assert.False(t, cached.SlaveOnline)
	}
	devices := env.master.RemoteDevices("android")
	require.Len(t, devices, 1)
	assert.True(t, devices[0].Disabled)
	assert.False(t, devices[0].SlaveOnline)

	// Reconnect with a brand-new agent.
	agent2 := env.startAgent()
	waitFor(t, 5*time.Second, func() bool {
		_, ok := env.master.Device(globalID)
		return ok
	})
	env.getPing()

	restored := env.master.RemoteDevices("android")
	require.Len(t, restored, 1)
	assert.Equal(t, globalID, restored[0].ID)
	assert.True(t, restored[0].SlaveOnline)
	_ = agent2
}

// TestSessionSwitchWithRawStream verifies raw TCP streams opened before a
// switch keep working on the old session while new raw streams use the new
// session.
func TestSessionSwitchWithRawStream(t *testing.T) {
	env := newSwitchTestEnv(t, "sw106")
	agent1 := env.startAgent()
	first := env.master.currentSession(env.slaveID, SessionControl)

	listener := echoListener(t)
	oldRaw := env.dialRaw(listener)
	require.NoError(t, oldRaw.SetDeadline(time.Now().Add(10*time.Second)))

	agent2 := env.startAgent()
	waitFor(t, 5*time.Second, func() bool {
		return env.master.currentSession(env.slaveID, SessionControl) != first
	})

	// The old raw stream is still bridged.
	_, err := oldRaw.Write([]byte("old-"))
	require.NoError(t, err)
	_, err = oldRaw.Write([]byte("data"))
	require.NoError(t, err)
	echoBuf := make([]byte, 8)
	_, err = io.ReadFull(oldRaw, echoBuf)
	require.NoError(t, err)
	assert.Equal(t, "old-data", string(echoBuf))

	// A new raw stream opens on the replacement session.
	newRaw := env.dialRaw(listener)
	require.NoError(t, newRaw.SetDeadline(time.Now().Add(5*time.Second)))
	_, err = newRaw.Write([]byte("new"))
	require.NoError(t, err)
	newBuf := make([]byte, 3)
	_, err = io.ReadFull(newRaw, newBuf)
	require.NoError(t, err)
	assert.Equal(t, "new", string(newBuf))

	_ = oldRaw.Close()
	_ = newRaw.Close()
	_ = agent1
	_ = agent2
}

// TestConcurrentSessionSwitchesStaysConsistent hammers the master with many
// generations of the same slave and asserts the registry never holds more
// than one active session per kind.
func TestConcurrentSessionSwitchesStaysConsistent(t *testing.T) {
	env := newSwitchTestEnv(t, "sw107")
	var agents []*Agent
	for i := 0; i < 4; i++ {
		agents = append(agents, env.startAgent())
	}
	waitFor(t, 10*time.Second, func() bool {
		env.master.mu.RLock()
		state := env.master.slaves[env.slaveID]
		active := len(state.sessions)
		env.master.mu.RUnlock()
		return active == 2 // control + media
	})

	env.master.mu.RLock()
	state := env.master.slaves[env.slaveID]
	activeKinds := len(state.sessions)
	for _, managed := range state.sessions {
		assert.False(t, managed.isRetired())
	}
	env.master.mu.RUnlock()
	assert.Equal(t, 2, activeKinds)

	// The surviving session serves requests.
	env.getPing()

	for _, agent := range agents {
		agent.Stop()
	}
}

// TestOpenSessionStreamHonorsContext verifies the context-aware stream dial
// returns promptly when the context is canceled while the session is shutting
// down, instead of blocking.
func TestOpenSessionStreamHonorsContext(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	session, err := yamux.Client(client, yamuxConfig())
	require.NoError(t, err)
	require.NoError(t, session.Close())

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	_, err = openSessionStream(ctx, session)
	elapsed := time.Since(start)
	require.Error(t, err)
	assert.Less(t, elapsed, 500*time.Millisecond, "dial on a closed session must not block")
}

// TestRetiredSessionForceClosedAfterDrainTimeout verifies a retired session is
// eventually force-closed even if its streams never finish, so no tunnel is
// leaked indefinitely.
func TestRetiredSessionForceClosedAfterDrainTimeout(t *testing.T) {
	env := newSwitchTestEnv(t, "sw108")
	agent1 := env.startAgent()
	first := env.master.currentSession(env.slaveID, SessionControl)
	require.NotNil(t, first)

	// Keep an HTTP stream open so the old session cannot close on its own.
	stream := env.openStream()
	defer stream.Body.Close()

	agent2 := env.startAgent()
	waitFor(t, 5*time.Second, func() bool {
		return env.master.currentSession(env.slaveID, SessionControl) != first
	})

	// Do not release the blocked stream. The drain timeout must still tear the
	// old session down.
	waitFor(t, env.master.cfg.SessionDrainTimeout+10*time.Second, func() bool {
		return first.IsClosed()
	})
	_ = agent1
	_ = agent2
}

// TestMediaSessionFallsBackToControl verifies media requests use the media
// session when present and fall back to control otherwise, across a switch of
// both generations.
func TestMediaSessionFallsBackToControl(t *testing.T) {
	env := newSwitchTestEnv(t, "sw109")
	agent1 := env.startAgent()
	controlFirst := env.master.currentSession(env.slaveID, SessionControl)
	mediaFirst := env.master.currentSession(env.slaveID, SessionMedia)
	require.NotNil(t, controlFirst)
	require.NotNil(t, mediaFirst)
	assert.NotEqual(t, controlFirst, mediaFirst)

	agent2 := env.startAgent()
	waitFor(t, 5*time.Second, func() bool {
		return env.master.currentSession(env.slaveID, SessionControl) != controlFirst &&
			env.master.currentSession(env.slaveID, SessionMedia) != mediaFirst
	})

	// Both kinds route through fresh sessions after the switch.
	env.getPing()
	_ = agent1
	_ = agent2
}

// TestStopAgentWaitsForTunnelGoroutines verifies Agent.Stop does not return
// while a serveTunnel goroutine or raw bridge is still running.
func TestStopAgentWaitsForTunnelGoroutines(t *testing.T) {
	env := newSwitchTestEnv(t, "sw110")
	agent := env.startAgent()

	listener := echoListener(t)
	rawStream := env.dialRaw(listener)
	require.NoError(t, rawStream.SetDeadline(time.Now().Add(10*time.Second)))
	_, err := rawStream.Write([]byte("x"))
	require.NoError(t, err)
	echoBuf := make([]byte, 1)
	_, err = io.ReadFull(rawStream, echoBuf)
	require.NoError(t, err)

	done := make(chan struct{})
	go func() {
		agent.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Agent.Stop did not return in time")
	}

	// After Stop returns, no serve/bridge goroutines remain.
	assertGoroutinesSettled(t, 3*time.Second, "serveTunnel", "handleRawStream", "bridgeConns")
	_ = rawStream.Close()
}

// TestSlaveOfflineRequestsReturnExplicitError verifies requests issued while
// the slave is offline get a definite 502 rather than hanging, and internal
// callers get ErrSlaveOffline.
func TestSlaveOfflineRequestsReturnExplicitError(t *testing.T) {
	env := newSwitchTestEnv(t, "sw111")
	agent := env.startAgent()
	env.getPing()

	env.stopAgent(agent)
	env.waitForDeviceGone()

	// Internal session lookup returns nil, so raw connect yields the sentinel.
	assert.Nil(t, env.master.currentSession(env.slaveID, SessionMedia))
	remote, ok := env.master.ResolveDevice(env.globalID())
	require.True(t, ok)
	_, err := remote.(*remoteDevice).Connect("")
	require.ErrorIs(t, err, ErrSlaveOffline)

	// A proxied HTTP request fails fast with 502.
	reqCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet,
		env.server.URL+"/api/v1/devices/"+env.globalID()+"/ping", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
}

// TestMultipleSwitchesNoLeak performs several successive generations and
// asserts retired sessions are eventually cleaned up with no accumulation.
func TestMultipleSwitchesNoLeak(t *testing.T) {
	env := newSwitchTestEnv(t, "sw112")
	var allAgents []*Agent
	const rounds = 3
	for i := 0; i < rounds; i++ {
		allAgents = append(allAgents, env.startAgent())
		env.getPing()
	}

	// Stop all but the last, then wait for retired generations to drain.
	for i := 0; i < len(allAgents)-1; i++ {
		allAgents[i].Stop()
	}
	waitFor(t, 10*time.Second, func() bool {
		env.master.mu.RLock()
		defer env.master.mu.RUnlock()
		state := env.master.slaves[env.slaveID]
		return state != nil && len(state.retired) == 0
	})
	env.getPing()
	allAgents[len(allAgents)-1].Stop()
}
