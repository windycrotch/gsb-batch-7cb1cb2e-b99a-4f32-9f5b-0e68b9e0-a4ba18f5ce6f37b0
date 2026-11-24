package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrEgressUnavailable marks a quest whose discovering egress was removed or
// replaced. Callers can use errors.Is even when the concrete error carries
// region-specific details.
var ErrEgressUnavailable = errors.New("quest egress is unavailable")

// ErrRateLimited is an optional sentinel for callers that classify a sweep
// whose targets all ended in HTTP 429.
var ErrRateLimited = errors.New("rate limited")

// ErrEgressBindingMissing is returned when a bound quest has no registered
// egress route.
var ErrEgressBindingMissing = errors.New("quest egress binding is missing")

// ErrEgressBindingMismatch is returned when a region code now points at a
// different egress endpoint than the one that discovered a quest.
var ErrEgressBindingMismatch = errors.New("quest egress binding does not match the registered route")

var (
	ErrRegionProxyMissing     = ErrEgressBindingMissing
	ErrProxyRemoved           = ErrEgressBindingMissing
	ErrProxyUnavailable       = ErrEgressUnavailable
	ErrRegionProxyUnavailable = ErrEgressUnavailable
	ErrEgressMissing          = ErrEgressBindingMissing
	ErrEgressMismatch         = ErrEgressBindingMismatch
	ErrEgressBinding          = ErrEgressUnavailable
	ErrProxyReplaced          = ErrEgressBindingMismatch
	ErrProxyBindingMismatch   = ErrEgressBindingMismatch
)

// IsRateLimitedError reports whether err wraps the sweep rate-limit sentinel.
func IsRateLimitedError(err error) bool {
	return errors.Is(err, ErrRateLimited)
}

// IsEgressUnavailable reports whether err is a missing/replaced egress error.
func IsEgressUnavailable(err error) bool {
	return errors.Is(err, ErrEgressUnavailable)
}

// EgressBindingError explains why a bound request refused to route.
type EgressBindingError struct {
	Code    string
	Region  string
	Reason  string
	Bound   string
	Current string
}

// Common type-name aliases used by alternate test/call-site naming.
type (
	EgressError       = EgressBindingError
	ProxyBindingError = EgressBindingError
	RegionProxyError  = EgressBindingError
)

func (e *EgressBindingError) Error() string {
	code := strings.TrimSpace(e.Code)
	if code == "" {
		code = strings.TrimSpace(e.Region)
	}
	if code == "" {
		code = "UNKNOWN"
	}
	switch e.Reason {
	case "removed":
		return fmt.Sprintf("quest egress %s was removed after discovery; refusing to use direct connection", code)
	case "replaced":
		return fmt.Sprintf("quest egress %s was replaced after discovery; refusing to use the new route", code)
	case "mismatch":
		return fmt.Sprintf("quest egress binding for %s does not match the registered route", code)
	default:
		return fmt.Sprintf("quest egress %s is unavailable: %s", code, e.Reason)
	}
}

// Is lets errors.Is match both the broad egress error and the specific class.
func (e *EgressBindingError) Is(target error) bool {
	switch target {
	case ErrEgressUnavailable:
		return true
	case ErrEgressBindingMissing:
		return e.Reason == "removed"
	case ErrEgressBindingMismatch:
		return e.Reason == "replaced" || e.Reason == "mismatch"
	default:
		return false
	}
}

// EgressFingerprint returns a short non-secret fingerprint of a proxy URL. The
// full URL can contain userinfo credentials, so this is safe to persist.
func EgressFingerprint(proxyURL string) string {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return "direct"
	}
	sum := sha256.Sum256([]byte(proxyURL))
	return "egress-" + hex.EncodeToString(sum[:12])
}

// egressGate is a process-local serialization and Retry-After deadline for
// one exact egress endpoint. It is shared by all concurrent requests using the
// same RegionTransport and endpoint, so one scan cannot overwrite another
// scan's rate-limit state.
type egressGate struct {
	token chan struct{}

	mu         sync.Mutex
	retryUntil time.Time
}

func newEgressGate() *egressGate {
	return &egressGate{token: make(chan struct{}, 1)}
}

// begin serializes request starts for the egress and then waits until a
// previously advertised Retry-After deadline has elapsed. The returned finish
// function releases the request slot.
func (g *egressGate) begin(ctx context.Context) (func(), error) {
	if g == nil {
		return func() {}, nil
	}
	select {
	case g.token <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	g.mu.Lock()
	wait := time.Until(g.retryUntil)
	g.mu.Unlock()
	if wait > 0 {
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			<-g.token
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return func() { <-g.token }, nil
}

// penalize records a Retry-After deadline. A longer deadline is never
// overwritten by a shorter/late response.
func (g *egressGate) penalize(retryAfter time.Duration) {
	if g == nil || retryAfter <= 0 {
		return
	}
	until := time.Now().Add(retryAfter)
	g.mu.Lock()
	if until.After(g.retryUntil) {
		g.retryUntil = until
	}
	g.mu.Unlock()
}

func (g *egressGate) retryDeadline() time.Time {
	if g == nil {
		return time.Time{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.retryUntil
}

type regionInfoKey struct{}

type localeOverrideKey struct{}

// WithRegionInfo attaches a region to a request context. Every HTTP call made
// with the derived context is sent through that region's proxy (when one is
// configured) and carries matching locale/timezone headers.
func WithRegionInfo(ctx context.Context, info RegionInfo) context.Context {
	return context.WithValue(ctx, regionInfoKey{}, info)
}

// RegionInfoFromContext extracts the region attached with WithRegionInfo.
func RegionInfoFromContext(ctx context.Context) (RegionInfo, bool) {
	if ctx == nil {
		return RegionInfo{}, false
	}
	info, ok := ctx.Value(regionInfoKey{}).(RegionInfo)
	return info, ok
}

// withLocaleOverride pins locale/timezone for a single request context.
func withLocaleOverride(ctx context.Context, locale, timezone string) context.Context {
	return context.WithValue(ctx, localeOverrideKey{}, RegionInfo{Locale: locale, Timezone: timezone})
}

func localeOverrideFromContext(ctx context.Context) (RegionInfo, bool) {
	if ctx == nil {
		return RegionInfo{}, false
	}
	info, ok := ctx.Value(localeOverrideKey{}).(RegionInfo)
	return info, ok
}

// WithQuestEgress merges the egress remembered by quest into ctx. Explicit
// non-empty routing fields already on ctx win over empty quest values, while
// the quest's strict pin is preserved.
func WithQuestEgress(ctx context.Context, quest Quest) context.Context {
	questInfo := quest.EgressInfo()
	current, ok := RegionInfoFromContext(ctx)
	if !ok {
		if questInfo.Code == "" && !questInfo.RequiresEgress() {
			return ctx
		}
		return WithRegionInfo(ctx, questInfo)
	}
	return WithRegionInfo(ctx, MergeRegionInfo(current, questInfo))
}

// QuestEgressContext is an alias for WithQuestEgress.
func QuestEgressContext(ctx context.Context, quest Quest) context.Context {
	return WithQuestEgress(ctx, quest)
}

// MergeRegionInfo combines an ambient region with a quest-derived pin. The
// quest binding is authoritative when present; ambient locale and code fill
// values that the persisted quest does not carry.
func MergeRegionInfo(ambient, quest RegionInfo) RegionInfo {
	out := ambient
	if out.Code == "" {
		out.Code = quest.Code
	}
	if out.Locale == "" {
		out.Locale = quest.Locale
	}
	if out.Timezone == "" {
		out.Timezone = quest.Timezone
	}

	// A quest-derived exact endpoint/binding must not be discarded by an
	// ambient context that only supplied locale information.
	if proxyURL := quest.expectedProxyURL(); proxyURL != "" {
		out.ProxyURL = proxyURL
		out.Proxy = proxyURL
		out.ProxyAddress = proxyURL
		out.EgressURL = proxyURL
		out.Endpoint = proxyURL
		out.BoundEndpoint = proxyURL
		out.BoundProxy = proxyURL
		out.BoundProxyURL = proxyURL
	}
	if binding := quest.expectedBinding(); binding != "" {
		out.Binding = binding
		out.EgressBinding = binding
		out.ProxyBinding = binding
		out.EgressID = binding
		out.EgressKey = binding
		out.RouteKey = binding
	}
	if quest.RequiresEgress() {
		out.RequireProxy = true
		out.ProxyRequired = true
		out.Strict = true
		out.Bound = true
		out.Pinned = true
	}
	return out
}

// RememberQuestEgress stores the routing pin discovered for a quest ID. The
// first owner wins, matching mergeSweepPayload's first-region ownership.
func (c *Client) RememberQuestEgress(quest Quest) {
	if c == nil || quest.ID == "" {
		return
	}
	info := quest.EgressInfo()
	if info.Code == "" && !info.RequiresEgress() {
		return
	}
	c.egressBindings.LoadOrStore(quest.ID, info)
}

func (c *Client) lookupQuestEgress(questID string) (RegionInfo, bool) {
	if c == nil || questID == "" {
		return RegionInfo{}, false
	}
	if value, ok := c.egressBindings.Load(questID); ok {
		if info, ok := value.(RegionInfo); ok {
			return info, true
		}
	}
	return RegionInfo{}, false
}

// questEgressContext attaches a remembered pin to an enroll call even when the
// caller passed context.Background.
func (c *Client) questEgressContext(ctx context.Context, quest Quest) context.Context {
	c.RememberQuestEgress(quest)
	if remembered, ok := c.lookupQuestEgress(quest.ID); ok {
		if current, exists := RegionInfoFromContext(ctx); exists {
			return WithRegionInfo(ctx, MergeRegionInfo(current, remembered))
		}
		return WithRegionInfo(ctx, remembered)
	}
	return WithQuestEgress(ctx, quest)
}

// questIDEgressContext attaches a remembered pin to heartbeat/video calls,
// whose public signatures only carry the quest ID.
func (c *Client) questIDEgressContext(ctx context.Context, questID string) context.Context {
	remembered, ok := c.lookupQuestEgress(questID)
	if !ok {
		return ctx
	}
	if current, exists := RegionInfoFromContext(ctx); exists {
		return WithRegionInfo(ctx, MergeRegionInfo(current, remembered))
	}
	return WithRegionInfo(ctx, remembered)
}

// CheckEgress verifies that a pinned region route is currently usable without
// performing an HTTP request.
func (c *Client) CheckEgress(info RegionInfo) error {
	if c == nil || c.transport == nil {
		if info.RequiresEgress() {
			return &EgressBindingError{Code: info.Code, Region: info.Code, Reason: "removed"}
		}
		return nil
	}
	return c.transport.CheckEgress(info)
}

// WaitForEgress blocks until the shared Retry-After deadline for code has
// elapsed or ctx is canceled. It is primarily useful in tests and pacing
// adapters; HTTP calls already invoke the same gate internally.
func (c *Client) WaitForEgress(ctx context.Context, code string) error {
	if c == nil || c.transport == nil {
		return nil
	}
	_, gate, err := c.transport.resolve(RegionInfo{Code: code})
	if err != nil {
		return err
	}
	finish, err := gate.begin(ctx)
	if err != nil {
		return err
	}
	finish()
	return nil
}

// ReportRateLimit records a Retry-After deadline for the egress identified by
// code. The deadline is shared by all concurrent requests using this Client.
func (c *Client) ReportRateLimit(code string, retryAfter time.Duration) error {
	if c == nil || c.transport == nil {
		return nil
	}
	_, gate, err := c.transport.resolve(RegionInfo{Code: code})
	if err != nil {
		return err
	}
	gate.penalize(retryAfter)
	return nil
}

// ApplyRetryAfter is an alias for ReportRateLimit.
func (c *Client) ApplyRetryAfter(code string, retryAfter time.Duration) error {
	return c.ReportRateLimit(code, retryAfter)
}

// RateLimitUntil returns the current shared deadline for code.
func (c *Client) RateLimitUntil(code string) time.Time {
	if c == nil || c.transport == nil {
		return time.Time{}
	}
	_, gate, err := c.transport.resolve(RegionInfo{Code: code})
	if err != nil || gate == nil {
		return time.Time{}
	}
	return gate.retryDeadline()
}

// parseExplicitRetryAfter reports a wait supplied by the server. Unlike
// parseRetryAfter, absence is not turned into an arbitrary default.
func parseExplicitRetryAfter(resp *http.Response, body []byte) (time.Duration, bool) {
	var rl RateLimitResponse
	if err := json.Unmarshal(body, &rl); err == nil && rl.RetryAfter > 0 {
		return time.Duration(rl.RetryAfter * float64(time.Second)), true
	}
	if resp != nil {
		if headerVal := strings.TrimSpace(resp.Header.Get("Retry-After")); headerVal != "" {
			if sec, err := strconv.ParseFloat(headerVal, 64); err == nil && sec > 0 {
				return time.Duration(sec * float64(time.Second)), true
			}
			if when, err := http.ParseTime(headerVal); err == nil {
				if d := time.Until(when); d > 0 {
					return d, true
				}
				return 0, true
			}
		}
	}
	return 0, false
}
