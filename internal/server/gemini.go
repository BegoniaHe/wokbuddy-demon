// gemini.go Gemini GenerateContent 入站适配（Gemini CLI GATEWAY 模式）。
//
// 把 /v1beta/models/{model}:generateContent 与 :streamGenerateContent 的请求体
// 转换为内部 OpenAI chat 请求，复用 chatFlow 的选号/轮转/粘性；响应再转换回
// Gemini GenerateContentResponse / SSE 分片。协议细节集中在本文件。
package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

var (
	errGeminiEmptyBody     = errors.New("empty request body")
	errGeminiContents      = errors.New("contents is required")
	errGeminiModelRequired = errors.New("model is required")
	errGeminiUnknownAction = errors.New("unknown model action")
)

// geminiRequest 只声明转换所需字段；兼容 @google/genai 的 camelCase REST 形态。
type geminiRequest struct {
	Contents               []geminiContent  `json:"contents"`
	SystemInstruction      json.RawMessage  `json:"systemInstruction"`
	SystemInstructionSnake json.RawMessage  `json:"system_instruction"`
	GenerationConfig       json.RawMessage  `json:"generationConfig"`
	GenerationConfigSnake  json.RawMessage  `json:"generation_config"`
	Tools                  []map[string]any `json:"tools"`
	ToolConfig             json.RawMessage  `json:"toolConfig"`
	ToolConfigSnake        json.RawMessage  `json:"tool_config"`
}

type geminiContent struct {
	Role  string           `json:"role"`
	Parts []map[string]any `json:"parts"`
}

// geminiModelsHandler 处理 Gemini models 动作端点：
//
//	POST /{version}/models/{model}:generateContent
//	POST /{version}/models/{model}:streamGenerateContent
//	POST /{version}/models/{model}:countTokens
//
// model 可带 `realm:` 前缀；按最后一个冒号切分 model 与 action。
func (h *Handler) geminiModelsHandler(w http.ResponseWriter, r *http.Request) {
	segment := r.PathValue("rest")

	index := strings.LastIndex(segment, ":")
	if index <= 0 || index == len(segment)-1 {
		writeGeminiError(w, http.StatusNotFound, "unknown Gemini endpoint", "")
		return
	}

	model, action := segment[:index], segment[index+1:]
	if strings.TrimSpace(model) == "" {
		writeGeminiError(w, http.StatusBadRequest, errGeminiModelRequired.Error(), "")
		return
	}

	switch action {
	case "countTokens":
		h.geminiCountTokens(w, r, model)
	case "generateContent":
		h.geminiGenerate(w, r, model, false)
	case "streamGenerateContent":
		h.geminiGenerate(w, r, model, true)
	default:
		writeGeminiError(w, http.StatusNotFound, fmt.Errorf("%w: %s", errGeminiUnknownAction, action).Error(), "")
	}
}

// geminiGenerate 读取并转换请求，复用 chatFlow 转发。
func (h *Handler) geminiGenerate(w http.ResponseWriter, r *http.Request, model string, stream bool) {
	in, out, ok := newGeminiChatInput(w, r, model, stream, h.cfg.KeepaliveInterval, h.cfg.MaxBodyBytes)
	if !ok {
		return
	}

	h.serveChat(r, &in, out)
}

// newGeminiChatInput 读取 Gemini 请求体并构造 chatInput；失败时已写出 Gemini 错误。
func newGeminiChatInput(
	w http.ResponseWriter,
	r *http.Request,
	model string,
	stream bool,
	keepalive time.Duration,
	maxBodyBytes int64,
) (chatInput, chatResponder, bool) {
	body, ok := readGeminiBody(w, r, maxBodyBytes)
	if !ok {
		return chatInput{}, nil, false
	}

	converted, err := geminiToOpenAI(body, model, stream)
	if err != nil {
		writeGeminiError(w, http.StatusBadRequest, err.Error(), "")
		return chatInput{}, nil, false
	}

	return chatInput{
		body:      converted,
		stream:    stream,
		model:     model,
		clientIP:  upstream.ExtractClientIP(r),
		convReqID: r.Header.Get("X-Conversation-Request-Id"),
		traceID:   r.Header.Get("X-Trace-Id"),
		trace:     requestTraceFrom(r),
		keyUsage:  keyUsageFrom(r.Context()),
		keyRealm:  keyRealmFrom(r.Context()),
	}, &geminiResponder{
		w: w, model: model, keepalive: keepalive,
	}, true
}

// readGeminiBody 读取请求体并套用入站大小上限。
func readGeminiBody(w http.ResponseWriter, r *http.Request, maxBodyBytes int64) ([]byte, bool) {
	limitRequestBody(w, r, maxBodyBytes)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		if isBodyTooLarge(err) {
			writeGeminiError(w, http.StatusRequestEntityTooLarge, bodyTooLargeMessage(maxBodyBytes), "")
			return nil, false
		}

		writeGeminiError(w, http.StatusBadRequest, "read body: "+err.Error(), "")

		return nil, false
	}

	return body, true
}

// geminiToOpenAI 把 Gemini GenerateContent 请求转换为 OpenAI chat 请求。
func geminiToOpenAI(body []byte, model string, stream bool) ([]byte, error) {
	req, err := decodeGeminiRequest(body)
	if err != nil {
		return nil, err
	}

	msgs, err := geminiMessages(req)
	if err != nil {
		return nil, err
	}

	out := map[string]any{"model": model, "stream": stream, "messages": msgs}

	geminiApplyGeneration(out, geminiGenerationRaw(req))
	geminiApplyTools(out, req.Tools, geminiToolConfigRaw(req))

	converted, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}

	return converted, nil
}

func decodeGeminiRequest(body []byte) (*geminiRequest, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, errGeminiEmptyBody
	}

	var req geminiRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("invalid JSON body: %w", err)
	}

	if len(req.Contents) == 0 {
		return nil, errGeminiContents
	}

	return &req, nil
}

func geminiSystemRaw(req *geminiRequest) json.RawMessage {
	if len(req.SystemInstruction) > 0 {
		return req.SystemInstruction
	}

	return req.SystemInstructionSnake
}

func geminiGenerationRaw(req *geminiRequest) json.RawMessage {
	if len(req.GenerationConfig) > 0 {
		return req.GenerationConfig
	}

	return req.GenerationConfigSnake
}

func geminiToolConfigRaw(req *geminiRequest) json.RawMessage {
	if len(req.ToolConfig) > 0 {
		return req.ToolConfig
	}

	return req.ToolConfigSnake
}

// geminiMessages 把 systemInstruction 与 contents 转成 OpenAI messages。
func geminiMessages(req *geminiRequest) ([]any, error) {
	msgs := make([]any, 0, len(req.Contents)+1)

	if system := geminiSystemText(geminiSystemRaw(req)); system != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": system})
	}

	for _, content := range req.Contents {
		msgs = append(msgs, geminiContentMessages(content)...)
	}

	if len(msgs) == 0 {
		return nil, errGeminiContents
	}

	return msgs, nil
}

// geminiContentMessages 转换单条 content；model 侧产出 assistant，user 侧可能
// 拆出 tool 消息（functionResponse）。
func geminiContentMessages(content geminiContent) []any {
	switch strings.ToLower(strings.TrimSpace(content.Role)) {
	case "model", "assistant":
		msg := geminiModelMessage(content.Parts)
		if msg == nil {
			return nil
		}

		return []any{msg}
	default:
		return geminiUserMessages(content.Parts)
	}
}

// geminiModelMessage 合并 model 文本、thought 与 functionCall。
func geminiModelMessage(parts []map[string]any) map[string]any {
	texts, reasoning := geminiModelTexts(parts)
	toolCalls := geminiModelToolCalls(parts)

	if len(texts) == 0 && len(toolCalls) == 0 && len(reasoning) == 0 {
		return nil
	}

	msg := map[string]any{"role": "assistant", "content": strings.Join(texts, "\n")}
	if len(reasoning) > 0 {
		msg["reasoning_content"] = strings.Join(reasoning, "\n")
	}

	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}

	return msg
}

// geminiModelTexts 拆分普通文本与 thought 文本。
func geminiModelTexts(parts []map[string]any) (texts, reasoning []string) {
	for _, part := range parts {
		text := stringValue(part["text"])
		if text == "" {
			continue
		}

		if thought, _ := part["thought"].(bool); thought {
			reasoning = append(reasoning, text)
			continue
		}

		texts = append(texts, text)
	}

	return texts, reasoning
}

// geminiModelToolCalls 收集 functionCall part。
func geminiModelToolCalls(parts []map[string]any) []any {
	toolCalls := make([]any, 0)

	for _, part := range parts {
		if call := geminiFunctionCall(part); call != nil {
			toolCalls = append(toolCalls, call)
		}
	}

	return toolCalls
}

// geminiFunctionCall 把 functionCall part 转成 OpenAI tool_call。
func geminiFunctionCall(part map[string]any) map[string]any {
	call, _ := part["functionCall"].(map[string]any)
	if call == nil {
		call, _ = part["function_call"].(map[string]any)
	}

	name := stringValue(call["name"])
	if name == "" {
		return nil
	}

	id := stringValue(call["id"])
	if id == "" {
		id = name
	}

	args := call["args"]
	if args == nil {
		args = map[string]any{}
	}

	raw, err := json.Marshal(args)
	if err != nil {
		raw = []byte("{}")
	}

	return map[string]any{
		"id": id, "type": "function",
		"function": map[string]any{"name": name, "arguments": string(raw)},
	}
}

// geminiUserMessages 把 user 文本/图片合并为 user 消息，functionResponse 拆成 tool 消息。
func geminiUserMessages(parts []map[string]any) []any {
	out := make([]any, 0, len(parts)+1)
	content := make([]any, 0, len(parts))

	for _, part := range parts {
		if response := geminiFunctionResponse(part); response != nil {
			out = append(out, response)
			continue
		}

		if item := geminiContentPart(part); item != nil {
			content = append(content, item)
		}
	}

	if len(content) > 0 {
		out = append(out, map[string]any{"role": "user", "content": openAIContent(content)})
	}

	if len(out) == 0 {
		out = append(out, map[string]any{"role": "user", "content": ""})
	}

	return out
}

// geminiFunctionResponse 把 functionResponse part 转成 role=tool 消息。
func geminiFunctionResponse(part map[string]any) map[string]any {
	responsePart, _ := part["functionResponse"].(map[string]any)
	if responsePart == nil {
		responsePart, _ = part["function_response"].(map[string]any)
	}

	name := stringValue(responsePart["name"])
	if name == "" {
		return nil
	}

	id := stringValue(responsePart["id"])
	if id == "" {
		id = name
	}

	body := ""

	if response, ok := responsePart["response"]; ok {
		if text, ok := response.(string); ok {
			body = text
		} else if raw, err := json.Marshal(response); err == nil {
			body = string(raw)
		}
	}

	return map[string]any{"role": "tool", "tool_call_id": id, "content": body}
}

// geminiContentPart 把 text / inlineData / fileData 转成 OpenAI content part。
func geminiContentPart(part map[string]any) map[string]any {
	if text := stringValue(part["text"]); text != "" {
		return map[string]any{"type": "text", "text": text}
	}

	if image := geminiInlineImage(part); image != nil {
		return image
	}

	return geminiFileImage(part)
}

func geminiInlineImage(part map[string]any) map[string]any {
	inline, _ := part["inlineData"].(map[string]any)
	if inline == nil {
		inline, _ = part["inline_data"].(map[string]any)
	}

	mime := stringValue(inline["mimeType"])
	if mime == "" {
		mime = stringValue(inline["mime_type"])
	}

	data := stringValue(inline["data"])
	if mime == "" || data == "" || !strings.HasPrefix(mime, "image/") {
		return nil
	}

	return map[string]any{
		"type": "image_url",
		"image_url": map[string]any{
			"url": "data:" + mime + ";base64," + data,
		},
	}
}

func geminiFileImage(part map[string]any) map[string]any {
	file, _ := part["fileData"].(map[string]any)
	if file == nil {
		file, _ = part["file_data"].(map[string]any)
	}

	uri := stringValue(file["fileUri"])
	if uri == "" {
		uri = stringValue(file["file_uri"])
	}

	mime := stringValue(file["mimeType"])
	if mime == "" {
		mime = stringValue(file["mime_type"])
	}

	if uri == "" || (!strings.HasPrefix(uri, "http://") && !strings.HasPrefix(uri, "https://")) {
		return nil
	}

	if mime != "" && !strings.HasPrefix(mime, "image/") {
		return nil
	}

	return map[string]any{"type": "image_url", "image_url": map[string]any{"url": uri}}
}

// geminiGenerationConfig 是 generationConfig 的转换子集。
type geminiGenerationConfig struct {
	Temperature        *float64              `json:"temperature"`
	TopP               *float64              `json:"topP"`
	MaxOutputTokens    *int                  `json:"maxOutputTokens"`
	StopSequences      []string              `json:"stopSequences"`
	PresencePenalty    *float64              `json:"presencePenalty"`
	FrequencyPenalty   *float64              `json:"frequencyPenalty"`
	Seed               *int                  `json:"seed"`
	ResponseMimeType   string                `json:"responseMimeType"`
	ResponseSchema     json.RawMessage       `json:"responseSchema"`
	ResponseJSONSchema json.RawMessage       `json:"responseJsonSchema"`
	ThinkingConfig     *geminiThinkingConfig `json:"thinkingConfig"`
}

type geminiThinkingConfig struct {
	ThinkingBudget  *int   `json:"thinkingBudget"`
	ThinkingLevel   string `json:"thinkingLevel"`
	IncludeThoughts *bool  `json:"includeThoughts"`
}

func geminiApplyGeneration(out map[string]any, raw json.RawMessage) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return
	}

	var cfg geminiGenerationConfig
	if json.Unmarshal(raw, &cfg) != nil {
		return
	}

	if cfg.MaxOutputTokens != nil && *cfg.MaxOutputTokens > 0 {
		out["max_tokens"] = *cfg.MaxOutputTokens
	}

	geminiApplySampling(out, &cfg)
	geminiApplyResponseFormat(out, &cfg)

	if effort := geminiThinkingEffort(cfg.ThinkingConfig); effort != "" {
		out["reasoning_effort"] = effort
	}

	// Gemini thinkingBudget=0 表示关闭思考；映射到上游 thinking 开关。
	if cfg.ThinkingConfig != nil && cfg.ThinkingConfig.ThinkingBudget != nil &&
		*cfg.ThinkingConfig.ThinkingBudget == 0 {
		out["thinking"] = map[string]any{"type": "disabled"}
	}
}

func geminiApplySampling(out map[string]any, cfg *geminiGenerationConfig) {
	if cfg.Temperature != nil {
		out["temperature"] = *cfg.Temperature
	}

	if cfg.TopP != nil {
		out["top_p"] = *cfg.TopP
	}

	if cfg.PresencePenalty != nil {
		out["presence_penalty"] = *cfg.PresencePenalty
	}

	if cfg.FrequencyPenalty != nil {
		out["frequency_penalty"] = *cfg.FrequencyPenalty
	}

	if len(cfg.StopSequences) > 0 {
		out["stop"] = cfg.StopSequences
	}

	if cfg.Seed != nil {
		out["seed"] = *cfg.Seed
	}
}

func geminiApplyResponseFormat(out map[string]any, cfg *geminiGenerationConfig) {
	if schema := geminiResponseSchema(cfg); schema != nil {
		out["response_format"] = map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name": "response", "schema": schema, "strict": true,
			},
		}

		return
	}

	if strings.EqualFold(cfg.ResponseMimeType, "application/json") {
		out["response_format"] = map[string]any{"type": "json_object"}
	}
}

// geminiResponseSchema 优先用标准 JSON Schema 的 responseJsonSchema；
// responseSchema 是 Gemini/OpenAPI 形态（大写 type、nullable 等），需先归一化。
func geminiResponseSchema(cfg *geminiGenerationConfig) any {
	rawSchema := bytes.TrimSpace(cfg.ResponseJSONSchema)
	if len(rawSchema) > 0 && !bytes.Equal(rawSchema, []byte("null")) {
		return cfg.ResponseJSONSchema
	}

	if len(bytes.TrimSpace(cfg.ResponseSchema)) == 0 {
		return nil
	}

	var schema any
	if json.Unmarshal(cfg.ResponseSchema, &schema) != nil {
		return nil
	}

	return geminiJSONSchema(schema)
}

// geminiJSONSchema 把 Gemini/OpenAPI 风格 schema 转成标准 JSON Schema：
// 大写 type → 小写，移除 nullable / propertyOrdering 等 Gemini 专有字段。
func geminiJSONSchema(raw any) any {
	switch value := raw.(type) {
	case map[string]any:
		return geminiSchemaObject(value)
	case []any:
		return geminiSchemaList(value)
	default:
		return raw
	}
}

func geminiSchemaObject(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for key, value := range in {
		switch key {
		case "type":
			if typ := geminiSchemaType(value); typ != nil {
				out[key] = typ
			}
		case "nullable", "propertyOrdering":
			// Gemini 专有字段，标准 JSON Schema 不接受。
		case "properties":
			out[key] = geminiSchemaDict(value)
		case "items", "additionalProperties":
			out[key] = geminiJSONSchema(value)
		case "anyOf", "oneOf", "allOf":
			out[key] = geminiSchemaList(value)
		default:
			out[key] = value
		}
	}

	return out
}

func geminiSchemaType(raw any) any {
	typ, ok := raw.(string)
	if !ok {
		return raw
	}

	if strings.EqualFold(typ, "TYPE_UNSPECIFIED") {
		return nil
	}

	return strings.ToLower(typ)
}

func geminiSchemaDict(raw any) map[string]any {
	dict, ok := raw.(map[string]any)
	if !ok {
		return nil
	}

	out := make(map[string]any, len(dict))
	for key, value := range dict {
		out[key] = geminiJSONSchema(value)
	}

	return out
}

func geminiSchemaList(raw any) []any {
	list, ok := raw.([]any)
	if !ok {
		return nil
	}

	out := make([]any, 0, len(list))
	for _, item := range list {
		out = append(out, geminiJSONSchema(item))
	}

	return out
}

// geminiThinkingEffort 把 thinkingLevel / thinkingBudget 映射为 reasoning_effort。
func geminiThinkingEffort(cfg *geminiThinkingConfig) string {
	if cfg == nil {
		return ""
	}

	if level := strings.ToLower(strings.TrimSpace(cfg.ThinkingLevel)); level != "" {
		switch level {
		case "low", "medium", "high", "xhigh", "max":
			return level
		}
	}

	if cfg.ThinkingBudget == nil {
		return ""
	}

	budget := *cfg.ThinkingBudget
	switch {
	case budget <= 0:
		return ""
	case budget <= 4096:
		return "low"
	case budget <= 16384:
		return "medium"
	default:
		return "high"
	}
}

// geminiApplyTools 转换 functionDeclarations 与 toolConfig。
func geminiApplyTools(out map[string]any, tools []map[string]any, rawToolConfig json.RawMessage) {
	if specs := geminiTools(tools); len(specs) > 0 {
		out["tools"] = specs
	}

	if choice := geminiToolChoice(rawToolConfig); choice != nil {
		out["tool_choice"] = choice
	}
}

func geminiTools(groups []map[string]any) []any {
	out := make([]any, 0)

	for _, group := range groups {
		decls := geminiDeclarations(group)
		for _, decl := range decls {
			if spec := geminiToolSpec(decl); spec != nil {
				out = append(out, spec)
			}
		}
	}

	return out
}

func geminiDeclarations(group map[string]any) []map[string]any {
	raw, ok := group["functionDeclarations"].([]any)
	if !ok {
		raw, ok = group["function_declarations"].([]any)
	}

	if !ok {
		return nil
	}

	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if decl, ok := item.(map[string]any); ok {
			out = append(out, decl)
		}
	}

	return out
}

func geminiToolSpec(decl map[string]any) map[string]any {
	name := stringValue(decl["name"])
	if name == "" {
		return nil
	}

	fn := map[string]any{"name": name}

	if desc := stringValue(decl["description"]); desc != "" {
		fn["description"] = desc
	}

	if params := geminiToolParameters(decl); params != nil {
		fn["parameters"] = params
	}

	return map[string]any{"type": "function", "function": fn}
}

func geminiToolParameters(decl map[string]any) any {
	if params, ok := decl["parameters"]; ok && params != nil {
		return geminiJSONSchema(params)
	}

	if params, ok := decl["parametersJsonSchema"]; ok && params != nil {
		return params
	}

	return decl["parameters_json_schema"]
}

func geminiToolChoice(raw json.RawMessage) any {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}

	var cfg struct {
		FunctionCallingConfig struct {
			Mode                 string   `json:"mode"`
			AllowedFunctionNames []string `json:"allowedFunctionNames"`
		} `json:"functionCallingConfig"`
	}
	if json.Unmarshal(raw, &cfg) != nil {
		return nil
	}

	mode := strings.ToUpper(strings.TrimSpace(cfg.FunctionCallingConfig.Mode))
	switch mode {
	case "NONE":
		return "none"
	case "ANY":
		if names := cfg.FunctionCallingConfig.AllowedFunctionNames; len(names) == 1 {
			return map[string]any{"type": "function", "function": map[string]any{"name": names[0]}}
		}

		return "required"
	case "AUTO", "VALIDATED":
		return "auto"
	default:
		return nil
	}
}

// geminiSystemText 提取 systemInstruction 的文本；支持字符串与 {parts:[...]}。
func geminiSystemText(raw json.RawMessage) string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return ""
	}

	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}

	var obj struct {
		Parts []map[string]any `json:"parts"`
	}
	if json.Unmarshal(raw, &obj) != nil {
		return ""
	}

	var texts []string

	for _, part := range obj.Parts {
		if value := stringValue(part["text"]); value != "" {
			texts = append(texts, value)
		}
	}

	return strings.Join(texts, "\n")
}

// geminiCountTokens 以请求文本字节数估算 token（无本地 tokenizer）。
func (h *Handler) geminiCountTokens(w http.ResponseWriter, r *http.Request, model string) {
	body, ok := readGeminiBody(w, r, h.cfg.MaxBodyBytes)
	if !ok {
		return
	}

	var iface any
	if err := json.Unmarshal(body, &iface); err != nil {
		writeGeminiError(w, http.StatusBadRequest, "invalid JSON body", "")
		return
	}

	if target := h.backgroundTarget(model); target != "" {
		model = target
	}

	if !h.geminiCountTokensAllowed(w, r, model) {
		return
	}

	tokens := max((countJSONTextBytes(iface)+3)/4, 1)
	writeJSON(w, http.StatusOK, map[string]any{"totalTokens": tokens})
}

// geminiCountTokensAllowed 校验命名密钥模型白名单；与 chat 同口径解析别名/realm。
func (h *Handler) geminiCountTokensAllowed(w http.ResponseWriter, r *http.Request, model string) bool {
	keyUsage2 := keyUsageFrom(r.Context())
	if keyUsage2 == nil {
		return true
	}

	realm, bare, err := resolveKeyModel(model, h.cfg.ModelAliases, keyRealmFrom(r.Context()))
	if err != nil {
		writeGeminiError(w, http.StatusBadRequest, err.Error(), "")
		return false
	}

	if h.cfg.Keys.ModelAllowed(keyUsage2.id, realm+":"+bare) {
		return true
	}

	h.cfg.Keys.RecordRejected(keyUsage2.id)
	writeGeminiError(w, http.StatusForbidden, "model is not allowed for this API key", "")

	return false
}

// geminiResponder 把核心的 OpenAI 形态结果编码为 Gemini 响应。
type geminiResponder struct {
	w         http.ResponseWriter
	model     string
	keepalive time.Duration
}

func (g *geminiResponder) JSON(status int, body any) {
	writeJSON(g.w, status, geminiResponseFromOpenAI(body, g.model))
}

func (g *geminiResponder) Error(status int, code, msg, hint string) {
	writeGeminiError(g.w, status, msg, hint)
}

func (g *geminiResponder) Header(name, value string) { g.w.Header().Set(name, value) }

func (g *geminiResponder) Stream(rc io.Reader, hint func(string) string, repair *upstream.MarkupRepair, onErrFrame func(string)) error {
	writer := newGeminiStreamWriter(g.w, g.model)

	opts := make([]upstream.StreamOption, 0, 2)
	if onErrFrame != nil {
		opts = append(opts, upstream.WithErrorFrameObserver(onErrFrame))
	}

	opts = append(opts, upstream.WithMarkupRepair(repair))
	if g.keepalive > 0 {
		opts = append(opts, upstream.WithKeepaliveInterval(g.keepalive))
	}

	if err := upstream.StreamHint(writer, rc, hint, opts...); err != nil {
		return fmt.Errorf("stream hint: %w", err)
	}

	return nil
}

// geminiResponseFromOpenAI 把聚合后的 chat.completion 转成 GenerateContentResponse。
func geminiResponseFromOpenAI(body any, model string) map[string]any {
	obj, _ := body.(map[string]any)
	choice := firstOpenAIChoice(obj)

	parts := geminiResponseParts(choice)
	if len(parts) == 0 {
		parts = []any{map[string]any{"text": ""}}
	}

	candidate := map[string]any{
		"content":      map[string]any{"role": "model", "parts": parts},
		"finishReason": geminiFinishReason(choice),
		"index":        0,
	}

	resp := map[string]any{
		"candidates":    []any{candidate},
		"usageMetadata": geminiUsage(obj["usage"]),
		"modelVersion":  model,
	}

	if id := stringValue(obj["id"]); id != "" {
		resp["responseId"] = id
	}

	return resp
}

func geminiResponseParts(choice map[string]any) []any {
	if choice == nil {
		return nil
	}

	msg, _ := choice["message"].(map[string]any)
	parts := make([]any, 0, 3)

	if reasoning := deltaReasoning(msg); reasoning != "" {
		parts = append(parts, map[string]any{"text": reasoning, "thought": true})
	}

	text := responsesMessageText(msg["content"])
	if text == "" {
		text = stringValue(msg["refusal"])
	}

	if text != "" {
		parts = append(parts, map[string]any{"text": text})
	}

	parts = append(parts, geminiFunctionParts(msg["tool_calls"])...)

	return parts
}

func geminiFunctionParts(raw any) []any {
	calls := mapItems(raw)
	parts := make([]any, 0, len(calls))

	for _, call := range calls {
		fn, _ := call["function"].(map[string]any)

		name := stringValue(fn["name"])
		if name == "" {
			continue
		}

		args := geminiArgsObject(stringValue(fn["arguments"]))

		callPart := map[string]any{"name": name, "args": args}
		if id := stringValue(call["id"]); id != "" {
			callPart["id"] = id
		}

		parts = append(parts, map[string]any{"functionCall": callPart})
	}

	return parts
}

// geminiArgsObject 把 OpenAI arguments JSON 解析成 Gemini 需要的对象。
func geminiArgsObject(raw string) map[string]any {
	if strings.TrimSpace(raw) == "" {
		return map[string]any{}
	}

	var args map[string]any
	if json.Unmarshal([]byte(raw), &args) == nil {
		return args
	}

	return map[string]any{}
}

func geminiFinishReason(choice map[string]any) string {
	if choice == nil {
		return "STOP"
	}

	switch stringValue(choice["finish_reason"]) {
	case "length":
		return "MAX_TOKENS"
	case "content_filter":
		return "SAFETY"
	case "tool_calls", "function_call":
		return "STOP"
	default:
		return "STOP"
	}
}

func geminiUsage(raw any) map[string]any {
	usage, _ := raw.(map[string]any)
	prompt := intValue(usage["prompt_tokens"])
	output := intValue(usage["completion_tokens"])

	total := intValue(usage["total_tokens"])
	if total == 0 {
		total = prompt + output
	}

	cached, _ := responsesCachedTokens(usage)
	thoughts := responsesReasoningTokens(usage)

	// Gemini 的 candidatesTokenCount 只含可见输出，thinking 单独计数；
	// OpenAI 的 completion_tokens 含 reasoning，需要拆开避免客户端重复累加。
	visible := max(output-thoughts, 0)

	out := map[string]any{
		"promptTokenCount":     prompt,
		"candidatesTokenCount": visible,
		"totalTokenCount":      total,
	}
	if cached > 0 {
		out["cachedContentTokenCount"] = cached
	}

	if thoughts > 0 {
		out["thoughtsTokenCount"] = thoughts
	}

	return out
}

// writeGeminiError 写出 Gemini 错误信封（非流式）。
func writeGeminiError(w http.ResponseWriter, status int, msg, hint string) {
	e := map[string]any{"code": status, "message": msg, "status": geminiStatus(status)}
	if hint != "" {
		e["gateway_hint"] = hint
	}

	writeJSON(w, status, map[string]any{"error": e})
}

func geminiStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "INVALID_ARGUMENT"
	case http.StatusUnauthorized:
		return "UNAUTHENTICATED"
	case http.StatusForbidden:
		return "PERMISSION_DENIED"
	case http.StatusNotFound:
		return "NOT_FOUND"
	case http.StatusRequestEntityTooLarge:
		return "RESOURCE_EXHAUSTED"
	case http.StatusTooManyRequests:
		return "RESOURCE_EXHAUSTED"
	case http.StatusInternalServerError:
		return "INTERNAL"
	case http.StatusBadGateway, http.StatusServiceUnavailable:
		return "UNAVAILABLE"
	default:
		return "UNKNOWN"
	}
}

// geminiStreamWriter 接收 upstream.StreamHint 写出的 OpenAI SSE 帧，转换为
// Gemini GenerateContentResponse 的 SSE data 帧。Gemini 客户端不识别 [DONE]，
// 这里负责吞掉它并补最终 finishReason / usageMetadata 帧。
type geminiStreamWriter struct {
	w     http.ResponseWriter
	fl    http.Flusher
	model string

	pending  []byte
	finished bool
	errored  bool
	sawText  bool

	id      string
	tools   map[int]*geminiToolState
	toolSeq []int

	finishReason    string
	inputTokens     int
	outputTokens    int
	cachedTokens    int
	reasoningTokens int
}

type geminiToolState struct {
	id   string
	name string
	args strings.Builder
}

func newGeminiStreamWriter(w http.ResponseWriter, model string) *geminiStreamWriter {
	fl, _ := w.(http.Flusher)

	return &geminiStreamWriter{w: w, fl: fl, model: model, tools: map[int]*geminiToolState{}}
}

func (g *geminiStreamWriter) Header() http.Header         { return g.w.Header() }
func (g *geminiStreamWriter) WriteHeader(code int)        { g.w.WriteHeader(code) }
func (g *geminiStreamWriter) Unwrap() http.ResponseWriter { return g.w }
func (g *geminiStreamWriter) Flush() {
	if g.fl != nil {
		g.fl.Flush()
	}
}

func (g *geminiStreamWriter) Write(nums []byte) (int, error) {
	g.pending = append(g.pending, nums...)

	for {
		i := bytes.IndexByte(g.pending, '\n')
		if i < 0 {
			break
		}

		line := strings.TrimRight(string(g.pending[:i]), "\r")

		g.pending = g.pending[i+1:]
		if err := g.handleLine(line); err != nil {
			return 0, err
		}
	}

	return len(nums), nil
}

func (g *geminiStreamWriter) handleLine(line string) error {
	if g.finished {
		return nil
	}

	if strings.HasPrefix(line, ":") {
		if _, err := io.WriteString(g.w, line+"\n\n"); err != nil {
			return fmt.Errorf("handleLine: %w", err)
		}

		g.Flush()

		return nil
	}

	payload, ok := strings.CutPrefix(line, "data: ")
	if !ok {
		payload, ok = strings.CutPrefix(line, "data:")
	}

	if !ok {
		return nil
	}

	payload = strings.TrimSpace(payload)
	if payload == "[DONE]" {
		return g.finish()
	}

	return g.handleChunk(payload)
}

func (g *geminiStreamWriter) handleChunk(payload string) error {
	var chunk map[string]any
	if json.Unmarshal([]byte(payload), &chunk) != nil {
		return nil //nolint:nilerr // 非 JSON 噪声帧丢弃
	}

	if errObj, ok := chunk["error"].(map[string]any); ok {
		return g.handleUpstreamError(errObj)
	}

	if id := stringValue(chunk["id"]); id != "" && g.id == "" {
		g.id = id
	}

	g.captureUsage(chunk)

	choice := firstOpenAIChoice(chunk)
	if choice == nil {
		return nil
	}

	if delta, _ := choice["delta"].(map[string]any); delta != nil {
		if err := g.applyDelta(delta); err != nil {
			return err
		}
	}

	if reason := stringValue(choice["finish_reason"]); reason != "" {
		g.finishReason = reason
	}

	return nil
}

func (g *geminiStreamWriter) applyDelta(delta map[string]any) error {
	text := stringValue(delta["content"])
	if text == "" {
		text = stringValue(delta["refusal"])
	}

	if text != "" {
		g.sawText = true
		if err := g.emitText(text, false); err != nil {
			return err
		}
	}

	if reasoning := deltaReasoning(delta); reasoning != "" {
		g.sawText = true
		if err := g.emitText(reasoning, true); err != nil {
			return err
		}
	}

	return g.collectToolCalls(delta["tool_calls"])
}

func (g *geminiStreamWriter) emitText(text string, thought bool) error {
	part := map[string]any{"text": text}
	if thought {
		part["thought"] = true
	}

	return g.writeChunk(map[string]any{
		"candidates": []any{map[string]any{
			"content": map[string]any{"role": "model", "parts": []any{part}},
			"index":   0,
		}},
		"modelVersion": g.model,
	})
}

func (g *geminiStreamWriter) collectToolCalls(raw any) error {
	for _, call := range mapItems(raw) {
		if call == nil {
			continue
		}

		idx := intValue(call["index"])

		state := g.tools[idx]
		if state == nil {
			state = &geminiToolState{}
			g.tools[idx] = state
			g.toolSeq = append(g.toolSeq, idx)
		}

		if name, id := toolCallIdentity(call); name != "" {
			state.name = name
			state.id = id
		}

		state.args.WriteString(toolCallArguments(call))
	}

	return nil
}

func (g *geminiStreamWriter) handleUpstreamError(errObj map[string]any) error {
	g.errored = true

	msg := stringValue(errObj["message"])
	if msg == "" {
		msg = "upstream stream error"
	}

	e := map[string]any{"code": http.StatusServiceUnavailable, "message": msg, "status": "UNAVAILABLE"}
	if hint := stringValue(errObj["gateway_hint"]); hint != "" {
		e["gateway_hint"] = hint
	}

	if err := g.writeChunk(map[string]any{"error": e}); err != nil {
		return err
	}

	g.finished = true

	return nil
}

func (g *geminiStreamWriter) finish() error {
	if g.finished {
		return nil
	}

	g.finished = true
	if g.errored {
		return nil
	}

	toolParts := g.finishToolParts()
	if len(toolParts) > 0 {
		if err := g.writeChunk(map[string]any{
			"candidates": []any{map[string]any{
				"content": map[string]any{"role": "model", "parts": toolParts},
				"index":   0,
			}},
			"modelVersion": g.model,
		}); err != nil {
			return err
		}
	}

	parts := []any{}
	if !g.sawText && len(toolParts) == 0 {
		parts = []any{map[string]any{"text": ""}}
	}

	usage := map[string]any{
		"promptTokenCount":     g.inputTokens,
		"candidatesTokenCount": max(g.outputTokens-g.reasoningTokens, 0),
		"totalTokenCount":      g.inputTokens + g.outputTokens,
	}
	if g.cachedTokens > 0 {
		usage["cachedContentTokenCount"] = g.cachedTokens
	}

	if g.reasoningTokens > 0 {
		usage["thoughtsTokenCount"] = g.reasoningTokens
	}

	chunk := map[string]any{
		"candidates": []any{map[string]any{
			"content":      map[string]any{"role": "model", "parts": parts},
			"finishReason": geminiFinishReason(map[string]any{"finish_reason": g.finishReason}),
			"index":        0,
		}},
		"usageMetadata": usage,
		"modelVersion":  g.model,
	}
	if g.id != "" {
		chunk["responseId"] = g.id
	}

	return g.writeChunk(chunk)
}

func (g *geminiStreamWriter) finishToolParts() []any {
	parts := make([]any, 0, len(g.toolSeq))
	for _, idx := range g.toolSeq {
		state := g.tools[idx]
		if state == nil || state.name == "" {
			continue
		}

		call := map[string]any{"name": state.name, "args": geminiArgsObject(state.args.String())}
		if state.id != "" {
			call["id"] = state.id
		}

		parts = append(parts, map[string]any{"functionCall": call})
	}

	return parts
}

func (g *geminiStreamWriter) captureUsage(chunk map[string]any) {
	usage, _ := chunk["usage"].(map[string]any)
	if usage == nil {
		return
	}

	if n := intValue(usage["prompt_tokens"]); n > 0 {
		g.inputTokens = n
	}

	if n := intValue(usage["completion_tokens"]); n > 0 {
		g.outputTokens = n
	}

	if cached, _ := responsesCachedTokens(usage); cached > 0 {
		g.cachedTokens = cached
	}

	if n := responsesReasoningTokens(usage); n > 0 {
		g.reasoningTokens = n
	}
}

func (g *geminiStreamWriter) writeChunk(payload map[string]any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("gemini chunk: %w", err)
	}

	if _, err := fmt.Fprintf(g.w, "data: %s\n\n", raw); err != nil {
		return fmt.Errorf("gemini chunk: %w", err)
	}

	g.Flush()

	return nil
}
