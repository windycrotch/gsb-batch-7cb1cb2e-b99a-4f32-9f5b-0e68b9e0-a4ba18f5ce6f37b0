package handler

import (
	"net/http"
	"testing"

	"github.com/guohuiyuan/go-music-api/service"
)

func TestBuildReqPrefersResolvedSnapshotCookie(t *testing.T) {
	service.CM.SetAll(map[string]string{"netease": "live-cookie"})

	req, err := buildReq(http.MethodGet, "https://example.com/audio.mp3", "netease", "", "snapshot-cookie")
	if err != nil {
		t.Fatalf("buildReq() error = %v", err)
	}
	if got := req.Header.Get("Cookie"); got != "snapshot-cookie" {
		t.Fatalf("Cookie header = %q, want snapshot-cookie", got)
	}
}

func TestBuildReqFallsBackToLiveCookie(t *testing.T) {
	service.CM.SetAll(map[string]string{"qq": "live-cookie"})

	req, err := buildReq(http.MethodGet, "https://example.com/audio.mp3", "qq", "", "")
	if err != nil {
		t.Fatalf("buildReq() error = %v", err)
	}
	if got := req.Header.Get("Cookie"); got != "live-cookie" {
		t.Fatalf("Cookie header = %q, want live-cookie", got)
	}
}

func TestBuildReqOmitsEmptyCookie(t *testing.T) {
	service.CM.SetAll(map[string]string{"migu": ""})

	req, err := buildReq(http.MethodGet, "https://example.com/audio.mp3", "migu", "bytes=0-1", "")
	if err != nil {
		t.Fatalf("buildReq() error = %v", err)
	}
	if got := req.Header.Get("Cookie"); got != "" {
		t.Fatalf("Cookie header = %q, want empty", got)
	}
	if got := req.Header.Get("Range"); got != "bytes=0-1" {
		t.Fatalf("Range header = %q, want bytes=0-1", got)
	}
}
