package hub_test

import (
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/whispem/ambivox/server/internal/testclient"
)

// metric reads one sample from the fixture's Prometheus exposition.
func (f *fixture) metric(name string) float64 {
	f.t.Helper()
	var exposition strings.Builder
	if _, err := f.registry.WriteTo(&exposition); err != nil {
		f.t.Fatal(err)
	}
	for line := range strings.SplitSeq(exposition.String(), "\n") {
		if rest, ok := strings.CutPrefix(line, name+" "); ok {
			value, err := strconv.ParseFloat(rest, 64)
			if err != nil {
				f.t.Fatalf("%s: %v", line, err)
			}
			return value
		}
	}
	f.t.Fatalf("no metric %q", name)
	return 0
}

func (f *fixture) pairs() [][2]string { return f.engine.Pairs() }

func (f *fixture) wantPairs(want ...[2]string) {
	f.t.Helper()
	sort.Slice(want, func(i, j int) bool {
		if want[i][0] != want[j][0] {
			return want[i][0] < want[j][0]
		}
		return want[i][1] < want[j][1]
	})
	deadline := time.Now().Add(10 * time.Second)
	var got [][2]string
	for time.Now().Before(deadline) {
		got = f.pairs()
		if slices.Equal(got, want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.t.Fatalf("engine streams = %v, want %v", got, want)
}

func (f *fixture) changeLanguage(c *testclient.Client, lang string) {
	f.t.Helper()
	if err := c.Send(testclient.Event{"type": "lang", "lang": lang}); err != nil {
		f.t.Fatal(err)
	}
}

func heard(c *testclient.Client, marker byte) (n int) {
	for _, packet := range c.Packets() {
		if len(packet) == 4 && packet[1] == marker {
			n++
		}
	}
	return n
}

func captions(c *testclient.Client, text string) (n int) {
	for _, event := range c.Events("caption") {
		if event.String("text") == text {
			n++
		}
	}
	return n
}

// A language change mid-call must swap both directions: the stream for the old
// pair is closed, a new one is opened, and whatever the old one still says is
// neither delivered nor counted.
func TestLanguageChangeReplacesStreamsAndDropsTheOldOne(t *testing.T) {
	f := setup(t, 10)
	alice, _ := f.join("swap", "en", "Alice")
	bob, _ := f.join("swap", "fr", "Bob")
	f.connect(alice, bob)
	f.wantPairs([2]string{"en", "fr"}, [2]string{"fr", "en"})
	f.talk(alice, bob, 0xa1, 5)

	f.engine.HoldClose(true)
	speechBefore, captionsBefore := f.metric("ambivox_speech_packets_sent_total"), f.metric("ambivox_captions_total")

	f.changeLanguage(alice, "es")
	for _, client := range []*testclient.Client{alice, bob} {
		if event := f.wait(client, "lang"); event.String("id") != alice.ID || event.String("lang") != "es" {
			t.Fatalf("lang event = %v", event)
		}
	}
	f.wantPairs([2]string{"es", "fr"}, [2]string{"fr", "es"})
	f.eventually("both old streams closed", func() bool { return f.engine.Closed() >= 2 })

	// The old en -> fr stream speaks and captions after it was replaced.
	if !f.engine.ReplaySpeech([]byte{0x78, 0x99, 0x01, 0xff}, "en", "fr") {
		t.Fatal("nothing to replay on the old stream")
	}
	if !f.engine.ReplayCaption("stale words", "mots périmés", "en", "fr") {
		t.Fatal("nothing to caption on the old stream")
	}
	time.Sleep(200 * time.Millisecond)
	if n := heard(bob, 0x99); n != 0 {
		t.Errorf("Bob heard %d packets from the retired stream", n)
	}
	if n := captions(bob, "stale words"); n != 0 {
		t.Errorf("Bob got %d captions from the retired stream", n)
	}
	if n := captions(alice, "stale words"); n != 0 {
		t.Errorf("Alice got %d captions from the retired stream", n)
	}
	if got := f.metric("ambivox_speech_packets_sent_total"); got != speechBefore {
		t.Errorf("speech counter moved by %v for a retired stream", got-speechBefore)
	}
	if got := f.metric("ambivox_captions_total"); got != captionsBefore {
		t.Errorf("caption counter moved by %v for a retired stream", got-captionsBefore)
	}

	// The new pairing works, and its captions name the new language.
	f.talk(alice, bob, 0xa2, 5)
	f.eventually("caption for the new pair", func() bool { return captions(bob, "heard es") > 0 })
}

// Changing to the other participant's language must close the translating
// streams and switch the call to direct relay, and changing back must rebuild
// them.
func TestLanguageChangeToALanguageInCommonRelaysDirectly(t *testing.T) {
	f := setup(t, 10)
	alice, _ := f.join("common", "en", "Alice")
	bob, _ := f.join("common", "fr", "Bob")
	f.connect(alice, bob)
	f.wantPairs([2]string{"en", "fr"}, [2]string{"fr", "en"})
	f.talk(alice, bob, 0xa1, 5)

	f.changeLanguage(alice, "fr")
	f.wait(bob, "lang")
	f.wantPairs()

	before := heard(bob, 0xa2)
	for i := range 10 {
		if err := alice.WriteAudio([]byte{0x78, 0xa2, byte(i), 0xff}, frame); err != nil {
			t.Fatal(err)
		}
		time.Sleep(frame)
	}
	f.eventually("direct relay", func() bool { return heard(bob, 0xa2) > before })
	if direct := f.metric("ambivox_rtp_packets_relayed_total"); direct < 10 {
		t.Errorf("relayed packets = %v", direct)
	}

	f.changeLanguage(bob, "es")
	f.wait(alice, "lang")
	f.wantPairs([2]string{"es", "fr"}, [2]string{"fr", "es"})
	f.talk(bob, alice, 0xb3, 5)
}

// Headphones mode reopens every stream with the early flag; the replaced
// streams must go silent at once.
func TestHeadphonesSwapRetiresTheOldStreams(t *testing.T) {
	f := setup(t, 10)
	alice, _ := f.join("early", "en", "Alice")
	bob, _ := f.join("early", "fr", "Bob")
	f.connect(alice, bob)
	f.wantPairs([2]string{"en", "fr"}, [2]string{"fr", "en"})

	f.engine.HoldClose(true)
	_ = alice.Send(testclient.Event{"type": "headphones", "headphones": true})
	f.wait(bob, "headphones")
	f.eventually("early streams", func() bool { return f.engine.OpenedEarly() == 2 })
	f.eventually("old streams closed", func() bool { return f.engine.Closed() >= 2 })

	if !f.engine.ReplaySpeech([]byte{0x78, 0x98, 0x01, 0xff}, "en", "fr") {
		t.Fatal("nothing to replay on the old stream")
	}
	time.Sleep(200 * time.Millisecond)
	if n := heard(bob, 0x98); n != 0 {
		t.Errorf("Bob heard %d packets from the pre-headphones stream", n)
	}

	_ = bob.Send(testclient.Event{"type": "headphones", "headphones": false})
	f.wait(alice, "headphones")
	f.eventually("plain streams reopened", func() bool { return f.engine.Closed() >= 4 })
	f.talk(alice, bob, 0xa4, 5)
}

// Joining and leaving replace the affected direction: a lone participant has
// no stream, a second one opens a pair, and their departure closes it.
func TestMembershipChangesRewireStreams(t *testing.T) {
	f := setup(t, 10)
	alice, _ := f.join("door", "en", "Alice")
	f.wantPairs()

	bob, _ := f.join("door", "fr", "Bob")
	f.wait(alice, "peer-joined")
	f.wantPairs([2]string{"en", "fr"}, [2]string{"fr", "en"})

	bob.Close()
	f.wait(alice, "peer-left")
	f.wantPairs()
	f.eventually("no peers left", func() bool { return f.hub.Peers() == 1 })

	carol, _ := f.join("door", "es", "Carol")
	f.wait(alice, "peer-joined")
	f.wantPairs([2]string{"en", "es"}, [2]string{"es", "en"})

	alice.Close()
	f.eventually("Alice to leave", func() bool { return f.hub.Peers() == 1 })
	f.wantPairs()
	carol.Close()
	f.eventually("room to empty", func() bool { return f.hub.Rooms() == 0 && f.hub.Peers() == 0 })
}

// If the engine refuses the new pair, the direction stays bare rather than
// keeping the old stream, and the next relevant change tries again.
func TestRefusedStreamLeavesTheDirectionBareUntilTheNextChange(t *testing.T) {
	f := setup(t, 10)
	alice, _ := f.join("refuse", "en", "Alice")
	f.wantPairs()

	f.engine.Refuse("fr", true)
	bob, _ := f.join("refuse", "fr", "Bob")
	f.wait(alice, "peer-joined")
	f.connect(alice, bob)
	// Bob still speaks fr -> en; Alice's en -> fr direction has no stream.
	f.wantPairs([2]string{"fr", "en"})

	// No translation stream exists, so Alice's speech does not reach Bob.
	before := heard(bob, 0xa5)
	for i := range 10 {
		if err := alice.WriteAudio([]byte{0x78, 0xa5, byte(i), 0xff}, frame); err != nil {
			t.Fatal(err)
		}
		time.Sleep(frame)
	}
	time.Sleep(200 * time.Millisecond)
	if n := heard(bob, 0xa5); n != before {
		t.Errorf("Bob heard %d packets without a stream", n-before)
	}

	f.engine.Refuse("fr", false)
	_ = bob.Send(testclient.Event{"type": "headphones", "headphones": true})
	f.wait(alice, "headphones")
	f.wantPairs([2]string{"en", "fr"}, [2]string{"fr", "en"})
	f.talk(alice, bob, 0xa6, 5)
}

// Several changes racing on the same room must settle on the last pairing,
// with exactly one live stream per direction.
func TestConcurrentChangesSettleOnOnePairing(t *testing.T) {
	f := setup(t, 10)
	alice, _ := f.join("race", "en", "Alice")
	bob, _ := f.join("race", "fr", "Bob")
	f.connect(alice, bob)
	f.wantPairs([2]string{"en", "fr"}, [2]string{"fr", "en"})

	var wg sync.WaitGroup
	for _, lang := range []string{"es", "it", "en", "es"} {
		wg.Add(1)
		go func(lang string) {
			defer wg.Done()
			f.changeLanguage(alice, lang)
		}(lang)
	}
	for _, on := range []bool{true, false, true} {
		wg.Add(1)
		go func(on bool) {
			defer wg.Done()
			_ = bob.Send(testclient.Event{"type": "headphones", "headphones": on})
		}(on)
	}
	wg.Wait()
	// Whichever change the room applied last is the one in effect: the streams
	// must match the last language Alice announced, exactly once per direction.
	var final [2]string
	f.eventually("a language announcement", func() bool {
		events := alice.Events("lang")
		if len(events) == 0 {
			return false
		}
		final = [2]string{events[len(events)-1].String("lang"), "fr"}
		return true
	})
	f.wantPairs(final, [2]string{final[1], final[0]})
	time.Sleep(300 * time.Millisecond)
	if pairs := f.pairs(); len(pairs) != 2 {
		t.Fatalf("more than one stream per direction: %v", pairs)
	}

	f.talk(alice, bob, 0xa7, 5)
	f.talk(bob, alice, 0xb8, 5)
}

// After the engine restarts, every room rebuilds its streams, and a replay
// aimed at the stream from before the restart goes nowhere.
func TestEngineRestartRebuildsAndDiscardsTheOldStreams(t *testing.T) {
	f := setup(t, 10)
	alice, _ := f.join("restart", "en", "Alice")
	bob, _ := f.join("restart", "fr", "Bob")
	f.connect(alice, bob)
	f.talk(alice, bob, 0xa1, 5)
	opened := f.engine.Opened()

	f.engine.HoldClose(true)
	f.engine.Stop()
	for _, client := range []*testclient.Client{alice, bob} {
		if status := f.wait(client, "media"); status["connected"] != false {
			t.Errorf("status after stop = %v", status)
		}
	}
	// The engine is gone; a replay from its last stream cannot be sent at all.
	if f.engine.ReplaySpeech([]byte{0x78, 0x97, 0x01, 0xff}) {
		t.Error("a stream outlived its engine")
	}

	if err := f.engine.Listen(); err != nil {
		t.Fatal(err)
	}
	for _, client := range []*testclient.Client{alice, bob} {
		if status := f.wait(client, "media"); status["connected"] != true {
			t.Errorf("status after restart = %v", status)
		}
	}
	f.eventually("streams rebuilt", func() bool { return f.engine.Opened() >= opened+2 })
	f.wantPairs([2]string{"en", "fr"}, [2]string{"fr", "en"})

	f.talk(alice, bob, 0xa2, 5)
	f.talk(bob, alice, 0xb3, 5)
}

// Shutting down asks every stream to finish and stops the rooms from posting
// anything to sessions that are already closed.
func TestShutdownClosesStreamsAndStopsDelivery(t *testing.T) {
	f := setup(t, 10)
	alice, _ := f.join("final", "en", "Alice")
	bob, _ := f.join("final", "fr", "Bob")
	f.connect(alice, bob)
	f.talk(alice, bob, 0xa1, 5)
	f.wantPairs([2]string{"en", "fr"}, [2]string{"fr", "en"})

	f.engine.HoldClose(true)
	f.hub.Close()
	f.eventually("every stream closed", func() bool { return f.engine.Closed() >= 2 })
	for _, client := range []*testclient.Client{alice, bob} {
		if code, err := client.WaitClosed(f.ctx); err != nil || code != 1001 {
			t.Errorf("close code = %v, %v", code, err)
		}
	}

	captionsBefore := f.metric("ambivox_captions_total")
	if !f.engine.ReplayCaption("late", "tard", "en", "fr") {
		t.Fatal("no closed stream to replay")
	}
	time.Sleep(200 * time.Millisecond)
	if got := f.metric("ambivox_captions_total"); got != captionsBefore {
		t.Errorf("a closed session still received captions: %v -> %v", captionsBefore, got)
	}
}

// An unsupported language is refused without touching the call.
func TestUnsupportedLanguageKeepsTheCall(t *testing.T) {
	f := setup(t, 10)
	alice, _ := f.join("strict", "en", "Alice")
	bob, _ := f.join("strict", "fr", "Bob")
	f.connect(alice, bob)
	f.wantPairs([2]string{"en", "fr"}, [2]string{"fr", "en"})

	f.changeLanguage(alice, "de")
	if event := f.wait(alice, "error"); !strings.Contains(event.String("message"), "unsupported language") {
		t.Errorf("error = %v", event)
	}
	f.wantPairs([2]string{"en", "fr"}, [2]string{"fr", "en"})
	f.talk(alice, bob, 0xa1, 5)
}
