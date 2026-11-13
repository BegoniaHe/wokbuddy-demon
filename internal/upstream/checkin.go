// checkin.go 每日签到：请求、结果分类与幂等/未开启判定。
//
// 上游签到的两种回执形态都只有「不看信封」才读得到：
//   - 成功：平铺 {credit, streak_days, is_streak_day}（官方 main/account.js mapCheckinStatus）；
//   - 失败：HTTP 400 + {code, msg, requestId} 信封。
//
// 分类只描述结果语义（checked / already / inactive / failed），不改变重试口径。
package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// DailyCheckin 执行每日签到。已签到（业务 code 非 0）也返回错误，调用方按 msg 区分。
// 偶发上游 5xx（code 10000）做有界重试（见 retryBillingTransient）——单次抖动不再
// 让该账号整天漏签；「已签到」等业务错误不重试。
// 签到是 X-Device-Token 的消费面之一（另一处是 chat，见 chatSurfaceOverrides；
// auth/state 是面板登录面，不经本包），见 checkinHeaders。
func (c *Client) DailyCheckin(ctx context.Context, a *auth.Auth) error {
	return c.retryBillingTransient(ctx, func() error {
		_, err := c.billingMeterJSONHeaders(a, checkinMeterPaths(), map[string]any{}, c.checkinHeaders(a))
		return err
	})
}

// CheckinActivityStatus 查询签到活动状态（官方桌面端 checkin-activity-status）。
// 返回 today_checked_in；请求失败时调用方回落直接签到路径。
func (c *Client) CheckinActivityStatus(a *auth.Auth) (todayChecked bool, err error) {
	data, err := c.billingMeterJSONHeaders(
		a, []string{checkinActivityStatusPath}, map[string]any{}, c.checkinHeaders(a),
	)
	if err != nil {
		return false, err
	}

	var status struct {
		TodayCheckedIn bool `json:"today_checked_in"`
	}
	if err := json.Unmarshal(data, &status); err != nil {
		return false, fmt.Errorf("checkin status parse: %w", err)
	}

	return status.TodayCheckedIn, nil
}

// IsAlreadyCheckin 报告 err 是否表示"今天已签到"（上游幂等拒绝重复签到）。
// 只认带分类的 *Error（业务 code 或 HTTP 错误）：网络层/解析层错误不得当作幂等成功，
// 否则停机补签遇到抖动会误记为 already，账号当天实际未签到却被判定正常。
func IsAlreadyCheckin(err error) bool {
	return matchCheckinMarker(err, alreadyCheckinMarkers)
}

// IsCheckinInactive 报告 err 是否表示"签到活动未开启/未开放/已结束"等非错误态。
// 与 IsAlreadyCheckin 同口径：只认 *Error.Msg，网络层/解析层错误不在此识别；
// 保守策略——未命中显式 inactive 关键词即返回 false（真失败不误判为 inactive）。
func IsCheckinInactive(err error) bool {
	return matchCheckinMarker(err, inactiveCheckinMarkers)
}

// matchCheckinMarker 对 *Error.Msg 做大小写不敏感的关键词包含匹配。
func matchCheckinMarker(err error, markers []string) bool {
	var upErr *Error
	if !errors.As(err, &upErr) {
		return false
	}

	lower := strings.ToLower(upErr.Msg)
	for _, m := range markers {
		if strings.Contains(upErr.Msg, m) || strings.Contains(lower, strings.ToLower(m)) {
			return true
		}
	}

	return false
}

// CheckinResult 签到结果分类：inactive（活动未开启）与 already（幂等）从真失败中分离，
// 供调度日志与面板按类呈现，避免把非错误态刷成 error 行。
type CheckinResult int

const (
	// CheckinOK 签到成功（err == nil）。
	CheckinOK CheckinResult = iota
	// CheckinAlready 上游幂等拒绝重复签到（"今天已签到"）。
	CheckinAlready
	// CheckinInactive 活动未开启/未开放/已结束等非错误态。
	CheckinInactive
	// CheckinFailed 其余业务/HTTP 错误（真失败）。
	CheckinFailed
)

// ClassifyCheckin 归类签到结果。分类只描述「结果语义」，不改变重试与幂等判定。
func ClassifyCheckin(err error) CheckinResult {
	code := checkinBusinessCode(err)
	switch {
	case err == nil:
		return CheckinOK
	case code == "1001":
		return CheckinAlready
	case code == "1002" || code == "1003":
		return CheckinInactive
	case IsAlreadyCheckin(err):
		return CheckinAlready
	case IsCheckinInactive(err):
		return CheckinInactive
	default:
		return CheckinFailed
	}
}

// checkinBusinessCode 从 *Error 提取签到业务码：优先结构化 Code（Msg 截断不影响），
// 再回落 Msg 文本匹配。非 *Error 返回空串。
func checkinBusinessCode(err error) string {
	var upstreamErr *Error
	if !errors.As(err, &upstreamErr) {
		return ""
	}

	for _, code := range []string{"1001", "1002", "1003"} {
		if upstreamErr.Code == code || matchBusinessCode(upstreamErr.Msg, code) {
			return code
		}
	}

	return ""
}

// matchBusinessCode 匹配结构化业务码（顶层/嵌套 code、字符串或数字），
// 完整比对避免 10012 被前缀匹配成 1001。
func matchBusinessCode(msg, code string) bool {
	return hasBusinessCode(msg, code)
}
