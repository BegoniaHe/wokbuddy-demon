package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

func TestResponsesRequestMapping(t *testing.T) {
	t.Parallel()

	body := `{
		"model":"cn:glm-5.2",
		"instructions":"sys",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
			{"type":"function_call","call_id":"c1","name":"lookup","arguments":"{\"q\":1}"},
			{"type":"function_call_output","call_id":"c1","output":"ok"}
		],
		"tools":[{"type":"function","name":"lookup","description":"d","parameters":{"type":"object"}}],
		"tool_choice":{"type":"function","name":"lookup"},
		"max_output_tokens":128,
		"stream":true
	}`

	raw, model, instructions, stream, err := responsesToOpenAI([]byte(body))
	if err != nil {
		t.Fatal(err)
	}

	if model != "cn:glm-5.2" || instructions != "sys" || !stream {
		t.Fatalf("model=%q instructions=%q stream=%v", model, instructions, stream)
	}

	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	if got["max_tokens"] != float64(128) || got["stream"] != true {
		t.Errorf("params=%v", got)
	}

	msgs, _ := got["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages=%d want 4: %v", len(msgs), msgs)
	}

	assertResponsesMessages(t, msgs)
	assertResponsesTools(t, got)
}

func assertResponsesMessages(t *testing.T, msgs []any) {
	t.Helper()
	assertResponsesSystemUser(t, msgs)
	assertResponsesToolRoundTrip(t, msgs)
}

func assertResponsesSystemUser(t *testing.T, msgs []any) {
	t.Helper()

	sys := msgs[0].(map[string]any)
	user := msgs[1].(map[string]any)

	if sys["role"] != "system" || sys["content"] != "sys" {
		t.Errorf("system=%v", sys)
	}

	if user["role"] != "user" || user["content"] != "hi" {
		t.Errorf("user=%v", user)
	}
}

func assertResponsesToolRoundTrip(t *testing.T, msgs []any) {
	t.Helper()

	assistant := msgs[2].(map[string]any)
	tool := msgs[3].(map[string]any)

	calls, _ := assistant["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool_calls=%v", assistant)
	}

	fn := calls[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "lookup" || fn["arguments"] != `{"q":1}` {
		t.Errorf("function=%v", fn)
	}

	if tool["role"] != "tool" || tool["tool_call_id"] != "c1" || tool["content"] != "ok" {
		t.Errorf("tool=%v", tool)
	}
}

func assertResponsesTools(t *testing.T, got map[string]any) {
	t.Helper()

	tools, _ := got["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools=%v", got["tools"])
	}

	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "lookup" || fn["description"] != "d" {
		t.Errorf("tool function=%v", fn)
	}

	choice := got["tool_choice"].(map[string]any)
	if choice["type"] != "function" {
		t.Errorf("tool_choice=%v", choice)
	}
}

func TestResponsesRequestRejectsUnknownItem(t *testing.T) {
	t.Parallel()

	body := `{"model":"m","input":[{"type":"custom_tool_call"}]}`
	if _, _, _, _, err := responsesToOpenAI([]byte(body)); err == nil {
		t.Fatal("unknown input item should fail")
	}
}

func TestResponsesFromOpenAI(t *testing.T) {
	t.Parallel()

	body := map[string]any{
		"id": "chatcmpl-1", "created": float64(1753600000),
		"choices": []any{map[string]any{
			"message": map[string]any{
				"role": "assistant", "content": "hello",
				"tool_calls": []any{map[string]any{
					"id": "call_1", "type": "function",
					"function": map[string]any{"name": "lookup", "arguments": `{"q":1}`},
				}},
			},
			"finish_reason": "tool_calls",
		}},
		"usage": map[string]any{"prompt_tokens": float64(3), "completion_tokens": float64(2)},
	}

	resp := responsesFromOpenAI(body, "glm-5.2", "sys")
	if resp["object"] != "response" || resp["output_text"] != "hello" {
		t.Fatalf("response=%v", resp)
	}

	output, _ := resp["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("output=%v", output)
	}

	if output[0].(map[string]any)["type"] != "message" || output[1].(map[string]any)["type"] != "function_call" {
		t.Errorf("output types=%v", output)
	}

	usage, _ := resp["usage"].(map[string]any)
	if usage["total_tokens"] != 5 {
		t.Errorf("usage=%v", usage)
	}
}

func TestResponsesStreamWriter(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	w := newResponsesStreamWriter(rec, "glm-5.2", "sys")

	chunks := `data: {"id":"chatcmpl-1","created":1753600000,"choices":[{"index":0,"delta":{"content":"he"}}]}` + "\n\n" +
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"llo"}}]}` + "\n\n" +
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"lookup","arguments":"{\"q\":"}}]}}]}` + "\n\n" +
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}` + "\n\n" +
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":2,"completion_tokens":3}}` + "\n\n" +
		"data: [DONE]\n\n"
	if _, err := w.Write([]byte(chunks)); err != nil {
		t.Fatal(err)
	}

	out := rec.Body.String()
	for _, want := range []string{
		"event: response.created", "event: response.output_item.added",
		"event: response.output_text.delta", `"delta":"he"`,
		"event: response.function_call_arguments.delta",
		"event: response.function_call_arguments.done", `"arguments":"{\"q\":1}"`,
		"event: response.output_item.done", "event: response.completed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stream missing %q:\n%s", want, out)
		}
	}
}

func TestResponsesEndpointNonStream(t *testing.T) {
	t.Parallel()
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(&Config{Pool: p, Upstream: up})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"glm-5.2","input":"hi"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}

	if resp["output_text"] != "你好" {
		t.Errorf("output_text=%v", resp["output_text"])
	}
}

func TestResponsesEndpointStream(t *testing.T) {
	t.Parallel()
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(&Config{Pool: p, Upstream: up})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"glm-5.2","input":"hi","stream":true}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	out := rec.Body.String()
	if !strings.Contains(out, "event: response.created") || !strings.Contains(out, "event: response.completed") {
		t.Fatalf("missing responses events:\n%s", out)
	}

	if !strings.Contains(out, `"delta":"你好"`) {
		t.Errorf("missing text delta:\n%s", out)
	}
}
