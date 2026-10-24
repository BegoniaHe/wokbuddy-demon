// anthropic_search.go /v1/messages 服务端 web_search 执行（issue #256）。
//
// 官方 Anthropic 服务端工具 web_search_20250305 在网关内的最小实现：
//  1. 模型/客户端要求搜索时，先从最后一条 user 文本提取检索词；
//  2. 复用 internal/upstream/search.go 的官方 /agenttool/v1/search；
//  3. 把结果注入 system 上下文，并回发 server_tool_use + web_search_tool_result；
//  4. 未声明该工具时零额外开销；检索失败如实降级，不静默当「没有结果」。
//
// 门控复用 features.search_endpoint（默认 false）：关闭时保持原 400 unsupported
// server tool 语义，不产生隐式搜索消耗。
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/session"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// anthropicConvertWithSearch 解码 → 可选执行 web_search → 转换。返回的 prelude 是
// 响应前部要回发的 Anthropic 内容块；未声明搜索时 nil（零开销）。
func (h *Handler) anthropicConvertWithSearch(
	ctx context.Context, r *http.Request, body []byte,
) (anthropicConversion, []any, error) {
	req, err := decodeAnthropicRequest(body)
	if err != nil {
		return anthropicConversion{}, nil, err
	}

	if !declaresWebSearch(req.Tools) || !h.loadLive().SearchEndpoint {
		converted, convErr := anthropicConvertRequest(req)

		return converted, nil, convErr
	}

	query := anthropicLastUserText(req.Messages)
	if query == "" {
		// 没有可检索文本：保持既有「自定义工具」语义（网关无法凭空搜索）。
		return anthropicConversion{}, nil, errSearchQueryEmpty
	}

	meta := searchMetaFromRequest(r)

	options := toolSearchOptions(req.Tools)

	if validationErr := validateSearchOptions(options); validationErr != nil {
		return anthropicConversion{}, nil, validationErr
	}

	started := time.Now()
	outcome, uid, fail := h.runSearch(ctx, query, options, &meta)
	h.recordSearchUsage(r, uid, req.Model, started, nil, fail == nil)

	prelude := anthropicSearchPrelude(query, outcome, fail)
	req.Tools = stripWebSearchTools(req.Tools)
	injectAnthropicSearchContext(req, query, outcome, fail)

	if trace := requestTraceFrom(r); trace != nil {
		outcomeName := "success"
		if fail != nil {
			outcomeName = "http_error"
		}

		trace.noteResult(uid, "", outcomeName)
	}

	converted, err := anthropicConvertRequest(req)

	return converted, prelude, err
}

// anthropicLastUserText 取最后一条 user 消息的文本（块数组只取 text 块）。
func anthropicLastUserText(messages []json.RawMessage) string {
	for _, raw := range slices.Backward(messages) {
		var msg struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(raw, &msg) != nil || msg.Role != "user" {
			continue
		}

		if parts := messageTextParts(msg.Content); len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}

	return ""
}

// stripWebSearchTools 移除 web_search 服务端工具；其余服务端工具保持原样，
// 由 anthropicTools 继续拒绝（不在本 issue 扩展）。
func stripWebSearchTools(tools []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(tools))

	for _, tool := range tools {
		if declaresWebSearch([]map[string]any{tool}) {
			continue
		}

		out = append(out, tool)
	}

	if len(out) == 0 {
		return nil
	}

	return out
}

// anthropicSearchPrelude 构造 server_tool_use + web_search_tool_result 块。
func anthropicSearchPrelude(query string, outcome *upstream.SearchOutcome, fail *searchFailure) []any {
	toolUseID := "srvtoolu_" + session.NewMessageID()

	use := map[string]any{
		"type": "server_tool_use", "id": toolUseID, "name": "web_search",
		"input": map[string]any{"query": query},
	}

	var result map[string]any
	if fail != nil {
		// 官方契约：出错时 content 是单个 error 对象，不是数组；成功但无结果才是
		// 空数组。客户端按该形状解析，数组会落进结果分支而丢掉错误码。
		result = map[string]any{
			"type": "web_search_tool_result", "tool_use_id": toolUseID,
			"content": map[string]any{
				"type": "web_search_tool_result_error", "error_code": "unavailable",
			},
		}
	} else {
		results := make([]any, 0, len(outcome.Results))
		for _, item := range outcome.Results {
			results = append(results, map[string]any{
				"type": "web_search_result", "title": item.Title, "url": item.URL,
			})
		}

		result = map[string]any{
			"type": "web_search_tool_result", "tool_use_id": toolUseID,
			"content": results,
		}
	}

	return []any{use, result}
}

// injectAnthropicSearchContext 把检索结果写进 system 消息；失败时写明失败原因，
// 让模型如实告知用户，而不是把「搜索失败」当成「网上没有」。
func injectAnthropicSearchContext(req *anthropicRequest, query string, outcome *upstream.SearchOutcome, fail *searchFailure) {
	var text strings.Builder

	if fail != nil {
		text.WriteString("Web search failed for query: ")
		text.WriteString(query)
		text.WriteString("\nReason: ")
		text.WriteString(fail.msg)
		text.WriteString("\nTell the user the search could not be completed; do not claim there are no results.")
	} else {
		text.WriteString("Web search results for query: ")
		text.WriteString(query)
		text.WriteString("\n")

		for i, item := range outcome.Results {
			fmt.Fprintf(&text, "%d. %s\n   %s\n", i+1, item.Title, item.URL)

			if item.Snippet != "" {
				fmt.Fprintf(&text, "   %s\n", item.Snippet)
			}
		}

		text.WriteString("\nUse these sources and cite their URLs when answering.")
	}

	raw, err := json.Marshal(map[string]any{"role": "system", "content": text.String()})
	if err != nil {
		return
	}

	req.Messages = append([]json.RawMessage{raw}, req.Messages...)
}
