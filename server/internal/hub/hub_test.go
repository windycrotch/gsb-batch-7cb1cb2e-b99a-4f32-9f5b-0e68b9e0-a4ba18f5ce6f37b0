package hub_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/whispem/ambivox/server/internal/hub"
	"github.com/whispem/ambivox/server/internal/medialink"
	"github.com/whispem/ambivox/server/internal/medialink/medialinktest"
	"github.com/whispem/ambivox/server/internal/metrics"
	"github.com/whispem/ambivox/server/internal/testclient"
)

const frame = 20 * time.Millisecond

type fixture struct {
	t        *testing.T
	ctx      context.Context
	engine   *medialinktest.Engine
	link     *medialink.Link
	hub      *hub.Hub
	registry *metrics.Registry
	url      string
}

func setup(t *testing.T, maxRooms int, options ...func(*hub.Config)) *fixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	engine := medialinktest.Start(t)
	link := medialink.Dial(ctx, engine.Path, log)
	registry := metrics.NewRegistry()
	cfg := hub.Config{
		Link:            link,
		Logger:          log,
		Metrics:         registry,
		MaxRooms:        maxRooms,
		IncludeLoopback: true,
		DisableMDNS:     true,
	}
	for _, option := range options {
		option(&cfg)
	}
	h, err := hub.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(httptestHandler(h))
	t.Cleanup(func() {
		h.Close()
		server.Close()
		cancel()
		<-link.Done()
	})

	f := &fixture{t: t, ctx: ctx, engine: engine, link: link, hub: h, registry: registry}
	f.url = "ws" + strings.TrimPrefix(server.URL, "http")
	f.eventually("media link", link.Connected)
	return f
}

func (f *fixture) eventually(what string, cond func() bool) {
	f.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			f.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (f *fixture) join(room, lang, name string) (*testclient.Client, testclient.Event) {
	f.t.Helper()
	client, welcome, err := testclient.Join(f.ctx, f.url, room, lang, name)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(client.Close)
	return client, welcome
}

func (f *fixture) connect(clients ...*testclient.Client) {
	f.t.Helper()
	for _, client := range clients {
		if err := client.Connect(f.ctx); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *fixture) wait(client *testclient.Client, kind string) testclient.Event {
	f.t.Helper()
	event, err := client.Wait(f.ctx, kind)
	if err != nil {
		f.t.Fatal(err)
	}
	return event
}

// talk sends marked packets from one client until the other has heard enough of them.
func (f *fixture) talk(from, to *testclient.Client, marker byte, want int) {
	f.t.Helper()
	heard := func() int {
		n := 0
		for _, packet := range to.Packets() {
			if len(packet) == 4 && packet[1] == marker {
				n++
			}
		}
		return n
	}
	deadline := time.Now().Add(10 * time.Second)
	for i := 0; heard() < want; i++ {
		if time.Now().After(deadline) {
			f.t.Fatalf("listener heard %d of %d packets", heard(), want)
		}
		if err := from.WriteAudio([]byte{0x78, marker, byte(i), 0xff}, frame); err != nil {
			f.t.Fatal(err)
		}
		time.Sleep(frame)
	}
}

func TestTranslatedCall(t *testing.T) {
	f := setup(t, 10)
	alice, welcome := f.join("kitchen", "en", "  Alice   Liddell ")
	if welcome["peer"] != nil || welcome.String("backend") != "fake" || welcome["connected"] != true {
		t.Errorf("first welcome = %v", welcome)
	}
	if welcome.String("name") != "Alice Liddell" {
		t.Errorf("name = %q", welcome.String("name"))
	}

	bob, welcome := f.join("kitchen", "fr", "Bob")
	peer, _ := welcome["peer"].(map[string]any)
	if peer["id"] != alice.ID || peer["lang"] != "en" || peer["name"] != "Alice Liddell" {
		t.Errorf("second welcome peer = %v", peer)
	}
	joined := f.wait(alice, "peer-joined")
	if joined["peer"].(map[string]any)["id"] != bob.ID {
		t.Errorf("peer-joined = %v", joined)
	}

	f.connect(alice, bob)
	f.talk(alice, bob, 0xa1, 10)
	f.talk(bob, alice, 0xb2, 10)

	for _, listener := range []*testclient.Client{alice, bob} {
		draft := f.wait(listener, "caption")
		if draft["final"] != false || draft.String("kind") != "source" || draft.String("text") != "heard" {
			t.Errorf("draft caption = %v", draft)
		}
		source := f.wait(listener, "caption")
		translation := f.wait(listener, "caption")
		if source["final"] != true || translation["final"] != true {
			t.Errorf("final flags = %v, %v", source["final"], translation["final"])
		}
		if source.String("from") != alice.ID || source.String("kind") != "source" || source.String("text") != "heard en" {
			t.Errorf("source caption = %v", source)
		}
		if translation.String("kind") != "translation" || translation.String("text") != "said fr" {
			t.Errorf("translation caption = %v", translation)
		}
	}
	if f.engine.Opened() != 2 {
		t.Errorf("engine opened %d streams, want one per direction", f.engine.Opened())
	}

	var exposition bytes.Buffer
	if _, err := f.registry.WriteTo(&exposition); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"ambivox_rooms 1\n", "ambivox_peers 2\n", "ambivox_media_connected 1\n", "ambivox_captions_total 8\n", "ambivox_partial_captions_total 4\n"} {
		if !strings.Contains(exposition.String(), line) {
			t.Errorf("metrics lack %q:\n%s", line, exposition.String())
		}
	}
	if strings.Contains(exposition.String(), "ambivox_rtp_packets_relayed_total 0\n") == false {
		t.Errorf("a translated call must not relay packets directly:\n%s", exposition.String())
	}
}

func TestSameLanguageIsRelayedDirectly(t *testing.T) {
	f := setup(t, 10)
	alice, _ := f.join("porch", "it", "Alice")
	bob, _ := f.join("porch", "it", "Bob")
	f.connect(alice, bob)
	f.talk(alice, bob, 0xa1, 10)

	if f.engine.Opened() != 0 {
		t.Errorf("engine opened %d streams for a same-language call", f.engine.Opened())
	}
	if captions := bob.Events("caption"); len(captions) != 0 {
		t.Errorf("unexpected captions: %v", captions)
	}
}

func TestRoomRules(t *testing.T) {
	f := setup(t, 1)
	f.join("attic", "en", "Alice")
	f.join("attic", "fr", "Bob")

	refusals := []struct {
		room, lang, reason string
	}{
		{"attic", "es", "two participants"},
		{"cellar", "en", "room limit"},
		{"bad room!", "en", "room names"},
		{"attic", "de", "unsupported language"},
	}
	for _, refusal := range refusals {
		_, _, err := testclient.Join(f.ctx, f.url, refusal.room, refusal.lang, "Eve")
		if err == nil {
			t.Fatalf("join(%q, %q) succeeded", refusal.room, refusal.lang)
		}
		client, err := testclient.Dial(f.ctx, f.url)
		if err != nil {
			t.Fatal(err)
		}
		_ = client.Send(testclient.Event{"type": "join", "room": refusal.room, "lang": refusal.lang})
		event := f.wait(client, "error")
		if !strings.Contains(event.String("message"), refusal.reason) {
			t.Errorf("join(%q, %q): %q lacks %q", refusal.room, refusal.lang, event.String("message"), refusal.reason)
		}
		if code, err := client.WaitClosed(f.ctx); err != nil || code != websocket.StatusPolicyViolation {
			t.Errorf("close code = %v, %v", code, err)
		}
	}

	client, err := testclient.Dial(f.ctx, f.url)
	if err != nil {
		t.Fatal(err)
	}
	_ = client.Send(testclient.Event{"type": "offer", "sdp": "v=0"})
	if event := f.wait(client, "error"); !strings.Contains(event.String("message"), "join") {
		t.Errorf("offer before join: %v", event)
	}
}

func TestSignallingErrorsKeepTheSessionOpen(t *testing.T) {
	f := setup(t, 10)
	alice, _ := f.join("study", "en", "Alice")

	_ = alice.Send(testclient.Event{"type": "dance"})
	if event := f.wait(alice, "error"); !strings.Contains(event.String("message"), "dance") {
		t.Errorf("unknown type: %v", event)
	}
	_ = alice.Send(testclient.Event{"type": "offer", "sdp": "not sdp"})
	if event := f.wait(alice, "error"); !strings.Contains(event.String("message"), "negotiation failed") {
		t.Errorf("bad offer: %v", event)
	}

	f.connect(alice)
	_ = alice.Send(testclient.Event{"type": "offer", "sdp": "v=0"})
	if event := f.wait(alice, "error"); !strings.Contains(event.String("message"), "already") {
		t.Errorf("second offer: %v", event)
	}
}

func TestPartnersCanChange(t *testing.T) {
	f := setup(t, 10)
	alice, _ := f.join("garden", "en", "Alice")
	bob, _ := f.join("garden", "fr", "Bob")
	f.wait(alice, "peer-joined")
	f.connect(alice, bob)
	f.talk(alice, bob, 0xa1, 5)

	bob.Close()
	if left := f.wait(alice, "peer-left"); left.String("id") != bob.ID {
		t.Errorf("peer-left = %v", left)
	}
	f.eventually("room to shrink", func() bool { return f.hub.Peers() == 1 })

	carol, welcome := f.join("garden", "es", "Carol")
	if welcome["peer"].(map[string]any)["id"] != alice.ID {
		t.Errorf("welcome = %v", welcome)
	}
	f.wait(alice, "peer-joined")
	f.connect(carol)
	f.talk(alice, carol, 0xa2, 5)
	f.talk(carol, alice, 0xc3, 5)

	alice.Close()
	carol.Close()
	f.eventually("rooms to empty", func() bool { return f.hub.Rooms() == 0 && f.hub.Peers() == 0 })
}

func TestCallSurvivesAnEngineRestart(t *testing.T) {
	f := setup(t, 10)
	alice, _ := f.join("lab", "en", "Alice")
	bob, _ := f.join("lab", "fr", "Bob")
	f.connect(alice, bob)
	f.talk(alice, bob, 0xa1, 5)

	f.engine.Stop()
	for _, client := range []*testclient.Client{alice, bob} {
		if status := f.wait(client, "media"); status["connected"] != false {
			t.Errorf("status after stop = %v", status)
		}
	}
	if err := f.engine.Listen(); err != nil {
		t.Fatal(err)
	}
	for _, client := range []*testclient.Client{alice, bob} {
		if status := f.wait(client, "media"); status["connected"] != true || status.String("backend") != "fake" {
			t.Errorf("status after restart = %v", status)
		}
	}
	f.talk(alice, bob, 0xa2, 5)
	f.talk(bob, alice, 0xb3, 5)
}

func TestCloseEndsSessions(t *testing.T) {
	f := setup(t, 10)
	alice, _ := f.join("hall", "en", "Alice")
	f.connect(alice)

	f.hub.Close()
	code, err := alice.WaitClosed(f.ctx)
	if err != nil || code != websocket.StatusGoingAway {
		t.Errorf("close code = %v, %v", code, err)
	}
	if f.hub.Rooms() != 0 {
		t.Errorf("%d rooms left after Close", f.hub.Rooms())
	}
	if _, _, err := testclient.Join(f.ctx, f.url, "hall", "en", "Late"); err == nil {
		t.Error("join after Close succeeded")
	}
}
