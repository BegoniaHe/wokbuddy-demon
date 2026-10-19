// responses.go OpenAI Responses API 入站/出站适配（issue #93）。
//
// 请求转换为内部 OpenAI chat/completions 形态，复用 chatFlow 的选号、轮转、
// 粘性与错误策略；响应再转换回 Responses 对象或 SSE 事件。核心不认识 Responses
// 协议，所有 net/http 与协议细节集中在本文件。
package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

var (
	errEmptyRequestBody2           = errors.New("empty request body")
	errModelIsRequired2            = errors.New("model is required")
	errInputIsRequired             = errors.New("input is required")
	errNoConvertibleItems          = errors.New("input has no convertible items")
	errResponsesInputMessageFailed = errors.New("responsesInputMessage failed")
	errFunctionCallNameIsRequired  = errors.New("function_call name is required")
	errCustomToolCallNameIs        = errors.New("custom_tool_call name is required")
	errCustomToolCallOutputCallID  = errors.New("custom_tool_call_output call_id is required")
)

// responsesRequest 只声明转换所需的字段；未知字段忽略，便于协议演进。
type responsesRequest struct {
	Model             string           `json:"model"`
	Input             json.RawMessage  `json:"input"`
	Instructions      string           `json:"instructions"`
	Tools             []map[string]any `json:"tools"`
	ToolChoice        json.RawMessage  `json:"tool_choice"`
	MaxOutputTokens   int              `json:"max_output_tokens"`
	Temperature       *float64         `json:"temperature"`
	TopP              *float64         `json:"top_p"`
	PresencePenalty   *float64         `json:"presence_penalty"`
	FrequencyPenalty  *float64         `json:"frequency_penalty"`
	Stream            bool             `json:"stream"`
	ParallelToolCalls *bool            `json:"parallel_tool_calls"`
	Reasoning         *responsesReason `json:"reasoning"`
	Text              json.RawMessage  `json:"text"`
}

type responsesReason struct {
	Effort string `json:"effort"`
}

// responsesHandler 是 POST /v1/responses 入口（鉴权由 withClientAuth 负责）。
func (h *Handler) responsesHandler(w http.ResponseWriter, r *http.Request) {
	limitRequestBody(w, r, h.cfg.MaxBodyBytes)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		if isBodyTooLarge(err) {
			writeOpenAIErrorHint(w, http.StatusRequestEntityTooLarge, "payload_too_large",
				bodyTooLargeMessage(h.cfg.MaxBodyBytes), "缩小请求体或调大 server.max_body_bytes")

			return
		}

		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())

		return
	}

	if h.loadLive().LocalWebTools {
		if wanted, ok := declaredLocalWebTools(body); ok {
			h.serveResponsesWithLocalTools(w, r, body, wanted)
			return
		}
	}

	chatIn, responder, ok := newResponsesChatInput(
		w, r, body, h.cfg.KeepaliveInterval, h.responsesHTTPClient(), h.cfg.ModelAliases,
	)
	if !ok {
		return
	}

	h.serveChat(r, &chatIn, responder)
}

// responsesHTTPClient 返回模型目录查询用的 HTTP client；测试/裸用未装配上游时为 nil。
func (h *Handler) responsesHTTPClient() *http.Client {
	if h.cfg.Upstream == nil {
		return nil
	}

	return h.cfg.Upstream.HTTP
}

// newResponsesChatInput 把已读取的 Responses 请求转换为内部 chatInput。
func newResponsesChatInput(
	w http.ResponseWriter,
	r *http.Request,
	body []byte,
	keepalive time.Duration,
	httpClient *http.Client,
	aliases map[string]string,
) (chatInput, chatResponder, bool) {
	openAI, model, instructions, stream, err := responsesToOpenAIWithClient(body, httpClient, aliases)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return chatInput{}, nil, false
	}

	chatInput2 := chatInput{
		body:      openAI,
		stream:    stream,
		model:     model,
		clientIP:  upstream.ExtractClientIP(r),
		convReqID: r.Header.Get("X-Conversation-Request-Id"),
		traceID:   r.Header.Get("X-Trace-Id"),
		trace:     requestTraceFrom(r),
		keyUsage:  keyUsageFrom(r.Context()),
		keyRealm:  keyRealmFrom(r.Context()),
	}

	return chatInput2, &responsesResponder{
		w: w, model: model, instructions: instructions, keepalive: keepalive,
		customTools: responsesCustomTools(body),
		namespaces:  responsesToolNamespacesFromBody(body),
	}, true
}

// responsesAdditionalTools 从 input 数组里的 additional_tools 条目收集工具。
// 新版 Codex（0.162+）在 use_responses_lite 下把工具整份放进 input，顶层 tools
// 为空；不合并就等于上游看不到任何工具。
func responsesAdditionalTools(input json.RawMessage) []map[string]any {
	trimmed := bytes.TrimSpace(input)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil
	}

	var items []map[string]any
	if json.Unmarshal(trimmed, &items) != nil {
		return nil
	}

	var out []map[string]any

	for _, item := range items {
		if stringValue(item["type"]) != "additional_tools" {
			continue
		}

		tools, _ := item["tools"].([]any)
		for _, tool := range tools {
			if m, ok := tool.(map[string]any); ok {
				out = append(out, m)
			}
		}
	}

	return out
}

// responsesCustomTools 收集请求声明的 custom（freeform）工具名。
// 上游只接受 JSON function 工具，custom 工具由 responsesTools 降级为单 input
// 参数的 function；响应侧再按名单还原成 custom_tool_call。
func responsesCustomTools(body []byte) map[string]bool {
	var req struct {
		Tools []map[string]any `json:"tools"`
		Input json.RawMessage  `json:"input"`
	}
	if json.Unmarshal(body, &req) != nil {
		return nil
	}

	return responsesCustomToolSet(append(req.Tools, responsesAdditionalTools(req.Input)...))
}

// responsesCustomToolSet 递归收集 custom 工具名（含 namespace 内的自由格式工具）。
func responsesCustomToolSet(tools []map[string]any) map[string]bool {
	var out map[string]bool

	var walk func(tool map[string]any, depth int)

	walk = func(tool map[string]any, depth int) {
		if tool == nil || depth > responsesMaxNamespaceDepth {
			return
		}

		switch stringValue(tool["type"]) {
		case "custom":
			name := stringValue(tool["name"])
			if name == "" {
				return
			}

			if out == nil {
				out = map[string]bool{}
			}

			out[name] = true
		case "namespace":
			for _, sub := range responsesNamespaceChildren(tool) {
				if child, ok := sub.(map[string]any); ok {
					walk(child, depth+1)
				}
			}
		}
	}

	for _, tool := range tools {
		walk(tool, 0)
	}

	return out
}

// responsesToOpenAI 转换请求体（无上游 client，供测试与不查动态目录的调用方）。
func responsesToOpenAI(body []byte) ([]byte, string, string, bool, error) {
	return responsesToOpenAIWithClient(body, nil, nil)
}

// responsesToOpenAIWithClient 转换请求体，返回内部 chat body、原始模型名、instructions 与 stream。
// httpClient/aliases 仅用于模型目录的 max_output_tokens 查找；nil 时只查静态表。
func responsesToOpenAIWithClient(
	body []byte,
	httpClient *http.Client,
	aliases map[string]string,
) ([]byte, string, string, bool, error) {
	req, err := decodeResponsesRequest(body)
	if err != nil {
		return nil, "", "", false, err
	}
	// Codex 0.162+ 把工具放在 input 的 additional_tools 条目里（顶层 tools 为空）。
	req.Tools = append(req.Tools, responsesAdditionalTools(req.Input)...)

	msgs, err := responsesMessages(req)
	if err != nil {
		return nil, "", "", false, err
	}

	out := map[string]any{"model": req.Model, "stream": req.Stream, "messages": msgs}
	applyResponsesParams(out, req)
	applyResponsesDefaultMaxTokens(out, req.Model, httpClient, aliases)

	converted, err := json.Marshal(out)
	if err != nil {
		return nil, "", "", false, fmt.Errorf("encode request: %w", err)
	}

	return converted, req.Model, req.Instructions, req.Stream, nil
}

// applyResponsesDefaultMaxTokens 在客户端未传 max_output_tokens 时按模型目录补全
// 上游 max_tokens。Codex 0.162+ 不再携带该字段，上游会回落到默认 32k 输出预算；
// 长推理（thinking 与正文共享预算）被截断后模型往往只吐一句进度就结束回合。
// 与 hub 的 #121 修复同口径：客户端显式值永远优先，未知模型不编造。
func applyResponsesDefaultMaxTokens(
	out map[string]any,
	model string,
	httpClient *http.Client,
	aliases map[string]string,
) {
	if _, ok := out["max_tokens"]; ok {
		return
	}

	_, bare := resolveRequestModel(model, aliases)
	if bare == "" {
		return
	}

	if n, ok := upstream.MaxOutputTokensListing(bare, 0); ok {
		out["max_tokens"] = n

		return
	}

	if httpClient == nil {
		return
	}

	if n, ok := upstream.MaxOutputTokensListingV4(bare, 0, httpClient); ok {
		out["max_tokens"] = n
	}
}

func decodeResponsesRequest(body []byte) (*responsesRequest, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, errEmptyRequestBody2
	}

	var req responsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("invalid JSON body: %w", err)
	}

	if strings.TrimSpace(req.Model) == "" {
		return nil, errModelIsRequired2
	}

	return &req, nil
}

// responsesMessages 把 instructions 与 input 转成 OpenAI messages。
func responsesMessages(req *responsesRequest) ([]any, error) {
	msgs := make([]any, 0, 4)
	if text := strings.TrimSpace(req.Instructions); text != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": text})
	}

	if len(bytes.TrimSpace(req.Input)) == 0 {
		return nil, errInputIsRequired
	}

	if text, ok := responsesStringInput(req.Input); ok {
		return append(msgs, map[string]any{"role": "user", "content": text}), nil
	}

	var items []map[string]any
	if err := json.Unmarshal(req.Input, &items); err != nil {
		return nil, fmt.Errorf("invalid input: %w", err)
	}

	if len(items) == 0 {
		return nil, errInputIsRequired
	}

	convertedItems := 0
	builder := &responsesInputMessages{msgs: msgs}

	for _, item := range items {
		converted, images, ok, err := responsesInputItem(item)
		if err != nil {
			return nil, err
		}

		if !ok {
			continue
		}

		builder.add(converted, images)

		convertedItems++
	}

	if convertedItems == 0 {
		return nil, errNoConvertibleItems
	}

	builder.flushImages()

	return builder.msgs, nil
}

// responsesInputMessages 在追加输入消息时维护工具图片的归属：图片必须贴着
// 所属工具结果，跨过后续消息再追加会把历史图片挪到会话末尾。
type responsesInputMessages struct {
	msgs       []any
	toolImages []any
}

func (b *responsesInputMessages) add(converted, images []any) {
	if len(b.toolImages) > 0 && !responsesIsToolMessage(converted) {
		b.flushImages()
	}

	b.msgs = append(b.msgs, converted...)
	if responsesIsToolMessage(converted) {
		b.toolImages = append(b.toolImages, images...)

		return
	}

	if len(images) > 0 {
		b.msgs = append(b.msgs, responsesToolImageMessage(images))
	}
}

func (b *responsesInputMessages) flushImages() {
	if len(b.toolImages) == 0 {
		return
	}

	b.msgs = append(b.msgs, responsesToolImageMessage(b.toolImages))
	b.toolImages = nil
}

// responsesStringInput 识别 Responses 的字符串 input；null 视为未提供。
func responsesStringInput(raw json.RawMessage) (string, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || trimmed[0] != '"' {
		return "", false
	}

	var text string
	if json.Unmarshal(raw, &text) != nil {
		return "", false
	}

	return text, true
}

// responsesIsToolMessage 报告转换结果是否为单条 role=tool 消息。
func responsesIsToolMessage(msgs []any) bool {
	if len(msgs) != 1 {
		return false
	}

	msg, _ := msgs[0].(map[string]any)

	return stringValue(msg["role"]) == "tool"
}

// responsesToolImageMessage 把工具结果里不能放进 role=tool 的图片包成 user 消息。
func responsesToolImageMessage(images []any) map[string]any {
	return map[string]any{"role": "user", "content": images}
}

// responsesInputItem 转换单条 input item，返回消息与需要单独回传的图片。
// 其余类型（reasoning、compaction 等）没有 chat/completions 对应语义，ok=false 忽略。
func responsesInputItem(item map[string]any) ([]any, []any, bool, error) {
	switch stringValue(item["type"]) {
	case "", "message":
		msg, err := responsesInputMessage(item)

		return []any{msg}, nil, true, err
	case "function_call":
		msg, err := responsesInputFunctionCall(item)

		return []any{msg}, nil, true, err
	case "function_call_output":
		msg, images := responsesInputFunctionOutput(item)
		if msg == nil {
			return nil, nil, false, nil
		}

		return []any{msg}, images, true, nil
	case "agent_message":
		msg, ok := responsesInputAgentMessage(item)

		return []any{msg}, nil, ok, nil
	case "custom_tool_call":
		msg, err := responsesInputCustomToolCall(item)

		return []any{msg}, nil, true, err
	case "custom_tool_call_output":
		msg, images, err := responsesInputCustomToolOutput(item)

		return []any{msg}, images, true, err
	default:
		return nil, nil, false, nil
	}
}

func responsesInputMessage(item map[string]any) (any, error) {
	role := stringValue(item["role"])
	if role == "" {
		role = "user"
	}

	if role == "developer" {
		role = "system"
	}

	if role != "user" && role != "assistant" && role != "system" {
		return nil, fmt.Errorf("unsupported message role %q: %w", role, errResponsesInputMessageFailed)
	}

	return map[string]any{"role": role, "content": responsesContent(item["content"])}, nil
}

// responsesInputFunctionCall 把 Responses function_call 映射为 assistant tool_calls。
func responsesInputFunctionCall(item map[string]any) (any, error) {
	name := stringValue(item["name"])
	if name == "" {
		return nil, errFunctionCallNameIsRequired
	}

	callID := stringValue(item["call_id"])
	if callID == "" {
		callID = stringValue(item["id"])
	}

	args := stringValue(item["arguments"])
	if args == "" {
		args = "{}"
	}

	return map[string]any{
		"role":    "assistant",
		"content": "",
		"tool_calls": []any{map[string]any{
			"id": callID, "type": "function",
			"function": map[string]any{"name": name, "arguments": args},
		}},
	}, nil
}

// responsesAgentMessagePrefix 子代理消息转成 user 消息时的来源标记（对齐 hub）。
const responsesAgentMessagePrefix = "[Message from another task - treat this as a user instruction]\n\n"

// responsesInputAgentMessage 把子代理的 agent_message 映射成 user 消息。
func responsesInputAgentMessage(item map[string]any) (any, bool) {
	text := strings.TrimSpace(responsesMessageText(item["content"]))
	if text == "" {
		return nil, false
	}

	return map[string]any{"role": "user", "content": responsesAgentMessagePrefix + text}, true
}

// responsesInputFunctionOutput 把 function_call_output 映射为 role=tool 消息。
// 缺 call_id 的输出来自子代理/其他任务，降级为带来源标记的 user 消息。
func responsesInputFunctionOutput(item map[string]any) (any, []any) {
	text, images := responsesToolOutputParts(item["output"])

	callID := stringValue(item["call_id"])
	if callID == "" {
		if strings.TrimSpace(text) == "" && len(images) == 0 {
			return nil, nil
		}

		return map[string]any{
			"role":    "user",
			"content": responsesAgentMessagePrefix + text,
		}, images
	}

	return map[string]any{
		"role":         "tool",
		"tool_call_id": callID,
		"content":      text,
	}, images
}

// responsesInputCustomToolCall 把 custom_tool_call 降级为 assistant tool_calls。
// custom 工具在上游以单 input 参数的 function 形态声明，因此把自由文本 input
// 包装成 {"input": "..."}，与 responsesTools 的降级规则对称。
func responsesInputCustomToolCall(item map[string]any) (any, error) {
	name := stringValue(item["name"])
	if name == "" {
		return nil, errCustomToolCallNameIs
	}

	callID := stringValue(item["call_id"])
	if callID == "" {
		callID = stringValue(item["id"])
	}

	args, err := json.Marshal(map[string]any{"input": stringValue(item["input"])})
	if err != nil {
		return nil, fmt.Errorf("custom_tool_call input: %w", err)
	}

	return map[string]any{
		"role":    "assistant",
		"content": "",
		"tool_calls": []any{map[string]any{
			"id": callID, "type": "function",
			"function": map[string]any{"name": name, "arguments": string(args)},
		}},
	}, nil
}

// responsesInputCustomToolOutput 把 custom_tool_call_output 映射为 role=tool 消息。
// 输出格式与 function_call_output 同构（字符串或内容块数组）。
func responsesInputCustomToolOutput(item map[string]any) (any, []any, error) {
	callID := stringValue(item["call_id"])
	if callID == "" {
		return nil, nil, errCustomToolCallOutputCallID
	}

	text, images := responsesToolOutputParts(item["output"])

	return map[string]any{
		"role":         "tool",
		"tool_call_id": callID,
		"content":      text,
	}, images, nil
}

// responsesToolOutputParts 拆分工具输出：文本压成字符串，图片保留为 content parts。
func responsesToolOutputParts(raw any) (string, []any) {
	if text, ok := raw.(string); ok {
		return text, nil
	}

	parts, ok := raw.([]any)
	if !ok {
		if raw == nil {
			return "", nil
		}

		data, err := json.Marshal(raw)
		if err != nil {
			return "", nil
		}

		return string(data), nil
	}

	var texts []string

	images := make([]any, 0)

	for _, part := range parts {
		m, _ := part.(map[string]any)
		if m == nil {
			continue
		}

		if stringValue(m["type"]) == "input_image" {
			if image := responsesContentPart(m); image != nil {
				images = append(images, image)
			}

			continue
		}

		if text := stringValue(m["text"]); text != "" {
			texts = append(texts, text)
		}
	}

	return strings.Join(texts, "\n"), images
}

// responsesContent 把消息内容转成 OpenAI content（纯文本压成字符串，含图片保留 parts）。
func responsesContent(raw any) any {
	if raw == nil {
		return ""
	}

	if text, ok := raw.(string); ok {
		return text
	}

	arr, ok := raw.([]any)
	if !ok {
		if m, ok := raw.(map[string]any); ok {
			return responsesContentPart(m)
		}

		return ""
	}

	parts := make([]any, 0, len(arr))
	texts := make([]string, 0, len(arr))
	allText := true

	for _, value := range arr {
		m, _ := value.(map[string]any)
		if m == nil {
			continue
		}

		part := responsesContentPart(m)
		if part == nil {
			continue
		}

		parts = append(parts, part)
		if part["type"] == "text" {
			texts = append(texts, stringValue(part["text"]))
			continue
		}

		allText = false
	}

	if allText {
		return strings.Join(texts, "\n")
	}

	return parts
}

// responsesContentPart 转换单个内容块；不支持的块返回 nil（忽略）。
func responsesContentPart(part map[string]any) map[string]any {
	switch stringValue(part["type"]) {
	case "input_text", "output_text", "text":
		return map[string]any{"type": "text", "text": stringValue(part["text"])}
	case "input_image":
		if url := responsesImageURL(part["image_url"]); url != "" {
			image := map[string]any{"url": url}

			if detail := stringValue(part["detail"]); detail != "" {
				// Responses 允许 original；chat/completions 只接受 auto/low/high。
				if detail == "original" {
					detail = "high"
				}

				image["detail"] = detail
			}

			return map[string]any{"type": "image_url", "image_url": image}
		}
	}

	return nil
}

func responsesImageURL(raw any) string {
	if s, ok := raw.(string); ok {
		return s
	}

	if m, ok := raw.(map[string]any); ok {
		return stringValue(m["url"])
	}

	return ""
}

// applyResponsesParams 映射采样参数、工具与推理强度。
func applyResponsesParams(out map[string]any, req *responsesRequest) {
	applyResponsesSampling(out, req)

	if tools, _ := responsesTools(req.Tools); len(tools) > 0 {
		out["tools"] = tools
	}

	if choice := responsesToolChoice(req.ToolChoice); choice != nil {
		out["tool_choice"] = choice
	}

	if req.ParallelToolCalls != nil {
		out["parallel_tool_calls"] = *req.ParallelToolCalls
	}

	if format := responsesTextFormat(req.Text); format != nil {
		out["response_format"] = format
	}

	if req.Reasoning != nil && req.Reasoning.Effort != "" {
		out["reasoning_effort"] = req.Reasoning.Effort
	}
}

// applyResponsesSampling 映射 max_output_tokens / 采样惩罚参数。
func applyResponsesSampling(out map[string]any, req *responsesRequest) {
	if req.MaxOutputTokens > 0 {
		out["max_tokens"] = req.MaxOutputTokens
	}

	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}

	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}

	if req.PresencePenalty != nil {
		out["presence_penalty"] = *req.PresencePenalty
	}

	if req.FrequencyPenalty != nil {
		out["frequency_penalty"] = *req.FrequencyPenalty
	}
}

// responsesTextFormat 把 Responses text.format 映射为 chat/completions response_format。
// 只映射上游认识的 text / json_object / json_schema；缺失或未知格式返回 nil。
func responsesTextFormat(raw json.RawMessage) any {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}

	var text struct {
		Format map[string]any `json:"format"`
	}
	if json.Unmarshal(raw, &text) != nil || text.Format == nil {
		return nil
	}

	switch stringValue(text.Format["type"]) {
	case "text":
		return map[string]any{"type": "text"}
	case "json_object":
		return map[string]any{"type": "json_object"}
	case "json_schema":
		return responsesJSONSchemaFormat(text.Format)
	default:
		return nil
	}
}

// responsesJSONSchemaFormat 把 Responses text.format 的 json_schema 形态转成
// chat/completions 的 response_format.json_schema。
func responsesJSONSchemaFormat(format map[string]any) any {
	schema, ok := format["schema"]
	if !ok {
		return nil
	}

	spec := map[string]any{"schema": schema}

	for _, key := range []string{"name", "description", "strict"} {
		if v, ok := format[key]; ok {
			spec[key] = v
		}
	}

	return map[string]any{"type": "json_schema", "json_schema": spec}
}

// responsesMaxNamespaceDepth namespace 展开的最大深度（防御异常嵌套）。
const responsesMaxNamespaceDepth = 4

// responsesTools 转换 function / custom 工具，并把 namespace 容器展开成 flat
// function。上游 Chat Completions 不认识 namespace；客户端回程又需要
// (name, namespace) 两个字段才能找到执行器，所以展开时记录 name → namespace，
// 响应侧由 responsesResolveToolName 还原。内置工具（web_search 等）仍忽略。
func responsesTools(tools []map[string]any) ([]any, map[string]string) {
	out := make([]any, 0, len(tools))
	namespaces := map[string]string{}
	seen := map[string]bool{}

	for _, tool := range tools {
		responsesAddTool(&out, namespaces, seen, tool, "", 0)
	}

	return out, namespaces
}

// responsesAddTool 递归展开单个工具；namespace 记录扁平名到命名空间的映射。
func responsesAddTool(
	out *[]any,
	namespaces map[string]string,
	seen map[string]bool,
	tool map[string]any,
	namespace string,
	depth int,
) {
	if tool == nil || depth > responsesMaxNamespaceDepth {
		return
	}

	switch typ := stringValue(tool["type"]); typ {
	case "", "function":
		responsesAddFlatTool(out, namespaces, seen, responsesFunctionTool(tool), namespace)
	case "custom":
		responsesAddFlatTool(out, namespaces, seen, responsesCustomTool(tool), namespace)
	case "namespace":
		childNS := stringValue(tool["name"])
		if childNS == "" {
			childNS = namespace
		}

		for _, sub := range responsesNamespaceChildren(tool) {
			if child, ok := sub.(map[string]any); ok {
				responsesAddTool(out, namespaces, seen, child, childNS, depth+1)
			}
		}
	}
}

// responsesAddFlatTool 把已转换的 function spec 追加到结果，首次出现的名字生效。
func responsesAddFlatTool(
	out *[]any,
	namespaces map[string]string,
	seen map[string]bool,
	spec map[string]any,
	namespace string,
) {
	if spec == nil {
		return
	}

	name := responsesToolSpecName(spec)
	if name == "" || seen[name] {
		return
	}

	seen[name] = true
	if namespace != "" {
		namespaces[name] = namespace
	}

	*out = append(*out, spec)
}

// responsesNamespaceChildren 读取 namespace 容器的子工具列表（兼容 tools/children/functions）。
func responsesNamespaceChildren(tool map[string]any) []any {
	for _, key := range []string{"tools", "children", "functions"} {
		if children, ok := tool[key].([]any); ok {
			return children
		}
	}

	return nil
}

// responsesToolSpecName 读取已构造 chat function spec 的工具名。
func responsesToolSpecName(spec map[string]any) string {
	fn, _ := spec["function"].(map[string]any)
	return stringValue(fn["name"])
}

// responsesToolNamespaces 从请求工具声明里收集 name → namespace。
func responsesToolNamespaces(tools []map[string]any) map[string]string {
	_, namespaces := responsesTools(tools)
	return namespaces
}

// responsesToolNamespacesFromBody 从原始 Responses 请求体收集 namespace 映射。
func responsesToolNamespacesFromBody(body []byte) map[string]string {
	var req struct {
		Tools []map[string]any `json:"tools"`
		Input json.RawMessage  `json:"input"`
	}
	if json.Unmarshal(body, &req) != nil {
		return nil
	}

	return responsesToolNamespaces(append(req.Tools, responsesAdditionalTools(req.Input)...))
}

// responsesResolveToolName 把模型回传的工具名解析成 (bare, namespace)。
// 正常路径是 flat 名直接命中映射；兼容模型自行拼出的 ns__tool / ns::tool。
func responsesResolveToolName(name string, namespaces map[string]string) (string, string) {
	if ns, ok := namespaces[name]; ok {
		return name, ns
	}

	for bare, ns := range namespaces {
		if name == ns+"__"+bare || name == ns+"::"+bare {
			return bare, ns
		}
	}

	return name, ""
}

// responsesFunctionTool 读取 function 工具定义（兼容嵌套 function 形态）。
func responsesFunctionTool(tool map[string]any) map[string]any {
	fn := tool
	if nested, ok := tool["function"].(map[string]any); ok {
		fn = nested
	}

	name := stringValue(fn["name"])
	if name == "" {
		return nil
	}

	spec := map[string]any{"name": name}
	if desc := stringValue(fn["description"]); desc != "" {
		spec["description"] = desc
	}

	if params, ok := fn["parameters"]; ok {
		spec["parameters"] = params
	}

	if strict, ok := fn["strict"].(bool); ok {
		spec["strict"] = strict
	}

	return map[string]any{"type": "function", "function": spec}
}

// responsesCustomTool 把 custom 工具降级为单 input 字符串参数的 function。
func responsesCustomTool(tool map[string]any) map[string]any {
	name := stringValue(tool["name"])
	if name == "" {
		return nil
	}

	desc := stringValue(tool["description"])
	if format, ok := tool["format"].(map[string]any); ok {
		if definition := stringValue(format["definition"]); definition != "" {
			if desc != "" {
				desc += "\n\n"
			}

			desc += definition
		}
	}

	spec := map[string]any{
		"name": name,
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"input": map[string]any{"type": "string", "description": "Raw tool input."},
			},
			"required":             []any{"input"},
			"additionalProperties": false,
		},
	}
	if desc != "" {
		spec["description"] = desc
	}

	return map[string]any{"type": "function", "function": spec}
}

// responsesToolChoice 映射 Responses tool_choice 到 OpenAI tool_choice。
func responsesToolChoice(raw json.RawMessage) any {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}

	var choice string
	if json.Unmarshal(raw, &choice) == nil {
		switch choice {
		case "auto", "none", "required":
			return choice
		default:
			return nil
		}
	}

	var obj struct {
		Type string `json:"type"`
		Name string `json:"name"`
		Mode string `json:"mode"`
	}
	if json.Unmarshal(raw, &obj) != nil {
		return nil
	}

	if obj.Type == "allowed_tools" {
		switch obj.Mode {
		case "auto", "none", "required":
			return obj.Mode
		default:
			return nil
		}
	}

	if obj.Name == "" {
		return nil
	}

	return map[string]any{"type": "function", "function": map[string]any{"name": obj.Name}}
}

// responsesResponder 把核心的 OpenAI 结果编码为 Responses 形态。
type responsesResponder struct {
	w            http.ResponseWriter
	model        string
	instructions string
	keepalive    time.Duration
	customTools  map[string]bool
	namespaces   map[string]string
}

func (r *responsesResponder) JSON(status int, body any) {
	writeJSON(r.w, status, responsesFromOpenAIWithNamespaces(body, r.model, r.instructions, r.customTools, r.namespaces))
}

// Error 保持 OpenAI 错误信封（Responses 客户端同样识别）。
func (r *responsesResponder) Error(status int, code, msg, hint string) {
	writeOpenAIErrorHint(r.w, status, code, msg, hint)
}

func (r *responsesResponder) Header(name, value string) { r.w.Header().Set(name, value) }

func (r *responsesResponder) Stream(rc io.Reader, hint func(string) string, repair *upstream.MarkupRepair, onErrFrame func(string)) error {
	responsesStreamWriter2 := newResponsesStreamWriter(r.w, r.model, r.instructions, r.customTools, r.namespaces)

	opts := make([]upstream.StreamOption, 0, 2)
	if onErrFrame != nil {
		opts = append(opts, upstream.WithErrorFrameObserver(onErrFrame))
	}

	opts = append(opts, upstream.WithMarkupRepair(repair))
	if r.keepalive > 0 {
		opts = append(opts, upstream.WithKeepaliveInterval(r.keepalive))
	}

	if err := upstream.StreamHint(responsesStreamWriter2, rc, hint, opts...); err != nil {
		return fmt.Errorf("stream hint: %w", err)
	}

	return nil
}

// responsesFromOpenAI 把聚合后的 chat.completion 转成 Responses 对象。
func responsesFromOpenAI(body any, model, instructions string, customTools ...map[string]bool) map[string]any {
	custom := map[string]bool{}
	if len(customTools) > 0 {
		custom = customTools[0]
	}

	return responsesFromOpenAIWithNamespaces(body, model, instructions, custom, nil)
}

// responsesFromOpenAIWithNamespaces 在 Responses 对象里还原 namespace 工具。
func responsesFromOpenAIWithNamespaces(
	body any,
	model, instructions string,
	customTools map[string]bool,
	namespaces map[string]string,
) map[string]any {
	obj, _ := body.(map[string]any)
	choice := firstOpenAIChoice(obj)

	output, text, status := responsesOutput(choice, customTools, namespaces)
	usage := responsesUsage(obj["usage"])

	id := stringValue(obj["id"])
	if id == "" {
		id = "resp_gateway"
	}

	created := intValue(obj["created"])
	if created == 0 {
		created = int(time.Now().Unix())
	}

	resp := responsesObject(id, model, instructions, int64(created), status, output, text, usage)
	if status == "incomplete" {
		resp["incomplete_details"] = map[string]any{"reason": responsesIncompleteReason(choice)}
	}

	return resp
}

// responsesIncompleteReason 映射 finish_reason → Responses incomplete_details.reason。
func responsesIncompleteReason(choice map[string]any) string {
	switch stringValue(choice["finish_reason"]) {
	case "content_filter":
		return "content_filter"
	default:
		return "max_output_tokens"
	}
}

// responsesOutput 从 choices[0] 构建 output 数组、output_text 与状态。
func responsesOutput(choice map[string]any, customTools map[string]bool, namespaces map[string]string) ([]any, string, string) {
	if choice == nil {
		return []any{}, "", "completed"
	}

	msg, _ := choice["message"].(map[string]any)

	text := responsesMessageText(msg["content"])
	if text == "" {
		// Chat refusal 没有 Responses output_text 对应字段；作为可见文本保留，
		// 同时 finish_reason=content_filter 仍映射为 incomplete。
		text = stringValue(msg["refusal"])
	}

	output := make([]any, 0, 2)
	if text != "" {
		output = append(output, responsesMessageItem("msg_gateway", text, "completed"))
	}

	output = append(output, responsesFunctionItems(msg["tool_calls"], customTools, namespaces)...)
	if len(output) == 0 {
		output = append(output, responsesMessageItem("msg_gateway", "", "completed"))
	}

	status := "completed"

	switch stringValue(choice["finish_reason"]) {
	case "length", "content_filter":
		status = "incomplete"
	}

	return output, text, status
}

// responsesMessageText 读取 message.content（字符串或 text parts）。
func responsesMessageText(raw any) string {
	if s, ok := raw.(string); ok {
		return s
	}

	parts, ok := raw.([]any)
	if !ok {
		return ""
	}

	var texts []string

	for _, part := range parts {
		m, _ := part.(map[string]any)
		if text := stringValue(m["text"]); text != "" {
			texts = append(texts, text)
		}
	}

	return strings.Join(texts, "")
}

func responsesMessageItem(id, text, status string) map[string]any {
	return map[string]any{
		"type": "message", "id": id, "status": status, "role": "assistant",
		"content": []any{responsesOutputText(text)},
	}
}

// responsesOutputText 构造 OpenAI Responses 的 output_text 内容块。
func responsesOutputText(text string) map[string]any {
	return map[string]any{"type": "output_text", "text": text, "annotations": []any{}, "logprobs": []any{}}
}

// mapItems 接受 JSON 解码后的 []any 与内部聚合产生的 []map[string]any，
// 统一成 map 切片。非流式聚合路径（upstream.Aggregate）产出的是后者。
func mapItems(raw any) []map[string]any {
	switch value := raw.(type) {
	case []map[string]any:
		return value
	case []any:
		out := make([]map[string]any, 0, len(value))
		for _, item := range value {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}

		return out
	default:
		return nil
	}
}

func responsesFunctionItems(raw any, customTools map[string]bool, namespaces map[string]string) []any {
	calls := mapItems(raw)

	out := make([]any, 0, len(calls))
	for i, call := range calls {
		fn, _ := call["function"].(map[string]any)

		name, namespace := responsesResolveToolName(stringValue(fn["name"]), namespaces)
		if name == "" {
			continue
		}

		callID := stringValue(call["id"])
		if callID == "" {
			callID = fmt.Sprintf("call_%d", i)
		}

		args := stringValue(fn["arguments"])
		if customTools[name] {
			item := map[string]any{
				"type": "custom_tool_call", "id": "ctc_" + callID, "call_id": callID,
				"name": name, "input": responsesCustomToolInput(args), "status": "completed",
			}
			responsesSetNamespace(item, namespace)
			out = append(out, item)

			continue
		}

		item := map[string]any{
			"type": "function_call", "id": "fc_" + callID, "call_id": callID,
			"name": name, "arguments": args, "status": "completed",
		}
		responsesSetNamespace(item, namespace)
		out = append(out, item)
	}

	return out
}

// responsesSetNamespace 把 namespace 写进 function_call / custom_tool_call。
func responsesSetNamespace(item map[string]any, namespace string) {
	if namespace != "" {
		item["namespace"] = namespace
	}
}

// responsesCustomToolInput 从降级后的 {"input":"..."} 参数里还原 freeform 文本。
// 模型未按约定输出时退回原始 arguments，避免丢调用。只有 input 键存在且为字符串
// 才解包；否则 {"patch":"..."} 这类偏差会被当成空 input 丢掉。
func responsesCustomToolInput(args string) string {
	var payload map[string]json.RawMessage
	if json.Unmarshal([]byte(args), &payload) != nil {
		return args
	}

	raw, ok := payload["input"]
	if !ok {
		return args
	}

	var input string
	if json.Unmarshal(raw, &input) != nil {
		return args
	}

	return input
}

func responsesUsage(raw any) map[string]any {
	usage, _ := raw.(map[string]any)
	input := intValue(usage["prompt_tokens"])
	output := intValue(usage["completion_tokens"])
	cached, cacheWrite := responsesCachedTokens(usage)

	return map[string]any{
		"input_tokens": input, "output_tokens": output, "total_tokens": input + output,
		"input_tokens_details": map[string]any{
			"cached_tokens": cached, "cache_write_tokens": cacheWrite,
		},
		"output_tokens_details": map[string]any{
			"reasoning_tokens": responsesReasoningTokens(usage),
		},
	}
}

// responsesCachedTokens 读取上游 prompt cache 统计（兼容 details 与扁平字段）。
func responsesCachedTokens(usage map[string]any) (cached, cacheWrite int) {
	details, _ := usage["prompt_tokens_details"].(map[string]any)
	cached = intValue(details["cached_tokens"])

	if cached == 0 {
		cached = intValue(usage["cached_tokens"])
	}

	cacheWrite = intValue(details["cache_write_tokens"])
	if cacheWrite == 0 {
		cacheWrite = intValue(usage["cache_write_tokens"])
	}

	return cached, cacheWrite
}

// responsesReasoningTokens 读取 completion_tokens_details.reasoning_tokens。
func responsesReasoningTokens(usage map[string]any) int {
	details, _ := usage["completion_tokens_details"].(map[string]any)
	return intValue(details["reasoning_tokens"])
}

// errorCodeString 把上游 error.code（字符串或数字）统一成字符串。
func errorCodeString(raw any) string {
	switch v := raw.(type) {
	case string:
		return v
	case float64:
		return fmt.Sprintf("%.0f", v)
	default:
		return ""
	}
}

func responsesObject(
	id, model, instructions string,
	created int64,
	status string,
	output []any,
	text string,
	usage map[string]any,
) map[string]any {
	return map[string]any{
		"id": id, "object": "response", "created_at": created, "status": status,
		"model": model, "instructions": instructions, "output": output, "output_text": text,
		"usage": usage, "error": nil, "incomplete_details": nil,
		"tools": []any{}, "tool_choice": "auto", "parallel_tool_calls": true,
		"background": false, "metadata": map[string]any{},
		"previous_response_id": nil, "reasoning": nil,
		"text": map[string]any{"format": map[string]any{"type": "text"}},
	}
}

// responsesStreamWriter 把上游 OpenAI SSE 转成 Responses SSE 事件。
type responsesStreamWriter struct {
	w            http.ResponseWriter
	fl           http.Flusher
	model        string
	instructions string
	customTools  map[string]bool
	namespaces   map[string]string

	pending  []byte
	started  bool
	finished bool
	errored  bool

	id      string
	created int64
	outputs []responsesIndexedItem
	nextIdx int
	msg     *responsesMessageState
	tools   map[int]*responsesToolState
	toolSeq []int
	seq     int

	finishReason    string
	inputTokens     int
	outputTokens    int
	cachedTokens    int
	cacheWrite      int
	reasoningTokens int
}

type responsesIndexedItem struct {
	index int
	item  map[string]any
}

type responsesMessageState struct {
	id    string
	index int
	text  strings.Builder
	open  bool
}

type responsesToolState struct {
	id      string
	callID  string
	name    string
	index   int
	args    strings.Builder
	started bool
	custom  bool
}

func newResponsesStreamWriter(
	w http.ResponseWriter,
	model, instructions string,
	customTools map[string]bool,
	namespaces map[string]string,
) *responsesStreamWriter {
	fl, _ := w.(http.Flusher)

	return &responsesStreamWriter{
		w: w, fl: fl, model: model, instructions: instructions, customTools: customTools,
		namespaces: namespaces,
		created:    time.Now().Unix(), tools: map[int]*responsesToolState{},
	}
}

func (s *responsesStreamWriter) Header() http.Header         { return s.w.Header() }
func (s *responsesStreamWriter) WriteHeader(code int)        { s.w.WriteHeader(code) }
func (s *responsesStreamWriter) Unwrap() http.ResponseWriter { return s.w }
func (s *responsesStreamWriter) Flush() {
	if s.fl != nil {
		s.fl.Flush()
	}
}

func (s *responsesStreamWriter) Write(nums []byte) (int, error) {
	s.pending = append(s.pending, nums...)
	for {
		i := bytes.IndexByte(s.pending, '\n')
		if i < 0 {
			break
		}

		line := strings.TrimRight(string(s.pending[:i]), "\r")

		s.pending = s.pending[i+1:]
		if err := s.handleLine(line); err != nil {
			return 0, err
		}
	}

	return len(nums), nil
}

func (s *responsesStreamWriter) handleLine(line string) error {
	if s.finished {
		return nil
	}

	if strings.HasPrefix(line, ":") {
		if _, err := io.WriteString(s.w, line+"\n\n"); err != nil {
			return fmt.Errorf("handleLine: %w", err)
		}

		s.Flush()

		return nil
	}

	if !strings.HasPrefix(line, "data: ") {
		return nil
	}

	payload := strings.TrimPrefix(line, "data: ")
	if payload == "[DONE]" {
		return s.finish()
	}

	return s.handleChunk(payload)
}

func (s *responsesStreamWriter) handleChunk(payload string) error {
	var chunk map[string]any
	if json.Unmarshal([]byte(payload), &chunk) != nil {
		return nil //nolint:nilerr // 非 JSON 噪声帧丢弃
	}

	if errObj, ok := chunk["error"].(map[string]any); ok {
		return s.handleUpstreamError(errObj)
	}

	if err := s.ensureStart(chunk); err != nil {
		return err
	}

	s.captureUsage(chunk)

	choice := firstOpenAIChoice(chunk)
	if choice == nil {
		return nil
	}

	if delta, _ := choice["delta"].(map[string]any); delta != nil {
		if err := s.applyDelta(delta); err != nil {
			return err
		}
	}

	if reason := stringValue(choice["finish_reason"]); reason != "" {
		s.finishReason = reason
	}

	return nil
}

func (s *responsesStreamWriter) handleUpstreamError(errObj map[string]any) error {
	msg := stringValue(errObj["message"])
	if msg == "" {
		msg = "upstream stream error"
	}

	code := errorCodeString(errObj["code"])
	if code == "" {
		code = "upstream_error"
	}

	body := map[string]any{"type": "api_error", "message": msg, "code": code}
	if hint := stringValue(errObj["gateway_hint"]); hint != "" {
		body["gateway_hint"] = hint
	}

	s.errored = true
	if err := s.ensureStart(nil); err != nil {
		return err
	}
	// OpenAI 兼容客户端识别顶层 message/code；Codex 还从 error 字段读取 flex 错误，
	// 两处都保留。随后补 response.failed，避免客户端只看到流被关闭。
	event := map[string]any{
		"type": "error", "message": msg, "code": code, "param": nil,
		"error": body,
	}
	if err := s.event("error", event); err != nil {
		return err
	}

	resp := s.responseObject("failed")
	resp["error"] = body

	return s.event("response.failed", map[string]any{"type": "response.failed", "response": resp})
}

// ensureStart 写出 response.created / response.in_progress。
func (s *responsesStreamWriter) ensureStart(chunk map[string]any) error {
	if s.started {
		return nil
	}

	s.started = true
	if id := stringValue(chunk["id"]); id != "" {
		s.id = id
	} else {
		s.id = "resp_gateway"
	}

	created := intValue(chunk["created"])
	if created != 0 {
		s.created = int64(created)
	}

	resp := s.responseObject("in_progress")
	if err := s.event("response.created", map[string]any{"type": "response.created", "response": resp}); err != nil {
		return err
	}

	return s.event("response.in_progress", map[string]any{"type": "response.in_progress", "response": resp})
}

func (s *responsesStreamWriter) applyDelta(delta map[string]any) error {
	text := stringValue(delta["content"])
	if text == "" {
		text = stringValue(delta["refusal"])
	}

	if text != "" {
		if err := s.emitText(text); err != nil {
			return err
		}
	}

	return s.emitTools(delta["tool_calls"])
}

func (s *responsesStreamWriter) emitText(text string) error {
	if s.msg == nil {
		s.msg = &responsesMessageState{id: fmt.Sprintf("msg_%d", s.nextIdx), index: s.nextIdx, open: true}

		s.nextIdx++
		if err := s.event("response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": s.msg.index, "item": s.messageItem("in_progress"),
		}); err != nil {
			return err
		}

		if err := s.event("response.content_part.added", map[string]any{
			"type": "response.content_part.added", "item_id": s.msg.id, "output_index": s.msg.index,
			"content_index": 0, "part": responsesOutputText(""),
		}); err != nil {
			return err
		}
	}

	s.msg.text.WriteString(text)

	return s.event("response.output_text.delta", map[string]any{
		"type": "response.output_text.delta", "item_id": s.msg.id, "output_index": s.msg.index,
		"content_index": 0, "delta": text, "logprobs": []any{},
	})
}

func (s *responsesStreamWriter) emitTools(raw any) error {
	for _, call := range mapItems(raw) {
		if call == nil {
			continue
		}

		idx := intValue(call["index"])

		state := s.toolState(idx, call)
		if state == nil {
			continue
		}

		if args := toolCallArguments(call); args != "" {
			state.args.WriteString(args)

			// custom 工具的增量是 JSON 包装的 {"input":...}，无法逐片还原为
			// freeform 文本；只在 done 事件给最终 input，避免把包装层透给客户端。
			if state.custom {
				continue
			}

			if err := s.event("response.function_call_arguments.delta", map[string]any{
				"type": "response.function_call_arguments.delta", "item_id": state.id,
				"output_index": state.index, "delta": args,
			}); err != nil {
				return err
			}
		}
	}

	return nil
}

// toolState 首次出现时登记并写出 output_item.added；缺 name 的片段返回 nil。
func (s *responsesStreamWriter) toolState(idx int, call map[string]any) *responsesToolState {
	if state, ok := s.tools[idx]; ok {
		return state
	}

	name, callID := toolCallIdentity(call)
	name, _ = responsesResolveToolName(name, s.namespaces)

	if name == "" {
		return nil
	}

	if callID == "" {
		callID = fmt.Sprintf("call_%d", idx)
	}

	custom := s.customTools[name]

	prefix := "fc_"
	if custom {
		prefix = "ctc_"
	}

	state := &responsesToolState{
		id: prefix + callID, callID: callID, name: name, index: s.nextIdx, started: true, custom: custom,
	}
	s.nextIdx++
	s.tools[idx] = state
	s.toolSeq = append(s.toolSeq, idx)
	_ = s.event("response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": state.index, "item": s.functionItem(state, "in_progress"),
	})

	return state
}

func (s *responsesStreamWriter) finish() error {
	if s.finished {
		return nil
	}

	s.finished = true
	if s.errored {
		return nil
	}

	if !s.started {
		if err := s.ensureStart(nil); err != nil {
			return err
		}
	}

	if err := s.closeMessage(); err != nil {
		return err
	}

	for _, idx := range s.toolSeq {
		if err := s.closeTool(s.tools[idx]); err != nil {
			return err
		}
	}

	if len(s.outputs) == 0 {
		if err := s.closeEmptyMessage(); err != nil {
			return err
		}
	}

	return s.emitCompletion()
}

// emitCompletion 写出 response.completed / response.incomplete。
func (s *responsesStreamWriter) emitCompletion() error {
	status, eventName, reason := s.completionStatus()

	resp := s.responseObject(status)
	if reason != "" {
		resp["incomplete_details"] = map[string]any{"reason": reason}
	}

	return s.event(eventName, map[string]any{"type": eventName, "response": resp})
}

// completionStatus 由 finish_reason 决定最终状态、事件名与 incomplete reason。
func (s *responsesStreamWriter) completionStatus() (status, eventName, reason string) {
	switch s.finishReason {
	case "length":
		return "incomplete", "response.incomplete", "max_output_tokens"
	case "content_filter":
		return "incomplete", "response.incomplete", "content_filter"
	default:
		return "completed", "response.completed", ""
	}
}

func (s *responsesStreamWriter) closeMessage() error {
	if s.msg == nil || !s.msg.open {
		return nil
	}

	s.msg.open = false

	text := s.msg.text.String()
	if err := s.event("response.output_text.done", map[string]any{
		"type": "response.output_text.done", "item_id": s.msg.id, "output_index": s.msg.index,
		"content_index": 0, "text": text, "logprobs": []any{},
	}); err != nil {
		return err
	}

	if err := s.event("response.content_part.done", map[string]any{
		"type": "response.content_part.done", "item_id": s.msg.id, "output_index": s.msg.index,
		"content_index": 0, "part": responsesOutputText(text),
	}); err != nil {
		return err
	}

	item := s.messageItem("completed")
	if err := s.event("response.output_item.done", map[string]any{
		"type": "response.output_item.done", "output_index": s.msg.index, "item": item,
	}); err != nil {
		return err
	}

	s.outputs = append(s.outputs, responsesIndexedItem{index: s.msg.index, item: item})

	return nil
}

func (s *responsesStreamWriter) closeEmptyMessage() error {
	s.msg = &responsesMessageState{id: fmt.Sprintf("msg_%d", s.nextIdx), index: s.nextIdx, open: true}

	s.nextIdx++
	if err := s.event("response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": s.msg.index, "item": s.messageItem("in_progress"),
	}); err != nil {
		return err
	}

	return s.closeMessage()
}

func (s *responsesStreamWriter) closeTool(state *responsesToolState) error {
	if state == nil || !state.started {
		return nil
	}

	args := state.args.String()
	if state.custom {
		if err := s.event("response.custom_tool_call_input.done", map[string]any{
			"type": "response.custom_tool_call_input.done", "item_id": state.id,
			"output_index": state.index, "input": responsesCustomToolInput(args),
		}); err != nil {
			return err
		}
	} else if err := s.event("response.function_call_arguments.done", map[string]any{
		"type": "response.function_call_arguments.done", "item_id": state.id,
		"output_index": state.index, "arguments": args,
	}); err != nil {
		return err
	}

	item := s.functionItem(state, "completed")
	if err := s.event("response.output_item.done", map[string]any{
		"type": "response.output_item.done", "output_index": state.index, "item": item,
	}); err != nil {
		return err
	}

	s.outputs = append(s.outputs, responsesIndexedItem{index: state.index, item: item})

	return nil
}

func (s *responsesStreamWriter) messageItem(status string) map[string]any {
	return responsesMessageItem(s.msg.id, s.msg.text.String(), status)
}

func (s *responsesStreamWriter) functionItem(state *responsesToolState, status string) map[string]any {
	namespace := s.namespaces[state.name]

	if state.custom {
		item := map[string]any{
			"type": "custom_tool_call", "id": state.id, "call_id": state.callID,
			"name": state.name, "input": responsesCustomToolInput(state.args.String()), "status": status,
		}
		responsesSetNamespace(item, namespace)

		return item
	}

	item := map[string]any{
		"type": "function_call", "id": state.id, "call_id": state.callID,
		"name": state.name, "arguments": state.args.String(), "status": status,
	}
	responsesSetNamespace(item, namespace)

	return item
}

func (s *responsesStreamWriter) responseObject(status string) map[string]any {
	sorted := append([]responsesIndexedItem(nil), s.outputs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].index < sorted[j].index })
	output := make([]any, 0, len(sorted))

	var text strings.Builder

	for _, item := range sorted {
		output = append(output, item.item)
		if item.item["type"] != "message" {
			continue
		}

		parts, _ := item.item["content"].([]any)
		for _, part := range parts {
			if m, ok := part.(map[string]any); ok && m["type"] == "output_text" {
				text.WriteString(stringValue(m["text"]))
			}
		}
	}

	usage := map[string]any{
		"input_tokens": s.inputTokens, "output_tokens": s.outputTokens,
		"total_tokens": s.inputTokens + s.outputTokens,
		"input_tokens_details": map[string]any{
			"cached_tokens": s.cachedTokens, "cache_write_tokens": s.cacheWrite,
		},
		"output_tokens_details": map[string]any{
			"reasoning_tokens": s.reasoningTokens,
		},
	}

	return responsesObject(s.id, s.model, s.instructions, s.created, status, output, text.String(), usage)
}

func (s *responsesStreamWriter) captureUsage(chunk map[string]any) {
	usage, _ := chunk["usage"].(map[string]any)
	if usage == nil {
		return
	}

	if n := intValue(usage["prompt_tokens"]); n > 0 {
		s.inputTokens = n
	}

	if n := intValue(usage["completion_tokens"]); n > 0 {
		s.outputTokens = n
	}

	if cached, cacheWrite := responsesCachedTokens(usage); cached > 0 || cacheWrite > 0 {
		s.cachedTokens, s.cacheWrite = cached, cacheWrite
	}

	if n := responsesReasoningTokens(usage); n > 0 {
		s.reasoningTokens = n
	}
}

func (s *responsesStreamWriter) event(name string, payload map[string]any) error {
	payload["sequence_number"] = s.seq
	s.seq++

	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("event: %w", err)
	}

	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, raw); err != nil {
		return fmt.Errorf("event: %w", err)
	}

	s.Flush()

	return nil
}
