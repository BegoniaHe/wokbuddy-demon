// report.go billing 域「对话活跃上报」接口：POST {billingBase}/v2/report。
// 照抄客户端 chat_request_send 事件形状（含 conversationId/mode/inputLength 等全字段，
// 勿用最小 3 字段，防上游后续加严）。事件必须带 userId（=账号 uid），缺失则服务端
// 200 但静默丢弃（实测见 REPORT-active-map.md §2）。
//
// 一条上报同时点亮 growth 连登 + 解锁 first_buddy 任务（领养前置）。
// 风控口径：每号每天 1 次即可（activity_hours 单时点），不做多时点高频上报。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/session"
)

// reportPath 活跃上报通道（实测）。默认走 CN billing 域（workbuddy.cn，可用
// cn.billing_base 退回 codebuddy.cn）；桌面 /v2/report 走 chatBase（desktopBase），
// Web 上报才走 webBase——三者共用该路径但是不同 surface。
const reportPath = "/v2/report"

// billingJSON 发 billing 域（billingBase，workbuddy.cn）请求并解信封；body 为 nil 时不带请求体。
// 与 travel.go 的 growthJSON 对称（growth 域走 chatBase + growth 头族；billing 域走 billingBase）。
// checkin / balance 等 billing 端点共用：请求头统一 BillingHeaders，信封与错误语义同 doJSON。
// 活跃上报走 reportJSON（surfaceReport 头族），不在此列。
func (c *Client) billingJSON(a *auth.Auth, path string, body any) (json.RawMessage, error) {
	return c.billingJSONLimit(a, path, body, defaultJSONBodyLimit)
}

// billingJSONLimit 同 billingJSON，允许调用方指定响应体上限（逐笔用量页含 prompt，
// 单页 1000 条可超过 1 MiB）。
func (c *Client) billingJSONLimit(
	a *auth.Auth, path string, body any, maxBytes int64,
) (json.RawMessage, error) {
	return c.billingJSONLimitHeaders(a, path, body, maxBytes, nil)
}

// billingJSONLimitHeaders 同 billingJSONLimit，额外注入调用方指定的请求头。
func (c *Client) billingJSONLimitHeaders(
	a *auth.Auth, path string, body any, maxBytes int64, headers map[string]string,
) (json.RawMessage, error) {
	return c.domainJSONLimitHeaders(
		a, surfaceBilling, c.billingBase(a), http.MethodPost, path, body, maxBytes, headers,
	)
}

// reportJSON 活跃上报面（POST {billingBase}/v2/report）请求：surfaceReport 头族 +
// billing 域 base，body 为事件数组。
func (c *Client) reportJSON(a *auth.Auth, body any) (json.RawMessage, error) {
	return c.domainJSONLimitHeaders(
		a, surfaceReport, c.billingBase(a), http.MethodPost, reportPath, body, defaultJSONBodyLimit, nil,
	)
}

// domainJSONLimitHeaders 对指定 base 域发请求并解信封，额外指定响应体上限与
// 调用方指定的请求头。头族按 surface 由 applyHeaders 构造。
func (c *Client) domainJSONLimitHeaders(
	account *auth.Auth, srf surface, base, method, path string, body any, maxBytes int64,
	headers map[string]string,
) (json.RawMessage, error) {
	var rdr io.Reader

	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("domainJSONLimit: %w", err)
		}

		rdr = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(context.Background(), method, base+path, rdr)
	if err != nil {
		return nil, fmt.Errorf("domainJSONLimit: %w", err)
	}

	c.applyHeaders(srf, req, account, "", ChatMeta{})

	for k, v := range headers {
		req.Header.Set(k, v)
	}

	return c.doJSONLimitFor(account, req, maxBytes)
}

// billingMeterJSON 仅对 /billing/meter 族端点（get-user-resource / summary /
// packages / daily-checkin）按候选序列做双路径 fallback：两 realm 都是无前缀优先
// （官方 5.9.3 桌面端 resourcePrefix 为空串），ErrNotFound 时换 `/v2` 前缀。
// 签到是例外：候选由 checkinMeterPaths 给出，仍是 `/v2` 优先。
func (c *Client) billingMeterJSON(a *auth.Auth, paths []string, body any) (json.RawMessage, error) {
	return c.billingMeterJSONLimit(a, paths, body, defaultJSONBodyLimit)
}

// billingMeterJSONHeaders 同 billingMeterJSON，额外给每个候选路径注入 headers
// （summary / packages 的 Accept-Language、签到的 X-Device-Token）。
func (c *Client) billingMeterJSONHeaders(
	a *auth.Auth, paths []string, body any, headers map[string]string,
) (json.RawMessage, error) {
	return c.billingMeterJSONLimitHeaders(a, paths, body, defaultJSONBodyLimit, headers)
}

// billingMeterJSONLimit 同 billingMeterJSON，允许调用方指定响应体上限。
func (c *Client) billingMeterJSONLimit(
	a *auth.Auth, paths []string, body any, maxBytes int64,
) (json.RawMessage, error) {
	return c.billingMeterJSONLimitHeaders(a, paths, body, maxBytes, nil)
}

// billingMeterJSONLimitHeaders 同 billingMeterJSONLimit，额外注入调用方指定的请求头。
func (c *Client) billingMeterJSONLimitHeaders(
	a *auth.Auth, paths []string, body any, maxBytes int64, headers map[string]string,
) (json.RawMessage, error) {
	var lastErr error

	for i, path := range paths {
		data, err := c.billingJSONLimitHeaders(a, path, body, maxBytes, headers)
		if err == nil {
			return data, nil
		}

		lastErr = err
		// 仅 404 换路径（路径不存在才值得 fallback）；其他错误直接返回。
		var ue *Error
		if !errors.As(err, &ue) || ue.Kind != ErrNotFound || i == len(paths)-1 {
			return nil, err
		}
	}

	return nil, lastErr
}

// billingRetryDelay 签到/余额等维护类计费调用瞬时错误重试的间隔基数默认值
// （生产固定 2s：第 1 次重试等 2s、第 2 次等 4s）。测试可经 Client.RetryDelay 注入
// 更短间隔（跨包测试无法改本包变量，issue #20）。
var billingRetryDelay = 2 * time.Second

// retryDelay 返回生效的重试间隔基数：Client.RetryDelay 非正时回落包级默认。
func (c *Client) retryDelay() time.Duration {
	if c.RetryDelay > 0 {
		return c.RetryDelay
	}

	return billingRetryDelay
}

// billingProcessingMarkers 上游 429「请求处理中」文案（签到/余额瞬时状态）。
// 该状态是上游正在处理上一笔请求，重试通常可成功；不能按普通业务错误丢掉，
// 否则会造成漏签（issue #250）。
var billingProcessingMarkers = []string{"请求处理中", "processing"}

// isTransientBillingErr 报告 err 是否值得对计费维护类调用做有界重试：
// 上游 5xx（ErrServer，实测偶发 "code 10000 / API request failed with status
// code: 500"）、网络层错误，以及 429「请求处理中」这类瞬时状态。其余业务错误
// （已签到/参数错/余额耗尽）与本地确定性错误不重试。
//
// 取消/超时（context.Canceled / DeadlineExceeded）先判掉：它们是本地信号而不是上游
// 抖动，重试只会白等退避并补打上游（issue #423）。注意 context.DeadlineExceeded 自身
// 也实现 net.Error，所以这一步必须排在网络层判定之前。
//
// 非 *Error 的错误只放行明确的传输层错误（*url.Error / net.Error）；解码失败、
// 参数校验等本地确定性错误一律不重试。
func isTransientBillingErr(err error) bool {
	if err == nil {
		return false
	}

	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	if ue, ok := errors.AsType[*Error](err); ok {
		if ue.Kind == ErrServer {
			return true
		}

		return ue.Kind == ErrSoftRate && containsAnyDual(strings.ToLower(ue.Msg), ue.Msg, billingProcessingMarkers)
	}

	if _, ok := errors.AsType[*url.Error](err); ok {
		return true
	}

	var netErr net.Error

	return errors.As(err, &netErr)
}

// retryBillingTransient 对签到/余额这类低频维护调用做瞬时错误有界重试：
// 最多补打 2 次（间隔 2s、4s），首次成功或非瞬时错误立即返回。chat 热路径
// 不用本策略——它有自己的换号轮转语义，重试会放大在途请求。
//
// ctx 传入调用方的请求/任务上下文：等待被取消时立即返回 ctx.Err()，不再补打上游。
func (c *Client) retryBillingTransient(ctx context.Context, fn func() error) error {
	err := fn()
	if err == nil || !isTransientBillingErr(err) {
		return err
	}

	delay := c.retryDelay()
	for i := 1; i <= 2; i++ {
		if !sleepCtx(ctx, time.Duration(i)*delay) {
			return ctx.Err()
		}

		if err = fn(); err == nil || !isTransientBillingErr(err) {
			return err
		}
	}

	return err
}

// sleepCtx 可取消等待：ctx 取消立即返回 false，睡满返回 true。d<=0 直接按 ctx
// 当前状态放行（与 scheduler / server 的同名辅助同口径）。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}

	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// chatRequestEvent 客户端 chat_request_send 事件形状（对齐 CN 5.9.3 的
// `reportChatRequestSendTelemetry()`，issue #366）。
//
// 与 5.8.x 的差异：删掉 maxToken / maxSteps / temperature / maxRetries / skillCount /
// totalCount（5.9.3 不再发）；补 isMaxMode。
//
// 条件字段按客户端控制流整键省略，不照抄单份真机样本：
//   - `source`：官方 `...typeof meta.pendingSource=="string"?{source}:{}`，只有从
//     特定入口发起才带；网关没有 pendingSource 状态，不发该键。
//   - `inputAttemptId`：官方 `...s?.inputAttemptId?{inputAttemptId}:{}`，只由技能
//     推荐链路携带；网关无该上下文，不发该键（同段 recommendId / skillId 才是
//     `??""` 无条件发）。
//   - `fileUri` / `vcs*`：官方 fileUri 取 `workspaceFolders?.[0]?.uri?.fsPath`，
//     无工作区时是空串；`VcsInfoProcessor` 只在 fileUri 非空时写 vcs 四键并删除
//     fileUri（`if(i&&"string"==typeof i) try{…} finally{delete fileUri}`），空串
//     则原样保留。网关没有工作区，故发 `fileUri: ""` 且四键缺席。
//
// isMaxMode 取 `getOverrideThinking(id)?.thinkingEnabled===true`，普通会话即 false；
// 网关没有 thinking 覆盖状态，按官方默认值发 false。
//
// agentType 只可能是 main / subagent / team，网关模拟本地主会话故发 main；
// agentName 取桌面内置 CLI 的 "cli"（与 chat_message_send 同口径）。
// userId 为必填字段（= a.UID）；conversationId 由调用方生成，无需真实会话。
type chatRequestEvent struct {
	EventCode             string `json:"eventCode"`
	Timestamp             int64  `json:"timestamp"`
	ReportDelay           int    `json:"reportDelay"`
	Mode                  string `json:"mode"`
	ConversationID        string `json:"conversationId"`
	RequestID             string `json:"requestId"`
	InputLength           int    `json:"inputLength"`
	RequestModelID        string `json:"requestModelId"`
	RequestModelName      string `json:"requestModelName"`
	IsPlan                bool   `json:"isPlan"`
	IsAutoExecuteTerminal bool   `json:"isAutoExecuteTerminal"`
	IsAutoModify          bool   `json:"isAutoModify"`
	CodebaseEnable        bool   `json:"codebaseEnable"`
	IsMaxMode             bool   `json:"isMaxMode"`
	MentionContexts       []any  `json:"mentionContexts"`
	KnowledgeID           []any  `json:"knowledgeId"`
	KnowledgeName         []any  `json:"knowledgeName"`
	CodebaseID            string `json:"codebaseId"`
	MentionContextCount   int    `json:"mentionContextCount"`
	Command               string `json:"command"`
	FileURI               string `json:"fileUri"`
	RecommendID           string `json:"recommendId"`
	SkillID               string `json:"skillId"`
	PresentAt             int64  `json:"presentAt"`
	TraceID               string `json:"traceId"`
	RootRequestID         string `json:"rootRequestId"`
	ParentConversationID  string `json:"parentConversationId"`
	AgentName             string `json:"agentName"`
	AgentType             string `json:"agentType"`
	UserID                string `json:"userId"`
}

// ReportChatActivity 向上游发送一条对话活跃上报（chat_request_send）。
// conversationID 由调用方生成（UUIDv4），无需真实会话——服务端不校验一致性。
// requestID 为本轮请求独立标识（多轮同会话上报时各条不同）；空时生成新的 UUIDv7。
// 错误语义与 doJSON 一致：HTTP 非 2xx / 业务 code != 0 → *Error。
func (c *Client) ReportChatActivity(a *auth.Auth, conversationID, requestID string) error {
	return c.ReportChatActivityModel(a, conversationID, requestID, "deepseek-v4-flash", "DeepSeek V4 Flash")
}

// ReportChatActivityModel 同上，但可指定上报携带的模型：供「体验某模型」类任务
// 对齐实际模型（如 Model_chat_GLM5.2 需 requestModelId=glm-5.2 与独立 requestID）。
func (c *Client) ReportChatActivityModel(
	account2 *auth.Auth, conversationID, requestID, modelID, modelName string,
) error {
	if requestID == "" {
		requestID = session.NewUUIDv7Hex()
	}

	if modelID == "" {
		modelID = "deepseek-v4-flash"
	}

	if modelName == "" {
		modelName = modelID
	}

	activity := newChatRequestEvent(account2, conversationID, requestID, modelID, modelName)

	event, err := c.chatRequestEventMap(account2, &activity)
	if err != nil {
		return fmt.Errorf("ReportChatActivityModel: %w", err)
	}

	raw, err := json.Marshal([]map[string]any{event})
	if err != nil {
		return fmt.Errorf("ReportChatActivityModel: %w", err)
	}

	_, err = c.reportJSON(account2, json.RawMessage(raw))

	return err
}

// chatRequestEventMap 把 chat 专属字段与桌面指纹合成一条完整事件，与官方报告
// 服务的处理链一致：ProductInfo / HostIDEInfo / ExtensionInfo / OS / Userinfo /
// Buddy 各 processor 会把设备与包指纹注入**每个**事件，chat_request_send 不是例外。
// 业务字段优先（后者覆盖同名指纹），与官方 processor 的 `e.userId ||= …` 语义相反
// 的一侧是「事件已带值时保留」，两种写法结果相同。
//
// 不补 turn carrier 的 `codebuddy.session_id` / `codebuddy.conversation_request_id`：
// 官方把它们作为 W3C baggage 的键注入 traceparent/baggage 请求头，从不写进事件体。
// vcs* 也不补——网关没有工作区，四键缺席而 fileUri 保留空串，见 chatRequestEvent
// 的条件字段说明。
func (c *Client) chatRequestEventMap(account *auth.Auth, activity *chatRequestEvent) (map[string]any, error) {
	raw, err := json.Marshal(activity)
	if err != nil {
		return nil, fmt.Errorf("chatRequestEventMap: %w", err)
	}

	fields := make(map[string]any)
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("chatRequestEventMap: %w", err)
	}

	event := c.desktopFingerprint(account)
	maps.Copy(event, fields)

	return event, nil
}

// newChatRequestEvent 构造 chat_request_send 事件；字段为零值即为官方默认。
//
// 时间三件套按官方 debounce 批模型回填：report() 入队时写 timestamp=now、
// reportDelay=0，FIRE_DELAY=2s 后才 flush，computeReportDelay 再把每条改成
// flush-timestamp。因此出站事件是 timestamp≈presentAt（请求发起/入队时刻）、
// reportDelay≈2000、到达≈两者之和；恒 0 只对应 reportSync / 退出前 flush。
func newChatRequestEvent(
	account *auth.Auth, conversationID, requestID, modelID, modelName string,
) chatRequestEvent {
	sentAt := time.Now().UnixMilli() - desktopReportDebounceMs

	return chatRequestEvent{
		EventCode:             "chat_request_send",
		Timestamp:             sentAt,
		ReportDelay:           int(desktopReportDebounceMs),
		Mode:                  "craft",
		ConversationID:        conversationID,
		RequestID:             requestID,
		InputLength:           12,
		RequestModelID:        modelID,
		RequestModelName:      modelName,
		IsPlan:                false,
		IsAutoExecuteTerminal: false,
		IsAutoModify:          false,
		CodebaseEnable:        false,
		// 网关没有 thinking 覆盖状态，取官方默认值 false。
		IsMaxMode:           false,
		MentionContexts:     []any{},
		KnowledgeID:         []any{},
		KnowledgeName:       []any{},
		CodebaseID:          "",
		MentionContextCount: 0,
		Command:             "",
		// 官方无工作区时 fileUri 为空串，VcsInfoProcessor 不写 vcs 四键（空串走
		// 的是 `if(fileUri)` 的假分支，finally 里的 delete 不执行）。
		FileURI:     "",
		RecommendID: "",
		SkillID:     "",
		PresentAt:   sentAt,
		// traceId 是 W3C trace id（随机 v4 形态），与 requestId 的 UUIDv7 不同源。
		TraceID:              session.NewTraceID(),
		RootRequestID:        requestID,
		ParentConversationID: conversationID,
		// 桌面端内置 CLI 主会话：与 chat_message_send 事件同口径。
		AgentName: "cli",
		AgentType: "main",
		UserID:    account.UID,
	}
}
