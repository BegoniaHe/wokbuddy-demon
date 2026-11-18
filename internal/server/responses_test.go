package server

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/jsonx"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
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
	if err := jsonx.Unmarshal(raw, &got); err != nil {
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

// TestResponsesRequestDefaultsMaxOutputTokens Codex 0.162+ 不再携带
// max_output_tokens；网关必须按模型目录补全，否则上游回落到 32k 输出预算。
func TestResponsesRequestDefaultsMaxOutputTokens(t *testing.T) {
	t.Parallel()

	body := `{"model":"cn:deepseek-v4.1-flash","input":"hi"}`

	raw, _, _, _, err := responsesToOpenAI([]byte(body))
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := jsonx.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	if got["max_tokens"] != float64(384000) {
		t.Errorf("max_tokens=%v want 384000", got["max_tokens"])
	}
}

// TestResponsesAliasDefaultMaxOutputTokens 别名解析后同样按目标模型补全。
func TestResponsesAliasDefaultMaxOutputTokens(t *testing.T) {
	t.Parallel()

	body := `{"model":"codex","input":"hi"}`

	raw, _, _, _, err := responsesToOpenAIWithClient(
		[]byte(body), nil, map[string]string{"codex": "cn:deepseek-v4.1-flash"},
	)
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := jsonx.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	if got["max_tokens"] != float64(384000) {
		t.Errorf("max_tokens=%v want 384000", got["max_tokens"])
	}
}

// TestResponsesRequestUnknownModelNoDefaultMaxTokens 未知模型不编造输出上限。
func TestResponsesRequestUnknownModelNoDefaultMaxTokens(t *testing.T) {
	t.Parallel()

	body := `{"model":"unknown-model","input":"hi"}`

	raw, _, _, _, err := responsesToOpenAI([]byte(body))
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := jsonx.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	if _, ok := got["max_tokens"]; ok {
		t.Errorf("unknown model should not get max_tokens: %v", got["max_tokens"])
	}
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
		{"type":"compaction","encrypted_content":"xX9f_-Qm9zLpT0aV3rYb8cN2dE5fG7hJ1kM4nP6qR8sT"}
	]}`

	raw, _, _, _, err := responsesToOpenAI([]byte(body))
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := jsonx.Unmarshal(raw, &got); err != nil {
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
	if err := jsonx.Unmarshal(raw, &got); err != nil {
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

// TestResponsesAdditionalTools Codex 0.162+ 把工具放在 input.additional_tools，
// 顶层 tools 为空；转换后必须合并进 chat tools，custom 工具也要被识别。
func TestResponsesAdditionalTools(t *testing.T) {
	t.Parallel()

	body := `{
		"model":"m",
		"input":[
			{"type":"additional_tools","role":"developer","tools":[
				{"type":"function","name":"exec_command","parameters":{"type":"object"}},
				{"type":"custom","name":"apply_patch","description":"patch"}
			]},
			{"type":"message","role":"user","content":"hi"}
		]
	}`

	raw, _, _, _, err := responsesToOpenAI([]byte(body))
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := jsonx.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	tools, _ := got["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools=%d want 2: %v", len(tools), tools)
	}

	names := map[string]bool{}
	for _, tool := range tools {
		names[tool.(map[string]any)["function"].(map[string]any)["name"].(string)] = true
	}

	if !names["exec_command"] || !names["apply_patch"] {
		t.Errorf("tools=%v", names)
	}

	custom := responsesCustomTools([]byte(body))
	if !custom["apply_patch"] {
		t.Errorf("custom tools=%v", custom)
	}
}

// TestResponsesNamespaceTools namespace 容器展开成 flat 工具，并记录回程映射。
func TestResponsesNamespaceTools(t *testing.T) {
	t.Parallel()

	body := `{
		"model":"m",
		"input":"hi",
		"tools":[
			{"type":"function","name":"exec_command","parameters":{"type":"object"}},
			{"type":"namespace","name":"multi_agent_v1","tools":[
				{"type":"function","name":"spawn_agent","parameters":{"type":"object"}},
				{"type":"custom","name":"exec","description":"run js"}
			]}
		]
	}`

	raw, _, _, _, err := responsesToOpenAI([]byte(body))
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := jsonx.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	tools, _ := got["tools"].([]any)
	if len(tools) != 3 {
		t.Fatalf("tools=%d want 3: %v", len(tools), tools)
	}

	assertResponsesFlatToolNames(t, tools, "exec_command", "spawn_agent", "exec")

	if !responsesCustomTools([]byte(body))["exec"] {
		t.Error("namespace 内的 custom 工具未被识别")
	}

	namespaces := responsesToolNamespacesFromBody([]byte(body))
	if namespaces["spawn_agent"].namespace != "multi_agent_v1" || namespaces["exec"].namespace != "multi_agent_v1" {
		t.Errorf("namespaces=%v", namespaces)
	}

	if _, ok := namespaces["exec_command"]; ok {
		t.Errorf("top-level tool should not have namespace: %v", namespaces)
	}
}

// TestResponsesNamespaceToolCollision 不同 namespace 的同名工具都要下发给上游。
// 重名时用限定名区分，回程再还原成裸名 + namespace，避免后一个工具被去重丢弃。
func TestResponsesNamespaceToolCollision(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"model":"m",
		"input":"hi",
		"tools":[
			{"type":"namespace","name":"mcp__a__","tools":[
				{"type":"function","name":"search","parameters":{"type":"object"}}
			]},
			{"type":"namespace","name":"mcp__b__","tools":[
				{"type":"function","name":"search","parameters":{"type":"object"}}
			]}
		]
	}`)

	raw, _, _, _, err := responsesToOpenAI(body)
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := jsonx.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	assertResponsesFlatToolNames(t, got["tools"].([]any), "mcp__a__search", "mcp__b__search")

	refs := responsesToolNamespacesFromBody(body)
	assertNamespaceCollisionRefs(t, refs)
	assertNamespaceCollisionResponse(t, refs)
	assertNamespaceCollisionStream(t, refs)
}

func assertNamespaceCollisionRefs(t *testing.T, refs map[string]responsesToolRef) {
	t.Helper()

	for _, testCase := range []struct {
		declared  string
		namespace string
	}{
		{"mcp__a__search", "mcp__a__"},
		{"mcp__b__search", "mcp__b__"},
	} {
		bare, namespace := responsesResolveToolName(testCase.declared, refs)
		if bare != "search" || namespace != testCase.namespace {
			t.Errorf("resolve %s = (%q, %q)", testCase.declared, bare, namespace)
		}
	}
}

func assertNamespaceCollisionResponse(t *testing.T, refs map[string]responsesToolRef) {
	t.Helper()

	resp := responsesFromOpenAIWithNamespaces(map[string]any{
		"choices": []any{map[string]any{
			"message": map[string]any{
				"content": "",
				"tool_calls": []any{map[string]any{
					"id": "call_b", "type": "function",
					"function": map[string]any{"name": "mcp__b__search", "arguments": "{}"},
				}},
			},
		}},
	}, "m", "", nil, refs)

	output, _ := resp["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output=%v", output)
	}

	item := output[0].(map[string]any)
	if item["name"] != "search" || item["namespace"] != "mcp__b__" {
		t.Errorf("item=%v", item)
	}
}

func assertNamespaceCollisionStream(t *testing.T, refs map[string]responsesToolRef) {
	t.Helper()

	rec := httptest.NewRecorder()
	writer := newResponsesStreamWriter(rec, "m", "", nil, refs)

	stream := `data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_b","function":{"name":"mcp__b__search","arguments":"{}"}}]}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	if _, err := writer.Write([]byte(stream)); err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{`"name":"search"`, `"namespace":"mcp__b__"`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("stream missing %q:\n%s", want, rec.Body)
		}
	}
}

// TestResponsesUpstreamToolNameBareAmbiguous 同名工具跨 namespace 时，bare 名无法
// 判定身份，必须保留原名；唯一匹配时才映射到限定名（issue #259）。
func TestResponsesUpstreamToolNameBareAmbiguous(t *testing.T) {
	t.Parallel()

	refs := map[string]responsesToolRef{
		"mcp__a__search": {bare: "search", namespace: "mcp__a__"},
		"mcp__b__search": {bare: "search", namespace: "mcp__b__"},
	}

	if got := responsesUpstreamToolName("search", refs); got != "search" {
		t.Fatalf("ambiguous bare name = %q want unchanged", got)
	}

	if got := responsesUpstreamToolName("mcp__a__search", refs); got != "mcp__a__search" {
		t.Fatalf("declared name = %q", got)
	}

	unique := map[string]responsesToolRef{
		"functions__lookup": {bare: "lookup", namespace: "functions"},
	}
	if got := responsesUpstreamToolName("lookup", unique); got != "functions__lookup" {
		t.Fatalf("unique bare name = %q want functions__lookup", got)
	}
}

// TestResponsesUpstreamToolNameTopLevelCollision 顶层裸名与 namespace 工具同名时，
// 历史 function_call 的裸名必须仍指向顶层工具，不能被改写成限定名（issue #259）。
func TestResponsesUpstreamToolNameTopLevelCollision(t *testing.T) {
	t.Parallel()

	tools := []map[string]any{
		{"type": "function", "name": "search", "parameters": map[string]any{"type": "object"}},
		{"type": "namespace", "name": "web", "tools": []any{
			map[string]any{"type": "function", "name": "search", "parameters": map[string]any{"type": "object"}},
		}},
	}

	_, refs := responsesTools(tools)

	if got := responsesUpstreamToolName("search", refs); got != "search" {
		t.Fatalf("top-level search = %q want search (refs=%v)", got, refs)
	}

	if got := responsesUpstreamToolName("web__search", refs); got != "web__search" {
		t.Fatalf("namespaced search = %q want web__search", got)
	}
}

// assertResponsesFlatToolNames 校验转换后的 chat tools 名字集合。
func assertResponsesFlatToolNames(t *testing.T, tools []any, want ...string) {
	t.Helper()

	names := map[string]bool{}
	for _, tool := range tools {
		names[tool.(map[string]any)["function"].(map[string]any)["name"].(string)] = true
	}

	for _, name := range want {
		if !names[name] {
			t.Errorf("missing flat tool %q: %v", name, names)
		}
	}
}

// TestResponsesAdditionalNamespaceTools Codex lite 的真实形状：
// input.additional_tools 里是 namespace 容器，需要先合并再展开。
func TestResponsesAdditionalNamespaceTools(t *testing.T) {
	t.Parallel()

	body := `{
		"model":"m",
		"input":[
			{"type":"additional_tools","role":"developer","tools":[
				{"type":"namespace","name":"functions","tools":[
					{"type":"function","name":"exec_command","parameters":{"type":"object"}}
				]}
			]},
			{"type":"message","role":"user","content":"hi"}
		]
	}`

	raw, _, _, _, err := responsesToOpenAI([]byte(body))
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := jsonx.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	assertResponsesFlatToolNames(t, got["tools"].([]any), "exec_command")
}

// TestResponsesFunctionItemsNamespace 非流式回程补回 namespace。
func TestResponsesFunctionItemsNamespace(t *testing.T) {
	t.Parallel()

	body := map[string]any{
		"choices": []any{map[string]any{
			"message": map[string]any{
				"content": "",
				"tool_calls": []any{map[string]any{
					"id": "call_1", "type": "function",
					"function": map[string]any{"name": "spawn_agent", "arguments": "{}"},
				}},
			},
		}},
	}

	resp := responsesFromOpenAIWithNamespaces(body, "m", "", nil, map[string]responsesToolRef{"spawn_agent": {bare: "spawn_agent", namespace: "multi_agent_v1"}})

	output, _ := resp["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output=%v", output)
	}

	item := output[0].(map[string]any)
	if item["name"] != "spawn_agent" || item["namespace"] != "multi_agent_v1" {
		t.Errorf("item=%v", item)
	}
}

// TestResponsesStreamWriterNamespace 流式 output_item 补回 namespace。
func TestResponsesStreamWriterNamespace(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	w := newResponsesStreamWriter(rec, "m", "", nil, map[string]responsesToolRef{"spawn_agent": {bare: "spawn_agent", namespace: "multi_agent_v1"}})

	chunks := `data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"spawn_agent","arguments":"{}"}}]}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
		"data: [DONE]\n\n"

	if _, err := w.Write([]byte(chunks)); err != nil {
		t.Fatal(err)
	}

	out := rec.Body.String()
	for _, want := range []string{`"name":"spawn_agent"`, `"namespace":"multi_agent_v1"`} {
		if !strings.Contains(out, want) {
			t.Errorf("stream missing %q:\n%s", want, out)
		}
	}
}

// TestResponsesAgentMessage 子代理消息与无 call_id 的工具输出降级为 user 消息。
func TestResponsesAgentMessage(t *testing.T) {
	t.Parallel()

	body := `{
		"model":"m",
		"input":[
			{"type":"agent_message","content":[{"type":"text","text":"child result"}]},
			{"type":"function_call_output","output":"orphan output"}
		]
	}`

	raw, _, _, _, err := responsesToOpenAI([]byte(body))
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := jsonx.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	msgs, _ := got["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages=%d want 2: %v", len(msgs), msgs)
	}

	for i, want := range []string{"child result", "orphan output"} {
		msg := msgs[i].(map[string]any)
		if msg["role"] != "user" || !strings.Contains(msg["content"].(string), want) {
			t.Errorf("message[%d]=%v", i, msg)
		}
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
	if err := jsonx.Unmarshal(raw, &got); err != nil {
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

// TestResponsesToolImagesStayWithToolRun 历史工具图片必须紧跟该轮工具结果，
// 不能因为会话后面还有消息就被挪到最末尾。
func TestResponsesToolImagesStayWithToolRun(t *testing.T) {
	t.Parallel()

	body := `{
		"model":"m",
		"input":[
			{"type":"message","role":"user","content":"look"},
			{"type":"function_call","call_id":"c1","name":"view_image","arguments":"{}"},
			{"type":"function_call_output","call_id":"c1","output":[
				{"type":"input_image","image_url":"data:image/png;base64,abc"}
			]},
			{"type":"message","role":"user","content":"next"}
		]
	}`

	raw, _, _, _, err := responsesToOpenAI([]byte(body))
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := jsonx.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	msgs, _ := got["messages"].([]any)
	if len(msgs) != 5 {
		t.Fatalf("messages=%d want 5: %v", len(msgs), msgs)
	}

	image := msgs[3].(map[string]any)
	if image["role"] != "user" {
		t.Fatalf("image 应紧跟 tool 消息: %v", msgs)
	}

	if parts, _ := image["content"].([]any); len(parts) != 1 || parts[0].(map[string]any)["type"] != "image_url" {
		t.Errorf("image message=%v", image)
	}

	if next := msgs[4].(map[string]any); next["role"] != "user" || next["content"] != "next" {
		t.Errorf("后续消息被挤到图片之后: %v", msgs[4])
	}
}

func TestResponsesCustomToolInputStrict(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name string
		args string
		want string
		ok   bool
	}{
		{"约定形态", `{"input":"*** Begin Patch"}`, "*** Begin Patch", true},
		{"缺 input 键", `{"patch":"*** Begin Patch"}`, "", false},
		{"input 非字符串", `{"input":123}`, "", false},
		{"非法 JSON", `not-json`, "", false},
		{"截断 JSON", `{"input":"*** Beg`, "", false},
	} {
		got, ok := responsesCustomToolInput(testCase.args)
		if got != testCase.want || ok != testCase.ok {
			t.Errorf("%s: got (%q,%v) want (%q,%v)", testCase.name, got, ok, testCase.want, testCase.ok)
		}
	}
}

func TestResponsesRequestRejectsNullInput(t *testing.T) {
	t.Parallel()

	body := `{"model":"m","input":null}`
	if _, _, _, _, err := responsesToOpenAI([]byte(body)); !errors.Is(err, errInputIsRequired) {
		t.Fatalf("err=%v want %v", err, errInputIsRequired)
	}
}

// TestResponsesRequestDropsReasoningAtBoundary reasoning 项只在后继 assistant 落地时
// 挂载；user / tool 边界上没有归属，就地丢弃，不跨轮泄漏（issue #584）。
func TestResponsesRequestDropsReasoningAtBoundary(t *testing.T) {
	t.Parallel()

	body := `{
		"model":"m",
		"instructions":"sys",
		"input":[
			{"type":"reasoning","id":"rs_0","summary":[{"type":"summary_text","text":"plan"}]},
			{"type":"message","role":"user","content":"hi"},
			{"type":"function_call","call_id":"c1","name":"lookup","arguments":"{\"q\":1}"},
			{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"think"}]},
			{"type":"function_call_output","call_id":"c1","output":"ok"},
			{"type":"reasoning","id":"rs_2","summary":[{"type":"summary_text","text":"tail"}]}
		]
	}`

	raw, _, _, _, err := responsesToOpenAI([]byte(body))
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := jsonx.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	msgs, _ := got["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages=%d want 4（边界上的 reasoning 应丢弃）: %v", len(msgs), msgs)
	}

	assertResponsesMessages(t, msgs)

	for i, raw2 := range msgs {
		if _, ok := raw2.(map[string]any)["reasoning_content"]; ok {
			t.Errorf("messages[%d] 不该带 reasoning_content: %v", i, raw2)
		}
	}
}

// TestResponsesRequestReplaysReasoning reasoning 项解出的思考挂到后继 assistant 的
// reasoning_content：正文消息与工具调用批次都算同一轮，连续段按 "\n" 拼接，
// 空文本既不产出消息也不产出字段（issue #584）。
func TestResponsesRequestReplaysReasoning(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name    string
		input   string
		want    []string
		wantMsg int
	}{
		{
			name: "思考挂到后继 assistant 正文",
			input: `{"type":"reasoning","summary":[{"type":"summary_text","text":"plan"}]},
				{"type":"message","role":"assistant","content":"done"}`,
			want:    []string{"plan"},
			wantMsg: 1,
		},
		{
			name: "连续思考段按换行拼接",
			input: `{"type":"reasoning","summary":[{"type":"summary_text","text":"first"}]},
				{"type":"reasoning","summary":[{"type":"summary_text","text":"second"}]},
				{"type":"message","role":"assistant","content":"done"}`,
			want:    []string{"first\nsecond"},
			wantMsg: 1,
		},
		{
			name: "summary 缺失时退到 content",
			input: `{"type":"reasoning","content":[{"type":"reasoning_text","text":"deep"}]},
				{"type":"message","role":"assistant","content":"done"}`,
			want:    []string{"deep"},
			wantMsg: 1,
		},
		{
			name: "思考挂到工具调用批次",
			input: `{"type":"reasoning","summary":[{"type":"summary_text","text":"need tool"}]},
				{"type":"function_call","call_id":"c1","name":"lookup","arguments":"{}"}`,
			want:    []string{"need tool"},
			wantMsg: 1,
		},
		{
			name: "空思考不产出字段",
			input: `{"type":"reasoning","summary":[]},
				{"type":"message","role":"assistant","content":"done"}`,
			want:    []string{""},
			wantMsg: 1,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			body := `{"model":"m","input":[` + testCase.input + `]}`

			raw, _, _, _, err := responsesToOpenAI([]byte(body))
			if err != nil {
				t.Fatal(err)
			}

			var got map[string]any
			if err := jsonx.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}

			msgs, _ := got["messages"].([]any)
			if len(msgs) != testCase.wantMsg {
				t.Fatalf("messages=%d want %d: %v", len(msgs), testCase.wantMsg, msgs)
			}

			assertReasoningContent(t, msgs, testCase.want[0])
		})
	}
}

// assertReasoningContent 断言转换结果里唯一的 reasoning_content 取值符合预期。
func assertReasoningContent(t *testing.T, msgs []any, want string) {
	t.Helper()

	got := ""

	for _, raw := range msgs {
		msg, _ := raw.(map[string]any)
		if value, ok := msg["reasoning_content"].(string); ok {
			got = value
		}
	}

	if got != want {
		t.Errorf("reasoning_content=%q want %q: %v", got, want, msgs)
	}
}

func TestResponsesRequestRejectsReasoningOnlyInput(t *testing.T) {
	t.Parallel()

	body := `{"model":"m","input":[{"type":"reasoning","id":"rs_1","summary":[]}]}`
	if _, _, _, _, err := responsesToOpenAI([]byte(body)); !errors.Is(err, errNoConvertibleItems) {
		t.Fatalf("reasoning-only input error = %v, want %v", err, errNoConvertibleItems)
	}
}

// TestResponsesRequestChatShapedToolPairs 客户端把 chat messages 原样塞进 input 时，
// role=tool 的工具结果必须保住 tool_call_id 与 name，出站的 assistant.tool_calls 与
// tool 结果仍成对（否则 cleanupOrphanToolCalls 把这一对一起删掉，issue #584）。
func TestResponsesRequestChatShapedToolPairs(t *testing.T) {
	t.Parallel()

	body := `{
		"model":"glm-5.2",
		"input":[
			{"role":"user","content":"hi"},
			{"role":"assistant","content":"","reasoning_content":"plan",
				"tool_calls":[{"id":"call_1","type":"function",
					"function":{"name":"lookup","arguments":"{\"q\":1}"}}]},
			{"role":"tool","tool_call_id":"call_1","name":"lookup","content":"ok"}
		]
	}`

	raw, _, _, _, err := responsesToOpenAI([]byte(body))
	if err != nil {
		t.Fatalf("Chat 形状的 tool 项不该 400: %v", err)
	}

	var converted map[string]any
	if err := jsonx.Unmarshal(raw, &converted); err != nil {
		t.Fatal(err)
	}

	msgs, _ := converted["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages=%d want 3: %v", len(msgs), msgs)
	}

	assistant := msgs[1].(map[string]any)

	calls, _ := assistant["tool_calls"].([]any)
	if len(calls) != 1 || calls[0].(map[string]any)["id"] != "call_1" {
		t.Fatalf("assistant.tool_calls=%v", assistant["tool_calls"])
	}

	if assistant["reasoning_content"] != "plan" {
		t.Errorf("assistant.reasoning_content=%v", assistant["reasoning_content"])
	}

	tool := msgs[2].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "call_1" || tool["name"] != "lookup" {
		t.Fatalf("tool=%v", tool)
	}

	assertChatToolPairSurvivesOutbound(t, body)
}

// assertChatToolPairSurvivesOutbound 走完整出站管线（cleanupOrphanToolCalls 在内），
// 断言上游收到的请求里调用与结果仍成对。
func assertChatToolPairSurvivesOutbound(t *testing.T, body string) {
	t.Helper()

	var captured []byte

	client := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			captured, _ = io.ReadAll(r.Body)

			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
	handler := NewHandler(&Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: client,
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	var outbound map[string]any
	if err := jsonx.Unmarshal(captured, &outbound); err != nil {
		t.Fatalf("出站请求体解析失败: %v (%s)", err, captured)
	}

	msgs, _ := outbound["messages"].([]any)

	callIDs := map[string]bool{}

	for _, item := range msgs {
		msg, _ := item.(map[string]any)

		if calls, ok := msg["tool_calls"].([]any); ok {
			for _, call := range calls {
				callIDs[stringValue(call.(map[string]any)["id"])] = true
			}
		}
	}

	resultIDs := map[string]bool{}

	for _, item := range msgs {
		msg, _ := item.(map[string]any)
		if msg["role"] == "tool" {
			resultIDs[stringValue(msg["tool_call_id"])] = true
		}
	}

	if !callIDs["call_1"] || !resultIDs["call_1"] {
		t.Fatalf("出站配对丢失: 调用=%v 结果=%v (%s)", callIDs, resultIDs, captured)
	}
}

// TestResponsesRequestChatShapedToolImages Chat 形状工具结果里的图片按
// function_call_output 同一条路径拆分，并等并行结果全部落地后才发图。
func TestResponsesRequestChatShapedToolImages(t *testing.T) {
	t.Parallel()

	body := `{
		"model":"m",
		"input":[
			{"role":"user","content":"look"},
			{"role":"assistant","content":"","tool_calls":[
				{"id":"c1","type":"function","function":{"name":"view_image","arguments":"{}"}},
				{"id":"c2","type":"function","function":{"name":"view_image","arguments":"{}"}}
			]},
			{"role":"tool","tool_call_id":"c1","content":[
				{"type":"input_image","image_url":"data:image/png;base64,aaa"}
			]},
			{"role":"tool","tool_call_id":"c2","content":"ok"}
		]
	}`

	raw, _, _, _, err := responsesToOpenAI([]byte(body))
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := jsonx.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	msgs, _ := got["messages"].([]any)
	if len(msgs) != 5 {
		t.Fatalf("messages=%d want 5（图片必须落在两条结果之后）: %v", len(msgs), msgs)
	}

	for i := 2; i <= 3; i++ {
		if role := msgs[i].(map[string]any)["role"]; role != "tool" {
			t.Fatalf("messages[%d] 应为 tool: %v", i, msgs)
		}
	}

	image := msgs[4].(map[string]any)

	parts, _ := image["content"].([]any)
	if image["role"] != "user" || len(parts) != 1 || parts[0].(map[string]any)["type"] != "image_url" {
		t.Errorf("图片消息=%v", image)
	}
}

// TestResponsesRequestSkipsHostedToolItems 托管工具回执（web_search_call、
// mcp_call 等）与无内容元数据项不生成 user 空消息、不打断并行工具结果，
// 也不清空夹在中间的思考（issue #584 追加验收）。
func TestResponsesRequestSkipsHostedToolItems(t *testing.T) {
	t.Parallel()

	body := `{
		"model":"m",
		"input":[
			{"type":"message","role":"user","content":"look"},
			{"type":"reasoning","summary":[{"type":"summary_text","text":"plan"}]},
			{"type":"web_search_call","id":"ws_1","status":"completed"},
			{"type":"image_generation_call","id":"ig_1","status":"completed"},
			{"type":"function_call","call_id":"c1","name":"view_image","arguments":"{}"},
			{"type":"function_call","call_id":"c2","name":"view_image","arguments":"{}"},
			{"type":"function_call_output","call_id":"c1","output":[
				{"type":"input_image","image_url":"data:image/png;base64,aaa"}
			]},
			{"type":"mcp_call","id":"mcp_1","status":"completed"},
			{"type":"computer_call","id":"cu_1"},
			{"id":"no_type_meta"},
			{"type":"file_search_call","id":"fs_1",
				"results":[{"file_id":"f1","text":"t","filename":"a.txt","score":1}]},
			{"type":"code_interpreter_call","id":"ci_1","code":"1+1"},
			{"type":"additional_tools","role":"developer","tools":[]},
			{"type":"function_call_output","call_id":"c2","output":[
				{"type":"input_image","image_url":"data:image/png;base64,bbb"}
			]}
		]
	}`

	raw, _, _, _, err := responsesToOpenAI([]byte(body))
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := jsonx.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	msgs, _ := got["messages"].([]any)
	if len(msgs) != 6 {
		t.Fatalf("messages=%d want 6（托管回执应被跳过）: %v", len(msgs), msgs)
	}

	assertNoEmptyUserMessages(t, msgs)
	assertParallelToolImagesMerged(t, msgs)

	// 托管回执夹在思考与工具批次之间，思考仍挂在 assistant 上。
	if rc := msgs[1].(map[string]any)["reasoning_content"]; rc != "plan" {
		t.Errorf("reasoning_content=%v want plan", rc)
	}
}

// assertNoEmptyUserMessages 断言正文之外没有 user 空消息（只能是工具图片消息）。
func assertNoEmptyUserMessages(t *testing.T, msgs []any) {
	t.Helper()

	for i, item := range msgs {
		msg, _ := item.(map[string]any)
		if msg["role"] != "user" || i == 0 {
			continue
		}

		parts, _ := msg["content"].([]any)
		if len(parts) == 0 || parts[0].(map[string]any)["type"] != "image_url" {
			t.Fatalf("messages[%d] 是 user 空消息: %v", i, msg)
		}
	}
}

// assertParallelToolImagesMerged 断言两条并行结果相邻、图片合并落在它们之后。
func assertParallelToolImagesMerged(t *testing.T, msgs []any) {
	t.Helper()

	if msgs[3].(map[string]any)["role"] != "tool" || msgs[4].(map[string]any)["role"] != "tool" {
		t.Fatalf("并行结果被打断: %v", msgs)
	}

	parts, _ := msgs[5].(map[string]any)["content"].([]any)
	if len(parts) != 2 {
		t.Errorf("图片应合并为一条消息: %v", msgs[5])
	}
}

// TestResponsesRequestCompaction compaction 只接受能解出 {"summary":...} 的摘要：
// 密文（URL-safe base64、非 JSON base64、summary 为空的 JSON）一律丢弃，
// 纯文本摘要按 rune 封顶 32 KiB（issue #584 追加验收）。
func TestResponsesRequestCompaction(t *testing.T) {
	t.Parallel()

	summaries := map[string]string{
		"有效摘要": base64.StdEncoding.EncodeToString([]byte(`{"v":1,"summary":"history"}`)),
		// URL-safe 密文：0xFB/0xFF 的 URL 编码必然产出 '-'/'_'，标准解码必然失败。
		"URL-safe base64":  base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0xfb, 0xff}, 24)),
		"非 JSON base64":    base64.StdEncoding.EncodeToString([]byte(strings.Repeat("not-json ", 8))),
		"summary 为空的 JSON": base64.StdEncoding.EncodeToString([]byte(`{"v":1,"summary":""}`)),
	}

	for name, blob := range summaries {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			body := `{"model":"m","input":[
				{"type":"compaction","encrypted_content":"` + blob + `"},
				{"type":"message","role":"user","content":"hi"}
			]}`

			raw, _, _, _, err := responsesToOpenAI([]byte(body))
			if err != nil {
				t.Fatal(err)
			}

			var got map[string]any
			if err := jsonx.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}

			msgs, _ := got["messages"].([]any)

			want := 1
			if name == "有效摘要" {
				want = 2
			}

			if len(msgs) != want {
				t.Fatalf("%s: messages=%d want %d: %v", name, len(msgs), want, msgs)
			}

			if want == 2 {
				summary := msgs[0].(map[string]any)
				if summary["role"] != "system" ||
					summary["content"] != responsesCompactionPrefix+"history" {
					t.Errorf("交接摘要=%v", summary)
				}
			}
		})
	}
}

// TestResponsesRequestCompactionSummaryCapped 超长纯文本摘要按 rune 边界封顶 32 KiB，
// 裁剪不得产生非法 UTF-8。
func TestResponsesRequestCompactionSummaryCapped(t *testing.T) {
	t.Parallel()

	body := `{"model":"m","input":[
		{"type":"compaction","encrypted_content":"` + strings.Repeat("摘要", 12000) + `"},
		{"type":"message","role":"user","content":"hi"}
	]}`

	raw, _, _, _, err := responsesToOpenAI([]byte(body))
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := jsonx.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	msgs, _ := got["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages=%d want 2: %v", len(msgs), msgs)
	}

	summary := stringValue(msgs[0].(map[string]any)["content"])
	trimmed := strings.TrimPrefix(summary, responsesCompactionPrefix)

	if len(trimmed) > responsesCompactionSummaryMax {
		t.Errorf("摘要 %d 字节超过上限 %d", len(trimmed), responsesCompactionSummaryMax)
	}

	if !utf8.ValidString(trimmed) {
		t.Error("摘要裁剪产生了非法 UTF-8")
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
	}}, nil, nil, "stop")

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
	w := newResponsesStreamWriter(rec, "glm-5.2", "sys", nil, nil)

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
	w := newResponsesStreamWriter(rec, "m", "", map[string]bool{"apply_patch": true}, nil)

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
	if err := jsonx.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
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

// TestResponsesCustomToolTruncatedIncomplete finish_reason=length 时工具状态必须
// incomplete，且畸形 custom 输入不得回退原文（issue #259）。
func TestResponsesCustomToolTruncatedIncomplete(t *testing.T) {
	t.Parallel()

	body := map[string]any{
		"choices": []any{map[string]any{
			"message": map[string]any{
				"content": "",
				"tool_calls": []any{
					map[string]any{
						"id": "call_1", "type": "function",
						"function": map[string]any{"name": "apply_patch", "arguments": `{"input":"*** Begin"`},
					},
					map[string]any{
						"id": "call_2", "type": "function",
						"function": map[string]any{"name": "lookup", "arguments": `{"q":`},
					},
				},
			},
			"finish_reason": "length",
		}},
	}

	resp := responsesFromOpenAI(body, "m", "", map[string]bool{"apply_patch": true})

	output := resp["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("output=%v", output)
	}

	custom := output[0].(map[string]any)
	if custom["status"] != "incomplete" || custom["input"] != "" {
		t.Fatalf("custom=%v", custom)
	}

	fn := output[1].(map[string]any)
	if fn["status"] != "incomplete" {
		t.Fatalf("function=%v", fn)
	}
}

// TestResponsesInputCustomToolCallStrict 历史 custom 调用缺 input/类型不符直接报错。
func TestResponsesInputCustomToolCallStrict(t *testing.T) {
	t.Parallel()

	if _, err := responsesInputCustomToolCall(map[string]any{"name": "apply_patch"}, nil); !errors.Is(err, errCustomToolCallInputIs) {
		t.Fatalf("err=%v want %v", err, errCustomToolCallInputIs)
	}

	if _, err := responsesInputCustomToolCall(map[string]any{"name": "apply_patch", "input": 1}, nil); !errors.Is(err, errCustomToolCallInputIs) {
		t.Fatalf("err=%v want %v", err, errCustomToolCallInputIs)
	}
}

// TestResponsesStreamWriterLengthMarksToolIncomplete length 终态下流式工具必须
// 以 incomplete 下发，客户端不会执行半截参数（issue #259）。
func TestResponsesStreamWriterLengthMarksToolIncomplete(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	w := newResponsesStreamWriter(rec, "m", "", map[string]bool{"apply_patch": true}, nil)

	chunks := `data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"apply_patch","arguments":"{\"input\":\"*** Begin\"}"}}]}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}` + "\n\n" +
		"data: [DONE]\n\n"

	if _, err := w.Write([]byte(chunks)); err != nil {
		t.Fatal(err)
	}

	out := rec.Body.String()
	if !strings.Contains(out, `"type":"custom_tool_call"`) || !strings.Contains(out, `"status":"incomplete"`) {
		t.Fatalf("tool must be incomplete:\n%s", out)
	}

	if strings.Count(out, `"status":"completed"`) > 0 && strings.Contains(out, `"type":"custom_tool_call"`) {
		// output_item.added 允许 in_progress，但不应出现 completed 的 custom item。
		if strings.Contains(out, `"input":"*** Begin","status":"completed"`) {
			t.Fatalf("half input must not be completed:\n%s", out)
		}
	}
}

// TestResponsesEarlyToolOutputReordered 端到端：function_call_output 排在
// function_call 之前（Codex 经本地代理转 /v1/responses 的形态）时，发往上游的 body
// 里 assistant(tool_calls) 必须在 tool 之前，否则上游判 11148 顶死整条会话。
func TestResponsesEarlyToolOutputReordered(t *testing.T) {
	t.Parallel()

	var seen []string

	up := recordingUpstream(t, []string{sseOK}, &seen)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	handler := NewHandler(&Config{Pool: p, Upstream: up})

	body := `{"model":"glm-5.2","input":[
		{"type":"function_call_output","call_id":"call_early","output":"ok"},
		{"type":"function_call","call_id":"call_early","name":"lookup","arguments":"{}"}
	]}`
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if len(seen) != 1 {
		t.Fatalf("upstream calls=%d want 1", len(seen))
	}

	assistantAt := strings.Index(seen[0], `"role":"assistant"`)
	toolAt := strings.Index(seen[0], `"role":"tool"`)

	if assistantAt < 0 || toolAt < 0 {
		t.Fatalf("出站 body 缺少 assistant 或 tool 消息: %s", seen[0])
	}

	if assistantAt > toolAt {
		t.Errorf("出站 body 仍是 tool 在 assistant 之前（上游判 11148）: %s", seen[0])
	}

	if !strings.Contains(seen[0], `"tool_calls"`) {
		t.Errorf("出站 body 丢失 tool_calls: %s", seen[0])
	}
}
