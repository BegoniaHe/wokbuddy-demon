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
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

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

	body, ok := h.readSearchBody(w, r)
	if !ok {
		return
	}

	req, perr := parseSearchRequest(body)
	if perr != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", perr.Error(), "")

		return
	}

	if req.deep {
		h.anthropicDeepSearch(w, r, req)
		return
	}

	meta := searchMetaFromRequest(r)
	started := time.Now()
	outcome, uid, fail := h.runSearch(r.Context(), req.query, req.options, &meta)
	h.recordSearchUsage(r, uid, req.model, started, nil, fail == nil)
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

// readSearchBody 读取并限制搜索请求体；失败已写出 Anthropic 错误。
func (h *Handler) readSearchBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	limitRequestBody(w, r, h.cfg.MaxBodyBytes)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		if isBodyTooLarge(err) {
			writeAnthropicError(w, http.StatusRequestEntityTooLarge, "request_too_large",
				bodyTooLargeMessage(h.cfg.MaxBodyBytes), "缩小请求体或调大 server.max_body_bytes")

			return nil, false
		}

		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error(), "")

		return nil, false
	}

	return body, true
}

// searchRequest 解析出的搜索请求要素。
type searchRequest struct {
	model   string
	query   string
	options upstream.SearchOptions
	deep    bool
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
		// Deep 可选：true = 官方综述式检索（features.search_deep）。
		Deep bool `json:"deep"`
	}

	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, errSearchInvalidJSON
	}

	model, merr := normalizeSearchModel(payload.Model)
	if merr != nil {
		return nil, merr
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

	// 按字符（rune）而不是字节计：文档写的是「字符」上限，单个 CJK 字符 3 字节，
	// 用 len() 会把 342 个汉字的合法搜索词误判成超长。
	if utf8.RuneCountInString(query) < upstream.SearchMinQueryLen {
		return nil, errSearchQueryTooShort
	}

	if utf8.RuneCountInString(query) > searchQueryMaxLen {
		return nil, errSearchQueryTooLong
	}

	// 条数：请求体顶层 max_results 优先，其次 tools[].max_results；两处都缺省时用
	// upstream 的默认值（ClampSearchMaxResults 负责收敛到 [1, cap]）。
	options := toolSearchOptions(payload.Tools)
	if payload.MaxResults > 0 {
		options.MaxResults = payload.MaxResults
	}

	if err := validateSearchOptions(options); err != nil {
		return nil, err
	}

	return &searchRequest{model: model, query: query, options: options, deep: payload.Deep}, nil
}

// toolSearchOptions 解析 web_search 工具声明里的官方可选参数。
func toolSearchOptions(tools []map[string]any) upstream.SearchOptions {
	for _, tool := range tools {
		name, _ := tool["name"].(string)
		kind, _ := tool["type"].(string)

		if name != "web_search" && !strings.HasPrefix(kind, "web_search") {
			continue
		}

		opts := upstream.SearchOptions{
			AllowedDomains: stringList(tool["allowed_domains"]),
			BlockedDomains: stringList(tool["blocked_domains"]),
			Freshness:      strings.TrimSpace(stringValue(tool["freshness"])),
		}

		switch value := tool["max_results"].(type) {
		case float64:
			opts.MaxResults = int(value)
		case int:
			opts.MaxResults = value
		}

		return opts
	}

	return upstream.SearchOptions{}
}

// stringList 把 JSON 字符串数组解析为 []string（非数组/非字符串项忽略）。
func stringList(v any) []string {
	items, _ := v.([]any)
	if len(items) == 0 {
		return nil
	}

	out := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, strings.TrimSpace(s))
		}
	}

	return out
}

// validateSearchOptions 在入口校验域名与时间范围：非法值 400，不烧账号。
func validateSearchOptions(opts upstream.SearchOptions) error {
	for _, domain := range append(append([]string{}, opts.AllowedDomains...), opts.BlockedDomains...) {
		if !upstream.ValidSearchDomain(domain) {
			return fmt.Errorf("%w: %q", errSearchInvalidDomain, domain)
		}
	}

	if len(opts.AllowedDomains) > upstream.SearchMaxDomains || len(opts.BlockedDomains) > upstream.SearchMaxDomains {
		return fmt.Errorf("%w（每类最多 %d 个）", errSearchTooManyDomains, upstream.SearchMaxDomains)
	}

	if !upstream.ValidSearchFreshness(opts.Freshness) {
		return fmt.Errorf("%w: %q", errSearchInvalidFreshness, opts.Freshness)
	}

	return nil
}

// normalizeSearchModel 归一搜索请求的 model：去空白、拒绝空值、只接受 cn 域。
// 搜索只走 CN 账号，显式 global: 前缀必须拒绝（否则客户端以为在查国际版，实际
// 消耗 CN 额度）；cn: 前缀剥掉后按裸名回显（与其它入站协议同口径）。
func normalizeSearchModel(raw string) (string, error) {
	model := strings.TrimSpace(raw)
	if model == "" {
		return "", errModelIsRequired
	}

	realm, bare, prefixed := splitRealm(model)
	if !prefixed {
		return model, nil
	}

	if realm != searchRealm {
		return "", errSearchRealmUnsupported
	}

	model = strings.TrimSpace(bare)
	if model == "" {
		return "", errModelIsRequired
	}

	return model, nil
}

// searchQueryMaxLen 搜索词长度上限（防御性：上游 zod 无上限，超长 query 只会让
// 上游超时并白占一次在途名额；1024 字符已远超任何真实搜索词）。
const searchQueryMaxLen = 1024

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

// searchQueryPrefix 官方搜索客户端（DSH / 官方 CLI）的固定指令前缀。它是写给模型
// 看的指令、不是搜索词：客户端把搜索 base 指到本端点时会把整句一起送来，不剥掉就
// 会把指令本身当作 query 打给上游（上游契约见 issue #203）。
const searchQueryPrefix = "Perform a web search for the query:"

// stripSearchQueryPrefix 大小写不敏感地剥掉官方固定前缀（不同客户端模板大小写可能
// 有差异）；没有前缀时原样返回（已 trim）。
func stripSearchQueryPrefix(text string) string {
	trimmed := strings.TrimSpace(text)

	if len(trimmed) >= len(searchQueryPrefix) &&
		strings.EqualFold(trimmed[:len(searchQueryPrefix)], searchQueryPrefix) {
		return strings.TrimSpace(trimmed[len(searchQueryPrefix):])
	}

	return trimmed
}

// lastUserText 取最后一条 user 消息里的文本作为搜索词（块数组取全部 text 块，
// 纯字符串取其本身），并剥掉官方固定指令前缀。
func lastUserText(messages []searchInboundMessage) string {
	for _, message := range slices.Backward(messages) {
		if message.Role != "user" {
			continue
		}

		if parts := messageTextParts(message.Content); len(parts) > 0 {
			return stripSearchQueryPrefix(strings.Join(parts, "\n"))
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
			{"type": "text", "text": summary, "citations": citations},
			{
				"type": "web_search_tool_result",
				// Anthropic 的 web_search_tool_result 必须带 tool_use_id（标识「一次
				// 工具调用」，每次响应唯一）；缺字段的块不符合协议，严格客户端会拒收。
				"tool_use_id": "srvtoolu_" + session.NewMessageID(),
				"content":     results,
			},
		},
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		// usage 是估算值（网关没有 tokenizer）：input 按搜索词字节/4、output 按摘要
		// 字节/4 粗估。客户端只用它做上下文预算，不作为计费依据（与 count_tokens 同口径）。
		"usage": map[string]any{
			"input_tokens":  max(1, queryBytes/4),
			"output_tokens": max(1, len(summary)/4),
		},
	}
}

// anthropicDeepSearch 执行综述式检索并写回 Anthropic message（issue #277）。
func (h *Handler) anthropicDeepSearch(w http.ResponseWriter, r *http.Request, req *searchRequest) {
	if !h.loadLive().SearchDeep {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error",
			"deep search is disabled; enable features.search_deep to use deep=true", "")

		return
	}

	meta := searchMetaFromRequest(r)
	started := time.Now()
	outcome, uid, fail := h.runAgenticSearch(r.Context(), req.query, &meta)

	var agenticUsage *upstream.AgenticUsage
	if outcome != nil {
		agenticUsage = outcome.Usage
	}

	h.recordSearchUsage(r, uid, req.model, started, agenticUsage, fail == nil)

	if fail != nil {
		requestTraceFrom(r).noteResult(logfmt.Label(uid, accountNickname(h, uid)), req.model, reqlog.OutcomeHTTPError)
		writeAnthropicError(w, fail.status, searchErrorType(fail.status), fail.msg, searchHint(fail))

		return
	}

	requestTraceFrom(r).noteResult(logfmt.Label(uid, accountNickname(h, uid)), req.model, reqlog.OutcomeSuccess)

	writeJSON(w, http.StatusOK, buildAgenticSearchMessage(req.model, outcome))
}

// buildAgenticSearchMessage 组装综述式检索的 Anthropic message：单个 text 块。
func buildAgenticSearchMessage(model string, outcome *upstream.AgenticOutcome) map[string]any {
	synthesis := ""
	inputTokens, outputTokens := 1, 1

	if outcome != nil {
		synthesis = outcome.Synthesis
		// 上游不返回用量时按查询/综述粗估（与 snippet 搜索同一口径），有真实
		// usage 再覆盖；输入与输出必须分别取 query / synthesis。
		inputTokens = max(1, len(outcome.Query)/4)
		outputTokens = max(1, len(synthesis)/4)
	}

	usage := map[string]any{
		"input_tokens":  inputTokens,
		"output_tokens": outputTokens,
	}

	if outcome != nil && outcome.Usage != nil {
		if outcome.Usage.PromptTokens > 0 {
			usage["input_tokens"] = outcome.Usage.PromptTokens
		}

		if outcome.Usage.CompletionTokens > 0 {
			usage["output_tokens"] = outcome.Usage.CompletionTokens
		}
	}

	return map[string]any{
		"id":            "msg_" + session.NewMessageID(),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       []map[string]any{{"type": "text", "text": synthesis}},
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		"usage":         usage,
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
func buildSearchBlocks(outcome *upstream.SearchOutcome) (citations, results []map[string]any, summary string) {
	citations = make([]map[string]any, 0, len(outcome.Results))
	results = make([]map[string]any, 0, len(outcome.Results))

	var text strings.Builder

	for i := range outcome.Results {
		result := &outcome.Results[i]

		// 无标题时用 URL 兜底：Anthropic 客户端按 title 渲染结果行，空标题会显示成
		// 空白条目（上游也不保证每条都带 title）。
		title := result.Title
		if title == "" {
			title = result.URL
		}

		cited := result.Snippet
		if cited == "" {
			cited = title
		}

		citations = append(citations, map[string]any{
			"type":            "web_search_result_location",
			"url":             result.URL,
			"title":           title,
			"cited_text":      cited,
			"encrypted_index": "",
		})
		results = append(results, map[string]any{
			"type":    "web_search_result",
			"title":   title,
			"url":     result.URL,
			"snippet": result.Snippet,
		})

		if text.Len() > 0 {
			text.WriteString("\n")
		}

		text.WriteString("- ")
		text.WriteString(title)

		if result.Snippet != "" {
			text.WriteString("：")
			text.WriteString(result.Snippet)
		}
	}

	if text.Len() == 0 {
		text.WriteString("未找到与该查询相关的网页结果。")
	}

	return citations, results, text.String()
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
	errSearchQueryEmpty       = errors.New("请在 messages 中提供一段用户文本作为搜索词")
	errSearchQueryTooShort    = errors.New("搜索词过短（至少 2 个字符）")
	errSearchQueryTooLong     = errors.New("搜索词过长（上限 1024 字符），请精简后重试")
	errSearchRealmUnsupported = errors.New(
		"该搜索端点只支持 cn 域；请去掉 global: 前缀（当前不支持国际版搜索）")
	errSearchInvalidDomain    = errors.New("非法搜索域名：只允许 host（不含协议/路径/端口/空白）")
	errSearchTooManyDomains   = errors.New("搜索域名数量超过上限")
	errSearchInvalidFreshness = errors.New("非法 freshness：只允许 d1-d30 / m1-m12 / y1-y5")
)
