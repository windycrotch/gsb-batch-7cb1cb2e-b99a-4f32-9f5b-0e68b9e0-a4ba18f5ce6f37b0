package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/whispem/ambivox/server/internal/httpapi"
	"github.com/whispem/ambivox/server/internal/hub"
	"github.com/whispem/ambivox/server/internal/medialink"
	"github.com/whispem/ambivox/server/internal/medialink/medialinktest"
	"github.com/whispem/ambivox/server/internal/metrics"
	"github.com/whispem/ambivox/server/internal/testclient"
	"github.com/whispem/ambivox/server/web"
)

func get(t *testing.T, url string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

func TestEndpoints(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := medialinktest.Start(t)
	engine.Stop()

	link := medialink.Dial(ctx, engine.Path, log)
	registry := metrics.NewRegistry()
	rooms, err := hub.New(hub.Config{Link: link, Logger: log, Metrics: registry, IncludeLoopback: true, DisableMDNS: true})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(httpapi.New(rooms, link, registry, web.Static()))
	t.Cleanup(func() {
		rooms.Close()
		server.Close()
		cancel()
		<-link.Done()
	})

	if resp, body := get(t, server.URL+"/healthz"); resp.StatusCode != 200 || body != "ok\n" {
		t.Errorf("/healthz = %d %q", resp.StatusCode, body)
	}
	if resp, _ := get(t, server.URL+"/readyz"); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("/readyz without engine = %d", resp.StatusCode)
	}

	if err := engine.Listen(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !link.Connected() {
		if time.Now().After(deadline) {
			t.Fatal("media link never came up")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if resp, body := get(t, server.URL+"/readyz"); resp.StatusCode != 200 || body != "ready\n" {
		t.Errorf("/readyz = %d %q", resp.StatusCode, body)
	}

	resp, body := get(t, server.URL+"/config")
	var config struct {
		Languages      []string `json:"languages"`
		Backend        string   `json:"backend"`
		MediaConnected bool     `json:"mediaConnected"`
		InviteOnly     bool     `json:"inviteOnly"`
		AccessKey      bool     `json:"accessKey"`
	}
	if err := json.Unmarshal([]byte(body), &config); err != nil {
		t.Fatalf("/config: %v in %q", err, body)
	}
	if len(config.Languages) != 4 || config.Backend != "fake" || !config.MediaConnected || config.InviteOnly || config.AccessKey {
		t.Errorf("/config = %+v", config)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("/config is cacheable")
	}

	resp, body = get(t, server.URL+"/")
	if resp.StatusCode != 200 || !strings.Contains(body, "<title>ambivox</title>") {
		t.Errorf("/ = %d", resp.StatusCode)
	}
	for header, want := range map[string]string{
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "no-referrer",
		"Content-Security-Policy": "default-src 'self'",
	} {
		if got := resp.Header.Get(header); !strings.HasPrefix(got, want) {
			t.Errorf("%s = %q", header, got)
		}
	}
	for _, asset := range []string{"/app.js", "/style.css"} {
		if resp, _ := get(t, server.URL+asset); resp.StatusCode != 200 {
			t.Errorf("%s = %d", asset, resp.StatusCode)
		}
	}
	if resp, _ := get(t, server.URL+"/nope"); resp.StatusCode != 404 {
		t.Errorf("/nope = %d", resp.StatusCode)
	}
	if resp, err := http.Post(server.URL+"/healthz", "text/plain", nil); err != nil || resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /healthz = %v, %v", resp.StatusCode, err)
	}
	if resp, err := http.Post(server.URL+"/rooms", "", nil); err != nil || resp.StatusCode != http.StatusCreated {
		t.Errorf("POST /rooms = %v, %v", resp.StatusCode, err)
	}

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
	client, welcome, err := testclient.Join(ctx, wsURL, "lobby", "es", "Ana")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if welcome.String("lang") != "es" {
		t.Errorf("welcome = %v", welcome)
	}

	_, body = get(t, server.URL+"/metrics")
	for _, line := range []string{"ambivox_sessions_total 1\n", "ambivox_rooms 1\n", "ambivox_media_connected 1\n"} {
		if !strings.Contains(body, line) {
			t.Errorf("/metrics lacks %q", line)
		}
	}
}

func TestCrossOriginWebSocketsAreRefused(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := medialinktest.Start(t)
	link := medialink.Dial(ctx, engine.Path, log)
	registry := metrics.NewRegistry()
	rooms, err := hub.New(hub.Config{Link: link, Logger: log, Metrics: registry})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(httpapi.New(rooms, link, registry, web.Static()))
	t.Cleanup(func() {
		rooms.Close()
		server.Close()
		cancel()
		<-link.Done()
	})

	req, _ := http.NewRequest("GET", server.URL+"/ws", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	req.Header.Set("Origin", "https://evil.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin upgrade = %d", resp.StatusCode)
	}
}
