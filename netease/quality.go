package netease

import (
	"errors"
	"fmt"
	"strings"

	"github.com/guohuiyuan/music-lib/model"
)

const neteaseQualityProbeSongID = "496869422"
const neteaseQualityProbeKeyword = "周杰伦"

type neteaseQualityCandidate struct {
	option model.AudioQualityOption
	levels []string
}

var neteaseQualityCandidates = []neteaseQualityCandidate{
	{option: model.AudioQualityOption{Value: model.AudioQualityMaster, Label: "超清母带", Format: "wav", VIP: true}, levels: []string{"jymaster"}},
	{option: model.AudioQualityOption{Value: model.AudioQualitySpatial, Label: "沉浸环绕声", Format: "flac", VIP: true}, levels: []string{"sky", "jyeffect"}},
	{option: model.AudioQualityOption{Value: model.AudioQualityVivid, Label: "臻音全景声", Format: "mp3", VIP: true}, levels: []string{"vivid"}},
	{option: model.AudioQualityOption{Value: model.AudioQualityAtmos, Label: "杜比全景声", Format: "flac", VIP: true}, levels: []string{"dolby"}},
	{option: model.AudioQualityOption{Value: model.AudioQualityHiRes, Label: "Hi-Res 无损", Format: "flac", VIP: true}, levels: []string{"hires"}},
	{option: model.AudioQualityOption{Value: model.AudioQualityLossless, Label: "无损音质", Format: "flac", VIP: true}, levels: []string{"lossless"}},
	{option: model.AudioQualityOption{Value: model.AudioQualityHigh, Label: "高品质 320k", Format: "mp3", Bitrate: 320}, levels: []string{"exhigh"}},
	{option: model.AudioQualityOption{Value: model.AudioQualityHigher, Label: "较高品质 192k", Format: "mp3", Bitrate: 192}, levels: []string{"higher"}},
	{option: model.AudioQualityOption{Value: model.AudioQualityStandard, Label: "标准音质 128k", Format: "mp3", Bitrate: 128}, levels: []string{"standard"}},
}

// ListQualityOptions 返回网易云支持的全部音质档位。
func ListQualityOptions() []model.AudioQualityOption {
	options := make([]model.AudioQualityOption, 0, len(neteaseQualityCandidates))
	for _, candidate := range neteaseQualityCandidates {
		options = append(options, candidate.option)
	}
	return options
}

// AvailableQualityOptions 用当前账号探测网易云的播放接口。实际权益以返回的
// 音频流为准，而不是只看单独的 VIP 标记。
//
// 同一实例上的并发调用共享同一次探测：等待者拿到与发起者完全相同的档位列表，
// 不会出现一半调用看到空列表、另一半看到完整列表的撕裂结果。只有探测成功且
// 非空的结果才会被缓存，探测失败不会被永久固化，后续调用可以重新探测。
func (n *Netease) AvailableQualityOptions() ([]model.AudioQualityOption, error) {
	const maxAttempts = 4
	for attempt := 0; attempt < maxAttempts; attempt++ {
		accountKey := n.accountCacheKey()

		n.stateMu.Lock()
		if n.availableQualityCache != nil &&
			n.availableQualityCacheKey == accountKey &&
			neteaseNow().Before(n.availableQualityExpiresAt) {
			options := cloneQualityOptions(n.availableQualityCache)
			n.stateMu.Unlock()
			return options, nil
		}
		if call := n.qualityProbe; call != nil && call.accountKey == accountKey {
			n.stateMu.Unlock()
			// 等待正在进行的同账号探测，拿到完全一致的结果。
			<-call.done
			if n.accountCacheKey() != accountKey {
				continue // Cookie 在等待期间被切换，重新按新账号探测。
			}
			return cloneQualityOptions(call.options), call.err
		}
		call := &neteaseQualityProbeCall{
			accountKey: accountKey,
			done:       make(chan struct{}),
		}
		n.qualityProbe = call
		n.stateMu.Unlock()

		options, transient, err := n.probeAvailableQualityOptions()

		n.stateMu.Lock()
		call.options = options
		call.err = err
		if n.qualityProbe == call {
			n.qualityProbe = nil
		}
		// 只有成功且非空的结果才缓存，且必须仍是同一个账号。失败的探测保持
		// 不缓存，下一次调用会重新探测；探测中出现临时网络错误时也不缓存，
		// 避免把一次抖动当成账号权益固化下来。
		accountChanged := n.accountKeyLocked() != accountKey
		if err == nil && len(options) > 0 && !transient && !accountChanged {
			n.availableQualityCache = cloneQualityOptions(options)
			n.availableQualityCacheKey = accountKey
			n.availableQualityExpiresAt = neteaseNow().Add(availableQualityCacheTTL)
		}
		n.stateMu.Unlock()

		close(call.done)
		if accountChanged {
			// Cookie 在探测期间被切换：这批结果属于旧账号，不能返回给调用方，
			// 按新账号重新探测。
			continue
		}
		return cloneQualityOptions(options), err
	}
	return nil, fmt.Errorf("netease quality probe aborted: account cookie changed repeatedly")
}

func cloneQualityOptions(options []model.AudioQualityOption) []model.AudioQualityOption {
	if options == nil {
		return nil
	}
	return append([]model.AudioQualityOption(nil), options...)
}

// probeAvailableQualityOptions 依次探测固定歌曲和搜索到的歌曲。任何一档探测
// 成功即得到该账号的能力区间；全部失败才返回错误。transient 表示探测期间遇到
// 了网络/解析等临时错误，此时结果不应被缓存，以免把一次抖动当成账号权益。
func (n *Netease) probeAvailableQualityOptions() (options []model.AudioQualityOption, transient bool, err error) {
	fixedProbeSong := &model.Song{
		Source: "netease",
		ID:     neteaseQualityProbeSongID,
	}
	options, transient, firstErr := n.probeNeteaseQualityLadder([]*model.Song{fixedProbeSong})
	if len(options) > 0 {
		return options, transient, nil
	}

	probeSongs := make([]*model.Song, 0, 5)
	probeSongs = append(probeSongs, fixedProbeSong)
	seenSongIDs := map[string]struct{}{neteaseQualityProbeSongID: {}}
	if songs, err := n.Search(neteaseQualityProbeKeyword); err == nil {
		for i := range songs {
			if i >= 4 {
				break
			}
			if _, exists := seenSongIDs[songs[i].ID]; exists {
				continue
			}
			if songs[i].Source == "" {
				songs[i].Source = "netease"
			}
			seenSongIDs[songs[i].ID] = struct{}{}
			probeSongs = append(probeSongs, &songs[i])
		}
	}
	searchOptions, searchTransient, searchErr := n.probeNeteaseQualityLadder(probeSongs)
	transient = transient || searchTransient
	if len(searchOptions) > 0 {
		return searchOptions, transient, nil
	}
	if firstErr == nil {
		firstErr = searchErr
	}
	if firstErr != nil {
		return nil, transient, firstErr
	}
	return []model.AudioQualityOption{}, transient, nil
}

func (n *Netease) probeNeteaseQualityLadder(
	probeSongs []*model.Song,
) ([]model.AudioQualityOption, bool, error) {
	var firstErr error
	transient := false
	for candidateIndex, candidate := range neteaseQualityCandidates {
		for _, song := range probeSongs {
			_, err := n.GetDownloadURLWithQuality(song, string(candidate.option.Value))
			if err == nil {
				return neteaseQualityLadderFrom(candidateIndex), transient, nil
			}
			if isNeteaseTransientError(err) {
				transient = true
			}
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return nil, transient, firstErr
}

// neteaseQualityLadderFrom 返回探测成功的档位及其全部更低档位。账号权益是一个
// 能力区间：能拿到 320 kbps 就说明 192 kbps 和 128 kbps 也可用，即使某首歌的
// 低档位接口暂时拒绝探测。
func neteaseQualityLadderFrom(highestAvailableIndex int) []model.AudioQualityOption {
	if highestAvailableIndex < 0 || highestAvailableIndex >= len(neteaseQualityCandidates) {
		return nil
	}
	options := make([]model.AudioQualityOption, 0, len(neteaseQualityCandidates)-highestAvailableIndex)
	for _, candidate := range neteaseQualityCandidates[highestAvailableIndex:] {
		options = append(options, candidate.option)
	}
	return options
}

func neteaseCandidateForQuality(quality model.AudioQuality) (neteaseQualityCandidate, bool) {
	switch quality {
	case model.AudioQualityMaster:
		return neteaseQualityCandidates[0], true
	case model.AudioQualitySpatial:
		return neteaseQualityCandidates[1], true
	case model.AudioQualityVivid:
		return neteaseQualityCandidates[2], true
	case model.AudioQualityAtmos:
		return neteaseQualityCandidates[3], true
	case model.AudioQualityStandard:
		return neteaseQualityCandidates[8], true
	case model.AudioQualityHigher:
		return neteaseQualityCandidates[7], true
	case model.AudioQualityHigh:
		return neteaseQualityCandidates[6], true
	case model.AudioQualityLossless:
		return neteaseQualityCandidates[5], true
	case model.AudioQualityHiRes:
		return neteaseQualityCandidates[4], true
	default:
		return neteaseQualityCandidate{}, false
	}
}

func neteaseLevelsForQuality(quality string) ([]string, bool) {
	normalized, ok := model.NormalizeAudioQuality(quality)
	if !ok {
		return nil, false
	}
	candidate, ok := neteaseCandidateForQuality(normalized)
	if !ok {
		return nil, false
	}
	return append([]string(nil), candidate.levels...), true
}

func neteaseLevelForQuality(quality string) (string, bool) {
	levels, ok := neteaseLevelsForQuality(quality)
	if !ok || len(levels) == 0 {
		return "", false
	}
	return levels[0], true
}

func neteaseQualityForLevel(level string) (model.AudioQuality, bool) {
	level = strings.ToLower(strings.TrimSpace(level))
	for _, candidate := range neteaseQualityCandidates {
		for _, candidateLevel := range candidate.levels {
			if level == candidateLevel {
				return candidate.option.Value, true
			}
		}
	}
	return "", false
}

func neteaseAudioQuality(bitrate int, ext string, level string) model.AudioQuality {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "jymaster":
		return model.AudioQualityMaster
	case "sky", "jyeffect":
		return model.AudioQualitySpatial
	case "vivid":
		return model.AudioQualityVivid
	case "dolby":
		return model.AudioQualityAtmos
	case "hires":
		return model.AudioQualityHiRes
	case "lossless":
		return model.AudioQualityLossless
	case "exhigh":
		return model.AudioQualityHigh
	case "higher":
		return model.AudioQualityHigher
	case "standard":
		return model.AudioQualityStandard
	}

	ext = strings.ToLower(strings.TrimSpace(ext))
	switch {
	case ext == "wav" && bitrate >= 2_000_000:
		return model.AudioQualityMaster
	case (ext == "flac" || ext == "wav") && bitrate >= 1_000_000:
		return model.AudioQualityHiRes
	case ext == "flac" || ext == "wav" || bitrate >= 700_000:
		return model.AudioQualityLossless
	case bitrate >= 280_000:
		return model.AudioQualityHigh
	case bitrate >= 160_000:
		return model.AudioQualityHigher
	case bitrate >= 96_000:
		return model.AudioQualityStandard
	default:
		return model.AudioQualityLow
	}
}

func neteaseQualityAtLeast(actual model.AudioQuality, requested model.AudioQuality) bool {
	rank := func(quality model.AudioQuality) int {
		switch quality {
		case model.AudioQualityMaster:
			return 9
		case model.AudioQualitySpatial:
			return 8
		case model.AudioQualityVivid:
			return 7
		case model.AudioQualityAtmos:
			return 6
		case model.AudioQualityHiRes:
			return 5
		case model.AudioQualityLossless:
			return 4
		case model.AudioQualityHigh:
			return 3
		case model.AudioQualityHigher:
			return 2
		case model.AudioQualityStandard:
			return 1
		default:
			return 0
		}
	}
	return rank(actual) >= rank(requested) && rank(requested) > 0
}

func neteaseQualityError(quality string) error {
	return fmt.Errorf("%w: %s is unavailable for this song or account", errNeteaseQualityUnavailable, quality)
}

var errNeteaseQualityUnavailable = errors.New("netease audio quality unavailable")

// isNeteaseTransientError 区分“临时拿不到”和“账号/歌曲本来就没有该档位”：
// 空地址、降级和明确不可用属于权益结论；网络、编解码、HTTP 状态等属于临时
// 故障，不应被当成探测结论缓存。
func isNeteaseTransientError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errNeteaseQualityUnavailable) {
		return false
	}
	message := strings.ToLower(err.Error())
	permanentMarkers := []string{
		"unavailable for this song or account",
		"eapi download url not found",
		"download url not found",
		"netease returned",
	}
	for _, marker := range permanentMarkers {
		if strings.Contains(message, marker) {
			return false
		}
	}
	return true
}
