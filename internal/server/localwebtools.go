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
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/logfmt"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

const (
	defaultLocalWebToolRounds = 3
	maxLocalWebToolRounds     = 8
	webToolMaxResponseBytes   = 2 << 20
	webToolMaxTextBytes       = 32 << 10
	webFetchTimeout           = 15 * time.Second
	webSearchTimeout          = 10 * time.Second
)

// 测试可替换的实现注入点（不导出）。
var (
	webSearchFn = webSearch
	webFetchFn  = webFetch
)

// declaredLocalWebTools 检测 Responses 请求声明的服务端 web 工具。
func declaredLocalWebTools(body []byte) (map[string]bool, bool) {
	var req struct {
		Tools []map[string]any `json:"tools"`
	}
	if json.Unmarshal(body, &req) != nil {
		return nil, false
	}
	wanted := map[string]bool{}
	for _, tool := range req.Tools {
		switch stringValue(tool["type"]) {
		case "web_search", "web_search_preview":
			wanted["web_search"] = true
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
func (h *Handler) serveResponsesWithLocalTools(w http.ResponseWriter, r *http.Request, body []byte, wanted map[string]bool) {
	openAI, model, instructions, stream, err := responsesToOpenAI(body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	var req map[string]any
	if err := json.Unmarshal(openAI, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "invalid converted request")
		return
	}
	ensureLocalToolDeclarations(req, wanted)
	msgs, _ := req["messages"].([]any)
	final, err := h.runLocalWebToolLoop(r, req, msgs, model)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "local_web_tool_failed", err.Error())
		return
	}
	resp := responsesFromOpenAI(final, model, instructions)
	if stream {
		writeResponsesObjectSSE(w, resp)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// runLocalWebToolLoop 反复执行模型请求与本地工具，返回最终 chat 响应。
func (h *Handler) runLocalWebToolLoop(r *http.Request, req map[string]any, msgs []any, model string) (map[string]any, error) {
	maxRounds := h.localWebToolRounds()
	for round := 0; round <= maxRounds; round++ {
		if round == maxRounds {
			// 代跑轮数用尽：禁止继续调用工具，让模型基于已有结果收尾。
			req["tool_choice"] = "none"
		}
		raw, err := json.Marshal(req)
		if err != nil {
			return nil, fmt.Errorf("encode request: %w", err)
		}
		buf := &bufferResponder{}
		in := chatInput{
			body:      raw,
			stream:    false,
			model:     model,
			clientIP:  upstream.ExtractClientIP(r),
			convReqID: r.Header.Get("X-Conversation-Request-Id"),
			traceID:   r.Header.Get("X-Trace-Id"),
			trace:     requestTraceFrom(r),
			keyUsage:  keyUsageFrom(r.Context()),
			keyRealm:  keyRealmFrom(r.Context()),
		}
		h.serveChat(r, in, buf)
		if buf.status != 0 {
			return nil, fmt.Errorf("%s: %s", buf.code, buf.msg)
		}
		obj, _ := buf.body.(map[string]any)
		if obj == nil {
			return nil, errors.New("empty upstream response")
		}
		calls, ok := localToolCalls(obj)
		if !ok || len(calls) == 0 || round == maxRounds {
			return obj, nil
		}
		msgs = append(msgs, assistantToolMessage(obj))
		for _, call := range calls {
			msgs = append(msgs, toolResultMessage(call))
		}
		req["messages"] = msgs
	}
	return nil, errors.New("local web tool loop exhausted")
}

// localToolCalls 提取仅由网关执行的工具调用；出现客户端 function 调用时返回
// ok=false，让最终响应交回客户端处理，避免吞掉它。
func localToolCalls(obj map[string]any) ([]localToolCall, bool) {
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
		if name != "web_search" && name != "web_fetch" {
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
func assistantToolMessage(obj map[string]any) map[string]any {
	choice := firstOpenAIChoice(obj)
	msg, _ := choice["message"].(map[string]any)
	out := map[string]any{"role": "assistant", "content": stringValue(msg["content"])}
	if calls, ok := msg["tool_calls"]; ok {
		out["tool_calls"] = calls
	}
	return out
}

// toolResultMessage 执行本地工具并构造 role=tool 消息；失败以文本回传，让模型
// 能看到具体原因继续回答。
func toolResultMessage(call localToolCall) map[string]any {
	content := executeLocalTool(call)
	return map[string]any{"role": "tool", "tool_call_id": call.ID, "content": content}
}

func executeLocalTool(call localToolCall) string {
	var args struct {
		Query string `json:"query"`
		URL   string `json:"url"`
	}
	_ = json.Unmarshal([]byte(call.Arguments), &args)
	switch call.Name {
	case "web_search":
		out, err := webSearchFn(args.Query)
		if err != nil {
			return "web_search error: " + err.Error()
		}
		return out
	case "web_fetch":
		out, err := webFetchFn(args.URL)
		if err != nil {
			return "web_fetch error: " + err.Error()
		}
		return out
	default:
		return "unsupported tool: " + call.Name
	}
}

// ensureLocalToolDeclarations 把服务端工具替换成网关 function 声明。
func ensureLocalToolDeclarations(req map[string]any, wanted map[string]bool) {
	tools, _ := req["tools"].([]any)
	seen := map[string]bool{}
	for _, item := range tools {
		tool, _ := item.(map[string]any)
		fn, _ := tool["function"].(map[string]any)
		if name := stringValue(fn["name"]); name != "" {
			seen[name] = true
		}
	}
	for name := range wanted {
		if seen[name] {
			continue
		}
		tools = append(tools, map[string]any{"type": "function", "function": localToolSpec(name)})
	}
	if len(tools) > 0 {
		req["tools"] = tools
	}
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
		"name": "web_search", "description": "使用 DuckDuckGo 搜索公开网页并返回标题、链接与摘要",
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
func (b *bufferResponder) Header(string, string) {}
func (b *bufferResponder) Stream(io.Reader, func(string) string, *upstream.MarkupRepair, func(string)) error {
	return errors.New("local web tool loop requires non-stream response")
}

// writeResponsesObjectSSE 把已聚合的 Responses 对象写成 SSE 事件（本地工具模式）。
func writeResponsesObjectSSE(w http.ResponseWriter, resp map[string]any) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	fl, _ := w.(http.Flusher)
	event := func(name string, payload map[string]any) {
		raw, err := json.Marshal(payload)
		if err != nil {
			return
		}
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, raw)
		if fl != nil {
			fl.Flush()
		}
	}
	created := map[string]any{}
	for k, v := range resp {
		created[k] = v
	}
	created["status"] = "in_progress"
	created["output"] = []any{}
	event("response.created", map[string]any{"type": "response.created", "response": created})
	output, _ := resp["output"].([]any)
	for index, item := range output {
		obj, _ := item.(map[string]any)
		event("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": index, "item": obj})
		if obj["type"] == "message" {
			emitBufferedMessage(event, obj, index)
		}
		event("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": index, "item": obj})
	}
	event("response.completed", map[string]any{"type": "response.completed", "response": resp})
}

func emitBufferedMessage(event func(string, map[string]any), item map[string]any, index int) {
	content, _ := item["content"].([]any)
	for partIndex, part := range content {
		m, _ := part.(map[string]any)
		if m["type"] != "output_text" {
			continue
		}
		text := stringValue(m["text"])
		event("response.content_part.added", map[string]any{
			"type": "response.content_part.added", "item_id": item["id"], "output_index": index,
			"content_index": partIndex, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		})
		event("response.output_text.delta", map[string]any{
			"type": "response.output_text.delta", "item_id": item["id"], "output_index": index,
			"content_index": partIndex, "delta": text,
		})
		event("response.output_text.done", map[string]any{
			"type": "response.output_text.done", "item_id": item["id"], "output_index": index,
			"content_index": partIndex, "text": text,
		})
	}
}

// webSearch 调用 DuckDuckGo HTML 搜索并提取标题/链接/摘要。
func webSearch(query string) (string, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return "", errors.New("query 不能为空")
	}
	ctx, cancel := context.WithTimeout(context.Background(), webSearchTimeout)
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
	var sb strings.Builder
	sb.WriteString("来源：DuckDuckGo\n")
	for i, r := range results {
		fmt.Fprintf(&sb, "%d. %s\n   %s\n", i+1, r.title, r.link)
		if r.snippet != "" {
			sb.WriteString("   " + r.snippet + "\n")
		}
	}
	return logfmt.Truncate(sb.String(), webToolMaxTextBytes), nil
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
func webFetch(rawURL string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), webFetchTimeout)
	defer cancel()
	body, contentType, err := fetchRaw(ctx, rawURL, webToolMaxResponseBytes)
	if err != nil {
		return "", err
	}
	if !isTextContentType(contentType) {
		return "", fmt.Errorf("非文本内容类型 %q", contentType)
	}
	return logfmt.Truncate(string(body), webToolMaxTextBytes), nil
}

func isTextContentType(ct string) bool {
	ct = strings.ToLower(strings.TrimSpace(strings.Split(ct, ";")[0]))
	return strings.HasPrefix(ct, "text/") || ct == "application/json" ||
		ct == "application/xml" || ct == "application/xhtml+xml"
}

// fetchRaw 校验目标、执行请求并读取有界响应；重定向逐跳校验。
func fetchRaw(ctx context.Context, rawURL string, maxBytes int64) ([]byte, string, error) {
	if err := validateWebURL(ctx, rawURL); err != nil {
		return nil, "", err
	}
	client := &http.Client{
		Timeout: webFetchTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("重定向次数过多")
			}
			return validateWebURL(req.Context(), req.URL.String())
		},
		// 用实际拨号地址兜底校验，关闭 validateWebURL 解析与 http.Client 再解析之间
		// 的 DNS rebinding 窗口。显式不用 HTTP(S)_PROXY：代理会在网关之外解析目标，
		// 使这里的 IP 校验失效。
		Transport: &http.Transport{DialContext: dialWebFetch},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, http.NoBody)
	if err != nil {
		return nil, "", fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("User-Agent", "wokbuddy-demon/local-web-tools")
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("上游返回 HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("读取响应失败: %w", err)
	}
	if int64(len(body)) > maxBytes {
		return nil, "", fmt.Errorf("响应超过 %d 字节上限", maxBytes)
	}
	return body, resp.Header.Get("Content-Type"), nil
}

// validateWebURL 只允许 http/https 且目标不得解析到内网/回环地址。
func validateWebURL(ctx context.Context, raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("非法 URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("只允许 http/https，当前 %q", u.Scheme)
	}
	if u.Hostname() == "" {
		return errors.New("URL 缺少主机名")
	}
	return checkHostIP(ctx, u.Hostname())
}

// checkHostIP 拒绝字面或 DNS 解析到私网/回环/链路本地/未指定/组播的地址。
func checkHostIP(ctx context.Context, host string) error {
	if ip := net.ParseIP(host); ip != nil {
		if isBlockedIP(ip) {
			return fmt.Errorf("拒绝访问内网地址 %s", ip)
		}
		return nil
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("解析域名失败: %w", err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("域名 %s 无解析结果", host)
	}
	for _, addr := range addrs {
		if isBlockedIP(addr.IP) {
			return fmt.Errorf("拒绝访问内网地址 %s（解析自 %s）", addr.IP, host)
		}
	}
	return nil
}

func isBlockedIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast()
}

// dialWebFetch 解析目标域名，校验所有解析结果后直接连接已验证的 IP。
// 只校验再交给默认 dialer 重新解析仍留有 DNS rebinding 窗口，故这里不二次解析。
func dialWebFetch(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
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
		return nil, fmt.Errorf("域名 %s 无解析结果", host)
	}
	for _, ip := range ips {
		if isBlockedIP(ip) {
			return nil, fmt.Errorf("拒绝访问内网地址 %s（解析自 %s）", ip, host)
		}
	}
	var dialer net.Dialer
	var lastErr error
	for _, ip := range ips {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}
