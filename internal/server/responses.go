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
	errResponsesInputItemFailed    = errors.New("responsesInputItem failed")
	errResponsesInputMessageFailed = errors.New("responsesInputMessage failed")
	errFunctionCallNameIsRequired  = errors.New("function_call name is required")
	errFunctionCallOutputCallIDIs  = errors.New("function_call_output call_id is required")
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

	in, out, ok := newResponsesChatInput(w, r, body, h.cfg.KeepaliveInterval)
	if !ok {
		return
	}

	h.serveChat(r, &in, out)
}

// newResponsesChatInput 把已读取的 Responses 请求转换为内部 chatInput。
func newResponsesChatInput(w http.ResponseWriter, r *http.Request, body []byte, keepalive time.Duration) (chatInput, chatResponder, bool) {
	openAI, model, instructions, stream, err := responsesToOpenAI(body)
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

	return chatInput2, &responsesResponder{w: w, model: model, instructions: instructions, keepalive: keepalive}, true
}

// responsesToOpenAI 转换请求体，返回内部 chat body、原始模型名、instructions 与 stream。
func responsesToOpenAI(body []byte) ([]byte, string, string, bool, error) {
	req, err := decodeResponsesRequest(body)
	if err != nil {
		return nil, "", "", false, err
	}

	msgs, err := responsesMessages(req)
	if err != nil {
		return nil, "", "", false, err
	}

	out := map[string]any{"model": req.Model, "stream": req.Stream, "messages": msgs}
	applyResponsesParams(out, req)

	converted, err := json.Marshal(out)
	if err != nil {
		return nil, "", "", false, fmt.Errorf("encode request: %w", err)
	}

	return converted, req.Model, req.Instructions, req.Stream, nil
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

	var text string
	if json.Unmarshal(req.Input, &text) == nil {
		return append(msgs, map[string]any{"role": "user", "content": text}), nil
	}

	var items []map[string]any
	if err := json.Unmarshal(req.Input, &items); err != nil {
		return nil, fmt.Errorf("invalid input: %w", err)
	}

	if len(items) == 0 {
		return nil, errInputIsRequired
	}

	for _, item := range items {
		converted, err := responsesInputItem(item)
		if err != nil {
			return nil, err
		}

		msgs = append(msgs, converted)
	}

	return msgs, nil
}

// responsesInputItem 转换单条 input item：message / function_call / function_call_output。
func responsesInputItem(item map[string]any) (any, error) {
	switch stringValue(item["type"]) {
	case "", "message":
		return responsesInputMessage(item)
	case "function_call":
		return responsesInputFunctionCall(item)
	case "function_call_output":
		return responsesInputFunctionOutput(item)
	default:
		return nil, fmt.Errorf("unsupported input item type %q: %w", stringValue(item["type"]), errResponsesInputItemFailed)
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

// responsesInputFunctionOutput 把 function_call_output 映射为 role=tool 消息。
func responsesInputFunctionOutput(item map[string]any) (any, error) {
	callID := stringValue(item["call_id"])
	if callID == "" {
		return nil, errFunctionCallOutputCallIDIs
	}

	return map[string]any{
		"role":         "tool",
		"tool_call_id": callID,
		"content":      responsesToolOutput(item["output"]),
	}, nil
}

// responsesToolOutput 把工具输出（字符串或内容块数组）压成文本。
func responsesToolOutput(raw any) string {
	if s, ok := raw.(string); ok {
		return s
	}

	parts, ok := raw.([]any)
	if !ok {
		if raw == nil {
			return ""
		}

		data, err := json.Marshal(raw)
		if err != nil {
			return ""
		}

		return string(data)
	}

	var texts []string

	for _, part := range parts {
		m, _ := part.(map[string]any)
		if text, _ := m["text"].(string); text != "" {
			texts = append(texts, text)
		}
	}

	return strings.Join(texts, "\n")
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
			return map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}}
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
	if req.MaxOutputTokens > 0 {
		out["max_tokens"] = req.MaxOutputTokens
	}

	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}

	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}

	if tools := responsesTools(req.Tools); len(tools) > 0 {
		out["tools"] = tools
	}

	if choice := responsesToolChoice(req.ToolChoice); choice != nil {
		out["tool_choice"] = choice
	}

	if req.Reasoning != nil && req.Reasoning.Effort != "" {
		out["reasoning_effort"] = req.Reasoning.Effort
	}
}

// responsesTools 只保留 function 工具（其他内置工具本网关不执行，直接忽略）。
func responsesTools(tools []map[string]any) []any {
	out := make([]any, 0, len(tools))
	for _, tool := range tools {
		fn := tool
		if nested, ok := tool["function"].(map[string]any); ok {
			fn = nested
		}

		name := stringValue(fn["name"])
		if name == "" {
			continue
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

		out = append(out, map[string]any{"type": "function", "function": spec})
	}

	return out
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
	}
	if json.Unmarshal(raw, &obj) != nil || obj.Name == "" {
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
}

func (r *responsesResponder) JSON(status int, body any) {
	writeJSON(r.w, status, responsesFromOpenAI(body, r.model, r.instructions))
}

// Error 保持 OpenAI 错误信封（Responses 客户端同样识别）。
func (r *responsesResponder) Error(status int, code, msg, hint string) {
	writeOpenAIErrorHint(r.w, status, code, msg, hint)
}

func (r *responsesResponder) Header(name, value string) { r.w.Header().Set(name, value) }

func (r *responsesResponder) Stream(rc io.Reader, hint func(string) string, repair *upstream.MarkupRepair, onErrFrame func(string)) error {
	responsesStreamWriter2 := newResponsesStreamWriter(r.w, r.model, r.instructions)

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
func responsesFromOpenAI(body any, model, instructions string) map[string]any {
	obj, _ := body.(map[string]any)
	choice := firstOpenAIChoice(obj)
	output, text, status := responsesOutput(choice)
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
		resp["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}

	return resp
}

// responsesOutput 从 choices[0] 构建 output 数组、output_text 与状态。
func responsesOutput(choice map[string]any) ([]any, string, string) {
	if choice == nil {
		return []any{}, "", "completed"
	}

	msg, _ := choice["message"].(map[string]any)
	text := responsesMessageText(msg["content"])

	output := make([]any, 0, 2)
	if text != "" {
		output = append(output, responsesMessageItem("msg_gateway", text, "completed"))
	}

	output = append(output, responsesFunctionItems(msg["tool_calls"])...)
	if len(output) == 0 {
		output = append(output, responsesMessageItem("msg_gateway", "", "completed"))
	}

	status := "completed"
	if stringValue(choice["finish_reason"]) == "length" {
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
		"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
	}
}

func responsesFunctionItems(raw any) []any {
	calls, _ := raw.([]any)

	out := make([]any, 0, len(calls))
	for i, item := range calls {
		call, _ := item.(map[string]any)
		fn, _ := call["function"].(map[string]any)

		name := stringValue(fn["name"])
		if name == "" {
			continue
		}

		callID := stringValue(call["id"])
		if callID == "" {
			callID = fmt.Sprintf("call_%d", i)
		}

		out = append(out, map[string]any{
			"type": "function_call", "id": "fc_" + callID, "call_id": callID,
			"name": name, "arguments": stringValue(fn["arguments"]), "status": "completed",
		})
	}

	return out
}

func responsesUsage(raw any) map[string]any {
	usage, _ := raw.(map[string]any)
	input := intValue(usage["prompt_tokens"])
	output := intValue(usage["completion_tokens"])

	return map[string]any{
		"input_tokens": input, "output_tokens": output, "total_tokens": input + output,
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
	}
}

// responsesStreamWriter 把上游 OpenAI SSE 转成 Responses SSE 事件。
type responsesStreamWriter struct {
	w            http.ResponseWriter
	fl           http.Flusher
	model        string
	instructions string

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

	inputTokens  int
	outputTokens int
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
}

func newResponsesStreamWriter(w http.ResponseWriter, model, instructions string) *responsesStreamWriter {
	fl, _ := w.(http.Flusher)

	return &responsesStreamWriter{
		w: w, fl: fl, model: model, instructions: instructions,
		created: time.Now().Unix(), tools: map[int]*responsesToolState{},
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

	return nil
}

func (s *responsesStreamWriter) handleUpstreamError(errObj map[string]any) error {
	msg := stringValue(errObj["message"])
	if msg == "" {
		msg = "upstream stream error"
	}

	body := map[string]any{"type": "api_error", "message": msg}
	if hint := stringValue(errObj["gateway_hint"]); hint != "" {
		body["gateway_hint"] = hint
	}

	s.errored = true

	return s.event("error", map[string]any{"type": "error", "error": body})
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
	if text := stringValue(delta["content"]); text != "" {
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
			"content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		}); err != nil {
			return err
		}
	}

	s.msg.text.WriteString(text)

	return s.event("response.output_text.delta", map[string]any{
		"type": "response.output_text.delta", "item_id": s.msg.id, "output_index": s.msg.index,
		"content_index": 0, "delta": text,
	})
}

func (s *responsesStreamWriter) emitTools(raw any) error {
	calls, _ := raw.([]any)
	for _, value := range calls {
		call, _ := value.(map[string]any)
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
	if name == "" {
		return nil
	}

	if callID == "" {
		callID = fmt.Sprintf("call_%d", idx)
	}

	state := &responsesToolState{
		id: "fc_" + callID, callID: callID, name: name, index: s.nextIdx, started: true,
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

	return s.event("response.completed", map[string]any{
		"type": "response.completed", "response": s.responseObject("completed"),
	})
}

func (s *responsesStreamWriter) closeMessage() error {
	if s.msg == nil || !s.msg.open {
		return nil
	}

	s.msg.open = false

	text := s.msg.text.String()
	if err := s.event("response.output_text.done", map[string]any{
		"type": "response.output_text.done", "item_id": s.msg.id, "output_index": s.msg.index,
		"content_index": 0, "text": text,
	}); err != nil {
		return err
	}

	if err := s.event("response.content_part.done", map[string]any{
		"type": "response.content_part.done", "item_id": s.msg.id, "output_index": s.msg.index,
		"content_index": 0, "part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
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
	if err := s.event("response.function_call_arguments.done", map[string]any{
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
	return map[string]any{
		"type": "function_call", "id": state.id, "call_id": state.callID,
		"name": state.name, "arguments": state.args.String(), "status": status,
	}
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
}

func (s *responsesStreamWriter) event(name string, payload map[string]any) error {
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
