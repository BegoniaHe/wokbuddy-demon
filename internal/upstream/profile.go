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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

var errFetchAccountProfileFailed = errors.New("FetchAccountProfile failed")

// FetchAccountProfile 拉取账号资料并返回最新昵称。uid 与凭证不一致时报错
// （防串号）；业务/网络错误原样返回，调用方静默跳过即可。
func (c *Client) FetchAccountProfile(account *auth.Auth) (string, error) {
	base := c.webBase(account)

	req, err := http.NewRequestWithContext(
		context.Background(), http.MethodGet, base+"/console/account", http.NoBody,
	)
	if err != nil {
		return "", fmt.Errorf("FetchAccountProfile: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+account.AccessTokenValue())
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Client-Platform", "web")
	// Origin/Referer 与请求主机同域（CN workbuddy.cn / global 签发域）。
	req.Header.Set("Origin", base)
	req.Header.Set("Referer", base+"/profile/account-settings")

	if ua := c.userAgent(account); ua != "" {
		req.Header.Set("User-Agent", ua)
	}

	// 取原始 body 自行解信封：上游两种形态并存（信封 {code,data:{uid,nickname}} 与
	// 顶层平铺 {uid,nickname}），doJSONFor 只返回 data，平铺形态会解出零值且不报错，
	// 调用方拿不到「解析落空」信号（issue #482）。
	raw, _, err := doRawAt(c.HTTPForAccount(account), req, defaultJSONBodyLimit)
	if err != nil {
		return "", err
	}

	uid, nickname, err := parseAccountProfile(raw)
	if err != nil {
		return "", err
	}

	if uid != "" && account.UID != "" && uid != account.UID {
		return "", fmt.Errorf("profile uid mismatch: resp=%s auth=%s: %w",
			uid, account.UID, errFetchAccountProfileFailed)
	}

	return nickname, nil
}

// parseAccountProfile 解析 /console/account 的两种已见形态，只取 uid 与 nickname
// （手机号等敏感字段在这里就被丢弃，不进入任何后续路径）。昵称为空返回错误而不是
// 静默成功——空昵称多半意味着形态没解出来，静默成功会让调用方把账号昵称覆盖成空串。
func parseAccountProfile(raw []byte) (string, string, error) {
	var profile struct {
		UID      string `json:"uid"`
		Nickname string `json:"nickname"`
	}

	var envelope struct {
		Data json.RawMessage `json:"data"`
	}

	if err := json.Unmarshal(raw, &envelope); err != nil {
		return "", "", fmt.Errorf("profile parse: %w", err)
	}

	trimmed := bytes.TrimSpace(envelope.Data)
	if len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) {
		if err := json.Unmarshal(trimmed, &profile); err != nil {
			return "", "", fmt.Errorf("profile parse: %w", err)
		}
	} else if err := json.Unmarshal(raw, &profile); err != nil {
		return "", "", fmt.Errorf("profile parse: %w", err)
	}

	if strings.TrimSpace(profile.Nickname) == "" {
		return profile.UID, "", fmt.Errorf("profile parse: empty nickname: %w",
			errFetchAccountProfileFailed)
	}

	return profile.UID, profile.Nickname, nil
}
