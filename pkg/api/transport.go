package api

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// DirectEgressBinding is the pin used by the host's own network egress.
const DirectEgressBinding = "direct"

// RegionTransport routes requests to the proxy that belongs to the region in
// the request context. Requests without a region use the direct connection.
type RegionTransport struct {
	mu sync.RWMutex

	direct http.RoundTripper

	// proxies preserves the original route map for compatibility.
	proxies  map[string]http.RoundTripper
	urls     map[string]string
	bindings map[string]string
	original map[string]string
	removed  map[string]bool
	gates    map[string]*egressGate

	directGate *egressGate
}

// NewRegionTransport builds a transport with a direct fallback.
func NewRegionTransport() *RegionTransport {
	return &RegionTransport{
		direct:     cloneDefaultTransport(),
		proxies:    make(map[string]http.RoundTripper),
		urls:       make(map[string]string),
		bindings:   make(map[string]string),
		original:   make(map[string]string),
		removed:    make(map[string]bool),
		gates:      make(map[string]*egressGate),
		directGate: newEgressGate(),
	}
}

func cloneDefaultTransport() *http.Transport {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Transport{}
	}
	return base.Clone()
}

func normalizeRegionCode(code string) string {
	return strings.ToUpper(strings.TrimSpace(code))
}

func isDirectRegionCode(code string) bool {
	switch normalizeRegionCode(code) {
	case "", "LOCAL", "DIRECT", "LOCALHOST", "SELF":
		return true
	default:
		return false
	}
}

// SetProxy registers (or replaces) the HTTP/SOCKS5 proxy for a region code.
func (t *RegionTransport) SetProxy(code, proxyURL string) error {
	code = normalizeRegionCode(code)
	if code == "" {
		return fmt.Errorf("region proxy code must not be empty")
	}
	parsed, err := url.Parse(proxyURL)
	if err != nil {
		return fmt.Errorf("invalid proxy URL: %w", err)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https", "socks5", "socks5h":
	default:
		return fmt.Errorf("unsupported proxy scheme %q (use http, https, socks5 or socks5h)", parsed.Scheme)
	}
	if parsed.Host == "" {
		return fmt.Errorf("proxy URL has no host")
	}

	transport := cloneDefaultTransport()
	transport.Proxy = http.ProxyURL(parsed)
	transport.MaxIdleConnsPerHost = 4
	binding := EgressFingerprint(proxyURL)

	t.mu.Lock()
	if t.proxies == nil {
		t.proxies = make(map[string]http.RoundTripper)
	}
	if t.urls == nil {
		t.urls = make(map[string]string)
	}
	if t.bindings == nil {
		t.bindings = make(map[string]string)
	}
	if t.original == nil {
		t.original = make(map[string]string)
	}
	if t.removed == nil {
		t.removed = make(map[string]bool)
	}
	if t.gates == nil {
		t.gates = make(map[string]*egressGate)
	}

	previous := t.proxies[code]
	t.proxies[code] = transport
	t.urls[code] = proxyURL
	t.bindings[code] = binding
	if _, ok := t.original[code]; !ok {
		t.original[code] = proxyURL
	}
	delete(t.removed, code)
	gate := t.gates[binding]
	if gate == nil {
		gate = newEgressGate()
		t.gates[binding] = gate
	}
	t.mu.Unlock()

	if closer, ok := previous.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
	return nil
}

// RemoveProxy drops the active proxy registration for one region code. A
// tombstone is retained so a previously bound quest can fail explicitly rather
// than silently using the direct route.
func (t *RegionTransport) RemoveProxy(code string) {
	code = normalizeRegionCode(code)
	t.mu.Lock()
	if t.proxies != nil {
		delete(t.proxies, code)
	}
	if t.urls != nil {
		delete(t.urls, code)
	}
	if t.bindings != nil {
		delete(t.bindings, code)
	}
	if t.removed == nil {
		t.removed = make(map[string]bool)
	}
	t.removed[code] = true
	if t.original == nil {
		t.original = make(map[string]string)
	}
	if _, ok := t.original[code]; !ok {
		t.original[code] = ""
	}
	t.mu.Unlock()
}

// HasProxy reports whether an active proxy is registered for a region code.
func (t *RegionTransport) HasProxy(code string) bool {
	code = normalizeRegionCode(code)
	t.mu.RLock()
	_, ok := t.proxies[code]
	t.mu.RUnlock()
	return ok
}

// CheckEgress returns an error when info is bound to a proxy route that no
// longer matches the currently registered endpoint.
func (t *RegionTransport) CheckEgress(info RegionInfo) error {
	_, _, err := t.resolve(info)
	return err
}

// gateFor returns the shared rate-limit gate for a binding, lazily creating it
// when tests or compatibility code populated the route map without SetProxy.
func (t *RegionTransport) gateFor(code, binding string) *egressGate {
	key := binding
	if key == "" {
		key = "code:" + code
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.gates == nil {
		t.gates = make(map[string]*egressGate)
	}
	if gate := t.gates[key]; gate != nil {
		return gate
	}
	gate := newEgressGate()
	t.gates[key] = gate
	return gate
}

// resolve selects the exact RoundTripper and rate-limit gate for info.
func (t *RegionTransport) resolve(info RegionInfo) (http.RoundTripper, *egressGate, error) {
	code := normalizeRegionCode(info.Code)
	expectedURL := info.expectedProxyURL()
	expectedBinding := info.expectedBinding()

	// The explicit direct pin always uses the host's own egress.
	if expectedBinding == DirectEgressBinding || (expectedURL == "" && isDirectRegionCode(code)) {
		gate := t.directGate
		if gate == nil {
			gate = t.gateFor(code, DirectEgressBinding)
		}
		return t.direct, gate, nil
	}

	t.mu.RLock()
	var (
		rt             http.RoundTripper
		currentURL     string
		currentBinding string
		originalURL    string
		wasRemoved     bool
		gate           *egressGate
	)
	if t.proxies != nil {
		rt = t.proxies[code]
	}
	if t.urls != nil {
		currentURL = t.urls[code]
	}
	if t.bindings != nil {
		currentBinding = t.bindings[code]
	}
	if t.original != nil {
		originalURL = t.original[code]
	}
	if t.removed != nil {
		wasRemoved = t.removed[code]
	}
	if t.gates != nil && currentBinding != "" {
		gate = t.gates[currentBinding]
	}
	direct := t.direct
	directGate := t.directGate
	t.mu.RUnlock()

	if rt != nil {
		if expectedURL != "" && expectedURL != currentURL {
			return nil, nil, &EgressBindingError{
				Code: code, Region: code, Reason: "replaced", Bound: expectedURL, Current: currentURL,
			}
		}
		if expectedBinding != "" && expectedBinding != currentBinding && expectedBinding != code {
			return nil, nil, &EgressBindingError{
				Code: code, Region: code, Reason: "replaced", Bound: expectedBinding, Current: currentBinding,
			}
		}
		// An unpinned context after SetProxy(code, differentURL) cannot prove it
		// belongs to the new egress. Refuse rather than replaying old sealed
		// metadata through a replacement IP.
		if expectedURL == "" && expectedBinding == "" &&
			originalURL != "" && currentURL != "" && originalURL != currentURL {
			return nil, nil, &EgressBindingError{
				Code: code, Region: code, Reason: "replaced", Bound: originalURL, Current: currentURL,
			}
		}
		if gate == nil {
			gate = t.gateFor(code, currentBinding)
		}
		return rt, gate, nil
	}

	if info.RequiresEgress() || wasRemoved {
		return nil, nil, &EgressBindingError{Code: code, Region: code, Reason: "removed", Bound: expectedURL}
	}

	// Backward-compatible behavior for locale-only probes and tests that attach
	// a bare region code without declaring an egress binding.
	if directGate == nil {
		directGate = t.gateFor(code, DirectEgressBinding)
	}
	return direct, directGate, nil
}

// RoundTrip implements http.RoundTripper.
func (t *RegionTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rt := t.direct
	gate := t.directGate

	if info, ok := RegionInfoFromContext(req.Context()); ok && normalizeRegionCode(info.Code) != "" {
		resolvedRT, resolvedGate, err := t.resolve(info)
		if err != nil {
			return nil, err
		}
		rt = resolvedRT
		if resolvedGate != nil {
			gate = resolvedGate
		}
	}

	if gate == nil {
		gate = t.gateFor("", DirectEgressBinding)
	}
	finish, err := gate.begin(req.Context())
	if err != nil {
		return nil, err
	}
	defer finish()

	resp, err := rt.RoundTrip(req)
	if err != nil {
		return resp, err
	}

	// Arm the shared egress gate from the actual 429 response. Buffering only
	// the error body lets us understand both JSON retry_after and HTTP
	// Retry-After headers while returning an equivalent response to callers.
	if resp.StatusCode == http.StatusTooManyRequests {
		body, readErr := io.ReadAll(resp.Body)
		closeErr := resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if explicit, ok := parseExplicitRetryAfter(resp, body); ok {
			gate.penalize(explicit)
		}
		resp.Body = io.NopCloser(bytes.NewReader(body))
		resp.ContentLength = int64(len(body))
	}

	return resp, nil
}

// CloseIdleConnections releases keep-alive connections held by every route.
func (t *RegionTransport) CloseIdleConnections() {
	t.mu.RLock()
	direct := t.direct
	proxies := make([]http.RoundTripper, 0, len(t.proxies))
	for _, transport := range t.proxies {
		proxies = append(proxies, transport)
	}
	t.mu.RUnlock()
	if closer, ok := direct.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
	for _, transport := range proxies {
		if closer, ok := transport.(interface{ CloseIdleConnections() }); ok {
			closer.CloseIdleConnections()
		}
	}
}
