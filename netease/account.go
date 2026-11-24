package netease

import (
	"encoding/json"
	"fmt"
	"github.com/guohuiyuan/music-lib/utils"
	"net/url"
	"strings"
)

// IsVipAccount 报告当前账号是否为 VIP。
func (n *Netease) IsVipAccount() (bool, error) {
	cookie := n.cookieSnapshot()
	accountKey := utils.MD5(cookie)

	n.stateMu.Lock()
	if n.isVipCache != nil && n.isVipCacheKey == accountKey {
		isVip := *n.isVipCache
		n.stateMu.Unlock()
		return isVip, nil
	}
	n.stateMu.Unlock()

	if cookie == "" {
		n.setAccountVIPCache(accountKey, false)
		return false, nil
	}

	if cached, ok := n.getCachedVIPStatus(accountKey); ok {
		n.setAccountVIPCache(accountKey, cached)
		return cached, nil
	}

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
		utils.WithHeader("Cookie", cookie),
		utils.WithRandomIPHeader(),
	}

	body, err := utils.Post(neteaseUserAccountEndpoint, strings.NewReader(form.Encode()), headers...)
	if err != nil {
		return false, fmt.Errorf("failed to fetch user account info: %w", err)
	}

	var resp struct {
		Code    int `json:"code"`
		Profile struct {
			VipType int `json:"vipType"`
		} `json:"profile"`
	}

	if err := json.Unmarshal(body, &resp); err != nil {
		return false, fmt.Errorf("netease user account json parse error: %w", err)
	}

	isVip := resp.Code == 200 && resp.Profile.VipType != 0
	n.setAccountVIPCache(accountKey, isVip)
	n.setCachedVIPStatus(accountKey, isVip)
	return isVip, nil
}

// setAccountVIPCache 只有在 Cookie 未被并发切换时才写入实例级 VIP 缓存，
// 避免把旧账号的结果贴到新账号上。
func (n *Netease) setAccountVIPCache(accountKey string, isVip bool) {
	n.stateMu.Lock()
	defer n.stateMu.Unlock()
	if n.accountKeyLocked() != accountKey {
		return
	}
	value := isVip
	n.isVipCache = &value
	n.isVipCacheKey = accountKey
}

// accountKeyLocked 返回当前账号键。调用方必须持有 stateMu。
func (n *Netease) accountKeyLocked() string {
	return utils.MD5(n.cookieSnapshot())
}
