package netease

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/guohuiyuan/music-lib/model"
	"github.com/guohuiyuan/music-lib/utils"
)

// neteaseMockServer 模拟网易云的探测/下载/账号接口。它按请求 Cookie 决定
// 账号能拿到哪些档位，并记录 EAPI 调用次数，用来验证探测缓存与地址缓存。
type neteaseMockServer struct {
	server *httptest.Server

	mu              sync.Mutex
	allowed         map[string]bool            // 所有账号共用
	allowedByCookie map[string]map[string]bool // 按 Cookie 覆盖
	failOnce        map[string]bool            // 下一次该档位返回空地址
	downgradeTo     map[string]string          // 请求该档位时返回更低档位
	eapiCalls       int
	weapiCalls      int

	blockFirst bool
	firstOnce  sync.Once
	firstCall  chan struct{}
	release    chan struct{}
}

func newNeteaseMockServer(t *testing.T) *neteaseMockServer {
	t.Helper()

	mock := &neteaseMockServer{
		allowed:         map[string]bool{},
		allowedByCookie: map[string]map[string]bool{},
		failOnce:        map[string]bool{},
		downgradeTo:     map[string]string{},
		firstCall:       make(chan struct{}),
		release:         make(chan struct{}),
	}
	mock.server = httptest.NewServer(http.HandlerFunc(mock.handle))

	oldSearch := neteaseSearchEndpoint
	oldWeAPI := neteaseDownloadWeAPIEndpoint
	oldEAPI := neteaseDownloadEAPIEndpoint
	oldUser := neteaseUserAccountEndpoint
	neteaseSearchEndpoint = mock.server.URL + "/api/linux/forward"
	neteaseDownloadWeAPIEndpoint = mock.server.URL + "/weapi/song/enhance/player/url"
	neteaseDownloadEAPIEndpoint = mock.server.URL + "/eapi/song/enhance/player/url/v1"
	neteaseUserAccountEndpoint = mock.server.URL + "/weapi/nuser/account/get"

	resetNeteaseCaches()

	t.Cleanup(func() {
		neteaseSearchEndpoint = oldSearch
		neteaseDownloadWeAPIEndpoint = oldWeAPI
		neteaseDownloadEAPIEndpoint = oldEAPI
		neteaseUserAccountEndpoint = oldUser
		mock.server.Close()
		resetNeteaseCaches()
	})
	return mock
}

func resetNeteaseCaches() {
	downloadURLCache.Lock()
	downloadURLCache.items = make(map[string]cachedDownloadURL)
	downloadURLCache.Unlock()

	vipStatusCache.Lock()
	vipStatusCache.items = make(map[string]cachedVIPStatus)
	vipStatusCache.Unlock()
}

func (m *neteaseMockServer) handle(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/api/linux/forward":
		writeJSON(w, map[string]any{"code": 200, "result": map[string]any{"songs": []any{}}})
	case r.URL.Path == "/weapi/nuser/account/get":
		writeJSON(w, map[string]any{"code": 200, "profile": map[string]any{"vipType": 11}})
	case r.URL.Path == "/weapi/song/enhance/player/url":
		m.mu.Lock()
		m.weapiCalls++
		m.mu.Unlock()
		writeJSON(w, map[string]any{"data": []any{}})
	case r.URL.Path == "/eapi/song/enhance/player/url/v1":
		m.handleEAPI(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (m *neteaseMockServer) handleEAPI(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	level, err := decodeEAPIRequestLevel(r.PostFormValue("params"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	m.mu.Lock()
	m.eapiCalls++
	callNumber := m.eapiCalls
	cookieTag := cookieTagFor(r.Header.Get("Cookie"))
	allowed := m.levelAllowedLocked(cookieTag, level)
	if m.failOnce[level] {
		delete(m.failOnce, level)
		allowed = false
	}
	responseLevel := level
	if downgrade, ok := m.downgradeTo[level]; ok {
		allowed = true
		responseLevel = downgrade
	}
	m.mu.Unlock()

	if m.blockFirst && callNumber == 1 {
		m.firstOnce.Do(func() { close(m.firstCall) })
		<-m.release
	}

	if !allowed {
		writeJSON(w, map[string]any{"data": []any{map[string]any{
			"url": "", "code": 404, "level": level,
		}}})
		return
	}

	bitrate, audioType := levelStreamMeta(responseLevel)
	writeJSON(w, map[string]any{"data": []any{map[string]any{
		"url":        fmt.Sprintf("https://cdn.example/%s/%s.mp3", cookieTag, responseLevel),
		"code":       200,
		"type":       audioType,
		"encodeType": audioType,
		"br":         bitrate,
		"level":      responseLevel,
	}}})
}

func (m *neteaseMockServer) levelAllowedLocked(cookieTag, level string) bool {
	if set, ok := m.allowedByCookie[cookieTag]; ok {
		return set[level]
	}
	return m.allowed[level]
}

func (m *neteaseMockServer) setAllowed(levels ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.allowed = make(map[string]bool, len(levels))
	for _, level := range levels {
		m.allowed[level] = true
	}
}

func (m *neteaseMockServer) setAllowedForCookie(cookie string, levels ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	set := make(map[string]bool, len(levels))
	for _, level := range levels {
		set[level] = true
	}
	m.allowedByCookie[cookieTagFor(cookie)] = set
}

func (m *neteaseMockServer) calls() (eapi int, weapi int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.eapiCalls, m.weapiCalls
}

func (m *neteaseMockServer) failNext(level string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failOnce[level] = true
}

func (m *neteaseMockServer) setDowngrade(requested, served string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.downgradeTo[requested] = served
}

func cookieTagFor(cookie string) string {
	return utils.MD5(withClientIdentity(cookie))[:8]
}

func writeJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}

// decodeEAPIRequestLevel 解开 EAPI 的 params，返回其中请求的音质档位。
// 加密方式是同包内的 AES-128-ECB，测试可以直接复用解密实现。
func decodeEAPIRequestLevel(params string) (string, error) {
	raw, err := hex.DecodeString(params)
	if err != nil {
		return "", fmt.Errorf("decode eapi params hex: %w", err)
	}
	plain, err := aesECBDecrypt([]byte("e82ckenh8dichen8"), raw)
	if err != nil {
		return "", fmt.Errorf("decrypt eapi params: %w", err)
	}
	plain = pkcs7Unpad(plain)
	parts := strings.SplitN(string(plain), "-36cd479b6b5-", 3)
	if len(parts) < 2 {
		return "", fmt.Errorf("unexpected eapi payload shape")
	}
	var payload struct {
		Level string `json:"level"`
	}
	if err := json.Unmarshal([]byte(parts[1]), &payload); err != nil {
		return "", fmt.Errorf("parse eapi payload: %w", err)
	}
	return payload.Level, nil
}

func levelStreamMeta(level string) (int, string) {
	switch level {
	case "jymaster":
		return 2_304_000, "wav"
	case "sky", "jyeffect", "dolby", "hires", "lossless":
		return 900_000, "flac"
	case "vivid", "exhigh":
		return 320_000, "mp3"
	case "higher":
		return 192_000, "mp3"
	case "standard":
		return 128_000, "mp3"
	default:
		return 128_000, "mp3"
	}
}

func qualityValues(options []model.AudioQualityOption) []model.AudioQuality {
	values := make([]model.AudioQuality, 0, len(options))
	for _, option := range options {
		values = append(values, option.Value)
	}
	return values
}

func qualitySet(options []model.AudioQualityOption) map[model.AudioQuality]bool {
	set := make(map[model.AudioQuality]bool, len(options))
	for _, option := range options {
		set[option.Value] = true
	}
	return set
}

func assertQualityLadder(t *testing.T, options []model.AudioQualityOption, want ...model.AudioQuality) {
	t.Helper()
	got := qualityValues(options)
	if len(got) != len(want) {
		t.Fatalf("available qualities = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("available qualities = %v, want %v", got, want)
		}
	}
}

// fakeNeteaseClock 让 TTL 过期逻辑可以被测试推进。
type fakeNeteaseClock struct {
	mu  sync.Mutex
	now time.Time
}

func installFakeNeteaseClock(t *testing.T) *fakeNeteaseClock {
	t.Helper()
	clock := &fakeNeteaseClock{now: time.Now()}
	old := neteaseNow
	neteaseNow = func() time.Time {
		clock.mu.Lock()
		defer clock.mu.Unlock()
		return clock.now
	}
	t.Cleanup(func() { neteaseNow = old })
	return clock
}

func (c *fakeNeteaseClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestNeteaseAvailableQualityOptionsReflectsAccountLadder(t *testing.T) {
	mock := newNeteaseMockServer(t)
	// 账号能拿到 hires 时，更高档位探测失败不应影响结果，且应包含全部更低档位。
	mock.setAllowed("hires", "lossless", "exhigh", "higher", "standard")

	client := New("MUSIC_U=normal-ladder")
	options, err := client.AvailableQualityOptions()
	if err != nil {
		t.Fatalf("AvailableQualityOptions() error = %v", err)
	}
	assertQualityLadder(t, options,
		model.AudioQualityHiRes,
		model.AudioQualityLossless,
		model.AudioQualityHigh,
		model.AudioQualityHigher,
		model.AudioQualityStandard,
	)

	// 第二次调用命中探测缓存，不再重复探测。
	before, _ := mock.calls()
	if _, err := client.AvailableQualityOptions(); err != nil {
		t.Fatalf("cached AvailableQualityOptions() error = %v", err)
	}
	after, _ := mock.calls()
	if after != before {
		t.Fatalf("cached probe made %d extra EAPI calls, want 0", after-before)
	}
}

func TestNeteaseAvailableQualityOptionsRetriesAfterFailure(t *testing.T) {
	mock := newNeteaseMockServer(t)
	// 所有档位都不可用：探测报错，且失败不得被固化。
	mock.setAllowed()

	client := New("MUSIC_U=retry-account")
	if _, err := client.AvailableQualityOptions(); err == nil {
		t.Fatal("AvailableQualityOptions() should fail when no quality is available")
	}

	// 恢复后同一实例应重新探测并成功，而不是继续返回失败结果。
	mock.setAllowed("exhigh", "higher", "standard")
	options, err := client.AvailableQualityOptions()
	if err != nil {
		t.Fatalf("AvailableQualityOptions() after recovery error = %v", err)
	}
	assertQualityLadder(t, options,
		model.AudioQualityHigh,
		model.AudioQualityHigher,
		model.AudioQualityStandard,
	)
}

func TestNeteaseAvailableQualityOptionsConcurrentCallsAgree(t *testing.T) {
	mock := newNeteaseMockServer(t)
	mock.setAllowed("exhigh", "higher", "standard")
	mock.blockFirst = true

	client := New("MUSIC_U=concurrent-account")

	const goroutines = 8
	var started atomic.Int32
	results := make([][]model.AudioQualityOption, goroutines)
	errs := make([]error, goroutines)
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(index int) {
			defer wg.Done()
			started.Add(1)
			results[index], errs[index] = client.AvailableQualityOptions()
		}(i)
	}

	// 等第一个 EAPI 请求卡在服务端，并确保所有并发调用都已进入探测等待，
	// 再放行，这样能确定性地验证“单飞 + 一致结果”。
	<-mock.firstCall
	deadline := time.Now().Add(5 * time.Second)
	for started.Load() < goroutines && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	close(mock.release)
	wg.Wait()

	for i := 0; i < goroutines; i++ {
		if errs[i] != nil {
			t.Fatalf("goroutine %d error = %v", i, errs[i])
		}
		assertQualityLadder(t, results[i],
			model.AudioQualityHigh,
			model.AudioQualityHigher,
			model.AudioQualityStandard,
		)
	}

	eapiCalls, _ := mock.calls()
	// 一次完整探测：master(1) + spatial(2) + vivid(1) + atmos(1) + hires(1) +
	// lossless(1) + exhigh(1) = 8。并发调用不应重复探测。
	if eapiCalls != 8 {
		t.Fatalf("concurrent probe issued %d EAPI calls, want 8 (single-flight)", eapiCalls)
	}
}

func TestNeteaseQualityIsolationAcrossCookieSwitch(t *testing.T) {
	mock := newNeteaseMockServer(t)
	mock.setAllowedForCookie("MUSIC_U=account-a",
		"hires", "lossless", "exhigh", "higher", "standard")
	mock.setAllowedForCookie("MUSIC_U=account-b", "standard")

	client := New("MUSIC_U=account-a")
	song := &model.Song{Source: "netease", ID: "1000001"}

	urlA, err := client.GetDownloadURLWithQuality(song, "standard")
	if err != nil {
		t.Fatalf("account A standard download error = %v", err)
	}
	if !strings.Contains(urlA, cookieTagFor("MUSIC_U=account-a")) {
		t.Fatalf("account A url = %q, want it to carry account A identity", urlA)
	}

	optionsA, err := client.AvailableQualityOptions()
	if err != nil {
		t.Fatalf("account A probe error = %v", err)
	}
	if !qualitySet(optionsA)[model.AudioQualityHiRes] {
		t.Fatalf("account A qualities = %v, want hires", qualityValues(optionsA))
	}

	// 切换到账号 B：探测结果与下载地址都必须重新按 B 的权益获取。
	client.setCookie("MUSIC_U=account-b")

	optionsB, err := client.AvailableQualityOptions()
	if err != nil {
		t.Fatalf("account B probe error = %v", err)
	}
	assertQualityLadder(t, optionsB, model.AudioQualityStandard)

	callsBefore, _ := mock.calls()
	urlB, err := client.GetDownloadURLWithQuality(song, "standard")
	if err != nil {
		t.Fatalf("account B standard download error = %v", err)
	}
	callsAfter, _ := mock.calls()
	if callsAfter == callsBefore {
		t.Fatal("account B reused account A's cached download url instead of requesting a fresh one")
	}
	if urlB == urlA {
		t.Fatalf("account B url = %q must not equal account A url", urlB)
	}
	if !strings.Contains(urlB, cookieTagFor("MUSIC_U=account-b")) {
		t.Fatalf("account B url = %q, want it to carry account B identity", urlB)
	}
}

func TestNeteaseDownloadURLRejectsHigherThanAvailable(t *testing.T) {
	t.Run("interface returns empty", func(t *testing.T) {
		mock := newNeteaseMockServer(t)
		mock.setAllowed("exhigh", "higher", "standard")

		client := New("MUSIC_U=high-only")
		song := &model.Song{Source: "netease", ID: "1000002"}

		if _, err := client.GetDownloadURLWithQuality(song, "lossless"); err == nil {
			t.Fatal("lossless request should fail when account only has 320k")
		}
	})

	t.Run("interface silently downgrades", func(t *testing.T) {
		mock := newNeteaseMockServer(t)
		mock.setAllowed("lossless")
		mock.setDowngrade("lossless", "exhigh")

		client := New("MUSIC_U=downgrade")
		song := &model.Song{Source: "netease", ID: "1000003"}

		if _, err := client.GetDownloadURLWithQuality(song, "lossless"); err == nil {
			t.Fatal("a downgraded 320k stream must not satisfy a lossless request")
		}
	})
}

func TestNeteaseDownloadURLDoesNotCacheTransientFailure(t *testing.T) {
	mock := newNeteaseMockServer(t)
	mock.setAllowed("exhigh")
	mock.failNext("exhigh")

	client := New("MUSIC_U=transient")
	song := &model.Song{Source: "netease", ID: "1000004"}

	if _, err := client.GetDownloadURLWithQuality(song, "high"); err == nil {
		t.Fatal("first request should fail while the interface fails")
	}

	// 失败不能写入缓存；第二次请求应重新调用接口并成功。
	url, err := client.GetDownloadURLWithQuality(song, "high")
	if err != nil {
		t.Fatalf("second request error = %v, want success after transient failure", err)
	}
	if strings.TrimSpace(url) == "" {
		t.Fatal("second request returned empty url")
	}
}

func TestNeteaseCachesRecoverAfterExpiry(t *testing.T) {
	clock := installFakeNeteaseClock(t)
	mock := newNeteaseMockServer(t)
	mock.setAllowed("exhigh", "higher", "standard")

	client := New("MUSIC_U=expiry")
	song := &model.Song{Source: "netease", ID: "1000005"}

	// 下载地址在有效期内复用。
	if _, err := client.GetDownloadURLWithQuality(song, "high"); err != nil {
		t.Fatalf("first download error = %v", err)
	}
	afterFirst, _ := mock.calls()

	if _, err := client.GetDownloadURLWithQuality(song, "high"); err != nil {
		t.Fatalf("cached download error = %v", err)
	}
	afterCached, _ := mock.calls()
	if afterCached != afterFirst {
		t.Fatalf("download cache reuse added %d EAPI calls, want 0", afterCached-afterFirst)
	}

	// 过期后必须重新请求。
	clock.Advance(downloadURLCacheTTL + time.Minute)
	if _, err := client.GetDownloadURLWithQuality(song, "high"); err != nil {
		t.Fatalf("expired download error = %v", err)
	}
	afterExpired, _ := mock.calls()
	if afterExpired == afterCached {
		t.Fatal("expired download cache was not refreshed")
	}

	// 探测结果同样在有效期内复用、过期后重新探测。
	probeBefore, _ := mock.calls()
	if _, err := client.AvailableQualityOptions(); err != nil {
		t.Fatalf("first probe error = %v", err)
	}
	probeCached, _ := mock.calls()
	if _, err := client.AvailableQualityOptions(); err != nil {
		t.Fatalf("cached probe error = %v", err)
	}
	probeAfterCached, _ := mock.calls()
	if probeAfterCached != probeCached {
		t.Fatalf("probe cache reuse added %d EAPI calls, want 0", probeAfterCached-probeCached)
	}
	if probeCached == probeBefore {
		t.Fatal("first probe did not issue any request")
	}

	clock.Advance(availableQualityCacheTTL + time.Minute)
	if _, err := client.AvailableQualityOptions(); err != nil {
		t.Fatalf("expired probe error = %v", err)
	}
	probeExpired, _ := mock.calls()
	if probeExpired == probeAfterCached {
		t.Fatal("expired probe cache was not refreshed")
	}
}

func TestNeteaseProbeAndDownloadShareAccountSnapshot(t *testing.T) {
	mock := newNeteaseMockServer(t)
	mock.setAllowedForCookie("MUSIC_U=snap-a",
		"hires", "lossless", "exhigh", "higher", "standard")
	mock.setAllowedForCookie("MUSIC_U=snap-b", "standard")

	client := New("MUSIC_U=snap-a")

	var wg sync.WaitGroup
	probeResults := make([][]model.AudioQualityOption, 6)
	probeErrs := make([]error, 6)
	downloadResults := make([]string, 6)
	downloadErrs := make([]error, 6)

	for i := 0; i < 6; i++ {
		wg.Add(2)
		go func(index int) {
			defer wg.Done()
			probeResults[index], probeErrs[index] = client.AvailableQualityOptions()
		}(i)
		go func(index int) {
			defer wg.Done()
			// 每个 goroutine 用独立 Song，避免测试自身在 Ext 上产生数据竞争；
			// 库对同一首歌曲的缓存共享仍会被验证到。
			song := &model.Song{Source: "netease", ID: "1000006"}
			downloadResults[index], downloadErrs[index] = client.GetDownloadURLWithQuality(song, "standard")
		}(i)
	}
	wg.Wait()

	for i := 0; i < 6; i++ {
		if probeErrs[i] != nil {
			t.Fatalf("probe %d error = %v", i, probeErrs[i])
		}
		if !qualitySet(probeResults[i])[model.AudioQualityHiRes] {
			t.Fatalf("probe %d qualities = %v, want account A ladder", i, qualityValues(probeResults[i]))
		}
		if downloadErrs[i] != nil {
			t.Fatalf("download %d error = %v", i, downloadErrs[i])
		}
		if !strings.Contains(downloadResults[i], cookieTagFor("MUSIC_U=snap-a")) {
			t.Fatalf("download %d url = %q, want account A identity", i, downloadResults[i])
		}
	}
}

func TestNeteaseProbeDiscardsStaleAccountResults(t *testing.T) {
	mock := newNeteaseMockServer(t)
	mock.setAllowedForCookie("MUSIC_U=stale-a", "hires", "lossless")
	mock.setAllowedForCookie("MUSIC_U=stale-b", "standard")
	mock.blockFirst = true

	client := New("MUSIC_U=stale-a")

	type probeResult struct {
		options []model.AudioQualityOption
		err     error
	}
	firstDone := make(chan probeResult, 1)
	go func() {
		options, err := client.AvailableQualityOptions()
		firstDone <- probeResult{options: options, err: err}
	}()

	<-mock.firstCall
	// 在旧账号探测进行中切换 Cookie；旧结果必须作废。
	client.setCookie("MUSIC_U=stale-b")
	close(mock.release)

	result := <-firstDone
	if result.err != nil {
		t.Fatalf("probe error after switch = %v", result.err)
	}
	assertQualityLadder(t, result.options, model.AudioQualityStandard)

	// 再次探测仍应是新账号结果，说明旧结果没有被缓存。
	options, err := client.AvailableQualityOptions()
	if err != nil {
		t.Fatalf("follow-up probe error = %v", err)
	}
	assertQualityLadder(t, options, model.AudioQualityStandard)
}
