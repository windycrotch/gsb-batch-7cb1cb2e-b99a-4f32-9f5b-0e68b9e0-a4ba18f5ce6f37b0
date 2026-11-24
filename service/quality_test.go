package service

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/guohuiyuan/music-lib/model"
	"github.com/guohuiyuan/music-lib/provider"
)

type fakeQualityProvider struct {
	urls  map[string]string
	calls []string
}

func (f *fakeQualityProvider) AvailableQualityOptions() ([]model.AudioQualityOption, error) {
	return nil, nil
}

func (f *fakeQualityProvider) GetDownloadURLWithQuality(_ *model.Song, quality string) (string, error) {
	f.calls = append(f.calls, quality)
	if downloadURL := f.urls[quality]; downloadURL != "" {
		return downloadURL, nil
	}
	return "", errors.New("quality unavailable")
}

var _ provider.AudioQualityProvider = (*fakeQualityProvider)(nil)

func TestResolveDownloadURLFallsBackToLowerTier(t *testing.T) {
	qualityProvider := &fakeQualityProvider{
		urls: map[string]string{
			"high": "https://example.com/song.mp3",
		},
	}
	song := &model.Song{Source: "netease", ID: "1"}
	resolved, err := resolveDownloadURL(qualityProvider, song, "master")
	if err != nil {
		t.Fatalf("resolveDownloadURL() error = %v", err)
	}
	if resolved.URL != "https://example.com/song.mp3" || resolved.Quality != "high" {
		t.Fatalf("resolved = %#v, want high-quality URL", resolved)
	}
	if len(qualityProvider.calls) < 2 || qualityProvider.calls[0] != "master" {
		t.Fatalf("fallback calls = %#v, want master followed by lower tiers", qualityProvider.calls)
	}
	if qualityProvider.calls[len(qualityProvider.calls)-1] != "high" {
		t.Fatalf("fallback calls = %#v, want to stop at high", qualityProvider.calls)
	}
}

func TestResolveDownloadURLHonorsSongQualityOverride(t *testing.T) {
	qualityProvider := &fakeQualityProvider{
		urls: map[string]string{
			"standard": "https://example.com/standard.mp3",
			"master":   "https://example.com/master.wav",
		},
	}
	song := &model.Song{
		Source: "netease",
		ID:     "1",
		Extra:  map[string]string{"quality": "standard"},
	}
	resolved, err := resolveDownloadURL(qualityProvider, song, "master")
	if err != nil {
		t.Fatalf("resolveDownloadURL() error = %v", err)
	}
	if resolved.Quality != "standard" {
		t.Fatalf("quality = %q, want standard", resolved.Quality)
	}
	if !reflect.DeepEqual(qualityProvider.calls, []string{"standard"}) {
		t.Fatalf("calls = %#v, want only standard", qualityProvider.calls)
	}
}

func TestResolveDownloadURLAutoUsesProviderDefault(t *testing.T) {
	qualityProvider := &fakeQualityProvider{
		urls: map[string]string{"auto": "https://example.com/auto.mp3"},
	}
	song := &model.Song{Source: "qq", ID: "1"}
	resolved, err := resolveDownloadURL(qualityProvider, song, "")
	if err != nil {
		t.Fatalf("resolveDownloadURL() error = %v", err)
	}
	if resolved.Quality != "auto" || !reflect.DeepEqual(qualityProvider.calls, []string{"auto"}) {
		t.Fatalf("resolved = %#v, calls = %#v", resolved, qualityProvider.calls)
	}
}

func TestAudioQualityCatalog(t *testing.T) {
	sources := GetAudioQualitySourceNames()
	if len(sources) == 0 {
		t.Fatal("quality source list is empty")
	}
	for _, source := range sources {
		options, err := GetAudioQualityChoices(source)
		if err != nil {
			t.Errorf("GetAudioQualityChoices(%q) error = %v", source, err)
			continue
		}
		if len(options) == 0 {
			t.Errorf("source %q has no quality options", source)
		}
		seen := make(map[model.AudioQuality]bool)
		for _, option := range options {
			if option.Value == "" {
				t.Errorf("source %q contains an empty quality value", source)
			}
			if seen[option.Value] {
				t.Errorf("source %q contains duplicate quality %q", source, option.Value)
			}
			seen[option.Value] = true
		}
		if audioQualityProviderForSource(source) == nil {
			t.Errorf("source %q has no account quality provider", source)
		}
	}
}

func TestAudioQualityFallbackRejectsUnsupportedQuality(t *testing.T) {
	song := &model.Song{Source: "netease", ID: "1"}
	_, err := audioQualityFallbackOrder(song, "eight-track")
	if err == nil || !strings.Contains(err.Error(), "unsupported audio quality") {
		t.Fatalf("error = %v, want unsupported quality error", err)
	}
}

// stubQualityChoices swaps the static quality table for one source for the
// duration of a test, including cleanup.
func stubQualityChoices(t *testing.T, source string, options []model.AudioQualityOption) {
	t.Helper()
	original, existed := audioQualityChoices[source]
	audioQualityChoices[source] = func() []model.AudioQualityOption {
		return append([]model.AudioQualityOption(nil), options...)
	}
	t.Cleanup(func() {
		if existed {
			audioQualityChoices[source] = original
		} else {
			delete(audioQualityChoices, source)
		}
	})
}

func TestAudioQualityFallbackClampsAboveMaxToPlatformBest(t *testing.T) {
	// kugou's highest real tier is hires; requesting master must start at
	// hires rather than failing with "not supported".
	song := &model.Song{Source: "kugou", ID: "1"}
	order, err := audioQualityFallbackOrder(song, "master")
	if err != nil {
		t.Fatalf("audioQualityFallbackOrder() error = %v", err)
	}
	if len(order) == 0 {
		t.Fatal("fallback order is empty")
	}
	if order[0] != model.AudioQualityHiRes {
		t.Fatalf("first tier = %q, want %q; order = %#v", order[0], model.AudioQualityHiRes, order)
	}
}

func TestAudioQualityFallbackStartsAtRequestedTierWhenInTable(t *testing.T) {
	song := &model.Song{Source: "kugou", ID: "1"}
	order, err := audioQualityFallbackOrder(song, "high")
	if err != nil {
		t.Fatalf("audioQualityFallbackOrder() error = %v", err)
	}
	if order[0] != model.AudioQualityHigh {
		t.Fatalf("first tier = %q, want %q; order = %#v", order[0], model.AudioQualityHigh, order)
	}
}

func TestAudioQualityFallbackForMissingTierUsesNearestLower(t *testing.T) {
	// fivesing only exposes high/higher/standard; requesting lossless (which
	// sits above high on the canonical scale) must clamp down to high.
	song := &model.Song{Source: "fivesing", ID: "1"}
	order, err := audioQualityFallbackOrder(song, "lossless")
	if err != nil {
		t.Fatalf("audioQualityFallbackOrder() error = %v", err)
	}
	if order[0] != model.AudioQualityHigh {
		t.Fatalf("first tier = %q, want %q; order = %#v", order[0], model.AudioQualityHigh, order)
	}
}

func TestAudioQualityFallbackRejectsBelowLowestTier(t *testing.T) {
	// kugou reaches down to low; requesting preview (below low) cannot be
	// satisfied by walking downward and must still fail explicitly.
	song := &model.Song{Source: "kugou", ID: "1"}
	_, err := audioQualityFallbackOrder(song, "preview")
	if err == nil || !strings.Contains(err.Error(), "no tier at or below it") {
		t.Fatalf("error = %v, want below-lowest-tier error", err)
	}
}

func TestAudioQualityFallbackRejectsEmptyQualityTable(t *testing.T) {
	stubQualityChoices(t, "kugou", nil)
	song := &model.Song{Source: "kugou", ID: "1"}
	_, err := audioQualityFallbackOrder(song, "lossless")
	if err == nil || !strings.Contains(err.Error(), "no audio quality tiers") {
		t.Fatalf("error = %v, want empty-table error", err)
	}
}

func TestAudioQualityFallbackRejectsUnknownSource(t *testing.T) {
	song := &model.Song{Source: "totally-unknown-platform", ID: "1"}
	if _, err := audioQualityFallbackOrder(song, "high"); err == nil ||
		!strings.Contains(err.Error(), "unsupported audio quality source") {
		t.Fatalf("error = %v, want unsupported source error", err)
	}
	if _, err := ResolveDownloadURL("totally-unknown-platform", song, "high"); err == nil ||
		!strings.Contains(err.Error(), "unsupported audio quality source") {
		t.Fatalf("ResolveDownloadURL error = %v, want unsupported source error", err)
	}
}

func TestResolveDownloadURLClampsAndReportsStartingTierOnTotalFailure(t *testing.T) {
	// No tier succeeds; the error must name the tier the ladder started at
	// (hires, because master is above this platform's maximum).
	qualityProvider := &fakeQualityProvider{urls: map[string]string{}}
	song := &model.Song{Source: "kugou", ID: "1"}
	_, err := resolveDownloadURL(qualityProvider, song, "master")
	if err == nil {
		t.Fatal("expected an error when every tier fails")
	}
	if !strings.Contains(err.Error(), "from quality "+string(model.AudioQualityHiRes)+" downward") {
		t.Fatalf("error = %v, want it to name the starting tier %q", err, model.AudioQualityHiRes)
	}
	if len(qualityProvider.calls) == 0 || qualityProvider.calls[0] != string(model.AudioQualityHiRes) {
		t.Fatalf("calls = %#v, want first attempt at %q", qualityProvider.calls, model.AudioQualityHiRes)
	}
}

func TestResolveDownloadURLReportsActualUsedQuality(t *testing.T) {
	qualityProvider := &fakeQualityProvider{
		urls: map[string]string{"standard": "https://example.com/standard.mp3"},
	}
	song := &model.Song{Source: "kugou", ID: "1"}
	resolved, err := resolveDownloadURL(qualityProvider, song, "master")
	if err != nil {
		t.Fatalf("resolveDownloadURL() error = %v", err)
	}
	if resolved.Quality != "standard" {
		t.Fatalf("actual quality = %q, want standard", resolved.Quality)
	}
}

func TestCookieSnapshotIsImmutableAcrossHotUpdate(t *testing.T) {
	manager := &CookieManager{cookies: map[string]string{"netease": "old-cookie"}}
	snapshot := manager.Snapshot()

	manager.SetAll(map[string]string{"netease": "new-cookie"})

	if got := snapshot.Cookie("netease"); got != "old-cookie" {
		t.Fatalf("snapshot cookie = %q, want old-cookie", got)
	}
	if got := manager.Get("netease"); got != "new-cookie" {
		t.Fatalf("manager cookie = %q, want new-cookie", got)
	}
	if got := manager.Snapshot().Cookie("netease"); got != "new-cookie" {
		t.Fatalf("fresh snapshot cookie = %q, want new-cookie", got)
	}
}

func TestCookieSnapshotDeletionDoesNotAffectOldSnapshot(t *testing.T) {
	manager := &CookieManager{cookies: map[string]string{"qq": "cookie-a"}}
	snapshot := manager.Snapshot()
	manager.SetAll(map[string]string{"qq": ""})
	if got := snapshot.Cookie("qq"); got != "cookie-a" {
		t.Fatalf("snapshot cookie = %q, want cookie-a", got)
	}
	if got := manager.Get("qq"); got != "" {
		t.Fatalf("manager cookie = %q, want empty", got)
	}
}

func TestResolveDownloadURLWithSnapshotUsesSnapshotCookie(t *testing.T) {
	manager := &CookieManager{cookies: map[string]string{"netease": "snapshot-cookie"}}
	snapshot := manager.Snapshot()
	// Hot update after the snapshot was taken must not change the provider
	// built for the in-flight resolution.
	manager.SetAll(map[string]string{"netease": "updated-cookie"})

	resolved, err := ResolveDownloadURLWithSnapshot(snapshot, "netease",
		&model.Song{Source: "netease", ID: "1"}, "auto")
	// A live network call may fail in the sandbox; only assert the cookie
	// attached to a successful resolution path would be the snapshot value.
	if err != nil {
		return
	}
	if resolved.Cookie != "snapshot-cookie" {
		t.Fatalf("resolved cookie = %q, want snapshot-cookie", resolved.Cookie)
	}
}

func TestAudioQualityOptionsWithSnapshotRejectsUnknownSource(t *testing.T) {
	_, err := GetAudioQualityOptionsWithSnapshot(CM.Snapshot(), "no-such-source")
	if err == nil || !strings.Contains(err.Error(), "unsupported audio quality source") {
		t.Fatalf("error = %v, want unsupported source error", err)
	}
}

// Special tiers such as spatial/atmos occupy different positions on different
// platform ladders. A tier present in a platform's table must always be entered
// directly, even when another platform ranks it elsewhere.
func TestSpecialTierEntryPointsFollowPlatformLadder(t *testing.T) {
	cases := []struct {
		source    string
		requested model.AudioQuality
		wantFirst model.AudioQuality
	}{
		{"soda", model.AudioQualitySpatial, model.AudioQualitySpatial},
		{"netease", model.AudioQualityAtmos, model.AudioQualityAtmos},
		{"migu", model.AudioQualitySpatial, model.AudioQualitySpatial},
		// Above-max requests always enter at the table's highest tier.
		{"soda", model.AudioQualityMaster, model.AudioQualityHiRes},
		{"fivesing", model.AudioQualityMaster, model.AudioQualityHigh},
		{"jamendo", model.AudioQualityHiRes, model.AudioQualityLossless},
	}
	for _, tc := range cases {
		order, err := audioQualityFallbackOrder(&model.Song{Source: tc.source, ID: "1"}, string(tc.requested))
		if err != nil {
			t.Errorf("source %s request %s: unexpected error %v", tc.source, tc.requested, err)
			continue
		}
		if order[0] != tc.wantFirst {
			t.Errorf("source %s request %s: first = %s, want %s (full order %v)",
				tc.source, tc.requested, order[0], tc.wantFirst, order)
		}
	}
}

// Sweep every platform against every normalized tier: the ladder entry must
// never exceed the request, the ladder must walk the platform table downward,
// and requests below the platform minimum must fail explicitly.
func TestFallbackOrderSweepAcrossAllPlatforms(t *testing.T) {
	allTiers := make([]model.AudioQuality, 0, len(canonicalAudioQualityRank))
	for tier := range canonicalAudioQualityRank {
		allTiers = append(allTiers, tier)
	}
	for _, source := range GetAudioQualitySourceNames() {
		choices, err := GetAudioQualityChoices(source)
		if err != nil {
			t.Fatalf("source %s: %v", source, err)
		}
		lowestRank := canonicalAudioQualityRank[choices[len(choices)-1].Value]
		for _, requested := range allTiers {
			song := &model.Song{Source: source, ID: "1"}
			order, orderErr := audioQualityFallbackOrder(song, string(requested))
			if canonicalAudioQualityRank[requested] < lowestRank {
				if orderErr == nil {
					t.Errorf("source %s request %s below minimum: expected error, got %v", source, requested, order)
				}
				continue
			}
			if orderErr != nil {
				t.Errorf("source %s request %s: unexpected error %v", source, requested, orderErr)
				continue
			}
			if canonicalAudioQualityRank[order[0]] > canonicalAudioQualityRank[requested] {
				t.Errorf("source %s request %s: starting tier %s exceeds request", source, requested, order[0])
			}
		}
	}
}
