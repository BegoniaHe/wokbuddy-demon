package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// parseAnthropicSSE 提取 data 帧为 JSON 对象，便于断言事件序列。
func parseAnthropicSSE(t *testing.T, body string) []map[string]any {
	t.Helper()

	events := make([]map[string]any, 0)

	for line := range strings.SplitSeq(body, "\n") {
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}

		var event map[string]any
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			t.Fatalf("bad SSE data %q: %v", payload, err)
		}

		events = append(events, event)
	}

	return events
}

// TestAnthropicSDKParallelToolCalls 交错 tool_calls 分片必须收尾为两个完整块。
func TestAnthropicSDKParallelToolCalls(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	writer := newAnthropicStreamWriter(rec, "m")

	chunks := []string{
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"A","function":{"name":"a","arguments":"{\"x\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"B","function":{"name":"b","arguments":"{\"y\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"2}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`[DONE]`,
	}
	for _, chunk := range chunks {
		if _, err := writer.Write([]byte("data: " + chunk + "\n\n")); err != nil {
			t.Fatal(err)
		}
	}

	events := parseAnthropicSSE(t, rec.Body.String())
	starts, args := collectAnthropicToolBlocks(events)

	if len(starts) != 2 {
		t.Fatalf("tool_use blocks=%d want 2: %s", len(starts), rec.Body.String())
	}

	if starts[0]["id"] != "A" || starts[1]["id"] != "B" {
		t.Fatalf("tool ids=%v/%v", starts[0]["id"], starts[1]["id"])
	}

	for index, want := range map[float64]string{0: `{"x":1}`, 1: `{"y":2}`} {
		if args[index] != want {
			t.Errorf("index %v args=%q want %q", index, args[index], want)
		}
	}

	last := events[len(events)-1]
	if last["type"] != "message_stop" {
		t.Fatalf("last event=%v", last)
	}
}

// collectAnthropicToolBlocks 汇总流中的 tool_use 起始块与各 index 的完整参数。
func collectAnthropicToolBlocks(events []map[string]any) ([]map[string]any, map[float64]string) {
	starts := make([]map[string]any, 0)

	args := map[float64]string{}

	for _, event := range events {
		switch event["type"] {
		case "content_block_start":
			block, _ := event["content_block"].(map[string]any)
			if block["type"] == "tool_use" {
				starts = append(starts, block)
			}
		case "content_block_delta":
			delta, _ := event["delta"].(map[string]any)
			if delta["type"] == "input_json_delta" {
				index, _ := event["index"].(float64)
				args[index] += stringValue(delta["partial_json"])
			}
		}
	}

	return starts, args
}

// TestAnthropicSDKToolNameArrivesLate 首个分片没有 name 时先缓存参数，不能丢。
func TestAnthropicSDKToolNameArrivesLate(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	writer := newAnthropicStreamWriter(rec, "m")

	chunks := []string{
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"A","function":{"arguments":"{\"x\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"a","arguments":"1}"}}]}}]}`,
		`[DONE]`,
	}
	for _, chunk := range chunks {
		if _, err := writer.Write([]byte("data: " + chunk + "\n\n")); err != nil {
			t.Fatal(err)
		}
	}

	if !strings.Contains(rec.Body.String(), `"partial_json":"{\"x\":1}"`) {
		t.Fatalf("late name lost args:\n%s", rec.Body.String())
	}
}

// TestAnthropicSDKServerToolRejected 服务端工具没有客户端执行语义，必须显式拒绝。
func TestAnthropicSDKServerToolRejected(t *testing.T) {
	t.Parallel()

	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":3}]}`)

	if _, _, _, err := anthropicToOpenAI(body); err == nil ||
		!strings.Contains(err.Error(), "unsupported server tool type") {
		t.Fatalf("err=%v want unsupported server tool", err)
	}
}

// TestAnthropicSDKContentBlocks 合法内容块要映射或明确报错，不能静默丢弃。
func TestAnthropicSDKContentBlocks(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"model":"m","cache_control":{"type":"ephemeral"},
		"messages":[
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"plan"},
				{"type":"text","text":"ok"},
				{"type":"tool_use","id":"t1","name":"lookup","input":{"q":"x"}}
			]},
			{"role":"user","content":[
				{"type":"document","source":{"type":"text","media_type":"text/plain","data":"DOC"}},
				{"type":"search_result","title":"T","source":"S","content":[{"type":"text","text":"BODY"}]},
				{"type":"container_upload","file_id":"f1"},
				{"type":"tool_result","tool_use_id":"t1","is_error":true,"content":[{"type":"text","text":"boom"}]}
			]}
		]}`)

	raw, _, _, err := anthropicToOpenAI(body)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	if _, ok := got["cache_control"]; !ok {
		t.Fatal("top-level cache_control dropped")
	}

	msgs, _ := got["messages"].([]any)

	assistant := msgs[0].(map[string]any)
	if assistant["reasoning_content"] != "plan" {
		t.Fatalf("assistant reasoning=%v", assistant["reasoning_content"])
	}

	user := msgs[2].(map[string]any)
	parts := user["content"].([]any)

	joined, _ := json.Marshal(parts)
	for _, want := range []string{"DOC", "BODY", "T (S)", "f1"} {
		if !strings.Contains(string(joined), want) {
			t.Errorf("user parts missing %q: %s", want, joined)
		}
	}

	tool := msgs[1].(map[string]any)
	if tool["role"] != "tool" || !strings.Contains(stringValue(tool["content"]), "[tool_error] boom") {
		t.Fatalf("tool message=%v", tool)
	}
}

// TestAnthropicSDKThinkingOmitted adaptive 映射为启用，display=omitted 不回传思维链。
func TestAnthropicSDKThinkingOmitted(t *testing.T) {
	t.Parallel()

	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],
		"thinking":{"type":"adaptive","display":"omitted"}}`)

	converted, err := anthropicConvert(body)
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := json.Unmarshal(converted.openAI, &got); err != nil {
		t.Fatal(err)
	}

	thinking, _ := got["thinking"].(map[string]any)
	if thinking["type"] != "enabled" {
		t.Fatalf("thinking=%v", got["thinking"])
	}

	if !converted.options.omitThinking {
		t.Fatal("display=omitted not recorded")
	}

	msg := anthropicMessageFromOpenAIWithOptions(map[string]any{
		"choices": []any{map[string]any{
			"finish_reason": "stop",
			"message":       map[string]any{"reasoning_content": "secret", "content": "answer"},
		}},
	}, "m", converted.options)
	for _, block := range msg["content"].([]any) {
		if block.(map[string]any)["type"] == "thinking" {
			t.Fatalf("omitted thinking leaked: %v", msg["content"])
		}
	}
}

// TestAnthropicSDKMaxTokensZero max_tokens=0 是缓存预热语义，不能被省略。
func TestAnthropicSDKMaxTokensZero(t *testing.T) {
	t.Parallel()

	body := []byte(`{"model":"m","max_tokens":0,"messages":[{"role":"user","content":"hi"}]}`)

	raw, _, _, err := anthropicToOpenAI(body)
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	if got["max_tokens"] != float64(0) {
		t.Fatalf("max_tokens=%v want 0", got["max_tokens"])
	}
}

// TestAnthropicSDKSystemRoleMerged messages 中的 system role 合入顶层 system。
func TestAnthropicSDKSystemRoleMerged(t *testing.T) {
	t.Parallel()

	body := []byte(`{"model":"m","system":"top","messages":[
		{"role":"system","content":"mid"},
		{"role":"user","content":"hi"}
	]}`)

	raw, _, _, err := anthropicToOpenAI(body)
	if err != nil {
		t.Fatal(err)
	}

	var got struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	if len(got.Messages) != 2 || got.Messages[0]["role"] != "system" ||
		got.Messages[0]["content"] != "top\n\nmid" {
		t.Fatalf("messages=%v", got.Messages)
	}
}

// TestAnthropicSDKStopSequence 命中停止序列时返回 stop_sequence。
func TestAnthropicSDKStopSequence(t *testing.T) {
	t.Parallel()

	msg := anthropicMessageFromOpenAIWithOptions(map[string]any{
		"choices": []any{map[string]any{
			"finish_reason": "stop",
			"message":       map[string]any{"content": "answer END"},
		}},
	}, "m", anthropicResponseOptions{stopSequences: []string{"END"}})

	if msg["stop_reason"] != "stop_sequence" || msg["stop_sequence"] != "END" {
		t.Fatalf("stop=%v/%v", msg["stop_reason"], msg["stop_sequence"])
	}

	// 上游回传 stop_sequence 但正文里没有可回传的序列（已被上游剥掉）：
	// 不能输出 stop_reason=stop_sequence + stop_sequence=null。
	msg = anthropicMessageFromOpenAIWithOptions(map[string]any{
		"choices": []any{map[string]any{
			"finish_reason": "stop_sequence",
			"message":       map[string]any{"content": "answer"},
		}},
	}, "m", anthropicResponseOptions{stopSequences: []string{"END"}})

	if msg["stop_reason"] != "end_turn" || msg["stop_sequence"] != nil {
		t.Fatalf("unpaired stop=%v/%v", msg["stop_reason"], msg["stop_sequence"])
	}

	// 正文保留了序列时仍按 stop_sequence 配对。
	msg = anthropicMessageFromOpenAIWithOptions(map[string]any{
		"choices": []any{map[string]any{
			"finish_reason": "stop_sequence",
			"message":       map[string]any{"content": "answer END"},
		}},
	}, "m", anthropicResponseOptions{stopSequences: []string{"END"}})

	if msg["stop_reason"] != "stop_sequence" || msg["stop_sequence"] != "END" {
		t.Fatalf("paired stop=%v/%v", msg["stop_reason"], msg["stop_sequence"])
	}
}

// TestAnthropicSDKStreamErrorType 上游 6004 必须映射 rate_limit_error。
func TestAnthropicSDKStreamErrorType(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()

	writer := newAnthropicStreamWriter(rec, "m")
	if err := writer.handleChunk(`{"error":{"message":"upstream rate limited","code":6004}}`); err != nil {
		t.Fatal(err)
	}

	if err := writer.handleChunk(`{"choices":[{"delta":{"content":"leak"}}]}`); err != nil {
		t.Fatal(err)
	}

	out := rec.Body.String()
	if !strings.Contains(out, `"type":"rate_limit_error"`) {
		t.Fatalf("stream error type=\n%s", out)
	}

	if strings.Contains(out, "leak") {
		t.Fatalf("error 后的残留帧泄漏到客户端:\n%s", out)
	}
}

// TestAnthropicSDKStreamErrorTypePassthrough 帧内自带 Anthropic error.type 时
// 必须采信，不能被 FrameKind 的 400 文案兜底降级成 invalid_request_error。
func TestAnthropicSDKStreamErrorTypePassthrough(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		`{"type":"error","error":{"message":"Overloaded","type":"overloaded_error"}}`: "overloaded_error",
		`{"error":{"message":"invalid x-api-key","type":"authentication_error"}}`:     "authentication_error",
		`{"error":{"message":"requests exceeded","type":"rate_limit_error"}}`:         "rate_limit_error",
	}
	for payload, want := range cases {
		rec := httptest.NewRecorder()
		writer := newAnthropicStreamWriter(rec, "m")

		if err := writer.handleChunk(payload); err != nil {
			t.Fatal(err)
		}

		if out := rec.Body.String(); !strings.Contains(out, `"type":"`+want+`"`) {
			t.Errorf("payload=%s want type %s:\n%s", payload, want, out)
		}
	}
}

// TestAnthropicSDKUnclassifiedStreamError 判不出类别的上游错误帧回落 api_error：
// FrameKind 对判不出的帧一律给 ErrClient，直接映射成 invalid_request_error 会让
// 瞬时上游故障被客户端当成不可重试的参数错误。
func TestAnthropicSDKUnclassifiedStreamError(t *testing.T) {
	t.Parallel()

	messages := []string{
		"Overloaded",
		"internal error",
		"system busy, please try again",
		"请求失败，请稍后重试",
	}
	for _, message := range messages {
		payload := `{"error":{"message":"` + message + `"}}`
		if got := anthropicStreamErrorType(payload, nil); got != "api_error" {
			t.Errorf("message=%s type=%s want api_error", message, got)
		}
	}
}

// TestAnthropicSDKErrorTypes 锁定状态码 → error.type 映射。
// 说明：413 `request_too_large`、409 `conflict_error` 不在 anthropic-sdk-python
// 1.17.0 的 ErrorType 字面量内（413 与官方文档一致，409 是网关防御性取值），
// SDK 只按 HTTP 状态码选异常类，error.type 仅作可读信息，故这里不按字面量收口。
func TestAnthropicSDKErrorTypes(t *testing.T) {
	t.Parallel()

	cases := map[int]string{
		http.StatusRequestEntityTooLarge: "request_too_large",
		http.StatusRequestTimeout:        "timeout_error",
		http.StatusGatewayTimeout:        "timeout_error",
		529:                              "overloaded_error",
		http.StatusPaymentRequired:       "billing_error",
		http.StatusConflict:              "conflict_error",
		http.StatusInternalServerError:   "api_error",
	}
	for status, want := range cases {
		if got := anthropicErrorType(status); got != want {
			t.Errorf("anthropicErrorType(%d)=%s want %s", status, got, want)
		}
	}
}

// TestAnthropicSDKUsageConsistent message_start 与 message_delta 使用同一净额口径。
func TestAnthropicSDKUsageConsistent(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	writer := newAnthropicStreamWriter(rec, "m")

	chunks := []string{
		`{"id":"c","usage":{"prompt_tokens":100,"prompt_tokens_details":{"cached_tokens":40}},"choices":[{"delta":{"content":"x"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":40}}}`,
		`[DONE]`,
	}
	for _, chunk := range chunks {
		if _, err := writer.Write([]byte("data: " + chunk + "\n\n")); err != nil {
			t.Fatal(err)
		}
	}

	var startInput, deltaInput, startCached float64

	for _, event := range parseAnthropicSSE(t, rec.Body.String()) {
		switch event["type"] {
		case "message_start":
			message := event["message"].(map[string]any)
			usage := message["usage"].(map[string]any)
			startInput, _ = usage["input_tokens"].(float64)
			startCached, _ = usage["cache_read_input_tokens"].(float64)
		case "message_delta":
			usage := event["usage"].(map[string]any)
			deltaInput, _ = usage["input_tokens"].(float64)
		}
	}

	if startInput != 60 || deltaInput != 60 {
		t.Fatalf("message_start=%v message_delta=%v want 60/60", startInput, deltaInput)
	}

	if startCached != 40 {
		t.Fatalf("message_start cache_read=%v want 40", startCached)
	}
}

// TestAnthropicSDKCountTokensValidation 缺 messages 必须 400，估算排除 model 与 base64。
func TestAnthropicSDKCountTokensValidation(t *testing.T) {
	t.Parallel()

	h := NewHandler(&Config{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens",
		strings.NewReader(`{"model":"glm-5.2"}`)))

	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "messages is required") {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}

	withModel := countAnthropicPromptBytes(map[string]any{
		"model": "very-long-model-name",
		"messages": []any{map[string]any{
			"role": "user", "content": "hi",
		}},
	})

	withoutModel := countAnthropicPromptBytes(map[string]any{
		"messages": []any{map[string]any{
			"role": "user", "content": "hi",
		}},
	})
	if withModel != withoutModel {
		t.Fatalf("model bytes counted: %d vs %d", withModel, withoutModel)
	}

	imageBody := countAnthropicPromptBytes(map[string]any{
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{map[string]any{
				"type": "image",
				"source": map[string]any{
					"type": "base64", "media_type": "image/png", "data": strings.Repeat("A", 1024),
				},
			}},
		}},
	})
	if imageBody > 100 {
		t.Fatalf("base64 payload counted: %d", imageBody)
	}
}

// TestAnthropicSDKRequestID 响应必须带 SDK 读取的 request-id 头。
func TestAnthropicSDKRequestID(t *testing.T) {
	t.Parallel()

	h := NewHandler(&Config{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}

	if rec.Header().Get("Request-Id") == "" {
		t.Fatal("missing request-id header")
	}
}

// TestAnthropicSDKCacheControlImage 顶层与 image 上的 cache_control 都要保留。
func TestAnthropicSDKCacheControlImage(t *testing.T) {
	t.Parallel()

	body := []byte(`{"model":"m","cache_control":{"type":"ephemeral"},"messages":[
		{"role":"user","content":[{"type":"image","cache_control":{"type":"ephemeral"},
			"source":{"type":"base64","media_type":"image/png","data":"abc"}}]}
	]}`)

	raw, _, _, err := anthropicToOpenAI(body)
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	if _, ok := got["cache_control"]; !ok {
		t.Fatal("top-level cache_control dropped")
	}

	msgs := got["messages"].([]any)

	parts := msgs[0].(map[string]any)["content"].([]any)
	if _, ok := parts[0].(map[string]any)["cache_control"]; !ok {
		t.Fatalf("image cache_control dropped: %v", parts[0])
	}
}

// TestAnthropicSDKRedactedThinkingSkipped 加密 thinking 块没有可见内容，
// 跳过而不是让整轮请求失败。
func TestAnthropicSDKRedactedThinkingSkipped(t *testing.T) {
	t.Parallel()

	body := []byte(`{"model":"m","messages":[
		{"role":"assistant","content":[{"type":"redacted_thinking","data":"opaque"}]}
	]}`)

	raw, _, _, err := anthropicToOpenAI(body)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}

	var got struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	if _, ok := got.Messages[0]["reasoning_content"]; ok {
		t.Fatalf("redacted_thinking leaked as reasoning: %v", got.Messages[0])
	}
}

// TestAnthropicSDKStreamStopSequence 流式同样映射 stop_sequence。
func TestAnthropicSDKStreamStopSequence(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	writer := newAnthropicStreamWriterWithOptions(rec, "m", anthropicResponseOptions{
		stopSequences: []string{"END"},
	})

	chunks := []string{
		`{"choices":[{"delta":{"content":"answer END"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`[DONE]`,
	}
	for _, chunk := range chunks {
		if _, err := writer.Write([]byte("data: " + chunk + "\n\n")); err != nil {
			t.Fatal(err)
		}
	}

	if !strings.Contains(rec.Body.String(), `"stop_reason":"stop_sequence"`) ||
		!strings.Contains(rec.Body.String(), `"stop_sequence":"END"`) {
		t.Fatalf("stream stop sequence not mapped:\n%s", rec.Body.String())
	}
}

// TestAnthropicSDKStopSequenceLongStream 长正文只保留末尾做后缀匹配，
// 命中判定不受影响（不再为整段输出留副本）。
func TestAnthropicSDKStopSequenceLongStream(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	writer := newAnthropicStreamWriterWithOptions(rec, "m", anthropicResponseOptions{
		stopSequences: []string{"END"},
	})

	for range 50 {
		if _, err := writer.Write([]byte("data: " + `{"choices":[{"delta":{"content":"filler"}}]}` + "\n\n")); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := writer.Write([]byte("data: " + `{"choices":[{"delta":{"content":"answer END"}}]}` + "\n\n")); err != nil {
		t.Fatal(err)
	}

	if _, err := writer.Write([]byte("data: [DONE]\n\n")); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(rec.Body.String(), `"stop_reason":"stop_sequence"`) {
		t.Fatalf("stop sequence lost on long stream:\n%s", rec.Body.String())
	}
}

// TestAnthropicSDKCustomTool 自定义工具的 type 可省略或为 "custom"，strict 透传。
func TestAnthropicSDKCustomTool(t *testing.T) {
	t.Parallel()

	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],
		"tools":[
			{"type":"custom","name":"a","input_schema":{"type":"object"},"strict":true},
			{"name":"b","input_schema":{"type":"object"}}
		]}`)

	raw, _, _, err := anthropicToOpenAI(body)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}

	var got struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	if len(got.Tools) != 2 {
		t.Fatalf("tools=%v", got.Tools)
	}

	fn := got.Tools[0]["function"].(map[string]any)
	if fn["name"] != "a" || fn["strict"] != true {
		t.Fatalf("custom tool=%v", fn)
	}
}

// TestAnthropicSDKStopSequenceOnlySuffix 正文中间的停止序列不是命中。
func TestAnthropicSDKStopSequenceOnlySuffix(t *testing.T) {
	t.Parallel()

	msg := anthropicMessageFromOpenAIWithOptions(map[string]any{
		"choices": []any{map[string]any{
			"finish_reason": "stop",
			"message":       map[string]any{"content": "a END b"},
		}},
	}, "m", anthropicResponseOptions{stopSequences: []string{"END"}})

	if msg["stop_reason"] != "end_turn" || msg["stop_sequence"] != nil {
		t.Fatalf("stop=%v/%v want end_turn/nil", msg["stop_reason"], msg["stop_sequence"])
	}
}

// TestAnthropicSDKToolChoiceValidation 非法 tool_choice 必须明确报错，不能静默降级。
func TestAnthropicSDKToolChoiceValidation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"missing name", `{"type":"tool"}`, "tool_choice name is required"},
		{"unknown type", `{"type":"bogus"}`, "unsupported field"},
	}
	for _, tc := range cases {
		body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],
			"tool_choice":` + tc.raw + `}`)
		if _, _, _, err := anthropicToOpenAI(body); err == nil ||
			!strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err=%v want %q", tc.name, err, tc.want)
		}
	}
}

// TestAnthropicSDKRequiredBlockFields 缺失 Anthropic 必填字段时明确 400。
func TestAnthropicSDKRequiredBlockFields(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "image media_type",
			body: `{"model":"m","messages":[{"role":"user","content":[
				{"type":"image","source":{"type":"base64","data":"abc"}}]}]}`,
			want: "source.media_type is required",
		},
		{
			name: "tool_result id",
			body: `{"model":"m","messages":[{"role":"user","content":[
				{"type":"tool_result","content":"ok"}]}]}`,
			want: "tool_use_id",
		},
		{
			name: "tool_use id",
			body: `{"model":"m","messages":[{"role":"assistant","content":[
				{"type":"tool_use","name":"f","input":{}}]}]}`,
			want: "tool_use id",
		},
		{
			name: "document text data",
			body: `{"model":"m","messages":[{"role":"user","content":[
				{"type":"document","source":{"type":"text","media_type":"text/plain"}}]}]}`,
			want: "source.data",
		},
		{
			name: "tool name",
			body: `{"model":"m","messages":[{"role":"user","content":"hi"}],
				"tools":[{"input_schema":{"type":"object"}}]}`,
			want: "tool name is required",
		},
	}
	for _, tc := range cases {
		if _, _, _, err := anthropicToOpenAI([]byte(tc.body)); err == nil ||
			!strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err=%v want %q", tc.name, err, tc.want)
		}
	}
}

// TestAnthropicSDKCanonicalRequest 固定 SDK 序列化出的完整请求形态，
// 防止后续改动漏映射常用字段。
func TestAnthropicSDKCanonicalRequest(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"max_tokens":16,
		"messages":[{"content":[{"text":"hi","cache_control":{"ttl":"5m","type":"ephemeral"},"type":"text"}],"role":"user"}],
		"model":"m",
		"temperature":0.3,
		"top_k":4,
		"top_p":0.9,
		"speed":"fast",
		"cache_control":{"ttl":"5m","type":"ephemeral"},
		"metadata":{"user_id":"u"},
		"output_config":{
			"effort":"high",
			"format":{"schema":{"type":"object"},"type":"json_schema"}
		},
		"service_tier":"standard_only",
		"stop_sequences":["END"],
		"system":[{"text":"sys","cache_control":{"ttl":"1h","type":"ephemeral"},"type":"text"}],
		"thinking":{"display":"omitted","type":"adaptive"},
		"tool_choice":{"disable_parallel_tool_use":true,"type":"auto"},
		"tools":[{
			"input_schema":{"properties":{"x":{"type":"string"}},"type":"object"},
			"name":"f","strict":true,"type":"custom"
		}]
	}`)

	converted, err := anthropicConvert(body)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(converted.openAI, &got); err != nil {
		t.Fatal(err)
	}

	assertCanonicalParams(t, got, converted)
	assertCanonicalTools(t, got)
	assertCanonicalMessages(t, got)
}

func assertCanonicalParams(t *testing.T, got map[string]any, converted anthropicConversion) {
	t.Helper()

	for key, want := range map[string]any{
		"max_tokens":   float64(16),
		"temperature":  0.3,
		"top_k":        float64(4),
		"top_p":        0.9,
		"speed":        "fast",
		"service_tier": "standard_only",
	} {
		if got[key] != want {
			t.Errorf("%s=%v want %v", key, got[key], want)
		}
	}

	for _, key := range []string{"cache_control", "metadata", "response_format"} {
		if _, ok := got[key]; !ok {
			t.Errorf("%s dropped", key)
		}
	}

	if got["reasoning_effort"] != "high" {
		t.Errorf("reasoning_effort=%v", got["reasoning_effort"])
	}

	if got["parallel_tool_calls"] != false {
		t.Errorf("parallel_tool_calls=%v", got["parallel_tool_calls"])
	}

	thinking, _ := got["thinking"].(map[string]any)
	if thinking["type"] != "enabled" || !converted.options.omitThinking {
		t.Errorf("thinking=%v omit=%v", thinking, converted.options.omitThinking)
	}
}

func assertCanonicalTools(t *testing.T, got map[string]any) {
	t.Helper()

	tools, _ := got["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools=%v", got["tools"])
	}

	fn, _ := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["strict"] != true {
		t.Errorf("tool strict=%v", fn["strict"])
	}
}

func assertCanonicalMessages(t *testing.T, got map[string]any) {
	t.Helper()

	msgs, _ := got["messages"].([]any)
	if len(msgs) == 0 || msgs[0].(map[string]any)["role"] != "system" {
		t.Errorf("system message missing: %v", msgs)
	}
}

// TestAnthropicSDKModelsRequestID Anthropic Models 响应同样带 SDK 读取的 request-id。
func TestAnthropicSDKModelsRequestID(t *testing.T) {
	t.Parallel()

	// 必须用有账号池的 handler：Config{} 下 modelList() 会 panic，被 withRecovery
	// 兜成 500，头部仍带 Request-Id，断言会假通过。
	handler := modelsTestHandler()

	rec := doModelsRequest(handler, "/v1/models", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/models code=%d body=%s", rec.Code, rec.Body)
	}

	if rec.Header().Get("Request-Id") == "" {
		t.Fatal("/v1/models missing request-id header")
	}

	rec = doModelsRequest(handler, "/v1/models/claude-sonnet-4-5", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/models/{model} code=%d body=%s", rec.Code, rec.Body)
	}

	if rec.Header().Get("Request-Id") == "" {
		t.Fatal("/v1/models/{model} missing request-id header")
	}
}

// TestAnthropicSDKThinkingBeforeText 同一 chunk 同时含 reasoning 与 content 时，
// thinking 块必须先于 text 块开启。
func TestAnthropicSDKThinkingBeforeText(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	writer := newAnthropicStreamWriter(rec, "m")

	chunks := []string{
		`{"choices":[{"delta":{"reasoning_content":"r","content":"a"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`[DONE]`,
	}
	for _, chunk := range chunks {
		if _, err := writer.Write([]byte("data: " + chunk + "\n\n")); err != nil {
			t.Fatal(err)
		}
	}

	var types []string

	for _, event := range parseAnthropicSSE(t, rec.Body.String()) {
		if event["type"] == "content_block_start" {
			block := event["content_block"].(map[string]any)
			types = append(types, stringValue(block["type"]))
		}
	}

	if len(types) != 2 || types[0] != "thinking" || types[1] != "text" {
		t.Fatalf("content block order=%v want [thinking text]", types)
	}
}

// TestAnthropicSDKDataWithoutSpace 兼容 SSE data 字段省略冒号后空格的写法。
func TestAnthropicSDKDataWithoutSpace(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	writer := newAnthropicStreamWriter(rec, "m")

	for _, chunk := range []string{
		`{"choices":[{"delta":{"content":"hi"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
	} {
		if _, err := writer.Write([]byte("data:" + chunk + "\n\n")); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := writer.Write([]byte("data:[DONE]\n\n")); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(rec.Body.String(), `"text":"hi"`) ||
		!strings.Contains(rec.Body.String(), "event: message_stop") {
		t.Fatalf("data without space not parsed:\n%s", rec.Body.String())
	}
}

// TestAnthropicSDKStreamThinkingOmitted 流式 display=omitted 不回传 thinking 块。
func TestAnthropicSDKStreamThinkingOmitted(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	writer := newAnthropicStreamWriterWithOptions(rec, "m", anthropicResponseOptions{omitThinking: true})

	chunks := []string{
		`{"choices":[{"delta":{"reasoning_content":"secret"}}]}`,
		`{"choices":[{"delta":{"content":"answer"},"finish_reason":"stop"}]}`,
		`[DONE]`,
	}
	for _, chunk := range chunks {
		if _, err := writer.Write([]byte("data: " + chunk + "\n\n")); err != nil {
			t.Fatal(err)
		}
	}

	if strings.Contains(rec.Body.String(), "secret") || strings.Contains(rec.Body.String(), `"type":"thinking"`) {
		t.Fatalf("omitted thinking leaked:\n%s", rec.Body.String())
	}
}

// TestAnthropicSDKGatewayStreamErrorType 网关本地兜底帧（空流/截断）不是客户端
// 请求错误，必须映射为 api_error 而不是 invalid_request_error。
func TestAnthropicSDKGatewayStreamErrorType(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	writer := newAnthropicStreamWriter(rec, "m")

	chunks := []string{
		`{"choices":[{"delta":{"content":"hi"}}]}`,
		`{"error":{"message":"upstream stream truncated before [DONE]","type":"upstream_error","code":"upstream_parse"}}`,
		`[DONE]`,
	}
	for _, chunk := range chunks {
		if _, err := writer.Write([]byte("data: " + chunk + "\n\n")); err != nil {
			t.Fatal(err)
		}
	}

	out := rec.Body.String()
	if !strings.Contains(out, `"type":"api_error"`) || strings.Contains(out, `"invalid_request_error"`) {
		t.Fatalf("gateway error type=\n%s", out)
	}
}

// TestAnthropicSDKNotFoundRequestID 未注册的 Anthropic 路径也要带 request-id。
func TestAnthropicSDKNotFoundRequestID(t *testing.T) {
	t.Parallel()

	h := NewHandler(&Config{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages/unknown",
		strings.NewReader(`{}`)))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if rec.Header().Get("Request-Id") == "" {
		t.Fatal("missing request-id header")
	}
}

// TestAnthropicSDKRequestIDNotOverwritten 已存在的 Request-Id 不被错误路径改写。
func TestAnthropicSDKRequestIDNotOverwritten(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	rec.Header().Set("Request-Id", "req-fixed")

	writeAnthropicError(rec, http.StatusBadRequest, "invalid_request_error", "bad", "")

	if got := rec.Header().Get("Request-Id"); got != "req-fixed" {
		t.Fatalf("Request-Id=%q want req-fixed", got)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}

	if body["request_id"] != "req-fixed" {
		t.Fatalf("body request_id=%v want req-fixed", body["request_id"])
	}
}

// TestAnthropicSDKMediaSources image/document 的每种 source 形态都必须落到
// 可用的 OpenAI content part，不能静默丢弃。
func TestAnthropicSDKMediaSources(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		block string
		want  []string
	}{
		{
			name:  "image url",
			block: `{"type":"image","source":{"type":"url","url":"https://x/a.png"}}`,
			want:  []string{`"type":"image_url"`, `https://x/a.png`},
		},
		{
			name:  "image file",
			block: `{"type":"image","source":{"type":"file","file_id":"f1"}}`,
			want:  []string{`"type":"file"`, `"file_id":"f1"`},
		},
		{
			name:  "document text",
			block: `{"type":"document","source":{"type":"text","media_type":"text/plain","data":"DOC"}}`,
			want:  []string{`"content":"DOC"`},
		},
		{
			name:  "document content string",
			block: `{"type":"document","source":{"type":"content","content":"NESTED"}}`,
			want:  []string{`"content":"NESTED"`},
		},
		{
			name: "document content blocks",
			block: `{"type":"document","source":{"type":"content","content":[
				{"type":"text","text":"A"},{"type":"image","source":{"type":"url","url":"https://x/b.png"}}]}}`,
			want: []string{`"type":"text"`, `"text":"A"`, `"type":"image_url"`, `https://x/b.png`},
		},
		{
			name:  "document base64",
			block: `{"type":"document","title":"a.pdf","source":{"type":"base64","media_type":"application/pdf","data":"abc"}}`,
			want:  []string{`"type":"file"`, `"filename":"a.pdf"`, `data:application/pdf;base64,abc`},
		},
		{
			name:  "document url",
			block: `{"type":"document","title":"a.pdf","source":{"type":"url","url":"https://x/a.pdf"}}`,
			want:  []string{`"type":"file"`, `"file_url":"https://x/a.pdf"`},
		},
		{
			name:  "document file",
			block: `{"type":"document","source":{"type":"file","file_id":"d1"}}`,
			want:  []string{`"type":"file"`, `"file_id":"d1"`},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			body := []byte(`{"model":"m","messages":[{"role":"user","content":[` + testCase.block + `]}]}`)

			raw, _, _, err := anthropicToOpenAI(body)
			if err != nil {
				t.Fatalf("convert: %v", err)
			}

			for _, want := range testCase.want {
				if !strings.Contains(string(raw), want) {
					t.Errorf("missing %q in %s", want, raw)
				}
			}
		})
	}
}

// TestAnthropicSDKUnsupportedRequestFields 无对应上游形态的字段必须明确拒绝，
// 不能静默忽略让客户端误以为已生效。
func TestAnthropicSDKUnsupportedRequestFields(t *testing.T) {
	t.Parallel()

	fields := []string{
		`"container":{"id":"c1"}`,
		`"diagnostics":{"previous_message_id":"msg_1"}`,
		`"inference_geo":"us"`,
	}
	for _, field := range fields {
		body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],` + field + `}`)

		if _, _, _, err := anthropicToOpenAI(body); err == nil ||
			!strings.Contains(err.Error(), "unsupported field") {
			t.Errorf("field=%s err=%v want unsupported field", field, err)
		}
	}
}

// TestAnthropicSDKEmptyUnsupportedFields 客户端序列化零值（{} / []）不等于
// 「使用了该能力」，不应被判为 unsupported field。
func TestAnthropicSDKEmptyUnsupportedFields(t *testing.T) {
	t.Parallel()

	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],
		"container":{},"diagnostics":[]}`)

	if _, _, _, err := anthropicToOpenAI(body); err != nil {
		t.Fatalf("empty container/diagnostics rejected: %v", err)
	}
}

// TestAnthropicSDKIgnoresUnknownFields 协议扩展字段不能导致请求失败或泄漏到上游。
func TestAnthropicSDKIgnoresUnknownFields(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"model":"m",
		"messages":[{"role":"user","content":"hi"}],
		"context_management":{"edits":[]},
		"mcp_servers":[{"type":"url","url":"https://mcp.example","name":"mcp"}],
		"future_field":{"enabled":true}
	}`)

	converted, err := anthropicConvert(body)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}

	for _, field := range []string{"context_management", "mcp_servers", "future_field"} {
		if strings.Contains(string(converted.openAI), `"`+field+`"`) {
			t.Errorf("unknown field %q leaked upstream: %s", field, converted.openAI)
		}
	}
}

// TestAnthropicSDKStopSequenceWithoutFinishReason 上游未发 finish_reason 时，
// 正文以停止序列收尾仍必须返回 stop_sequence。
func TestAnthropicSDKStopSequenceWithoutFinishReason(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	writer := newAnthropicStreamWriterWithOptions(rec, "m", anthropicResponseOptions{
		stopSequences: []string{"END"},
	})

	for _, frame := range []string{
		`data: {"choices":[{"delta":{"content":"answer END"}}]}` + "\n\n",
		"data: [DONE]\n\n",
	} {
		if _, err := writer.Write([]byte(frame)); err != nil {
			t.Fatal(err)
		}
	}

	if !strings.Contains(rec.Body.String(), `"stop_reason":"stop_sequence"`) {
		t.Fatalf("missing stop_sequence:\n%s", rec.Body.String())
	}
}

// TestAnthropicSDKCacheControlBlocks document 与 tool_result 的 cache_control
// 必须保留到出站内容，供上游 prompt cache 使用。
func TestAnthropicSDKCacheControlBlocks(t *testing.T) {
	t.Parallel()

	body := []byte(`{"model":"m","messages":[
		{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"f","input":{}}]},
		{"role":"user","content":[
			{"type":"document","cache_control":{"type":"ephemeral"},
				"source":{"type":"text","media_type":"text/plain","data":"DOC"}},
			{"type":"tool_result","tool_use_id":"t1","cache_control":{"type":"ephemeral"},
				"content":[{"type":"text","text":"ok"}]}
		]}
	]}`)

	raw, _, _, err := anthropicToOpenAI(body)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}

	var got struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	for _, message := range got.Messages {
		if message["role"] == "tool" && message["cache_control"] == nil {
			t.Fatalf("tool message cache_control lost: %v", message)
		}
	}

	userContent := got.Messages[len(got.Messages)-1]["content"]

	parts, ok := userContent.([]any)
	if !ok {
		t.Fatalf("document content should keep parts: %T %v", userContent, userContent)
	}

	if parts[0].(map[string]any)["cache_control"] == nil {
		t.Fatalf("document cache_control lost: %v", parts)
	}
}

// TestAnthropicSDKNullCacheControl 显式 cache_control=null 等同未设置，
// 不应把纯文本 content 撑成 parts 数组。
func TestAnthropicSDKNullCacheControl(t *testing.T) {
	t.Parallel()

	body := []byte(`{"model":"m","messages":[{"role":"user","content":[
		{"type":"text","text":"hi","cache_control":null}]}]}`)

	raw, _, _, err := anthropicToOpenAI(body)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}

	var got struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	if got.Messages[0]["content"] != "hi" {
		t.Fatalf("content=%v want string", got.Messages[0]["content"])
	}
}

// TestAnthropicSDKStopReasonPassthrough 兼容上游直接回传 Anthropic 形态
// stop_reason，避免被降级成 end_turn。
func TestAnthropicSDKStopReasonPassthrough(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"end_turn":                      "end_turn",
		"max_tokens":                    "max_tokens",
		"tool_use":                      "tool_use",
		"pause_turn":                    "pause_turn",
		"model_context_window_exceeded": "model_context_window_exceeded",
		"refusal":                       "refusal",
	}
	for reason, want := range cases {
		if got := anthropicStopReason(reason, false); got != want {
			t.Errorf("anthropicStopReason(%q)=%q want %q", reason, got, want)
		}
	}
}

// TestAnthropicSDKMaxTokensNegative max_tokens 官方下限为 0，负值必须 400。
func TestAnthropicSDKMaxTokensNegative(t *testing.T) {
	t.Parallel()

	body := []byte(`{"model":"m","max_tokens":-1,"messages":[{"role":"user","content":"hi"}]}`)

	if _, _, _, err := anthropicToOpenAI(body); err == nil ||
		!strings.Contains(err.Error(), "max_tokens") {
		t.Fatalf("err=%v want max_tokens validation", err)
	}
}

// TestAnthropicSDKIgnoresPingFrames ping 帧不能提前触发 message_start，
// 否则后续 chunk 的真实 id 会被 msg_gateway 占位。
func TestAnthropicSDKIgnoresPingFrames(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	writer := newAnthropicStreamWriter(rec, "m")

	for _, frame := range []string{
		"data: {\"type\":\"ping\"}\n\n",
		"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n",
		"data: [DONE]\n\n",
	} {
		if _, err := writer.Write([]byte(frame)); err != nil {
			t.Fatal(err)
		}
	}

	out := rec.Body.String()
	if !strings.Contains(out, `"type":"message_start"`) || !strings.Contains(out, `"id":"c1"`) {
		t.Fatalf("ping frame started message early:\n%s", out)
	}
}

// collectAnthropicBlockTypes 返回流中 content_block_start 的块类型序列。
func collectAnthropicBlockTypes(t *testing.T, chunks []string) []string {
	t.Helper()

	rec := httptest.NewRecorder()
	writer := newAnthropicStreamWriter(rec, "m")

	for _, chunk := range chunks {
		frame := "data: " + chunk + "\n\n"
		if chunk == "[DONE]" {
			frame = "data: [DONE]\n\n"
		}

		if _, err := writer.Write([]byte(frame)); err != nil {
			t.Fatal(err)
		}
	}

	var types []string

	for _, event := range parseAnthropicSSE(t, rec.Body.String()) {
		if event["type"] == "content_block_start" {
			block := event["content_block"].(map[string]any)
			types = append(types, stringValue(block["type"]))
		}
	}

	return types
}

// TestAnthropicSDKBlockOrder text / tool_use 交错时按原顺序开块：
// 文本不能被提前到工具调用之前，工具调用也不能被延后到文本之后。
func TestAnthropicSDKBlockOrder(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		chunks []string
		want   []string
	}{
		{
			name: "text then tool then text",
			chunks: []string{
				`{"choices":[{"delta":{"content":"hello "}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"A","function":{"name":"a","arguments":"{}"}}]}}]}`,
				`{"choices":[{"delta":{"content":"world"}}]}`,
				`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
				`[DONE]`,
			},
			want: []string{"text", "tool_use", "text"},
		},
		{
			name: "tool then text then tool",
			chunks: []string{
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"A","function":{"name":"a","arguments":"{\"x\":1}"}}]}}]}`,
				`{"choices":[{"delta":{"content":"mid"}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"B","function":{"name":"b","arguments":"{\"y\":2}"}}]}}]}`,
				`[DONE]`,
			},
			want: []string{"tool_use", "text", "tool_use"},
		},
		{
			name: "alternating",
			chunks: []string{
				`{"choices":[{"delta":{"content":"a"}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"A","function":{"name":"a","arguments":"{}"}}]}}]}`,
				`{"choices":[{"delta":{"content":"b"}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"B","function":{"name":"b","arguments":"{}"}}]}}]}`,
				`{"choices":[{"delta":{"content":"c"}}]}`,
				`[DONE]`,
			},
			want: []string{"text", "tool_use", "text", "tool_use", "text"},
		},
		{
			name: "parallel tools then text",
			chunks: []string{
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"A","function":{"name":"a","arguments":"{}"}}]}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"B","function":{"name":"b","arguments":"{}"}}]}}]}`,
				`{"choices":[{"delta":{"content":"done"}}]}`,
				`[DONE]`,
			},
			want: []string{"tool_use", "tool_use", "text"},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got := collectAnthropicBlockTypes(t, testCase.chunks)
			if len(got) != len(testCase.want) {
				t.Fatalf("block order=%v want %v", got, testCase.want)
			}

			for i := range testCase.want {
				if got[i] != testCase.want[i] {
					t.Fatalf("block order=%v want %v", got, testCase.want)
				}
			}
		})
	}
}

// TestAnthropicSDKTrailingTextStreamsImmediately 工具调用已完整后正文恢复时，
// 工具块与正文必须立即落地，不能压到 [DONE] 才发（否则工具调用后的长尾正文
// 对客户端是一次性吐出，流式失效）。
func TestAnthropicSDKTrailingTextStreamsImmediately(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	writer := newAnthropicStreamWriter(rec, "m")

	for _, chunk := range []string{
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"A","function":{"name":"a","arguments":"{}"}}]}}]}`,
		`{"choices":[{"delta":{"content":"trailing answer"}}]}`,
	} {
		if _, err := writer.Write([]byte("data: " + chunk + "\n\n")); err != nil {
			t.Fatal(err)
		}
	}

	out := rec.Body.String()
	if !strings.Contains(out, `"type":"tool_use"`) || !strings.Contains(out, `"text":"trailing answer"`) {
		t.Fatalf("trailing text not streamed before [DONE]:\n%s", out)
	}

	if _, err := writer.Write([]byte("data: [DONE]\n\n")); err != nil {
		t.Fatal(err)
	}

	assertStreamInvariants(t, "trailing text", parseAnthropicSSE(t, rec.Body.String()))
}

// TestAnthropicSDKZeroArgToolFlushOnFinish 零参工具（arguments 为空）永远不满足
// 「参数是合法 JSON」，必须在 finish_reason 到达时落地，否则尾文要等 [DONE]；
// 截断流没有 [DONE] 时更会整段丢失。
func TestAnthropicSDKZeroArgToolFlushOnFinish(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	writer := newAnthropicStreamWriter(rec, "m")

	for _, chunk := range []string{
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"A","function":{"name":"a","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"content":"after tool"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	} {
		if _, err := writer.Write([]byte("data: " + chunk + "\n\n")); err != nil {
			t.Fatal(err)
		}
	}

	out := rec.Body.String()
	if !strings.Contains(out, `"type":"tool_use"`) || !strings.Contains(out, `"text":"after tool"`) {
		t.Fatalf("zero-arg tool/text not flushed at finish_reason:\n%s", out)
	}

	if _, err := writer.Write([]byte("data: [DONE]\n\n")); err != nil {
		t.Fatal(err)
	}

	assertStreamInvariants(t, "zero-arg finish", parseAnthropicSSE(t, rec.Body.String()))
}

// TestAnthropicSDKMergedTrailingText 工具参数补齐前后各有一段正文时，两段
// 必须续在同一个 text 块里，不能拆成相邻的两个 content_block。
func TestAnthropicSDKMergedTrailingText(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	writer := newAnthropicStreamWriter(rec, "m")

	for _, chunk := range []string{
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"A","function":{"name":"a","arguments":"{\"x\":"}}]}}]}`,
		`{"choices":[{"delta":{"content":"t"}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`,
		`{"choices":[{"delta":{"content":"after"}}]}`,
		`[DONE]`,
	} {
		if _, err := writer.Write([]byte("data: " + chunk + "\n\n")); err != nil {
			t.Fatal(err)
		}
	}

	out := rec.Body.String()
	if got := strings.Count(out, `"type":"text"`); got != 1 {
		t.Fatalf("text blocks=%d want 1:\n%s", got, out)
	}

	for _, want := range []string{`"text":"t"`, `"text":"after"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %s:\n%s", want, out)
		}
	}

	assertStreamInvariants(t, "merged trailing text", parseAnthropicSSE(t, out))
}

// TestAnthropicSDKErrorFlushesPendingContent 上游中途报错/截断时，已生成的
// 正文与已缓冲的工具块必须先落地，再写 error 事件，不能一起丢掉。
func TestAnthropicSDKErrorFlushesPendingContent(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	writer := newAnthropicStreamWriter(rec, "m")

	for _, chunk := range []string{
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"A","function":{"name":"a","arguments":"{\"x\":"}}]}}]}`,
		`{"choices":[{"delta":{"content":"partial answer"}}]}`,
		`{"error":{"message":"upstream stream truncated before [DONE]","type":"upstream_error","code":"upstream_parse"}}`,
	} {
		if _, err := writer.Write([]byte("data: " + chunk + "\n\n")); err != nil {
			t.Fatal(err)
		}
	}

	out := rec.Body.String()
	for _, want := range []string{`"type":"tool_use"`, `"text":"partial answer"`, `"type":"api_error"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %s before error:\n%s", want, out)
		}
	}

	if strings.Index(out, "event: error") < strings.Index(out, `"type":"tool_use"`) {
		t.Fatalf("error event written before pending content:\n%s", out)
	}
}

// TestAnthropicSDKToolBatchBoundaryKeepsArgs 首片只带 name（arguments 为空）
// 的调用不能被批次边界提前写出，否则后续参数分片会落进没有 name 的新块而丢失。
func TestAnthropicSDKToolBatchBoundaryKeepsArgs(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	writer := newAnthropicStreamWriter(rec, "m")

	for _, chunk := range []string{
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"A","function":{"name":"a","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"content":"mid"}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"B","function":{"name":"b","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"x\":1}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{\"y\":2}"}}]}}]}`,
		`[DONE]`,
	} {
		frame := "data: " + chunk + "\n\n"
		if chunk == "[DONE]" {
			frame = "data: [DONE]\n\n"
		}

		if _, err := writer.Write([]byte(frame)); err != nil {
			t.Fatal(err)
		}
	}

	starts, args := collectAnthropicToolBlocks(parseAnthropicSSE(t, rec.Body.String()))
	if len(starts) != 2 {
		t.Fatalf("tool_use blocks=%d want 2:\n%s", len(starts), rec.Body.String())
	}

	for index, want := range map[float64]string{0: `{"x":1}`, 1: `{"y":2}`} {
		if args[index] != want {
			t.Errorf("index %v args=%q want %q", index, args[index], want)
		}
	}
}

// TestAnthropicSDKNestedCacheControl tool_result / search_result 的子块
// cache_control 必须回退保留到出站内容。
func TestAnthropicSDKNestedCacheControl(t *testing.T) {
	t.Parallel()

	body := []byte(`{"model":"m","messages":[
		{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"f","input":{}}]},
		{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"t1","content":[
				{"type":"text","text":"ok","cache_control":{"type":"ephemeral"}}]},
			{"type":"search_result","title":"T","source":"S","content":[
				{"type":"text","text":"BODY","cache_control":{"type":"ephemeral"}}]}
		]}
	]}`)

	raw, _, _, err := anthropicToOpenAI(body)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}

	var got struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	var toolMsg map[string]any

	for _, message := range got.Messages {
		if message["role"] == "tool" {
			toolMsg = message
		}
	}

	if toolMsg == nil || toolMsg["cache_control"] == nil {
		t.Fatalf("tool_result nested cache_control lost: %v", got.Messages)
	}

	user := got.Messages[len(got.Messages)-1]

	parts, _ := user["content"].([]any)
	if len(parts) == 0 || parts[len(parts)-1].(map[string]any)["cache_control"] == nil {
		t.Fatalf("search_result nested cache_control lost: %v", user)
	}
}

// TestAnthropicSDKTooManyMessages 官方上限 100,000 条，超限必须 400。
func TestAnthropicSDKTooManyMessages(t *testing.T) {
	t.Parallel()

	body := []byte(`{"model":"m","messages":[` +
		strings.Repeat(`{"role":"user","content":"hi"},`, anthropicMaxMessages) +
		`{"role":"user","content":"hi"}]}`)

	if _, _, _, err := anthropicToOpenAI(body); err == nil ||
		!strings.Contains(err.Error(), "too many messages") {
		t.Fatalf("err=%v want too many messages", err)
	}
}

// TestAnthropicSDKMethodNotAllowed Messages 端点方法不匹配返回 405 + Allow。
func TestAnthropicSDKMethodNotAllowed(t *testing.T) {
	t.Parallel()

	handler := NewHandler(&Config{})

	// 尾斜杠写法不落在精确路由上，但仍是同一个端点，不能退化成 404。
	for _, path := range []string{
		"/v1/messages", "/v1/messages/count_tokens",
		"/v1/messages/", "/v1/messages/count_tokens/",
	} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s code=%d want 405", path, rec.Code)
		}

		if rec.Header().Get("Allow") != http.MethodPost {
			t.Errorf("%s Allow=%q want POST", path, rec.Header().Get("Allow"))
		}

		if !strings.Contains(rec.Body.String(), `"type":"error"`) {
			t.Errorf("%s body=%s", path, rec.Body)
		}
	}
}

// TestAnthropicSDKToolResultMedia tool_result 内的 document / search_result
// 必须转成后续 user 内容，不能丢失。
func TestAnthropicSDKToolResultMedia(t *testing.T) {
	t.Parallel()

	body := []byte(`{"model":"m","messages":[
		{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"f","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[
			{"type":"text","text":"ok"},
			{"type":"document","source":{"type":"text","media_type":"text/plain","data":"DOC"}},
			{"type":"search_result","title":"T","source":"S","content":[{"type":"text","text":"BODY"}]}
		]}]}
	]}`)

	raw, _, _, err := anthropicToOpenAI(body)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}

	var got struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	var toolContent string

	for _, message := range got.Messages {
		if message["role"] == "tool" {
			toolContent = stringValue(message["content"])
		}
	}

	if toolContent != "ok" {
		t.Fatalf("tool content=%q want ok", toolContent)
	}

	joined, _ := json.Marshal(got.Messages)
	for _, want := range []string{"DOC", "BODY", "T (S)"} {
		if !strings.Contains(string(joined), want) {
			t.Errorf("missing %q in %s", want, joined)
		}
	}
}

// TestAnthropicSDKSystemCacheControl system 文本块的 cache_control 必须保留为
// content parts，不能压缩成纯字符串而丢掉断点。
func TestAnthropicSDKSystemCacheControl(t *testing.T) {
	t.Parallel()

	body := []byte(`{"model":"m","system":[
		{"type":"text","text":"sys","cache_control":{"type":"ephemeral","ttl":"1h"}}
	],"messages":[{"role":"user","content":"hi"}]}`)

	raw, _, _, err := anthropicToOpenAI(body)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}

	var got struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	system := got.Messages[0]

	parts, ok := system["content"].([]any)
	if !ok || len(parts) != 1 {
		t.Fatalf("system content=%v want parts", system["content"])
	}

	if parts[0].(map[string]any)["cache_control"] == nil {
		t.Fatalf("system cache_control lost: %v", parts[0])
	}
}

// TestAnthropicSDKEmptyArgsBoundary 首片只带 name（arguments 为空）时，
// 文本后的新工具调用不能提前刷新旧块，否则旧调用的后续参数会丢失。
func TestAnthropicSDKEmptyArgsBoundary(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	writer := newAnthropicStreamWriter(rec, "m")

	chunks := []string{
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"A","function":{"name":"a","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"content":"mid"}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"B","function":{"name":"b","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"x\":1}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{\"y\":2}"}}]}}]}`,
	}
	for _, chunk := range chunks {
		if _, err := writer.Write([]byte("data: " + chunk + "\n\n")); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := writer.Write([]byte("data: [DONE]\n\n")); err != nil {
		t.Fatal(err)
	}

	starts, args := collectAnthropicToolBlocks(parseAnthropicSSE(t, rec.Body.String()))
	if len(starts) != 2 {
		t.Fatalf("tool_use blocks=%d want 2", len(starts))
	}

	if args[0] != `{"x":1}` || args[1] != `{"y":2}` {
		t.Fatalf("args=%v want 0:{\"x\":1} 1:{\"y\":2}", args)
	}
}

// TestAnthropicSDKMessageContentRequired 消息 content 缺失或为 null 必须 400，
// 不能静默变成空 user 消息。
func TestAnthropicSDKMessageContentRequired(t *testing.T) {
	t.Parallel()

	bodies := []string{
		`{"model":"m","messages":[{"role":"user"}]}`,
		`{"model":"m","messages":[{"role":"user","content":null}]}`,
		`{"model":"m","messages":[{"role":"user","content":[]}]}`,
		`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":""}]}]}`,
	}
	for _, body := range bodies {
		if _, _, _, err := anthropicToOpenAI([]byte(body)); err == nil ||
			!strings.Contains(err.Error(), "content is required") {
			t.Errorf("body=%s err=%v want content required", body, err)
		}
	}
}

// TestAnthropicSDKNullInputSchema input_schema:null 不应把 parameters 置为 null。
func TestAnthropicSDKNullInputSchema(t *testing.T) {
	t.Parallel()

	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"custom","name":"f","input_schema":null}]}`)

	raw, _, _, err := anthropicToOpenAI(body)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}

	var got struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	fn := got.Tools[0]["function"].(map[string]any)
	if _, ok := fn["parameters"]; ok {
		t.Fatalf("parameters=%v want absent", fn["parameters"])
	}
}

// TestAnthropicSDKInvalidToolShapes input_schema / tool_use.input 必须是对象。
func TestAnthropicSDKInvalidToolShapes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "input_schema",
			body: `{"model":"m","messages":[{"role":"user","content":"hi"}],
				"tools":[{"name":"f","input_schema":"x"}]}`,
			want: "input_schema",
		},
		{
			name: "tool_use input",
			body: `{"model":"m","messages":[{"role":"assistant","content":[
				{"type":"tool_use","id":"t1","name":"f","input":"x"}]}]}`,
			want: "tool_use input",
		},
	}
	for _, testCase := range cases {
		if _, _, _, err := anthropicToOpenAI([]byte(testCase.body)); err == nil ||
			!strings.Contains(err.Error(), testCase.want) {
			t.Errorf("%s: err=%v want %q", testCase.name, err, testCase.want)
		}
	}
}

// TestAnthropicSDKSamplingRange temperature/top_p 限定 [0,1]，top_k 不得为负。
func TestAnthropicSDKSamplingRange(t *testing.T) {
	t.Parallel()

	cases := []struct {
		body string
		want string
	}{
		{`{"model":"m","messages":[{"role":"user","content":"hi"}],"temperature":-0.1}`, "temperature"},
		{`{"model":"m","messages":[{"role":"user","content":"hi"}],"temperature":1.1}`, "temperature"},
		{`{"model":"m","messages":[{"role":"user","content":"hi"}],"top_p":-0.1}`, "top_p"},
		{`{"model":"m","messages":[{"role":"user","content":"hi"}],"top_p":1.1}`, "top_p"},
		{`{"model":"m","messages":[{"role":"user","content":"hi"}],"top_k":-1}`, "top_k"},
	}
	for _, testCase := range cases {
		if _, _, _, err := anthropicToOpenAI([]byte(testCase.body)); err == nil ||
			!strings.Contains(err.Error(), testCase.want) {
			t.Errorf("body=%s err=%v want %q", testCase.body, err, testCase.want)
		}
	}
}

// TestAnthropicSDKMetadataUserIDTooLong metadata.user_id 上限 512。
func TestAnthropicSDKMetadataUserIDTooLong(t *testing.T) {
	t.Parallel()

	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"metadata":{"user_id":"` +
		strings.Repeat("a", anthropicMaxUserIDLen+1) + `"}}`)

	if _, _, _, err := anthropicToOpenAI(body); err == nil ||
		!strings.Contains(err.Error(), "metadata.user_id") {
		t.Fatalf("err=%v want metadata.user_id validation", err)
	}

	// 非对象 metadata 不能跳过校验直接上送。
	body = []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"metadata":"u"}`)

	if _, _, _, err := anthropicToOpenAI(body); err == nil ||
		!strings.Contains(err.Error(), "metadata must be an object") {
		t.Fatalf("err=%v want metadata object validation", err)
	}
}

// TestAnthropicSDKToolNameTooLong tool / tool_use name 超出官方长度上限。
func TestAnthropicSDKToolNameTooLong(t *testing.T) {
	t.Parallel()

	cases := []string{
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"` +
			strings.Repeat("a", anthropicMaxToolNameLen+1) + `","input_schema":{"type":"object"}}]}`,
		`{"model":"m","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"` +
			strings.Repeat("a", anthropicMaxToolUseNameLen+1) + `","input":{}}]}]}`,
	}
	for _, body := range cases {
		if _, _, _, err := anthropicToOpenAI([]byte(body)); err == nil ||
			!strings.Contains(err.Error(), "tool name is too long") {
			t.Errorf("err=%v want tool name too long", err)
		}
	}
}

// TestAnthropicSDKSearchResultRequired search_result 的 title/source/content 必填。
func TestAnthropicSDKSearchResultRequired(t *testing.T) {
	t.Parallel()

	blocks := []string{
		`{"type":"search_result","source":"S","content":[{"type":"text","text":"B"}]}`,
		`{"type":"search_result","title":"T","content":[{"type":"text","text":"B"}]}`,
		`{"type":"search_result","title":"T","source":"S"}`,
	}
	for _, block := range blocks {
		body := []byte(`{"model":"m","messages":[{"role":"user","content":[` + block + `]}]}`)
		if _, _, _, err := anthropicToOpenAI(body); err == nil ||
			!strings.Contains(err.Error(), "search_result") {
			t.Errorf("block=%s err=%v want search_result validation", block, err)
		}
	}
}

// TestAnthropicSDKStreamInvariants 检查 Anthropic SSE 的结构不变量：
// message_start/stop 各一次、块 index 严格递增且不复用、delta/stop 只作用于打开的块。
func TestAnthropicSDKStreamInvariants(t *testing.T) {
	t.Parallel()

	cases := map[string][]string{
		"plain text": {
			`{"choices":[{"delta":{"content":"a"}}]}`,
			`{"choices":[{"delta":{"content":"b"}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		},
		"thinking then text": {
			`{"choices":[{"delta":{"reasoning_content":"r"}}]}`,
			`{"choices":[{"delta":{"content":"a"}}]}`,
		},
		"tool then text": {
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"A","function":{"name":"a","arguments":"{}"}}]}}]}`,
			`{"choices":[{"delta":{"content":"after"}}]}`,
		},
		"text tool text tool text": {
			`{"choices":[{"delta":{"content":"t1"}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"A","function":{"name":"a","arguments":"{}"}}]}}]}`,
			`{"choices":[{"delta":{"content":"t2"}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"B","function":{"name":"b","arguments":"{}"}}]}}]}`,
			`{"choices":[{"delta":{"content":"t3"}}]}`,
		},
		"thinking after tool": {
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"A","function":{"name":"a","arguments":"{}"}}]}}]}`,
			`{"choices":[{"delta":{"reasoning_content":"r2","content":"t"}}]}`,
		},
		"only usage chunk": {
			`{"usage":{"prompt_tokens":10,"completion_tokens":2}}`,
		},
	}

	for name, chunks := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			rec := streamAnthropicChunks(t, chunks)
			assertStreamInvariants(t, name, parseAnthropicSSE(t, rec.Body.String()))
		})
	}
}

func streamAnthropicChunks(t *testing.T, chunks []string) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	writer := newAnthropicStreamWriter(rec, "m")

	for _, chunk := range chunks {
		if _, err := writer.Write([]byte("data: " + chunk + "\n\n")); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := writer.Write([]byte("data: [DONE]\n\n")); err != nil {
		t.Fatal(err)
	}

	return rec
}

type streamInvariantChecker struct {
	t         *testing.T
	name      string
	open      map[float64]bool
	closed    map[float64]bool
	lastIndex float64
	started   bool
	stopped   bool
}

func assertStreamInvariants(t *testing.T, name string, events []map[string]any) {
	t.Helper()

	checker := &streamInvariantChecker{
		t: t, name: name, open: map[float64]bool{}, closed: map[float64]bool{}, lastIndex: -1,
	}
	for _, event := range events {
		checker.dispatch(event)
	}

	checker.finish()
}

func (c *streamInvariantChecker) dispatch(event map[string]any) {
	index, _ := event["index"].(float64)

	switch event["type"] {
	case "message_start":
		c.messageStart()
	case "content_block_start":
		c.blockStart(index)
	case "content_block_delta":
		c.blockDelta(index)
	case "content_block_stop":
		c.blockStop(index)
	case "message_stop":
		c.stopped = true
	}
}

func (c *streamInvariantChecker) messageStart() {
	if c.started {
		c.t.Errorf("%s: duplicate message_start", c.name)
	}

	c.started = true
}

func (c *streamInvariantChecker) blockStart(index float64) {
	if !c.started || c.open[index] || c.closed[index] || index <= c.lastIndex {
		c.t.Errorf("%s: invalid block start index=%v started=%v", c.name, index, c.started)
	}

	c.open[index] = true
	c.lastIndex = index
}

func (c *streamInvariantChecker) blockDelta(index float64) {
	if !c.open[index] {
		c.t.Errorf("%s: delta for closed block %v", c.name, index)
	}
}

func (c *streamInvariantChecker) blockStop(index float64) {
	if !c.open[index] {
		c.t.Errorf("%s: stop for closed block %v", c.name, index)
	}

	c.open[index] = false
	c.closed[index] = true
}

func (c *streamInvariantChecker) finish() {
	for index, isOpen := range c.open {
		if isOpen {
			c.t.Errorf("%s: block %v left open", c.name, index)
		}
	}

	if !c.started || !c.stopped {
		c.t.Errorf("%s: started=%v stopped=%v", c.name, c.started, c.stopped)
	}
}

// TestAnthropicSDKThinkingBudget enabled thinking 的 budget 必须 >=1024
// 且小于 max_tokens。
func TestAnthropicSDKThinkingBudget(t *testing.T) {
	t.Parallel()

	bad := []string{
		`{"model":"m","max_tokens":4096,"messages":[{"role":"user","content":"hi"}],
			"thinking":{"type":"enabled","budget_tokens":100}}`,
		`{"model":"m","max_tokens":1024,"messages":[{"role":"user","content":"hi"}],
			"thinking":{"type":"enabled","budget_tokens":1024}}`,
	}
	for _, body := range bad {
		if _, _, _, err := anthropicToOpenAI([]byte(body)); err == nil ||
			!strings.Contains(err.Error(), "budget_tokens") {
			t.Errorf("body=%s err=%v want budget validation", body, err)
		}
	}

	good := []byte(`{"model":"m","max_tokens":2048,"messages":[{"role":"user","content":"hi"}],
		"thinking":{"type":"enabled","budget_tokens":1024}}`)
	if _, _, _, err := anthropicToOpenAI(good); err != nil {
		t.Fatalf("valid budget rejected: %v", err)
	}
}

// streamToolArgs 用给定的 arguments 文本跑一次最小流式工具调用，返回 SSE 输出。
func streamToolArgs(t *testing.T, args string) string {
	t.Helper()

	rec := httptest.NewRecorder()
	writer := newAnthropicStreamWriter(rec, "m")

	for _, frame := range []string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"A","function":{"name":"a","arguments":"` +
			args + `"}}]}}]}` + "\n\n",
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n",
		"data: [DONE]\n\n",
	} {
		if _, err := writer.Write([]byte(frame)); err != nil {
			t.Fatalf("args=%q: %v", args, err)
		}
	}

	return rec.Body.String()
}

// TestAnthropicSDKNonObjectToolArguments 上游下发合法但非对象的工具参数
// （null / [] / 数字）时，流式与非流式都必须把 tool_use.input 归一成对象：
// Anthropic SDK 解析出 null 会违反 input 的对象类型。纯空白参数不下发
// input_json_delta（否则 SDK 的 json.loads 抛错）。
func TestAnthropicSDKNonObjectToolArguments(t *testing.T) {
	t.Parallel()

	for _, args := range []string{"null", "[]", "1", " ", "  null  "} {
		out := streamToolArgs(t, args)
		if strings.Contains(out, `"partial_json":"`+args+`"`) {
			t.Fatalf("args=%q: 非对象参数原样下发:\n%s", args, out)
		}

		if !strings.Contains(out, `"type":"tool_use"`) {
			t.Fatalf("args=%q: 缺少 tool_use 块:\n%s", args, out)
		}

		assertStreamInvariants(t, "non-object args "+args, parseAnthropicSSE(t, out))
	}

	// 非流式路径同口径：input 必须是对象。
	msg := anthropicMessageFromOpenAI(map[string]any{
		"choices": []any{map[string]any{
			"finish_reason": "tool_calls",
			"message": map[string]any{"tool_calls": []any{
				map[string]any{"id": "A", "function": map[string]any{"name": "a", "arguments": "null"}},
			}},
		}},
	}, "m")

	blocks, _ := msg["content"].([]any)
	if len(blocks) != 1 {
		t.Fatalf("content=%v", msg["content"])
	}

	block, _ := blocks[0].(map[string]any)
	if _, ok := block["input"].(map[string]any); !ok {
		t.Fatalf("input=%#v want object", block["input"])
	}
}

// TestAnthropicSDKInvalidMediaTypes 官方 media_type 枚举校验。
func TestAnthropicSDKInvalidMediaTypes(t *testing.T) {
	t.Parallel()

	bodies := []string{
		`{"model":"m","messages":[{"role":"user","content":[{"type":"image",
			"source":{"type":"base64","media_type":"image/svg+xml","data":"abc"}}]}]}`,
		`{"model":"m","messages":[{"role":"user","content":[{"type":"document",
			"source":{"type":"base64","media_type":"text/plain","data":"abc"}}]}]}`,
		`{"model":"m","messages":[{"role":"user","content":[{"type":"document",
			"source":{"type":"text","media_type":"text/html","data":"abc"}}]}]}`,
	}
	for _, body := range bodies {
		if _, _, _, err := anthropicToOpenAI([]byte(body)); err == nil ||
			!strings.Contains(err.Error(), "media_type") {
			t.Errorf("body=%s err=%v want media_type validation", body, err)
		}
	}
}

// streamAnthropicThroughUpstream 走真实上游透传层（StreamHint）把帧喂给
// Anthropic 流写入器：白名单重建、name/id 收敛、空流/截断兜底都在该层。
func streamAnthropicThroughUpstream(t *testing.T, frames []string) string {
	t.Helper()

	rec := httptest.NewRecorder()
	writer := newAnthropicStreamWriter(rec, "m")

	src := strings.Join(append(frames, "data: [DONE]"), "\n\n") + "\n\n"

	if err := upstream.StreamHint(writer, strings.NewReader(src), nil); err != nil {
		t.Fatal(err)
	}

	return rec.Body.String()
}

// TestAnthropicSDKLateNameThroughUpstream 首片不带 name 时 name 只能来自后续分片：
// 上游层的 name 收敛不能把它删掉，否则整块 tool_use 会被丢弃。
func TestAnthropicSDKLateNameThroughUpstream(t *testing.T) {
	t.Parallel()

	out := streamAnthropicThroughUpstream(t, []string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"A","function":{"arguments":"{\"x\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"a","arguments":"1}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	})

	if !strings.Contains(out, `"type":"tool_use"`) || !strings.Contains(out, `"name":"a"`) {
		t.Fatalf("tool_use 整块丢失:\n%s", out)
	}

	_, args := collectAnthropicToolBlocks(parseAnthropicSSE(t, out))
	if args[0] != `{"x":1}` {
		t.Fatalf("args=%q want {\"x\":1}", args[0])
	}
}

// TestAnthropicSDKPingThroughUpstream 无 choices 的帧（ping / 上游透传的非内容帧）
// 不能触发 message_start：上游层补的占位 id 会顶掉真实 id。
func TestAnthropicSDKPingThroughUpstream(t *testing.T) {
	t.Parallel()

	out := streamAnthropicThroughUpstream(t, []string{
		`data: {"type":"ping"}`,
		`data: {"id":"c1","choices":[{"delta":{"content":"hi"}}]}`,
	})

	if !strings.Contains(out, `"type":"message_start"`) {
		t.Fatalf("缺少 message_start:\n%s", out)
	}

	if !strings.Contains(out, `"id":"c1"`) {
		t.Fatalf("message_start 用了占位 id:\n%s", out)
	}

	assertStreamInvariants(t, "ping through upstream", parseAnthropicSSE(t, out))
}
