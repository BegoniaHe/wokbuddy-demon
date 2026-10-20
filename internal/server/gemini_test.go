package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

func TestGeminiToOpenAIRequest(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"systemInstruction":{"parts":[{"text":"sys"}]},
		"contents":[
			{"role":"user","parts":[
				{"text":"hi"},
				{"inlineData":{"mimeType":"image/png","data":"abc"}}
			]},
			{"role":"model","parts":[
				{"text":"calling"},
				{"functionCall":{"name":"lookup","args":{"q":1}}}
			]},
			{"role":"user","parts":[
				{"functionResponse":{"name":"lookup","response":{"ok":true}}}
			]}
		],
		"generationConfig":{
			"temperature":0.2,"maxOutputTokens":128,"stopSequences":["STOP"],
			"presencePenalty":0.1,"frequencyPenalty":0.2,
			"responseMimeType":"application/json","responseSchema":{"type":"object"},
			"thinkingConfig":{"thinkingLevel":"HIGH"}
		},
		"tools":[{"functionDeclarations":[{"name":"lookup","description":"d","parameters":{"type":"OBJECT","properties":{"q":{"type":"STRING"}}}}]}],
		"toolConfig":{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["lookup"]}}
	}`)

	raw, err := geminiToOpenAI(body, "glm-5.2", true)
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	if got["model"] != "glm-5.2" || got["stream"] != true {
		t.Errorf("routing=%v", got)
	}

	assertGeminiRequestParams(t, got)
	assertGeminiRequestMessages(t, got)
}

func assertGeminiRequestParams(t *testing.T, got map[string]any) {
	t.Helper()

	if got["max_tokens"] != float64(128) || got["temperature"] != 0.2 || got["top_p"] != nil {
		t.Errorf("sampling=%v", got)
	}

	if got["presence_penalty"] != 0.1 || got["frequency_penalty"] != 0.2 {
		t.Errorf("penalties=%v", got)
	}

	if got["reasoning_effort"] != "high" {
		t.Errorf("reasoning_effort=%v", got["reasoning_effort"])
	}

	format, _ := got["response_format"].(map[string]any)
	if format["type"] != "json_schema" {
		t.Errorf("response_format=%v", format)
	}

	choice, _ := got["tool_choice"].(map[string]any)
	if choice["type"] != "function" {
		t.Errorf("tool_choice=%v", got["tool_choice"])
	}

	tools, _ := got["tools"].([]any)

	params := tools[0].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)
	if params["type"] != "object" {
		t.Errorf("tool schema type=%v", params["type"])
	}
}

func assertGeminiRequestMessages(t *testing.T, got map[string]any) {
	t.Helper()

	msgs, _ := got["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages=%d want 4: %v", len(msgs), msgs)
	}

	assertGeminiSystemUser(t, msgs)
	assertGeminiToolRoundTrip(t, msgs)
}

func assertGeminiSystemUser(t *testing.T, msgs []any) {
	t.Helper()

	system := msgs[0].(map[string]any)
	if system["role"] != "system" || system["content"] != "sys" {
		t.Errorf("system=%v", system)
	}

	user := msgs[1].(map[string]any)

	parts, _ := user["content"].([]any)
	if len(parts) != 2 || parts[1].(map[string]any)["type"] != "image_url" {
		t.Errorf("user=%v", user)
	}
}

func assertGeminiToolRoundTrip(t *testing.T, msgs []any) {
	t.Helper()

	assistant := msgs[2].(map[string]any)

	calls, _ := assistant["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("assistant=%v", assistant)
	}

	call := calls[0].(map[string]any)
	if call["id"] != "lookup" || call["function"].(map[string]any)["arguments"] != `{"q":1}` {
		t.Errorf("tool_call=%v", call)
	}

	tool := msgs[3].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "lookup" || tool["content"] != `{"ok":true}` {
		t.Errorf("tool=%v", tool)
	}
}

func TestGeminiJSONSchemaNormalization(t *testing.T) {
	t.Parallel()

	got := geminiJSONSchema(map[string]any{
		"type":             "OBJECT",
		"nullable":         true,
		"propertyOrdering": []any{"x"},
		"properties": map[string]any{
			"x": map[string]any{"type": "STRING", "items": map[string]any{"type": "INTEGER"}},
		},
	})

	schema, _ := got.(map[string]any)
	if schema["type"] != "object" {
		t.Fatalf("schema=%v", schema)
	}

	if _, ok := schema["nullable"]; ok {
		t.Errorf("nullable should be removed: %v", schema)
	}

	props, _ := schema["properties"].(map[string]any)

	x, _ := props["x"].(map[string]any)
	if x["type"] != "string" || x["items"].(map[string]any)["type"] != "integer" {
		t.Errorf("nested schema=%v", x)
	}
}

// TestGeminiFunctionPartsAggregatedShape 非流式聚合产出 []map[string]any。
func TestGeminiFunctionPartsAggregatedShape(t *testing.T) {
	t.Parallel()

	parts := geminiFunctionParts([]map[string]any{{
		"id": "call_1", "type": "function",
		"function": map[string]any{"name": "lookup", "arguments": `{"q":1}`},
	}})

	if len(parts) != 1 {
		t.Fatalf("parts=%v", parts)
	}

	call := parts[0].(map[string]any)["functionCall"].(map[string]any)
	if call["name"] != "lookup" {
		t.Errorf("call=%v", call)
	}
}

// TestGeminiUsageVisibleOutput Gemini 的 candidatesTokenCount 只含可见输出，
// thinking 单独放进 thoughtsTokenCount，避免客户端重复累加。
func TestGeminiUsageVisibleOutput(t *testing.T) {
	t.Parallel()

	usage := geminiUsage(map[string]any{
		"prompt_tokens":     float64(10),
		"completion_tokens": float64(7),
		"total_tokens":      float64(17),
		"completion_tokens_details": map[string]any{
			"reasoning_tokens": float64(3),
		},
	})

	if usage["candidatesTokenCount"] != 4 || usage["thoughtsTokenCount"] != 3 {
		t.Fatalf("usage=%v", usage)
	}

	if usage["totalTokenCount"] != 17 {
		t.Errorf("total=%v", usage["totalTokenCount"])
	}
}

func TestGeminiEndpointNonStream(t *testing.T) {
	t.Parallel()

	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	handler := NewHandler(&Config{Pool: p, Upstream: up})

	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/glm-5.2:generateContent",
		strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}

	candidates, _ := resp["candidates"].([]any)
	if len(candidates) != 1 {
		t.Fatalf("candidates=%v", resp)
	}

	candidate := candidates[0].(map[string]any)
	if candidate["finishReason"] != "STOP" {
		t.Errorf("finishReason=%v", candidate["finishReason"])
	}

	content := candidate["content"].(map[string]any)

	parts, _ := content["parts"].([]any)
	if len(parts) != 1 || parts[0].(map[string]any)["text"] != "你好" {
		t.Errorf("parts=%v", parts)
	}

	usage, _ := resp["usageMetadata"].(map[string]any)
	if usage["totalTokenCount"] != float64(2) {
		t.Errorf("usage=%v", usage)
	}
}

func TestGeminiEndpointStream(t *testing.T) {
	t.Parallel()

	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	handler := NewHandler(&Config{Pool: p, Upstream: up})

	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/glm-5.2:streamGenerateContent?alt=sse",
		strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	out := rec.Body.String()
	if strings.Contains(out, "[DONE]") {
		t.Fatalf("Gemini SSE 不应包含 [DONE]:\n%s", out)
	}

	for _, want := range []string{`"text":"你好"`, `"finishReason":"STOP"`, `"totalTokenCount":2`} {
		if !strings.Contains(out, want) {
			t.Errorf("stream missing %q:\n%s", want, out)
		}
	}
}

func TestGeminiCountTokens(t *testing.T) {
	t.Parallel()

	handler := NewHandler(&Config{})
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/glm-5.2:countTokens",
		strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hello world"}]}]}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}

	if tokens, _ := resp["totalTokens"].(float64); tokens < 1 {
		t.Errorf("totalTokens=%v", resp["totalTokens"])
	}
}

func TestGeminiXGoogAPIKeyAuth(t *testing.T) {
	t.Parallel()

	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	handler := NewHandler(&Config{APIKey: "secret", Pool: p, Upstream: up})

	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/glm-5.2:generateContent",
		strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
	req.Header.Set("X-Goog-Api-Key", "secret")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("x-goog-api-key code=%d body=%s", rec.Code, rec.Body)
	}
}

// TestGeminiStreamModelNotFoundStatus 流式模型不存在应与非流式 404 同口径，
// 不能退化成 400 INVALID_ARGUMENT。
func TestGeminiStreamModelNotFoundStatus(t *testing.T) {
	t.Parallel()

	if got := geminiStreamErrorStatus(upstream.ErrModelNotFound); got != http.StatusNotFound {
		t.Fatalf("status=%d want 404", got)
	}
}

// TestGeminiStreamRequestLevelError 流式请求级错误应返回 400 INVALID_ARGUMENT，
// 不能把内容拦截/上下文超限伪装成可重试的 UNAVAILABLE。
func TestGeminiStreamRequestLevelError(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	w := newGeminiStreamWriter(rec, "m")

	raw := `data: {"error":{"code":400,"message":"blocked by security policy"}}` + "\n\n"
	if _, err := w.Write([]byte(raw)); err != nil {
		t.Fatal(err)
	}

	out := rec.Body.String()
	for _, want := range []string{`"code":400`, `"status":"INVALID_ARGUMENT"`} {
		if !strings.Contains(out, want) {
			t.Errorf("stream error missing %q:\n%s", want, out)
		}
	}

	if strings.Contains(out, "UNAVAILABLE") {
		t.Errorf("请求级错误不应返回 UNAVAILABLE:\n%s", out)
	}
}
