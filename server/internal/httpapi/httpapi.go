// Package httpapi assembles the public HTTP surface of the server.
package httpapi

import (
	"encoding/json"
	"io/fs"
	"net/http"

	"github.com/whispem/ambivox/server/internal/hub"
	"github.com/whispem/ambivox/server/internal/medialink"
	"github.com/whispem/ambivox/server/internal/metrics"
)

const contentSecurityPolicy = "default-src 'self'; connect-src 'self' ws: wss:; media-src 'self' blob: mediastream:; img-src 'self' data:; " +
	"base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

func New(h *hub.Hub, link *medialink.Link, registry *metrics.Registry, static fs.FS) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws", h.ServeWS)
	mux.HandleFunc("POST /rooms", h.CreateRoom)
	mux.Handle("GET /metrics", registry)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !link.Connected() {
			http.Error(w, "media engine unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ready\n"))
	})
	mux.HandleFunc("GET /config", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"languages":      hub.Languages,
			"backend":        link.Backend(),
			"mediaConnected": link.Connected(),
			"inviteOnly":     h.InviteOnly(),
			"accessKey":      h.AccessKeyRequired(),
		})
	})
	mux.Handle("GET /", http.FileServerFS(static))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("Referrer-Policy", "no-referrer")
		header.Set("Content-Security-Policy", contentSecurityPolicy)
		header.Set("Permissions-Policy", "microphone=(self)")
		mux.ServeHTTP(w, r)
	})
}
