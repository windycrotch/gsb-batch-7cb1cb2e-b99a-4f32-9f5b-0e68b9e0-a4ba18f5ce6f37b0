package hub_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/whispem/ambivox/server/internal/hub"
	"github.com/whispem/ambivox/server/internal/invite"
	"github.com/whispem/ambivox/server/internal/testclient"
)

func freeUDPPort(t *testing.T) uint16 {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	return uint16(conn.LocalAddr().(*net.UDPAddr).Port)
}

func TestInvitationsOverOneUDPPort(t *testing.T) {
	port := freeUDPPort(t)
	f := setup(t, 10, func(cfg *hub.Config) {
		cfg.Invites = invite.New([]byte("test key"), time.Hour)
		cfg.AccessKey = "open sesame"
		cfg.UDPPort = port
	})
	if !f.hub.InviteOnly() || !f.hub.AccessKeyRequired() {
		t.Fatal("invitations or access key not reported")
	}

	create := func(key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/rooms", nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		rec := httptest.NewRecorder()
		f.hub.CreateRoom(rec, req)
		return rec
	}
	for _, key := range []string{"", "open sesam"} {
		if rec := create(key); rec.Code != http.StatusUnauthorized {
			t.Errorf("access key %q: status %d", key, rec.Code)
		}
	}
	rec := create("open sesame")
	var grant struct{ Room, Token, Expires string }
	if err := json.NewDecoder(rec.Body).Decode(&grant); err != nil || rec.Code != http.StatusCreated {
		t.Fatalf("create room: %d %v", rec.Code, err)
	}
	if expires, err := time.Parse(time.RFC3339, grant.Expires); err != nil || time.Until(expires) < 59*time.Minute {
		t.Errorf("expires = %q", grant.Expires)
	}

	for _, token := range []string{"", grant.Token + "x", strings.Replace(grant.Token, ".", "0.", 1)} {
		client, err := testclient.Dial(f.ctx, f.url)
		if err != nil {
			t.Fatal(err)
		}
		_ = client.Send(testclient.Event{"type": "join", "room": grant.Room, "lang": "en", "token": token})
		if event := f.wait(client, "error"); !strings.Contains(event.String("message"), "invitation") {
			t.Errorf("token %q: %v", token, event)
		}
		if code, err := client.WaitClosed(f.ctx); err != nil || code != websocket.StatusPolicyViolation {
			t.Errorf("close code = %v, %v", code, err)
		}
	}
	if f.hub.Rooms() != 0 {
		t.Errorf("a refused join opened a room")
	}

	join := func(name string) *testclient.Client {
		client, _, err := testclient.JoinWith(f.ctx, f.url, testclient.Event{"room": grant.Room, "lang": "it", "name": name, "token": grant.Token})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(client.Close)
		return client
	}
	alice, bob := join("Alice"), join("Bob")
	f.connect(alice, bob)
	f.talk(alice, bob, 0xa1, 5)

	candidate := fmt.Sprintf(" %d typ host", port)
	for _, answer := range append(alice.Events("answer"), bob.Events("answer")...) {
		if !strings.Contains(answer.String("sdp"), candidate) {
			t.Errorf("answer does not offer UDP port %d:\n%s", port, answer.String("sdp"))
		}
	}
}

func TestOpenRoomsNeedNoToken(t *testing.T) {
	f := setup(t, 10)
	rec := httptest.NewRecorder()
	f.hub.CreateRoom(rec, httptest.NewRequest(http.MethodPost, "/rooms", nil))
	var grant map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&grant); err != nil || rec.Code != http.StatusCreated {
		t.Fatalf("create room: %d %v", rec.Code, err)
	}
	if len(grant["room"]) != 32 || grant["token"] != "" {
		t.Errorf("grant = %v", grant)
	}
	f.join(grant["room"], "en", "Alice")
}

func TestEchoGuardSilencesTheSpeakerWhileTheirTranslationPlays(t *testing.T) {
	f := setup(t, 10, func(cfg *hub.Config) { cfg.EchoGuard = true })
	alice, _ := f.join("echo", "en", "Alice")
	bob, _ := f.join("echo", "fr", "Bob")
	f.connect(alice, bob)

	marked := func() (heard, silent int) {
		for _, packet := range bob.Packets() {
			switch {
			case len(packet) == 4 && packet[1] == 0xa1:
				heard++
			case bytes.Equal(packet, []byte{0xf8, 0xff, 0xfe}):
				silent++
			}
		}
		return heard, silent
	}
	for i := range 20 {
		if err := alice.WriteAudio([]byte{0x78, 0xa1, byte(i), 0xff}, frame); err != nil {
			t.Fatal(err)
		}
		time.Sleep(frame)
	}
	f.eventually("silenced packets", func() bool { _, silent := marked(); return silent >= 10 })
	if heard, _ := marked(); heard > 5 {
		t.Errorf("Bob heard %d of Alice's packets before the guard closed", heard)
	}

	time.Sleep(time.Second)
	f.talk(alice, bob, 0xa2, 1)
}

func TestSameRoomGivesTheFloorToTheLouderMicrophone(t *testing.T) {
	f := setup(t, 10)
	alice, welcome := f.join("kitchen", "it", "Alice")
	if welcome["sameRoom"] != false {
		t.Errorf("welcome = %v", welcome)
	}
	bob, _ := f.join("kitchen", "it", "Bob")
	f.connect(alice, bob)

	_ = alice.Send(testclient.Event{"type": "same-room", "sameRoom": true})
	for _, client := range []*testclient.Client{alice, bob} {
		if event := f.wait(client, "same-room"); event["sameRoom"] != true {
			t.Fatalf("same-room = %v", event)
		}
	}

	count := func(c *testclient.Client, marker byte) (n int) {
		for _, packet := range c.Packets() {
			if len(packet) == 4 && packet[1] == marker {
				n++
			}
		}
		return n
	}
	both := func(near, far *testclient.Client, nearMarker, farMarker byte) {
		for i := range 40 {
			if err := near.WriteAudio([]byte{0x78, nearMarker, byte(i), 0xff}, frame); err != nil {
				t.Fatal(err)
			}
			if err := far.WriteAudio([]byte{0x78, farMarker, byte(i), 0xff}, frame); err != nil {
				t.Fatal(err)
			}
			time.Sleep(frame)
		}
		time.Sleep(200 * time.Millisecond)
	}

	alice.SetLevel(20)
	bob.SetLevel(36)
	both(alice, bob, 0xa1, 0xb1)
	if heard := count(bob, 0xa1); heard < 30 {
		t.Errorf("Bob heard %d of Alice's 40 packets", heard)
	}
	if leaked := count(alice, 0xb1); leaked > 5 {
		t.Errorf("Alice heard %d packets of her own voice through Bob's microphone", leaked)
	}

	time.Sleep(time.Second)
	bob.SetLevel(22)
	alice.SetLevel(38)
	both(bob, alice, 0xb2, 0xa2)
	if heard := count(alice, 0xb2); heard < 30 {
		t.Errorf("Alice heard %d of Bob's 40 packets once he spoke", heard)
	}
	if leaked := count(bob, 0xa2); leaked > 5 {
		t.Errorf("Bob heard %d packets of his own voice through Alice's microphone", leaked)
	}

	_ = bob.Send(testclient.Event{"type": "same-room", "sameRoom": false})
	f.wait(alice, "same-room")
	both(alice, bob, 0xa3, 0xb3)
	if count(bob, 0xa3) < 30 || count(alice, 0xb3) < 30 {
		t.Errorf("with the mode off, each side should hear the other: %d, %d", count(bob, 0xa3), count(alice, 0xb3))
	}
}

func TestHeadphonesReopenStreamsEarlyAndLiftTheEchoGuard(t *testing.T) {
	f := setup(t, 10, func(cfg *hub.Config) { cfg.EchoGuard = true })
	alice, welcome := f.join("desk", "en", "Alice")
	if welcome["headphones"] != false {
		t.Errorf("welcome = %v", welcome)
	}
	bob, _ := f.join("desk", "fr", "Bob")
	f.connect(alice, bob)
	f.eventually("two streams", func() bool { return f.engine.Opened() == 2 })

	_ = bob.Send(testclient.Event{"type": "headphones", "headphones": true})
	for _, client := range []*testclient.Client{alice, bob} {
		if event := f.wait(client, "headphones"); event["headphones"] != true {
			t.Fatalf("headphones = %v", event)
		}
	}
	f.eventually("early streams", func() bool { return f.engine.OpenedEarly() == 2 })

	for i := range 20 {
		if err := alice.WriteAudio([]byte{0x78, 0xa1, byte(i), 0xff}, frame); err != nil {
			t.Fatal(err)
		}
		time.Sleep(frame)
	}
	heard := func() (n int) {
		for _, packet := range bob.Packets() {
			if len(packet) == 4 && packet[1] == 0xa1 {
				n++
			}
		}
		return n
	}
	f.eventually("Alice's packets", func() bool { return heard() >= 15 })

	_ = alice.Send(testclient.Event{"type": "headphones", "headphones": false})
	f.wait(bob, "headphones")
	f.eventually("plain streams", func() bool { return f.engine.Opened() == 6 })
	if f.engine.OpenedEarly() != 2 {
		t.Errorf("%d early streams after headphones were turned off", f.engine.OpenedEarly())
	}
}
