// profile.go Web 控制台账号资料（issue #94：改名后免重登同步昵称）。
//
// GET {webBase}/console/account，Bearer + x-client-platform: web（与 tasks/claim
// 同一鉴权形态，实测 2026-10-01：无需 Web 会话 cookie）。
//
// 隐私边界（重要）：该接口响应包含手机号（phoneNumber）等个人敏感信息。本方法
// 只解析 nickname 与 uid（uid 仅做一致性核对），其余字段一概不解析、不落日志、
// 不透传——调用方也拿不到。昵称同步只在面板手动「刷新」时触发，后台余额定时
// 刷新不调用本接口。
package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

var errFetchAccountProfileFailed = errors.New("FetchAccountProfile failed")

// FetchAccountProfile 拉取账号资料并返回最新昵称。uid 与凭证不一致时报错
// （防串号）；业务/网络错误原样返回，调用方静默跳过即可。
func (c *Client) FetchAccountProfile(account *auth.Auth) (string, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, c.webBase(account)+"/console/account", http.NoBody)
	if err != nil {
		return "", fmt.Errorf("FetchAccountProfile: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+account.AccessTokenValue())
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Client-Platform", "web")
	req.Header.Set("Origin", "https://www.workbuddy.cn")
	req.Header.Set("Referer", "https://www.workbuddy.cn/profile/account-settings")

	if ua := c.userAgent(account); ua != "" {
		req.Header.Set("User-Agent", ua)
	}

	data, err := c.doJSON(req)
	if err != nil {
		return "", err
	}
	// 只取两个字段：敏感信息（手机号等）在这里就被丢弃，不进入任何后续路径。
	var resp struct {
		UID      string `json:"uid"`
		Nickname string `json:"nickname"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("profile parse: %w", err)
	}

	if resp.UID != "" && account.UID != "" && resp.UID != account.UID {
		return "", fmt.Errorf("profile uid mismatch: resp=%s auth=%s: %w", resp.UID, account.UID, errFetchAccountProfileFailed)
	}

	return resp.Nickname, nil
}
