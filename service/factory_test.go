package service

import (
	"fmt"
	"sync"
	"testing"
)

func TestFactorySourceCoverage(t *testing.T) {
	for _, source := range GetAllSourceNames() {
		if GetSearchFunc(source) == nil {
			t.Errorf("source %q has no song search", source)
		}
		if GetParseFunc(source) == nil {
			t.Errorf("source %q has no song parser", source)
		}
		if GetDownloadFunc(source) == nil {
			t.Errorf("source %q has no download resolver", source)
		}
		if GetLyricFunc(source) == nil {
			t.Errorf("source %q has no lyric provider", source)
		}
	}

	for _, source := range GetPlaylistSourceNames() {
		if GetPlaylistSearchFunc(source) == nil {
			t.Errorf("source %q has no playlist search", source)
		}
		if GetPlaylistDetailFunc(source) == nil {
			t.Errorf("source %q has no playlist detail", source)
		}
		if GetParsePlaylistFunc(source) == nil {
			t.Errorf("source %q has no playlist parser", source)
		}
	}

	for _, source := range GetAlbumSourceNames() {
		if GetAlbumSearchFunc(source) == nil {
			t.Errorf("source %q has no album search", source)
		}
		if GetAlbumDetailFunc(source) == nil {
			t.Errorf("source %q has no album detail", source)
		}
		if GetParseAlbumFunc(source) == nil {
			t.Errorf("source %q has no album parser", source)
		}
	}

	for _, source := range GetPlaylistCategorySourceNames() {
		if GetPlaylistCategoriesFunc(source) == nil {
			t.Errorf("source %q has no playlist categories", source)
		}
		if GetCategoryPlaylistsFunc(source) == nil {
			t.Errorf("source %q has no category playlists", source)
		}
	}

	for _, source := range GetRecommendSourceNames() {
		if GetRecommendFunc(source) == nil {
			t.Errorf("source %q has no recommended playlists", source)
		}
	}

	for _, source := range GetTopListSourceNames() {
		if GetTopListsFunc(source) == nil {
			t.Errorf("source %q has no top-list catalog", source)
		}
		if GetTopListSongsFunc(source) == nil {
			t.Errorf("source %q has no top-list songs", source)
		}
		if GetHotSongsFunc(source) == nil {
			t.Errorf("source %q has no hot songs", source)
		}
	}

	for _, source := range GetUserPlaylistSourceNames() {
		if GetUserPlaylistsFunc(source) == nil {
			t.Errorf("source %q has no user playlists", source)
		}
	}

	for _, source := range GetQRLoginSourceNames() {
		if GetQRLoginCreateFunc(source) == nil {
			t.Errorf("source %q has no QR login create function", source)
		}
		if GetQRLoginCheckFunc(source) == nil {
			t.Errorf("source %q has no QR login check function", source)
		}
	}
}

func TestDetectSourceSupportsNewProviders(t *testing.T) {
	tests := map[string]string{
		"https://music.apple.com/cn/album/example/123?i=456": "apple",
		"https://open.spotify.com/track/abc123":              "spotify",
		"spotify:track:abc123":                               "spotify",
	}
	for link, want := range tests {
		if got := DetectSource(link); got != want {
			t.Errorf("DetectSource(%q) = %q, want %q", link, got, want)
		}
	}
}

func TestDefaultSourcesIncludeNewPlayableProviders(t *testing.T) {
	sources := make(map[string]bool)
	for _, source := range GetDefaultSourceNames() {
		sources[source] = true
	}
	for _, source := range []string{"apple", "soda", "qianqian"} {
		if !sources[source] {
			t.Errorf("default sources do not include %q", source)
		}
	}
	if sources["spotify"] {
		t.Error("spotify should remain opt-in because it is not a direct playback source")
	}
}

// TestCookieSnapshotConsistencyUnderConcurrentUpdate exercises the hot-update
// vs. probe race: every snapshot taken while updates are in flight must report
// one internally consistent set of cookie values (all old or all new), never a
// mixture of the two.
func TestCookieSnapshotConsistencyUnderConcurrentUpdate(t *testing.T) {
	manager := &CookieManager{cookies: map[string]string{
		"netease": "old-netease",
		"qq":      "old-qq",
		"kugou":   "old-kugou",
	}}
	sources := []string{"netease", "qq", "kugou"}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Mutator: repeatedly flips every platform cookie as one update batch.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			cookies := make(map[string]string, len(sources))
			value := fmt.Sprintf("new-%d", i)
			for _, source := range sources {
				cookies[source] = value
			}
			manager.SetAll(cookies)
		}
	}()

	// Readers: each snapshot must be internally consistent — either the
	// untouched initial set ("old-<source>" everywhere) or one complete update
	// batch (the same "new-<i>" value everywhere). A mixture would mean a
	// probe could see some platforms with the new cookie and others with old.
	allOld := func(snapshot CookieSnapshot) bool {
		for _, source := range sources {
			if snapshot.Cookie(source) != "old-"+source {
				return false
			}
		}
		return true
	}
	allSameNew := func(snapshot CookieSnapshot) bool {
		batch := snapshot.Cookie(sources[0])
		if batch == "old-"+sources[0] || len(batch) < 5 || batch[:4] != "new-" {
			return false
		}
		for _, source := range sources[1:] {
			if snapshot.Cookie(source) != batch {
				return false
			}
		}
		return true
	}

	for i := 0; i < 2000; i++ {
		snapshot := manager.Snapshot()
		if !allOld(snapshot) && !allSameNew(snapshot) {
			close(stop)
			wg.Wait()
			t.Fatalf("mixed snapshot detected: %s=%q, %s=%q, %s=%q",
				sources[0], snapshot.Cookie(sources[0]),
				sources[1], snapshot.Cookie(sources[1]),
				sources[2], snapshot.Cookie(sources[2]))
		}
	}
	close(stop)
	wg.Wait()
}
