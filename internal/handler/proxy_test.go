package handler

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/dovakiin0/proxy-m3u8/config"
	"github.com/dovakiin0/proxy-m3u8/internal/utils"
	"github.com/labstack/echo/v4"
	"github.com/redis/go-redis/v9"
)

// testUpstream is a controllable local origin server.
type testUpstream struct {
	srv *httptest.Server

	mu       sync.Mutex
	mode     string // "ok" | "500" | "slow" | "hang"
	referers map[string][]string
	hitCount map[string]int
}

func newTestUpstream(t *testing.T) *testUpstream {
	t.Helper()
	u := &testUpstream{
		mode:     "ok",
		referers: map[string][]string{},
		hitCount: map[string]int{},
	}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.referers[r.URL.Path] = append(u.referers[r.URL.Path], r.Header.Get("Referer"))
		u.hitCount[r.URL.Path]++
		mode := u.mode
		u.mu.Unlock()

		switch mode {
		case "500":
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, "upstream exploded")
			return
		case "slow":
			time.Sleep(300 * time.Millisecond)
		case "hang":
			time.Sleep(3 * time.Second)
		}

		switch r.URL.Path {
		case "/playlist.m3u8":
			w.Header().Set("Content-Type", "application/x-mpegURL")
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:2.0,\nseg1.ts\n#EXTINF:2.0,\nseg2.ts\n#EXT-X-ENDLIST\n")
		case "/master.m3u8":
			w.Header().Set("Content-Type", "application/x-mpegURL")
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=800000\nindex.m3u8\n")
		case "/index.m3u8":
			w.Header().Set("Content-Type", "application/x-mpegURL")
			fmt.Fprint(w, "#EXTM3U\n#EXTINF:2.0,\nseg1.ts\n#EXT-X-ENDLIST\n")
		case "/seg1.ts", "/seg2.ts":
			w.Header().Set("Content-Type", "video/mp2t")
			fmt.Fprint(w, "fake-ts-data")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *testUpstream) setMode(m string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.mode = m
}

func (u *testUpstream) hits(path string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hitCount[path]
}

func (u *testUpstream) lastReferer(path string) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	refs := u.referers[path]
	if len(refs) == 0 {
		return ""
	}
	return refs[len(refs)-1]
}

// proxyEnv wires the handler to a miniredis instance and a real echo server.
type proxyEnv struct {
	baseURL string
	redis   *miniredis.Miniredis
	client  *http.Client
}

func newProxyEnv(t *testing.T, withRedis bool) *proxyEnv {
	t.Helper()

	oldClient, oldAvail := config.Client, config.IsAvailable
	t.Cleanup(func() { config.Client, config.IsAvailable = oldClient, oldAvail })

	var mr *miniredis.Miniredis
	if withRedis {
		mr = miniredis.RunT(t)
		config.Client = redis.NewClient(&redis.Options{Addr: mr.Addr()})
		config.IsAvailable = true
		t.Cleanup(func() { config.Client.Close() })
	} else {
		config.Client = nil
		config.IsAvailable = false
	}

	e := echo.New()
	e.GET("/m3u8-proxy", M3U8ProxyHandler)
	srv := httptest.NewServer(e)
	t.Cleanup(srv.Close)

	return &proxyEnv{baseURL: srv.URL, redis: mr, client: srv.Client()}
}

// get issues a real HTTP request against the proxy endpoint.
func (p *proxyEnv) get(t *testing.T, targetURL, referer string) (int, string) {
	t.Helper()
	q := url.Values{}
	q.Set("url", targetURL)
	if referer != "" {
		q.Set("referer", referer)
	}
	return p.getRaw(t, "/m3u8-proxy?"+q.Encode())
}

// getRaw requests an already-encoded path (used for nested playlist URLs).
func (p *proxyEnv) getRaw(t *testing.T, path string) (int, string) {
	t.Helper()
	resp, err := p.client.Get(p.baseURL + path)
	if err != nil {
		t.Fatalf("proxy request failed: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading proxy response: %v", err)
	}
	return resp.StatusCode, string(body)
}

func TestCacheKeyIsolatesReferers(t *testing.T) {
	up := newTestUpstream(t)
	p := newProxyEnv(t, true)
	target := up.srv.URL + "/playlist.m3u8"

	status, bodyA := p.get(t, target, "https://site-a.example/")
	if status != http.StatusOK {
		t.Fatalf("referer A: got status %d", status)
	}
	if !strings.Contains(bodyA, "referer="+url.QueryEscape("https://site-a.example/")) {
		t.Fatalf("referer A: rewritten body missing its referer:\n%s", bodyA)
	}
	if got := up.lastReferer("/playlist.m3u8"); got != "https://site-a.example/" {
		t.Fatalf("upstream saw referer %q, want site-a", got)
	}

	// Same playlist, different referer: must NOT be served from A's cache entry.
	status, bodyB := p.get(t, target, "https://site-b.example/")
	if status != http.StatusOK {
		t.Fatalf("referer B: got status %d", status)
	}
	if !strings.Contains(bodyB, "referer="+url.QueryEscape("https://site-b.example/")) {
		t.Fatalf("referer B: rewritten body missing its referer:\n%s", bodyB)
	}
	if strings.Contains(bodyB, "site-a") {
		t.Fatalf("referer B: got content rewritten for referer A:\n%s", bodyB)
	}
	if up.hits("/playlist.m3u8") != 2 {
		t.Fatalf("expected 2 upstream fetches (one per referer), got %d", up.hits("/playlist.m3u8"))
	}

	// Repeat referer A: served from cache, no new upstream fetch, still A's content.
	_, bodyA2 := p.get(t, target, "https://site-a.example/")
	if up.hits("/playlist.m3u8") != 2 {
		t.Fatalf("expected cache hit for referer A, upstream hits = %d", up.hits("/playlist.m3u8"))
	}
	if !strings.Contains(bodyA2, "site-a") {
		t.Fatalf("cached body for referer A is wrong:\n%s", bodyA2)
	}
}

func TestUpstream5xxPropagatesAndLeavesNoCache(t *testing.T) {
	up := newTestUpstream(t)
	p := newProxyEnv(t, true)
	target := up.srv.URL + "/playlist.m3u8"

	up.setMode("500")
	status, _ := p.get(t, target, "https://site-a.example/")
	if status != http.StatusInternalServerError {
		t.Fatalf("expected 500 propagated to client, got %d", status)
	}
	if keys := p.redis.Keys(); len(keys) != 0 {
		t.Fatalf("failed request left cache entries behind: %v", keys)
	}

	// Retry after the upstream recovers: must go back to origin and succeed.
	up.setMode("ok")
	status, body := p.get(t, target, "https://site-a.example/")
	if status != http.StatusOK {
		t.Fatalf("retry after recovery: got status %d", status)
	}
	if !strings.Contains(body, "seg1.ts") && !strings.Contains(body, url.QueryEscape("seg1.ts")) {
		t.Fatalf("retry after recovery: unexpected body:\n%s", body)
	}
	if up.hits("/playlist.m3u8") != 2 {
		t.Fatalf("expected retry to hit origin, upstream hits = %d", up.hits("/playlist.m3u8"))
	}
}

func TestUpstreamTimeoutReturns504AndLeavesNoCache(t *testing.T) {
	up := newTestUpstream(t)
	p := newProxyEnv(t, true)
	target := up.srv.URL + "/playlist.m3u8"

	oldClient := utils.ProxyHTTPClient
	utils.ProxyHTTPClient = &http.Client{Timeout: 500 * time.Millisecond}
	t.Cleanup(func() { utils.ProxyHTTPClient = oldClient })

	up.setMode("hang")
	status, _ := p.get(t, target, "https://site-a.example/")
	if status != http.StatusGatewayTimeout {
		t.Fatalf("expected 504 on upstream timeout, got %d", status)
	}
	if keys := p.redis.Keys(); len(keys) != 0 {
		t.Fatalf("timed-out request left cache entries behind: %v", keys)
	}
}

func TestSlowUpstreamStillWorks(t *testing.T) {
	up := newTestUpstream(t)
	p := newProxyEnv(t, true)
	target := up.srv.URL + "/playlist.m3u8"

	up.setMode("slow")
	status, body := p.get(t, target, "https://site-a.example/")
	if status != http.StatusOK {
		t.Fatalf("slow upstream: got status %d", status)
	}
	if !strings.Contains(body, "m3u8-proxy") {
		t.Fatalf("slow upstream: body not rewritten:\n%s", body)
	}
}

func TestNestedPlaylistKeepsReferer(t *testing.T) {
	up := newTestUpstream(t)
	p := newProxyEnv(t, true)
	target := up.srv.URL + "/master.m3u8"

	status, body := p.get(t, target, "https://nested.example/")
	if status != http.StatusOK {
		t.Fatalf("master playlist: got status %d", status)
	}

	// Extract the rewritten nested playlist URL and follow it through the proxy.
	var nested string
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, "index.m3u8") {
			nested = strings.TrimSpace(line)
		}
	}
	if nested == "" {
		t.Fatalf("no rewritten nested playlist found in:\n%s", body)
	}
	if !strings.Contains(nested, "referer="+url.QueryEscape("https://nested.example/")) {
		t.Fatalf("nested URL does not carry the request referer: %s", nested)
	}

	status, indexBody := p.getRaw(t, "/"+nested)
	if status != http.StatusOK {
		t.Fatalf("nested playlist: got status %d", status)
	}
	if got := up.lastReferer("/index.m3u8"); got != "https://nested.example/" {
		t.Fatalf("nested upstream request carried referer %q, want https://nested.example/", got)
	}
	if !strings.Contains(indexBody, "referer="+url.QueryEscape("https://nested.example/")) {
		t.Fatalf("nested playlist segments lost the referer:\n%s", indexBody)
	}
}

func TestConcurrentMixedReferers(t *testing.T) {
	up := newTestUpstream(t)
	p := newProxyEnv(t, true)
	target := up.srv.URL + "/playlist.m3u8"

	const workers = 8
	referers := []string{"https://r1.example/", "https://r2.example/"}

	var wg sync.WaitGroup
	errs := make(chan string, workers*2)
	for i := 0; i < workers; i++ {
		for _, ref := range referers {
			wg.Add(1)
			go func(ref string) {
				defer wg.Done()
				status, body := p.get(t, target, ref)
				if status != http.StatusOK {
					errs <- fmt.Sprintf("referer %s: status %d", ref, status)
					return
				}
				if !strings.Contains(body, "referer="+url.QueryEscape(ref)) {
					errs <- fmt.Sprintf("referer %s: body carries wrong referer:\n%s", ref, body)
				}
			}(ref)
		}
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

func TestRedisDownFallsBackToOrigin(t *testing.T) {
	up := newTestUpstream(t)
	p := newProxyEnv(t, false) // no redis at all
	target := up.srv.URL + "/playlist.m3u8"

	status, body := p.get(t, target, "https://site-a.example/")
	if status != http.StatusOK {
		t.Fatalf("no redis: got status %d", status)
	}
	if !strings.Contains(body, "referer="+url.QueryEscape("https://site-a.example/")) {
		t.Fatalf("no redis: body not rewritten correctly:\n%s", body)
	}
}

func TestSegmentRequestUsesItsOwnReferer(t *testing.T) {
	up := newTestUpstream(t)
	p := newProxyEnv(t, true)
	target := up.srv.URL + "/playlist.m3u8"

	_, body := p.get(t, target, "https://seg.example/")

	var segURL string
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, "seg1.ts") {
			segURL = strings.TrimSpace(line)
		}
	}
	if segURL == "" {
		t.Fatalf("no rewritten segment URL in:\n%s", body)
	}
	status, seg := p.getRaw(t, "/"+segURL)
	if status != http.StatusOK {
		t.Fatalf("segment request: got status %d", status)
	}
	if seg != "fake-ts-data" {
		t.Fatalf("segment body mismatch: %q", seg)
	}
	if got := up.lastReferer("/seg1.ts"); got != "https://seg.example/" {
		t.Fatalf("segment upstream request carried referer %q, want https://seg.example/", got)
	}
}
