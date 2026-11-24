package sfu

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"atomirex.com/umbrella/razor"
	"google.golang.org/protobuf/proto"
)

// newTestServer wires the real HTTP endpoints onto a test SFU, so the tests exercise
// exactly what /servers and /status do (including protobuf encoding).
func newTestServer(t *testing.T) (*Sfu, *fakeRemoteClientFactory, *httptest.Server) {
	t.Helper()

	logger := razor.NewLogger(razor.LoggingLevelOff, false)
	factory := &fakeRemoteClientFactory{}
	s := NewSfuWithFactory(logger, factory)

	mux := http.NewServeMux()
	mux.HandleFunc("/servers", func(w http.ResponseWriter, r *http.Request) {
		if !s.HandleServers(w, r) {
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		if !s.HandleStatus(w, r) {
			http.NotFound(w, r)
		}
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return s, factory, server
}

func postServers(t *testing.T, baseURL string, urls ...string) *CurrentServers {
	t.Helper()

	body, err := proto.Marshal(&CurrentServers{Servers: urls})
	if err != nil {
		t.Fatalf("failed to marshal request: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, baseURL+"/servers", bytesReader(body))
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}
	req.Header.Set("Content-Type", ProtobufContentType)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /servers failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /servers returned %d: %s", resp.StatusCode, raw)
	}

	if ct := resp.Header.Get("Content-Type"); ct != ProtobufContentType {
		t.Fatalf("expected protobuf content type, got %q", ct)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read response: %v", err)
	}

	var result CurrentServers
	if err := proto.Unmarshal(raw, &result); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	return &result
}

func getServers(t *testing.T, baseURL string) *CurrentServers {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, baseURL+"/servers", nil)
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}
	req.Header.Set("Content-Type", ProtobufContentType)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /servers failed: %v", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read response: %v", err)
	}

	var result CurrentServers
	if err := proto.Unmarshal(raw, &result); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	return &result
}

func getStatus(t *testing.T, baseURL string) *SFUStatus {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, baseURL+"/status", nil)
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}
	req.Header.Set("Content-Type", ProtobufContentType)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /status failed: %v", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read response: %v", err)
	}

	var status SFUStatus
	if err := proto.Unmarshal(raw, &status); err != nil {
		t.Fatalf("failed to unmarshal status: %v", err)
	}

	return &status
}

// The POSTed list must reflect what actually happened to the submitted addresses,
// and both endpoints must agree on it.
func TestHTTPServersReflectsConnectionState(t *testing.T) {
	_, factory, server := newTestServer(t)

	// A formed but not yet established connection is not reported as current.
	result := postServers(t, server.URL, "ws://a", "ws://b")
	if len(result.Servers) != 0 {
		t.Fatalf("expected no established servers yet, got %v", result.Servers)
	}
	if len(result.ServerStatuses) != 2 {
		t.Fatalf("expected a status for each submitted address, got %d", len(result.ServerStatuses))
	}
	for _, st := range result.ServerStatuses {
		if st.State != ServerState_Connecting {
			t.Fatalf("%s should be Connecting, got %v", st.Url, st.State)
		}
	}

	factory.lastFor("ws://a").connect()

	waitFor(t, 2*time.Second, "ws://a over HTTP", func() bool {
		return len(getServers(t, server.URL).Servers) == 1
	})

	got := getServers(t, server.URL)
	assertServersEqual(t, serversOf(got), "ws://a")

	// /status reports the same set.
	status := getStatus(t, server.URL)
	assertServersEqual(t, status.Servers, "ws://a")

	if len(status.ServerStatuses) != 2 {
		t.Fatalf("expected both addresses in status, got %d", len(status.ServerStatuses))
	}
}

// A failed address is retried and finally reported as Failed; it never appears in the
// list of current servers, and the GET endpoint tells the same story as the POST.
func TestHTTPServersFailedAddressIsDistinguishable(t *testing.T) {
	s, factory, server := newTestServer(t)
	s.serverRetryBaseDelay = 10 * time.Millisecond
	s.serverRetryMaxDelay = 10 * time.Millisecond
	s.serverMaxConnectAttempts = 2

	postServers(t, server.URL, "ws://dead")
	factory.lastFor("ws://dead").fail(fmt.Errorf("connection refused"))

	waitFor(t, 2*time.Second, "HTTP to show retrying", func() bool {
		got := getServers(t, server.URL)
		return len(got.ServerStatuses) == 1 && got.ServerStatuses[0].State == ServerState_Retrying
	})

	got := getServers(t, server.URL)
	if len(got.Servers) != 0 {
		t.Fatalf("retrying address must not be listed as current, got %v", got.Servers)
	}
	if got.ServerStatuses[0].LastError == "" {
		t.Fatal("retrying status should expose the failure reason")
	}

	waitFor(t, 2*time.Second, "second attempt", func() bool {
		return factory.count() == 2
	})
	factory.lastFor("ws://dead").fail(fmt.Errorf("connection refused again"))

	waitFor(t, 2*time.Second, "HTTP to show failed", func() bool {
		got := getServers(t, server.URL)
		return len(got.ServerStatuses) == 1 && got.ServerStatuses[0].State == ServerState_Failed
	})

	final := postServers(t, server.URL, "ws://dead")
	if len(final.Servers) != 0 {
		t.Fatalf("failed address must not be listed as current, got %v", final.Servers)
	}

	// Resubmitting a failed address is a fresh intent and gets a new attempt.
	if factory.count() != 3 {
		t.Fatalf("expected resubmission to start a new attempt, got %d clients", factory.count())
	}
}

// Deleting an address in the middle of a retry must stop it, via HTTP as well.
func TestHTTPRemoveDuringRetryStopsForGood(t *testing.T) {
	s, factory, server := newTestServer(t)
	s.serverRetryBaseDelay = 40 * time.Millisecond
	s.serverMaxConnectAttempts = 10

	postServers(t, server.URL, "ws://transient")
	factory.lastFor("ws://transient").fail(fmt.Errorf("flaky"))

	waitFor(t, 2*time.Second, "retrying", func() bool {
		got := getServers(t, server.URL)
		return len(got.ServerStatuses) == 1 && got.ServerStatuses[0].State == ServerState_Retrying
	})

	before := factory.count()

	empty := postServers(t, server.URL)
	if len(empty.Servers) != 0 || len(empty.ServerStatuses) != 0 {
		t.Fatalf("removed address must be gone from the response, got %v", empty)
	}

	time.Sleep(150 * time.Millisecond)

	if factory.count() != before {
		t.Fatalf("removed address was retried after removal (%d clients, was %d)", factory.count(), before)
	}
	if got := getServers(t, server.URL); len(got.ServerStatuses) != 0 {
		t.Fatalf("removed address still present over HTTP: %v", got.ServerStatuses)
	}
}

// After a client disconnects, /status must not still describe its tracks or senders.
func TestHTTPStatusClearsDisconnectedClientTracks(t *testing.T) {
	s, _, server := newTestServer(t)

	contributor := &fakeRemoteClient{url: "incoming-a", observer: nil}
	viewer := &fakeRemoteClient{url: "incoming-b", observer: nil}

	s.handler.Send(sfuAddClient, &sfuCommandMessage{remoteClient: contributor})
	s.handler.Send(sfuAddClient, &sfuCommandMessage{remoteClient: viewer})

	track := &incomingTrack{
		descriptor: &TrackDescriptor{UmbrellaId: "UMB_ID1", Kind: TrackKind_Video},
	}
	s.handler.Send(sfuAddOutgoingTracksForIncomingTrack, &sfuCommandMessage{intrack: track, trackOwner: contributor})

	waitFor(t, 2*time.Second, "track visible over HTTP", func() bool {
		return len(getStatus(t, server.URL).RelayingTracks) == 1
	})

	waitFor(t, 2*time.Second, "viewer to have a sender for the track", func() bool {
		viewer.mu.Lock()
		defer viewer.mu.Unlock()
		return viewer.tracksAdded == 1
	})

	// Disconnect the contributor exactly as the client teardown path does.
	s.handler.Send(sfuRemoveClientTracks, &sfuCommandMessage{remoteClient: contributor})
	s.handler.Send(sfuRemoveClient, &sfuCommandMessage{remoteClient: contributor})

	waitFor(t, 2*time.Second, "track removed over HTTP", func() bool {
		return len(getStatus(t, server.URL).RelayingTracks) == 0
	})

	status := getStatus(t, server.URL)
	if len(status.RelayingTracks) != 0 {
		t.Fatalf("relaying tracks should be empty, got %v", status.RelayingTracks)
	}
	if len(status.Clients) != 1 {
		t.Fatalf("expected only the live session in /status, got %d", len(status.Clients))
	}

	// Senders for the departed track are gone from every remaining client.
	for _, c := range status.Clients {
		for _, sender := range c.Senders {
			if sender.UmbrellaId == "UMB_ID1" {
				t.Fatalf("a sender for the disconnected session's track survived: %v", sender)
			}
		}
	}

	waitFor(t, 2*time.Second, "viewer sender removal", func() bool {
		viewer.mu.Lock()
		defer viewer.mu.Unlock()
		return viewer.tracksRemove == 1
	})
}

// A request which is not protobuf must fall through to the HTML page.
func TestHTTPServersIgnoresNonProtobufRequests(t *testing.T) {
	_, _, server := newTestServer(t)

	resp, err := http.Get(server.URL + "/servers")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected non-protobuf GET to be unhandled, got %d", resp.StatusCode)
	}
}

// A tiny reader helper so the tests do not need bytes.Reader imported everywhere.
type byteReader struct {
	data []byte
	pos  int
}

func bytesReader(b []byte) io.Reader { return &byteReader{data: b} }

func (b *byteReader) Read(p []byte) (int, error) {
	if b.pos >= len(b.data) {
		return 0, io.EOF
	}

	n := copy(p, b.data[b.pos:])
	b.pos += n

	return n, nil
}
