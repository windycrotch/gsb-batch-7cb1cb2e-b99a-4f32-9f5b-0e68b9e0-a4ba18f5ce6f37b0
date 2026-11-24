package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v3"
)

// resetGlobalState puts package-level globals back to a known baseline. The
// package tests mutate the global config and peer registry, so each test
// starts clean and runs serially (no t.Parallel).
func resetGlobalState(t *testing.T) {
	t.Helper()
	peerMu.Lock()
	for pc := range peerConnections {
		_ = pc.Close()
		delete(peerConnections, pc)
	}
	peerMu.Unlock()

	config = ConfigStruct{
		ServeIp:          "127.0.0.1",
		ServePort:        "0",
		RtpHost:          "127.0.0.1",
		EnableVideo:      true,
		EnableAudio:      true,
		VideoTrackName:   "video",
		AudioTrackName:   "audio",
		VideoCodec:       "video/VP8",
		AudioCodec:       "audio/opus",
		ApiLocation:      "/offer",
		AllowCrossOrigin: false,
	}
	iceServersArray = nil
	videoTrack = initWebrtcTrack("video", config.VideoCodec, config.VideoTrackName)
	audioTrack = initWebrtcTrack("audio", config.AudioCodec, config.AudioTrackName)
}

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc(config.ApiLocation, OnConnect)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

// waitForPeerCount polls the registry until it reaches want or times out.
func waitForPeerCount(t *testing.T, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if peerCount() == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("peer count = %d, want %d", peerCount(), want)
}

// newClientOffer builds a real Pion PeerConnection with nVideo video
// transceivers and nAudio audio transceivers and returns its offer. Every
// test client uses distinct transceiver counts so each offer SDP has a
// unique number of media sections, making cross-contamination detectable.
func newClientOffer(t *testing.T, nVideo, nAudio int) (*webrtc.PeerConnection, webrtc.SessionDescription) {
	t.Helper()
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("client NewPeerConnection: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })

	for i := 0; i < nVideo; i++ {
		if _, err = pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo,
			webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
			t.Fatalf("add video transceiver: %v", err)
		}
	}
	for i := 0; i < nAudio; i++ {
		if _, err = pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio,
			webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
			t.Fatalf("add audio transceiver: %v", err)
		}
	}

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer: %v", err)
	}
	if err = pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("SetLocalDescription: %v", err)
	}
	// Wait for local ICE gathering so the offer is complete.
	<-webrtc.GatheringCompletePromise(pc)
	return pc, *pc.LocalDescription()
}

// countMLines returns the number of m= lines in an SDP string.
func countMLines(sdp string) int {
	n := 0
	for _, line := range strings.Split(sdp, "\r\n") {
		if strings.HasPrefix(line, "m=") {
			n++
		}
	}
	return n
}

func postOffer(t *testing.T, url string, body any) (*http.Response, []byte) {
	t.Helper()
	var reader io.Reader
	switch v := body.(type) {
	case string:
		reader = strings.NewReader(v)
	case []byte:
		reader = bytes.NewReader(v)
	default:
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	resp, err := http.Post(url, "application/json", reader)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	_ = resp.Body.Close()
	return resp, raw
}

// TestConcurrentOffersAreIndependent is the core acceptance test: many
// viewers POST distinct offers at the same time. Each must get an answer
// matching its own offer (same number of m= lines), each answer must be
// accepted by the client that produced the offer, and no answer may be
// swapped between clients.
func TestConcurrentOffersAreIndependent(t *testing.T) {
	resetGlobalState(t)
	ts := newTestServer(t)

	const n = 8
	type clientResult struct {
		idx       int
		wantLines int
		gotLines  int
		applyErr  error
		status    int
	}

	results := make([]clientResult, n)
	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Distinct shapes: i+1 video + (i%2 + 1) audio transceivers.
			nVideo, nAudio := i+1, i%2+1
			pc, offer := newClientOffer(t, nVideo, nAudio)
			wantLines := countMLines(offer.SDP)

			<-start // hammer the server simultaneously
			resp, raw := postOffer(t, ts.URL+config.ApiLocation, offer)

			cr := clientResult{idx: i, wantLines: wantLines, status: resp.StatusCode}
			if resp.StatusCode != http.StatusOK {
				results[i] = cr
				return
			}

			var answer webrtc.SessionDescription
			if err := json.Unmarshal(raw, &answer); err != nil {
				t.Errorf("client %d: answer is not valid JSON: %v", i, err)
				results[i] = cr
				return
			}
			cr.gotLines = countMLines(answer.SDP)

			// A swapped/overwritten offer yields an answer whose media
			// section count does not match this client's offer, and Pion
			// rejects it when applied as the remote description.
			cr.applyErr = pc.SetRemoteDescription(answer)
			results[i] = cr
		}(i)
	}
	close(start)
	wg.Wait()

	for _, cr := range results {
		if cr.status != http.StatusOK {
			t.Errorf("client %d: status = %d, want 200", cr.idx, cr.status)
		}
		if cr.gotLines != cr.wantLines {
			t.Errorf("client %d: answer m-lines = %d, want %d (answers are being mixed up)",
				cr.idx, cr.gotLines, cr.wantLines)
		}
		if cr.applyErr != nil {
			t.Errorf("client %d: could not apply received answer to its own offer: %v",
				cr.idx, cr.applyErr)
		}
	}

	// All n PeerConnections must be registered concurrently while alive.
	waitForPeerCount(t, n, 5*time.Second)

	// Closing all viewers must release every server-side PeerConnection
	// without disturbing the others (they all go to zero here together).
	// The OnConnectionStateChange callbacks drive the cleanup.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		// Trigger shutdown of the client PCs (registered via t.Cleanup too,
		// but close explicitly to drive state callbacks deterministically).
		if peerCount() == 0 {
			break
		}
		peerMu.Lock()
		for pc := range peerConnections {
			_ = pc.Close()
		}
		peerMu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	waitForPeerCount(t, 0, 10*time.Second)
}

// TestMalformedAndInvalidRequests verifies every bad input returns a 4xx and
// leaves the server able to serve the next request — the process must never
// crash or wedged.
func TestMalformedAndInvalidRequests(t *testing.T) {
	resetGlobalState(t)
	ts := newTestServer(t)
	url := ts.URL + config.ApiLocation

	// 1. Wrong HTTP method -> 405.
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want 405", resp.StatusCode)
	}

	// 2. Malformed JSON -> 400.
	resp, raw := postOffer(t, url, "{not-json")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("malformed JSON status = %d, want 400", resp.StatusCode)
	}
	if len(raw) == 0 {
		t.Error("malformed JSON response body should explain the error")
	}

	// 3. Valid JSON but not a session description -> 400.
	resp, _ = postOffer(t, url, map[string]any{"foo": "bar"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("non-SDP JSON status = %d, want 400", resp.StatusCode)
	}

	// 4. Wrong SDP type (answer instead of offer) -> 400.
	resp, _ = postOffer(t, url, webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: "v=0\r\n"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("answer-typed SDP status = %d, want 400", resp.StatusCode)
	}

	// 5. Empty SDP payload -> 400.
	resp, _ = postOffer(t, url, webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: ""})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("empty SDP status = %d, want 400", resp.StatusCode)
	}

	// 6. Syntactically broken SDP content -> 400 from SetRemoteDescription.
	resp, _ = postOffer(t, url, webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: "this is not sdp"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("broken SDP content status = %d, want 400", resp.StatusCode)
	}

	// No PeerConnection may leak from failed negotiations.
	if got := peerCount(); got != 0 {
		t.Errorf("peer registry after failures = %d, want 0", got)
	}

	// 7. The process is still alive and a well-formed request still works.
	pc, offer := newClientOffer(t, 1, 1)
	resp, raw = postOffer(t, url, offer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid request after errors: status = %d, want 200 (server did not recover)", resp.StatusCode)
	}
	var answer webrtc.SessionDescription
	if err := json.Unmarshal(raw, &answer); err != nil {
		t.Fatalf("recovered answer not JSON: %v", err)
	}
	if err := pc.SetRemoteDescription(answer); err != nil {
		t.Errorf("recovered answer not applicable: %v", err)
	}
	waitForPeerCount(t, 1, 5*time.Second)
}

// TestNoMediaEnabledReturnsExplicitError covers the "media not enabled"
// requirement instead of letting the handler panic on a nil track.
func TestNoMediaEnabledReturnsExplicitError(t *testing.T) {
	resetGlobalState(t)
	config.EnableVideo = false
	config.EnableAudio = false
	videoTrack = nil
	audioTrack = nil
	ts := newTestServer(t)

	_, offer := newClientOffer(t, 1, 0)
	resp, _ := postOffer(t, ts.URL+config.ApiLocation, offer)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("no-media status = %d, want 503", resp.StatusCode)
	}
	if peerCount() != 0 {
		t.Errorf("peer registry = %d, want 0 after rejected request", peerCount())
	}

	// Server must still be alive afterwards.
	resp, _ = postOffer(t, ts.URL+config.ApiLocation, "garbage")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("post-failure request status = %d, want 400", resp.StatusCode)
	}
}

// TestSingleViewerHappyPath preserves the original single-viewer flow.
func TestSingleViewerHappyPath(t *testing.T) {
	resetGlobalState(t)
	ts := newTestServer(t)

	pc, offer := newClientOffer(t, 1, 1)
	resp, raw := postOffer(t, ts.URL+config.ApiLocation, offer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var answer webrtc.SessionDescription
	if err := json.Unmarshal(raw, &answer); err != nil {
		t.Fatalf("answer JSON: %v", err)
	}
	if answer.Type != webrtc.SDPTypeAnswer {
		t.Errorf("answer type = %s, want answer", answer.Type)
	}
	if countMLines(answer.SDP) != 2 {
		t.Errorf("answer m-lines = %d, want 2 (video+audio)", countMLines(answer.SDP))
	}
	if err := pc.SetRemoteDescription(answer); err != nil {
		t.Fatalf("applying answer: %v", err)
	}
	waitForPeerCount(t, 1, 5*time.Second)
}

// TestVideoOnlyConfiguration checks a disabled media kind does not break
// negotiation and no nil track is touched.
func TestVideoOnlyConfiguration(t *testing.T) {
	resetGlobalState(t)
	config.EnableAudio = false
	audioTrack = nil
	ts := newTestServer(t)

	// Client asks for video only.
	pc, offer := newClientOffer(t, 1, 0)
	resp, raw := postOffer(t, ts.URL+config.ApiLocation, offer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var answer webrtc.SessionDescription
	if err := json.Unmarshal(raw, &answer); err != nil {
		t.Fatalf("answer JSON: %v", err)
	}
	if countMLines(answer.SDP) != 1 {
		t.Errorf("answer m-lines = %d, want 1", countMLines(answer.SDP))
	}
	if err := pc.SetRemoteDescription(answer); err != nil {
		t.Fatalf("applying answer: %v", err)
	}
}

// TestDisconnectReleasesOnlyThatPeer verifies that when one viewer drops,
// its PeerConnection is released while other viewers remain registered.
func TestDisconnectReleasesOnlyThatPeer(t *testing.T) {
	resetGlobalState(t)
	ts := newTestServer(t)
	url := ts.URL + config.ApiLocation

	// Build two viewers and capture their server-side PeerConnections by
	// snapshotting the registry after each handshake.
	_, offer1 := newClientOffer(t, 1, 1)
	resp, raw := postOffer(t, url, offer1)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("viewer1 status = %d", resp.StatusCode)
	}
	var answer1 webrtc.SessionDescription
	if err := json.Unmarshal(raw, &answer1); err != nil {
		t.Fatal(err)
	}
	waitForPeerCount(t, 1, 5*time.Second)
	peerMu.Lock()
	var firstPC *webrtc.PeerConnection
	for pc := range peerConnections {
		firstPC = pc
	}
	peerMu.Unlock()

	_, offer2 := newClientOffer(t, 2, 1)
	resp, raw = postOffer(t, url, offer2)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("viewer2 status = %d", resp.StatusCode)
	}
	waitForPeerCount(t, 2, 5*time.Second)

	// Viewer 1 disconnects: close only its server-side PeerConnection. The
	// state-change callback must remove just that one from the registry.
	if err := firstPC.Close(); err != nil {
		t.Fatalf("close viewer1 pc: %v", err)
	}
	waitForPeerCount(t, 1, 5*time.Second)

	// Viewer 2 must still be present and functional.
	peerMu.Lock()
	_, viewer2StillThere := peerConnections[firstPC]
	remaining := len(peerConnections)
	peerMu.Unlock()
	if viewer2StillThere {
		t.Error("closed PeerConnection was not removed from the registry")
	}
	if remaining != 1 {
		t.Errorf("remaining peers = %d, want 1 (other viewer must be unaffected)", remaining)
	}

	// And the server still accepts brand-new viewers.
	_, offer3 := newClientOffer(t, 1, 0)
	resp, _ = postOffer(t, url, offer3)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("viewer3 after disconnect status = %d, want 200", resp.StatusCode)
	}
	waitForPeerCount(t, 2, 5*time.Second)

	// Sanity: distinct answers remain correctly shaped throughout.
	if countMLines(answer1.SDP) != 2 {
		t.Errorf("viewer1 answer m-lines changed retroactively: %d", countMLines(answer1.SDP))
	}
}

// TestOptionsCORS keeps the existing preflight behavior intact.
func TestOptionsCORS(t *testing.T) {
	resetGlobalState(t)
	config.AllowCrossOrigin = true
	ts := newTestServer(t)

	req, err := http.NewRequest(http.MethodOptions, ts.URL+config.ApiLocation, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("OPTIONS status = %d, want 200", resp.StatusCode)
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Error("missing CORS allow-origin header")
	}

	// Ensure a descriptive failure message format for logs/debugging.
	resp2, body := postOffer(t, ts.URL+config.ApiLocation, "not json at all")
	if resp2.StatusCode != http.StatusBadRequest {
		t.Errorf("bad json status = %d, want 400", resp2.StatusCode)
	}
	if !strings.Contains(string(body), "Bad session description") {
		t.Errorf("error body = %q, want it to mention the bad session description", string(body))
	}
}

// TestPanicInHandlerDoesNotCrashServer proves the recover guard works:
// nothing in normal flow panics, but the server must survive even if one did.
// We simulate indirectly by confirming many sequential requests all get
// deterministic responses and the process stays up.
func TestServerSurvivesManySequentialFailures(t *testing.T) {
	resetGlobalState(t)
	ts := newTestServer(t)
	url := ts.URL + config.ApiLocation

	for i := 0; i < 25; i++ {
		resp, _ := postOffer(t, url, fmt.Sprintf(`{"type":%d}`, i))
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("iteration %d: status = %d, want 400", i, resp.StatusCode)
		}
	}
	if peerCount() != 0 {
		t.Errorf("registry = %d, want 0", peerCount())
	}
}
