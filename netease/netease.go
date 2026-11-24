package netease

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/guohuiyuan/music-lib/model"
	"github.com/guohuiyuan/music-lib/utils"
)

const (
	Referer                = "http://music.163.com/"
	SearchAPI              = "http://music.163.com/api/linux/forward"
	DownloadAPI            = "http://music.163.com/weapi/song/enhance/player/url"
	DownloadEAPI           = "https://interface3.music.163.com/eapi/song/enhance/player/url/v1"
	DetailAPI              = "https://music.163.com/weapi/v3/song/detail"
	PlaylistAPI            = "https://music.163.com/weapi/v3/playlist/detail"
	PlaylistCategoryAPI    = "https://music.163.com/weapi/playlist/catalogue"
	CategoryPlaylistAPI    = "https://music.163.com/weapi/playlist/list"
	UserPlaylistAPI        = "https://music.163.com/weapi/user/playlist"
	AlbumAPI               = "https://music.163.com/weapi/v1/album/%s"
	UserAccountAPI         = "https://music.163.com/weapi/nuser/account/get"
	RecommendedPlaylistAPI = "https://music.163.com/weapi/personalized/playlist"
)

// Netease 是一个账号绑定实例：探测出的音质权益、VIP 状态和下载地址缓存都只
// 对创建/登录时的 Cookie 有效。并发调用会共享同一次探测，不会看到半成品结果。
type Netease struct {
	cookie   string
	cookieMu sync.RWMutex

	// stateMu 保护账号级缓存。网络请求期间不会持有该锁。
	stateMu                   sync.Mutex
	isVipCache                *bool
	isVipCacheKey             string
	availableQualityCache     []model.AudioQualityOption
	availableQualityCacheKey  string
	availableQualityExpiresAt time.Time
	qualityProbe              *neteaseQualityProbeCall
}

// neteaseQualityProbeCall 表示一次进行中的音质探测。并发调用者等待同一个
// done 通道，从而拿到完全一致的结果，而不是各自探测出不同的档位集合。
type neteaseQualityProbeCall struct {
	accountKey string
	done       chan struct{}
	options    []model.AudioQualityOption
	err        error
}

type neteaseLinkKind string

const (
	neteaseLinkUnknown  neteaseLinkKind = ""
	neteaseLinkSong     neteaseLinkKind = "song"
	neteaseLinkAlbum    neteaseLinkKind = "album"
	neteaseLinkPlaylist neteaseLinkKind = "playlist"
)

var (
	errInvalidNeteaseLink      = errors.New("invalid netease link")
	errNeteasePlaylistLink     = errors.New("netease playlist link detected, use ParsePlaylist")
	errNeteaseAlbumLink        = errors.New("netease album link detected, use ParseAlbum")
	errNeteaseInvalidAlbumLink = errors.New("invalid netease album link")
	errNeteaseInvalidListLink  = errors.New("invalid netease playlist link")
	errNeteaseSongNotFound     = errors.New("netease song not found")
)

type cachedDownloadURL struct {
	url       string
	ext       string
	expiresAt time.Time
}

type cachedVIPStatus struct {
	isVip     bool
	expiresAt time.Time
}

var downloadURLCache = struct {
	sync.Mutex
	items map[string]cachedDownloadURL
}{items: make(map[string]cachedDownloadURL)}

var vipStatusCache = struct {
	sync.Mutex
	items map[string]cachedVIPStatus
}{items: make(map[string]cachedVIPStatus)}

// 这些 TTL 用变量而不是常量，测试可以缩短它们来验证过期后的重新探测/重取。
var (
	downloadURLCacheTTL      = 10 * time.Minute
	vipStatusCacheTTL        = 10 * time.Minute
	availableQualityCacheTTL = 10 * time.Minute
	// neteaseNow 让缓存过期逻辑可测试。
	neteaseNow = time.Now
)

// 探测与下载都通过这几个入口发请求。保留常量给调用方引用，同时用变量承接，
// 测试可以指向本地 httptest 服务器而不触碰真实网易云接口。
var (
	neteaseSearchEndpoint        = SearchAPI
	neteaseDownloadWeAPIEndpoint = DownloadAPI
	neteaseDownloadEAPIEndpoint  = DownloadEAPI
	neteaseUserAccountEndpoint   = UserAccountAPI
)

// New 创建网易云客户端。Cookie 会保留调用方的登录字段，并在缺失时补上高档
// 音质需要的 PC 客户端标识；调用方传入的 os/appver 不会被覆盖。
func New(cookie string) *Netease { return &Netease{cookie: withClientIdentity(cookie)} }

var defaultNetease = New("")

// cookieSnapshot 返回当前 Cookie 的一致快照，避免读取与登录写入并发竞争。
func (n *Netease) cookieSnapshot() string {
	n.cookieMu.RLock()
	defer n.cookieMu.RUnlock()
	return n.cookie
}

// accountCacheKey 是账号级缓存的隔离键。不同 Cookie（不同账号）绝不会共享
// 探测结果或下载地址。
func (n *Netease) accountCacheKey() string {
	return utils.MD5(n.cookieSnapshot())
}

// setCookie 更新实例 Cookie，并让所有账号级缓存失效。QR 登录成功或上层切换
// 账号时必须走这里，否则新账号会继续命中旧账号的探测结果和地址。
func (n *Netease) setCookie(cookie string) {
	identity := withClientIdentity(cookie)

	oldAccountKey := n.accountCacheKey()
	n.cookieMu.Lock()
	n.cookie = identity
	n.cookieMu.Unlock()

	n.stateMu.Lock()
	n.resetAccountStateLocked()
	n.stateMu.Unlock()

	// 账号键已变，旧账号的地址不会再被命中；顺手清掉，避免长期占用内存。
	purgeAccountDownloadURLCache(oldAccountKey)
}

// resetAccountStateLocked 清空账号级缓存。调用方必须持有 stateMu。
func (n *Netease) resetAccountStateLocked() {
	n.isVipCache = nil
	n.isVipCacheKey = ""
	n.availableQualityCache = nil
	n.availableQualityCacheKey = ""
	n.availableQualityExpiresAt = time.Time{}
	n.qualityProbe = nil
}

func (n *Netease) getCachedVIPStatus(accountKey string) (bool, bool) {
	now := neteaseNow()

	vipStatusCache.Lock()
	defer vipStatusCache.Unlock()

	item, ok := vipStatusCache.items[accountKey]
	if !ok {
		return false, false
	}
	if now.After(item.expiresAt) {
		delete(vipStatusCache.items, accountKey)
		return false, false
	}
	return item.isVip, true
}

func (n *Netease) setCachedVIPStatus(accountKey string, isVip bool) {
	vipStatusCache.Lock()
	defer vipStatusCache.Unlock()
	vipStatusCache.items[accountKey] = cachedVIPStatus{
		isVip:     isVip,
		expiresAt: neteaseNow().Add(vipStatusCacheTTL),
	}
}

// cloudSearch 调用共享的网易云搜索接口。
func (n *Netease) cloudSearch(keyword string, searchType int, limit int) ([]byte, error) {
	eparams := map[string]interface{}{
		"method": "POST",
		"url":    "http://music.163.com/api/cloudsearch/pc",
		"params": map[string]interface{}{
			"s":      keyword,
			"type":   searchType,
			"offset": 0,
			"limit":  limit,
		},
	}
	eparamsJSON, _ := json.Marshal(eparams)
	encryptedParam := EncryptLinux(string(eparamsJSON))
	form := url.Values{}
	form.Set("eparams", encryptedParam)

	headers := []utils.RequestOption{
		utils.WithHeader("Referer", Referer),
		utils.WithHeader("Content-Type", "application/x-www-form-urlencoded"),
		utils.WithHeader("Cookie", n.cookieSnapshot()),
		utils.WithRandomIPHeader(),
	}

	return utils.Post(neteaseSearchEndpoint, strings.NewReader(form.Encode()), headers...)
}

// joinArtistNames 拼接用于展示的歌手名。
func joinArtistNames(names []string) string {
	return strings.Join(names, ", ")
}

// fetchAlbumDetail 返回专辑信息和歌曲。
func (n *Netease) fetchAlbumDetail(albumID string) (*model.Playlist, []model.Song, error) {
	reqData := map[string]interface{}{
		"csrf_token": "",
	}
	reqJSON, _ := json.Marshal(reqData)
	params, encSecKey := EncryptWeApi(string(reqJSON))
	form := url.Values{}
	form.Set("params", params)
	form.Set("encSecKey", encSecKey)

	headers := []utils.RequestOption{
		utils.WithHeader("Referer", Referer),
		utils.WithHeader("Content-Type", "application/x-www-form-urlencoded"),
		utils.WithHeader("Cookie", n.cookieSnapshot()),
		utils.WithRandomIPHeader(),
	}

	body, err := utils.Post(fmt.Sprintf(AlbumAPI, albumID), strings.NewReader(form.Encode()), headers...)
	if err != nil {
		return nil, nil, err
	}

	var resp struct {
		Code  int `json:"code"`
		Album struct {
			ID          int64  `json:"id"`
			Name        string `json:"name"`
			PicURL      string `json:"picUrl"`
			Size        int    `json:"size"`
			Company     string `json:"company"`
			Description string `json:"description"`
			BriefDesc   string `json:"briefDesc"`
			PublishTime int64  `json:"publishTime"`
			Artist      struct {
				Name string `json:"name"`
			} `json:"artist"`
			Artists []struct {
				Name string `json:"name"`
			} `json:"artists"`
		} `json:"album"`
		Songs []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
			Ar   []struct {
				Name string `json:"name"`
			} `json:"ar"`
			Al struct {
				ID     int64  `json:"id"`
				Name   string `json:"name"`
				PicURL string `json:"picUrl"`
			} `json:"al"`
			Dt        int `json:"dt"`
			Privilege struct {
				Fl int `json:"fl"`
			} `json:"privilege"`
			H struct {
				Size int64 `json:"size"`
			} `json:"h"`
			M struct {
				Size int64 `json:"size"`
			} `json:"m"`
			L struct {
				Size int64 `json:"size"`
			} `json:"l"`
		} `json:"songs"`
	}

	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, nil, fmt.Errorf("netease album detail json parse error: %w", err)
	}
	if resp.Code != 200 {
		return nil, nil, fmt.Errorf("netease api error code: %d", resp.Code)
	}

	artistName := resp.Album.Artist.Name
	if artistName == "" && len(resp.Album.Artists) > 0 {
		names := make([]string, 0, len(resp.Album.Artists))
		for _, artist := range resp.Album.Artists {
			if artist.Name != "" {
				names = append(names, artist.Name)
			}
		}
		artistName = joinArtistNames(names)
	}

	description := resp.Album.Description
	if description == "" {
		description = resp.Album.BriefDesc
	}

	album := &model.Playlist{
		Source:      "netease",
		ID:          strconv.FormatInt(resp.Album.ID, 10),
		Name:        resp.Album.Name,
		Cover:       resp.Album.PicURL,
		TrackCount:  resp.Album.Size,
		Creator:     artistName,
		Description: description,
		Link:        fmt.Sprintf("https://music.163.com/#/album?id=%d", resp.Album.ID),
		Extra: map[string]string{
			"type":         "album",
			"company":      resp.Album.Company,
			"publish_time": strconv.FormatInt(resp.Album.PublishTime, 10),
		},
	}

	songs := make([]model.Song, 0, len(resp.Songs))
	for _, item := range resp.Songs {
		artistNames := make([]string, 0, len(item.Ar))
		for _, artist := range item.Ar {
			if artist.Name != "" {
				artistNames = append(artistNames, artist.Name)
			}
		}

		var size int64
		if item.Privilege.Fl >= 320000 && item.H.Size > 0 {
			size = item.H.Size
		} else if item.Privilege.Fl >= 192000 && item.M.Size > 0 {
			size = item.M.Size
		} else {
			size = item.L.Size
		}

		duration := item.Dt / 1000
		bitrate := 128
		if duration > 0 && size > 0 {
			bitrate = int(size * 8 / 1000 / int64(duration))
		}

		songs = append(songs, model.Song{
			Source:   "netease",
			ID:       strconv.FormatInt(item.ID, 10),
			Name:     item.Name,
			Artist:   joinArtistNames(artistNames),
			Album:    item.Al.Name,
			AlbumID:  strconv.FormatInt(item.Al.ID, 10),
			Duration: duration,
			Size:     size,
			Bitrate:  bitrate,
			Cover:    item.Al.PicURL,
			Link:     fmt.Sprintf("https://music.163.com/#/song?id=%d", item.ID),
			Extra: map[string]string{
				"song_id":  strconv.FormatInt(item.ID, 10),
				"album_id": strconv.FormatInt(item.Al.ID, 10),
			},
		})
	}

	return album, songs, nil
}

func (n *Netease) fetchPlaylistDetail(playlistID string) (*model.Playlist, []model.Song, error) {
	return n.fetchPlaylistDetailLimited(playlistID, 0)
}

// fetchPlaylistDetailLimited 获取歌单信息和歌曲。
// maxSongs 限制抓取的歌曲数量（0 表示不限制）。
func (n *Netease) fetchPlaylistDetailLimited(playlistID string, maxSongs int) (*model.Playlist, []model.Song, error) {
	reqData := map[string]interface{}{
		"id":         playlistID,
		"n":          0, // 0表示不直接返回详情，我们只需要ID列表
		"csrf_token": "",
	}
	reqJSON, _ := json.Marshal(reqData)
	params, encSecKey := EncryptWeApi(string(reqJSON))
	form := url.Values{}
	form.Set("params", params)
	form.Set("encSecKey", encSecKey)

	headers := []utils.RequestOption{
		utils.WithHeader("Referer", Referer),
		utils.WithHeader("Content-Type", "application/x-www-form-urlencoded"),
		utils.WithHeader("Cookie", n.cookieSnapshot()),
		utils.WithRandomIPHeader(),
	}

	body, err := utils.Post(PlaylistAPI, strings.NewReader(form.Encode()), headers...)
	if err != nil {
		return nil, nil, err
	}

	var resp struct {
		Code     int `json:"code"`
		Playlist struct {
			ID          int64  `json:"id"`
			Name        string `json:"name"`
			CoverImgURL string `json:"coverImgUrl"`
			Description string `json:"description"`
			PlayCount   int    `json:"playCount"`
			TrackCount  int    `json:"trackCount"`
			Creator     struct {
				Nickname string `json:"nickname"`
			} `json:"creator"`
			// 使用 trackIds，方便后续单独拉取完整列表。
			TrackIds []struct {
				ID int64 `json:"id"`
			} `json:"trackIds"`
		} `json:"playlist"`
	}

	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, nil, fmt.Errorf("netease playlist detail json parse error: %w", err)
	}
	if resp.Code != 200 {
		return nil, nil, fmt.Errorf("netease api error code: %d", resp.Code)
	}

	// 构建歌单信息。
	playlist := &model.Playlist{
		Source:      "netease",
		ID:          strconv.FormatInt(resp.Playlist.ID, 10),
		Name:        resp.Playlist.Name,
		Cover:       resp.Playlist.CoverImgURL,
		TrackCount:  resp.Playlist.TrackCount,
		PlayCount:   resp.Playlist.PlayCount,
		Creator:     resp.Playlist.Creator.Nickname,
		Description: resp.Playlist.Description,
		Link:        fmt.Sprintf("https://music.163.com/#/playlist?id=%d", resp.Playlist.ID),
	}

	// 先收集所有歌曲 ID。
	var allIDs []string
	for _, tid := range resp.Playlist.TrackIds {
		allIDs = append(allIDs, strconv.FormatInt(tid.ID, 10))
	}
	if maxSongs > 0 && len(allIDs) > maxSongs {
		allIDs = allIDs[:maxSongs]
	}

	// 分批获取歌曲详情。
	var allSongs []model.Song
	batchSize := 500
	for i := 0; i < len(allIDs); i += batchSize {
		end := i + batchSize
		if end > len(allIDs) {
			end = len(allIDs)
		}

		batchIDs := allIDs[i:end]
		batchSongs, err := n.fetchSongsBatch(batchIDs)
		if err == nil {
			allSongs = append(allSongs, batchSongs...)
		}
	}

	return playlist, allSongs, nil
}

// fetchSongsBatch 分批获取歌曲详情。
func (n *Netease) fetchSongsBatch(songIDs []string) ([]model.Song, error) {
	if len(songIDs) == 0 {
		return nil, nil
	}

	// 构造 c 参数：[{"id":123},{"id":456},...]
	var cList []map[string]interface{}
	for _, id := range songIDs {
		cList = append(cList, map[string]interface{}{"id": id})
	}
	cJSON, _ := json.Marshal(cList)
	idsJSON, _ := json.Marshal(songIDs)

	reqData := map[string]interface{}{
		"c":   string(cJSON),
		"ids": string(idsJSON),
	}
	reqJSON, _ := json.Marshal(reqData)
	params, encSecKey := EncryptWeApi(string(reqJSON))

	form := url.Values{}
	form.Set("params", params)
	form.Set("encSecKey", encSecKey)

	headers := []utils.RequestOption{
		utils.WithHeader("Referer", Referer),
		utils.WithHeader("Content-Type", "application/x-www-form-urlencoded"),
		utils.WithHeader("Cookie", n.cookieSnapshot()),
		utils.WithRandomIPHeader(),
	}

	body, err := utils.Post(DetailAPI, strings.NewReader(form.Encode()), headers...)
	if err != nil {
		return nil, err
	}

	var resp struct {
		Songs []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
			Ar   []struct {
				Name string `json:"name"`
			} `json:"ar"`
			Al struct {
				Name   string `json:"name"`
				PicURL string `json:"picUrl"`
			} `json:"al"`
			Dt int `json:"dt"`
		} `json:"songs"`
	}

	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}

	var songs []model.Song
	for _, item := range resp.Songs {
		var artistNames []string
		for _, ar := range item.Ar {
			artistNames = append(artistNames, ar.Name)
		}

		songs = append(songs, model.Song{
			Source:   "netease",
			ID:       strconv.FormatInt(item.ID, 10),
			Name:     item.Name,
			Artist:   strings.Join(artistNames, "、"),
			Album:    item.Al.Name,
			Duration: item.Dt / 1000,
			Cover:    item.Al.PicURL,
			Link:     fmt.Sprintf("https://music.163.com/#/song?id=%d", item.ID),
			Extra: map[string]string{
				"song_id": strconv.FormatInt(item.ID, 10),
			},
		})
	}
	return songs, nil
}

func parseNeteaseLink(link string) (neteaseLinkKind, string, error) {
	candidates := []string{link}

	if parsed, err := url.Parse(link); err == nil {
		if parsed.Path != "" && parsed.Path != "/" {
			pathCandidate := parsed.Path
			if parsed.RawQuery != "" {
				pathCandidate += "?" + parsed.RawQuery
			}
			candidates = append(candidates, pathCandidate)
		}
		if fragment := strings.TrimSpace(strings.TrimPrefix(parsed.Fragment, "!")); fragment != "" {
			candidates = append(candidates, fragment)
		}
	}

	for _, candidate := range candidates {
		if kind, id, ok := parseNeteaseLinkCandidate(candidate); ok {
			return kind, id, nil
		}
	}

	return neteaseLinkUnknown, "", errInvalidNeteaseLink
}

func parseNeteaseLinkCandidate(candidate string) (neteaseLinkKind, string, bool) {
	parsed, err := url.Parse(candidate)
	if err != nil {
		return neteaseLinkUnknown, "", false
	}

	var kind neteaseLinkKind
	segments := strings.FieldsFunc(strings.ToLower(parsed.Path), func(r rune) bool {
		return r == '/'
	})
	for _, segment := range segments {
		switch segment {
		case string(neteaseLinkSong):
			kind = neteaseLinkSong
		case string(neteaseLinkAlbum):
			kind = neteaseLinkAlbum
		case string(neteaseLinkPlaylist):
			kind = neteaseLinkPlaylist
		}
	}

	id := parsed.Query().Get("id")
	if !isDigits(id) {
		id = ""
	}

	if kind == neteaseLinkUnknown && len(segments) >= 2 {
		last := segments[len(segments)-1]
		prev := segments[len(segments)-2]
		if isDigits(last) {
			switch prev {
			case string(neteaseLinkSong):
				kind = neteaseLinkSong
			case string(neteaseLinkAlbum):
				kind = neteaseLinkAlbum
			case string(neteaseLinkPlaylist):
				kind = neteaseLinkPlaylist
			}
			if kind != neteaseLinkUnknown {
				id = last
			}
		}
	}

	if kind == neteaseLinkUnknown || id == "" {
		return neteaseLinkUnknown, "", false
	}

	return kind, id, true
}

func isDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, ch := range value {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

// downloadURLCacheKey 同时带上账号、歌曲与档位，保证换 Cookie 后不会命中
// 上一个账号的地址，也不会把某一档位的地址复用给另一档位。
func downloadURLCacheKey(accountKey string, songID string, levels string) string {
	return accountKey + ":" + songID + ":" + levels
}

func (n *Netease) getCachedDownloadURL(accountKey string, songID string, levels string) (cachedDownloadURL, bool) {
	key := downloadURLCacheKey(accountKey, songID, levels)
	now := neteaseNow()

	downloadURLCache.Lock()
	defer downloadURLCache.Unlock()

	item, ok := downloadURLCache.items[key]
	if !ok {
		return cachedDownloadURL{}, false
	}
	if now.After(item.expiresAt) {
		delete(downloadURLCache.items, key)
		return cachedDownloadURL{}, false
	}
	return item, true
}

// setCachedDownloadURL 只缓存真正可用的地址。空地址、或档位高于返回流实际
// 音质的结果都不应固化，否则后续请求会拿到被静默降级的地址。
func (n *Netease) setCachedDownloadURL(accountKey string, songID string, levels string, url string, ext string) {
	if strings.TrimSpace(url) == "" {
		return
	}
	key := downloadURLCacheKey(accountKey, songID, levels)

	downloadURLCache.Lock()
	defer downloadURLCache.Unlock()
	downloadURLCache.items[key] = cachedDownloadURL{
		url:       url,
		ext:       ext,
		expiresAt: neteaseNow().Add(downloadURLCacheTTL),
	}
}

// purgeAccountDownloadURLCache 清掉某账号在某次切换前的下载地址缓存。账号键已
// 变化时旧条目不会再被命中，这里主动清理避免内存里长期残留其他账号的地址。
func purgeAccountDownloadURLCache(accountKey string) {
	downloadURLCache.Lock()
	defer downloadURLCache.Unlock()
	for key := range downloadURLCache.items {
		if strings.HasPrefix(key, accountKey+":") {
			delete(downloadURLCache.items, key)
		}
	}
}

func preferredDownloadLevels(s *model.Song) []string {
	if s != nil && s.Extra != nil {
		level := strings.ToLower(strings.TrimSpace(s.Extra["netease_level"]))
		if level == "" {
			level = strings.ToLower(strings.TrimSpace(s.Extra["level"]))
		}
		switch level {
		case "standard", "higher", "exhigh", "lossless", "hires",
			"dolby", "vivid", "sky", "jyeffect", "jymaster":
			return []string{level}
		}
	}
	return []string{
		"jymaster",
		"sky",
		"jyeffect",
		"vivid",
		"dolby",
		"hires",
		"lossless",
		"exhigh",
		"higher",
		"standard",
	}
}

// getEAPIDownloadURL 获取下载地址，以及描述实际返回音频流的元数据。
func (n *Netease) getEAPIDownloadURL(songID string, quality string, cookie string) (string, string, int, string, error) {
	idNum, err := strconv.Atoi(songID)
	if err != nil {
		return "", "", 0, "", fmt.Errorf("invalid song id: %v", err)
	}

	headerJSON := `{"os":"pc","appver":"","osver":"","deviceId":"pyncm!","requestId":"12345678"}`
	encodeType := "flac"
	if strings.EqualFold(strings.TrimSpace(quality), "vivid") {
		// 网易云目前只通过 Android 客户端开放 vivid 音质。
		headerJSON = `{"os":"android","appver":"9.5.61","osver":"","deviceId":"pyncm!","requestId":"12345678"}`
		encodeType = "mp3"
	}

	payload := map[string]interface{}{
		"ids":        []int{idNum},
		"level":      quality,
		"encodeType": encodeType,
		"header":     headerJSON,
	}

	eapiEndpoint := neteaseDownloadEAPIEndpoint

	payloadBytes, _ := json.Marshal(payload)
	params := EncryptEApi(eapiEndpoint, string(payloadBytes))

	form := url.Values{}
	form.Set("params", params)

	headers := []utils.RequestOption{
		utils.WithHeader("Referer", Referer),
		utils.WithHeader("Content-Type", "application/x-www-form-urlencoded"),
		utils.WithHeader("Cookie", cookie),
		utils.WithRandomIPHeader(),
	}

	body, err := utils.Post(eapiEndpoint, strings.NewReader(form.Encode()), headers...)
	if err != nil {
		return "", "", 0, "", err
	}

	var resp struct {
		Data []struct {
			URL        string `json:"url"`
			Code       int    `json:"code"`
			Type       string `json:"type"`
			Bitrate    int    `json:"br"`
			Level      string `json:"level"`
			EncodeType string `json:"encodeType"`
		} `json:"data"`
	}

	if err := json.Unmarshal(body, &resp); err != nil {
		return "", "", 0, "", fmt.Errorf("eapi json parse error: %w", err)
	}
	if len(resp.Data) == 0 || resp.Data[0].URL == "" {
		return "", "", 0, "", errors.New("eapi download url not found")
	}
	item := resp.Data[0]
	actualLevel := strings.ToLower(strings.TrimSpace(item.Level))
	ext := normalizeNeteaseAudioType(
		firstNeteaseNonEmpty(item.Type, item.EncodeType),
		actualLevel,
	)
	return item.URL, ext, item.Bitrate, actualLevel, nil
}

func normalizeNeteaseAudioType(audioType string, actualLevel string) string {
	audioType = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(audioType, ".")))
	switch audioType {
	case "flac", "mp3", "m4a", "wav":
		return audioType
	}
	switch strings.ToLower(strings.TrimSpace(actualLevel)) {
	case "lossless", "hires":
		return "flac"
	default:
		return ""
	}
}

func firstNeteaseNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
