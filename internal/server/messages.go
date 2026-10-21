// messages.go POST /v1/search/messages：Anthropic Messages 形态的官方联网搜索。
//
// 契约（对齐官方 WebSearchTool 的输出形态）：
//   - 入站：`{"model":..., "messages":[...], "tools":[{"type":"web_search_20250305",...}]}`；
//     必须声明 web_search 工具（否则 400，说明这是搜索专用端点）。
//   - 出站：assistant message，content 依次为
//     ① text 块（搜索摘要，带 citations[].cited_text）
//     ② web_search_tool_result 块（逐条结果 title/url/snippet）
//
// 为什么单独做端点而不是给 /v1/messages 注入搜索工具：本 fork 的 `/v1/messages` 对
// 服务端 web_search 工具是**显式拒绝**（本地工具只服务 Responses，见 localwebtools.go），
// 保持那条语义不动，另开一个语义明确的搜索端点更安全（也不会让普通对话悄悄多花钱）。
package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/BegoniaHe/wokbuddy-demon/internal/logfmt"
	"github.com/BegoniaHe/wokbuddy-demon/internal/reqlog"
	"github.com/BegoniaHe/wokbuddy-demon/internal/session"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// anthropicSearchHandler /v1/search/messages 入口（鉴权由 withClientAuth 负责）。
func (h *Handler) anthropicSearchHandler(w http.ResponseWriter, r *http.Request) {
	setAnthropicRequestID(w, r)

	if !h.loadLive().SearchEndpoint {
		writeAnthropicError(w, http.StatusNotFound, "not_found_error",
			"search endpoint is disabled; enable features.search_endpoint to use POST /v1/search/messages", "")

		return
	}

	// 命名密钥绑定出口是运维侧的显式约束：搜索只放行 CN 域，绑定 global 的
	// key 不能在未经授权的情况下消耗 CN 账号额度（与 chat 的 key realm 校验同口径）。
	if keyRealm := keyRealmFrom(r.Context()); keyRealm != "" && keyRealm != searchRealm {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error",
			"search endpoint only supports the cn realm; this API key is bound to "+keyRealm, "")

		return
	}

	limitRequestBody(w, r, h.cfg.MaxBodyBytes)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error(), "")

		return
	}

	req, perr := parseSearchRequest(body)
	if perr != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", perr.Error(), "")

		return
	}

	outcome, uid, fail := h.runSearch(r.Context(), req.query, req.maxResults, &upstream.ChatMeta{})
	// 请求日志记账：搜索路径没有 chatStat，账号/模型/结果显式写进 trace
	// （trace 由 ServeHTTP 在 /v1/search/messages 上创建，开关与其它 POST 接口一致）。
	if fail == nil {
		requestTraceFrom(r).noteResult(logfmt.Label(uid, accountNickname(h, uid)), req.model, reqlog.OutcomeSuccess)
	} else {
		requestTraceFrom(r).noteResult(logfmt.Label(uid, accountNickname(h, uid)), req.model, reqlog.OutcomeHTTPError)
	}

	if fail != nil {
		writeAnthropicError(w, fail.status, searchErrorType(fail.status), fail.msg, searchHint(fail))

		return
	}

	writeJSON(w, http.StatusOK, buildSearchMessage(req.model, req.query, outcome))
}

// searchRequest 解析出的搜索请求要素。
type searchRequest struct {
	model      string
	query      string
	maxResults int
}

// parseSearchRequest 解析并校验入站请求：模型、messages、web_search 工具声明。
func parseSearchRequest(body []byte) (*searchRequest, error) {
	var payload struct {
		Model     string                 `json:"model"`
		MaxTokens int                    `json:"max_tokens"`
		Messages  []searchInboundMessage `json:"messages"`
		Tools     []map[string]any       `json:"tools"`
		// MaxResults 可选：非 0 时覆盖默认条数（上游按 ClampSearchMaxResults 收敛）。
		MaxResults int `json:"max_results"`
	}

	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, errSearchInvalidJSON
	}

	if strings.TrimSpace(payload.Model) == "" {
		return nil, errModelIsRequired
	}

	if len(payload.Messages) == 0 {
		return nil, errMessagesIsRequired
	}

	if !declaresWebSearch(payload.Tools) {
		return nil, errSearchToolRequired
	}

	query := lastUserText(payload.Messages)
	if query == "" {
		return nil, errSearchQueryEmpty
	}

	if len(query) > searchQueryMaxLen {
		return nil, errSearchQueryTooLong
	}

	// 条数：请求体顶层 max_results 优先，其次 tools[].max_results；两处都缺省时用
	// upstream 的默认值（ClampSearchMaxResults 负责收敛到 [1, cap]）。
	maxResults := payload.MaxResults
	if maxResults <= 0 {
		maxResults = toolMaxResults(payload.Tools)
	}

	return &searchRequest{model: payload.Model, query: query, maxResults: maxResults}, nil
}

// searchQueryMaxLen 搜索词长度上限（防御性：上游 zod 无上限，超长 query 只会让
// 上游超时并白占一次在途名额；1024 字符已远超任何真实搜索词）。
const searchQueryMaxLen = 1024

// toolMaxResults 取 web_search 工具声明里的 max_results（缺省/非法返回 0 = 用默认）。
func toolMaxResults(tools []map[string]any) int {
	for _, tool := range tools {
		name, _ := tool["name"].(string)
		kind, _ := tool["type"].(string)

		if name != "web_search" && !strings.HasPrefix(kind, "web_search") {
			continue
		}

		switch value := tool["max_results"].(type) {
		case float64:
			return int(value)
		case int:
			return value
		}
	}

	return 0
}

// declaresWebSearch 报告请求是否声明了 web_search 工具（官方形态：
// {"type":"web_search_20250305","name":"web_search"}；也接受 name=web_search 的简写）。
func declaresWebSearch(tools []map[string]any) bool {
	for _, tool := range tools {
		name, _ := tool["name"].(string)
		if name == "web_search" {
			return true
		}

		kind, _ := tool["type"].(string)
		if strings.HasPrefix(kind, "web_search") {
			return true
		}
	}

	return false
}

// searchInboundMessage 入站消息。content 保留原始 JSON：Anthropic Messages 允许
// content 是块数组，也允许是纯字符串（两种都是合法入站形态），用固定结构体会让
// 字符串形态在解析阶段直接失败并被误报成「invalid JSON body」。
type searchInboundMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// messageTextParts 取一条消息的文本部分：块数组只认 text 块（忽略 tool_result
// 之类的历史块）；纯字符串整段即文本；无法识别的形态返回 nil（不参与搜索词）。
func messageTextParts(content json.RawMessage) []string {
	var blocks []map[string]any
	if err := json.Unmarshal(content, &blocks); err == nil {
		var parts []string

		for _, block := range blocks {
			kind, _ := block["type"].(string)
			if kind != "" && kind != "text" {
				continue
			}

			if text, ok := block["text"].(string); ok && strings.TrimSpace(text) != "" {
				parts = append(parts, strings.TrimSpace(text))
			}
		}

		return parts
	}

	var text string
	if err := json.Unmarshal(content, &text); err == nil {
		if trimmed := strings.TrimSpace(text); trimmed != "" {
			return []string{trimmed}
		}
	}

	return nil
}

// lastUserText 取最后一条 user 消息里的文本作为搜索词（块数组取全部 text 块，
// 纯字符串取其本身）。
func lastUserText(messages []searchInboundMessage) string {
	for _, message := range slices.Backward(messages) {
		if message.Role != "user" {
			continue
		}

		if parts := messageTextParts(message.Content); len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}

	return ""
}

// buildSearchMessage 组装 Anthropic Messages 形态的响应：摘要 text 块（带 citations）
// + web_search_tool_result 块（逐条结果）。
func buildSearchMessage(model, query string, outcome *upstream.SearchOutcome) map[string]any {
	queryBytes := len(query)
	citations, results, summary := buildSearchBlocks(outcome)

	return map[string]any{
		"id":    "msg_" + session.NewMessageID(),
		"type":  "message",
		"role":  "assistant",
		"model": model,
		"content": []map[string]any{
			{"type": "text", "text": summary.String(), "citations": citations},
			{
				"type":    "web_search_tool_result",
				"content": results,
			},
		},
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		// usage 是估算值（网关没有 tokenizer）：input 按搜索词字节/4、output 按摘要
		// 字节/4 粗估。客户端只用它做上下文预算，不作为计费依据（与 count_tokens 同口径）。
		"usage": map[string]any{
			"input_tokens":  max(1, queryBytes/4),
			"output_tokens": max(1, summary.Len()/4),
		},
	}
}

// accountNickname 取账号昵称（记账展示用；账号已移除时返回空串）。
func accountNickname(h *Handler, uid string) string {
	if uid == "" {
		return ""
	}

	if acct := h.cfg.Pool.AuthByUID(uid); acct != nil {
		return acct.NicknameValue()
	}

	return ""
}

// buildSearchBlocks 把搜索结果折成 Anthropic content 需要的三样东西：
// citations（text 块）、web_search_result（tool_result 块）与摘要文本。
func buildSearchBlocks(outcome *upstream.SearchOutcome) (citations, results []map[string]any, summary strings.Builder) {
	citations = make([]map[string]any, 0, len(outcome.Results))
	results = make([]map[string]any, 0, len(outcome.Results))

	for i := range outcome.Results {
		result := &outcome.Results[i]

		cited := result.Snippet
		if cited == "" {
			cited = result.Title
		}

		citations = append(citations, map[string]any{
			"type":            "web_search_result_location",
			"url":             result.URL,
			"title":           result.Title,
			"cited_text":      cited,
			"encrypted_index": "",
		})
		results = append(results, map[string]any{
			"type":    "web_search_result",
			"title":   result.Title,
			"url":     result.URL,
			"snippet": result.Snippet,
		})

		if summary.Len() > 0 {
			summary.WriteString("\n")
		}

		summary.WriteString("- ")
		summary.WriteString(result.Title)

		if result.Snippet != "" {
			summary.WriteString("：")
			summary.WriteString(result.Snippet)
		}
	}

	if summary.Len() == 0 {
		summary.WriteString("未找到与该查询相关的网页结果。")
	}

	return citations, results, summary
}

// searchHint 给搜索失败补一条客户端可操作的提示（上游原文仍在 message 里透传）。
// 15001「联网搜索次数用尽」是账号级额度，换号可能仍有额度——提示写明这一点。
func searchHint(fail *searchFailure) string {
	if strings.Contains(fail.msg, "15001") {
		return "search quota exhausted for the current account; the gateway may retry on another account"
	}

	return fail.hint
}

// searchErrorType 把终端状态码映射成 Anthropic 错误类型。
func searchErrorType(status int) string {
	switch {
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status >= 500:
		return "overloaded_error"
	case status >= 400:
		return "invalid_request_error"
	default:
		return "api_error"
	}
}

// 搜索端点专用错误（面向开发者，中文可读）。
var (
	errSearchInvalidJSON  = errors.New("invalid JSON body")
	errSearchToolRequired = errors.New(
		"该端点只用于联网搜索：请求必须在 tools 中声明 web_search 工具")
	errSearchQueryEmpty   = errors.New("请在 messages 中提供一段用户文本作为搜索词")
	errSearchQueryTooLong = errors.New("搜索词过长（上限 1024 字符），请精简后重试")
)
