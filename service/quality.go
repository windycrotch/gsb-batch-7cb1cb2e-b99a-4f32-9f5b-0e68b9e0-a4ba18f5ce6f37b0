package service

import (
	"fmt"
	"strings"

	"github.com/guohuiyuan/music-lib/apple"
	"github.com/guohuiyuan/music-lib/bilibili"
	"github.com/guohuiyuan/music-lib/fivesing"
	"github.com/guohuiyuan/music-lib/jamendo"
	"github.com/guohuiyuan/music-lib/joox"
	"github.com/guohuiyuan/music-lib/kugou"
	"github.com/guohuiyuan/music-lib/kuwo"
	"github.com/guohuiyuan/music-lib/migu"
	"github.com/guohuiyuan/music-lib/model"
	"github.com/guohuiyuan/music-lib/netease"
	"github.com/guohuiyuan/music-lib/provider"
	"github.com/guohuiyuan/music-lib/qianqian"
	"github.com/guohuiyuan/music-lib/qq"
	"github.com/guohuiyuan/music-lib/soda"
	"github.com/guohuiyuan/music-lib/spotify"
)

type ResolvedAudioQuality struct {
	URL     string `json:"url"`
	Quality string `json:"quality"`
	// Cookie is the snapshot value used while resolving the URL. It is not
	// serialized but lets the streaming handler replay the exact same cookie
	// when fetching the upstream stream.
	Cookie string `json:"-"`
}

// ResolvedSodaAudio carries a soda download resolution together with the
// quality tier and cookie snapshot that produced it.
type ResolvedSodaAudio struct {
	Info    *soda.DownloadInfo
	Quality string
	Cookie  string
}

var audioQualitySourceOrder = []string{
	"netease",
	"qq",
	"kugou",
	"kuwo",
	"migu",
	"soda",
	"apple",
	"spotify",
	"qianqian",
	"joox",
	"bilibili",
	"fivesing",
	"jamendo",
}

var audioQualityChoices = map[string]func() []model.AudioQualityOption{
	"netease":  netease.ListQualityOptions,
	"qq":       qq.ListQualityOptions,
	"kugou":    kugou.ListQualityOptions,
	"kuwo":     kuwo.ListQualityOptions,
	"migu":     migu.ListQualityOptions,
	"soda":     soda.ListQualityOptions,
	"apple":    apple.ListQualityOptions,
	"spotify":  spotify.ListQualityOptions,
	"qianqian": qianqian.ListQualityOptions,
	"joox":     joox.ListQualityOptions,
	"bilibili": bilibili.ListQualityOptions,
	"fivesing": fivesing.ListQualityOptions,
	"jamendo":  jamendo.ListQualityOptions,
}

// canonicalAudioQualityRank orders every normalized, non-auto quality on a
// single high-to-low scale. It is only used to locate the entry tier when the
// requested quality is absent from a platform's table (e.g. a request above
// the platform maximum): the platform's own table remains the authoritative
// downgrade ladder, since special tiers such as spatial/atmos occupy different
// positions on different platforms.
var canonicalAudioQualityRank = map[model.AudioQuality]int{
	model.AudioQualityPreview:  1,
	model.AudioQualityLow:      2,
	model.AudioQualityStandard: 3,
	model.AudioQualityHigher:   4,
	model.AudioQualityHigh:     5,
	model.AudioQualityUltra:    6,
	model.AudioQualityLossless: 7,
	model.AudioQualityAtmos:    8,
	model.AudioQualityAtmos51:  9,
	model.AudioQualityVivid:    10,
	model.AudioQualitySpatial:  11,
	model.AudioQualityHiRes:    12,
	model.AudioQualityMaster:   13,
}

func GetAudioQualitySourceNames() []string {
	return append([]string(nil), audioQualitySourceOrder...)
}

func GetAudioQualityChoices(source string) ([]model.AudioQualityOption, error) {
	source = strings.ToLower(strings.TrimSpace(source))
	factory, ok := audioQualityChoices[source]
	if !ok {
		return nil, fmt.Errorf("unsupported audio quality source: %s", source)
	}
	return append([]model.AudioQualityOption(nil), factory()...), nil
}

func GetAllAudioQualityChoices() map[string][]model.AudioQualityOption {
	result := make(map[string][]model.AudioQualityOption, len(audioQualitySourceOrder))
	for _, source := range audioQualitySourceOrder {
		options, _ := GetAudioQualityChoices(source)
		result[source] = options
	}
	return result
}

// GetAudioQualityOptions probes the account entitlements using a fresh cookie
// snapshot taken at call time.
func GetAudioQualityOptions(source string) ([]model.AudioQualityOption, error) {
	return GetAudioQualityOptionsWithSnapshot(CM.Snapshot(), source)
}

// GetAudioQualityOptionsWithSnapshot probes account entitlements against a
// fixed cookie snapshot so a concurrent hot update cannot change the cookie
// mid-probe.
func GetAudioQualityOptionsWithSnapshot(snapshot CookieSnapshot, source string) ([]model.AudioQualityOption, error) {
	qualityProvider := audioQualityProviderForSourceWithSnapshot(source, snapshot)
	if qualityProvider == nil {
		return nil, fmt.Errorf("unsupported audio quality source: %s", strings.TrimSpace(source))
	}
	return qualityProvider.AvailableQualityOptions()
}

func GetDownloadFuncWithQuality(source, requestedQuality string) func(*model.Song) (string, error) {
	qualityProvider := audioQualityProviderForSource(source)
	if qualityProvider == nil {
		return nil
	}
	return func(song *model.Song) (string, error) {
		resolved, err := resolveDownloadURL(qualityProvider, song, requestedQuality)
		if err != nil {
			return "", err
		}
		return resolved.URL, nil
	}
}

// ResolveDownloadURL resolves a playable URL using a cookie snapshot captured
// for this invocation, so the whole downgrade ladder shares one cookie value.
func ResolveDownloadURL(source string, song *model.Song, requestedQuality string) (ResolvedAudioQuality, error) {
	return ResolveDownloadURLWithSnapshot(CM.Snapshot(), source, song, requestedQuality)
}

// ResolveDownloadURLWithSnapshot resolves against an explicit cookie snapshot.
func ResolveDownloadURLWithSnapshot(
	snapshot CookieSnapshot,
	source string,
	song *model.Song,
	requestedQuality string,
) (ResolvedAudioQuality, error) {
	qualityProvider := audioQualityProviderForSourceWithSnapshot(source, snapshot)
	if qualityProvider == nil {
		return ResolvedAudioQuality{}, fmt.Errorf("unsupported audio quality source: %s", strings.TrimSpace(source))
	}
	resolved, err := resolveDownloadURL(qualityProvider, song, requestedQuality)
	if err != nil {
		return ResolvedAudioQuality{}, err
	}
	resolved.Cookie = snapshot.Cookie(source)
	return resolved, nil
}

// ResolveSodaDownloadInfo resolves soda stream info using a cookie snapshot
// captured for this invocation.
func ResolveSodaDownloadInfo(song *model.Song, requestedQuality string) (ResolvedSodaAudio, error) {
	return ResolveSodaDownloadInfoWithSnapshot(CM.Snapshot(), song, requestedQuality)
}

// ResolveSodaDownloadInfoWithSnapshot resolves soda stream info against an
// explicit cookie snapshot.
func ResolveSodaDownloadInfoWithSnapshot(
	snapshot CookieSnapshot,
	song *model.Song,
	requestedQuality string,
) (ResolvedSodaAudio, error) {
	sodaProvider := soda.New(snapshot.Cookie("soda"))
	order, err := audioQualityFallbackOrder(song, requestedQuality)
	if err != nil {
		return ResolvedSodaAudio{}, err
	}

	var lastErr error
	for _, quality := range order {
		info, resolveErr := sodaProvider.GetDownloadInfoWithQuality(song, string(quality))
		if resolveErr == nil && info != nil && strings.TrimSpace(info.URL) != "" {
			return ResolvedSodaAudio{
				Info:    info,
				Quality: string(quality),
				Cookie:  snapshot.Cookie("soda"),
			}, nil
		}
		if resolveErr != nil {
			lastErr = resolveErr
		} else {
			lastErr = fmt.Errorf("soda returned an empty download URL for quality %s", quality)
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no playable soda quality was found")
	}
	return ResolvedSodaAudio{}, fmt.Errorf("audio unavailable from quality %s downward: %w", order[0], lastErr)
}

func audioQualityProviderForSource(source string) provider.AudioQualityProvider {
	source = strings.ToLower(strings.TrimSpace(source))
	candidate := sourceProvider(source, CM.Get(source))
	if candidate == nil {
		return nil
	}
	qualityProvider, ok := candidate.(provider.AudioQualityProvider)
	if !ok {
		return nil
	}
	return qualityProvider
}

func audioQualityProviderForSourceWithSnapshot(source string, snapshot CookieSnapshot) provider.AudioQualityProvider {
	source = strings.ToLower(strings.TrimSpace(source))
	candidate := sourceProviderFromSnapshot(source, snapshot)
	if candidate == nil {
		return nil
	}
	qualityProvider, ok := candidate.(provider.AudioQualityProvider)
	if !ok {
		return nil
	}
	return qualityProvider
}

func resolveDownloadURL(
	qualityProvider provider.AudioQualityProvider,
	song *model.Song,
	requestedQuality string,
) (ResolvedAudioQuality, error) {
	order, err := audioQualityFallbackOrder(song, requestedQuality)
	if err != nil {
		return ResolvedAudioQuality{}, err
	}

	var lastErr error
	for _, quality := range order {
		downloadURL, resolveErr := qualityProvider.GetDownloadURLWithQuality(song, string(quality))
		if resolveErr == nil && strings.TrimSpace(downloadURL) != "" {
			return ResolvedAudioQuality{
				URL:     downloadURL,
				Quality: string(quality),
			}, nil
		}
		if resolveErr != nil {
			lastErr = resolveErr
		} else {
			lastErr = fmt.Errorf("source returned an empty download URL for quality %s", quality)
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no playable quality was found")
	}
	return ResolvedAudioQuality{}, fmt.Errorf(
		"audio unavailable from quality %s downward: %w",
		order[0],
		lastErr,
	)
}

func audioQualityFallbackOrder(song *model.Song, requestedQuality string) ([]model.AudioQuality, error) {
	if song == nil {
		return nil, fmt.Errorf("song is required")
	}

	quality := resolveSongAudioQuality(song, requestedQuality)
	normalized, ok := model.NormalizeAudioQuality(quality)
	if !ok {
		return nil, fmt.Errorf("unsupported audio quality: %s", quality)
	}
	if normalized == model.AudioQualityAuto {
		return []model.AudioQuality{model.AudioQualityAuto}, nil
	}

	choices, err := GetAudioQualityChoices(song.Source)
	if err != nil {
		return nil, err
	}
	if len(choices) == 0 {
		return nil, fmt.Errorf("no audio quality tiers are configured for source %s", song.Source)
	}

	requestedRank, known := canonicalAudioQualityRank[normalized]
	if !known {
		return nil, fmt.Errorf("unsupported audio quality: %s", normalized)
	}

	// Platform tables are ordered high-to-low, but special tiers such as
	// spatial/atmos may sit at different positions on different platforms.
	// A tier that exists in the table is therefore entered directly, keeping
	// each platform's own ladder authoritative.
	for index, option := range choices {
		if option.Value == normalized {
			return qualityOrderFrom(choices, index), nil
		}
	}

	// The requested tier is absent from the platform table. Enter at the
	// highest table tier whose canonical rank does not exceed the request:
	// when the request is above the platform maximum this is the platform's
	// best tier; when it is below the platform minimum every tier ranks higher
	// and the request cannot be served by walking downward.
	startIndex := -1
	for index, option := range choices {
		tierRank, ok := canonicalAudioQualityRank[option.Value]
		if !ok {
			continue
		}
		if tierRank <= requestedRank {
			startIndex = index
			break
		}
	}
	if startIndex < 0 {
		return nil, fmt.Errorf(
			"audio quality %s cannot be served by source %s: no tier at or below it exists in the quality table",
			normalized,
			song.Source,
		)
	}
	return qualityOrderFrom(choices, startIndex), nil
}

func qualityOrderFrom(choices []model.AudioQualityOption, startIndex int) []model.AudioQuality {
	order := make([]model.AudioQuality, 0, len(choices)-startIndex)
	for _, fallback := range choices[startIndex:] {
		order = append(order, fallback.Value)
	}
	return order
}

func resolveSongAudioQuality(song *model.Song, requestedQuality string) string {
	if song != nil && song.Extra != nil {
		if override := strings.TrimSpace(song.Extra["quality"]); override != "" {
			return override
		}
	}
	if requested := strings.TrimSpace(requestedQuality); requested != "" {
		return requested
	}
	return string(model.AudioQualityAuto)
}
