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
	"net/http"
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
// code: 500"）、网络层错误（非 *Error 的传输失败），以及 429「请求处理中」
// 这类瞬时状态。其余业务错误（已签到/参数错/余额耗尽）不重试。
func isTransientBillingErr(err error) bool {
	if err == nil {
		return false
	}

	if ue, ok := errors.AsType[*Error](err); ok {
		if ue.Kind == ErrServer {
			return true
		}

		return ue.Kind == ErrSoftRate && containsAnyDual(strings.ToLower(ue.Msg), ue.Msg, billingProcessingMarkers)
	}

	return true
}

// retryBillingTransient 对签到/余额这类低频维护调用做瞬时错误有界重试：
// 最多补打 2 次（间隔 2s、4s），首次成功或非瞬时错误立即返回。chat 热路径
// 不用本策略——它有自己的换号轮转语义，重试会放大在途请求。
func (c *Client) retryBillingTransient(fn func() error) error {
	err := fn()
	if err == nil || !isTransientBillingErr(err) {
		return err
	}

	delay := c.retryDelay()
	for i := 1; i <= 2; i++ {
		time.Sleep(time.Duration(i) * delay)

		if err = fn(); err == nil || !isTransientBillingErr(err) {
			return err
		}
	}

	return err
}

// chatRequestEvent 客户端 chat_request_send 事件形状（对齐 CN 5.9.3 的
// `reportChatRequestSendTelemetry()`，issue #366）。
//
// 与 5.8.x 的差异：删掉 maxToken / maxSteps / temperature / maxRetries / skillCount /
// totalCount（5.9.3 不再发）；新增 isMaxMode（网关无 thinking 覆盖状态，按官方默认
// false）以及 source / inputAttemptId / connectorId（官方是条件发，网关取不到就整键
// 不发，不伪造）。
//
// fileUri / vcs* 的具体形态由 VcsInfoProcessor 决定：它拿事件里的 fileUri 当工作区
// 路径，**只有 fileUri 是非空字符串时**才写 vcsType / vcsRepo / vcsBranchName /
// vcsRevId 并删掉 fileUri（`if(e&&"string"==typeof e)try{…}finally{delete e.fileUri}`）。
// 网关没有工作区，fileUri 为空串，处理器不会改写——所以官方在这一形态下发的是
// `fileUri: ""` 且**没有** vcs* 四键；发四个空串 vcs 键反而是官方从不出现的组合。
//
// agentType 只可能是 main / subagent / team，网关模拟本地主会话故发 main；
// agentName 取官方 `agentName || meta.agentName` 的空值形态（不写死 "default"）。
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
	Source                string `json:"source,omitempty"`
	MentionContexts       []any  `json:"mentionContexts"`
	KnowledgeID           []any  `json:"knowledgeId"`
	KnowledgeName         []any  `json:"knowledgeName"`
	CodebaseID            string `json:"codebaseId"`
	MentionContextCount   int    `json:"mentionContextCount"`
	Command               string `json:"command"`
	ExpertID              string `json:"expertId"`
	RecommendID           string `json:"recommendId"`
	SkillID               string `json:"skillId"`
	FileURI               string `json:"fileUri"`
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

	raw, err := json.Marshal([]chatRequestEvent{
		newChatRequestEvent(account2, conversationID, requestID, modelID, modelName),
	})
	if err != nil {
		return fmt.Errorf("ReportChatActivityModel: %w", err)
	}

	_, err = c.reportJSON(account2, json.RawMessage(raw))

	return err
}

// newChatRequestEvent 构造 chat_request_send 事件；字段为零值即为官方默认。
// fileUri 恒发空串：网关没有工作区，官方在该形态下也不会产生 vcs* 四键。
func newChatRequestEvent(
	account *auth.Auth, conversationID, requestID, modelID, modelName string,
) chatRequestEvent {
	now := time.Now().UnixMilli()

	return chatRequestEvent{
		EventCode:             "chat_request_send",
		Timestamp:             now,
		ReportDelay:           0,
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
		// 网关没有 thinking 覆盖状态，取官方默认 false。
		IsMaxMode: false,
		// source / inputAttemptId / connectorId：官方条件发，网关无对应状态 → 不发。
		MentionContexts:      []any{},
		KnowledgeID:          []any{},
		KnowledgeName:        []any{},
		CodebaseID:           "",
		MentionContextCount:  0,
		Command:              "",
		ExpertID:             "",
		RecommendID:          "",
		SkillID:              "",
		FileURI:              "",
		PresentAt:            now,
		TraceID:              requestID,
		RootRequestID:        requestID,
		ParentConversationID: conversationID,
		// 网关没有 agent 上下文：与官方空值形态一致，不写死默认名。
		AgentName: "",
		AgentType: "main",
		UserID:    account.UID,
	}
}
