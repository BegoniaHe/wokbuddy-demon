package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// TestAnthropicToOpenAIRequest system/messages/tool_use/tool_result 映射。
func TestAnthropicToOpenAIRequest(t *testing.T) {
	body := []byte(`{
		"model":"claude-sonnet-4-5",
		"max_tokens":512,
		"system":[{"type":"text","text":"sys-a"},{"type":"text","text":"sys-b"}],
		"messages":[
			{"role":"user","content":"hello"},
			{"role":"assistant","content":[
				{"type":"text","text":"calling"},
				{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"SF"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"sunny"}]},
				{"type":"text","text":"thanks"}
			]}
		],
		"stop_sequences":["END"],
		"stream":true
	}`)
	raw, model, stream, err := anthropicToOpenAI(body)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if model != "claude-sonnet-4-5" || !stream {
		t.Fatalf("model=%q stream=%v", model, stream)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if got["max_tokens"] != float64(512) {
		t.Errorf("max_tokens=%v", got["max_tokens"])
	}
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 5 {
		t.Fatalf("messages=%d want 5: %v", len(msgs), msgs)
	}
	assertAnthropicSystem(t, msgs[0])
	assertAnthropicToolRoundTrip(t, msgs)
}

// assertAnthropicSystem 校验 system 消息拼接。
func assertAnthropicSystem(t *testing.T, raw any) {
	t.Helper()
	sys := raw.(map[string]any)
	if sys["role"] != "system" || sys["content"] != "sys-a\n\nsys-b" {
		t.Errorf("system=%v", sys)
	}
}

// assertAnthropicToolRoundTrip 校验 assistant tool_use → tool_calls 与 tool_result → tool。
func assertAnthropicToolRoundTrip(t *testing.T, msgs []any) {
	t.Helper()
	assistant := msgs[2].(map[string]any)
	calls, _ := assistant["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool_calls=%v", assistant["tool_calls"])
	}
	fn := calls[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "get_weather" || fn["arguments"] != `{"city":"SF"}` {
		t.Errorf("function=%v", fn)
	}
	toolMsg := msgs[3].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "toolu_1" || toolMsg["content"] != "sunny" {
		t.Errorf("tool message=%v", toolMsg)
	}
	if msgs[4].(map[string]any)["content"] != "thanks" {
		t.Errorf("trailing user message=%v", msgs[4])
	}
}

// TestAnthropicToOpenAITools tools / tool_choice 映射。
func TestAnthropicToOpenAITools(t *testing.T) {
	body := []byte(`{
		"model":"glm-5.2",
		"max_tokens":64,
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"name":"get_weather","description":"weather","input_schema":{"type":"object"}}],
		"tool_choice":{"type":"tool","name":"get_weather"}
	}`)
	raw, _, _, err := anthropicToOpenAI(body)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	tools, _ := got["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools=%v", got["tools"])
	}
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "get_weather" || fn["description"] != "weather" {
		t.Errorf("tool function=%v", fn)
	}
	choice := got["tool_choice"].(map[string]any)
	if choice["type"] != "function" {
		t.Errorf("tool_choice=%v", choice)
	}
}

// TestAnthropicNonStreamResponse 非流式文本 + usage 映射。
func TestAnthropicNonStreamResponse(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{Pool: p, Upstream: up})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"glm-5.2","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var msg map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if msg["type"] != "message" || msg["role"] != "assistant" {
		t.Fatalf("message=%v", msg)
	}
	content := msg["content"].([]any)
	if len(content) == 0 || content[0].(map[string]any)["text"] != "你好" {
		t.Fatalf("content=%v", content)
	}
	if msg["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason=%v", msg["stop_reason"])
	}
	usage := msg["usage"].(map[string]any)
	if usage["input_tokens"] != float64(1) || usage["output_tokens"] != float64(1) {
		t.Errorf("usage=%v", usage)
	}
}

// TestAnthropicStreamText 流式文本：message_start / text_delta / message_stop。
func TestAnthropicStreamText(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{Pool: p, Upstream: up})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"glm-5.2","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	out := rec.Body.String()
	for _, want := range []string{
		"event: message_start", `"type":"message"`,
		"event: content_block_start", `"type":"text"`,
		"event: content_block_delta", `"text":"你好"`,
		"event: content_block_stop", `"index":0`,
		"event: message_delta", `"stop_reason":"end_turn"`,
		"event: message_stop",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stream missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, `"object":"chat.completion.chunk"`) {
		t.Fatalf("OpenAI 帧泄漏到 Anthropic 流:\n%s", out)
	}
}

// TestAnthropicStreamToolUse tool_calls → tool_use 块 + input_json_delta。
func TestAnthropicStreamToolUse(t *testing.T) {
	const toolSSE = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"{\\\"city\\\":\"}}]}}]}\n\n" +
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"SF\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, toolSSE, true })
	h := NewHandler(Config{Pool: p, Upstream: up})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"glm-5.2","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"weather"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	out := rec.Body.String()
	for _, want := range []string{
		`"type":"tool_use"`, `"id":"call_1"`, `"name":"get_weather"`,
		`"type":"input_json_delta"`, `"partial_json":"{\"city\":"`,
		`"stop_reason":"tool_use"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stream missing %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "content_block_start") < 1 {
		t.Errorf("no content block:\n%s", out)
	}
}

// TestAnthropicCountTokens count_tokens 返回 Anthropic 形态 200。
func TestAnthropicCountTokens(t *testing.T) {
	h := NewHandler(Config{})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hello world"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if n, _ := resp["input_tokens"].(float64); n < 1 {
		t.Fatalf("input_tokens=%v", resp["input_tokens"])
	}
}

// TestAnthropicAuthErrorShape 鉴权失败使用 Anthropic 错误体。
func TestAnthropicAuthErrorShape(t *testing.T) {
	h := NewHandler(Config{APIKey: "secret"})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if resp["type"] != "error" {
		t.Fatalf("body=%v", resp)
	}
	e := resp["error"].(map[string]any)
	if e["type"] != "authentication_error" {
		t.Fatalf("error.type=%v", e["type"])
	}
}

// TestAnthropicMessageFromOpenAIToolUse 非流式 tool_calls → tool_use 块与 stop_reason。
func TestAnthropicMessageFromOpenAIToolUse(t *testing.T) {
	body := map[string]any{
		"id": "chatcmpl-1",
		"choices": []any{map[string]any{
			"finish_reason": "tool_calls",
			"message": map[string]any{
				"content": "calling",
				"tool_calls": []any{map[string]any{
					"id": "call_1", "type": "function",
					"function": map[string]any{"name": "get_weather", "arguments": `{"city":"SF"}`},
				}},
			},
		}},
		"usage": map[string]any{"prompt_tokens": 3, "completion_tokens": 4},
	}
	msg := anthropicMessageFromOpenAI(body, "claude-sonnet-4-5")
	if msg["stop_reason"] != "tool_use" {
		t.Fatalf("stop_reason=%v", msg["stop_reason"])
	}
	content := msg["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("content=%v", content)
	}
	tool := content[1].(map[string]any)
	if tool["type"] != "tool_use" || tool["name"] != "get_weather" {
		t.Fatalf("tool block=%v", tool)
	}
	input := tool["input"].(map[string]any)
	if input["city"] != "SF" {
		t.Fatalf("input=%v", input)
	}
}

// TestAnthropicStreamErrorKeepsGatewayHint 流式上游 error 帧的 gateway_hint
// 必须保留在 Anthropic error 事件里，不能被协议转换丢掉。
func TestAnthropicStreamErrorKeepsGatewayHint(t *testing.T) {
	rec := httptest.NewRecorder()
	tw := newAnthropicStreamWriter(rec, "m")
	if err := tw.handleChunk(`{"error":{"message":"rate limited","gateway_hint":"wait 30s"}}`); err != nil {
		t.Fatalf("handleChunk: %v", err)
	}
	out := rec.Body.String()
	if !strings.Contains(out, "event: error") || !strings.Contains(out, `"gateway_hint":"wait 30s"`) {
		t.Fatalf("流式错误丢失 gateway_hint:\n%s", out)
	}
}
