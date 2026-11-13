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
	"net/http"
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

// 签到关键词表。already 只认带上下文的整短语：裸「已签到」子串会命中真失败文案
// 「签到失败，已签到天数不足 7 天」，把真失败当成幂等成功（issue #511）。
// failed 是硬词，判定序里必须压过 already（见 classifyCheckinResult）。
var (
	alreadyCheckinMarkers = []string{
		"今天已签到", "今日已签到", "already checked in", "already signed in",
	}
	checkinFailedMarkers = []string{"签到失败", "checkin failed", "check in failed"}
	// inactiveCheckinMarkers "活动未开启/未开放/已结束"等非错误态关键词（上游对未开放
	// 的签到活动返回 code!=0 + 明确文案）。保守取带「活动」上下文的显式短语：未命中即判
	// 真失败，避免把真失败误当 inactive 而漏报；也不误伤凭证类文案（如「登录态已过期」
	// 含「已过期」但非活动态）。文案按实测补充（见 issue #27）。
	inactiveCheckinMarkers = []string{
		"活动未开启", "活动未开放", "活动已结束", "活动已过期",
	}
)

// ClassifyCheckin 归类签到结果。分类只描述「结果语义」，不改变重试与幂等判定。
func ClassifyCheckin(err error) CheckinResult {
	if err == nil {
		return CheckinOK
	}

	var upErr *Error
	if !errors.As(err, &upErr) {
		return CheckinFailed
	}

	return classifyCheckinResult(checkinErrorCode(upErr), upErr.Msg)
}

// checkinErrorCode 取签到业务码：优先结构化 Code；Error 只带 Msg（Msg 就是整段信封）
// 时回落信封文本匹配。完整比对避免 10012 被前缀匹配成 1001。
func checkinErrorCode(upErr *Error) string {
	if upErr.Code != "" {
		return upErr.Code
	}

	for _, code := range []string{"1001", "10001", "1002", "1003"} {
		if hasBusinessCode(upErr.Msg, code) {
			return code
		}
	}

	return ""
}

// classifyCheckinResult 是签到结果分类的唯一实现：结构化业务码优先，其次失败硬词，
// 再幂等/未开启关键词。裸子串匹配一律不做（见关键词表注释）。
func classifyCheckinResult(code, msg string) CheckinResult {
	if result, ok := checkinCodeResult(code); ok {
		return result
	}

	if matchCheckinText(msg, checkinFailedMarkers) {
		return CheckinFailed
	}

	if matchCheckinText(msg, alreadyCheckinMarkers) {
		return CheckinAlready
	}

	if matchCheckinText(msg, inactiveCheckinMarkers) {
		return CheckinInactive
	}

	return CheckinFailed
}

// checkinCodeResult 结构化业务码 → 结果。官方 main/account.js mapCheckinStatus 的四态表
// 是 1001=already_claimed / 1002=not_eligible / 1003=event_ended；实测重复签到另有
// code=10001「今天已签到」。表外的码不在这里定性，交文案判定。
func checkinCodeResult(code string) (CheckinResult, bool) {
	switch code {
	case "1001", "10001":
		return CheckinAlready, true
	case "1002", "1003":
		return CheckinInactive, true
	}

	return CheckinOK, false
}

// matchCheckinText 大小写不敏感的关键词包含匹配。
func matchCheckinText(text string, markers []string) bool {
	lower := strings.ToLower(text)
	for _, marker := range markers {
		if strings.Contains(text, marker) || strings.Contains(lower, strings.ToLower(marker)) {
			return true
		}
	}

	return false
}

// CheckinState 签到回执状态（面板与前端按它出文案，不再靠「有没有 error」猜）。
type CheckinState string

const (
	// CheckinStateChecked 本次新签到成功。
	CheckinStateChecked CheckinState = "checked"
	// CheckinStateAlready 今日已签到（上游幂等拒绝）。
	CheckinStateAlready CheckinState = "already"
	// CheckinStateInactive 活动未开启/未开放/已结束等非错误态。
	CheckinStateInactive CheckinState = "inactive"
	// CheckinStateFailed 真失败。
	CheckinStateFailed CheckinState = "failed"
)

// CheckinReceipt 签到回执。CreditKnown 为 false 表示上游没带回本次签到积分，
// 调用方不得显示数字（既不补 0 也不硬编码）。
type CheckinReceipt struct {
	State       CheckinState
	Credit      int64
	CreditKnown bool
	StreakDays  int64
	IsStreakDay bool
	Message     string
}

// DailyCheckinReceipt 执行每日签到并返回回执。与 DailyCheckin 的差别只是把上游
// 回执读出来：成功回执是平铺 {credit, streak_days, is_streak_day}，失败回执是
// HTTP 400 信封，两者只有原始 body 拿得到（见 checkinRaw）。
//
// 第二个返回值只承载传输层/瞬时错误（5xx 重试耗尽、连接中断、ctx 取消）——业务
// 拒绝（已签到/未开启/失败）一律装在回执的 State 里、error 为 nil。调用方若要判断
// 「是否值得补签」，看这个 error（upstream.IsTransientBillingErr）。
func (c *Client) DailyCheckinReceipt(ctx context.Context, account *auth.Auth) (CheckinReceipt, error) {
	var (
		raw    []byte
		status int
	)

	err := c.retryBillingTransient(ctx, func() error {
		var reqErr error

		raw, status, reqErr = c.checkinRaw(ctx, account, c.checkinHeaders(account))
		if reqErr != nil {
			return reqErr
		}

		if status >= http.StatusInternalServerError {
			// 5xx 是瞬时态：转成 ErrServer 交给有界重试；耗尽后按失败回执收尾。
			return &Error{
				Kind: ErrServer, Status: status,
				Msg: truncate(string(raw), 200), Code: businessCode(string(raw)),
			}
		}

		return nil
	})
	if err != nil {
		return CheckinReceipt{State: CheckinStateFailed}, err
	}

	return checkinReceipt(status, raw), nil
}

// checkinRaw 发签到请求并返回原始 body 与状态码：不解信封、不按状态码报错——成功
// 回执是平铺对象、失败回执是 HTTP 400 信封，两条都只能从原始 body 取。候选路径
// 仍按 404 换下一条（与 billingMeterJSONLimitHeaders 同口径）。
func (c *Client) checkinRaw(
	ctx context.Context, account *auth.Auth, headers map[string]string,
) ([]byte, int, error) {
	paths := checkinMeterPaths()

	var lastErr error

	for i, path := range paths {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.billingBase(account)+path,
			strings.NewReader("{}"))
		if err != nil {
			return nil, 0, fmt.Errorf("checkinRaw: %w", err)
		}

		c.applyHeaders(surfaceBilling, req, account, "", ChatMeta{})

		for k, v := range headers {
			req.Header.Set(k, v)
		}

		raw, status, err := doRawResponse(c.HTTPForAccount(account), req, defaultJSONBodyLimit)
		if err != nil {
			return nil, status, err
		}

		if status == http.StatusNotFound && i < len(paths)-1 {
			lastErr = &Error{Kind: ErrNotFound, Status: status, Msg: truncate(string(raw), 200)}

			continue
		}

		return raw, status, nil
	}

	return nil, 0, lastErr
}

// checkinReceipt 把「状态码 + 原始 body」转成回执。
//
// 上游两种形态：成功是平铺对象（没有 {code,msg,data} 信封），失败是 HTTP 400 +
// 信封；也有 200 + code!=0 的业务拒绝。三条都先按码/文案定状态，成功才读积分。
func checkinReceipt(status int, raw []byte) CheckinReceipt {
	var env struct {
		Code json.Number     `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}

	parsed := json.Unmarshal(raw, &env) == nil

	code := ""
	if parsed {
		code = env.Code.String()
	}

	msg := cleanCheckinMessage(env.Msg)

	if status < http.StatusBadRequest && (code == "" || code == "0") {
		if receipt, ok := checkinTextOnlyReceipt(msg); ok {
			return receipt
		}

		if receipt, ok := checkinSuccessReceipt(raw, env.Data); ok {
			return receipt
		}

		// 2xx 但回执读不出来（空 body / 反代与 WAF 的 HTML 页 / 半截 JSON）：不能
		// 当成新签到成功——否则会标记当日已签、面板报「签到成功」，账号实际没签。
		return CheckinReceipt{
			State:   CheckinStateFailed,
			Message: fmt.Sprintf("签到回执无法解析（HTTP %d）", status),
		}
	}

	state := checkinState(classifyCheckinResult(code, msg))
	if state == CheckinStateChecked {
		// 非 2xx 或业务码非 0 不可能是新签到：分类缺省值只在这里回落失败。
		state = CheckinStateFailed
	}

	if state == CheckinStateFailed && msg == "" {
		msg = fmt.Sprintf("签到失败（HTTP %d）", status)
	}

	return CheckinReceipt{State: state, Message: msg}
}

// checkinTextOnlyReceipt 处理「成功码 + 拒绝文案」的 2xx 形态：上游把幂等/失败塞进
// 200 + 文案时按文案走，不当成新签到（issue #511）。无文案可依时返回 ok=false。
func checkinTextOnlyReceipt(msg string) (CheckinReceipt, bool) {
	switch {
	case matchCheckinText(msg, checkinFailedMarkers):
		return CheckinReceipt{State: CheckinStateFailed, Message: msg}, true
	case matchCheckinText(msg, alreadyCheckinMarkers):
		return CheckinReceipt{State: CheckinStateAlready, Message: msg}, true
	case matchCheckinText(msg, inactiveCheckinMarkers):
		return CheckinReceipt{State: CheckinStateInactive, Message: msg}, true
	}

	return CheckinReceipt{}, false
}

// checkinSuccessReceipt 读成功回执：官方是平铺 {credit, streak_days, is_streak_day}；
// 若上游把回执包在 data 信封里也照读。积分缺失只置 CreditKnown=false，不补 0。
// 第二个返回值报告回执是否真的读成了 JSON 对象：读不成时由调用方判失败，
// 不能当成新签到（空 body / HTML 页 / 半截 JSON 都会落到这里）。
func checkinSuccessReceipt(raw, data json.RawMessage) (CheckinReceipt, bool) {
	body := raw
	if len(data) > 0 && data[0] == '{' {
		body = data
	}

	if !strings.HasPrefix(strings.TrimSpace(string(body)), "{") {
		return CheckinReceipt{}, false
	}

	var flat struct {
		Credit      *json.Number `json:"credit"`
		StreakDays  int64        `json:"streak_days"`
		IsStreakDay bool         `json:"is_streak_day"`
	}

	if json.Unmarshal(body, &flat) != nil {
		return CheckinReceipt{}, false
	}

	receipt := CheckinReceipt{State: CheckinStateChecked}
	receipt.StreakDays = flat.StreakDays
	receipt.IsStreakDay = flat.IsStreakDay

	if flat.Credit != nil {
		if credit, err := flat.Credit.Int64(); err == nil {
			receipt.Credit = credit
			receipt.CreditKnown = true
		}
	}

	return receipt, true
}

// checkinState 把分类映射到回执状态（CheckinOK 归为 checked）。
func checkinState(result CheckinResult) CheckinState {
	switch result {
	case CheckinOK:
		return CheckinStateChecked
	case CheckinAlready:
		return CheckinStateAlready
	case CheckinInactive:
		return CheckinStateInactive
	default:
		return CheckinStateFailed
	}
}

// cleanCheckinMessage 只保留可读的上游文案：空串、原始 JSON、带 requestId 的
// 诊断串一律丢弃（面板不得回显上游原文与请求号）。
func cleanCheckinMessage(msg string) string {
	msg = strings.TrimSpace(msg)
	if msg == "" || strings.HasPrefix(msg, "{") ||
		strings.Contains(msg, `"requestId"`) || strings.Contains(msg, "upstream client (") {
		return ""
	}

	return truncate(msg, 200)
}
