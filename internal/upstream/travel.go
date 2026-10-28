// travel.go growth 域「猫猫旅行」接口：状态查询 / 派出 / 领奖 / 领养 / 协议。
// 全部走 chatBase + BillingHeaders，信封同 doJSON；路径默认 /v2 优先、404 回落旧路径。
package upstream

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// growth 域路径（实测）。
const (
	travelStatusPath   = "/activity/growth/buddy/travel/status"
	travelDepartPath   = "/activity/growth/buddy/travel/depart"
	travelClaimPath    = "/activity/growth/buddy/travel/claim"
	buddyInfoPath      = "/activity/growth/buddy/info"
	buddyFirstPath     = "/activity/growth/buddy/first"
	buddyAgreementPath = "/activity/growth/buddy/agreement"
	streakPath         = "/activity/growth/streak"
)

// buddyTaskIncompleteMarker 领养门槛未达标的业务错误关键词（HTTP 400 时出现）。
const buddyTaskIncompleteMarker = "first_buddy task not completed yet"

// Buddy 账号当前猫档案；nil（data.buddy 为 null）表示无猫。
type Buddy struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// TravelState 猫猫旅行状态。
type TravelState struct {
	State             string `json:"state"`               // idle / traveling / arrived
	DailyLimitReached bool   `json:"daily_limit_reached"` // 今日已派出过（自然日 00:00 CST 重置）
	RecordID          int64  `json:"record_id"`           // 在途/到站记录 id；到站判据，claim 只作 400 兜底
	RewardCredit      int64  `json:"reward_credit"`       // 到站可领奖励积分
}

// growthJSON 发 growth 域请求并解信封；body 为 nil 时不带请求体。
// /v2 前缀优先（官方 Bearer 通道），仅 404 时回落无 /v2 旧路径。
// 错误语义与 doJSON 一致：HTTP 非 2xx / 业务 code != 0 → *Error。
func (c *Client) growthJSON(account *auth.Auth, method, path string, body any) (json.RawMessage, error) {
	// 已带 /v2 的路径（任务族）直接使用，避免拼成 /v2/v2/...；它们没有旧路径可回落。
	if strings.HasPrefix(path, "/v2/") {
		return c.growthJSONPath(account, method, path, body, nil)
	}

	data, err := c.growthJSONPath(account, method, "/v2"+path, body, nil)
	if err == nil {
		return data, nil
	}

	var ue *Error
	if !errors.As(err, &ue) || ue.Kind != ErrNotFound {
		return nil, err
	}

	return c.growthJSONPath(account, method, path, body, nil)
}

// growthJSONPath 单路径 growth 请求，可附加额外请求头。
func (c *Client) growthJSONPath(
	account *auth.Auth, method, path string, body any, headers map[string]string,
) (json.RawMessage, error) {
	return c.domainJSONLimitHeaders(
		account, c.chatBase(account), method, path, body, defaultJSONBodyLimit, headers,
	)
}

// TravelStatus 查询猫猫旅行状态。
func (c *Client) TravelStatus(a *auth.Auth) (*TravelState, error) {
	data, err := c.growthJSON(a, http.MethodGet, travelStatusPath, nil)
	if err != nil {
		return nil, err
	}

	var st TravelState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("TravelStatus: %w", err)
	}

	return &st, nil
}

// TravelDepart 派出猫旅行；locationID 实测 1~4（收益/时长区间相同）。
func (c *Client) TravelDepart(a *auth.Auth, locationID int) error {
	_, err := c.growthJSON(a, http.MethodPost, travelDepartPath, map[string]any{"location_id": locationID})
	return err
}

// TravelClaim 领取到站奖励，返回 reward_credit。
//
// 首选空对象 body（`{}`）：官方桌面 claimTravelReward 固定
// `http.post('/activity/growth/buddy/travel/claim', {}, productHeaders)`（CN 5.9.3
// main/server.js@337115 与 @422336，5.8.3 同形），是**空 JSON 对象**而不是零字节
// body——Content-Length 与 body 都要按 `{}` 发，否则上游 JSON 解析会 400。
// 本仓首发请求因此也不带 record_id。`{"record_id":N}` 是历史形态，静态包内不存在，
// 仅作 400 兜底：触发条件是上游坚持要 record_id。
//
// 请求顺序（三跳，最多 3 次）：
//  1. POST /v2 + travelClaimPath，body `{}`；
//  2. 仅当上一跳是 404（路径不存在）时换成无前缀 travelClaimPath，body 仍为 `{}`；
//  3. 仅当最后一跳是 400（参数错误）且调用方有 record_id 时，在最后一次用的路径上
//     带 record_id 再试一次。
//
// 多次尝试都失败时返回最后一次错误（避免用更早的 404 / 参数错误覆盖会话失效或
// 服务端错误，让调用方仍能按真实分类处置账号）。
func (c *Client) TravelClaim(account *auth.Auth, recordID int64) (int64, error) {
	headers := map[string]string{"X-Product-Code": "workbuddy"}
	lastPath := "/v2" + travelClaimPath

	data, err := c.growthJSONPath(account, http.MethodPost, lastPath, map[string]any{}, headers)
	if err != nil && isKind(err, ErrNotFound) {
		lastPath = travelClaimPath
		data, err = c.growthJSONPath(account, http.MethodPost, lastPath, map[string]any{}, headers)
	}

	if err != nil && claimNeedsRecordIDBody(err, recordID) {
		// 带 record_id 兜底的成败都覆盖上一次：失败时保留最后一次错误分类
		// （如会话失效/服务端错误），不被更早的参数错误掩盖。
		data, err = c.growthJSONPath(
			account, http.MethodPost, lastPath, map[string]any{"record_id": recordID}, headers,
		)
	}

	if err != nil {
		return 0, err
	}

	var resp struct {
		RewardCredit int64 `json:"reward_credit"`
	}
	if len(data) > 0 {
		// 奖励字段缺失不视为失败：调用方按 0 记日志即可。
		_ = json.Unmarshal(data, &resp)
	}

	return resp.RewardCredit, nil
}

// BuddyInfo 查询当前猫档案；返回 (nil, nil) 表示无猫（data.buddy 为 null）。
func (c *Client) BuddyInfo(a *auth.Auth) (*Buddy, error) {
	data, err := c.growthJSON(a, http.MethodGet, buddyInfoPath, nil)
	if err != nil {
		return nil, err
	}

	var resp struct {
		Buddy json.RawMessage `json:"buddy"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("BuddyInfo: %w", err)
	}
	// null / 缺字段 / 空对象都按无猫处理。
	trimmed := strings.TrimSpace(string(resp.Buddy))
	if trimmed == "" || trimmed == "null" {
		return nil, nil //nolint:nilnil // 无猫状态返回 (nil,nil)，调用方以 nil 判定
	}

	var buddy Buddy
	if err := json.Unmarshal(resp.Buddy, &buddy); err != nil {
		return nil, fmt.Errorf("BuddyInfo: %w", err)
	}
	// 空对象（{}）等价于无猫：id=0 不是合法档案，继续当成有猫会让调度器
	// 多发一次 travel/status 并在 idle 时直接派出。
	if buddy.ID == 0 && strings.TrimSpace(buddy.Name) == "" {
		return nil, nil //nolint:nilnil // 无猫状态返回 (nil,nil)，调用方以 nil 判定
	}

	return &buddy, nil
}

// BuddyFirst 领养第一只猫。无猫且已过 conversation 门槛时送 300 分。
// 门槛未达标返回 HTTP 400（见 IsBuddyTaskIncomplete），属预期行为，调用方静默跳过。
func (c *Client) BuddyFirst(a *auth.Auth) error {
	_, err := c.growthJSON(a, http.MethodPost, buddyFirstPath, map[string]any{})
	return err
}

// BuddyAgreement 同意协议（幂等，重复调用无副作用）。
func (c *Client) BuddyAgreement(a *auth.Auth) error {
	_, err := c.growthJSON(a, http.MethodPost, buddyAgreementPath, map[string]any{"agree": true})
	return err
}

// GrowthStreak 查询连登天数（只读 oracle）。响应 `data.streak.days`（probe_active.py
// 实测口径：`(s.get("data", {}).get("streak", {}) or {}).get("days")`）。
// GET 失败（HTTP 非 2xx / 业务 code != 0）返回 *Error；缺 streak/days 字段返回 0
// （days==0 即活跃自检的「上报 200 但静默丢弃」告警信号）。
func (c *Client) GrowthStreak(a *auth.Auth) (int, error) {
	data, err := c.growthJSON(a, http.MethodGet, streakPath, nil)
	if err != nil {
		return 0, err
	}

	var resp struct {
		Streak struct {
			Days int `json:"days"`
		} `json:"streak"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, fmt.Errorf("GrowthStreak: %w", err)
	}

	return resp.Streak.Days, nil
}

// isKind 判断上游错误分类是否为目标类型。
func isKind(err error, kind ErrKind) bool {
	var upstreamErr *Error

	return errors.As(err, &upstreamErr) && upstreamErr.Kind == kind
}

// claimNeedsRecordIDBody 判定 claim 是否需要带 record_id 兜底重试：仅参数错误（400）
// 且调用方确实拿到了 record_id。
//
// 不认 404：上一跳已把 404 换成无前缀路径再试，两条路径都 404 说明端点不存在，
// 带 record_id 打第三次不可能成功，只会用这次结果覆盖真实的 ErrNotFound 分类。
// record_id 为 0 时同样不兜底：官方形态不需要它，带上 `{"record_id":0}` 只会把
// 「到达但状态里没有 record_id」变成一次注定失败的请求。
func claimNeedsRecordIDBody(err error, recordID int64) bool {
	if recordID == 0 {
		return false
	}

	var upstreamErr *Error
	if !errors.As(err, &upstreamErr) {
		return false
	}

	return upstreamErr.Status == http.StatusBadRequest
}

// IsBuddyTaskIncomplete 判定「领养门槛未达标」：HTTP 400 + first_buddy 关键词。
// 该错误当日不应重试（避免对上游重试轰炸）。
func IsBuddyTaskIncomplete(err error) bool {
	if err == nil {
		return false
	}

	var ue *Error
	if !errors.As(err, &ue) || ue.Status != http.StatusBadRequest {
		return false
	}

	return strings.Contains(strings.ToLower(ue.Msg), buddyTaskIncompleteMarker)
}
