package upstream

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

func TestPrepareBodyForcesStream(t *testing.T) {
	out := PrepareBodyOpt([]byte(`{"model":"glm-5.2","messages":[]}`), true)
	var m map[string]any
	json.Unmarshal(out, &m)
	if m["stream"] != true {
		t.Errorf("stream=%v", m["stream"])
	}
}

func TestPrepareBodyToolChoiceFunctionObject(t *testing.T) {
	out := PrepareBodyOpt([]byte(`{"tool_choice":{"type":"function","function":{"name":"get_weather"}},"tools":[{"type":"function"}]}`), true)
	var m map[string]any
	json.Unmarshal(out, &m)
	if m["tool_choice"] != "get_weather" {
		t.Errorf("tool_choice=%v", m["tool_choice"])
	}
	if _, ok := m["tools"]; !ok {
		t.Error("tools should be kept for function choice")
	}
}

func TestPrepareBodyToolChoiceNone(t *testing.T) {
	for _, in := range []string{
		`{"tool_choice":"none","tools":[{}],"functions":[{}]}`,
		`{"tool_choice":{"type":"none"},"tools":[{}]}`,
	} {
		out := PrepareBodyOpt([]byte(in), true)
		var m map[string]any
		json.Unmarshal(out, &m)
		if _, ok := m["tool_choice"]; ok {
			t.Errorf("%s: tool_choice should be deleted", in)
		}
		if _, ok := m["tools"]; ok {
			t.Errorf("%s: tools should be deleted", in)
		}
		if _, ok := m["functions"]; ok {
			t.Errorf("%s: functions should be deleted", in)
		}
	}
}

func TestPrepareBodyToolChoiceAuto(t *testing.T) {
	out := PrepareBodyOpt([]byte(`{"tool_choice":{"type":"auto"}}`), true)
	var m map[string]any
	json.Unmarshal(out, &m)
	if m["tool_choice"] != "auto" {
		t.Errorf("tool_choice=%v", m["tool_choice"])
	}
}

func TestPrepareBodyInvalidJSON(t *testing.T) {
	in := []byte(`{broken`)
	out := PrepareBodyOpt(in, true)
	if string(out) != string(in) {
		t.Error("invalid json should pass through unchanged")
	}
}

const sseFixture = "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"你好\"}}]}\n\n" +
	"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"，世界\"}}]}\n\n" +
	"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7}}\n\n" +
	"data: [DONE]\n\n"

func TestAggregate(t *testing.T) {
	resp, err := Aggregate(strings.NewReader(sseFixture))
	if err != nil {
		t.Fatal(err)
	}
	if resp["object"] != "chat.completion" {
		t.Errorf("object=%v", resp["object"])
	}
	if resp["model"] != "glm-5.2" {
		t.Errorf("model=%v", resp["model"])
	}
	choices := resp["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "你好，世界" {
		t.Errorf("content=%q", msg["content"])
	}
	if msg["role"] != "assistant" {
		t.Errorf("role=%v", msg["role"])
	}
	if choices[0].(map[string]any)["finish_reason"] != "stop" {
		t.Errorf("finish_reason=%v", choices[0].(map[string]any)["finish_reason"])
	}
	usage := resp["usage"].(map[string]any)
	if usage["total_tokens"].(float64) != 7 {
		t.Errorf("usage=%v", usage)
	}
}

func TestAggregateSkipsNonDataLines(t *testing.T) {
	raw := ": comment\n\n" + sseFixture
	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "你好，世界" {
		t.Errorf("content=%q", msg["content"])
	}
}

func TestAggregateToolCalls(t *testing.T) {
	// 流式 tool_calls：首片带 id/type/name + 空 arguments，后续只带 arguments 片段
	raw := `data: {"id":"x1","model":"deepseek-v4-pro","created":1,"choices":[{"index":0,"delta":{"role":"assistant","content":"","tool_calls":[{"id":"call_a","type":"function","function":{"name":"get_weather","arguments":""},"index":0}]}}],"usage":null}

data: {"id":"x1","choices":[{"index":0,"delta":{"tool_calls":[{"function":{"arguments":"{\"city\":"},"index":0}]}}]}

data: {"id":"x1","choices":[{"index":0,"delta":{"tool_calls":[{"function":{"arguments":"\"北京\"}"},"index":0}]}}]}

data: {"id":"x1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"total_tokens":11}}

data: [DONE]

`
	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	choice := resp["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason=%v", choice["finish_reason"])
	}
	msg := choice["message"].(map[string]any)
	calls, ok := msg["tool_calls"].([]map[string]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("tool_calls=%#v", msg["tool_calls"])
	}
	if calls[0]["id"] != "call_a" || calls[0]["type"] != "function" {
		t.Errorf("call meta=%v", calls[0])
	}
	fn := calls[0]["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Errorf("fn.name=%v", fn["name"])
	}
	if fn["arguments"] != `{"city":"北京"}` {
		t.Errorf("fn.arguments=%q", fn["arguments"])
	}
}

// TestStripToolCallNames 直测跨帧 name 收敛：首片保留 name、同 index 后续分片删除
// name 键（空串或重复非空串都删），不同 index 互不串扰，非 tool_calls 帧零影响。
func TestStripToolCallNames(t *testing.T) {
	seen := map[int]bool{}
	assertStripFirstChunk(t, seen)
	assertStripSubsequentChunks(t, seen)
	assertStripOtherIndex(t, seen)
	assertStripContentOnlyUntouched(t, seen)
}

// stripTestFrame 构造单 tool_call 的 delta 帧（name/args 为空则不写对应键）。
func stripTestFrame(idx float64, name, args string) map[string]any {
	fn := map[string]any{}
	if name != "" {
		fn["name"] = name
	}
	if args != "" {
		fn["arguments"] = args
	}
	return map[string]any{"choices": []any{
		map[string]any{"delta": map[string]any{"tool_calls": []any{
			map[string]any{"index": idx, "function": fn},
		}}},
	}}
}

// stripTestFn 取 stripTestFrame 帧里的 function 对象。
func stripTestFn(f map[string]any) map[string]any {
	return f["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
}

// assertStripFirstChunk 首片带 name：保留，seen 建立。
func assertStripFirstChunk(t *testing.T, seen map[int]bool) {
	t.Helper()
	f0 := stripTestFrame(0, "lookup", "")
	stripToolCallNames(f0, seen)
	if !seen[0] {
		t.Fatal("index 0 should be marked seen after first chunk")
	}
	if stripTestFn(f0)["name"] != "lookup" {
		t.Errorf("first chunk name=%v want lookup", stripTestFn(f0)["name"])
	}
}

// assertStripSubsequentChunks 同 index 后续分片（空串或重复非空 name）删除 name 键，
// arguments 原样保留。
func assertStripSubsequentChunks(t *testing.T, seen map[int]bool) {
	t.Helper()
	// 后续 chunk name 为空串：删除 name 键
	f1 := stripTestFrame(0, "", `{"term":"x"}`)
	stripToolCallNames(f1, seen)
	if _, ok := stripTestFn(f1)["name"]; ok {
		t.Errorf("subsequent empty name should be stripped: %#v", stripTestFn(f1))
	}
	if stripTestFn(f1)["arguments"] != `{"term":"x"}` {
		t.Errorf("arguments altered: %#v", stripTestFn(f1)["arguments"])
	}
	// 后续 chunk 重复非空 name（上游噪声）：同样删除，arguments 原样
	f2 := stripTestFrame(0, "lookup", "y")
	stripToolCallNames(f2, seen)
	if _, ok := stripTestFn(f2)["name"]; ok {
		t.Errorf("subsequent duplicate non-empty name should be stripped: %#v", stripTestFn(f2))
	}
	if stripTestFn(f2)["arguments"] != "y" {
		t.Errorf("arguments altered: %#v", stripTestFn(f2)["arguments"])
	}
}

// assertStripOtherIndex 不同 index 互不串扰：index 1 首片保留 name。
func assertStripOtherIndex(t *testing.T, seen map[int]bool) {
	t.Helper()
	f3 := stripTestFrame(1, "other", "")
	stripToolCallNames(f3, seen)
	if stripTestFn(f3)["name"] != "other" {
		t.Errorf("index 1 first name=%v want other", stripTestFn(f3)["name"])
	}
	if !seen[1] {
		t.Error("index 1 should be marked seen")
	}
}

// assertStripContentOnlyUntouched 非 tool_calls 帧（content only）零影响。
func assertStripContentOnlyUntouched(t *testing.T, seen map[int]bool) {
	t.Helper()
	f4 := map[string]any{"choices": []any{
		map[string]any{"delta": map[string]any{"content": "hi"}},
	}}
	stripToolCallNames(f4, seen)
	if got := f4["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any); len(got) != 1 || got["content"] != "hi" {
		t.Errorf("content-only frame altered: %#v", got)
	}
}

// TestStreamToolCallNameOnce 11 帧 tool_call：首帧 name=Bash，后续 10 帧不得携带
// name 键，arguments 逐帧原样透传（issue #82：累加型客户端把每个分片 name 拼接成
// Bash×帧数；正确行为是 name 只在首帧出现一次）。
func TestStreamToolCallNameOnce(t *testing.T) {
	const nFrames = 11
	var sb strings.Builder
	for i := range nFrames {
		sb.WriteString(`data: {"id":"x1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"Bash","arguments":"arg` + string(rune('0'+i)) + `"}}]}}]}`)
		sb.WriteString("\n\n")
	}
	sb.WriteString("data: [DONE]\n\n")

	frames, done := streamFrames(t, sb.String())
	if done != 1 {
		t.Fatalf("done=%d want 1", done)
	}
	if len(frames) != nFrames {
		t.Fatalf("frames=%d want %d", len(frames), nFrames)
	}
	assertToolCallNameOnceFrames(t, frames)
}

// assertToolCallNameOnceFrames 断言 name 只出现在首帧（值 Bash），arguments 逐帧原样。
func assertToolCallNameOnceFrames(t *testing.T, frames []map[string]any) {
	t.Helper()
	gotName := 0
	for i, fr := range frames {
		chs, _ := fr["choices"].([]any)
		d, _ := chs[0].(map[string]any)["delta"].(map[string]any)
		tcs, _ := d["tool_calls"].([]any)
		if len(tcs) != 1 {
			t.Fatalf("frame %d tool_calls len=%d want 1", i, len(tcs))
		}
		fn, _ := tcs[0].(map[string]any)["function"].(map[string]any)
		if _, ok := fn["name"]; ok {
			gotName++
			if i != 0 || fn["name"] != "Bash" {
				t.Errorf("frame %d unexpected name=%v (name 只能出现在首帧且为 Bash)", i, fn["name"])
			}
		}
		if want := "arg" + string(rune('0'+i)); fn["arguments"] != want {
			t.Errorf("frame %d arguments=%v want %q", i, fn["arguments"], want)
		}
	}
	if gotName != 1 {
		t.Errorf("name 出现帧数=%d want 1", gotName)
	}
}

// TestStreamToolCallParallelFragments 多 tool_call（index 0 与 1 并行分片交错下发）：
// 每个 index 只保留自己的首帧 name，后续分片互不串扰、arguments 各自原样。
func TestStreamToolCallParallelFragments(t *testing.T) {
	raw := "data: {\"id\":\"x1\",\"object\":\"chat.completion.chunk\",\"created\":0,\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_0\",\"type\":\"function\",\"function\":{\"name\":\"Bash\",\"arguments\":\"\"}}]}}]}\n\n" +
		"data: {\"id\":\"x1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":1,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"Read\",\"arguments\":\"\"}}]}}]}\n\n" +
		"data: {\"id\":\"x1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"Bash\",\"arguments\":\"a0\"}}]}}]}\n\n" +
		"data: {\"id\":\"x1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":1,\"function\":{\"name\":\"Read\",\"arguments\":\"a1\"}}]}}]}\n\n" +
		"data: {\"id\":\"x1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"

	frames, done := streamFrames(t, raw)
	if done != 1 {
		t.Fatalf("done=%d want 1", done)
	}

	argByIndex := map[int]string{}
	nameCount := map[int]int{}
	for _, fr := range frames {
		chs, _ := fr["choices"].([]any)
		d, _ := chs[0].(map[string]any)["delta"].(map[string]any)
		tcs, _ := d["tool_calls"].([]any)
		for _, tci := range tcs {
			tc, _ := tci.(map[string]any)
			idx := int(tc["index"].(float64))
			fn, _ := tc["function"].(map[string]any)
			if _, ok := fn["name"]; ok {
				nameCount[idx]++
			}
			if a, _ := fn["arguments"].(string); a != "" {
				argByIndex[idx] = a
			}
		}
	}
	// 每个 index 恰好出现一次 name，arguments 逐片原样（a0/a1 各自保留）
	if nameCount[0] != 1 || nameCount[1] != 1 {
		t.Errorf("name 出现次数 index0=%d index1=%d，各 want 1", nameCount[0], nameCount[1])
	}
	if argByIndex[0] != "a0" || argByIndex[1] != "a1" {
		t.Errorf("arguments index0=%q index1=%q want a0/a1", argByIndex[0], argByIndex[1])
	}
}

// TestStreamToolCallNoiseEmptyName 上游后续帧带空串 name（噪声形态）→ 输出帧无 name 键。
// 键缺失是比空串更安全的形态，客户端「键缺失则保留旧值」不会清空工具名。
func TestStreamToolCallNoiseEmptyName(t *testing.T) {
	raw := "data: {\"id\":\"x1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_a\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"\"}}]}}]}\n\n" +
		"data: {\"id\":\"x1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"\",\"arguments\":\"arg1\"}}]}}]}\n\n" +
		"data: {\"id\":\"x1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"\",\"arguments\":\"arg2\"}}]}}]}\n\n" +
		"data: {\"id\":\"x1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"

	frames, done := streamFrames(t, raw)
	if done != 1 {
		t.Fatalf("done=%d want 1", done)
	}
	for i, fr := range frames {
		chs, _ := fr["choices"].([]any)
		d, _ := chs[0].(map[string]any)["delta"].(map[string]any)
		tcs, _ := d["tool_calls"].([]any)
		for _, tci := range tcs {
			tc, _ := tci.(map[string]any)
			fn, _ := tc["function"].(map[string]any)
			if i == 0 {
				if fn["name"] != "lookup" {
					t.Errorf("frame 0 name=%v want lookup", fn["name"])
				}
				continue
			}
			if _, ok := fn["name"]; ok {
				t.Errorf("frame %d: 空串 name 应被剥离为键缺失, got %#v", i, fn)
			}
		}
	}
}

// TestStreamToolCallOverwriteClientSemantics 覆盖型语义验证：模拟「键缺失则保留旧值」
// 的覆盖型客户端（name ?? state.name / if (name) state.name = name），在输出流上逐帧
// 重建 name，最终必须收敛为 Bash——证明键缺失形态不会清空工具名。
func TestStreamToolCallOverwriteClientSemantics(t *testing.T) {
	raw := "data: {\"id\":\"x1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_a\",\"type\":\"function\",\"function\":{\"name\":\"Bash\",\"arguments\":\"\"}}]}}]}\n\n" +
		"data: {\"id\":\"x1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"\",\"arguments\":\"{\\\"cmd\\\":\\\"ls\\\"}\"}}]}}]}\n\n" +
		"data: {\"id\":\"x1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"Bash\",\"arguments\":\"\"}}]}}]}\n\n" +
		"data: [DONE]\n\n"

	frames, done := streamFrames(t, raw)
	if done != 1 {
		t.Fatalf("done=%d want 1", done)
	}
	state := map[int]string{}
	reconstructed := map[int]string{}
	for _, fr := range frames {
		chs, _ := fr["choices"].([]any)
		d, _ := chs[0].(map[string]any)["delta"].(map[string]any)
		tcs, _ := d["tool_calls"].([]any)
		for _, tci := range tcs {
			tc, _ := tci.(map[string]any)
			idx := int(tc["index"].(float64))
			fn, _ := tc["function"].(map[string]any)
			// 覆盖型语义：键缺失 → ?? 保留旧值；非空 name → 覆盖。
			if name, ok := fn["name"]; ok {
				state[idx] = name.(string)
			}
			if state[idx] != "" {
				reconstructed[idx] = state[idx]
			}
		}
	}
	// 覆盖型客户端重建后最终 name 必须是 Bash（首帧建立，后续空/重复分片均不破坏）。
	if len(reconstructed) != 1 || reconstructed[0] != "Bash" {
		t.Errorf("覆盖型重建 name=%v want map[0:Bash]", reconstructed)
	}
}

// streamFrames 把原始 SSE 输入经 Stream 处理后解析出所有 JSON 帧及 [DONE] 计数。
func streamFrames(t *testing.T, raw string) (frames []map[string]any, doneCount int) {
	t.Helper()
	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	for _, ln := range strings.Split(body, "\n") {
		ln = strings.TrimSpace(ln)
		if strings.HasPrefix(ln, "data: [DONE]") {
			doneCount++
			continue
		}
		if strings.HasPrefix(ln, "data: ") {
			var obj map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(ln, "data: ")), &obj); err != nil {
				t.Fatalf("bad frame %q: %v", ln, err)
			}
			frames = append(frames, obj)
		}
	}
	return frames, doneCount
}

func TestNormalizeFrame(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]any
		want string // 规范化后 marshal 的期望 JSON（Go map 键按字典序输出）
	}{
		{
			"empty content/refusal and finish_reason empty string",
			map[string]any{"id": "x", "choices": []any{
				map[string]any{"index": 0, "delta": map[string]any{"content": "", "refusal": ""}, "finish_reason": ""},
			}},
			`{"choices":[{"delta":{},"finish_reason":null,"index":0}],"id":"x","object":"chat.completion.chunk","usage":null}`,
		},
		{
			"non-empty tool_calls kept",
			map[string]any{"choices": []any{
				map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"id": "c1", "type": "function"}}}},
			}},
			`{"choices":[{"delta":{"tool_calls":[{"id":"c1","type":"function"}]},"finish_reason":null,"index":0}],"id":"chatcmpl-wb2api","object":"chat.completion.chunk","usage":null}`,
		},
		{
			"empty tool_calls list dropped",
			map[string]any{"choices": []any{
				map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{}, "content": "hi"}},
			}},
			`{"choices":[{"delta":{"content":"hi"},"finish_reason":null,"index":0}],"id":"chatcmpl-wb2api","object":"chat.completion.chunk","usage":null}`,
		},
		{
			"empty placeholder function_call dropped",
			map[string]any{"choices": []any{
				map[string]any{"index": 0, "delta": map[string]any{"function_call": map[string]any{"name": "", "arguments": ""}}},
			}},
			`{"choices":[{"delta":{},"finish_reason":null,"index":0}],"id":"chatcmpl-wb2api","object":"chat.completion.chunk","usage":null}`,
		},
		{
			"top-level unknown fields dropped, usage null when absent",
			map[string]any{"id": "x", "object": "chat.completion.chunk", "created": 1, "junk": "noise", "choices": []any{
				map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"},
			}},
			`{"choices":[{"delta":{},"finish_reason":"stop","index":0}],"created":1,"id":"x","object":"chat.completion.chunk","usage":null}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw, err := json.Marshal(normalizeFrame(c.in))
			if err != nil {
				t.Fatal(err)
			}
			if string(raw) != c.want {
				t.Errorf("got  %s\nwant %s", raw, c.want)
			}
		})
	}
}

func TestStreamNormalizesFrames(t *testing.T) {
	// 混合噪声帧：空 content/reasoning/refusal/function_call + 空 tool_calls + 顶层非标字段，
	// 随后非空 content + tool_calls 帧，最后 finish/usage 帧。
	raw := "data: {\"id\":\"x1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\",\"reasoning_content\":\"\",\"refusal\":\"\",\"tool_calls\":[],\"function_call\":{\"name\":\"\",\"arguments\":\"\"}},\"finish_reason\":\"\"}],\"extra_field\":\"junk\"}\n\n" +
		"data: {\"id\":\"x1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\",\"tool_calls\":[{\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"{\\\"city\\\":\\\"北京\\\"}\"},\"index\":0}]},\"finish_reason\":\"\"}]}\n\n" +
		"data: {\"id\":\"x1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"total_tokens\":7}}\n\n" +
		"data: [DONE]\n\n"

	frames, done := streamFrames(t, raw)
	if done != 1 {
		t.Fatalf("done frames=%d want 1", done)
	}
	if len(frames) != 3 {
		t.Fatalf("frames=%d want 3", len(frames))
	}

	// 帧 1：噪声全剔除，finish_reason ""→null，usage 缺失→null，顶层非标字段剥除
	assertNormalizedFrame0(t, frames[0])
	// 帧 2：非空 content 与 tool_calls 保留，finish_reason ""→null
	assertNormalizedFrame1(t, frames[1])
	// 帧 3：finish_reason 非空保留，usage 保留
	assertNormalizedFrame2(t, frames[2])
}

// assertNormalizedFrame0 断言首帧：顶层非标字段剥除、空 delta 噪声全剔除、finish/usage 归 null。
func assertNormalizedFrame0(t *testing.T, f0 map[string]any) {
	t.Helper()
	if _, ok := f0["extra_field"]; ok {
		t.Error("top-level extra_field should be dropped")
	}
	if f0["usage"] != nil {
		t.Errorf("usage should be null when absent, got %v", f0["usage"])
	}
	ch0 := f0["choices"].([]any)[0].(map[string]any)
	if ch0["finish_reason"] != nil {
		t.Errorf("frame1 finish_reason=%v want null", ch0["finish_reason"])
	}
	d := ch0["delta"].(map[string]any)
	// role 是合法白名单键保留；空 content/reasoning/refusal/tool_calls/function_call 噪声全剔除
	if len(d) != 1 || d["role"] != "assistant" {
		t.Errorf("frame1 delta should only keep role, got %#v", d)
	}
	for _, noise := range []string{"content", "reasoning_content", "refusal", "tool_calls", "function_call"} {
		if _, ok := d[noise]; ok {
			t.Errorf("frame1 delta should drop %q, got %#v", noise, d)
		}
	}
}

// assertNormalizedFrame1 断言第二帧：非空 content 与 tool_calls 保留，空 finish_reason 归 null。
func assertNormalizedFrame1(t *testing.T, f1 map[string]any) {
	t.Helper()
	ch1 := f1["choices"].([]any)[0].(map[string]any)
	d1 := ch1["delta"].(map[string]any)
	if d1["content"] != "hello" {
		t.Errorf("frame2 content=%v", d1["content"])
	}
	tcs, ok := d1["tool_calls"].([]any)
	if !ok || len(tcs) != 1 {
		t.Fatalf("frame2 tool_calls=%#v", d1["tool_calls"])
	}
	if ch1["finish_reason"] != nil {
		t.Errorf("frame2 finish_reason=%v want null (input empty string)", ch1["finish_reason"])
	}
}

// assertNormalizedFrame2 断言第三帧：finish_reason 与 usage 均保留。
func assertNormalizedFrame2(t *testing.T, f2 map[string]any) {
	t.Helper()
	ch2 := f2["choices"].([]any)[0].(map[string]any)
	if ch2["finish_reason"] != "stop" {
		t.Errorf("frame3 finish_reason=%v want stop", ch2["finish_reason"])
	}
	if f2["usage"].(map[string]any)["total_tokens"].(float64) != 7 {
		t.Errorf("frame3 usage=%v", f2["usage"])
	}
}

// TestStreamFirstIdPassthrough 帧混合（首帧有 id / 中间帧无 id / 空串 id）：输出每帧 id
// 必须连续一致（取首帧真实值），不再一律 chatcmpl-wb2api（issue #35 后台聚合：透传流里
// 每帧同 id 才能按消息归并）。
func TestStreamFirstIdPassthrough(t *testing.T) {
	raw := "data: {\"id\":\"chatcmpl-upstream-9\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello\"}}]}\n\n" +
		// 中间帧无 id：应复用首帧 id。
		"data: {\"object\":\"chat.completion.chunk\",\"created\":1,\"choices\":[{\"index\":0,\"delta\":{\"content\":\" world\"}}]}\n\n" +
		// 中间帧 id 为空串：同样复用首帧 id。
		"data: {\"id\":\"\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"!\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"

	frames, done := streamFrames(t, raw)
	if done != 1 {
		t.Fatalf("done=%d want 1", done)
	}
	if len(frames) != 3 {
		t.Fatalf("frames=%d want 3", len(frames))
	}
	for i, fr := range frames {
		if got := fr["id"]; got != "chatcmpl-upstream-9" {
			t.Errorf("frame %d id=%v want chatcmpl-upstream-9 (首帧真实 id 续传)", i, got)
		}
	}
}

// TestStreamNoIdFallsBackToSentinel 全流无任何真实 id → 兜底 chatcmpl-wb2api
// （整流无 id 时的既有哨兵，帧与帧之间仍一惯性存在）。
func TestStreamNoIdFallsBackToSentinel(t *testing.T) {
	raw := "data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{}," +
		"\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	frames, _ := streamFrames(t, raw)
	if len(frames) != 2 {
		t.Fatalf("frames=%d want 2", len(frames))
	}
	for i, fr := range frames {
		if got := fr["id"]; got != "chatcmpl-wb2api" {
			t.Errorf("frame %d id=%v want sentinel chatcmpl-wb2api (无真实 id)", i, got)
		}
	}
}

func TestStreamDoneFallback(t *testing.T) {
	// 上游流在无 [DONE] 时 EOF，Stream 必须兜底写一个 [DONE]
	rec := httptest.NewRecorder()
	err := Stream(rec, strings.NewReader("data: {\"id\":\"x1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n"))
	if err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	if !strings.HasSuffix(strings.TrimRight(body, "\n"), "data: [DONE]") {
		t.Errorf("missing [DONE] fallback: %q", body)
	}

	// 已有 [DONE] 时只写一次，不重复
	rec2 := httptest.NewRecorder()
	if err := Stream(rec2, strings.NewReader("data: {\"id\":\"x1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(rec2.Body.String(), "data: [DONE]"); n != 1 {
		t.Errorf("[DONE] count=%d want 1: %q", n, rec2.Body.String())
	}
}

func TestStreamPassthrough(t *testing.T) {
	rec := httptest.NewRecorder()
	err := Stream(rec, strings.NewReader(sseFixture))
	if err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "你好") || !strings.Contains(body, "data: [DONE]") {
		t.Errorf("body missing chunks: %q", body)
	}
	// 逐行仍是合法 SSE（每行以 data: 开头或是空行）
	for _, ln := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		if ln != "" && !strings.HasPrefix(ln, "data: ") {
			t.Errorf("bad line: %q", ln)
		}
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/event-stream") {
		t.Errorf("content-type=%q", ct)
	}
}

// TestAggregateEmptyStreamCases 覆盖空流检测：0 有效事件必须报错、[DONE] 即 break、
// [DONE] 后垃圾不进聚合、正常聚合回归。
func TestAggregateEmptyStreamCases(t *testing.T) {
	// 正常回归基流：content + finish_reason + usage，[DONE] 收尾。
	valid := "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"total_tokens\":7}}\n\n" +
		"data: [DONE]\n\n"

	cases := []struct {
		name    string
		raw     string
		wantErr bool
		// 回归断言（仅在 wantErr=false 时校验）
		wantContent string
		wantUsage   float64
	}{
		{
			name:    "空流（EOF 即止）",
			raw:     "",
			wantErr: true,
		},
		{
			name:    "只有注释行和空行加 DONE",
			raw:     ": comment\n\n: another comment\n\ndata: [DONE]\n\n",
			wantErr: true,
		},
		{
			name:    "DONE 后跟垃圾帧不进聚合",
			raw:     valid[:len(valid)-len("data: [DONE]\n\n")] + "data: [DONE]\n\ndata: {\"junk\":\"should not aggregate\"}\n\n",
			wantErr: false,
			// 与 valid 基流一致的聚合期望
			wantContent: "hi",
			wantUsage:   7,
		},
		{
			name:        "正常流回归",
			raw:         valid,
			wantErr:     false,
			wantContent: "hi",
			wantUsage:   7,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, err := Aggregate(strings.NewReader(c.raw))
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (resp=%v)", resp)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
			if msg["content"] != c.wantContent {
				t.Errorf("content=%q want %q", msg["content"], c.wantContent)
			}
			if u, ok := resp["usage"].(map[string]any); ok {
				if u["total_tokens"].(float64) != c.wantUsage {
					t.Errorf("usage=%v want %v", u["total_tokens"], c.wantUsage)
				}
			} else {
				t.Errorf("usage missing")
			}
		})
	}
}

// TestAggregateEmptyStreamError 校验空流错误信息形如约定文案。
func TestAggregateEmptyStreamError(t *testing.T) {
	_, err := Aggregate(strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "no valid data events") {
		t.Fatalf("err=%v", err)
	}
}

// TestStreamEmptyFramesCase 覆盖流式空流检测：0 有效帧时写 error 帧（error 字段存活）,
// 恰好一个 [DONE]，并返回非 nil error。
func TestStreamEmptyFramesCase(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"空流", ""},
		{"只有注释行", ": comment\n\n"},
		{"只有 DONE", "data: [DONE]\n\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			err := Stream(rec, strings.NewReader(c.raw))
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			assertEmptyStreamErrorFrame(t, rec.Body.String())
		})
	}
}

// assertEmptyStreamErrorFrame 断言空流响应恰好一个 [DONE] 且带完整的空流 error 帧。
func assertEmptyStreamErrorFrame(t *testing.T, body string) {
	t.Helper()
	if n := strings.Count(body, "data: [DONE]"); n != 1 {
		t.Errorf("[DONE] count=%d want 1: %q", n, body)
	}
	if !findEmptyStreamError(body) {
		t.Errorf("error frame absent or error field stripped: %q", body)
	}
}

// findEmptyStreamError 在 SSE body 里查找空流 error 帧（error 字段未被白名单剥掉）。
func findEmptyStreamError(body string) bool {
	for _, ln := range strings.Split(body, "\n") {
		payload, ok := strings.CutPrefix(strings.TrimSpace(ln), "data: ")
		if !ok || payload == "[DONE]" {
			continue
		}
		var e map[string]any
		if json.Unmarshal([]byte(payload), &e) != nil {
			continue
		}
		em, ok := e["error"].(map[string]any)
		if ok && em["message"] == "empty upstream stream" && em["type"] == "upstream_error" {
			return true
		}
	}
	return false
}

// TestStreamGarbageAfterDone 校验 DONE 之后的垃圾帧不出现在响应里。
func TestStreamGarbageAfterDone(t *testing.T) {
	raw := "data: {\"id\":\"x1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n" +
		"data: [DONE]\n\n" +
		"data: {\"should\":\"not appear\"}\n\n"
	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	if strings.Contains(body, "should") {
		t.Errorf("garbage after DONE leaked into response: %q", body)
	}
	if n := strings.Count(body, "data: [DONE]"); n != 1 {
		t.Errorf("[DONE] count=%d want 1: %q", n, body)
	}
	// 有效帧仍被透传
	if !strings.Contains(body, "hello") {
		t.Errorf("valid frame missing: %q", body)
	}
}

// TestStreamNormalPassthroughRegression 校验正常透传回归：帧被 normalize 后透传、
// 末尾恰好一个 [DONE]、无 error 帧；上游漏发 DONE 时自动补。
func TestStreamNormalPassthroughRegression(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"带 DONE 的正常流", sseFixture},
		{"漏发 DONE 自动补", "data: {\"id\":\"x1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			if err := Stream(rec, strings.NewReader(c.raw)); err != nil {
				t.Fatal(err)
			}
			body := rec.Body.String()
			if strings.Contains(body, `"error"`) {
				t.Errorf("unexpected error frame: %q", body)
			}
			if n := strings.Count(body, "data: [DONE]"); n != 1 {
				t.Errorf("[DONE] count=%d want 1: %q", n, body)
			}
			// 帧被规范化：含 "id" 且有标准 object 字段
			if !strings.Contains(body, `"object":"chat.completion.chunk"`) {
				t.Errorf("frame not normalized: %q", body)
			}
		})
	}
}

// TestNormalizeUsageCacheAliasesMirrorsNestedHit / PreservesZeroResult 自
// usage_test.go 迁入（PR #57 原新文件按仓库规则不收，核心断言保留在此——
// normalize 钩子就挂在 sse.go 的 Aggregate/normalizeFrame 两个出口）。
func TestNormalizeUsageCacheAliasesMirrorsNestedHit(t *testing.T) {
	usage := map[string]any{
		"prompt_tokens":            21041.0,
		"completion_tokens":        8.0,
		"total_tokens":             21049.0,
		"cache_read_input_tokens":  0.0,
		"cached_tokens":            0.0,
		"prompt_cache_hit_tokens":  0.0,
		"prompt_cache_miss_tokens": 177.0,
		"prompt_tokens_details": map[string]any{
			"cached_tokens": 20864.0,
		},
	}

	got := normalizeUsageCacheAliases(usage)

	for _, key := range []string{
		"cache_read_input_tokens",
		"cached_tokens",
		"prompt_cache_hit_tokens",
	} {
		if got[key] != 20864.0 {
			t.Fatalf("%s=%v want 20864", key, got[key])
		}
	}
	details := got["prompt_tokens_details"].(map[string]any)
	if details["cached_tokens"] != 20864.0 {
		t.Fatalf("prompt_tokens_details.cached_tokens=%v want 20864", details["cached_tokens"])
	}
}

func TestNormalizeUsageCacheAliasesPreservesZeroResult(t *testing.T) {
	usage := map[string]any{
		"prompt_tokens":           35.0,
		"completion_tokens":       2.0,
		"total_tokens":            37.0,
		"cache_read_input_tokens": 0.0,
		"prompt_cache_hit_tokens": 0.0,
		"prompt_tokens_details": map[string]any{
			"cached_tokens": 0.0,
		},
	}

	got := normalizeUsageCacheAliases(usage)

	details := got["prompt_tokens_details"].(map[string]any)
	if details["cached_tokens"] != 0.0 {
		t.Fatalf("prompt_tokens_details.cached_tokens=%v want 0", details["cached_tokens"])
	}
}

// TestStreamHintErrorFrameObserver 上游 error 帧必须旁路通知观察者（账号处置挂载点），
// 且透传字节不变——error 帧原文（code/msg/requestId）照常到达客户端。
// 正常数据帧不得触发观察者。移植自 OkRoromori 分支的 WithErrorFrameObserver。
func TestStreamHintErrorFrameObserver(t *testing.T) {
	const errPayload = `{"error":{"code":6004,"message":"模型限流，将在 2026-09-27 01:00:00 重置"}}`
	raw := "data: {\"id\":\"x1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: " + errPayload + "\n\n" +
		"data: [DONE]\n\n"

	var observed []string
	rec := httptest.NewRecorder()
	if err := StreamHint(rec, strings.NewReader(raw), nil, WithErrorFrameObserver(func(payload string) {
		observed = append(observed, payload)
	})); err != nil {
		t.Fatal(err)
	}

	if len(observed) != 1 || observed[0] != errPayload {
		t.Fatalf("观察者回调 = %v want [%s]", observed, errPayload)
	}
	// 透传字节不变：error 帧原文必须仍在响应里（error-passthrough 语义）。
	if !strings.Contains(rec.Body.String(), "6004") {
		t.Fatalf("error 帧原文未透传: %s", rec.Body.String())
	}

	// 正常流（无 error 帧）：观察者零回调。
	var normalObserved int
	raw2 := "data: {\"id\":\"x2\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n" +
		"data: [DONE]\n\n"
	rec2 := httptest.NewRecorder()
	if err := StreamHint(rec2, strings.NewReader(raw2), nil, WithErrorFrameObserver(func(payload string) {
		normalObserved++
	})); err != nil {
		t.Fatal(err)
	}
	if normalObserved != 0 {
		t.Fatalf("正常流触发了观察者 %d 次，want 0", normalObserved)
	}
}

func TestUserResourceDetailedWithExpirySnapshot(t *testing.T) {
	now := time.Now().In(softRateResetLoc)
	soon := now.Add(24 * time.Hour).Truncate(time.Second)
	later := now.Add(10 * 24 * time.Hour).Truncate(time.Second)
	payload := `{"code":0,"data":{"Response":{"Data":{"Accounts":[` +
		`{"PackageName":"soon-a","CycleCapacitySize":10,"CycleCapacityRemain":10,"CycleCapacityUsed":0,"CycleEndTime":"` + soon.Format(packageEndLayout) + `"},` +
		`{"PackageName":"soon-b","CycleCapacitySize":15,"CycleCapacityRemain":15,"CycleCapacityUsed":0,"CycleEndTime":"` + soon.Format(packageEndLayout) + `"},` +
		`{"PackageName":"later","CycleCapacitySize":20,"CycleCapacityRemain":20,"CycleCapacityUsed":0,"CycleEndTime":"` + later.Format(packageEndLayout) + `"},` +
		`{"PackageName":"unknown","CycleCapacitySize":5,"CycleCapacityRemain":5,"CycleCapacityUsed":0}` +
		`]}}}}`
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/v2/billing/meter/get-user-resource") {
			return nil, errors.New("wrong path: " + r.URL.Path)
		}
		return jsonResp(200, payload), nil
	})

	remain, total, expiring, earliestAt, earliestRemaining, err := c.UserResourceDetailedWithExpiry(
		&auth.Auth{AccessToken: "at", UID: "u1"}, 48*time.Hour,
	)
	if err != nil {
		t.Fatalf("resource: %v", err)
	}
	if remain != 50 || total != 50 || expiring != 25 {
		t.Fatalf("remain/total/expiring=%d/%d/%d want 50/50/25", remain, total, expiring)
	}
	if earliestRemaining != 25 || !earliestAt.Equal(soon) {
		t.Fatalf("earliest=%v/%d want %v/25", earliestAt, earliestRemaining, soon)
	}
}

func TestCreditPackagesExpiryTimestamp(t *testing.T) {
	end := time.Now().In(softRateResetLoc).Add(7 * 24 * time.Hour).Truncate(time.Second)
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/v2/billing/meter/get-user-resource") {
			return nil, errors.New("wrong path: " + r.URL.Path)
		}
		return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"Accounts":[`+
			`{"PackageName":"gift","CycleCapacitySize":100,"CycleCapacityRemain":80,"CycleCapacityUsed":20,"CycleEndTime":"`+
			end.Format(packageEndLayout)+`"},`+
			`{"PackageName":"unknown","CycleCapacitySize":10,"CycleCapacityRemain":10,"CycleCapacityUsed":0}`+
			`]}}}}`), nil
	})
	packs, remain, size, err := c.CreditPackages(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("packages: %v", err)
	}
	if remain != 90 || size != 110 {
		t.Fatalf("remain/size=%d/%d want 90/110", remain, size)
	}
	var found bool
	for _, p := range packs {
		if p.Name == "gift" {
			found = p.ExpiresAt == end.UnixMilli()
		}
	}
	if !found {
		t.Fatalf("gift pack missing Unix-ms expiry: %+v", packs)
	}
}

// --- 标记修复（流式端到端，见 dsml.go）-------------------------------------

// upstreamSSEWithContent 用给定的正文分片拼一条上游 SSE：role 首帧 + N 个正文帧
// + finish 收尾帧 + [DONE]。
func upstreamSSEWithContent(chunks []string, finish string) string {
	var sb strings.Builder
	sb.WriteString(`data: {"id":"resp-1","object":"chat.completion.chunk","created":1,"model":"global:deepseek-v4.1-flash","choices":[{"index":0,"delta":{"role":"assistant"}}]}` + "\n\n")
	for _, c := range chunks {
		b, _ := json.Marshal(map[string]any{
			"id": "resp-1", "object": "chat.completion.chunk", "created": 1,
			"model": "global:deepseek-v4.1-flash",
			"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{"content": c},
			}},
		})
		sb.WriteString("data: " + string(b) + "\n\n")
	}
	b, _ := json.Marshal(map[string]any{
		"id": "resp-1", "object": "chat.completion.chunk", "created": 1,
		"model": "global:deepseek-v4.1-flash",
		"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{}, "finish_reason": finish,
		}},
	})
	sb.WriteString("data: " + string(b) + "\n\n")
	sb.WriteString("data: [DONE]\n\n")
	return sb.String()
}

// sseFrameView 是测试侧对一帧的扁平视图。
type sseFrameView struct {
	content string
	calls   []map[string]any
	finish  string
}

// viewSSE 解析网关写出的 SSE 文本。
func viewSSE(t *testing.T, body string) []sseFrameView {
	t.Helper()
	var out []sseFrameView
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(payload), &obj); err != nil {
			t.Fatalf("帧不是合法 JSON: %q (%v)", payload, err)
		}
		chs, _ := obj["choices"].([]any)
		if len(chs) == 0 {
			continue
		}
		ch, _ := chs[0].(map[string]any)
		v := sseFrameView{}
		if d, ok := ch["delta"].(map[string]any); ok {
			v.content, _ = d["content"].(string)
			if tcs, ok := d["tool_calls"].([]any); ok {
				for _, tci := range tcs {
					if tc, ok := tci.(map[string]any); ok {
						v.calls = append(v.calls, tc)
					}
				}
			}
		}
		v.finish, _ = ch["finish_reason"].(string)
		out = append(out, v)
	}
	return out
}

// TestStreamMarkupRepairEndToEnd 上游把工具调用吐成正文标记时，网关必须还原成
// delta.tool_calls：正文里不再残留标记、调用帧带正确的 name/arguments、收尾帧
// finish_reason 为 tool_calls 且出现在调用帧之后。
func TestStreamMarkupRepairEndToEnd(t *testing.T) {
	prose := "先读一遍控制器，把接口清单核实准确。\n\n"
	content := prose + mkBlock(
		mkInvoke("exec_command", mkParam("cmd", "ls -la")),
		mkInvoke("exec_command", mkParam("cmd", "pwd")),
	)
	raw := upstreamSSEWithContent([]string{content}, "stop")

	repair := NewMarkupRepair(testTools, false)
	rec := httptest.NewRecorder()
	if err := StreamHint(rec, strings.NewReader(raw), nil, WithMarkupRepair(repair)); err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	if strings.Contains(body, markupPipe) {
		t.Fatalf("透出内容里仍残留标记: %s", body)
	}
	if repair.Converted() != 2 {
		t.Fatalf("Converted 应为 2，实际 %d", repair.Converted())
	}

	frames := viewSSE(t, body)
	s := summarizeMarkupFrames(frames)
	if s.text.String() != prose {
		t.Fatalf("正文应只保留标记之外的部分\nwant %q\ngot  %q", prose, s.text.String())
	}
	assertMarkupRepairCalls(t, s.calls, []string{"ls -la", "pwd"})
	if s.finishCount != 1 {
		t.Fatalf("收尾帧应恰好 1 个，实际 %d（body=%s）", s.finishCount, body)
	}
	if frames[s.finishIdx].finish != "tool_calls" {
		t.Fatalf("收尾 finish_reason=%q want tool_calls", frames[s.finishIdx].finish)
	}
	if s.finishIdx < s.lastCallIdx {
		t.Fatalf("收尾帧（第 %d 帧）必须排在最后一个调用帧（第 %d 帧）之后", s.finishIdx, s.lastCallIdx)
	}
}

// markupRepairSummary 汇总透传帧：正文、调用帧、收尾帧位置。
type markupRepairSummary struct {
	text        strings.Builder
	calls       []map[string]any
	lastCallIdx int
	finishIdx   int
	finishCount int
}

// summarizeMarkupFrames 汇总 viewSSE 帧序列。
func summarizeMarkupFrames(frames []sseFrameView) markupRepairSummary {
	s := markupRepairSummary{lastCallIdx: -1, finishIdx: -1}
	for i, f := range frames {
		s.text.WriteString(f.content)
		for _, tc := range f.calls {
			s.calls = append(s.calls, tc)
			s.lastCallIdx = i
		}
		if f.finish != "" {
			s.finishCount++
			s.finishIdx = i
		}
	}
	return s
}

// assertMarkupRepairCalls 断言还原出的 tool_call 帧的 type/id/index/name/arguments。
func assertMarkupRepairCalls(t *testing.T, calls []map[string]any, wants []string) {
	t.Helper()
	if len(calls) != len(wants) {
		t.Fatalf("应还原 %d 个 tool_call，实际 %d", len(wants), len(calls))
	}
	for i, want := range wants {
		assertMarkupRepairCall(t, i, calls[i], want)
	}
}

// assertMarkupRepairCall 断言第 i 个 tool_call 的字段与参数。
func assertMarkupRepairCall(t *testing.T, i int, tc map[string]any, want string) {
	t.Helper()
	if tc["type"] != "function" {
		t.Fatalf("第 %d 个 tool_call type=%v want function", i, tc["type"])
	}
	if id, _ := tc["id"].(string); id == "" {
		t.Fatalf("第 %d 个 tool_call 缺 id", i)
	}
	if idx, ok := tc["index"].(float64); !ok || int(idx) != i {
		t.Fatalf("第 %d 个 tool_call index=%v", i, tc["index"])
	}
	fn, _ := tc["function"].(map[string]any)
	if fn == nil || fn["name"] != "exec_command" {
		t.Fatalf("第 %d 个 tool_call 函数名错误: %#v", i, tc)
	}
	argsJSON, _ := fn["arguments"].(string)
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		t.Fatalf("第 %d 个 tool_call arguments 不是合法 JSON: %q", i, argsJSON)
	}
	if args["cmd"] != want {
		t.Fatalf("第 %d 个 tool_call cmd=%#v want %q", i, args["cmd"], want)
	}
}

// TestStreamMarkupRepairAcrossManyChunks 上游把标记切碎成多个正文帧时同样成立。
func TestStreamMarkupRepairAcrossManyChunks(t *testing.T) {
	content := "开头\n" + mkBlock(mkInvoke("exec_command", mkParam("cmd", "ls -la"))) + "\n结尾"
	// 按 rune 边界切：真实上游的 JSON 字符串不可能把多字节字符劈成半个 rune，
	// 字节级切分会在 json.Marshal 时把非法 UTF-8 替换成 U+FFFD，等于先改坏夹具。
	raw := upstreamSSEWithContent(chunkRunes(content, 5), "stop")

	repair := NewMarkupRepair(testTools, false)
	rec := httptest.NewRecorder()
	if err := StreamHint(rec, strings.NewReader(raw), nil, WithMarkupRepair(repair)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rec.Body.String(), markupPipe) {
		t.Fatalf("透出内容里仍残留标记: %s", rec.Body.String())
	}
	if repair.Converted() != 1 {
		t.Fatalf("Converted 应为 1，实际 %d", repair.Converted())
	}
	text, calls, finish := markupTextSummary(viewSSE(t, rec.Body.String()))
	if text != "开头\n\n结尾" {
		t.Fatalf("正文错误: %q", text)
	}
	if calls != 1 {
		t.Fatalf("应还原 1 个 tool_call，实际 %d", calls)
	}
	if finish != "tool_calls" {
		t.Fatalf("收尾 finish_reason=%q want tool_calls", finish)
	}
}

// chunkRunes 按 rune 边界把 content 切成每块最多 size 个 rune。
func chunkRunes(content string, size int) []string {
	runes := []rune(content)
	chunks := make([]string, 0, len(runes)/size+1)
	for i := 0; i < len(runes); i += size {
		end := i + size
		if end > len(runes) {
			end = len(runes)
		}
		chunks = append(chunks, string(runes[i:end]))
	}
	return chunks
}

// markupTextSummary 汇总透传帧的正文、调用数与收尾 reason。
func markupTextSummary(frames []sseFrameView) (text string, calls int, finish string) {
	var sb strings.Builder
	for _, f := range frames {
		sb.WriteString(f.content)
		calls += len(f.calls)
		if f.finish != "" {
			finish = f.finish
		}
	}
	return sb.String(), calls, finish
}

// TestStreamMarkupRepairUnclosedFlushedVerbatim 未闭合的块在流尾原样回吐，不吞字节。
func TestStreamMarkupRepairUnclosedFlushedVerbatim(t *testing.T) {
	content := "说明\n" + markupOpen + mkInvoke("exec_command", mkParam("cmd", "pwd"))
	raw := upstreamSSEWithContent([]string{content}, "stop")

	repair := NewMarkupRepair(testTools, false)
	rec := httptest.NewRecorder()
	if err := StreamHint(rec, strings.NewReader(raw), nil, WithMarkupRepair(repair)); err != nil {
		t.Fatal(err)
	}
	if repair.Converted() != 0 {
		t.Fatalf("未闭合块不得转换，实际 %d", repair.Converted())
	}
	var text strings.Builder
	var calls int
	for _, f := range viewSSE(t, rec.Body.String()) {
		text.WriteString(f.content)
		calls += len(f.calls)
	}
	if calls != 0 {
		t.Fatalf("不得产出 tool_call，实际 %d", calls)
	}
	if text.String() != content {
		t.Fatalf("未闭合块应原样回吐\nwant %q\ngot  %q", content, text.String())
	}
}

// TestStreamWithoutMarkupRepairIsUnchanged 修复器不启用时，流式输出必须与修复前
// 逐字节一致（回归护栏：客户端没声明 tools、或没挂修复器）。
func TestStreamWithoutMarkupRepairIsUnchanged(t *testing.T) {
	content := mkBlock(mkInvoke("exec_command", mkParam("cmd", "ls -la")))
	raw := upstreamSSEWithContent([]string{content}, "stop")

	base := httptest.NewRecorder()
	if err := StreamHint(base, strings.NewReader(raw), nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(base.Body.String(), markupPipe) {
		t.Fatal("基线流里本应还带着标记（上游原样透传）")
	}

	disabled := httptest.NewRecorder()
	if err := StreamHint(disabled, strings.NewReader(raw), nil,
		WithMarkupRepair(NewMarkupRepair(nil, false))); err != nil {
		t.Fatal(err)
	}
	if base.Body.String() != disabled.Body.String() {
		t.Fatalf("未声明 tools 时输出被改动\nbase=%s\ngot =%s", base.Body.String(), disabled.Body.String())
	}
}

// deadlineWriter 可控的 ResponseWriter：实现 SetWriteDeadline（模拟真实连接），
// block=true 时 Write 阻塞到截止时间后返回超时错误，用于验证入站写截止。
type deadlineWriter struct {
	h        http.Header
	block    bool
	deadline time.Time
	sets     int
	writes   int
}

func (w *deadlineWriter) Header() http.Header {
	if w.h == nil {
		w.h = http.Header{}
	}
	return w.h
}

func (w *deadlineWriter) Write(p []byte) (int, error) {
	w.writes++
	if !w.block {
		return len(p), nil
	}
	if d := w.deadline; !d.IsZero() {
		if wait := time.Until(d); wait > 0 {
			time.Sleep(wait)
		}
	}
	return 0, os.ErrDeadlineExceeded
}

func (w *deadlineWriter) WriteHeader(int) {}

func (w *deadlineWriter) Flush() {}

func (w *deadlineWriter) SetWriteDeadline(t time.Time) error {
	w.deadline = t
	w.sets++
	return nil
}

// TestStreamClientWriteDeadline 慢客户端不消费时，单次写必须在截止后失败返回，
// 不能永久占住 goroutine（issue #53）。
func TestStreamClientWriteDeadline(t *testing.T) {
	w := &deadlineWriter{block: true}
	sw := newStreamWriter(w, nil)
	sw.writeTimeout = 40 * time.Millisecond

	start := time.Now()
	err := sw.stream(strings.NewReader("data: {\"choices\":[]}\n\n"))
	if err == nil {
		t.Fatal("写阻塞超过截止后应返回错误")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("写截止未生效，耗时 %v", elapsed)
	}
	if w.sets == 0 || w.writes == 0 {
		t.Fatalf("未设置写截止或未尝试写：sets=%d writes=%d", w.sets, w.writes)
	}
}

// TestStreamClientWriteDeadlineCleared 正常流不被误杀，且流结束后清空写截止，
// 避免过期 deadline 污染 keep-alive 连接的后续请求。
func TestStreamClientWriteDeadlineCleared(t *testing.T) {
	w := &deadlineWriter{}
	raw := "data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: [DONE]\n\n"

	if err := StreamHint(w, strings.NewReader(raw), nil); err != nil {
		t.Fatalf("正常流被误杀：%v", err)
	}
	if w.sets == 0 {
		t.Fatal("支持 deadline 的 writer 应设置过写截止")
	}
	if !w.deadline.IsZero() {
		t.Fatalf("流结束后写截止未清空：%v", w.deadline)
	}
}
