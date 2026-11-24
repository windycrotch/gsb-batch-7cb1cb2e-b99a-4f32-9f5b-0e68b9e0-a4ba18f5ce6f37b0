package sfu

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"atomirex.com/umbrella/razor"
)

// fakeRemoteClient is an injectable stand-in for a relay server connection. It records
// how many times it was stopped so tests can assert exactly-once teardown, and it can
// be driven through connect / fail / disconnect from the test goroutine.
type fakeRemoteClient struct {
	url      string
	observer RemoteClientObserver

	mu           sync.Mutex
	stopCount    int
	tracksAdded  int
	tracksRemove int
	evalRequests int

	// stopHook runs (outside the lock) when stop is called, letting tests assert that
	// the SFU asked for exactly one stop per removed connection.
	stopHook func()
}

func (f *fakeRemoteClient) Label() string { return "fake " + f.url }

func (f *fakeRemoteClient) getStatus() *SFUStatusClient { return nil }

func (f *fakeRemoteClient) getStatusWithTimeout(time.Duration) *SFUStatusClient {
	return &SFUStatusClient{Label: f.Label(), TrunkUrl: f.url}
}

func (f *fakeRemoteClient) stop() {
	f.mu.Lock()
	f.stopCount++
	hook := f.stopHook
	f.mu.Unlock()

	if hook != nil {
		hook()
	}
}

func (f *fakeRemoteClient) AddOutgoingTracksForIncomingTrack(*incomingTrack) {
	f.mu.Lock()
	f.tracksAdded++
	f.mu.Unlock()
}

func (f *fakeRemoteClient) RemoveOutgoingTracksForIncomingTrack(*incomingTrack) {
	f.mu.Lock()
	f.tracksRemove++
	f.mu.Unlock()
}

func (f *fakeRemoteClient) RequestEvalState() {
	f.mu.Lock()
	f.evalRequests++
	f.mu.Unlock()
}

func (f *fakeRemoteClient) stopped() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopCount
}

// connect reports a successful connection, as a real client would after its dial.
func (f *fakeRemoteClient) connect() {
	if f.observer != nil {
		f.observer.ServerConnected(f.url, f)
	}
}

func (f *fakeRemoteClient) fail(err error) {
	if f.observer != nil {
		f.observer.ServerConnectFailed(f.url, f, err)
	}
}

func (f *fakeRemoteClient) disconnect(err error) {
	if f.observer != nil {
		f.observer.ServerDisconnected(f.url, f, err)
	}
}

type fakeRemoteClientFactory struct {
	mu      sync.Mutex
	created []*fakeRemoteClient
}

func (f *fakeRemoteClientFactory) NewClient(params *RemoteClientParameters) RemoteClient {
	c := &fakeRemoteClient{url: params.trunkurl, observer: params.observer}

	f.mu.Lock()
	f.created = append(f.created, c)
	f.mu.Unlock()

	return c
}

func (f *fakeRemoteClientFactory) snapshot() []*fakeRemoteClient {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]*fakeRemoteClient, len(f.created))
	copy(out, f.created)
	return out
}

func (f *fakeRemoteClientFactory) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.created)
}

func (f *fakeRemoteClientFactory) lastFor(url string) *fakeRemoteClient {
	f.mu.Lock()
	defer f.mu.Unlock()

	for i := len(f.created) - 1; i >= 0; i-- {
		if f.created[i].url == url {
			return f.created[i]
		}
	}

	return nil
}

func newTestSfu(t *testing.T) (*Sfu, *fakeRemoteClientFactory) {
	t.Helper()

	logger := razor.NewLogger(razor.LoggingLevelOff, false)
	factory := &fakeRemoteClientFactory{}
	s := NewSfuWithFactory(logger, factory)

	return s, factory
}

// waitFor polls cond until it becomes true or the deadline expires. Connection
// lifecycle changes are asynchronous (they travel through the SFU command queue).
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for %s", what)
}

func serversOf(servers *CurrentServers) []string {
	out := make([]string, len(servers.Servers))
	copy(out, servers.Servers)
	return out
}

func serverStates(servers *CurrentServers) map[string]ServerState {
	out := make(map[string]ServerState, len(servers.ServerStatuses))
	for _, st := range servers.ServerStatuses {
		out[st.Url] = st.State
	}
	return out
}

func assertServersEqual(t *testing.T, got []string, want ...string) {
	t.Helper()

	wantSet := make(map[string]bool, len(want))
	for _, w := range want {
		wantSet[w] = true
	}

	if len(got) != len(wantSet) {
		t.Fatalf("expected servers %v, got %v", want, got)
	}

	for _, g := range got {
		if !wantSet[g] {
			t.Fatalf("expected servers %v, got %v", want, got)
		}
	}
}

// A submitted server is not visible as connected until its connection actually
// succeeds, and becomes visible once it does.
func TestServerListOnlyShowsEstablishedConnections(t *testing.T) {
	s, factory := newTestSfu(t)

	result := s.SetCurrentServers(&CurrentServers{Servers: []string{"ws://a"}})
	if len(result.Servers) != 0 {
		t.Fatalf("a server which has not connected yet must not be listed as current, got %v", result.Servers)
	}

	if result.ServerStatuses[0].State != ServerState_Connecting {
		t.Fatalf("expected Connecting while the dial is in flight, got %v", result.ServerStatuses[0].State)
	}

	client := factory.lastFor("ws://a")
	if client == nil {
		t.Fatal("expected a client to be created for the submitted server")
	}

	client.connect()

	waitFor(t, time.Second, "server to be listed as connected", func() bool {
		return len(s.GetCurrentServers().Servers) == 1
	})

	got := s.GetCurrentServers()
	assertServersEqual(t, serversOf(got), "ws://a")
	if got.ServerStatuses[0].State != ServerState_Connected {
		t.Fatalf("expected Connected, got %v", got.ServerStatuses[0].State)
	}
}

// A connection which never succeeds must not occupy the list. It is retried with
// backoff and the retry is observable, then finally gives up with a distinct state.
func TestFailedServerRetriesWithBackoffThenFails(t *testing.T) {
	s, factory := newTestSfu(t)

	s.serverRetryBaseDelay = 60 * time.Millisecond
	s.serverRetryMaxDelay = 60 * time.Millisecond
	s.serverMaxConnectAttempts = 3

	result := s.SetCurrentServers(&CurrentServers{Servers: []string{"ws://bad"}})
	if len(result.Servers) != 0 {
		t.Fatalf("failed server must never be listed as current, got %v", result.Servers)
	}

	if factory.count() != 1 {
		t.Fatalf("expected one connection attempt, got %d", factory.count())
	}

	factory.created[0].fail(fmt.Errorf("dial refused"))

	waitFor(t, time.Second, "server to enter Retrying", func() bool {
		got := s.GetCurrentServers()
		return len(got.ServerStatuses) == 1 && got.ServerStatuses[0].State == ServerState_Retrying
	})

	retrying := s.GetCurrentServers()
	if len(retrying.Servers) != 0 {
		t.Fatalf("retrying server must not be listed as current, got %v", retrying.Servers)
	}
	if retrying.ServerStatuses[0].Attempts != 1 {
		t.Fatalf("expected 1 attempt recorded, got %d", retrying.ServerStatuses[0].Attempts)
	}
	if retrying.ServerStatuses[0].LastError == "" {
		t.Fatal("expected the failure reason to be observable while retrying")
	}
	if retrying.ServerStatuses[0].NextRetryInMillis <= 0 {
		t.Fatal("expected a visible next retry delay while retrying")
	}

	// Each retry uses a brand new client; the failed one is never reused.
	waitFor(t, time.Second, "a retry attempt using a new client", func() bool {
		return factory.count() == 2
	})

	factory.lastFor("ws://bad").fail(fmt.Errorf("still refused"))

	waitFor(t, time.Second, "third attempt", func() bool {
		return factory.count() == 3
	})

	factory.lastFor("ws://bad").fail(fmt.Errorf("refused again"))

	waitFor(t, time.Second, "server to give up", func() bool {
		got := s.GetCurrentServers()
		return len(got.ServerStatuses) == 1 && got.ServerStatuses[0].State == ServerState_Failed
	})

	final := s.GetCurrentServers()
	if len(final.Servers) != 0 {
		t.Fatalf("a failed server must not be listed as current, got %v", final.Servers)
	}
	if final.ServerStatuses[0].Attempts != 3 {
		t.Fatalf("expected 3 attempts recorded, got %d", final.ServerStatuses[0].Attempts)
	}

	// Giving up is terminal: no further attempts appear on their own.
	time.Sleep(50 * time.Millisecond)
	if factory.count() != 3 {
		t.Fatalf("a failed server must stop being retried, got %d attempts", factory.count())
	}
}

// A connection which drops mid-flight must lose its list slot, and the SFU must retry.
func TestDisconnectedServerLeavesListAndIsRetried(t *testing.T) {
	s, factory := newTestSfu(t)
	s.serverRetryBaseDelay = 5 * time.Millisecond

	s.SetCurrentServers(&CurrentServers{Servers: []string{"ws://flaky"}})
	first := factory.lastFor("ws://flaky")
	first.connect()

	waitFor(t, time.Second, "initial connection", func() bool {
		return len(s.GetCurrentServers().Servers) == 1
	})

	first.disconnect(fmt.Errorf("remote hung up"))

	waitFor(t, time.Second, "server to leave the connected list", func() bool {
		return len(s.GetCurrentServers().Servers) == 0
	})

	waitFor(t, time.Second, "reconnection attempt", func() bool {
		return factory.count() == 2
	})

	factory.lastFor("ws://flaky").connect()

	waitFor(t, time.Second, "server back in the list", func() bool {
		return len(s.GetCurrentServers().Servers) == 1
	})
}

// Removing a server which is in the middle of a backoff retry must stop it for good.
func TestRemovedRetryingServerIsStoppedAndNeverRestarted(t *testing.T) {
	s, factory := newTestSfu(t)
	s.serverRetryBaseDelay = 20 * time.Millisecond
	s.serverMaxConnectAttempts = 10

	s.SetCurrentServers(&CurrentServers{Servers: []string{"ws://retrying"}})
	factory.lastFor("ws://retrying").fail(fmt.Errorf("nope"))

	waitFor(t, time.Second, "server to enter Retrying", func() bool {
		got := s.GetCurrentServers()
		return len(got.ServerStatuses) == 1 && got.ServerStatuses[0].State == ServerState_Retrying
	})

	before := factory.count()
	result := s.SetCurrentServers(&CurrentServers{})

	if len(result.Servers) != 0 || len(result.ServerStatuses) != 0 {
		t.Fatalf("removed server must disappear entirely, got %v", result)
	}

	// Wait past the scheduled retry: nothing may be recreated.
	time.Sleep(120 * time.Millisecond)

	if factory.count() != before {
		t.Fatalf("a removed server must not be retried, saw %d clients for %d before removal", factory.count(), before)
	}

	if got := s.GetCurrentServers(); len(got.ServerStatuses) != 0 {
		t.Fatalf("removed server reappeared in /servers: %v", got.ServerStatuses)
	}
}

// Removing a connected server stops it exactly once; a late disconnect report from it
// must not resurrect it or create a new attempt.
func TestRemovedConnectedServerIsStoppedAndLateEventsIgnored(t *testing.T) {
	s, factory := newTestSfu(t)
	s.serverRetryBaseDelay = 5 * time.Millisecond

	s.SetCurrentServers(&CurrentServers{Servers: []string{"ws://live"}})
	client := factory.lastFor("ws://live")
	client.connect()

	waitFor(t, time.Second, "connection", func() bool {
		return len(s.GetCurrentServers().Servers) == 1
	})

	s.SetCurrentServers(&CurrentServers{})

	if n := client.stopped(); n != 1 {
		t.Fatalf("expected the removed server to be stopped exactly once, got %d", n)
	}

	// A real client may report read failure during teardown; that must be inert.
	client.disconnect(fmt.Errorf("closed during removal"))

	time.Sleep(80 * time.Millisecond)

	if got := s.GetCurrentServers(); len(got.ServerStatuses) != 0 {
		t.Fatalf("removed server reappeared after a late disconnect: %v", got.ServerStatuses)
	}
	if factory.count() != 1 {
		t.Fatalf("late disconnect spawned a new connection, got %d clients", factory.count())
	}
}

// Submitting the same address repeatedly, and submitting duplicates in one payload,
// must converge on exactly one connection.
func TestRepeatedAndDuplicateSubmissionsYieldOneConnection(t *testing.T) {
	s, factory := newTestSfu(t)

	s.SetCurrentServers(&CurrentServers{Servers: []string{"ws://dup", "ws://dup", "ws://dup"}})
	s.SetCurrentServers(&CurrentServers{Servers: []string{"ws://dup"}})
	s.SetCurrentServers(&CurrentServers{Servers: []string{"ws://dup"}})

	if factory.count() != 1 {
		t.Fatalf("expected exactly one connection for duplicate submissions, got %d", factory.count())
	}

	factory.lastFor("ws://dup").connect()

	waitFor(t, time.Second, "single connected server", func() bool {
		return len(s.GetCurrentServers().Servers) == 1
	})

	got := s.GetCurrentServers()
	assertServersEqual(t, serversOf(got), "ws://dup")

	// And no duplicate entries leak into the status list either.
	if len(got.ServerStatuses) != 1 {
		t.Fatalf("expected one status entry for one address, got %d", len(got.ServerStatuses))
	}
}

// Removing then re-adding the same address must build a new connection rather than
// reuse the stopped one.
func TestRemoveThenReAddCreatesFreshConnection(t *testing.T) {
	s, factory := newTestSfu(t)

	s.SetCurrentServers(&CurrentServers{Servers: []string{"ws://cycle"}})
	first := factory.lastFor("ws://cycle")
	first.connect()

	waitFor(t, time.Second, "first connection", func() bool {
		return len(s.GetCurrentServers().Servers) == 1
	})

	s.SetCurrentServers(&CurrentServers{})
	if first.stopped() != 1 {
		t.Fatalf("expected first connection to be stopped, got %d stops", first.stopped())
	}

	s.SetCurrentServers(&CurrentServers{Servers: []string{"ws://cycle"}})

	if factory.count() != 2 {
		t.Fatalf("expected a new client after re-adding, got %d clients", factory.count())
	}

	second := factory.lastFor("ws://cycle")
	if second == first {
		t.Fatal("re-added address reused the stopped client")
	}

	// The old connection must not be able to make the address look connected again.
	first.connect()
	time.Sleep(40 * time.Millisecond)

	if got := s.GetCurrentServers(); len(got.Servers) != 0 {
		t.Fatalf("a stale connection made the server look connected: %v", got.Servers)
	}

	second.connect()

	waitFor(t, time.Second, "second connection", func() bool {
		return len(s.GetCurrentServers().Servers) == 1
	})
}

// When a client which contributed inbound tracks disconnects, its tracks and the
// matching senders must disappear from /status.
func TestClientDisconnectRemovesItsTracksAndSenders(t *testing.T) {
	s, _ := newTestSfu(t)

	contributor := &fakeRemoteClient{url: "incoming-a"}
	viewer := &fakeRemoteClient{url: "incoming-b"}

	s.handler.Send(sfuAddClient, &sfuCommandMessage{remoteClient: contributor})
	s.handler.Send(sfuAddClient, &sfuCommandMessage{remoteClient: viewer})

	track := &incomingTrack{descriptor: &TrackDescriptor{UmbrellaId: "UMB_ID1"}}
	s.handler.Send(sfuAddOutgoingTracksForIncomingTrack, &sfuCommandMessage{intrack: track, trackOwner: contributor})

	waitFor(t, time.Second, "track to be relaying", func() bool {
		return len(s.GetStatus().RelayingTracks) == 1
	})

	// Every client, including the viewer, was told to forward it.
	waitFor(t, time.Second, "viewer to pick up the track", func() bool {
		viewer.mu.Lock()
		defer viewer.mu.Unlock()
		return viewer.tracksAdded == 1
	})

	// The contributor goes away.
	s.handler.Send(sfuRemoveClientTracks, &sfuCommandMessage{remoteClient: contributor})
	s.handler.Send(sfuRemoveClient, &sfuCommandMessage{remoteClient: contributor})

	waitFor(t, time.Second, "track to stop relaying", func() bool {
		return len(s.GetStatus().RelayingTracks) == 0
	})

	status := s.GetStatus()
	if len(status.RelayingTracks) != 0 {
		t.Fatalf("relaying tracks should be empty after disconnect, got %v", status.RelayingTracks)
	}

	started := make(chan int32, 1)
	go func() {
		started <- int32(len(s.GetStatus().Clients))
	}()

	select {
	case n := <-started:
		if n != 1 {
			t.Fatalf("expected only the viewer's session in /status, got %d", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("/status blocked waiting for a disconnected session")
	}

	// The viewer must be told to remove the sender for the departed track.
	waitFor(t, time.Second, "viewer to drop the track", func() bool {
		viewer.mu.Lock()
		defer viewer.mu.Unlock()
		return viewer.tracksRemove == 1
	})
}

// Concurrent submissions and connection events must still converge on the addresses of
// the last accepted submission.
func TestConcurrentSubmissionsConvergeOnLastIntent(t *testing.T) {
	s, factory := newTestSfu(t)

	s.SetCurrentServers(&CurrentServers{Servers: []string{"ws://one", "ws://two"}})

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			switch i % 3 {
			case 0:
				s.GetCurrentServers()
			case 1:
				s.SetCurrentServers(&CurrentServers{Servers: []string{"ws://one", "ws://two"}})
			case 2:
				if c := factory.lastFor("ws://one"); c != nil {
					c.connect()
				}
			}
		}(i)
	}
	wg.Wait()

	// The final intent is a single reachable address, and only that one may remain.
	s.SetCurrentServers(&CurrentServers{Servers: []string{"ws://two"}})

	final := s.GetCurrentServers()
	if len(final.ServerStatuses) != 1 || final.ServerStatuses[0].Url != "ws://two" {
		t.Fatalf("expected only the last submitted address, got %v", final.ServerStatuses)
	}

	// A stale event from the removed address must be ignored.
	if c := factory.lastFor("ws://one"); c != nil {
		c.connect()
		c.disconnect(fmt.Errorf("late"))
	}

	time.Sleep(60 * time.Millisecond)

	after := s.GetCurrentServers()
	if len(after.ServerStatuses) != 1 || after.ServerStatuses[0].Url != "ws://two" {
		t.Fatalf("stale events from a removed address leaked in: %v", after.ServerStatuses)
	}

	if _, exists := serverStates(after)["ws://one"]; exists {
		t.Fatalf("removed address is still present: %v", after.ServerStatuses)
	}
}

// A connection attempt which never reports an outcome is timed out and retried, so the
// address cannot sit forever in a "connecting" limbo.
func TestConnectTimeoutTreatsSilentAttemptAsFailure(t *testing.T) {
	s, factory := newTestSfu(t)
	s.serverConnectTimeout = 10 * time.Millisecond
	s.serverRetryBaseDelay = 5 * time.Millisecond
	s.serverMaxConnectAttempts = 2

	s.SetCurrentServers(&CurrentServers{Servers: []string{"ws://silent"}})

	waitFor(t, time.Second, "silent attempt to be retried", func() bool {
		return factory.count() == 2
	})

	waitFor(t, time.Second, "second attempt to be timed out", func() bool {
		got := s.GetCurrentServers()
		return len(got.ServerStatuses) == 1 && got.ServerStatuses[0].State == ServerState_Failed
	})

	if len(s.GetCurrentServers().Servers) != 0 {
		t.Fatal("a timed out server must not be listed as current")
	}
}

// When a relay server connection drops, the media tracks it contributed must stop
// being relayed, and removing it must not leave orphaned tracks behind either.
func TestServerDisconnectDropsItsContributedTracks(t *testing.T) {
	s, factory := newTestSfu(t)
	s.serverRetryBaseDelay = 5 * time.Millisecond

	s.SetCurrentServers(&CurrentServers{Servers: []string{"ws://relay"}})
	relay := factory.lastFor("ws://relay")
	relay.connect()

	waitFor(t, time.Second, "relay connected", func() bool {
		return len(s.GetCurrentServers().Servers) == 1
	})

	// Simulate the relay contributing an inbound track (as fanoutIncoming does).
	intrack := &incomingTrack{descriptor: &TrackDescriptor{UmbrellaId: "UMB_IDRELAY", Kind: TrackKind_Video}}
	s.handler.Send(sfuAddOutgoingTracksForIncomingTrack, &sfuCommandMessage{intrack: intrack, trackOwner: relay})

	waitFor(t, time.Second, "track relaying", func() bool {
		return len(s.GetStatus().RelayingTracks) == 1
	})

	// The relay's media session ends, then the client tears down and is removed.
	s.handler.Send(sfuRemoveClient, &sfuCommandMessage{remoteClient: relay})

	waitFor(t, time.Second, "relay track removed on disconnect", func() bool {
		return len(s.GetStatus().RelayingTracks) == 0
	})

	// Removing the address must not resurrect or retain anything either.
	s.SetCurrentServers(&CurrentServers{})

	status := s.GetStatus()
	if len(status.RelayingTracks) != 0 {
		t.Fatalf("tracks survived the relay removal: %v", status.RelayingTracks)
	}
	if len(status.ServerStatuses) != 0 {
		t.Fatalf("removed relay is still in status: %v", status.ServerStatuses)
	}
}

// A client whose peer connections cannot be constructed must be reported as a failed
// attempt (retried with backoff) rather than being left with no handler, which used to
// make stop() panic and take the whole process down.
func TestClientConstructionFailureIsAFailedAttemptNotAPanic(t *testing.T) {
	logger := razor.NewLogger(razor.LoggingLevelOff, false)

	factory := &failingPeerConnectionFactory{}

	s := NewSfuWithFactory(logger, &defaultTrunkClientFactory{pcFactory: factory})
	s.serverRetryBaseDelay = 5 * time.Millisecond
	s.serverMaxConnectAttempts = 2

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("constructing a client with no peer connection panicked: %v", r)
		}
	}()

	s.SetCurrentServers(&CurrentServers{Servers: []string{"ws://nopc"}})

	waitFor(t, 2*time.Second, "construction failure to be retried then given up", func() bool {
		got := s.GetCurrentServers()
		return len(got.ServerStatuses) == 1 && got.ServerStatuses[0].State == ServerState_Failed
	})

	// Removing it must not panic either (stop on a handler-less client).
	s.SetCurrentServers(&CurrentServers{})

	if got := s.GetCurrentServers(); len(got.ServerStatuses) != 0 {
		t.Fatalf("address still present after removal: %v", got.ServerStatuses)
	}
}

// failingPeerConnectionFactory always fails, simulating resource exhaustion.
type failingPeerConnectionFactory struct{}

func (f *failingPeerConnectionFactory) NewPeerConnection(string) (*PeerConnection, error) {
	return nil, fmt.Errorf("no peer connections available")
}

// defaultTrunkClientFactory builds the real trunk client but with an injectable peer
// connection factory.
type defaultTrunkClientFactory struct {
	pcFactory PeerConnectionFactory
}

func (f *defaultTrunkClientFactory) NewClient(params *RemoteClientParameters) RemoteClient {
	c := &client{
		BaseClient: BaseClient{
			label:  "Trunking client to " + params.trunkurl,
			logger: params.logger,
		},
		trunkurl: params.trunkurl,
		observer: params.observer,
	}

	c.runWithPeerConnectionFactory(nil, params.s, f.pcFactory)

	return c
}
