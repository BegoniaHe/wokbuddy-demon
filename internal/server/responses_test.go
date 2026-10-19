package server

import (
	"encoding/json"
	"errors"
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
		"parallel_tool_calls":false,
		"presence_penalty":0.5,
		"frequency_penalty":0.25,
		"text":{"format":{"type":"json_schema","name":"out","schema":{"type":"object"},"strict":true}},
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
	assertResponsesParams(t, got)
}

func assertResponsesParams(t *testing.T, got map[string]any) {
	t.Helper()

	if got["parallel_tool_calls"] != false {
		t.Errorf("parallel_tool_calls=%v", got["parallel_tool_calls"])
	}

	format, _ := got["response_format"].(map[string]any)
	if format["type"] != "json_schema" || format["json_schema"].(map[string]any)["name"] != "out" {
		t.Errorf("response_format=%v", format)
	}

	if got["presence_penalty"] != 0.5 || got["frequency_penalty"] != 0.25 {
		t.Errorf("penalties=%v", got)
	}
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

func TestResponsesRequestIgnoresUnknownItem(t *testing.T) {
	t.Parallel()

	body := `{"model":"m","input":[
		{"type":"message","role":"user","content":"hi"},
		{"type":"web_search_call","id":"ws_1","status":"completed"},
		{"type":"compaction","encrypted_content":"xx"}
	]}`

	raw, _, _, _, err := responsesToOpenAI([]byte(body))
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	msgs, _ := got["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages=%d want 1（未知 item 应被忽略）: %v", len(msgs), msgs)
	}
}

func TestResponsesRequestCustomToolRoundTrip(t *testing.T) {
	t.Parallel()

	body := `{
		"model":"m",
		"input":[
			{"type":"message","role":"user","content":"hi"},
			{"type":"custom_tool_call","call_id":"c1","name":"apply_patch","input":"*** Begin Patch"},
			{"type":"custom_tool_call_output","call_id":"c1","output":"done"}
		],
		"tools":[{"type":"custom","name":"apply_patch","description":"patch","format":{"type":"grammar","syntax":"lark","definition":"start: patch"}}]
	}`

	raw, _, _, _, err := responsesToOpenAI([]byte(body))
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	msgs, _ := got["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages=%d want 3: %v", len(msgs), msgs)
	}

	call := msgs[1].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	if call["function"].(map[string]any)["arguments"] != `{"input":"*** Begin Patch"}` {
		t.Errorf("custom call=%v", call)
	}

	if tool := msgs[2].(map[string]any); tool["role"] != "tool" || tool["content"] != "done" {
		t.Errorf("custom tool output=%v", tool)
	}

	tools := got["tools"].([]any)

	params := tools[0].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)
	if params["properties"].(map[string]any)["input"] == nil {
		t.Errorf("custom tool parameters=%v", params)
	}
}

func TestResponsesFunctionOutputImage(t *testing.T) {
	t.Parallel()

	body := `{
		"model":"m",
		"input":[
			{"type":"message","role":"user","content":"look"},
			{"type":"function_call","call_id":"c1","name":"view_image","arguments":"{}"},
			{"type":"function_call_output","call_id":"c1","output":[
				{"type":"input_image","image_url":"data:image/png;base64,abc","detail":"original"}
			]}
		]
	}`

	raw, _, _, _, err := responsesToOpenAI([]byte(body))
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	msgs, _ := got["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages=%d want 4: %v", len(msgs), msgs)
	}

	tool := msgs[2].(map[string]any)
	if tool["role"] != "tool" || tool["content"] != "" {
		t.Errorf("tool=%v", tool)
	}

	image := msgs[3].(map[string]any)
	if image["role"] != "user" {
		t.Fatalf("image message=%v", image)
	}

	parts, _ := image["content"].([]any)

	imageURL := parts[0].(map[string]any)["image_url"].(map[string]any)
	if imageURL["url"] != "data:image/png;base64,abc" || imageURL["detail"] != "high" {
		t.Errorf("image=%v", imageURL)
	}
}

func TestResponsesRequestRejectsNullInput(t *testing.T) {
	t.Parallel()

	body := `{"model":"m","input":null}`
	if _, _, _, _, err := responsesToOpenAI([]byte(body)); !errors.Is(err, errInputIsRequired) {
		t.Fatalf("err=%v want %v", err, errInputIsRequired)
	}
}

func TestResponsesRequestIgnoresReasoningItem(t *testing.T) {
	t.Parallel()

	body := `{
		"model":"m",
		"instructions":"sys",
		"input":[
			{"type":"reasoning","id":"rs_0","summary":[{"type":"summary_text","text":"plan"}]},
			{"type":"message","role":"user","content":"hi"},
			{"type":"function_call","call_id":"c1","name":"lookup","arguments":"{\"q\":1}"},
			{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"think"}]},
			{"type":"function_call_output","call_id":"c1","output":"ok"}
		]
	}`

	raw, _, _, _, err := responsesToOpenAI([]byte(body))
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	msgs, _ := got["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages=%d want 4 (reasoning 应被忽略): %v", len(msgs), msgs)
	}

	assertResponsesMessages(t, msgs)
}

func TestResponsesRequestRejectsReasoningOnlyInput(t *testing.T) {
	t.Parallel()

	body := `{"model":"m","input":[{"type":"reasoning","id":"rs_1","summary":[]}]}`
	if _, _, _, _, err := responsesToOpenAI([]byte(body)); !errors.Is(err, errNoConvertibleItems) {
		t.Fatalf("reasoning-only input error = %v, want %v", err, errNoConvertibleItems)
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

	assertResponsesRequiredFields(t, resp, 5)
}

// assertResponsesRequiredFields 校验 SDK 要求存在的基础字段与 usage 明细。
func assertResponsesRequiredFields(t *testing.T, resp map[string]any, total int) {
	t.Helper()

	usage, _ := resp["usage"].(map[string]any)
	if usage["total_tokens"] != total {
		t.Errorf("usage=%v", usage)
	}

	if usage["input_tokens_details"] == nil || usage["output_tokens_details"] == nil {
		t.Errorf("usage details missing: %v", usage)
	}

	if resp["tools"] == nil || resp["tool_choice"] != "auto" || resp["parallel_tool_calls"] != true {
		t.Errorf("required response fields=%v", resp)
	}
}

// TestResponsesFunctionItemsAggregatedShape 非流式聚合产出 []map[string]any，
// 不能因为类型断言失败而丢掉 tool_calls。
func TestResponsesFunctionItemsAggregatedShape(t *testing.T) {
	t.Parallel()

	items := responsesFunctionItems([]map[string]any{{
		"id": "call_1", "type": "function",
		"function": map[string]any{"name": "lookup", "arguments": `{"q":1}`},
	}}, nil)

	if len(items) != 1 {
		t.Fatalf("items=%v", items)
	}

	if items[0].(map[string]any)["name"] != "lookup" {
		t.Errorf("item=%v", items[0])
	}
}

// TestResponsesCustomToolOutput custom 工具调用还原为 custom_tool_call。
func TestResponsesCustomToolOutput(t *testing.T) {
	t.Parallel()

	body := map[string]any{
		"choices": []any{map[string]any{
			"message": map[string]any{
				"content": "",
				"tool_calls": []any{map[string]any{
					"id": "call_1", "type": "function",
					"function": map[string]any{"name": "apply_patch", "arguments": `{"input":"*** Begin Patch"}`},
				}},
			},
			"finish_reason": "tool_calls",
		}},
	}

	resp := responsesFromOpenAI(body, "m", "", map[string]bool{"apply_patch": true})
	output := resp["output"].([]any)

	item := output[0].(map[string]any)
	if item["type"] != "custom_tool_call" || item["input"] != "*** Begin Patch" {
		t.Fatalf("item=%v", item)
	}
}

func TestResponsesStreamWriter(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	w := newResponsesStreamWriter(rec, "glm-5.2", "sys", nil)

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
		`"sequence_number":0`, `"logprobs":[]`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stream missing %q:\n%s", want, out)
		}
	}
}

// TestResponsesStreamWriterCustomTool custom 工具流式输出使用
// custom_tool_call / custom_tool_call_input.done，且 input 解开 JSON 包装。
func TestResponsesStreamWriterCustomTool(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	w := newResponsesStreamWriter(rec, "m", "", map[string]bool{"apply_patch": true})

	chunks := `data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"apply_patch","arguments":"{\"input\":\"patch\"}"}}]}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
		"data: [DONE]\n\n"

	if _, err := w.Write([]byte(chunks)); err != nil {
		t.Fatal(err)
	}

	out := rec.Body.String()
	for _, want := range []string{
		`"type":"custom_tool_call"`,
		"event: response.custom_tool_call_input.done", `"input":"patch"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stream missing %q:\n%s", want, out)
		}
	}

	if strings.Contains(out, "response.function_call_arguments") {
		t.Errorf("custom 工具不应出现 function 参数事件:\n%s", out)
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
