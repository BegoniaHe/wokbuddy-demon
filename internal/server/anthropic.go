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
	"slices"
	"strings"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/httpauth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/logfmt"
	"github.com/BegoniaHe/wokbuddy-demon/internal/reqlog"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

var (
	errEmptyRequestBody       = errors.New("empty request body")
	errModelIsRequired        = errors.New("model is required")
	errMessagesIsRequired     = errors.New("messages is required")
	errMessageContentRequired = errors.New("message content is required")
	errMaxTokensNegative      = errors.New("max_tokens must be >= 0")
	errTemperatureOutOfRange  = errors.New("temperature must be in [0,1]")
	errTopPOutOfRange         = errors.New("top_p must be in [0,1]")
	errTopKNegative           = errors.New("top_k must be >= 0")
	errMetadataUserIDTooLong  = errors.New("metadata.user_id is too long")
	errThinkingBudgetTooSmall = errors.New("thinking.budget_tokens must be >= 1024")
	errThinkingBudgetTooLarge = errors.New("thinking.budget_tokens must be less than max_tokens")
	errToolNameTooLong        = errors.New("tool name is too long")
	errTooManyMessages        = errors.New("too many messages")
	errAnthropicMessageFailed = errors.New("anthropicMessage failed")
	errToolUseNameIsRequired  = errors.New("tool_use name is required")
	errToolUseIDIsRequired    = errors.New("tool_use id is required")
	errToolResultIDIsRequired = errors.New("tool_result tool_use_id is required")
	errToolChoiceNameRequired = errors.New("tool_choice name is required")

	errAnthropicUnsupportedField = errors.New("unsupported field")
	errAnthropicUnsupportedBlock = errors.New("unsupported content block")
	errAnthropicServerTool       = errors.New("unsupported server tool type")
	errAnthropicToolSchema       = errors.New("invalid tool input_schema")
	errAnthropicToolInput        = errors.New("invalid tool_use input")
	errAnthropicToolNameRequired = errors.New("tool name is required")
	errAnthropicImageSource      = errors.New("invalid image source")
	errAnthropicDocumentSource   = errors.New("invalid document source")
	errAnthropicToolResult       = errors.New("invalid tool_result content")
	errAnthropicSearchResult     = errors.New("invalid search_result")
	errAnthropicContainerUpload  = errors.New("invalid container_upload")
	errAnthropicMediaType        = errors.New("unsupported media_type")
)

// anthropicMaxMessages 官方单请求消息条数上限。
const anthropicMaxMessages = 100_000

// anthropicMaxUserIDLen 官方 metadata.user_id 最大长度。
const anthropicMaxUserIDLen = 512

// 工具名长度上限：官方对 tools[].name 限制 64 字符，这里放宽，避免把 MCP
// 生成的长工具名（mcp__server__tool 一类）拦在网关，只挡住明显异常的输入。
// tool_use 块里的 name 是回放历史消息，额外放宽到 200，避免旧会话被拒。
const (
	anthropicMaxToolNameLen    = 128
	anthropicMaxToolUseNameLen = 200
)

// stopReasonKey Anthropic 响应里表示停止原因的字段名。
const stopReasonKey = "stop_reason"

// anthropicMessagesHandler 是 /v1/messages 的入口（鉴权由 withClientAuth 负责）。
func (h *Handler) anthropicMessagesHandler(w http.ResponseWriter, r *http.Request) {
	input, out, ok := h.newAnthropicChatInput(w, r, h.cfg.KeepaliveInterval, h.cfg.MaxBodyBytes)
	if !ok {
		return
	}

	input.clientIP = httpauth.ClientIP(r, h.cfg.TrustedProxies)

	h.serveChat(r, &input, out)
}

// setAnthropicRequestID 设置 Anthropic SDK 读取的 request-id 响应头并返回该值。
// 优先复用请求日志 trace id；未启用请求日志时生成一个，并同步 X-Request-ID 便于排查。
func setAnthropicRequestID(w http.ResponseWriter, r *http.Request) string {
	id := ""
	if trace := requestTraceFrom(r); trace != nil {
		id = trace.id
	}

	if id == "" {
		id = w.Header().Get("Request-Id")
	}

	if id == "" {
		id = w.Header().Get("X-Request-ID")
	}

	if id == "" {
		id = reqlog.NewRequestID()
		w.Header().Set("X-Request-ID", id)
	}

	w.Header().Set("Request-Id", id)

	return id
}

// anthropicCountTokensHandler 返回 Anthropic 形态的输入 token 估算。
// 网关没有本地 tokenizer；这里按请求 JSON 文本字节数做近似（4 字节 ≈ 1 token），
// 只用于客户端上下文预算，不作为计费依据。
func (h *Handler) anthropicCountTokensHandler(w http.ResponseWriter, r *http.Request) {
	setAnthropicRequestID(w, r)
	limitRequestBody(w, r, h.cfg.MaxBodyBytes)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		if isBodyTooLarge(err) {
			writeAnthropicError(w, http.StatusRequestEntityTooLarge, "request_too_large",
				bodyTooLargeMessage(h.cfg.MaxBodyBytes), "缩小请求体或调大 server.max_body_bytes")

			return
		}

		writeAnthropicError(w, http.StatusBadRequest, errorTypeInvalidReq, "read body: "+err.Error(), "")

		return
	}

	var iface any
	if err := json.Unmarshal(body, &iface); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, errorTypeInvalidReq, "invalid JSON body", "")
		return
	}

	obj, _ := iface.(map[string]any)
	if messages, ok := obj["messages"].([]any); !ok || len(messages) == 0 {
		writeAnthropicError(w, http.StatusBadRequest, errorTypeInvalidReq, errMessagesIsRequired.Error(), "")
		return
	}

	model, _ := obj["model"].(string)
	if strings.TrimSpace(model) == "" {
		writeAnthropicError(w, http.StatusBadRequest, errorTypeInvalidReq, errModelIsRequired.Error(), "")
		return
	}
	// 与 /v1/messages 同口径：白名单校验前先应用后台模型改写，避免同一模型在
	// 两个端点上判定不一致。
	if target := h.backgroundTarget(model); target != "" {
		model = target
	}

	if !h.countTokensModelVisible(w, r, model) {
		return
	}

	if !h.countTokensModelAllowed(w, r, model) {
		return
	}

	tokens := max((countAnthropicPromptBytes(obj)+3)/4, 1)
	writeJSON(w, http.StatusOK, map[string]any{keyInputTokens: tokens})
}

// countTokensModelVisible 校验 count_tokens 的模型可见性（models.hidden 黑名单与
// models.allowed 部署级白名单）：与 /v1/messages 同口径地把模型解析到最终
// (realm, bare) 后判定，隐藏或不在白名单内即 400。
// 判定**先于**命名密钥白名单（部署策略不给任何 key 放行）。
func (h *Handler) countTokensModelVisible(w http.ResponseWriter, r *http.Request, model string) bool {
	keyRealm := keyRealmFrom(r.Context())

	realm, bare, err := resolveKeyModel(model, h.requestModelAliases(model, keyRealm), keyRealm)
	if err != nil {
		// 解析失败不在此拦：白名单/主路径照旧给出各自的错误语义。
		return true
	}

	if h.modelHidden(realm, bare) {
		writeAnthropicError(w, http.StatusBadRequest, errorTypeInvalidReq,
			"model is hidden by gateway config (models.hidden); pick another model", "")

		return false
	}

	if !h.modelAllowedByConfig(realm, bare) {
		writeAnthropicError(w, http.StatusBadRequest, errorTypeInvalidReq,
			"model is not in the gateway allowlist (models.allowed); pick another model", "")

		return false
	}

	return true
}

// countTokensModelAllowed 校验 count_tokens 的命名密钥模型白名单；与
// /v1/messages 同口径：先解析别名与 key 出口绑定，再按最终 realm:bare 匹配。
// 返回 false 时已写出 Anthropic 错误。
func (h *Handler) countTokensModelAllowed(w http.ResponseWriter, r *http.Request, model string) bool {
	keyUsage2 := keyUsageFrom(r.Context())
	if keyUsage2 == nil {
		return true
	}

	keyRealm := keyRealmFrom(r.Context())

	realm, bare, err := resolveKeyModel(model, h.requestModelAliases(model, keyRealm), keyRealm)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, errorTypeInvalidReq, err.Error(), "")
		return false
	}

	if h.cfg.Keys.ModelAllowed(keyUsage2.id, realm+":"+bare) {
		return true
	}

	h.cfg.Keys.RecordRejected(keyUsage2.id)
	writeAnthropicError(w, http.StatusForbidden, "permission_error", "model is not allowed for this API key", "")

	return false
}

// countAnthropicPromptBytes 汇总 prompt 文本的字节数：跳过 model 名与 base64
// 载荷（data/file_data），避免把模型名和图片编码算进输入 token。
func countAnthropicPromptBytes(obj map[string]any) int {
	return countAnthropicTextBytes(obj, true)
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

func countAnthropicTextBytes(v any, root bool) int {
	switch x := v.(type) {
	case string:
		return len(x)
	case []any:
		return countAnthropicSliceBytes(x)
	case map[string]any:
		return countAnthropicMapBytes(x, root)
	default:
		return 0
	}
}

func countAnthropicSliceBytes(items []any) int {
	n := 0
	for _, item := range items {
		n += countAnthropicTextBytes(item, false)
	}

	return n
}

func countAnthropicMapBytes(x map[string]any, root bool) int {
	n := 0
	base64Source := stringValue(x["type"]) == "base64"

	for key, item := range x {
		if root && key == "model" {
			continue
		}

		if key == "file_data" || (base64Source && key == "data") {
			continue
		}

		n += countAnthropicTextBytes(item, false)
	}

	return n
}

// newAnthropicChatInput 读取并转换 Anthropic 请求体；失败时已写出 Anthropic 错误。
func (h *Handler) newAnthropicChatInput(
	w http.ResponseWriter,
	r *http.Request,
	keepalive time.Duration,
	maxBodyBytes int64,
) (chatInput, chatResponder, bool) {
	setAnthropicRequestID(w, r)
	limitRequestBody(w, r, maxBodyBytes)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		if isBodyTooLarge(err) {
			writeAnthropicError(w, http.StatusRequestEntityTooLarge, "request_too_large",
				bodyTooLargeMessage(maxBodyBytes), "缩小请求体或调大 server.max_body_bytes")

			return chatInput{}, nil, false
		}

		writeAnthropicError(w, http.StatusBadRequest, errorTypeInvalidReq, "read body: "+err.Error(), "")

		return chatInput{}, nil, false
	}

	converted, prelude, err := h.anthropicConvertWithSearch(r.Context(), r, body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, errorTypeInvalidReq, err.Error(), "")
		return chatInput{}, nil, false
	}

	chatInput2 := newChatInputFromRequest(r, converted.openAI, converted.stream, converted.model)

	return chatInput2, &anthropicResponder{
		w: w, model: converted.model, keepalive: keepalive, options: converted.options,
		prelude: prelude,
	}, true
}

// anthropicRequest 只声明转换需要的字段；未知字段由客户端协议自行演进，忽略。
type anthropicRequest struct {
	Model         string            `json:"model"`
	MaxTokens     *int              `json:"max_tokens"`
	System        json.RawMessage   `json:"system"`
	Messages      []json.RawMessage `json:"messages"`
	CacheControl  json.RawMessage   `json:"cache_control"`
	Tools         []map[string]any  `json:"tools"`
	ToolChoice    json.RawMessage   `json:"tool_choice"`
	Temperature   *float64          `json:"temperature"`
	TopP          *float64          `json:"top_p"`
	TopK          *float64          `json:"top_k"`
	Metadata      json.RawMessage   `json:"metadata"`
	ServiceTier   string            `json:"service_tier"`
	Speed         string            `json:"speed"`
	InferenceGeo  string            `json:"inference_geo"`
	Container     json.RawMessage   `json:"container"`
	Diagnostics   json.RawMessage   `json:"diagnostics"`
	StopSequences []string          `json:"stop_sequences"`
	Stream        bool              `json:"stream"`
	Thinking      json.RawMessage   `json:"thinking"`
	OutputConfig  json.RawMessage   `json:"output_config"`
}

// anthropicResponseOptions 是请求侧决定响应编码的选项（不进入上游请求体）。
type anthropicResponseOptions struct {
	omitThinking  bool
	stopSequences []string
}

// anthropicConversion 是一次入站请求转换的完整结果。
type anthropicConversion struct {
	openAI  []byte
	model   string
	stream  bool
	options anthropicResponseOptions
}

// anthropicConvert 转换请求体并保留响应编码选项。
func anthropicConvert(body []byte) (anthropicConversion, error) {
	req, err := decodeAnthropicRequest(body)
	if err != nil {
		return anthropicConversion{}, err
	}

	return anthropicConvertRequest(req)
}

// anthropicConvertRequest 把已解码请求转换为内部 chat 请求（不做搜索）。
func anthropicConvertRequest(req *anthropicRequest) (anthropicConversion, error) {
	out, err := buildOpenAIRequest(req)
	if err != nil {
		return anthropicConversion{}, err
	}

	encoded, err := json.Marshal(out)
	if err != nil {
		return anthropicConversion{}, fmt.Errorf("encode request: %w", err)
	}

	return anthropicConversion{
		openAI: encoded,
		model:  req.Model,
		stream: req.Stream,
		options: anthropicResponseOptions{
			omitThinking:  anthropicThinkingOmitted(req.Thinking),
			stopSequences: req.StopSequences,
		},
	}, nil
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

	if err := validateAnthropicRequest(&req); err != nil {
		return nil, err
	}

	return &req, nil
}

// validateAnthropicRequest 校验官方参数约束。
func validateAnthropicRequest(req *anthropicRequest) error {
	if strings.TrimSpace(req.Model) == "" {
		return errModelIsRequired
	}

	if len(req.Messages) == 0 {
		return errMessagesIsRequired
	}

	if len(req.Messages) > anthropicMaxMessages {
		return errTooManyMessages
	}

	if err := validateAnthropicThinking(req); err != nil {
		return err
	}

	return validateAnthropicSampling(req)
}

// validateAnthropicThinking 校验 enabled thinking 的 budget 约束。
func validateAnthropicThinking(req *anthropicRequest) error {
	if len(bytes.TrimSpace(req.Thinking)) == 0 || string(bytes.TrimSpace(req.Thinking)) == "null" {
		return nil
	}

	var thinking struct {
		Type         string `json:"type"`
		BudgetTokens int    `json:"budget_tokens"`
	}
	if json.Unmarshal(req.Thinking, &thinking) != nil {
		return nil //nolint:nilerr // 非对象 thinking 交由后续映射忽略
	}

	if thinking.Type != "enabled" || thinking.BudgetTokens == 0 {
		return nil
	}

	if thinking.BudgetTokens < 1024 {
		return errThinkingBudgetTooSmall
	}

	if req.MaxTokens != nil && thinking.BudgetTokens >= *req.MaxTokens {
		return errThinkingBudgetTooLarge
	}

	return nil
}

// validateAnthropicSampling 校验采样参数范围。
func validateAnthropicSampling(req *anthropicRequest) error {
	if req.MaxTokens != nil && *req.MaxTokens < 0 {
		return errMaxTokensNegative
	}

	if unitRangeInvalid(req.Temperature) {
		return errTemperatureOutOfRange
	}

	if unitRangeInvalid(req.TopP) {
		return errTopPOutOfRange
	}

	if req.TopK != nil && *req.TopK < 0 {
		return errTopKNegative
	}

	return nil
}

// unitRangeInvalid 报告可选浮点参数是否越出 [0,1]。
func unitRangeInvalid(value *float64) bool {
	return value != nil && (*value < 0 || *value > 1)
}

// buildOpenAIRequest 组装 OpenAI chat 请求体。
func buildOpenAIRequest(req *anthropicRequest) (map[string]any, error) {
	out := map[string]any{"model": req.Model, "stream": req.Stream}

	system, err := anthropicSystemContent(req.System)
	if err != nil {
		return nil, err
	}

	msgs := make([]any, 0, len(req.Messages)+1)
	for _, raw := range req.Messages {
		role, content, err := decodeAnthropicMessage(raw)
		if err != nil {
			return nil, err
		}

		if role == "system" {
			added, addErr := anthropicSystemContent(content)
			if addErr != nil {
				return nil, addErr
			}

			system = mergeAnthropicSystem(system, added)

			continue
		}

		converted, err := anthropicMessageContent(role, content)
		if err != nil {
			return nil, err
		}

		msgs = append(msgs, converted...)
	}

	if system != nil {
		msgs = append([]any{map[string]any{"role": "system", "content": system}}, msgs...)
	}

	out["messages"] = msgs
	if err := applyAnthropicParams(out, req); err != nil {
		return nil, err
	}

	return out, nil
}

// mergeAnthropicSystem 合并顶层 system 与 messages 中 role=system 的内容。
func mergeAnthropicSystem(existing, add any) any {
	if add == nil {
		return existing
	}

	if existing == nil {
		return add
	}

	existingString, existingIsString := existing.(string)

	addString, addIsString := add.(string)
	if existingIsString && addIsString {
		return existingString + "\n\n" + addString
	}

	parts := append(anthropicSystemParts(existing), anthropicSystemParts(add)...)

	return parts
}

// anthropicSystemParts 把已归一化的 system content 压成 OpenAI text part 数组。
func anthropicSystemParts(content any) []any {
	if text, ok := content.(string); ok {
		return []any{map[string]any{"type": "text", "text": text}}
	}

	parts, _ := content.([]any)

	return parts
}

// applyAnthropicParams 映射采样参数、工具与 thinking 等可选字段。
func applyAnthropicParams(out map[string]any, req *anthropicRequest) error {
	applyAnthropicSampling(out, req)

	if err := applyAnthropicTools(out, req); err != nil {
		return err
	}

	applyAnthropicThinking(out, req)

	return applyAnthropicExtensions(out, req)
}

// applyAnthropicSampling 映射 max_tokens / temperature / top_p / top_k / stop。
func applyAnthropicSampling(out map[string]any, req *anthropicRequest) {
	if req.MaxTokens != nil {
		out["max_tokens"] = *req.MaxTokens
	}

	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}

	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}

	if req.TopK != nil {
		out["top_k"] = *req.TopK
	}

	if len(req.StopSequences) > 0 {
		out["stop"] = req.StopSequences
	}

	if cc := decodeRawJSON(req.CacheControl); cc != nil {
		out["cache_control"] = cc
	}
}

// applyAnthropicExtensions 映射有 OpenAI 对应形态的扩展字段；无对应形态的字段
// 明确拒绝，避免客户端以为已生效。
func applyAnthropicExtensions(out map[string]any, req *anthropicRequest) error {
	if metadata := decodeRawJSON(req.Metadata); metadata != nil {
		table, ok := metadata.(map[string]any)
		if !ok {
			return fmt.Errorf("%w: metadata must be an object", errAnthropicUnsupportedField)
		}

		if userID, _ := table["user_id"].(string); len(userID) > anthropicMaxUserIDLen {
			return errMetadataUserIDTooLong
		}

		out["metadata"] = metadata
	}

	if req.ServiceTier != "" {
		out["service_tier"] = req.ServiceTier
	}

	if req.Speed != "" {
		out["speed"] = req.Speed
	}

	unsupported := []struct {
		name string
		raw  json.RawMessage
	}{
		{"inference_geo", json.RawMessage(req.InferenceGeo)},
		{"container", req.Container},
		{"diagnostics", req.Diagnostics},
	}
	for _, field := range unsupported {
		if !emptyJSONField(field.raw) {
			return fmt.Errorf("%w %q", errAnthropicUnsupportedField, field.name)
		}
	}

	return nil
}

// emptyJSONField 报告字段是否没有实际内容（缺失 / null / 空串 / 空对象 / 空数组）。
func emptyJSONField(raw json.RawMessage) bool {
	switch string(bytes.TrimSpace(raw)) {
	case "", "null", `""`, "{}", "[]":
		return true
	default:
		return false
	}
}

// decodeRawJSON 把已确认存在的 RawMessage 解码为通用 JSON 值。
func decodeRawJSON(raw json.RawMessage) any {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}

	return value
}

// applyAnthropicTools 映射 tools / tool_choice 与并行控制。
func applyAnthropicTools(out map[string]any, req *anthropicRequest) error {
	if len(req.Tools) > 0 {
		tools, err := anthropicTools(req.Tools)
		if err != nil {
			return err
		}

		out["tools"] = tools
	}

	choice, err := anthropicToolChoice(req.ToolChoice)
	if err != nil {
		return err
	}

	if choice == nil {
		return nil
	}

	out["tool_choice"] = choice
	// Anthropic 把并行控制放在 tool_choice.disable_parallel_tool_use；
	// chat/completions 用独立字段表达。
	if anthropicDisableParallel(req.ToolChoice) {
		out["parallel_tool_calls"] = false
	}

	return nil
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
	if format == nil || stringValue(format["type"]) != jsonSchemaType {
		return nil
	}

	schema, ok := format["schema"]
	if !ok {
		return nil
	}

	return map[string]any{
		"type": jsonSchemaType,
		jsonSchemaType: map[string]any{
			"name": "output", "schema": schema, "strict": true,
		},
	}
}

// anthropicSystemContent 转换 system：无 cache_control 时压成字符串（与旧行为一致），
// 带 cache_control 时保留 text part 数组，让上游 prompt cache 标记不被网关吞掉。
// 非 text block 没有 OpenAI 对应形态，返回明确错误而不是静默丢弃。
//
//nolint:nilnil // nil content 表示无 system，是合法输入。
func anthropicSystemContent(raw json.RawMessage) (any, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	var text string
	if json.Unmarshal(raw, &text) == nil {
		if text == "" {
			return nil, nil
		}

		return text, nil
	}

	var blocks []map[string]any
	if json.Unmarshal(raw, &blocks) != nil {
		return nil, fmt.Errorf("invalid system content: %w", errAnthropicMessageFailed)
	}

	parts := make([]any, 0, len(blocks))
	texts := make([]string, 0, len(blocks))

	for _, block := range blocks {
		if stringValue(block["type"]) != "text" {
			return nil, fmt.Errorf("%w %q in system", errAnthropicUnsupportedBlock, stringValue(block["type"]))
		}

		part := textPart(block)
		if part == nil {
			continue
		}

		parts = append(parts, part)
		texts = append(texts, stringValue(part["text"]))
	}

	if len(parts) == 0 {
		return nil, nil
	}

	if !hasCacheControl(parts) {
		return strings.Join(texts, "\n\n"), nil
	}

	return parts, nil
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
	copyCacheControl(part, block)

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

// decodeAnthropicMessage 解码消息的 role 与 content。
func decodeAnthropicMessage(raw json.RawMessage) (string, json.RawMessage, error) {
	var msg struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return "", nil, fmt.Errorf("invalid message: %w", err)
	}

	content := bytes.TrimSpace(msg.Content)
	if len(content) == 0 || string(content) == "null" {
		return "", nil, errMessageContentRequired
	}

	return msg.Role, msg.Content, nil
}

// anthropicMessageContent 把 user / assistant 消息内容转换为 OpenAI 消息。
// user 里的 tool_result 拆成 role=tool，其余内容合并为一条 user；assistant 的
// tool_use 转成 tool_calls，thinking 回填 reasoning_content。
func anthropicMessageContent(role string, content json.RawMessage) ([]any, error) {
	if role != "user" && role != "assistant" {
		return nil, fmt.Errorf("unsupported message role %q: %w", role, errAnthropicMessageFailed)
	}

	var text string
	if json.Unmarshal(content, &text) == nil {
		if text == "" {
			return nil, errMessageContentRequired
		}

		return []any{map[string]any{"role": role, "content": text}}, nil
	}

	var blocks []map[string]any
	if err := json.Unmarshal(content, &blocks); err != nil {
		return nil, fmt.Errorf("invalid %s content: %w", role, err)
	}

	if len(blocks) == 0 {
		return nil, errMessageContentRequired
	}

	if role == "assistant" {
		return anthropicAssistantMessage(blocks)
	}

	return anthropicUserMessage(blocks)
}

// anthropicAssistantPart 是 assistant 单个内容块的转换结果。
type anthropicAssistantPart struct {
	text     string
	thinking string
	toolCall any
}

// anthropicAssistantMessage 合并 assistant 文本、thinking 与 tool_use 为一条 OpenAI 消息。
func anthropicAssistantMessage(blocks []map[string]any) ([]any, error) {
	var (
		texts     []string
		reasoning strings.Builder
		toolCalls []any
	)

	for _, block := range blocks {
		part, err := anthropicAssistantBlock(block)
		if err != nil {
			return nil, err
		}

		if part.text != "" {
			texts = append(texts, part.text)
		}

		if part.thinking != "" {
			reasoning.WriteString(part.thinking)
		}

		if part.toolCall != nil {
			toolCalls = append(toolCalls, part.toolCall)
		}
	}

	msg := map[string]any{"role": "assistant", "content": strings.Join(texts, "\n")}
	if parts := textParts(blocks); hasCacheControl(parts) {
		msg["content"] = parts
	}

	if reasoning.Len() > 0 {
		msg["reasoning_content"] = reasoning.String()
	}

	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}

	return []any{msg}, nil
}

func anthropicAssistantBlock(block map[string]any) (anthropicAssistantPart, error) {
	switch stringValue(block["type"]) {
	case "text":
		text, _ := block["text"].(string)
		return anthropicAssistantPart{text: text}, nil
	case "thinking":
		text, _ := block["thinking"].(string)
		return anthropicAssistantPart{thinking: text}, nil
	case "redacted_thinking":
		// 加密块对非 Anthropic 上游没有可验证语义，但也没有可见内容；
		// 跳过即可让会话继续，避免整轮请求因签名块 400。
		return anthropicAssistantPart{}, nil
	case "tool_use":
		call, err := anthropicToolUseCall(block)
		if err != nil {
			return anthropicAssistantPart{}, err
		}

		return anthropicAssistantPart{toolCall: call}, nil
	default:
		return anthropicAssistantPart{}, fmt.Errorf(
			"%w %q in assistant", errAnthropicUnsupportedBlock, stringValue(block["type"]),
		)
	}
}

// anthropicToolUseCall 把单个 tool_use 块转成 OpenAI tool_call。
func anthropicToolUseCall(block map[string]any) (any, error) {
	id, _ := block["id"].(string)
	if id == "" {
		return nil, errToolUseIDIsRequired
	}

	name, _ := block["name"].(string)
	if name == "" {
		return nil, errToolUseNameIsRequired
	}

	if len(name) > anthropicMaxToolUseNameLen {
		return nil, errToolNameTooLong
	}

	input := block["input"]
	if input == nil {
		input = map[string]any{}
	}

	if _, ok := input.(map[string]any); !ok {
		return nil, fmt.Errorf("%w: input must be an object", errAnthropicToolInput)
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

// anthropicUserMessage 把 tool_result 拆成独立 tool 消息，其余内容合并为 user。
func anthropicUserMessage(blocks []map[string]any) ([]any, error) {
	out := make([]any, 0, len(blocks))

	var parts []any

	for _, block := range blocks {
		if stringValue(block["type"]) == "tool_result" {
			toolMsg, extra, err := anthropicToolResultMessage(block)
			if err != nil {
				return nil, err
			}

			out = append(out, toolMsg)
			parts = append(parts, extra...)

			continue
		}

		blockParts, err := anthropicUserBlockParts(block)
		if err != nil {
			return nil, err
		}

		parts = append(parts, blockParts...)
	}

	if len(parts) > 0 {
		out = append(out, map[string]any{"role": "user", "content": openAIContent(parts)})
	}

	if len(out) == 0 {
		return nil, errMessageContentRequired
	}

	return out, nil
}

// anthropicUserBlockParts 转换 user 消息中的非 tool_result 内容块。
func anthropicUserBlockParts(block map[string]any) ([]any, error) {
	switch stringValue(block["type"]) {
	case "text":
		return anthropicOptionalPart(textPart(block)), nil
	case "image":
		part, err := anthropicImagePart(block)
		if err != nil {
			return nil, err
		}

		return []any{part}, nil
	case "document":
		return anthropicDocumentParts(block)
	case "search_result":
		if err := validateAnthropicSearchResult(block); err != nil {
			return nil, err
		}

		return anthropicOptionalPart(anthropicSearchResultPart(block)), nil
	case "container_upload":
		part, err := anthropicContainerUploadPart(block)
		if err != nil {
			return nil, err
		}

		return []any{part}, nil
	default:
		return nil, fmt.Errorf("%w %q in user", errAnthropicUnsupportedBlock, stringValue(block["type"]))
	}
}

// anthropicOptionalPart 把可选 part 包成 0/1 个元素的数组。
func anthropicOptionalPart(part map[string]any) []any {
	if part == nil {
		return nil
	}

	return []any{part}
}

// anthropicToolResultMessage 生成一个 tool 消息，并返回需要挂到 user 消息的
// 多模态 part（Chat Completions 的 tool 消息不承载图片/文档）。
func anthropicToolResultMessage(block map[string]any) (map[string]any, []any, error) {
	id := stringValue(block["tool_use_id"])
	if id == "" {
		return nil, nil, errToolResultIDIsRequired
	}

	text, extra, err := anthropicToolResult(block["content"])
	if err != nil {
		return nil, nil, err
	}

	if isError, _ := block["is_error"].(bool); isError {
		if text == "" {
			text = "[tool_error]"
		} else {
			text = "[tool_error] " + text
		}
	}

	toolMsg := map[string]any{
		"role":        "tool",
		keyToolCallID: id,
		"content":     text,
	}
	// chat 形态的 tool 消息没有 content part 数组，缓存断点只能挂在消息对象上
	// （其余块类型都落在 part 上）。
	copyCacheControl(toolMsg, block)
	copyNestedCacheControl(toolMsg, block)

	return toolMsg, extra, nil
}

// anthropicToolResult 拆分 tool_result.content：文本压成 tool 消息，图片与文档
// 转成 user content parts。
func anthropicToolResult(raw any) (string, []any, error) {
	blocks, ok := raw.([]any)
	if !ok {
		return anthropicText(raw), nil, nil
	}

	var texts []string

	extra := make([]any, 0)

	for _, item := range blocks {
		block, _ := item.(map[string]any)
		if block == nil {
			continue
		}

		text, parts, err := anthropicToolResultContent(block)
		if err != nil {
			return "", nil, err
		}

		if text != "" {
			texts = append(texts, text)
		}

		extra = append(extra, parts...)
	}

	return strings.Join(texts, "\n\n"), extra, nil
}

// anthropicToolResultContent 转换 tool_result.content 的单个内容块。
func anthropicToolResultContent(block map[string]any) (string, []any, error) {
	switch stringValue(block["type"]) {
	case "text":
		return anthropicText(block), nil, nil
	case "image":
		image, err := anthropicImagePart(block)
		if err != nil {
			return "", nil, err
		}

		return "", []any{image}, nil
	case "document":
		parts, err := anthropicDocumentParts(block)
		if err != nil {
			return "", nil, err
		}

		return "", parts, nil
	case "search_result":
		if err := validateAnthropicSearchResult(block); err != nil {
			return "", nil, err
		}

		if part := anthropicSearchResultPart(block); part != nil {
			return "", []any{part}, nil
		}

		return "", nil, nil
	default:
		return "", nil, fmt.Errorf(
			"%w: unsupported block %q", errAnthropicToolResult, stringValue(block["type"]),
		)
	}
}

// anthropicImageMediaTypes 官方支持的内联图片 media_type。
var anthropicImageMediaTypes = map[string]bool{
	"image/jpeg": true, "image/png": true, "image/gif": true, "image/webp": true,
}

// anthropicBase64DataURL 校验 base64 source 的必填字段并拼 data URL。
func anthropicBase64DataURL(source map[string]any, errSource error) (string, error) {
	media := stringValue(source["media_type"])
	if media == "" {
		return "", fmt.Errorf("%w: source.media_type is required", errSource)
	}

	data := stringValue(source["data"])
	if data == "" {
		return "", fmt.Errorf("%w: source.data is required", errSource)
	}

	return "data:" + media + ";base64," + data, nil
}

// anthropicImagePart 把 Anthropic image block 转为 OpenAI image_url / file part。
// 缺字段或未知 source.type 明确报错而不是静默丢弃。
func anthropicImagePart(block map[string]any) (map[string]any, error) {
	source, _ := block["source"].(map[string]any)
	if source == nil {
		return nil, fmt.Errorf("%w: source is required", errAnthropicImageSource)
	}

	switch stringValue(source["type"]) {
	case "base64":
		part, err := anthropicImageBase64Part(source)
		if err != nil {
			return nil, err
		}

		copyCacheControl(part, block)

		return part, nil
	case "url":
		url := stringValue(source["url"])
		if url == "" {
			return nil, fmt.Errorf("%w: source.url is required", errAnthropicImageSource)
		}

		part := map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}}
		copyCacheControl(part, block)

		return part, nil
	case "file":
		fileID := stringValue(source["file_id"])
		if fileID == "" {
			return nil, fmt.Errorf("%w: source.file_id is required", errAnthropicImageSource)
		}

		part := map[string]any{"type": "file", "file": map[string]any{"file_id": fileID}}
		copyCacheControl(part, block)

		return part, nil
	default:
		return nil, fmt.Errorf("%w: unsupported type %q", errAnthropicImageSource, stringValue(source["type"]))
	}
}

// anthropicImageBase64Part 生成内联 base64 图片的 image_url part。
// 缺 media_type 与枚举外的 media_type 是两类错误：前者是必填缺失，报
// 「unsupported」会把空串当成客户端给了非法值。
func anthropicImageBase64Part(source map[string]any) (map[string]any, error) {
	media := stringValue(source["media_type"])
	if media == "" {
		return nil, fmt.Errorf("%w: source.media_type is required", errAnthropicImageSource)
	}

	if !anthropicImageMediaTypes[media] {
		return nil, fmt.Errorf("%w: %q", errAnthropicMediaType, media)
	}

	url, err := anthropicBase64DataURL(source, errAnthropicImageSource)
	if err != nil {
		return nil, err
	}

	return map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}}, nil
}

// anthropicDocumentParts 把 document 转为文本或 OpenAI file part。
func anthropicDocumentParts(block map[string]any) ([]any, error) {
	source, _ := block["source"].(map[string]any)
	if source == nil {
		return nil, fmt.Errorf("%w: source is required", errAnthropicDocumentSource)
	}

	parts, err := anthropicDocumentSourceParts(block, source)
	if err != nil {
		return nil, err
	}

	if len(parts) == 0 {
		return nil, fmt.Errorf("%w: document has no content", errAnthropicDocumentSource)
	}

	if last, ok := parts[len(parts)-1].(map[string]any); ok {
		copyCacheControl(last, block)
	}

	return parts, nil
}

// anthropicDocumentSourceParts 按 document.source.type 生成 OpenAI content part。
func anthropicDocumentSourceParts(block, source map[string]any) ([]any, error) {
	switch stringValue(source["type"]) {
	case "text":
		return anthropicDocumentTextPart(source)
	case "content":
		return anthropicDocumentContentParts(source["content"])
	case "base64":
		return anthropicDocumentBase64Part(block, source)
	case "file":
		fileID := stringValue(source["file_id"])
		if fileID == "" {
			return nil, fmt.Errorf("%w: source.file_id is required", errAnthropicDocumentSource)
		}

		return []any{map[string]any{"type": "file", "file": map[string]any{"file_id": fileID}}}, nil
	case "url":
		url := stringValue(source["url"])
		if url == "" {
			return nil, fmt.Errorf("%w: source.url is required", errAnthropicDocumentSource)
		}

		return []any{map[string]any{
			"type": "file",
			"file": map[string]any{
				"filename": anthropicDocumentFilename(block),
				"file_url": url,
			},
		}}, nil
	default:
		return nil, fmt.Errorf("%w: unsupported type %q", errAnthropicDocumentSource, stringValue(source["type"]))
	}
}

// anthropicDocumentTextPart 生成纯文本文档的 text part。
func anthropicDocumentTextPart(source map[string]any) ([]any, error) {
	if media := stringValue(source["media_type"]); media != "" && media != "text/plain" {
		return nil, fmt.Errorf("%w: %q", errAnthropicMediaType, media)
	}

	data, ok := source["data"].(string)
	if !ok {
		return nil, fmt.Errorf("%w: source.data is required", errAnthropicDocumentSource)
	}

	return []any{map[string]any{"type": "text", "text": data}}, nil
}

// anthropicDocumentBase64Part 生成 base64 document 的 OpenAI file part。
func anthropicDocumentBase64Part(block, source map[string]any) ([]any, error) {
	media := stringValue(source["media_type"])
	if media == "" {
		return nil, fmt.Errorf("%w: source.media_type is required", errAnthropicDocumentSource)
	}

	if media != "application/pdf" {
		return nil, fmt.Errorf("%w: %q", errAnthropicMediaType, media)
	}

	url, err := anthropicBase64DataURL(source, errAnthropicDocumentSource)
	if err != nil {
		return nil, err
	}

	return []any{map[string]any{
		"type": "file",
		"file": map[string]any{
			"filename":  anthropicDocumentFilename(block),
			"file_data": url,
		},
	}}, nil
}

// anthropicDocumentContentParts 转换 document.source.type=content 的嵌套内容。
func anthropicDocumentContentParts(raw any) ([]any, error) {
	if text, ok := raw.(string); ok {
		return []any{map[string]any{"type": "text", "text": text}}, nil
	}

	blocks, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("%w: source.content is invalid", errAnthropicDocumentSource)
	}

	parts := make([]any, 0, len(blocks))

	for _, item := range blocks {
		block, _ := item.(map[string]any)
		if block == nil {
			continue
		}

		switch stringValue(block["type"]) {
		case "text":
			if part := textPart(block); part != nil {
				parts = append(parts, part)
			}
		case "image":
			part, err := anthropicImagePart(block)
			if err != nil {
				return nil, err
			}

			parts = append(parts, part)
		default:
			return nil, fmt.Errorf("%w: unsupported content block %q",
				errAnthropicDocumentSource, stringValue(block["type"]))
		}
	}

	return parts, nil
}

// anthropicDocumentFilename 取 document.title 作为文件名，缺省 document.pdf。
func anthropicDocumentFilename(block map[string]any) string {
	if title := strings.TrimSpace(stringValue(block["title"])); title != "" {
		return title
	}

	return "document.pdf"
}

// validateAnthropicSearchResult 校验 search_result 的必填字段。
func validateAnthropicSearchResult(block map[string]any) error {
	if strings.TrimSpace(stringValue(block["title"])) == "" {
		return fmt.Errorf("%w: title is required", errAnthropicSearchResult)
	}

	if strings.TrimSpace(stringValue(block["source"])) == "" {
		return fmt.Errorf("%w: source is required", errAnthropicSearchResult)
	}

	if content, ok := block["content"]; !ok || content == nil {
		return fmt.Errorf("%w: content is required", errAnthropicSearchResult)
	}

	return nil
}

// anthropicSearchResultPart 把 RAG search_result 块压成 text part。
func anthropicSearchResultPart(block map[string]any) map[string]any {
	text := anthropicText(block["content"])
	title := strings.TrimSpace(stringValue(block["title"]))
	source := strings.TrimSpace(stringValue(block["source"]))

	header := title
	if source != "" {
		if header != "" {
			header += " (" + source + ")"
		} else {
			header = source
		}
	}

	body := strings.TrimSpace(header + "\n" + text)

	if body == "" {
		return nil
	}

	part := map[string]any{"type": "text", "text": body}
	copyCacheControl(part, block)
	copyNestedCacheControl(part, block)

	return part
}

// anthropicContainerUploadPart 把 container_upload 转为 OpenAI file part。
func anthropicContainerUploadPart(block map[string]any) (map[string]any, error) {
	fileID := stringValue(block["file_id"])
	if fileID == "" {
		return nil, fmt.Errorf("%w: file_id is required", errAnthropicContainerUpload)
	}

	part := map[string]any{"type": "file", "file": map[string]any{"file_id": fileID}}
	copyCacheControl(part, block)

	return part, nil
}

// copyCacheControl 把 Anthropic 块级 cache_control 复制到 OpenAI part。
// 显式 null 等同于未设置，不写空标记。
func copyCacheControl(part, block map[string]any) {
	if cc, ok := block["cache_control"]; ok && cc != nil {
		part["cache_control"] = cc
	}
}

// copyNestedCacheControl 无块级 cache_control 时，回退到 content 中最后一个
// 带标记的子块，避免 search_result / tool_result 的嵌套断点被吞掉。
func copyNestedCacheControl(part, block map[string]any) {
	if _, ok := part["cache_control"]; ok {
		return
	}

	items, _ := block["content"].([]any)

	for _, raw := range slices.Backward(items) {
		item, _ := raw.(map[string]any)
		if item == nil {
			continue
		}

		if cc, ok := item["cache_control"]; ok && cc != nil {
			part["cache_control"] = cc

			return
		}
	}
}

// hasCacheControl 报告 parts 中是否有 cache_control（有则不压缩成字符串）。
func hasCacheControl(parts []any) bool {
	for _, part := range parts {
		if m, ok := part.(map[string]any); ok {
			if cc, exists := m["cache_control"]; exists && cc != nil {
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

		if cc, ok := m["cache_control"]; ok && cc != nil {
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

// anthropicTools 把 Anthropic tools 转为 OpenAI function tools。客户端自定义
// 工具的 type 可以省略，也可以是 "custom"；服务端工具（web_search/bash/
// code_execution 等）没有客户端执行语义，明确拒绝。
//
//nolint:gocognit // 工具类型/服务端工具/参数转换线性展开。
func anthropicTools(tools []map[string]any) ([]any, error) {
	out := make([]any, 0, len(tools))
	for _, tool := range tools {
		switch toolType := stringValue(tool["type"]); toolType {
		case "", "custom":
		default:
			return nil, fmt.Errorf("%w %q", errAnthropicServerTool, toolType)
		}

		name, err := anthropicToolName(tool)
		if err != nil {
			return nil, err
		}

		fn := map[string]any{"name": name}
		if desc, _ := tool["description"].(string); desc != "" {
			fn["description"] = desc
		}

		if schema, ok := tool["input_schema"]; ok && schema != nil {
			if _, isObject := schema.(map[string]any); !isObject {
				return nil, fmt.Errorf("%w: input_schema must be an object", errAnthropicToolSchema)
			}

			fn["parameters"] = schema
		}

		if strict, ok := tool["strict"].(bool); ok {
			fn["strict"] = strict
		}

		out = append(out, map[string]any{"type": "function", "function": fn})
	}

	return out, nil
}

// anthropicToolName 读取并校验客户端工具名。
func anthropicToolName(tool map[string]any) (string, error) {
	name, _ := tool["name"].(string)
	if name == "" {
		return "", errAnthropicToolNameRequired
	}

	if len(name) > anthropicMaxToolNameLen {
		return "", errToolNameTooLong
	}

	return name, nil
}

// anthropicToolChoice 把 Anthropic tool_choice 映射为 OpenAI tool_choice。
// 未知 type 或缺失 name 的 tool 选择返回错误，避免客户端以为约束已生效。
func anthropicToolChoice(raw json.RawMessage) (any, error) {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		//nolint:nilnil // nil choice 表示客户端未指定 tool_choice。
		return nil, nil
	}

	var choice struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &choice); err != nil {
		return nil, fmt.Errorf("invalid tool_choice: %w", errAnthropicMessageFailed)
	}

	switch choice.Type {
	case "auto":
		return "auto", nil
	case "any":
		return "required", nil
	case "none":
		return "none", nil
	case "tool":
		if choice.Name == "" {
			return nil, errToolChoiceNameRequired
		}

		return map[string]any{"type": "function", "function": map[string]any{"name": choice.Name}}, nil
	default:
		return nil, fmt.Errorf("%w %q", errAnthropicUnsupportedField, "tool_choice.type")
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
// adaptive / between_tools 在上游没有独立形态，统一按启用处理。
func anthropicThinking(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}

	var item struct {
		Type         string `json:"type"`
		BudgetTokens int    `json:"budget_tokens"`
	}
	if json.Unmarshal(raw, &item) != nil {
		return nil
	}

	switch item.Type {
	case "enabled":
		thinking := map[string]any{"type": "enabled"}
		if item.BudgetTokens > 0 {
			thinking["budget_tokens"] = item.BudgetTokens
		}

		return thinking
	case "adaptive", "between_tools":
		// 这两种 type 没有 budget_tokens 语义（自适应由 output_config.effort 控制），
		// 透传客户端附带的 budget 只会让上游按 enabled 的约束拒掉请求。
		return map[string]any{"type": "enabled"}
	case "disabled":
		return map[string]any{"type": "disabled"}
	default:
		return nil
	}
}

// anthropicThinkingOmitted 读取 thinking.display=omitted：上游思维链仍可用于
// 推理，但不得作为 thinking 块回传给客户端。
func anthropicThinkingOmitted(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}

	var item struct {
		Display string `json:"display"`
	}
	if json.Unmarshal(raw, &item) != nil {
		return false
	}

	return item.Display == "omitted"
}

// anthropicResponder 把核心的 OpenAI 形态结果编码为 Anthropic 响应。
type anthropicResponder struct {
	w         http.ResponseWriter
	model     string
	keepalive time.Duration
	options   anthropicResponseOptions
	// prelude 响应前部的服务端工具块（server_tool_use / web_search_tool_result）。
	prelude []any
}

// JSON 写出非流式 Anthropic Message。
func (r *anthropicResponder) JSON(status int, body any) {
	msg := anthropicMessageFromOpenAIWithOptions(body, r.model, r.options)
	if len(r.prelude) > 0 {
		content, _ := msg["content"].([]any)
		msg["content"] = append(append([]any{}, r.prelude...), content...)
	}

	writeJSON(r.w, status, msg)
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
func (r *anthropicResponder) Stream(
	rc io.Reader, hint func(string) string, repair *upstream.MarkupRepair, onErrFrame func(string),
) error {
	anthropicStreamWriter2 := newAnthropicStreamWriterWithOptions(r.w, r.model, r.options)
	anthropicStreamWriter2.prelude = r.prelude

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

// anthropicMessageFromOpenAIWithOptions 按请求选项编码非流式响应。
//
//nolint:funlen // 非流式响应字段密集，平铺便于与协议对照。
func anthropicMessageFromOpenAIWithOptions(
	body any, model string, options anthropicResponseOptions,
) map[string]any {
	obj, _ := body.(map[string]any)

	id, _ := obj["id"].(string)
	if id == "" {
		id = "msg_gateway"
	}

	choice := firstOpenAIChoice(obj)
	content := make([]any, 0, 2)

	var text, finishReason string

	stopReason := "end_turn"

	var stopSequence any

	if choice != nil {
		msg, _ := choice["message"].(map[string]any)
		if reasoning := deltaReasoning(msg); reasoning != "" && !options.omitThinking {
			content = append(content, map[string]any{
				"type": "thinking", "thinking": reasoning, "signature": "",
			})
		}

		text = responsesMessageText(msg["content"])
		if text == "" {
			text = stringValue(msg["refusal"])
		}

		if text != "" {
			content = append(content, map[string]any{"type": "text", "text": text})
		}

		tools := anthropicToolUseBlocks(msg["tool_calls"])
		content = append(content, tools...)
		finishReason = stringValue(choice["finish_reason"])
		stopReason, stopSequence = anthropicStop(finishReason, text, options.stopSequences, len(tools) > 0)
	}

	if len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": ""})
	}

	usage := anthropicUsage(obj["usage"])

	return map[string]any{
		"id":               id,
		"type":             "message",
		"role":             "assistant",
		"model":            model,
		"content":          content,
		stopReasonKey:      stopReason,
		stopReasonSequence: stopSequence,
		"usage":            usage,
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
			if input == nil {
				// arguments="null" 等合法 JSON 非对象值：tool_use.input 必须是对象。
				input = map[string]any{}
			}
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
		keyInputTokens:                input,
		keyOutputTokens:               output,
		"cache_creation_input_tokens": cacheWrite,
		"cache_read_input_tokens":     cached,
	}
	if thinking := responsesReasoningTokens(usage); thinking > 0 {
		usageOut["output_tokens_details"] = map[string]any{"thinking_tokens": thinking}
	}

	return usageOut
}

// anthropicStopReason 映射 OpenAI finish_reason → Anthropic stop_reason。
// 兼容上游直接回传 Anthropic 形态 stop_reason（链式网关/兼容实现）。
func anthropicStopReason(reason string, hasTool bool) string {
	switch reason {
	case "length", "max_tokens":
		return "max_tokens"
	case "tool_calls", typeFunctionCall, "tool_use":
		return "tool_use"
	case statusContentFilter, "refusal":
		return "refusal"
	case stopReasonSequence, "pause_turn", "model_context_window_exceeded", "end_turn":
		return reason
	case "stop":
		if hasTool {
			return "tool_use"
		}

		return "end_turn"
	default:
		if hasTool {
			return "tool_use"
		}

		return "end_turn"
	}
}

// anthropicStop 计算 stop_reason / stop_sequence。上游 finish_reason=stop 不带
// 命中序列，只能在已生成文本里找后缀；找不到时保持 end_turn，不伪造序列。
func anthropicStop(reason, text string, sequences []string, hasTool bool) (string, any) {
	// 上游可能不发 finish_reason；此时若正文以客户端声明的停止序列收尾，
	// 仍按 stop_sequence 返回，不能退化成 end_turn。
	reasonMissing := reason == ""
	if (reasonMissing || reason == "stop" || reason == stopReasonSequence) && !hasTool {
		if sequence, ok := matchedStopSequence(text, sequences); ok {
			return stopReasonSequence, sequence
		}
	}

	if reason == stopReasonSequence {
		// 上游说命中了停止序列，但正文里没有可回传的序列（已被上游剥掉）：
		// 没有配对值就不能保持 stop_sequence，退回 end_turn。
		return anthropicStopReason("stop", hasTool), nil
	}

	return anthropicStopReason(reason, hasTool), nil
}

// matchedStopSequence 返回文本末尾命中的最长停止序列。停止序列只会出现在
// 生成终止处，正文中间的相同片段不应被误报为命中的序列。
func matchedStopSequence(text string, sequences []string) (string, bool) {
	best := ""

	for _, sequence := range sequences {
		if sequence == "" || !strings.HasSuffix(text, sequence) {
			continue
		}

		if len(sequence) > len(best) {
			best = sequence
		}
	}

	return best, best != ""
}

// statusAnthropicOverloaded Anthropic 的非标准重载状态码。
const statusAnthropicOverloaded = 529

// anthropicErrorType 按 HTTP 状态码映射 Anthropic error.type。529 是 Anthropic
// 的非标准重载码，单独保留。
func anthropicErrorType(status int) string {
	if status == statusAnthropicOverloaded {
		return errorTypeOverloaded
	}

	if errorType, ok := anthropicErrorTypes[status]; ok {
		return errorType
	}

	return "api_error"
}

var anthropicErrorTypes = map[int]string{
	http.StatusBadRequest:            errorTypeInvalidReq,
	http.StatusUnprocessableEntity:   errorTypeInvalidReq,
	http.StatusUnauthorized:          "authentication_error",
	http.StatusForbidden:             "permission_error",
	http.StatusNotFound:              "not_found_error",
	http.StatusConflict:              "conflict_error",
	http.StatusRequestEntityTooLarge: "request_too_large",
	http.StatusInternalServerError:   "api_error",
	http.StatusTooManyRequests:       "rate_limit_error",
	http.StatusPaymentRequired:       "billing_error",
	http.StatusRequestTimeout:        "timeout_error",
	http.StatusGatewayTimeout:        "timeout_error",
	http.StatusServiceUnavailable:    errorTypeOverloaded,
	http.StatusBadGateway:            errorTypeOverloaded,
}

// writeAnthropicError 写出 Anthropic 错误信封。错误路径也要保证 Request-Id 可读，
// 否则 SDK 在 404 等未经过 setAnthropicRequestID 的分支拿不到 request-id。
// 官方错误体同时带顶层 request_id，与响应头取值一致。
func writeAnthropicError(w http.ResponseWriter, status int, errType, msg, hint string) {
	requestID := setAnthropicRequestID(w, nil)

	e := map[string]any{"type": errType, "message": msg}
	if hint != "" {
		e["gateway_hint"] = hint
	}

	writeJSON(w, status, map[string]any{"type": "error", "error": e, "request_id": requestID})
}

// anthropicStreamWriter 接收 upstream.StreamHint 写出的 OpenAI SSE 帧，转换为
// Anthropic SSE 事件。实现 http.ResponseWriter + Flusher + Unwrap，使上游写路径
// 的响应头、逐帧 flush 与写截止（issue #53）继续作用于底层连接。
type anthropicStreamWriter struct {
	w       http.ResponseWriter
	fl      http.Flusher
	model   string
	logger  *log.Logger
	options anthropicResponseOptions

	pending         []byte
	started         bool
	errored         bool
	finished        bool
	blockOpen       bool
	blockIndex      int
	blockType       string // text / thinking / tool_use
	toolBlocks      map[int]*anthropicToolBlock
	toolCount       int
	deferred        []anthropicDeferredPart
	stopTail        string // 仅在声明 stop_sequences 时保留正文末尾，用于收尾后缀匹配
	stopTailSize    int    // 需要保留的最大尾部字节数（= 最长停止序列长度）
	finishReason    string
	inputTokens     int
	outputTokens    int
	cachedTokens    int
	cacheWrite      int
	reasoningTokens int
	// prelude 服务端工具块，在 message_start 之后、正文之前完整下发。
	prelude []any
}

func newAnthropicStreamWriterWithOptions(
	w http.ResponseWriter, model string, options anthropicResponseOptions,
) *anthropicStreamWriter {
	fl, _ := w.(http.Flusher)

	return &anthropicStreamWriter{
		w: w, fl: fl, model: model, blockIndex: -1, options: options,
		toolBlocks:   map[int]*anthropicToolBlock{},
		stopTailSize: maxStopSequenceLen(options.stopSequences),
		logger:       log.Default(),
	}
}

// maxStopSequenceLen 返回声明的最长停止序列字节数（无声明为 0）。
// 停止序列只会出现在生成终止处，收尾的后缀匹配只需保留这么长的正文尾巴。
func maxStopSequenceLen(sequences []string) int {
	longest := 0

	for _, sequence := range sequences {
		longest = max(longest, len(sequence))
	}

	return longest
}

// recordText 累积正文尾部（最多保留最长停止序列长度），供收尾后缀匹配。
// 不保留全文：长流的输出可以有几百 KB，而匹配只看末尾。
func (a *anthropicStreamWriter) recordText(text string) {
	if a.stopTailSize == 0 {
		return
	}

	a.stopTail += text
	if len(a.stopTail) > a.stopTailSize {
		a.stopTail = a.stopTail[len(a.stopTail)-a.stopTailSize:]
	}
}

// anthropicDeferredPart 暂存工具调用之后的文本/思考增量，收尾时在 tool_use
// 块之后按原顺序补发，避免 trailing text 被排到工具调用之前。
type anthropicDeferredPart struct {
	deltaType string
	blockType string
	text      string
}

// anthropicToolBlock 缓冲单个 tool call 的 id/name/参数；收尾时按 index 顺序
// 一次性写出，避免并行 tool_calls 交错把同一个调用拆成多个块。
type anthropicToolBlock struct {
	index int
	id    string
	name  string
	args  strings.Builder
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
	return writeSSELines(&a.pending, nums, a.handleLine)
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

	// SSE 的 data 字段允许省略冒号后的空格；这里统一 TrimSpace 后再分派。
	if payload, ok := strings.CutPrefix(line, "data:"); ok {
		payload = strings.TrimSpace(payload)
		if payload == "[DONE]" {
			return a.finish()
		}

		return a.handleChunk(payload)
	}

	return nil
}

// handleChunk 解析 OpenAI chunk 并写出对应 Anthropic 事件。
func (a *anthropicStreamWriter) handleChunk(payload string) error {
	if a.finished {
		return nil
	}

	var chunk map[string]any
	if json.Unmarshal([]byte(payload), &chunk) != nil {
		return nil //nolint:nilerr // 非 JSON 数据帧（上游噪声）直接丢弃
	}

	if errObj, ok := chunk["error"].(map[string]any); ok {
		return a.handleUpstreamError(payload, errObj)
	}

	// 上游透传的 Anthropic ping 帧没有内容语义，不能触发 message_start。
	if stringValue(chunk["type"]) == "ping" {
		return nil
	}

	// 上游透传层会把帧重建成 OpenAI 白名单形态（丢弃 type、缺 id 时补占位），
	// 所以「这一帧是否承载内容」只能按有没有 choices 判定：无 choices 的帧
	// （ping、usage-only、上游透传的非内容帧）不产生内容，不能触发 message_start，
	// 否则占位 id 会顶掉后续帧里的真实 id。usage 仍要记录。
	a.captureUsage(chunk)

	choice := firstOpenAIChoice(chunk)
	if choice == nil {
		return nil
	}

	if err := a.ensureStart(chunk); err != nil {
		return err
	}

	if delta, _ := choice["delta"].(map[string]any); delta != nil {
		if err := a.applyDelta(delta); err != nil {
			return err
		}
	}

	if reason := stringValue(choice["finish_reason"]); reason != "" {
		return a.flushAtFinishReason(reason)
	}

	return nil
}

// flushAtFinishReason 记录 finish_reason 并立刻落地缓冲的工具块与暂存正文。
// finish_reason 之后不会再有参数分片：零参工具（arguments 为空）只能靠这里
// 判定批次结束；上游截断、没有 [DONE] 时也不会让已生成内容随 error 事件丢失。
func (a *anthropicStreamWriter) flushAtFinishReason(reason string) error {
	a.finishReason = reason

	if err := a.flushToolBlocks(); err != nil {
		return err
	}

	if err := a.flushDeferred(); err != nil {
		return err
	}

	return a.closeBlock()
}

// handleUpstreamError 把上游 error 帧转成 Anthropic error 事件并标记流已失败。
func (a *anthropicStreamWriter) handleUpstreamError(payload string, errObj map[string]any) error {
	msg, _ := errObj["message"].(string)
	if msg == "" {
		msg = "upstream stream error"
	}

	// 先把已生成内容落地：error 事件之后流即终止，未刷新的 tool_use 与暂存
	// 正文都补不回来。截断的参数只记 WARN，不改写分片。
	if err := a.flushBeforeError(); err != nil {
		return err
	}

	a.errored = true
	// error 事件之后流已终止：后续上游帧（含 [DONE] 前的残留）一律忽略。
	a.finished = true

	errType := anthropicStreamErrorType(payload, errObj)
	// 空流 / 截断 / SSE 超长等网关本地兜底帧（type=upstream_error）不是客户端
	// 请求的问题，统一按 api_error 输出。
	if stringValue(errObj["type"]) == "upstream_error" {
		errType = "api_error"
	}

	errBody := map[string]any{"type": errType, "message": msg}
	// gateway_hint 由上游流层附在 error 帧上；流式错误不能把它丢掉。
	if hint, _ := errObj["gateway_hint"].(string); hint != "" {
		errBody["gateway_hint"] = hint
	}

	return a.event("error", map[string]any{"type": "error", "error": errBody})
}

// applyDelta 处理一个 choice delta 的文本、思考与工具调用增量。
func (a *anthropicStreamWriter) applyDelta(delta map[string]any) error {
	// thinking 先于正文：同一 chunk 同时带两者时，保持 Anthropic 的块顺序。
	if reasoning := deltaReasoning(delta); reasoning != "" && !a.options.omitThinking {
		if err := a.emitOrDefer(reasoning, "thinking_delta", "thinking"); err != nil {
			return err
		}
	}

	text, _ := delta["content"].(string)
	if text == "" {
		text, _ = delta["refusal"].(string)
	}

	if text != "" {
		a.recordText(text)

		if err := a.emitOrDefer(text, "text_delta", "text"); err != nil {
			return err
		}
	}

	return a.bufferToolCalls(delta["tool_calls"])
}

// emitOrDefer 工具调用缓冲期间先暂存文本/思考，收尾时在 tool_use 之后补发。
// 参数已完整的批次在正文恢复时立即落地（先工具块、再暂存内容、最后本片）：
// 否则工具调用之后的长尾正文要一直压到流尾才发给客户端。
// 没有待发工具块（上一批已写出）时直接落块：此时正文一定排在已写出的
// tool_use 之后，暂存只会把整段尾文压到流尾并占住内存。
func (a *anthropicStreamWriter) emitOrDefer(text, deltaType, blockType string) error {
	if a.toolBatchComplete() {
		if err := a.flushToolBlocks(); err != nil {
			return err
		}

		if err := a.flushDeferred(); err != nil {
			return err
		}
	}

	if len(a.toolBlocks) > 0 {
		a.deferred = append(a.deferred, anthropicDeferredPart{
			deltaType: deltaType, blockType: blockType, text: text,
		})

		return nil
	}

	return a.emitText(text, deltaType, blockType)
}

// deltaReasoning 读取上游思维链增量（reasoning_content 优先，reasoning 兜底）。
func deltaReasoning(delta map[string]any) string {
	if reasoning, _ := delta["reasoning_content"].(string); reasoning != "" {
		return reasoning
	}

	reasoning, _ := delta["reasoning"].(string)

	return reasoning
}

// ensureStart 在首个内容帧写出 message_start（usage 由调用方先行 captureUsage，
// 缺失为 0）。上游要到末帧才给全量 usage，这里的 input_tokens 只是快照，
// 最终值以 message_delta 为准。
func (a *anthropicStreamWriter) ensureStart(chunk map[string]any) error {
	if a.started {
		return nil
	}

	a.started = true

	id := "msg_gateway"

	if chunk != nil {
		if v, _ := chunk["id"].(string); v != "" {
			id = v
		}
	}

	if err := a.event("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":               id,
			"type":             "message",
			"role":             "assistant",
			"model":            a.model,
			"content":          []any{},
			stopReasonKey:      nil,
			stopReasonSequence: nil,
			"usage": map[string]any{
				keyInputTokens:                a.netInputTokens(),
				keyOutputTokens:               0,
				"cache_creation_input_tokens": a.cacheWrite,
				"cache_read_input_tokens":     a.cachedTokens,
			},
		},
	}); err != nil {
		return err
	}

	return a.emitPrelude()
}

// emitPrelude 完整下发服务端工具块（start 携带完整 content，随后 stop）。
func (a *anthropicStreamWriter) emitPrelude() error {
	for _, block := range a.prelude {
		a.blockIndex++

		if err := a.event("content_block_start", map[string]any{
			"type": "content_block_start", "index": a.blockIndex, "content_block": block,
		}); err != nil {
			return err
		}

		if err := a.event("content_block_stop", map[string]any{
			"type": "content_block_stop", "index": a.blockIndex,
		}); err != nil {
			return err
		}
	}

	return nil
}

// captureUsage 记录 chunk.usage 的 token 数（末帧通常带 usage）。
func (a *anthropicStreamWriter) captureUsage(chunk map[string]any) {
	usage, _ := chunk["usage"].(map[string]any)
	if usage == nil {
		return
	}

	// 与 responses 流式同口径：只接受正向值，避免上游尾帧的显式 0 覆盖已
	// 收到的累计值。
	if n := intValue(usage["prompt_tokens"]); n > 0 {
		a.inputTokens = n
	}

	if n := intValue(usage["completion_tokens"]); n > 0 {
		a.outputTokens = n
	}

	// 两个计数独立更新：末帧可能只补其中一项，不能把另一项覆盖回 0。
	if cached, cacheWrite := responsesCachedTokens(usage); cached > 0 || cacheWrite > 0 {
		if cached > 0 {
			a.cachedTokens = cached
		}

		if cacheWrite > 0 {
			a.cacheWrite = cacheWrite
		}
	}

	if n := responsesReasoningTokens(usage); n > 0 {
		a.reasoningTokens = n
	}
}

// netInputTokens 与非流式 usage 同口径：input_tokens 不含缓存命中/写入。
func (a *anthropicStreamWriter) netInputTokens() int {
	return max(a.inputTokens-a.cachedTokens-a.cacheWrite, 0)
}

// anthropicFrameErrorTypes 上游错误帧自带 Anthropic error.type 时可原样采信的
// 取值（链式网关与 Anthropic 兼容实现会回传这些字面量）。网关本地兜底帧用
// upstream_error，不在此表内。
var anthropicFrameErrorTypes = map[string]bool{
	errorTypeInvalidReq:    true,
	"authentication_error": true,
	"permission_error":     true,
	"not_found_error":      true,
	"conflict_error":       true,
	"request_too_large":    true,
	"rate_limit_error":     true,
	"billing_error":        true,
	"timeout_error":        true,
	errorTypeOverloaded:    true,
	"api_error":            true,
}

// anthropicStreamErrorType 把上游错误帧映射为 Anthropic error.type。
// 帧内已带 Anthropic error.type 时优先采信（兼容链式网关）；否则按 FrameKind
// 分类。FrameKind 对判不出的帧回落 Classify(400, message) → ErrClient，这既
// 不是「客户端请求有错」的证据（可能是网关转换或上游故障），也会把 overloaded
// 一类瞬时故障报成不可重试的参数错误，故 ErrNone/ErrClient 一律回落 api_error。
func anthropicStreamErrorType(payload string, errObj map[string]any) string {
	if errType := stringValue(errObj["type"]); anthropicFrameErrorTypes[errType] {
		return errType
	}

	switch upstream.FrameKind(payload) {
	case upstream.ErrSoftRate, upstream.ErrQueueWaiting:
		return "rate_limit_error"
	case upstream.ErrQueueFull, upstream.ErrServer:
		return errorTypeOverloaded
	case upstream.ErrHardCredit:
		return "billing_error"
	case upstream.ErrSessionDead, upstream.ErrAccountFault:
		return "authentication_error"
	case upstream.ErrNotFound, upstream.ErrModelNotFound:
		return "not_found_error"
	case upstream.ErrBadParams, upstream.ErrPromptTooLong, upstream.ErrContentBlocked,
		upstream.ErrImageInvalid, upstream.ErrModelBlocked, upstream.ErrModelAccess,
		upstream.ErrWafBlock, upstream.ErrChannelRefused:
		return errorTypeInvalidReq
	default:
		return "api_error"
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

// bufferToolCalls 缓冲 OpenAI delta.tool_calls，不在分片到达时直接写 content block。
// 并行工具调用可能交错（index 0/1/0/1），按 index 聚合到参数完整后才整块输出
// （文本边界或收尾），才能保证每个调用对应一个完整 tool_use 块。
func (a *anthropicStreamWriter) bufferToolCalls(raw any) error {
	calls := mapItems(raw)
	if len(calls) == 0 {
		return nil
	}

	// 工具批次开始前先关掉在开的正文块。每片都调（closeBlock 幂等）：暂存策略
	// 允许「无待发工具块时」直接写正文，那种情况下正文块会一直开着。
	if err := a.closeBlock(); err != nil {
		return err
	}

	// 新工具调用且中间出现过文本：先把上一批工具块与暂存文本按原顺序写出，
	// 避免 tool -> text -> tool 的文本被排到所有工具调用之后。
	if a.shouldFlushToolBoundary(calls) {
		if err := a.flushToolBoundary(); err != nil {
			return err
		}
	}

	for _, call := range calls {
		idx := intValue(call["index"])

		block, exists := a.toolBlocks[idx]
		if !exists {
			block = &anthropicToolBlock{}
			a.toolBlocks[idx] = block
		}

		name, id := toolCallIdentity(call)
		if block.name == "" {
			block.name = name
		}

		if block.id == "" {
			block.id = id
		}

		block.args.WriteString(toolCallArguments(call))
	}

	return nil
}

// shouldFlushToolBoundary 判断本帧是否开启全新的一批工具调用，且中间已有暂存文本。
func (a *anthropicStreamWriter) shouldFlushToolBoundary(calls []map[string]any) bool {
	if len(a.deferred) == 0 {
		return false
	}

	for _, call := range calls {
		if _, exists := a.toolBlocks[intValue(call["index"])]; exists {
			return false
		}
	}

	return a.toolBatchComplete()
}

// toolBatchComplete 报告缓冲中的工具块是否都已完整，可以整块写出。
// 参数为空说明后续分片还会到（首片常只带 name），此时刷新会让该 index 的
// 后续分片落到没有 name 的新块里被丢弃。
// 代价：无参工具（上游只发 arguments:""）永远判不完整，其后的正文会一直暂存
// 到 finish_reason / [DONE] 才发出，且 deferred 无上限、后续工具调用的正文可能
// 排到前一批文本之前——这是「参数可能晚到」与「无参调用」无法区分时的保守取舍。
// 不能用「收到正文就判批次结束」来收敛：并行调用的首片可能只带 name，参数仍在
// 后续分片里（见 TestAnthropicSDKToolBatchBoundaryKeepsArgs）。
func (a *anthropicStreamWriter) toolBatchComplete() bool {
	if len(a.toolBlocks) == 0 {
		return false
	}

	for _, block := range a.toolBlocks {
		args := strings.TrimSpace(block.args.String())
		if block.name == "" || args == "" || !json.Valid([]byte(args)) {
			return false
		}
	}

	return true
}

// flushToolBoundary 在工具批次边界写出当前工具块与暂存文本。
func (a *anthropicStreamWriter) flushToolBoundary() error {
	if err := a.flushToolBlocks(); err != nil {
		return err
	}

	return a.flushDeferred()
}

// indexedToolBlocks 按 OpenAI tool index 返回缓冲块（收尾输出顺序）。
func (a *anthropicStreamWriter) indexedToolBlocks() []*anthropicToolBlock {
	indexes := make([]int, 0, len(a.toolBlocks))
	for idx := range a.toolBlocks {
		indexes = append(indexes, idx)
	}

	slices.Sort(indexes)

	blocks := make([]*anthropicToolBlock, 0, len(indexes))
	for _, idx := range indexes {
		blocks = append(blocks, a.toolBlocks[idx])
	}

	return blocks
}

// flushToolBlocks 按 index 顺序写出当前缓冲的 tool_use 块并清空缓冲；
// 可被文本边界或收尾多次调用。
//
//nolint:funlen,gocognit // 工具块收尾按状态顺序写出。
func (a *anthropicStreamWriter) flushToolBlocks() error {
	// 调用方遍布正文边界、finish_reason、错误与收尾：先关掉可能还在开的
	// text/thinking 块，否则工具块会在旧块未关闭时开新 index，旧块永远拿不到
	// content_block_stop，SSE 的块记账就错了。closeBlock 幂等。
	if err := a.closeBlock(); err != nil {
		return err
	}

	blocks := a.indexedToolBlocks()
	emitted := 0

	for _, block := range blocks {
		a.validateToolBlock(block)

		if block.name == "" {
			a.logger.Printf("WARN: [anthropic] tool call without name model=%s id=%s",
				logfmt.Truncate(a.model, 64), logfmt.Truncate(block.id, 64))

			continue
		}

		if block.id == "" {
			block.id = fmt.Sprintf("toolu_%d", a.toolCount+emitted)
		}

		a.blockIndex++
		block.index = a.blockIndex
		a.blockOpen = true
		a.blockType = "tool_use"

		if err := a.toolBlockStart(block); err != nil {
			return err
		}

		if args := a.toolDeltaArgs(block); args != "" {
			if err := a.event("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": block.index,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
			}); err != nil {
				return err
			}
		}

		if err := a.closeBlock(); err != nil {
			return err
		}

		emitted++
	}

	clear(a.toolBlocks)
	a.toolCount += emitted

	return nil
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

	// Anthropic thinking 块在 stop 前会有一条 signature_delta；上游不给签名，
	// 用空串补齐事件结构，客户端累积行为与 start 里的空 signature 一致。
	if a.blockType == "thinking" {
		if err := a.event("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": a.blockIndex,
			"delta": map[string]any{"type": "signature_delta", "signature": ""},
		}); err != nil {
			return err
		}
	}

	return a.event("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": a.blockIndex,
	})
}

// toolDeltaArgs 返回 input_json_delta 要下发的参数文本。
//
// 三种情形分开处理：空/纯空白 → 不下发，客户端沿用 content_block_start 里的空
// 对象 input（发 " " 会让 SDK 的 json.loads 直接抛错）；合法 JSON 但不是对象
// （null / [] / 数字等）→ 归一成 "{}"，与非流式路径 anthropicToolUseBlocks 同口径，
// 否则客户端 SDK 解析出的 tool_use.input 会是 null 而不是对象；非法 JSON（上游
// 截断）原样下发，只在收尾记 WARN（不改写分片）。
func (a *anthropicStreamWriter) toolDeltaArgs(block *anthropicToolBlock) string {
	args := block.args.String()

	trimmed := strings.TrimSpace(args)
	if trimmed == "" {
		return ""
	}

	if json.Valid([]byte(trimmed)) && !strings.HasPrefix(trimmed, "{") {
		a.logger.Printf("WARN: [anthropic] non-object tool arguments model=%s tool=%s id=%s bytes=%d",
			logfmt.Truncate(a.model, 64), logfmt.Truncate(block.name, 64),
			logfmt.Truncate(block.id, 64), len(trimmed))

		return "{}"
	}

	return args
}

// validateToolBlock 校验单个工具块聚合后的参数是否为合法 JSON。
// 本期只观测，不改写分片：上游截断会在这里留下可检索的 WARN（不含参数原文）。
func (a *anthropicStreamWriter) validateToolBlock(block *anthropicToolBlock) {
	args := strings.TrimSpace(block.args.String())
	if args == "" || json.Valid([]byte(args)) {
		return
	}

	a.logger.Printf("WARN: [anthropic] invalid tool arguments model=%s tool=%s id=%s bytes=%d",
		logfmt.Truncate(a.model, 64), logfmt.Truncate(block.name, 64), logfmt.Truncate(block.id, 64), len(args))
}

// usagePayload 组装 message_delta 的累计 usage。
func (a *anthropicStreamWriter) usagePayload() map[string]any {
	usage := map[string]any{
		keyInputTokens:                a.netInputTokens(),
		keyOutputTokens:               a.outputTokens,
		"cache_creation_input_tokens": a.cacheWrite,
		"cache_read_input_tokens":     a.cachedTokens,
	}
	if a.reasoningTokens > 0 {
		usage["output_tokens_details"] = map[string]any{"thinking_tokens": a.reasoningTokens}
	}

	return usage
}

// flushDeferred 在 tool_use 块之后补发暂存的文本/思考。不关块：后续正文
// 若同类型会续写同一个 text/thinking 块，避免拆成相邻两块；终态出口
// （finish / 错误 / finish_reason）各自负责 closeBlock。
func (a *anthropicStreamWriter) flushDeferred() error {
	for _, part := range a.deferred {
		if err := a.emitText(part.text, part.deltaType, part.blockType); err != nil {
			return err
		}
	}

	a.deferred = nil

	return nil
}

// flushBeforeError 在写 error 事件前落地缓冲的工具块与暂存正文。
func (a *anthropicStreamWriter) flushBeforeError() error {
	if err := a.flushToolBlocks(); err != nil {
		return err
	}

	if err := a.flushDeferred(); err != nil {
		return err
	}

	return a.closeBlock()
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

	if err := a.flushToolBlocks(); err != nil {
		return err
	}

	if err := a.flushDeferred(); err != nil {
		return err
	}

	if err := a.closeBlock(); err != nil {
		return err
	}

	reason, stopSequence := anthropicStop(
		a.finishReason, a.stopTail, a.options.stopSequences, a.toolCount > 0,
	)

	if err := a.event("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{stopReasonKey: reason, stopReasonSequence: stopSequence},
		"usage": a.usagePayload(),
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
