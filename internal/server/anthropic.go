// anthropic.go Anthropic Messages 入站适配（issue #51）：
// 把 /v1/messages 的请求体转换为内部 OpenAI chat 请求，复用 chatFlow 的
// 选号/轮转/上游转发；响应再转换回 Anthropic Message / SSE 事件形态。
// 协议转换集中在本文件，核心业务不认识 Anthropic 形态。
package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/logfmt"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

var (
	errEmptyRequestBody       = errors.New("empty request body")
	errModelIsRequired        = errors.New("model is required")
	errMessagesIsRequired     = errors.New("messages is required")
	errAnthropicMessageFailed = errors.New("anthropicMessage failed")
	errToolUseNameIsRequired  = errors.New("tool_use name is required")
)

// anthropicMessagesHandler 是 /v1/messages 的入口（鉴权由 withClientAuth 负责）。
func (h *Handler) anthropicMessagesHandler(w http.ResponseWriter, r *http.Request) {
	in, out, ok := newAnthropicChatInput(w, r, h.cfg.KeepaliveInterval, h.cfg.MaxBodyBytes)
	if !ok {
		return
	}

	h.serveChat(r, &in, out)
}

// anthropicCountTokensHandler 返回 Anthropic 形态的输入 token 估算。
// 网关没有本地 tokenizer；这里按请求 JSON 文本字节数做近似（4 字节 ≈ 1 token），
// 只用于客户端上下文预算，不作为计费依据。
func (h *Handler) anthropicCountTokensHandler(w http.ResponseWriter, r *http.Request) {
	limitRequestBody(w, r, h.cfg.MaxBodyBytes)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		if isBodyTooLarge(err) {
			writeAnthropicError(w, http.StatusRequestEntityTooLarge, "request_too_large",
				bodyTooLargeMessage(h.cfg.MaxBodyBytes), "缩小请求体或调大 server.max_body_bytes")

			return
		}

		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error(), "")

		return
	}

	var iface any
	if err := json.Unmarshal(body, &iface); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body", "")
		return
	}

	obj, _ := iface.(map[string]any)

	model, _ := obj["model"].(string)
	if strings.TrimSpace(model) == "" {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", errModelIsRequired.Error(), "")
		return
	}
	// 与 /v1/messages 同口径：白名单校验前先应用后台模型改写，避免同一模型在
	// 两个端点上判定不一致。
	if target := h.backgroundTarget(model); target != "" {
		model = target
	}

	if !h.countTokensModelAllowed(w, r, model) {
		return
	}

	tokens := max((countJSONTextBytes(iface)+3)/4, 1)
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": tokens})
}

// countTokensModelAllowed 校验 count_tokens 的命名密钥模型白名单；与
// /v1/messages 同口径：先解析别名与 key 出口绑定，再按最终 realm:bare 匹配。
// 返回 false 时已写出 Anthropic 错误。
func (h *Handler) countTokensModelAllowed(w http.ResponseWriter, r *http.Request, model string) bool {
	keyUsage2 := keyUsageFrom(r.Context())
	if keyUsage2 == nil {
		return true
	}

	realm, bare, err := resolveKeyModel(model, h.cfg.ModelAliases, keyRealmFrom(r.Context()))
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error(), "")
		return false
	}

	if h.cfg.Keys.ModelAllowed(keyUsage2.id, realm+":"+bare) {
		return true
	}

	h.cfg.Keys.RecordRejected(keyUsage2.id)
	writeAnthropicError(w, http.StatusForbidden, "permission_error", "model is not allowed for this API key", "")

	return false
}

// countJSONTextBytes 汇总 JSON 中所有字符串值的字节数（键名与结构符号不计）。
func countJSONTextBytes(v any) int {
	switch x := v.(type) {
	case string:
		return len(x)
	case []any:
		n := 0
		for _, item := range x {
			n += countJSONTextBytes(item)
		}

		return n
	case map[string]any:
		n := 0
		for _, item := range x {
			n += countJSONTextBytes(item)
		}

		return n
	default:
		return 0
	}
}

// newAnthropicChatInput 读取并转换 Anthropic 请求体；失败时已写出 Anthropic 错误。
func newAnthropicChatInput(
	w http.ResponseWriter,
	r *http.Request,
	keepalive time.Duration,
	maxBodyBytes int64,
) (chatInput, chatResponder, bool) {
	limitRequestBody(w, r, maxBodyBytes)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		if isBodyTooLarge(err) {
			writeAnthropicError(w, http.StatusRequestEntityTooLarge, "request_too_large",
				bodyTooLargeMessage(maxBodyBytes), "缩小请求体或调大 server.max_body_bytes")

			return chatInput{}, nil, false
		}

		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error(), "")

		return chatInput{}, nil, false
	}

	openAI, model, stream, err := anthropicToOpenAI(body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error(), "")
		return chatInput{}, nil, false
	}

	chatInput2 := chatInput{
		body:           openAI,
		stream:         stream,
		model:          model,
		clientIP:       upstream.ExtractClientIP(r),
		convReqID:      r.Header.Get("X-Conversation-Request-Id"),
		traceID:        r.Header.Get("X-Trace-Id"),
		inboundSession: inboundSessionMetaFrom(r),
		trace:          requestTraceFrom(r),
		keyUsage:       keyUsageFrom(r.Context()),
		keyRealm:       keyRealmFrom(r.Context()),
	}

	return chatInput2, &anthropicResponder{w: w, model: model, keepalive: keepalive}, true
}

// anthropicRequest 只声明转换需要的字段；未知字段由客户端协议自行演进，忽略。
type anthropicRequest struct {
	Model         string            `json:"model"`
	MaxTokens     int               `json:"max_tokens"`
	System        json.RawMessage   `json:"system"`
	Messages      []json.RawMessage `json:"messages"`
	Tools         []map[string]any  `json:"tools"`
	ToolChoice    json.RawMessage   `json:"tool_choice"`
	Temperature   *float64          `json:"temperature"`
	TopP          *float64          `json:"top_p"`
	StopSequences []string          `json:"stop_sequences"`
	Stream        bool              `json:"stream"`
	Thinking      json.RawMessage   `json:"thinking"`
	OutputConfig  json.RawMessage   `json:"output_config"`
}

// anthropicToOpenAI 把 Anthropic Messages 请求转换为 OpenAI chat/completions 请求体。
// 返回转换后的 body、原始模型名与 stream 标志。
func anthropicToOpenAI(body []byte) ([]byte, string, bool, error) {
	req, err := decodeAnthropicRequest(body)
	if err != nil {
		return nil, "", false, err
	}

	out, err := buildOpenAIRequest(req)
	if err != nil {
		return nil, "", false, err
	}

	converted, err := json.Marshal(out)
	if err != nil {
		return nil, "", false, fmt.Errorf("encode request: %w", err)
	}

	return converted, req.Model, req.Stream, nil
}

// decodeAnthropicRequest 解码并校验必填字段。
func decodeAnthropicRequest(body []byte) (*anthropicRequest, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, errEmptyRequestBody
	}

	var req anthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("invalid JSON body: %w", err)
	}

	if strings.TrimSpace(req.Model) == "" {
		return nil, errModelIsRequired
	}

	if len(req.Messages) == 0 {
		return nil, errMessagesIsRequired
	}

	return &req, nil
}

// buildOpenAIRequest 组装 OpenAI chat 请求体。
func buildOpenAIRequest(req *anthropicRequest) (map[string]any, error) {
	out := map[string]any{"model": req.Model, "stream": req.Stream}

	msgs := make([]any, 0, len(req.Messages)+1)
	if system := anthropicSystemContent(req.System); system != nil {
		msgs = append(msgs, map[string]any{"role": "system", "content": system})
	}

	for _, raw := range req.Messages {
		converted, err := anthropicMessage(raw)
		if err != nil {
			return nil, err
		}

		msgs = append(msgs, converted...)
	}

	out["messages"] = msgs
	applyAnthropicParams(out, req)

	return out, nil
}

// applyAnthropicParams 映射采样参数、工具与 thinking 等可选字段。
func applyAnthropicParams(out map[string]any, req *anthropicRequest) {
	applyAnthropicSampling(out, req)
	applyAnthropicTools(out, req)
	applyAnthropicThinking(out, req)
}

// applyAnthropicSampling 映射 max_tokens / temperature / top_p / stop。
func applyAnthropicSampling(out map[string]any, req *anthropicRequest) {
	if req.MaxTokens > 0 {
		out["max_tokens"] = req.MaxTokens
	}

	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}

	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}

	if len(req.StopSequences) > 0 {
		out["stop"] = req.StopSequences
	}
}

// applyAnthropicTools 映射 tools / tool_choice 与并行控制。
func applyAnthropicTools(out map[string]any, req *anthropicRequest) {
	if len(req.Tools) > 0 {
		out["tools"] = anthropicTools(req.Tools)
	}

	choice := anthropicToolChoice(req.ToolChoice)
	if choice == nil {
		return
	}

	out["tool_choice"] = choice
	// Anthropic 把并行控制放在 tool_choice.disable_parallel_tool_use；
	// chat/completions 用独立字段表达。
	if anthropicDisableParallel(req.ToolChoice) {
		out["parallel_tool_calls"] = false
	}
}

// applyAnthropicThinking 映射 thinking 与 output_config。
func applyAnthropicThinking(out map[string]any, req *anthropicRequest) {
	if thinking := anthropicThinking(req.Thinking); thinking != nil {
		out["thinking"] = thinking
	}

	effort, format := anthropicOutputConfig(req.OutputConfig)
	if format != nil {
		out["response_format"] = format
	}

	if effort != "" {
		out["reasoning_effort"] = effort
	}
}

// anthropicOutputConfig 读取 output_config.effort / output_config.format。
// format 是 Anthropic 的结构化输出扩展，映射到 chat/completions response_format。
func anthropicOutputConfig(raw json.RawMessage) (string, any) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return "", nil
	}

	var cfg struct {
		Effort string         `json:"effort"`
		Format map[string]any `json:"format"`
	}
	if json.Unmarshal(raw, &cfg) != nil {
		return "", nil
	}

	format := anthropicJSONOutputFormat(cfg.Format)

	return strings.TrimSpace(cfg.Effort), format
}

// anthropicJSONOutputFormat 把 Anthropic JSONOutputFormat 转成 chat response_format。
func anthropicJSONOutputFormat(format map[string]any) any {
	if format == nil || stringValue(format["type"]) != "json_schema" {
		return nil
	}

	schema, ok := format["schema"]
	if !ok {
		return nil
	}

	return map[string]any{
		"type": "json_schema",
		"json_schema": map[string]any{
			"name": "output", "schema": schema, "strict": true,
		},
	}
}

// anthropicSystemContent 转换 system：无 cache_control 时压成字符串（与旧行为一致），
// 带 cache_control 时保留 text part 数组，让上游 prompt cache 标记不被网关吞掉。
func anthropicSystemContent(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}

	var text string
	if json.Unmarshal(raw, &text) == nil {
		if text == "" {
			return nil
		}

		return text
	}

	var blocks []map[string]any
	if json.Unmarshal(raw, &blocks) != nil {
		return nil
	}

	parts := textParts(blocks)
	if len(parts) == 0 {
		return nil
	}

	if !hasCacheControl(parts) {
		return anthropicText(raw) // 无缓存标记：保持旧版 \n\n 拼接
	}

	return parts
}

// textParts 提取 text block（保留 cache_control），忽略其它类型。
func textParts(blocks []map[string]any) []any {
	parts := make([]any, 0, len(blocks))
	for _, block := range blocks {
		if part := textPart(block); part != nil {
			parts = append(parts, part)
		}
	}

	return parts
}

// textPart 把单个 Anthropic text block 转为 OpenAI text part（保留 cache_control）。
func textPart(block map[string]any) map[string]any {
	if block["type"] != "text" {
		return nil
	}

	text, _ := block["text"].(string)
	if text == "" {
		return nil
	}

	part := map[string]any{"type": "text", "text": text}
	if cc, ok := block["cache_control"]; ok {
		part["cache_control"] = cc
	}

	return part
}

// anthropicText 提取 Anthropic system / 内容块里的纯文本（字符串、raw JSON
// 或 text block 数组）。
func anthropicText(raw any) string {
	switch item := raw.(type) {
	case nil:
		return ""
	case string:
		return item
	case json.RawMessage:
		var decoded any
		if json.Unmarshal(item, &decoded) != nil {
			return ""
		}

		return anthropicText(decoded)
	case []any:
		return anthropicTextBlocks(item)
	case map[string]any:
		if item["type"] == "text" {
			text, _ := item["text"].(string)
			return text
		}

		return ""
	default:
		return ""
	}
}

// anthropicTextBlocks 拼接 text block 数组的文本。
func anthropicTextBlocks(blocks []any) string {
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if text := anthropicText(block); text != "" {
			parts = append(parts, text)
		}
	}

	return strings.Join(parts, "\n\n")
}

// anthropicMessage 把单条 Anthropic 消息转换为 0..N 条 OpenAI 消息。
// user 里的 tool_result 拆成 role=tool，其余内容合并为一条 user；assistant 的
// tool_use 转成 tool_calls。thinking / redacted_thinking 不回填（上游无对应语义）。
func anthropicMessage(raw json.RawMessage) ([]any, error) {
	var msg struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil, fmt.Errorf("invalid message: %w", err)
	}

	if msg.Role != "user" && msg.Role != "assistant" {
		return nil, fmt.Errorf("unsupported message role %q: %w", msg.Role, errAnthropicMessageFailed)
	}

	var text string
	if json.Unmarshal(msg.Content, &text) == nil {
		return []any{map[string]any{"role": msg.Role, "content": text}}, nil
	}

	var blocks []map[string]any
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		return nil, fmt.Errorf("invalid %s content: %w", msg.Role, err)
	}

	if msg.Role == "assistant" {
		return anthropicAssistantMessage(blocks)
	}

	return anthropicUserMessage(blocks), nil
}

// anthropicAssistantMessage 合并 assistant 文本与 tool_use 为一条 OpenAI 消息。
func anthropicAssistantMessage(blocks []map[string]any) ([]any, error) {
	var (
		texts     []string
		toolCalls []any
	)

	for _, block := range blocks {
		switch block["type"] {
		case "text":
			if text, ok := block["text"].(string); ok && text != "" {
				texts = append(texts, text)
			}
		case "tool_use":
			call, err := anthropicToolUseCall(block)
			if err != nil {
				return nil, err
			}

			toolCalls = append(toolCalls, call)
		}
	}

	msg := map[string]any{"role": "assistant", "content": strings.Join(texts, "\n")}
	if parts := textParts(blocks); hasCacheControl(parts) {
		msg["content"] = parts
	}

	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}

	return []any{msg}, nil
}

// anthropicToolUseCall 把单个 tool_use 块转成 OpenAI tool_call。
func anthropicToolUseCall(block map[string]any) (any, error) {
	id, _ := block["id"].(string)

	name, _ := block["name"].(string)
	if name == "" {
		return nil, errToolUseNameIsRequired
	}

	input := block["input"]
	if input == nil {
		input = map[string]any{}
	}

	args, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("tool_use input: %w", err)
	}

	return map[string]any{
		"id":   id,
		"type": "function",
		"function": map[string]any{
			"name":      name,
			"arguments": string(args),
		},
	}, nil
}

// anthropicUserMessage 把 tool_result 拆成独立 tool 消息，其余文本/图片合并为 user。
func anthropicUserMessage(blocks []map[string]any) []any {
	out := make([]any, 0, len(blocks))

	var parts []any

	for _, block := range blocks {
		switch block["type"] {
		case "text":
			if part := textPart(block); part != nil {
				parts = append(parts, part)
			}
		case "image":
			if part := anthropicImagePart(block); part != nil {
				parts = append(parts, part)
			}
		case "tool_result":
			id, _ := block["tool_use_id"].(string)
			text, images := anthropicToolResult(block["content"])
			parts = append(parts, images...)
			out = append(out, map[string]any{
				"role":         "tool",
				"tool_call_id": id,
				"content":      text,
			})
		}
	}

	if len(parts) > 0 {
		out = append(out, map[string]any{"role": "user", "content": openAIContent(parts)})
	}

	if len(out) == 0 {
		out = append(out, map[string]any{"role": "user", "content": ""})
	}

	return out
}

// anthropicToolResult 拆分 tool_result.content：文本压成 tool 消息，图片转成
// user content parts（Chat Completions 的 tool 消息不承载图片）。
func anthropicToolResult(raw any) (string, []any) {
	blocks, ok := raw.([]any)
	if !ok {
		return anthropicText(raw), nil
	}

	var texts []string

	images := make([]any, 0)

	for _, item := range blocks {
		block, _ := item.(map[string]any)
		if block == nil {
			continue
		}

		if block["type"] == "image" {
			if image := anthropicImagePart(block); image != nil {
				images = append(images, image)
			}

			continue
		}

		if text := anthropicText(block); text != "" {
			texts = append(texts, text)
		}
	}

	return strings.Join(texts, "\n\n"), images
}

// anthropicImagePart 把 Anthropic image block 转为 OpenAI image_url part。
func anthropicImagePart(block map[string]any) map[string]any {
	source, _ := block["source"].(map[string]any)
	if source == nil {
		return nil
	}

	switch source["type"] {
	case "base64":
		media, _ := source["media_type"].(string)

		data, _ := source["data"].(string)
		if data == "" {
			return nil
		}

		return map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": "data:" + media + ";base64," + data},
		}
	case "url":
		if url, _ := source["url"].(string); url != "" {
			return map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}}
		}
	}

	return nil
}

// hasCacheControl 报告 parts 中是否有 cache_control（有则不压缩成字符串）。
func hasCacheControl(parts []any) bool {
	for _, part := range parts {
		if m, ok := part.(map[string]any); ok {
			if _, exists := m["cache_control"]; exists {
				return true
			}
		}
	}

	return false
}

// openAIContent 纯文本时压成字符串，含图片时保留 parts 数组（上游多模态形态）。
func openAIContent(parts []any) any {
	allText := true

	var builder strings.Builder

	for _, part := range parts {
		m, _ := part.(map[string]any)
		if m == nil || m["type"] != "text" {
			allText = false
			break
		}

		if _, ok := m["cache_control"]; ok {
			allText = false // 保留 parts 数组，cache_control 不能在压缩成字符串时丢失
			break
		}

		text, _ := m["text"].(string)

		if builder.Len() > 0 {
			builder.WriteString("\n")
		}

		builder.WriteString(text)
	}

	if allText {
		return builder.String()
	}

	return parts
}

// anthropicTools 把 Anthropic tools 转为 OpenAI function tools。
func anthropicTools(tools []map[string]any) []any {
	out := make([]any, 0, len(tools))
	for _, tool := range tools {
		name, _ := tool["name"].(string)
		if name == "" {
			continue
		}

		fn := map[string]any{"name": name}
		if desc, _ := tool["description"].(string); desc != "" {
			fn["description"] = desc
		}

		if schema, ok := tool["input_schema"]; ok {
			fn["parameters"] = schema
		}

		out = append(out, map[string]any{"type": "function", "function": fn})
	}

	return out
}

// anthropicToolChoice 把 Anthropic tool_choice 映射为 OpenAI tool_choice。
func anthropicToolChoice(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}

	var choice struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &choice) != nil {
		return nil
	}

	switch choice.Type {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		if choice.Name == "" {
			return nil
		}

		return map[string]any{"type": "function", "function": map[string]any{"name": choice.Name}}
	default:
		return nil
	}
}

// anthropicDisableParallel 读取 tool_choice.disable_parallel_tool_use。
func anthropicDisableParallel(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}

	var choice struct {
		DisableParallel bool `json:"disable_parallel_tool_use"`
	}
	if json.Unmarshal(raw, &choice) != nil {
		return false
	}

	return choice.DisableParallel
}

// anthropicThinking 把 Anthropic thinking 开关映射为上游 thinking 字段。
func anthropicThinking(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}

	var item struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &item) != nil {
		return nil
	}

	switch item.Type {
	case "enabled":
		return map[string]any{"type": "enabled"}
	case "disabled":
		return map[string]any{"type": "disabled"}
	default:
		return nil
	}
}

// anthropicResponder 把核心的 OpenAI 形态结果编码为 Anthropic 响应。
type anthropicResponder struct {
	w         http.ResponseWriter
	model     string
	keepalive time.Duration
}

// JSON 写出非流式 Anthropic Message。
func (r *anthropicResponder) JSON(status int, body any) {
	writeJSON(r.w, status, anthropicMessageFromOpenAI(body, r.model))
}

// Error 写出 Anthropic 错误信封（保留 gateway_hint 等价字段）。
func (r *anthropicResponder) Error(status int, code, msg, hint string) {
	writeAnthropicError(r.w, status, anthropicErrorType(status), msg, hint)
}

// Header 设置响应头（与 OpenAI 适配器同契约，供 Retry-After 等使用）。
func (r *anthropicResponder) Header(name, value string) {
	r.w.Header().Set(name, value)
}

// Stream 读取上游 OpenAI SSE，逐帧转换为 Anthropic SSE 事件。
func (r *anthropicResponder) Stream(rc io.Reader, hint func(string) string, repair *upstream.MarkupRepair, onErrFrame func(string)) error {
	anthropicStreamWriter2 := newAnthropicStreamWriter(r.w, r.model)

	opts := make([]upstream.StreamOption, 0, 2)
	if onErrFrame != nil {
		opts = append(opts, upstream.WithErrorFrameObserver(onErrFrame))
	}

	opts = append(opts, upstream.WithMarkupRepair(repair))
	if r.keepalive > 0 {
		opts = append(opts, upstream.WithKeepaliveInterval(r.keepalive))
	}

	if err := upstream.StreamHint(anthropicStreamWriter2, rc, hint, opts...); err != nil {
		return fmt.Errorf("stream hint: %w", err)
	}

	return nil
}

// anthropicMessageFromOpenAI 把聚合后的 OpenAI chat.completion 转为 Anthropic Message。
func anthropicMessageFromOpenAI(body any, model string) map[string]any {
	obj, _ := body.(map[string]any)

	id, _ := obj["id"].(string)
	if id == "" {
		id = "msg_gateway"
	}

	choice := firstOpenAIChoice(obj)
	content := make([]any, 0, 2)
	stopReason := "end_turn"

	if choice != nil {
		msg, _ := choice["message"].(map[string]any)
		if reasoning := deltaReasoning(msg); reasoning != "" {
			content = append(content, map[string]any{
				"type": "thinking", "thinking": reasoning, "signature": "",
			})
		}

		text := responsesMessageText(msg["content"])
		if text == "" {
			text = stringValue(msg["refusal"])
		}

		if text != "" {
			content = append(content, map[string]any{"type": "text", "text": text})
		}

		tools := anthropicToolUseBlocks(msg["tool_calls"])
		content = append(content, tools...)
		stopReason = anthropicStopReason(stringValue(choice["finish_reason"]), len(tools) > 0)
	}

	if len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": ""})
	}

	usage := anthropicUsage(obj["usage"])

	return map[string]any{
		"id":            id,
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage":         usage,
	}
}

// firstOpenAIChoice 取 choices[0]。
func firstOpenAIChoice(obj map[string]any) map[string]any {
	choices, _ := obj["choices"].([]any)
	if len(choices) == 0 {
		return nil
	}

	choice, _ := choices[0].(map[string]any)

	return choice
}

// anthropicToolUseBlocks 把 OpenAI tool_calls 转为 Anthropic tool_use 块。
func anthropicToolUseBlocks(raw any) []any {
	calls := mapItems(raw)

	out := make([]any, 0, len(calls))
	for i, call := range calls {
		fn, _ := call["function"].(map[string]any)

		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}

		id, _ := call["id"].(string)
		if id == "" {
			id = fmt.Sprintf("toolu_%d", i)
		}

		input := map[string]any{}
		if args, _ := fn["arguments"].(string); args != "" {
			_ = json.Unmarshal([]byte(args), &input)
		}

		out = append(out, map[string]any{
			"type":  "tool_use",
			"id":    id,
			"name":  name,
			"input": input,
		})
	}

	return out
}

// anthropicUsage 映射 token 计数；上游缺失时给 0（Anthropic 要求字段存在）。
func anthropicUsage(raw any) map[string]any {
	usage, _ := raw.(map[string]any)
	input := intValue(usage["prompt_tokens"])
	output := intValue(usage["completion_tokens"])
	cached, cacheWrite := responsesCachedTokens(usage)
	// Anthropic 的 input_tokens 不含缓存命中/写入；OpenAI 的 prompt_tokens 含。
	input = max(input-cached-cacheWrite, 0)

	usageOut := map[string]any{
		"input_tokens":                input,
		"output_tokens":               output,
		"cache_creation_input_tokens": cacheWrite,
		"cache_read_input_tokens":     cached,
	}
	if thinking := responsesReasoningTokens(usage); thinking > 0 {
		usageOut["output_tokens_details"] = map[string]any{"thinking_tokens": thinking}
	}

	return usageOut
}

// anthropicStopReason 映射 OpenAI finish_reason → Anthropic stop_reason。
func anthropicStopReason(reason string, hasTool bool) string {
	switch reason {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter":
		return "refusal"
	case "stop":
		if hasTool {
			return "tool_use"
		}

		return "end_turn"
	default:
		return "end_turn"
	}
}

// anthropicErrorType 按 HTTP 状态码映射 Anthropic error.type。
func anthropicErrorType(status int) string {
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusServiceUnavailable, http.StatusBadGateway:
		return "overloaded_error"
	default:
		return "api_error"
	}
}

// writeAnthropicError 写出 Anthropic 错误信封。
func writeAnthropicError(w http.ResponseWriter, status int, errType, msg, hint string) {
	e := map[string]any{"type": errType, "message": msg}
	if hint != "" {
		e["gateway_hint"] = hint
	}

	writeJSON(w, status, map[string]any{"type": "error", "error": e})
}

// anthropicStreamWriter 接收 upstream.StreamHint 写出的 OpenAI SSE 帧，转换为
// Anthropic SSE 事件。实现 http.ResponseWriter + Flusher + Unwrap，使上游写路径
// 的响应头、逐帧 flush 与写截止（issue #53）继续作用于底层连接。
type anthropicStreamWriter struct {
	w      http.ResponseWriter
	fl     http.Flusher
	model  string
	logger *log.Logger

	pending         []byte
	started         bool
	errored         bool
	finished        bool
	blockOpen       bool
	blockIndex      int
	blockType       string // text / thinking / tool_use
	toolIndex       int    // 当前活动工具块对应的 OpenAI delta.tool_calls index
	toolBlocks      map[int]*anthropicToolBlock
	stopReason      string
	inputTokens     int
	outputTokens    int
	cachedTokens    int
	cacheWrite      int
	reasoningTokens int
}

func newAnthropicStreamWriter(w http.ResponseWriter, model string) *anthropicStreamWriter {
	fl, _ := w.(http.Flusher)

	return &anthropicStreamWriter{
		w: w, fl: fl, model: model, blockIndex: -1, toolIndex: -1,
		toolBlocks: map[int]*anthropicToolBlock{},
		logger:     log.Default(),
	}
}

// anthropicToolBlock 记录已开启工具块的 Anthropic index 与 id（参数续片复用）。
type anthropicToolBlock struct {
	index int
	id    string
	name  string
	// args 聚合本块所有 input_json_delta 分片，供收尾时校验是否可解析。
	args strings.Builder
}

// Header 透传底层响应头（StreamHint 在此设置 SSE 头）。
func (a *anthropicStreamWriter) Header() http.Header { return a.w.Header() }

// WriteHeader 透传状态码。
func (a *anthropicStreamWriter) WriteHeader(code int) { a.w.WriteHeader(code) }

// Unwrap 让 http.ResponseController 继续找到底层连接（写截止）。
func (a *anthropicStreamWriter) Unwrap() http.ResponseWriter { return a.w }

// Flush 透传逐帧刷新。
func (a *anthropicStreamWriter) Flush() {
	if a.fl != nil {
		a.fl.Flush()
	}
}

// Write 按行拆分开源 SSE，转换 data 帧；返回错误会中止上游透传。
func (a *anthropicStreamWriter) Write(nums []byte) (int, error) {
	a.pending = append(a.pending, nums...)
	for {
		i := bytes.IndexByte(a.pending, '\n')
		if i < 0 {
			break
		}

		line := strings.TrimRight(string(a.pending[:i]), "\r")

		a.pending = a.pending[i+1:]
		if err := a.handleLine(line); err != nil {
			return 0, err
		}
	}

	return len(nums), nil
}

// handleLine 处理单行：data 帧转换，[DONE] 收尾，注释/空行忽略。
func (a *anthropicStreamWriter) handleLine(line string) error {
	if a.finished {
		return nil
	}
	// SSE 注释（如上游静默心跳 ": keepalive"）原样转发；上游写路径已设置
	// 单次写截止，这里只负责补帧尾并 flush。
	if strings.HasPrefix(line, ":") {
		if _, err := io.WriteString(a.w, line+"\n\n"); err != nil {
			return fmt.Errorf("handleLine: %w", err)
		}

		a.Flush()

		return nil
	}

	if payload, ok := strings.CutPrefix(line, "data: "); ok {
		if payload == "[DONE]" {
			return a.finish()
		}

		return a.handleChunk(payload)
	}

	return nil
}

// handleChunk 解析 OpenAI chunk 并写出对应 Anthropic 事件。
func (a *anthropicStreamWriter) handleChunk(payload string) error {
	var chunk map[string]any
	if json.Unmarshal([]byte(payload), &chunk) != nil {
		return nil //nolint:nilerr // 非 JSON 数据帧（上游噪声）直接丢弃
	}

	if errObj, ok := chunk["error"].(map[string]any); ok {
		return a.handleUpstreamError(errObj)
	}

	if err := a.ensureStart(chunk); err != nil {
		return err
	}

	a.captureUsage(chunk)

	choice := firstOpenAIChoice(chunk)
	if choice == nil {
		return nil
	}

	if delta, _ := choice["delta"].(map[string]any); delta != nil {
		if err := a.applyDelta(delta); err != nil {
			return err
		}
	}

	if reason := stringValue(choice["finish_reason"]); reason != "" {
		a.stopReason = anthropicStopReason(reason, len(a.toolBlocks) > 0)
	}

	return nil
}

// handleUpstreamError 把上游 error 帧转成 Anthropic error 事件并标记流已失败。
func (a *anthropicStreamWriter) handleUpstreamError(errObj map[string]any) error {
	msg, _ := errObj["message"].(string)
	if msg == "" {
		msg = "upstream stream error"
	}

	a.errored = true
	errBody := map[string]any{"type": "api_error", "message": msg}
	// gateway_hint 由上游流层附在 error 帧上；流式错误不能把它丢掉。
	if hint, _ := errObj["gateway_hint"].(string); hint != "" {
		errBody["gateway_hint"] = hint
	}

	return a.event("error", map[string]any{"type": "error", "error": errBody})
}

// applyDelta 处理一个 choice delta 的文本、思考与工具调用增量。
func (a *anthropicStreamWriter) applyDelta(delta map[string]any) error {
	text, _ := delta["content"].(string)
	if text == "" {
		text, _ = delta["refusal"].(string)
	}

	if text != "" {
		if err := a.emitText(text, "text_delta", "text"); err != nil {
			return err
		}
	}

	if reasoning := deltaReasoning(delta); reasoning != "" {
		if err := a.emitText(reasoning, "thinking_delta", "thinking"); err != nil {
			return err
		}
	}

	return a.emitToolCalls(delta["tool_calls"])
}

// deltaReasoning 读取上游思维链增量（reasoning_content 优先，reasoning 兜底）。
func deltaReasoning(delta map[string]any) string {
	if reasoning, _ := delta["reasoning_content"].(string); reasoning != "" {
		return reasoning
	}

	reasoning, _ := delta["reasoning"].(string)

	return reasoning
}

// ensureStart 在首帧写出 message_start（usage 取该帧可见值，缺失为 0）。
func (a *anthropicStreamWriter) ensureStart(chunk map[string]any) error {
	if a.started {
		return nil
	}

	a.started = true
	a.captureUsage(chunk)

	id := "msg_gateway"

	if chunk != nil {
		if v, _ := chunk["id"].(string); v != "" {
			id = v
		}
	}

	return a.event("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            id,
			"type":          "message",
			"role":          "assistant",
			"model":         a.model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": a.inputTokens, "output_tokens": 0},
		},
	})
}

// captureUsage 记录 chunk.usage 的 token 数（末帧通常带 usage）。
func (a *anthropicStreamWriter) captureUsage(chunk map[string]any) {
	usage, _ := chunk["usage"].(map[string]any)
	if usage == nil {
		return
	}

	if n := intValue(usage["prompt_tokens"]); n > 0 {
		a.inputTokens = n
	}

	if n := intValue(usage["completion_tokens"]); n > 0 {
		a.outputTokens = n
	}

	if cached, cacheWrite := responsesCachedTokens(usage); cached > 0 || cacheWrite > 0 {
		a.cachedTokens, a.cacheWrite = cached, cacheWrite
	}

	if n := responsesReasoningTokens(usage); n > 0 {
		a.reasoningTokens = n
	}
}

// emitText 追加文本/思考增量；块类型变化时先关闭旧块再开新块。
func (a *anthropicStreamWriter) emitText(text, deltaType, blockType string) error {
	if err := a.ensureBlock(blockType); err != nil {
		return err
	}

	delta := map[string]any{"type": deltaType}
	if blockType == "thinking" {
		delta["thinking"] = text
	} else {
		delta["text"] = text
	}

	return a.event("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": a.blockIndex,
		"delta": delta,
	})
}

// errToolCallSkipped 工具调用缺少 name（无法构造 tool_use 块），跳过该片。
var errToolCallSkipped = errors.New("tool call without name")

// emitToolCalls 处理 OpenAI delta.tool_calls：首片开 tool_use 块，后续片发参数增量。
func (a *anthropicStreamWriter) emitToolCalls(raw any) error {
	for _, call := range mapItems(raw) {
		idx := intValue(call["index"])

		block, err := a.ensureToolBlock(idx, call)
		if errors.Is(err, errToolCallSkipped) {
			continue
		}

		if err != nil {
			return err
		}

		if args := toolCallArguments(call); args != "" {
			block.args.WriteString(args)

			if err := a.event("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": block.index,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
			}); err != nil {
				return err
			}
		}
	}

	return nil
}

// ensureToolBlock 确保该 OpenAI 工具 index 有一个可写的 Anthropic 块。
// 首次出现 → 新开块；块已关闭（工具之间插入正文）→ 用同 id 重开继续拼参数。
func (a *anthropicStreamWriter) ensureToolBlock(idx int, call map[string]any) (*anthropicToolBlock, error) {
	block, exists := a.toolBlocks[idx]
	if !exists {
		name, id := toolCallIdentity(call)
		if name == "" {
			return nil, errToolCallSkipped
		}

		if id == "" {
			id = fmt.Sprintf("toolu_%d", idx)
		}

		if err := a.closeBlock(); err != nil {
			return nil, err
		}

		a.blockIndex++
		block = &anthropicToolBlock{index: a.blockIndex, id: id, name: name}
		a.toolBlocks[idx] = block
		a.toolIndex = idx
		a.blockOpen = true
		a.blockType = "tool_use"

		return block, a.toolBlockStart(block)
	}

	if a.blockOpen && a.blockType == "tool_use" && a.toolIndex == idx {
		return block, nil
	}

	if err := a.closeBlock(); err != nil {
		return nil, err
	}

	a.blockIndex++
	block.index = a.blockIndex
	a.toolIndex = idx
	a.blockOpen = true
	a.blockType = "tool_use"

	return block, a.toolBlockStart(block)
}

// toolBlockStart 写出 tool_use 的 content_block_start。
func (a *anthropicStreamWriter) toolBlockStart(block *anthropicToolBlock) error {
	return a.event("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": block.index,
		"content_block": map[string]any{
			"type": "tool_use", "id": block.id, "name": block.name, "input": map[string]any{},
		},
	})
}

// ensureBlock 确保当前打开的是指定类型块；不同则关闭旧块并开新块。
func (a *anthropicStreamWriter) ensureBlock(blockType string) error {
	if a.blockOpen && a.blockType == blockType {
		return nil
	}

	if err := a.closeBlock(); err != nil {
		return err
	}

	a.blockOpen = true
	a.blockType = blockType
	a.blockIndex++
	start := map[string]any{"type": "content_block_start", "index": a.blockIndex}

	switch blockType {
	case "text":
		start["content_block"] = map[string]any{"type": "text", "text": ""}
	case "thinking":
		// signature 是 Anthropic thinking block 的必需字段；上游不给签名，
		// 这里输出空串占位，保持事件结构可被 SDK 解析。
		start["content_block"] = map[string]any{"type": "thinking", "thinking": "", "signature": ""}
	}

	return a.event("content_block_start", start)
}

// closeBlock 结束当前打开的 content block（幂等）。
func (a *anthropicStreamWriter) closeBlock() error {
	if !a.blockOpen {
		return nil
	}

	a.blockOpen = false

	return a.event("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": a.blockIndex,
	})
}

// validateToolArguments 在流收尾时校验各工具块聚合后的参数是否为合法 JSON。
// 本期只观测，不改写分片：上游截断会在这里留下可检索的 WARN（不含参数原文）。
func (a *anthropicStreamWriter) validateToolArguments() {
	for _, block := range a.toolBlocks {
		args := strings.TrimSpace(block.args.String())
		if args == "" || json.Valid([]byte(args)) {
			continue
		}

		a.logger.Printf("WARN: [anthropic] invalid tool arguments model=%s tool=%s id=%s bytes=%d",
			logfmt.Truncate(a.model, 64), logfmt.Truncate(block.name, 64), logfmt.Truncate(block.id, 64), len(args))
	}
}

// finish 关闭块并写出 message_delta / message_stop（错误流不补正常收尾）。
func (a *anthropicStreamWriter) finish() error {
	if a.finished {
		return nil
	}

	a.finished = true
	if a.errored {
		return nil
	}

	if !a.started {
		if err := a.ensureStart(nil); err != nil {
			return err
		}
	}

	if err := a.closeBlock(); err != nil {
		return err
	}

	a.validateToolArguments()

	reason := a.stopReason
	if reason == "" {
		reason = "end_turn"
	}

	usage := map[string]any{
		"input_tokens":                max(a.inputTokens-a.cachedTokens-a.cacheWrite, 0),
		"output_tokens":               a.outputTokens,
		"cache_creation_input_tokens": a.cacheWrite,
		"cache_read_input_tokens":     a.cachedTokens,
	}
	if a.reasoningTokens > 0 {
		usage["output_tokens_details"] = map[string]any{"thinking_tokens": a.reasoningTokens}
	}

	if err := a.event("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": reason, "stop_sequence": nil},
		"usage": usage,
	}); err != nil {
		return err
	}

	return a.event("message_stop", map[string]any{"type": "message_stop"})
}

// event 写出一条命名 SSE 事件并 flush。
func (a *anthropicStreamWriter) event(name string, payload map[string]any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("event: %w", err)
	}

	if _, err := fmt.Fprintf(a.w, "event: %s\ndata: %s\n\n", name, raw); err != nil {
		return fmt.Errorf("event: %w", err)
	}

	a.Flush()

	return nil
}

// toolCallIdentity 取工具调用的 name/id（可能只在首片出现）。
func toolCallIdentity(call map[string]any) (name, id string) {
	id, _ = call["id"].(string)
	fn, _ := call["function"].(map[string]any)
	name, _ = fn["name"].(string)

	return name, id
}

// toolCallArguments 取 function.arguments 片段。
func toolCallArguments(call map[string]any) string {
	fn, _ := call["function"].(map[string]any)
	args, _ := fn["arguments"].(string)

	return args
}

// stringValue 读取 map 里的字符串值。
func stringValue(v any) string {
	s, _ := v.(string)
	return s
}

// intValue 读取 JSON/内部数值并转 int。内部聚合层会把 created 与模型目录字段
// 构造成 int64，不能只认 JSON 解码的 float64。
func intValue(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	case json.Number:
		parsed, err := n.Int64()
		if err != nil {
			return 0
		}

		return int(parsed)
	default:
		return 0
	}
}
