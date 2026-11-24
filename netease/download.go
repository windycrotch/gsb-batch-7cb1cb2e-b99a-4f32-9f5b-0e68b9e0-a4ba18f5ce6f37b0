package netease

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/guohuiyuan/music-lib/model"
	"github.com/guohuiyuan/music-lib/utils"
)

func GetDownloadURL(s *model.Song) (string, error) { return defaultNetease.GetDownloadURL(s) }

// GetDownloadURL 返回下载地址。
func (n *Netease) GetDownloadURL(s *model.Song) (string, error) {
	return n.getDownloadURL(s, preferredDownloadLevels(s), true)
}

// GetDownloadURLWithQuality 解析指定的音质档位。选择器不会静默降级会员档位，
// 只有 auto 才会选择更低档位。
func (n *Netease) GetDownloadURLWithQuality(s *model.Song, quality string) (string, error) {
	normalized, ok := model.NormalizeAudioQuality(quality)
	if !ok {
		return "", fmt.Errorf("unsupported netease audio quality: %s", quality)
	}
	if normalized == model.AudioQualityAuto {
		levels := preferredDownloadLevels(s)
		return n.getDownloadURL(s, levels, true)
	}
	levels, ok := neteaseLevelsForQuality(string(normalized))
	if !ok {
		return "", fmt.Errorf("unsupported netease audio quality: %s", quality)
	}
	return n.getDownloadURL(s, levels, false)
}

func (n *Netease) getDownloadURL(s *model.Song, levels []string, allowFallback bool) (string, error) {
	if s == nil {
		return "", errors.New("song is nil")
	}
	if s.Source != "netease" {
		return "", errors.New("source mismatch")
	}

	songID := s.ID
	if s.Extra != nil && s.Extra["song_id"] != "" {
		songID = s.Extra["song_id"]
	}

	// 整个请求绑定同一账号快照：请求期间即使发生登录/切换，也不会把 A 账号
	// 拿到的地址写到 B 账号的缓存里。账号键从同一个快照推导，避免两次读取之间
	// 被切换导致键与请求所用 Cookie 不一致。
	cookie := n.cookieSnapshot()
	accountKey := utils.MD5(cookie)
	levelKey := strings.Join(levels, ",")

	if cached, ok := n.getCachedDownloadURL(accountKey, songID, levelKey); ok {
		if cached.ext != "" {
			s.Ext = cached.ext
		}
		return cached.url, nil
	}

	for _, level := range levels {
		url, ext, bitrate, actualLevel, err := n.getEAPIDownloadURL(songID, level, cookie)
		if err == nil && url != "" {
			requestedLevel, ok := neteaseQualityForLevel(level)
			if !ok {
				continue
			}
			actualLevelQuality := neteaseAudioQuality(bitrate, ext, actualLevel)
			if !neteaseQualityAtLeast(actualLevelQuality, requestedLevel) {
				continue
			}
			s.Ext = ext
			n.setCachedDownloadURL(accountKey, songID, levelKey, url, s.Ext)
			return url, nil
		}
	}
	if !allowFallback {
		return "", neteaseQualityError(levelKey)
	}

	br := 320000
	expectedBr := 0
	if len(levels) == 1 {
		switch levels[0] {
		case "standard":
			br = 128000
			expectedBr = br
		case "higher":
			br = 192000
			expectedBr = br
		case "exhigh":
			br = 320000
			expectedBr = br
		default:
			if !allowFallback {
				return "", neteaseQualityError(levels[0])
			}
		}
	}

	// 回退到原来的 weapi 接口。
	reqData := map[string]interface{}{
		"ids": []string{songID},
		"br":  br,
	}
	reqJSON, _ := json.Marshal(reqData)
	params, encSecKey := EncryptWeApi(string(reqJSON))
	form := url.Values{}
	form.Set("params", params)
	form.Set("encSecKey", encSecKey)

	weapiEndpoint := neteaseDownloadWeAPIEndpoint
	headers := []utils.RequestOption{
		utils.WithHeader("Referer", Referer),
		utils.WithHeader("Content-Type", "application/x-www-form-urlencoded"),
		utils.WithHeader("Cookie", cookie),
		utils.WithRandomIPHeader(),
	}

	body, err := utils.Post(weapiEndpoint, strings.NewReader(form.Encode()), headers...)
	if err != nil {
		return "", err
	}

	var resp struct {
		Data []struct {
			URL  string `json:"url"`
			Code int    `json:"code"`
			Br   int    `json:"br"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("json parse error: %w", err)
	}
	if len(resp.Data) == 0 || resp.Data[0].URL == "" {
		return "", errors.New("download url not found (might be vip or copyright restricted)")
	}
	if expectedBr > 0 && resp.Data[0].Br > 0 && resp.Data[0].Br != expectedBr {
		return "", fmt.Errorf(
			"netease returned %d bps instead of requested %d bps",
			resp.Data[0].Br,
			expectedBr,
		)
	}
	n.setCachedDownloadURL(accountKey, songID, levelKey, resp.Data[0].URL, s.Ext)
	return resp.Data[0].URL, nil
}
