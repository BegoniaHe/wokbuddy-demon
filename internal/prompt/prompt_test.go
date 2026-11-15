package prompt

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// systemRoles 提取 messages 中所有 role 值，用于断言改写后的角色序列。
func systemRoles(t *testing.T, body []byte) []string {
	t.Helper()

	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, body)
	}

	msgs, ok := obj["messages"].([]any)
	if !ok {
		t.Fatalf("messages not []any: %v", obj["messages"])
	}

	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			t.Fatalf("msg not map: %v", m)
		}

		r, _ := mm["role"].(string)
		out = append(out, r)
	}

	return out
}

func TestRewriteReplacesSystemAndDeveloper(t *testing.T) {
	t.Parallel()

	nums := []byte(`{
		"model":"glm-5.2",
		"messages":[
			{"role":"system","content":"被替换的旧提示词"},
			{"role":"developer","content":"开发者指令"},
			{"role":"user","content":"你好"}
		],
		"metadata":{"conversation_id":"c1"}
	}`)
	out := Rewrite(nums, "我是自有提示词")
	roles := systemRoles(t, out)

	wantRoles := []string{"system", "user"}
	if len(roles) != len(wantRoles) {
		t.Fatalf("roles=%v want %v", roles, wantRoles)
	}

	for i, r := range roles {
		if r != wantRoles[i] {
			t.Fatalf("roles[%d]=%q want %q (all=%v)", i, r, wantRoles[i], roles)
		}
	}
	// 头部 system 内容恰为自有提示词，旧 system/developer 内容零残留。
	var obj map[string]any
	json.Unmarshal(out, &obj)
	msgs := obj["messages"].([]any)

	first := msgs[0].(map[string]any)
	if first["content"] != "我是自有提示词" {
		t.Errorf("first system content=%v", first["content"])
	}

	if strings.Contains(string(out), "被替换的旧提示词") || strings.Contains(string(out), "开发者指令") {
		t.Errorf("old system/developer content leaked: %s", out)
	}
}

func TestRewriteKeepsUserAssistantToolUntouched(t *testing.T) {
	t.Parallel()

	nums := []byte(`{
		"messages":[
			{"role":"user","content":"u-content"},
			{"role":"assistant","content":"a-content"},
			{"role":"tool","tool_call_id":"t1","content":"tool-result"}
		]
	}`)
	out := Rewrite(nums, "SYS")

	var obj map[string]any
	json.Unmarshal(out, &obj)
	msgs := obj["messages"].([]any)
	// 预期：system(新) + user + assistant + tool，顺序保留。
	if len(msgs) != 4 {
		t.Fatalf("len=%d", len(msgs))
	}

	roles := systemRoles(t, out)

	want := []string{"system", "user", "assistant", "tool"}
	for i := range want {
		if roles[i] != want[i] {
			t.Fatalf("roles=%v want %v", roles, want)
		}
	}
	// user/assistant/tool 字段逐字不动。
	userMsg := msgs[1].(map[string]any)
	if userMsg["content"] != "u-content" {
		t.Errorf("user content changed: %v", userMsg["content"])
	}

	toolMsg := msgs[3].(map[string]any)
	if toolMsg["tool_call_id"] != "t1" || toolMsg["content"] != "tool-result" {
		t.Errorf("tool changed: %v", toolMsg)
	}
}

func TestRewriteKeepsMetadataAndOtherFields(t *testing.T) {
	t.Parallel()

	nums := []byte(`{
		"model":"glm-5.2",
		"stream":true,
		"metadata":{"conversation_id":"c1","user_id":"u9"},
		"messages":[{"role":"user","content":"hi"}]
	}`)
	out := Rewrite(nums, "SYS")

	var obj map[string]any
	json.Unmarshal(out, &obj)

	if obj["model"] != "glm-5.2" {
		t.Errorf("model changed: %v", obj["model"])
	}

	if obj["stream"] != true {
		t.Errorf("stream changed: %v", obj["stream"])
	}

	meta, ok := obj["metadata"].(map[string]any)
	if !ok || meta["conversation_id"] != "c1" || meta["user_id"] != "u9" {
		t.Errorf("metadata changed: %v", obj["metadata"])
	}
}

func TestRewriteInjectsSystemWhenAbsent(t *testing.T) {
	t.Parallel()

	in := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	out := Rewrite(in, "SYS")
	roles := systemRoles(t, out)

	want := []string{"system", "user"}
	if len(roles) != len(want) {
		t.Fatalf("roles=%v want %v", roles, want)
	}

	for i, r := range roles {
		if r != want[i] {
			t.Fatalf("roles=%v want %v", roles, want)
		}
	}
}

func TestRewriteMultimodalContentUntouched(t *testing.T) {
	t.Parallel()
	// user content 为多模态数组（text + image_url），Rewrite 只动 messages 层级，
	// 不应改动 content 内部结构。
	nums := []byte(`{
		"messages":[
			{"role":"system","content":"old"},
			{"role":"user","content":[
				{"type":"text","text":"看图"},
				{"type":"image_url","image_url":{"url":"data:..."}}
			]}
		]
	}`)
	out := Rewrite(nums, "SYS")

	var obj map[string]any
	json.Unmarshal(out, &obj)

	msgs := obj["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("len=%d", len(msgs))
	}

	userMsg := msgs[1].(map[string]any)

	arr, ok := userMsg["content"].([]any)
	if !ok || len(arr) != 2 {
		t.Fatalf("multimodal content changed: %v", userMsg["content"])
	}

	textPart := arr[0].(map[string]any)
	if textPart["type"] != "text" || textPart["text"] != "看图" {
		t.Errorf("text part changed: %v", textPart)
	}
}

func TestRewriteInvalidJSONReturnedAsIs(t *testing.T) {
	t.Parallel()

	in := []byte(`{not valid json`)

	out := Rewrite(in, "SYS")
	if string(out) != string(in) {
		t.Errorf("invalid json should return as-is: got %s", out)
	}
}

// TestRewriteAppendNullBodyReturnedAsIs 顶层 null 解析为 nil map：改写必须原样返回，
// 不能对 nil map 赋值（custom/append 模式下客户端可远程触发 panic）。
func TestRewriteAppendNullBodyReturnedAsIs(t *testing.T) {
	t.Parallel()

	body := []byte(`null`)

	if out := Rewrite(body, "SYS"); string(out) != string(body) {
		t.Errorf("Rewrite(null) = %s, want 原样返回", out)
	}

	if out := Append(body, "SYS"); string(out) != string(body) {
		t.Errorf("Append(null) = %s, want 原样返回", out)
	}
}

func TestRewriteEmptyBodyReturnedAsIs(t *testing.T) {
	t.Parallel()

	out := Rewrite([]byte{}, "SYS")
	if len(out) != 0 {
		t.Errorf("empty body should return as-is: got %s", out)
	}
}

func TestRewriteEmptyPromptReturnedAsIs(t *testing.T) {
	t.Parallel()

	in := []byte(`{"messages":[{"role":"system","content":"old"}]}`)
	out := Rewrite(in, "")
	// systemPrompt 空 → 不改写，原样返回。
	if string(out) != string(in) {
		t.Errorf("empty prompt should return as-is: got %s", out)
	}
}

func TestLoadDefaultWhenFileEmpty(t *testing.T) {
	t.Parallel()

	got, err := Load("custom", "")
	if err != nil {
		t.Fatal(err)
	}

	if got != defaultPrompt {
		t.Errorf("Load() returned non-default prompt (len=%d vs %d)", len(got), len(defaultPrompt))
	}

	if len(got) == 0 {
		t.Error("default prompt is empty")
	}
}

func TestLoadFileOverride(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fp := filepath.Join(dir, "my.md")
	want := "这是我的自定义人格入口。"
	os.WriteFile(fp, []byte(want), 0o600)

	got, err := Load("custom", fp)
	if err != nil {
		t.Fatal(err)
	}

	if got != want {
		t.Errorf("Load()=%q want %q", got, want)
	}
}

func TestLoadFileMissingFailsFast(t *testing.T) {
	t.Parallel()

	if _, err := Load("custom", "/nonexistent/promp.md"); err == nil {
		t.Fatal("missing file should return error (fail fast)")
	}
}

// TestRewritePreservesNumberLiterals 改写 messages 不能改掉客户端的数值字面量。
// 默认 json.Unmarshal 把一切数字变 float64：seed 9007199254740993 会变成 ...992、
// 1.0 变成 1（与 split.go 的 UseNumber 同口径，split_test 已钉住同一断言）。
func TestRewritePreservesNumberLiterals(t *testing.T) {
	t.Parallel()

	body := []byte(`{"model":"x","seed":9007199254740993,"temperature":1.0,` +
		`"messages":[{"role":"user","content":"hi"}]}`)

	for name, out := range map[string][]byte{
		"Rewrite":           Rewrite(body, "人格"),
		"Append":            Append(body, "人格"),
		"EnsureSystemFirst": EnsureSystemFirst(body, "人格"),
	} {
		got := string(out)

		if !strings.Contains(got, "9007199254740993") {
			t.Fatalf("%s: seed 被改写: %s", name, got)
		}

		if !strings.Contains(got, `"temperature":1.0`) {
			t.Fatalf("%s: temperature 字面量被改写: %s", name, got)
		}
	}
}

// TestLoadRejectsEmptyFile 空提示词文件必须报错：custom/append 模式下 systemPrompt
// 为空时 Rewrite/Append 原样返回 body，等于「配了文件、一行都没生效」。
func TestLoadRejectsEmptyFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "system.md")
	if err := os.WriteFile(path, []byte(" \n\t\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Load("custom", path); err == nil {
		t.Fatal("空白提示词文件应报错")
	}
}

// TestRewriteLeavesInvalidUTF8BodyUntouched 非法 UTF-8 请求体不改写：
// json 往返会把坏字节换成 U+FFFD，改写后与客户端原文不再逐字节相同。
func TestRewriteLeavesInvalidUTF8BodyUntouched(t *testing.T) {
	t.Parallel()

	body := []byte("{\"messages\":[{\"role\":\"user\",\"content\":\"a\xffb\"}]}")

	if out := Rewrite(body, "sys"); string(out) != string(body) {
		t.Fatalf("body 被改写:\n got=%q\nwant=%q", out, body)
	}

	if out := Append(body, "sys"); string(out) != string(body) {
		t.Fatalf("Append 改写了非法 UTF-8 body: %q", out)
	}

	if out, ok := SplitSystemBySentences(body); ok || string(out) != string(body) {
		t.Fatalf("Split 改写了非法 UTF-8 body: ok=%v out=%q", ok, out)
	}
}

// TestDecodeJSONObjectRejectsTrailingContent 尾随内容（{}{} 或 {}garbage）必须解析
// 失败并原样转发：原实现只解第一个对象，改写信道会把它重写成只剩第一个对象。
func TestDecodeJSONObjectRejectsTrailingContent(t *testing.T) {
	t.Parallel()

	cases := []string{
		`{"messages":[]}{"messages":[{"role":"user","content":"x"}]}`,
		`{"messages":[]}garbage`,
	}

	for _, body := range cases {
		if _, err := decodeJSONObject([]byte(body)); err == nil {
			t.Fatalf("尾随内容应解析失败: %s", body)
		}
	}

	if _, err := decodeJSONObject([]byte(`{"messages":[]}`)); err != nil {
		t.Fatalf("正常对象不应失败: %v", err)
	}
}

// TestRewriteKeepsBodyWhenMessagesWrongType messages 键存在但不是数组时不能覆盖：
// 覆盖等于丢掉客户端内容（字符串/对象/null 都属此形态）。
func TestRewriteKeepsBodyWhenMessagesWrongType(t *testing.T) {
	t.Parallel()

	for _, body := range []string{
		`{"messages":"hello","model":"m"}`,
		`{"messages":{"a":1},"model":"m"}`,
		`{"messages":null,"model":"m"}`,
	} {
		if got := Rewrite([]byte(body), "SYS"); string(got) != body {
			t.Errorf("Rewrite(%s)=%s want 原样返回", body, got)
		}

		if got := Append([]byte(body), "SYS"); string(got) != body {
			t.Errorf("Append(%s)=%s want 原样返回", body, got)
		}
	}

	// 键不存在时仍按既有语义补一条 system。
	got := Rewrite([]byte(`{"model":"m"}`), "SYS")
	if !strings.Contains(string(got), `"role":"system"`) {
		t.Fatalf("无 messages 键应插入 system: %s", got)
	}
}
