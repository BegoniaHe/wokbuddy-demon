// localwebtools.go Responses API 本地 web_search / web_fetch 代跑（issue #104）。
//
// 仅当 features.local_web_tools 打开且请求声明服务端 web 工具时启用：
//  1. 把服务端工具替换为网关同名 function；
//  2. 用非流式聚合反复调用 chatFlow，执行模型返回的 web 工具调用；
//  3. 把结果作为 tool 消息回传，直到没有工具调用或达到轮数上限；
//  4. 最终一次性写回 Responses JSON 或 SSE。
//
// 安全边界：web_fetch 只允许 http/https，拒绝字面/解析到私网、回环、链路本地
// 地址的目标，重定向逐跳重新校验；限制超时、响应大小与文本类型白名单。
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/httpauth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/logfmt"
	"github.com/BegoniaHe/wokbuddy-demon/internal/reqlog"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

const (
	defaultLocalWebToolRounds = 3
	maxLocalWebToolRounds     = 8
	// maxLocalToolCallsPerRound 单轮工具调用上限：轮数已有封顶，但一轮里的 calls
	// 由模型输出决定。每个 web_fetch 一次出站请求、单次超时 15s 且串行——
	// 一次返回 100 个并行调用就是 100 次抓取、最长 25 分钟挂住一个入站请求。
	maxLocalToolCallsPerRound = 8
	webToolMaxResponseBytes   = 2 << 20
	webToolMaxTextBytes       = 32 << 10
	webFetchTimeout           = 15 * time.Second
	webSearchTimeout          = 10 * time.Second
	// localWebToolErrorMessageLimit 回给客户端的错误 message 上限：上游原文可能很长，
	// 整段透传既无用又可能带内部细节（issue #416）。
	localWebToolErrorMessageLimit = 512
)

// 测试可替换的实现注入点（不导出）。
var (
	webSearchFn = webSearch
	webFetchFn  = webFetch
)

var (
	errEmptyUpstreamResponse       = errors.New("empty upstream response")
	errLocalWebToolLoopExhausted   = errors.New("local web tool loop exhausted")
	errLocalWebToolLoopRequiresNon = errors.New("local web tool loop requires non-stream response")
	errQuery                       = errors.New("query 不能为空")
	errWebFetchFailed              = errors.New("webFetch failed")
	errStatic                      = errors.New("重定向次数过多")
	errFetchRawFailed              = errors.New("fetchRaw failed")
	errValidateWebURLFailed        = errors.New("validateWebURL failed")
	errURL                         = errors.New("URL 缺少主机名")
	errCheckHostIPFailed           = errors.New("checkHostIP failed")
	errDialWebFetchFailed          = errors.New("dialWebFetch failed")
)

// declaredLocalWebTools 检测 Responses 请求声明的服务端 web 工具。
func declaredLocalWebTools(body []byte) (map[string]bool, bool) {
	var req struct {
		Tools []map[string]any `json:"tools"`
		Input json.RawMessage  `json:"input"`
	}
	if json.Unmarshal(body, &req) != nil {
		return nil, false
	}

	wanted := map[string]bool{}

	// Codex 0.162+ 把工具放进 input.additional_tools；与 responsesToOpenAIWithClient
	// 的合并口径保持一致，否则本地 web 工具不会触发、反而被透传给上游。
	for _, tool := range append(req.Tools, responsesAdditionalTools(req.Input)...) {
		switch stringValue(tool["type"]) {
		case webSearchTool, "web_search_preview":
			wanted[webSearchTool] = true
		case "web_fetch":
			wanted["web_fetch"] = true
		}
	}

	return wanted, len(wanted) > 0
}

// localWebToolRounds 返回生效轮数（默认 3，封顶 8）。
func (h *Handler) localWebToolRounds() int {
	n := h.loadLive().LocalWebToolRounds
	if n <= 0 {
		return defaultLocalWebToolRounds
	}

	if n > maxLocalWebToolRounds {
		return maxLocalWebToolRounds
	}

	return n
}

// serveResponsesWithLocalTools 执行本地工具循环并写回最终响应。
func (h *Handler) serveResponsesWithLocalTools(
	w http.ResponseWriter, r *http.Request, body []byte, wanted map[string]bool,
) {
	openAI, model, instructions, stream, err := responsesToOpenAIWithClient(
		body, h.responsesHTTPClient(), h.cfg.ModelAliases,
	)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	var req map[string]any
	if uerr := json.Unmarshal(openAI, &req); uerr != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "invalid converted request")
		return
	}

	localTools := ensureLocalToolDeclarations(req, wanted)
	msgs, _ := req["messages"].([]any)

	final, err := h.runLocalWebToolLoop(r, req, msgs, model, localTools)
	if err != nil {
		// 调用方已断开：同 #404 口径，不写响应体、不压成 502 污染上游故障样本。
		if localToolCallerGone(r) {
			return
		}
		// 内层请求级错误（content_blocked / prompt_too_long / 参数错 / 模型不存在）
		// 保留原状态码与分类 code，不压平成 502（issue #416）。
		if toolErr, ok := errors.AsType[*localWebToolLoopError](err); ok {
			writeOpenAIError(w, toolErr.status, toolErr.code, toolErr.msg)

			return
		}

		writeOpenAIError(w, http.StatusBadGateway, "local_web_tool_failed",
			upstream.RedactForLog(err.Error(), localWebToolErrorMessageLimit))

		return
	}

	resp := responsesFromOpenAIWithNamespaces(
		final, model, instructions,
		responsesCustomTools(body), responsesToolNamespacesFromBody(body),
	)
	if stream {
		writeResponsesObjectSSE(w, resp)
		return
	}

	writeJSON(w, http.StatusOK, resp)
}

// localToolCallerGone 报告入站请求是否已断开，并把请求日志定性为 499 / interrupted：
// 连接已关，写响应体没有意义；最后一轮 chatStat 可能仍是成功，不能用来冒充结果。
func localToolCallerGone(r *http.Request) bool {
	if r.Context().Err() == nil {
		return false
	}

	if trace := requestTraceFrom(r); trace != nil {
		trace.fallbackStatus = clientClosedRequestStatus
		trace.noteForcedOutcome(reqlog.OutcomeInterrupted)
	}

	return true
}

// runLocalWebToolLoop 反复执行模型请求与本地工具，返回最终 chat 响应。
func (h *Handler) runLocalWebToolLoop(
	r *http.Request, req map[string]any, msgs []any, model string, localTools map[string]bool,
) (map[string]any, error) {
	ctx := r.Context()
	maxRounds := h.localWebToolRounds()

	for round := 0; round <= maxRounds; round++ {
		// 入站请求取消/断开后不再发起新的一轮上游调用（issue #415）。
		if cerr := ctx.Err(); cerr != nil {
			return nil, cerr
		}

		if round == maxRounds {
			// 代跑轮数用尽：禁止继续调用工具，让模型基于已有结果收尾。
			req["tool_choice"] = "none"
		}

		obj, err := h.runLocalWebToolRound(r, req, model)
		if err != nil {
			return nil, err
		}

		calls, ok := localToolCalls(obj, localTools)
		if !ok || len(calls) == 0 || round == maxRounds {
			return obj, nil
		}

		// 声明与执行必须同口径：assistant 消息里保留几个 tool_calls，就必须有
		// 同样多个 role=tool 结果。多声明的调用没有对应结果，上游会以「未回应
		// 的 tool_call」拒掉整轮。
		calls = calls[:min(len(calls), maxLocalToolCallsPerRound)]

		msgs = append(msgs, assistantToolMessage(obj, len(calls)))

		msgs, err = h.appendLocalToolResults(ctx, msgs, calls)
		if err != nil {
			return nil, err
		}

		req["messages"] = msgs
	}

	return nil, errLocalWebToolLoopExhausted
}

// runLocalWebToolRound 执行一轮上游请求并返回解析后的 chat 响应。
func (h *Handler) runLocalWebToolRound(
	r *http.Request, req map[string]any, model string,
) (map[string]any, error) {
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}

	buf := &bufferResponder{}
	chatInput2 := newChatInputFromRequest(r, raw, false, model)
	chatInput2.clientIP = httpauth.ClientIP(r, h.cfg.TrustedProxies)
	h.serveChat(r, &chatInput2, buf)

	if buf.status != 0 {
		return nil, &localWebToolLoopError{
			status: buf.status,
			code:   buf.code,
			// 上游原文可能有界透传为 message，但必须脱敏且截断，不能整段回给客户端。
			msg: upstream.RedactForLog(buf.msg, localWebToolErrorMessageLimit),
		}
	}

	obj, _ := buf.body.(map[string]any)
	if obj == nil {
		return nil, errEmptyUpstreamResponse
	}

	return obj, nil
}

// appendLocalToolResults 执行本轮工具调用；每次调用前确认入站请求仍在（#415）。
// 调用数已由调用方按 maxLocalToolCallsPerRound 截断，与 assistant 消息里的
// tool_calls 一一对应。
func (h *Handler) appendLocalToolResults(
	ctx context.Context, msgs []any, calls []localToolCall,
) ([]any, error) {
	for _, call := range calls {
		// 抓取进行中断开时，剩余工具不再发起（#415）。
		if cerr := ctx.Err(); cerr != nil {
			return nil, cerr
		}

		msgs = append(msgs, h.toolResultMessage(ctx, call))
	}

	return msgs, nil
}

// localWebToolLoopError 内层 serveChat 的请求级错误：保留内层状态码与分类 code，
// 由 serveResponsesWithLocalTools 按原语义回给客户端。只有无法归类的错误才回 502。
// message 已脱敏并按 localWebToolErrorMessageLimit 截断（issue #416）。
type localWebToolLoopError struct {
	status int
	code   string
	msg    string
}

func (e *localWebToolLoopError) Error() string {
	return e.code + ": " + e.msg
}

// localToolCalls 提取仅由网关执行的工具调用；出现客户端 function 调用时返回
// ok=false，让最终响应交回客户端处理，避免吞掉它。
//
// localTools 是网关本轮**实际注入**声明的工具名集合。只按名字白名单判定会把
// 「客户端自己声明了同名 web_search 函数」的调用也劫持成网关工具执行，客户端
// 永远收不到这次调用（ensureLocalToolDeclarations 见名字已存在就跳过注入）。
func localToolCalls(obj map[string]any, localTools map[string]bool) ([]localToolCall, bool) {
	choice := firstOpenAIChoice(obj)
	if choice == nil {
		return nil, true
	}

	msg, _ := choice["message"].(map[string]any)

	var raw []any

	switch v := msg["tool_calls"].(type) {
	case []any:
		raw = v
	case []map[string]any:
		for _, item := range v {
			raw = append(raw, item)
		}
	}

	calls := make([]localToolCall, 0, len(raw))
	for _, item := range raw {
		call, _ := item.(map[string]any)
		fn, _ := call["function"].(map[string]any)

		name := stringValue(fn["name"])
		if !localTools[name] {
			return nil, false
		}

		calls = append(calls, localToolCall{
			ID: stringValue(call["id"]), Name: name, Arguments: stringValue(fn["arguments"]),
		})
	}

	return calls, true
}

type localToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// assistantToolMessage 把模型返回的工具调用作为 assistant 消息追加。
// limit 是本轮真正要执行的调用数：多出的调用不能留在消息里，否则历史里声明了
// 没有结果的 tool_call。
func assistantToolMessage(obj map[string]any, limit int) map[string]any {
	choice := firstOpenAIChoice(obj)
	msg, _ := choice["message"].(map[string]any)

	out := map[string]any{"role": "assistant", "content": stringValue(msg["content"])}

	switch calls := msg["tool_calls"].(type) {
	case []any:
		if limit < len(calls) {
			calls = calls[:limit]
		}

		out["tool_calls"] = calls
	case []map[string]any:
		if limit < len(calls) {
			calls = calls[:limit]
		}

		out["tool_calls"] = calls
	}

	return out
}

// toolResultMessage 执行本地工具并构造 role=tool 消息；失败以文本回传，让模型
// 能看到具体原因继续回答。
func (h *Handler) toolResultMessage(ctx context.Context, call localToolCall) map[string]any {
	content := h.executeLocalTool(ctx, call)
	return map[string]any{"role": "tool", keyToolCallID: call.ID, "content": content}
}

func (h *Handler) executeLocalTool(ctx context.Context, call localToolCall) string {
	var args struct {
		Query string `json:"query"`
		URL   string `json:"url"`
	}

	_ = json.Unmarshal([]byte(call.Arguments), &args)
	switch call.Name {
	case webSearchTool:
		out, err := h.webSearch(ctx, args.Query)
		if err != nil {
			return "web_search error: " + err.Error()
		}

		return out
	case "web_fetch":
		out, err := h.webFetch(ctx, args.URL)
		if err != nil {
			return "web_fetch error: " + err.Error()
		}

		return out
	default:
		return "unsupported tool: " + call.Name
	}
}

// ensureLocalToolDeclarations 把服务端工具替换成网关 function 声明，返回本轮
// **实际注入**的工具名集合（客户端已声明同名 function 的不算，见 localToolCalls）。
func ensureLocalToolDeclarations(req map[string]any, wanted map[string]bool) map[string]bool {
	tools, _ := req["tools"].([]any)
	seen := map[string]bool{}

	for _, item := range tools {
		tool, _ := item.(map[string]any)

		fn, _ := tool["function"].(map[string]any)
		if name := stringValue(fn["name"]); name != "" {
			seen[name] = true
		}
	}

	injected := make(map[string]bool, len(wanted))

	for name := range wanted {
		if seen[name] {
			continue
		}

		injected[name] = true
		tools = append(tools, map[string]any{"type": "function", "function": localToolSpec(name)})
	}

	if len(tools) > 0 {
		req["tools"] = tools
	}

	return injected
}

func localToolSpec(name string) map[string]any {
	if name == "web_fetch" {
		return map[string]any{
			"name": "web_fetch", "description": "抓取一个 http/https URL 的文本内容（仅公网、文本类型）",
			"parameters": map[string]any{
				"type": "object", "properties": map[string]any{"url": map[string]any{"type": "string"}},
				"required": []any{"url"},
			},
		}
	}

	return map[string]any{
		"name": webSearchTool, "description": "使用 DuckDuckGo 搜索公开网页并返回标题、链接与摘要",
		"parameters": map[string]any{
			"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}},
			"required": []any{"query"},
		},
	}
}

// bufferResponder 捕获内部非流式调用的最终结果，不写客户端。
type bufferResponder struct {
	body   any
	status int
	code   string
	msg    string
}

func (b *bufferResponder) JSON(_ int, body any) { b.body = body }
func (b *bufferResponder) Error(status int, code, msg, _ string) {
	b.status, b.code, b.msg = status, code, msg
}
func (*bufferResponder) Header(string, string) {}
func (*bufferResponder) Stream(io.Reader, func(string) string, *upstream.MarkupRepair, func(string)) error {
	return errLocalWebToolLoopRequiresNon
}

// sseEventStream 顺序写 Responses SSE 事件（自带序列号与 flush）。
//
// 写失败后静默丢弃后续事件：客户端已走时继续生成既无意义，也会把后续写入的错误
// 吞掉（原实现丢弃 Fprintf 返回值）。
type sseEventStream struct {
	w        http.ResponseWriter
	flusher  http.Flusher
	seq      int
	writeErr error
}

// newSSEEventStream 设置 SSE 响应头（与 httpResponder.setSSEHeaders 同口径）：
// 缺 X-Accel-Buffering 时反代（nginx 默认）会缓冲整个响应，本地工具跑完前客户端
// 一个事件都收不到。
func newSSEEventStream(w http.ResponseWriter) *sseEventStream {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")

	flusher, _ := w.(http.Flusher)

	return &sseEventStream{w: w, flusher: flusher}
}

// event 写一个事件帧（失败后所有后续调用变为 no-op）。
func (s *sseEventStream) event(name string, payload map[string]any) {
	if s.writeErr != nil {
		return
	}

	payload["sequence_number"] = s.seq
	s.seq++

	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}

	if _, werr := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, raw); werr != nil {
		s.writeErr = werr

		return
	}

	if s.flusher != nil {
		s.flusher.Flush()
	}
}

// writeResponsesObjectSSE 把已聚合的 Responses 对象写成 SSE 事件（本地工具模式）。
func writeResponsesObjectSSE(w http.ResponseWriter, resp map[string]any) {
	stream := newSSEEventStream(w)
	event := stream.event

	created := map[string]any{}
	maps.Copy(created, resp)
	created["status"] = "in_progress"
	created["output"] = []any{}
	event("response.created", map[string]any{"type": "response.created", "response": created})
	// 与响应写出器的统一序列对齐（responses.go 的 ensureStart 在 created 之后固定
	// 补 in_progress）：同一个请求走不走本地工具必须是同一套事件序列，按 SSE 事件
	// 严格校验的客户端会等这一条。
	event("response.in_progress", map[string]any{"type": "response.in_progress", "response": created})

	output, _ := resp["output"].([]any)
	for index, item := range output {
		obj, _ := item.(map[string]any)
		event("response.output_item.added", map[string]any{
			"type": "response.output_item.added", keyOutputIndex: index, "item": obj,
		})

		if obj["type"] == "message" {
			emitBufferedMessage(stream, obj, index)
		}

		event("response.output_item.done", map[string]any{
			"type": "response.output_item.done", keyOutputIndex: index, "item": obj,
		})
	}

	event("response.completed", map[string]any{"type": "response.completed", "response": resp})
}

func emitBufferedMessage(stream *sseEventStream, item map[string]any, index int) {
	event := stream.event

	content, _ := item["content"].([]any)
	for partIndex, part := range content {
		m, _ := part.(map[string]any)
		if m["type"] != typeOutputText {
			continue
		}

		text := stringValue(m["text"])

		event("response.content_part.added", map[string]any{
			"type": "response.content_part.added", "item_id": item["id"], keyOutputIndex: index,
			keyContentIndex: partIndex, "part": responsesOutputText(""),
		})
		event("response.output_text.delta", map[string]any{
			"type": "response.output_text.delta", "item_id": item["id"], keyOutputIndex: index,
			keyContentIndex: partIndex, "delta": text, "logprobs": []any{},
		})
		event("response.output_text.done", map[string]any{
			"type": "response.output_text.done", "item_id": item["id"], keyOutputIndex: index,
			keyContentIndex: partIndex, "text": text, "logprobs": []any{},
		})
		// 与流式响应写出器同口径补 part 终态：只发 added/delta/done 而缺
		// content_part.done 时，严格按事件序列校验的客户端会等不到 part 结束。
		event("response.content_part.done", map[string]any{
			"type": "response.content_part.done", "item_id": item["id"], keyOutputIndex: index,
			keyContentIndex: partIndex, "part": responsesOutputText(text),
		})
	}
}

// webSearch 调用 DuckDuckGo HTML 搜索并提取标题/链接/摘要。
func webSearch(ctx context.Context, query string) (string, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return "", errQuery
	}

	ctx, cancel := context.WithTimeout(ctx, webSearchTimeout)
	defer cancel()

	target := "https://html.duckduckgo.com/html/?q=" + url.QueryEscape(query)

	body, _, err := fetchRaw(ctx, target, webToolMaxResponseBytes)
	if err != nil {
		return "", err
	}

	results := parseDuckDuckGoResults(string(body))
	if len(results) == 0 {
		return "未找到结果（来源：DuckDuckGo HTML）", nil
	}

	var builder strings.Builder
	builder.WriteString("来源：DuckDuckGo\n")

	for i, r := range results {
		fmt.Fprintf(&builder, "%d. %s\n   %s\n", i+1, r.title, r.link)

		if r.snippet != "" {
			builder.WriteString("   " + r.snippet + "\n")
		}
	}

	return logfmt.Truncate(builder.String(), webToolMaxTextBytes), nil
}

type searchResult struct{ title, link, snippet string }

var (
	ddgAnchorRe  = regexp.MustCompile(`(?is)<a[^>]+class="result__a"[^>]*href="([^"]+)"[^>]*>(.*?)</a>`)
	ddgSnippetRe = regexp.MustCompile(`(?is)<a[^>]+class="result__snippet"[^>]*>(.*?)</a>`)
	htmlTagRe    = regexp.MustCompile(`(?s)<[^>]+>`)
)

func parseDuckDuckGoResults(page string) []searchResult {
	anchors := ddgAnchorRe.FindAllStringSubmatch(page, 6)
	snippets := ddgSnippetRe.FindAllStringSubmatch(page, 6)

	out := make([]searchResult, 0, len(anchors))
	for i, m := range anchors {
		title := stripHTML(m[2])
		if title == "" {
			continue
		}

		result := searchResult{title: title, link: cleanDuckDuckGoLink(html.UnescapeString(m[1]))}
		if i < len(snippets) {
			result.snippet = stripHTML(snippets[i][1])
		}

		out = append(out, result)
	}

	return out
}

func stripHTML(s string) string {
	return strings.TrimSpace(html.UnescapeString(htmlTagRe.ReplaceAllString(s, " ")))
}

func cleanDuckDuckGoLink(href string) string {
	if strings.HasPrefix(href, "//duckduckgo.com/l/") {
		if u, err := url.Parse("https:" + href); err == nil {
			if uddg := u.Query().Get("uddg"); uddg != "" {
				return uddg
			}
		}
	}

	return href
}

// webFetch 抓取公网文本 URL；拒绝私网/回环/内网解析，限制大小与超时。
func webFetch(ctx context.Context, rawURL string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, webFetchTimeout)
	defer cancel()

	body, contentType, err := fetchRaw(ctx, rawURL, webToolMaxResponseBytes)
	if err != nil {
		return "", err
	}

	if !isTextContentType(contentType) {
		return "", fmt.Errorf("非文本内容类型 %q: %w", contentType, errWebFetchFailed)
	}

	return logfmt.Truncate(string(body), webToolMaxTextBytes), nil
}

func isTextContentType(ct string) bool {
	ct = strings.ToLower(strings.TrimSpace(strings.Split(ct, ";")[0]))

	return strings.HasPrefix(ct, "text/") || ct == "application/json" ||
		ct == "application/xml" || ct == "application/xhtml+xml"
}

// webFetchTransport 本地 web 工具的共享 transport。
//
// 必须是单例：此前每次 fetchRaw 都新建 http.Transport，而零值 Transport 的
// IdleConnTimeout/TLSHandshakeTimeout 都为 0（永不回收）。body 读到 EOF 后连接会
// 进入这个随即被丢弃的 transport 的空闲池，连接与它的读写协程要等对端关闭才退出；
// 模型每次调用 web 工具都要重建一次 TCP+TLS，同 URL 也无法复用连接。
//
// 仍然显式不用 HTTP(S)_PROXY：代理会在网关之外解析目标，让 validateWebURL 与
// dialWebFetch 的 IP 校验失效。
var webFetchTransport = &http.Transport{
	DialContext:           dialWebFetch,
	TLSHandshakeTimeout:   10 * time.Second,
	ResponseHeaderTimeout: webFetchTimeout,
	IdleConnTimeout:       30 * time.Second,
	MaxIdleConns:          8,
	MaxIdleConnsPerHost:   2,
}

// webFetchClient 与 webFetchTransport 配套的共享 client；重定向逐跳校验。
// ctx 由调用方按 webSearchTimeout/webFetchTimeout 控制总时长。
var webFetchClient = &http.Client{
	Transport: webFetchTransport,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errStatic
		}

		return validateWebURL(req.Context(), req.URL.String())
	},
}

// fetchRaw 校验目标、执行请求并读取有界响应；重定向逐跳校验。
func fetchRaw(ctx context.Context, rawURL string, maxBytes int64) ([]byte, string, error) {
	if err := validateWebURL(ctx, rawURL); err != nil {
		return nil, "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, http.NoBody)
	if err != nil {
		return nil, "", fmt.Errorf("构造请求失败: %w", err)
	}

	req.Header.Set("User-Agent", "wokbuddy-demon/local-web-tools")

	resp, err := webFetchClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("上游返回 HTTP %d: %w", resp.StatusCode, errFetchRawFailed)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("读取响应失败: %w", err)
	}

	if int64(len(body)) > maxBytes {
		return nil, "", fmt.Errorf("响应超过 %d 字节上限: %w", maxBytes, errFetchRawFailed)
	}

	return body, resp.Header.Get("Content-Type"), nil
}

// validateWebURL 只允许 http/https 且目标不得解析到内网/回环地址。
func validateWebURL(ctx context.Context, raw string) error {
	url2, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("非法 URL: %w", err)
	}

	if url2.Scheme != "http" && url2.Scheme != "https" {
		return fmt.Errorf("只允许 http/https，当前 %q: %w", url2.Scheme, errValidateWebURLFailed)
	}

	if url2.User != nil {
		return fmt.Errorf("URL 不允许包含用户信息: %w", errValidateWebURLFailed)
	}

	if url2.Hostname() == "" {
		return errURL
	}

	return checkHostIP(ctx, url2.Hostname())
}

// checkHostIP 拒绝字面或 DNS 解析到私网/回环/链路本地/未指定/组播的地址。
func checkHostIP(ctx context.Context, host string) error {
	if ip := net.ParseIP(host); ip != nil {
		if isBlockedIP(ip) {
			return fmt.Errorf("拒绝访问内网地址 %s: %w", ip, errCheckHostIPFailed)
		}

		return nil
	}

	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("解析域名失败: %w", err)
	}

	if len(addrs) == 0 {
		return fmt.Errorf("域名 %s 无解析结果: %w", host, errCheckHostIPFailed)
	}

	for _, addr := range addrs {
		if isBlockedIP(addr.IP) {
			return fmt.Errorf("拒绝访问内网地址 %s（解析自 %s）: %w", addr.IP, host, errCheckHostIPFailed)
		}
	}

	return nil
}

// blockedNonPublicPrefixes 非私网但同样不可公网访问的地址段（netip 的
// IsPrivate 只覆盖 RFC1918/RFC4193）：0.0.0.0/8、CGNAT（100.64/10，含 Tailscale
// 等内网）、IETF 协议分配、文档/基准测试段与 240/4 保留段。漏掉它们会让
// web_fetch 穿过内网边界（如 http://100.100.100.100）。
//
// IPv6 过渡/转换前缀必须单列：NAT64（64:ff9b::/96 与 RFC 8215 本地前缀）和
// 6to4/Teredo 都能把 IPv4 地址嵌进 IPv6 字面量，IsGlobalUnicast/IsPrivate 却判为
// 公网。漏掉时 http://[64:ff9b::a00:1]/ 会在 NAT64 环境里访问 10.0.0.1。
//
// NAT64 知名前缀例外：IPv6-only + DNS64 环境会把**所有**公网 IPv4 目标合成为
// 64:ff9b::/96，直接封整段会让 web_fetch 失去全部 IPv4 站点。该前缀在
// isBlockedIP 里按嵌入的 IPv4 逐地址判定（内嵌私网仍拒绝）。
var blockedNonPublicPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	// IPv6 特殊用途：文档/基准/丢弃段。
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001:2::/48"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("3fff::/20"),
	// IPv4 过渡/转换前缀：可嵌入内网 IPv4。
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2001::/32"),
	netip.MustParsePrefix("2002::/16"),
	// IPv4 兼容/转换形态（::a.b.c.d、::ffff:0:a.b.c.d）与已废弃的 site-local。
	netip.MustParsePrefix("::/96"),
	netip.MustParsePrefix("::ffff:0:0:0/96"),
	netip.MustParsePrefix("fec0::/10"),
}

// nat64WellKnownPrefix DNS64 合成的知名前缀（RFC 6052）。单独判定而非整段封禁。
var nat64WellKnownPrefix = netip.MustParsePrefix("64:ff9b::/96")

func isBlockedIP(ip net.IP) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	// 非全局单播（回环/未指定/链路本地/组播/广播）与 RFC1918/RFC4193 私网。
	addr = addr.Unmap()
	if !addr.IsGlobalUnicast() || addr.IsPrivate() {
		return true
	}

	// NAT64 知名前缀按内嵌 IPv4 判定：公网目标放行（IPv6-only/DNS64 环境可用），
	// 内嵌私网/保留段仍拒绝。
	if nat64WellKnownPrefix.Contains(addr) {
		raw := addr.As16()

		return isBlockedIP(net.IPv4(raw[12], raw[13], raw[14], raw[15]))
	}

	for _, prefix := range blockedNonPublicPrefixes {
		if prefix.Contains(addr) {
			return true
		}
	}

	return false
}

// dialWebFetch 解析目标域名，校验所有解析结果后直接连接已验证的 IP。
// 只校验再交给默认 dialer 重新解析仍留有 DNS rebinding 窗口，故这里不二次解析。
func dialWebFetch(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("dialWebFetch: %w", err)
	}

	var ips []net.IP
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IP{ip}
	} else {
		addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("解析域名失败: %w", err)
		}

		for _, a := range addrs {
			ips = append(ips, a.IP)
		}
	}

	if len(ips) == 0 {
		return nil, fmt.Errorf("域名 %s 无解析结果: %w", host, errDialWebFetchFailed)
	}

	for _, ip := range ips {
		if isBlockedIP(ip) {
			return nil, fmt.Errorf("拒绝访问内网地址 %s（解析自 %s）: %w", ip, host, errDialWebFetchFailed)
		}
	}

	var (
		dialer  net.Dialer
		lastErr error
	)

	for _, ip := range ips {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}

		lastErr = err
	}

	return nil, lastErr
}
