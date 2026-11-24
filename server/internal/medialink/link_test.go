package medialink_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/whispem/ambivox/server/internal/medialink"
	"github.com/whispem/ambivox/server/internal/medialink/medialinktest"
	"github.com/whispem/ambivox/server/internal/protocol"
)

type recorder struct {
	mu       sync.Mutex
	speech   [][]byte
	captions []string
	errors   []string
	closed   chan struct{}
}

func newRecorder() *recorder { return &recorder{closed: make(chan struct{})} }

func (r *recorder) OnSpeech(packet []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.speech = append(r.speech, packet)
}

func (r *recorder) OnCaption(kind protocol.CaptionKind, _ uint32, text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	prefix := kind.String()
	if kind.Partial() {
		prefix += " (partial)"
	}
	r.captions = append(r.captions, prefix+": "+text)
}

func (r *recorder) OnError(message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errors = append(r.errors, message)
}

func (r *recorder) OnClosed() { close(r.closed) }

func (r *recorder) waitClosed(t *testing.T) {
	t.Helper()
	select {
	case <-r.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("stream was never closed")
	}
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func dial(t *testing.T, path string) *medialink.Link {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	link := medialink.Dial(ctx, path, quiet())
	t.Cleanup(func() {
		cancel()
		<-link.Done()
	})
	return link
}

func TestStreamLifecycle(t *testing.T) {
	engine := medialinktest.Start(t)
	link := dial(t, engine.Path)
	eventually(t, "connection", link.Connected)
	if link.Backend() != "fake" {
		t.Errorf("backend = %q", link.Backend())
	}

	rec := newRecorder()
	stream, err := link.Open("en", "fr", false, rec)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		stream.Write(uint16(i), []byte{byte(i), 0xaa})
	}
	stream.Close()
	stream.Close()
	rec.waitClosed(t)

	if len(rec.speech) != 3 || rec.speech[2][0] != 2 {
		t.Errorf("speech = %x", rec.speech)
	}
	want := []string{"source (partial): heard", "source: heard en", "translation: said fr"}
	if strings.Join(rec.captions, "|") != strings.Join(want, "|") {
		t.Errorf("captions = %q", rec.captions)
	}
	eventually(t, "stats", func() bool { return link.Stats()["streams"] == 1 })
}

func TestRefusedStreamReportsErrorThenCloses(t *testing.T) {
	engine := medialinktest.Start(t)
	link := dial(t, engine.Path)
	eventually(t, "connection", link.Connected)

	rec := newRecorder()
	if _, err := link.Open("en", "zz", false, rec); err != nil {
		t.Fatal(err)
	}
	rec.waitClosed(t)
	if len(rec.errors) != 1 {
		t.Errorf("errors = %q", rec.errors)
	}
}

func TestStreamsAreIndependent(t *testing.T) {
	engine := medialinktest.Start(t)
	link := dial(t, engine.Path)
	eventually(t, "connection", link.Connected)

	a, b := newRecorder(), newRecorder()
	sa, _ := link.Open("en", "fr", false, a)
	sb, _ := link.Open("fr", "en", false, b)
	sa.Write(0, []byte("a"))
	sb.Write(0, []byte("b"))
	sa.Close()
	sb.Close()
	a.waitClosed(t)
	b.waitClosed(t)
	if len(a.speech) != 1 || string(a.speech[0]) != "a" || len(b.speech) != 1 || string(b.speech[0]) != "b" {
		t.Errorf("a = %q, b = %q", a.speech, b.speech)
	}
}

func TestReconnectsAndOrphansStreams(t *testing.T) {
	engine := medialinktest.Start(t)
	link := dial(t, engine.Path)

	var mu sync.Mutex
	var changes []bool
	link.OnChange(func(connected bool) {
		mu.Lock()
		defer mu.Unlock()
		changes = append(changes, connected)
	})
	eventually(t, "connection", link.Connected)

	rec := newRecorder()
	stream, err := link.Open("en", "fr", false, rec)
	if err != nil {
		t.Fatal(err)
	}
	engine.Stop()
	rec.waitClosed(t)
	eventually(t, "disconnection", func() bool { return !link.Connected() })

	stream.Write(1, []byte("ignored"))
	stream.Close()
	if _, err := link.Open("en", "fr", false, newRecorder()); err != medialink.ErrDisconnected {
		t.Errorf("Open while down: %v", err)
	}

	if err := engine.Listen(); err != nil {
		t.Fatal(err)
	}
	eventually(t, "reconnection", link.Connected)

	again := newRecorder()
	fresh, err := link.Open("en", "fr", false, again)
	if err != nil {
		t.Fatal(err)
	}
	fresh.Write(0, []byte("back"))
	fresh.Close()
	again.waitClosed(t)
	if len(again.speech) != 1 {
		t.Errorf("speech after reconnect = %q", again.speech)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(changes) < 3 || changes[0] != true || changes[1] != false || changes[2] != true {
		t.Errorf("changes = %v", changes)
	}
}

func TestDialBeforeEngineExists(t *testing.T) {
	engine := medialinktest.Start(t)
	engine.Stop()
	link := dial(t, engine.Path)
	time.Sleep(150 * time.Millisecond)
	if link.Connected() {
		t.Fatal("connected to nothing")
	}
	if err := engine.Listen(); err != nil {
		t.Fatal(err)
	}
	eventually(t, "connection", link.Connected)
}
