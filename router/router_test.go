package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/guohuiyuan/music-lib/model"
)

func TestQualityEndpointReturnsStaticCatalog(t *testing.T) {
	recorder := serveJSON("/api/v1/music/qualities?source=netease")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	var response struct {
		Code int `json:"code"`
		Data struct {
			Source    string                     `json:"source"`
			Qualities []model.AudioQualityOption `json:"qualities"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Code != http.StatusOK || response.Data.Source != "netease" {
		t.Fatalf("unexpected response: %s", recorder.Body.String())
	}
	if len(response.Data.Qualities) == 0 {
		t.Fatal("quality catalog is empty")
	}
}

func TestQualityCompatibilityRoute(t *testing.T) {
	recorder := serveJSON("/music/qualities?source=qq")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"source":"qq"`) {
		t.Fatalf("compatibility response missing qq source: %s", recorder.Body.String())
	}
}

func TestQualityProbeRejectsMultipleSourcesOnBothRoutes(t *testing.T) {
	paths := []string{
		"/api/v1/music/qualities?probe=true&sources=netease&sources=qq",
		"/music/qualities?probe=true&sources=netease&sources=qq",
		"/api/v1/music/qualities?probe=1&sources=netease&sources=qq",
	}
	for _, path := range paths {
		recorder := serveJSON(path)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("GET %s status = %d, want 400; body = %s", path, recorder.Code, recorder.Body.String())
		}
	}
}

func TestQualityProbeAcceptsSingleSourceOnBothRoutes(t *testing.T) {
	// The single-source guard runs before any network probe, so validation
	// must accept the request shape on both standard and compatibility routes.
	paths := []string{
		"/api/v1/music/qualities?probe=true&source=kugou",
		"/music/qualities?probe=true&source=kugou",
	}
	for _, path := range paths {
		recorder := serveJSON(path)
		if recorder.Code != http.StatusOK {
			t.Errorf("GET %s status = %d, want 200; body = %s", path, recorder.Code, recorder.Body.String())
		}
	}
}

func TestQualityUnknownSourceReturnsExplicitErrorOnBothRoutes(t *testing.T) {
	paths := []string{
		"/api/v1/music/qualities?source=not-a-platform",
		"/music/qualities?source=not-a-platform",
		"/api/v1/music/qualities?probe=true&source=not-a-platform",
	}
	for _, path := range paths {
		recorder := serveJSON(path)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("GET %s status = %d, want 400; body = %s", path, recorder.Code, recorder.Body.String())
			continue
		}
		if !strings.Contains(recorder.Body.String(), "unsupported audio quality source") {
			t.Errorf("GET %s body = %s, want explicit unsupported-source message", path, recorder.Body.String())
		}
	}
}

func TestQualityRoutesBehaveIdentically(t *testing.T) {
	queries := []string{
		"?source=netease",
		"?probe=true&sources=netease&sources=qq",
		"?source=not-a-platform",
	}
	for _, query := range queries {
		standard := serveJSON("/api/v1/music/qualities" + query)
		compat := serveJSON("/music/qualities" + query)
		if standard.Code != compat.Code {
			t.Errorf("query %q: standard status %d != compat status %d", query, standard.Code, compat.Code)
		}
	}
}

func TestRankingValidationRoutes(t *testing.T) {
	tests := []struct {
		path string
		code int
	}{
		{"/api/v1/ranking/toplist", http.StatusBadRequest},
		{"/api/v1/ranking/hot?source=unknown", http.StatusBadRequest},
		{"/music/toplist_songs", http.StatusBadRequest},
	}
	for _, test := range tests {
		recorder := serveJSON(test.path)
		if recorder.Code != test.code {
			t.Errorf("GET %s status = %d, want %d; body = %s", test.path, recorder.Code, test.code, recorder.Body.String())
		}
	}
}

func serveJSON(path string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	SetupRouter().ServeHTTP(recorder, request)
	return recorder
}
