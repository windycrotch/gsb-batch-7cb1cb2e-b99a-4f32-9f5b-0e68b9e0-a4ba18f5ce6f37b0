package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
)

// switchTestEnv wires a master HTTP server together with an in-process slave
// agent. Everything runs over loopback so the tests need no real device.
type switchTestEnv struct {
	t       *testing.T
	slaveID string
	secret  string
	localID string

	store  *memoryStore
	master *Master
	server *httptest.Server

	// requestCount counts how many times the slave served the ping endpoint.
	requestCount atomic.Int64
	// activeHandlers counts in-flight HTTP requests on the slave side.
	activeHandlers atomic.Int64
	// handlerReleases is closed when every in-flight request has finished.
	handlerReleases chan struct{}
	// blockStream holds a long-running response open until it is closed.
	blockStream chan struct{}

	slaveHandler *http.ServeMux
}

func newSwitchTestEnv(t *testing.T, slaveID string) *switchTestEnv {
	t.Helper()
	const secret = "switch-secret" // pragma: allowlist secret
	const localID = "a-switch"

	store := newMemoryStore()
	require.NoError(t, store.SaveSlave(SlaveRecord{
		ID:        slaveID,
		Name:      "switch-slave-" + slaveID,
		TokenHash: HashSecret(secret),
		CreatedAt: time.Now(),
	}))
	master := NewMaster(store, MasterConfig{
		Version:             "test",
		SessionPingTimeout:  2 * time.Second,
		SessionDrainTimeout: 2 * time.Second,
		Relay:               ADBRelayConfig{Bind: "127.0.0.1", PortRange: [2]int{0, 0}},
	})
	t.Cleanup(master.Close)

	env := &switchTestEnv{
		t:               t,
		slaveID:         slaveID,
		secret:          secret,
		localID:         localID,
		store:           store,
		master:          master,
		handlerReleases: make(chan struct{}, 64),
		blockStream:     make(chan struct{}),
	}
	env.slaveHandler = env.buildHandler()

	masterEcho := echo.New()
	master.RegisterRoutes(masterEcho, nil)
	// Echo only runs the route middleware after matching a route; register the
	// device path so DeviceRouteMiddleware can proxy it to the slave.
	masterEcho.Any("/api/v1/devices/:device_id/*", func(c echo.Context) error {
		return c.NoContent(http.StatusNotImplemented)
	})
	masterEcho.Use(master.DeviceRouteMiddleware())
	env.server = httptest.NewServer(masterEcho)
	t.Cleanup(env.server.Close)
	return env
}

func (env *switchTestEnv) buildHandler() *http.ServeMux {
	const localID = "a-switch"
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/cluster/devices", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": Snapshot{Devices: []Device{{
				ID:       localID,
				Platform: "android",
				Props:    map[string]any{"serial": "switch"},
			}}},
		})
	})
	mux.HandleFunc("/api/v1/devices/"+localID+"/ping", func(w http.ResponseWriter, r *http.Request) {
		env.requestCount.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]string{"pong": "ok"}})
	})
	// A response that stays open until the test releases it, modeling a screen
	// stream or a large HTTP transfer in flight during a session switch.
	mux.HandleFunc("/api/v1/devices/"+localID+"/stream", func(w http.ResponseWriter, r *http.Request) {
		env.activeHandlers.Add(1)
		defer func() {
			env.activeHandlers.Add(-1)
			env.handlerReleases <- struct{}{}
		}()
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("start\n"))
		if flusher != nil {
			flusher.Flush()
		}
		select {
		case <-env.blockStream:
		case <-r.Context().Done():
		}
	})
	return mux
}

func (env *switchTestEnv) token() string {
	return NewToken(env.slaveID, env.secret)
}

func (env *switchTestEnv) startAgent() *Agent {
	env.t.Helper()
	agent, err := NewAgent(AgentConfig{
		MasterURL: env.server.URL,
		Token:     env.token(),
		Name:      "switch-slave-" + env.slaveID,
	}, env.slaveHandler)
	require.NoError(env.t, err)
	ctx, cancel := context.WithCancel(context.Background())
	env.t.Cleanup(func() {
		cancel()
		agent.Stop()
	})
	agent.Start(ctx)
	waitFor(env.t, 5*time.Second, func() bool {
		return env.master.currentSession(env.slaveID, SessionControl) != nil
	})
	return agent
}

// stopAgent cancels the agent and waits for every runner goroutine, including
// tunnel stream handlers, to exit.
func (env *switchTestEnv) stopAgent(agent *Agent) {
	env.t.Helper()
	agent.Stop()
}

func (env *switchTestEnv) globalID() string {
	return GlobalDeviceID(env.slaveID, env.localID)
}

// getPing issues a device request through the master and expects success.
func (env *switchTestEnv) getPing() {
	env.t.Helper()
	resp, err := http.Get(env.server.URL + "/api/v1/devices/" + env.globalID() + "/ping")
	require.NoError(env.t, err)
	defer resp.Body.Close()
	require.Equal(env.t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(env.t, err)
	require.Contains(env.t, string(body), "pong")
}

// openStream opens the long-running endpoint and returns the live response.
func (env *switchTestEnv) openStream() *http.Response {
	env.t.Helper()
	resp, err := http.Get(env.server.URL + "/api/v1/devices/" + env.globalID() + "/stream")
	require.NoError(env.t, err)
	require.Equal(env.t, http.StatusOK, resp.StatusCode)
	buf := make([]byte, 6)
	_, err = io.ReadFull(resp.Body, buf)
	require.NoError(env.t, err)
	require.Equal(env.t, "start\n", string(buf))
	return resp
}

// waitForDeviceGone waits until the slave is considered offline and its devices
// are no longer served.
func (env *switchTestEnv) waitForDeviceGone() {
	env.t.Helper()
	waitFor(env.t, 5*time.Second, func() bool {
		return !env.master.slaveOnline(env.slaveID)
	})
	resp, err := http.Get(env.server.URL + "/api/v1/devices/" + env.globalID() + "/ping")
	require.NoError(env.t, err)
	defer resp.Body.Close()
	require.Equal(env.t, http.StatusBadGateway, resp.StatusCode)
}

// echoListener starts a local TCP echo server used by raw stream tests.
func echoListener(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}(conn)
		}
	}()
	return listener
}

// dialRaw opens a raw tunnel stream to the echo server through the master.
func (env *switchTestEnv) dialRaw(listener net.Listener) net.Conn {
	env.t.Helper()
	session := env.master.currentSession(env.slaveID, SessionControl)
	require.NotNil(env.t, session)
	host, portText, err := net.SplitHostPort(listener.Addr().String())
	require.NoError(env.t, err)
	var port int
	_, err = fmt.Sscanf(portText, "%d", &port)
	require.NoError(env.t, err)
	stream, err := openRawStream(session, RawTarget{Host: host, Port: port})
	require.NoError(env.t, err)
	return stream
}

// assertGoroutinesSettled fails the test if goroutines matching any of the
// cluster tunnel signatures keep running after the timeout.
func assertGoroutinesSettled(t *testing.T, timeout time.Duration, signatures ...string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !goroutinesPresent(signatures) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, signature := range signatures {
		if goroutinesPresent([]string{signature}) {
			t.Fatalf("goroutine %q still running after %s", signature, timeout)
		}
	}
}

func goroutinesPresent(signatures []string) bool {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	stack := string(buf[:n])
	for _, signature := range signatures {
		if strings.Contains(stack, signature) {
			return true
		}
	}
	return false
}

// wsConnect opens a raw WebSocket to the master connect endpoint with the
// slave token, returning the upgraded connection.
func (env *switchTestEnv) wsConnect() *websocket.Conn {
	env.t.Helper()
	wsURL := "ws" + strings.TrimPrefix(env.server.URL, "http") + ConnectPath
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+env.token())
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, headers)
	if resp != nil {
		defer resp.Body.Close()
	}
	require.NoError(env.t, err)
	return conn
}
