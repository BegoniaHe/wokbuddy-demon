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
		writeAnthropicError(w, fail.status, searchErrorType(fail.status), fail.msg, fail.hint)

		return
	}

	writeJSON(w, http.StatusOK, buildSearchMessage(req.model, outcome))
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
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
		Messages  []struct {
			Role    string           `json:"role"`
			Content []map[string]any `json:"content"`
		} `json:"messages"`
		Tools []map[string]any `json:"tools"`
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

	return &searchRequest{model: payload.Model, query: query, maxResults: 0}, nil
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

// lastUserText 取最后一条 user 消息里的文本块作为搜索词（Anthropic 入站形态里
// content 是块数组；只认 text 块，忽略 tool_result 之类的历史块）。
func lastUserText(messages []struct {
	Role    string           `json:"role"`
	Content []map[string]any `json:"content"`
},
) string {
	for _, message := range slices.Backward(messages) {
		if message.Role != "user" {
			continue
		}

		var parts []string

		for _, block := range message.Content {
			kind, _ := block["type"].(string)
			if kind != "" && kind != "text" {
				continue
			}

			if text, ok := block["text"].(string); ok && strings.TrimSpace(text) != "" {
				parts = append(parts, strings.TrimSpace(text))
			}
		}

		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}

	return ""
}

// buildSearchMessage 组装 Anthropic Messages 形态的响应：摘要 text 块（带 citations）
// + web_search_tool_result 块（逐条结果）。
func buildSearchMessage(model string, outcome *upstream.SearchOutcome) map[string]any {
	citations := make([]map[string]any, 0, len(outcome.Results))
	results := make([]map[string]any, 0, len(outcome.Results))

	var summary strings.Builder

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
		"usage":         map[string]any{"input_tokens": 0, "output_tokens": 0},
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
	errSearchQueryEmpty = errors.New("请在 messages 中提供一段用户文本作为搜索词")
)
