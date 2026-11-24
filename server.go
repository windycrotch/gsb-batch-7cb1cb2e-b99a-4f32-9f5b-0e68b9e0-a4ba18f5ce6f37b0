package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"math/rand"
	"net/http"
	"strings"
)

//go:embed templates static
var embedFS embed.FS

type Server struct {
	mediaLib      *MediaLibrary
	tmpl          *template.Template
	staticVersion string
}

func httpError(r *http.Request, w http.ResponseWriter, err error, code int) {
	http.Error(w, err.Error(), code)
	slog.Error("failed request",
		slog.Any("err", err),
		slog.String("url", r.URL.String()),
		slog.Int("code", code),
	)
}

// HTTPStatusForError maps storage/media errors to HTTP status codes.
// Unknown errors are treated as retryable server-side failures. A specific
// upstream 5xx code (e.g. 503) is preserved when available.
func HTTPStatusForError(err error) int {
	switch {
	case err == nil:
		return http.StatusOK
	case errors.Is(err, ErrInvalidPath):
		return http.StatusBadRequest
	case IsNotFoundError(err):
		return http.StatusNotFound
	}
	var statusCoder interface{ HTTPStatusCode() int }
	if errors.As(err, &statusCoder) {
		if code := statusCoder.HTTPStatusCode(); code >= 500 && code < 600 {
			return code
		}
	}
	return http.StatusInternalServerError
}

// writeHandlerError translates an error into an HTTP response. When the
// client context is canceled there is nowhere to write and continuing would
// be pointless, so the response is left untouched.
func writeHandlerError(r *http.Request, w http.ResponseWriter, err error) {
	if err == nil {
		return
	}
	if r.Context().Err() != nil || errors.Is(err, context.Canceled) {
		slog.Info("request canceled by client",
			slog.String("url", r.URL.String()),
			slog.Any("err", err),
		)
		return
	}
	code := HTTPStatusForError(err)
	if code == http.StatusNotFound {
		// Start the body with the canonical, recognizable status text.
		err = fmt.Errorf("%s: %w", http.StatusText(http.StatusNotFound), err)
	}
	if code >= 500 {
		// Transient storage failures must not end up cached anywhere; a
		// retry should hit the origin again once the storage recovers.
		w.Header().Set("Cache-Control", "no-store")
	}
	httpError(r, w, err, code)
}

// ValidatePath provides a basic protection from the path traversal vulnerability.
func ValidatePath(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "./") || strings.Contains(r.URL.Path, ".\\") {
			httpError(r, w, errors.New("invalid path"), http.StatusBadRequest)
			return
		}
		h(w, r)
	}
}

// NormalizePath normalizes the request URL by removing the delimeter suffix.
func NormalizePath(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = strings.TrimRight(r.URL.Path, Delimiter)
		h(w, r)
	}
}

// DisableFileListing disables file listing under directories. It can be used with the built-in http.FileServer.
func DisableFileListing(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		h.ServeHTTP(w, r)
	})
}

type TemplateData struct {
	StaticVersion string
	*MediaListing
}

// renderListing executes the listing template. When the Server was created
// as a zero-value literal (common in focused unit tests) the embedded
// templates are parsed on demand instead of panicking on a nil template.
func (s *Server) renderListing(w http.ResponseWriter, r *http.Request, data TemplateData) error {
	tmpl := s.tmpl
	if tmpl == nil {
		parsed, err := template.New("").Funcs(templateFunctions).ParseFS(embedFS, "templates/*.gohtml")
		if err != nil {
			return err
		}
		tmpl = parsed
	}
	return tmpl.ExecuteTemplate(w, "listing.gohtml", data)
}

func (s *Server) ListingHandler(w http.ResponseWriter, r *http.Request) {
	listing, err := s.mediaLib.List(r.Context(), r.URL.Path)
	if err != nil {
		writeHandlerError(r, w, err)
		return
	}
	tmplData := TemplateData{
		StaticVersion: s.staticVersion,
		MediaListing:  listing,
	}
	if err := s.renderListing(w, r, tmplData); err != nil {
		if r.Context().Err() != nil {
			return
		}
		httpError(r, w, err, http.StatusInternalServerError)
		return
	}
}

func (s *Server) StreamHandler(w http.ResponseWriter, r *http.Request) {
	contentURL, err := s.mediaLib.ContentURL(r.Context(), r.URL.Path)
	if err != nil {
		writeHandlerError(r, w, err)
		return
	}
	// Cancellation may have arrived while the URL was being signed; do not
	// write the redirect for a client that has gone away.
	if r.Context().Err() != nil {
		return
	}
	if contentURL == "" {
		writeHandlerError(r, w, ErrEmptyFile)
		return
	}
	http.Redirect(w, r, contentURL, http.StatusFound)
}

// Don't include sprig just for one function.
var templateFunctions = map[string]any{
	"defaultString": func(s string, def string) string {
		if s == "" {
			return def
		}
		return s
	},
}

// NewServer parses templates and builds a Server.
func NewServer(mediaLib *MediaLibrary) (*Server, error) {
	tmpl, err := template.New("").Funcs(templateFunctions).ParseFS(embedFS, "templates/*.gohtml")
	if err != nil {
		return nil, err
	}
	return &Server{
		mediaLib:      mediaLib,
		tmpl:          tmpl,
		staticVersion: fmt.Sprintf("%x", rand.Uint64()),
	}, nil
}

// Handler builds the application mux. It is separated from StartServer so
// tests (and alternative listeners) can serve the exact production routes
// through httptest without binding a fixed address.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.Handle("/", http.RedirectHandler("/library/", http.StatusMovedPermanently))

	staticFS, err := fs.Sub(embedFS, "static")
	if err != nil {
		// embed.FS guarantees "static" exists at compile time.
		panic(err)
	}
	staticPath := fmt.Sprintf("/static/%s/", s.staticVersion)
	mux.Handle(staticPath, DisableFileListing(http.StripPrefix(staticPath, http.FileServer(http.FS(staticFS)))))

	mux.Handle("/library/", http.StripPrefix("/library/", ValidatePath(NormalizePath(s.ListingHandler))))
	mux.Handle("/stream/", http.StripPrefix("/stream/", ValidatePath(NormalizePath(s.StreamHandler))))

	// Run traversal protection before the mux so raw "/stream/../x" targets
	// get a 400 instead of ServeMux's own redirect, satisfying the "no
	// redirect on an escaping path" contract.
	return ValidatePath(http.HandlerFunc(mux.ServeHTTP))
}

// NewHandler builds the fully wired HTTP handler for the media library.
func NewHandler(mediaLib *MediaLibrary) (http.Handler, error) {
	srv, err := NewServer(mediaLib)
	if err != nil {
		return nil, err
	}
	return srv.Handler(), nil
}

// StartServer starts HTTP server.
func StartServer(mediaLib *MediaLibrary, addr string) error {
	handler, err := NewHandler(mediaLib)
	if err != nil {
		return err
	}
	return http.ListenAndServe(addr, handler)
}
