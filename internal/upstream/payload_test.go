package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// TestNormalizeRoles 验证出站请求体把 developer 角色归一为 system。
// 上游 role 白名单不含 developer（OpenAI 新规范的 system 别名），
// 命中即 HTTP 400 code=11-128；此处走 PrepareBodyOptWithEfforts 全链路断言。
func TestNormalizeRoles(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		body      string
		wantRoles []string // 与输出 messages 逐条对应的期望 role；len 即消息数
	}{
		{
			"developer 改写为 system",
			`{"messages":[{"role":"developer","content":"x"}]}`,
			[]string{"system"},
		},
		{
			"Developer 首字母大写改写",
			`{"messages":[{"role":"Developer","content":"x"}]}`,
			[]string{"system"},
		},
		{
			"DEVELOPER 全大写改写",
			`{"messages":[{"role":"DEVELOPER","content":"x"}]}`,
			[]string{"system"},
		},
		{
			"前后空白 TrimSpace 后改写",
			`{"messages":[{"role":" developer ","content":"x"}]}`,
			[]string{"system"},
		},
		{
			"system 原样保留",
			`{"messages":[{"role":"system","content":"x"}]}`,
			[]string{"system"},
		},
		{
			"user 原样保留",
			`{"messages":[{"role":"user","content":"x"}]}`,
			[]string{"user"},
		},
		{
			"assistant 原样保留",
			`{"messages":[{"role":"assistant","content":"x"}]}`,
			[]string{"assistant"},
		},
		{
			"tool 原样保留（不因未知而改写）",
			`{"messages":[{"role":"tool","content":"x"}]}`,
			[]string{"tool"},
		},
		{
			"messages 缺失不 panic 且其余字段不变",
			`{"model":"glm-5.2"}`,
			[]string{},
		},
		{
			"messages 为空数组不 panic",
			`{"messages":[]}`,
			[]string{},
		},
		{
			"混合消息仅 developer 被改写",
			`{"messages":[{"role":"developer","content":"a"},{"role":"user","content":"b"},{"role":"developer","content":"c"}]}`,
			[]string{"system", "user", "system"},
		},
		{
			"sanitize=false 时仍归一（与脱敏解耦）",
			`{"messages":[{"role":"developer","content":"x"}]}`,
			[]string{"system"},
		},
		{
			"非对象消息元素跳过、其余正常处理",
			`{"messages":["str",{"role":"developer","content":"x"},42]}`,
			[]string{"system"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			// 全程 sanitize=false：验证 role 归一与内容脱敏开关无关（D4）。
			out := PrepareBodyOptWithEfforts([]byte(c.body), false, nil)
			assertRoles(t, rolesFromBody(t, out), c.wantRoles)
		})
	}

	// messages 缺失时，其余字段必须原样保留（除强制 stream）。
	t.Run("messages 缺失时其余字段不变", func(t *testing.T) {
		t.Parallel()

		out := PrepareBodyOptWithEfforts([]byte(`{"model":"glm-5.2","temperature":0.7}`), false, nil)

		var obj map[string]any
		if err := json.Unmarshal(out, &obj); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}

		if obj["model"] != "glm-5.2" || obj["temperature"] != 0.7 {
			t.Errorf("其余字段被改动: %v", obj)
		}
	})
}

// rolesFromBody 解出 PrepareBodyOptWithEfforts 输出里 messages 的 role 列表
// （非对象元素跳过，不 panic）。
func rolesFromBody(t *testing.T, out []byte) []string {
	t.Helper()

	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v (out=%s)", err, out)
	}

	var got []string

	if msgs, ok := obj["messages"].([]any); ok {
		for _, m := range msgs {
			msg, ok := m.(map[string]any)
			if !ok {
				continue
			}

			if role, ok := msg["role"].(string); ok {
				got = append(got, role)
			}
		}
	}

	return got
}

// assertRoles 断言 got 与 want 逐条相等。
func assertRoles(t *testing.T, got, want []string) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("role 数量不符: got %v (%d) want %v (%d)", got, len(got), want, len(want))
	}

	for i := range got {
		if got[i] != want[i] {
			t.Errorf("role[%d] = %q want %q", i, got[i], want[i])
		}
	}
}

func TestPrepareBodyOptWithEfforts(t *testing.T) {
	t.Parallel()

	efforts := map[string][]string{
		"glm-5.2":      {"off", "low", "high"},
		"glm-5.2-mini": {"low", "medium"},
		"glm-5.2-max":  {"high", "xhigh"},
	}

	cases := []struct {
		name    string
		body    string
		efforts map[string][]string
		wantKey string // 输出应带有的 effort 字段名；空表示该字段应不存在
		wantVal string // 期望值
	}{
		{
			"downgrade to highest supported at or below request",
			`{"model":"glm-5.2-mini","reasoning_effort":"high"}`, efforts, "reasoning_effort", "medium",
		},
		{
			"floor to lowest when all supported above request",
			`{"model":"glm-5.2-max","reasoning_effort":"low"}`, efforts, "reasoning_effort", "high",
		},
		{
			"supported effort passes through unchanged",
			`{"model":"glm-5.2","reasoning_effort":"low"}`, efforts, "reasoning_effort", "low",
		},
		{
			"camelCase field name downgrades and keeps key",
			`{"model":"glm-5.2-mini","reasoningEffort":"high"}`, efforts, "reasoningEffort", "medium",
		},
		{
			"unknown model passes through",
			`{"model":"unknown","reasoning_effort":"max"}`, efforts, "reasoning_effort", "max",
		},
		{
			"unknown effort value passes through",
			`{"model":"glm-5.2","reasoning_effort":"ultra"}`, efforts, "reasoning_effort", "ultra",
		},
		{
			"empty cache passes through",
			`{"model":"glm-5.2","reasoning_effort":"max"}`,
			map[string][]string{},
			"reasoning_effort", "max",
		},
		{
			"no effort field untouched",
			`{"model":"glm-5.2-mini","messages":[]}`, efforts, "", "",
		},
		{
			"nil efforts map passes through",
			`{"model":"glm-5.2","reasoning_effort":"max"}`, nil, "reasoning_effort", "max",
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			t.Parallel()

			out := PrepareBodyOptWithEfforts([]byte(item.body), false, item.efforts)

			var m map[string]any
			if err := json.Unmarshal(out, &m); err != nil {
				t.Fatalf("unmarshal: %v (body=%s)", err, out)
			}

			if item.wantKey == "" {
				if _, ok := m["reasoning_effort"]; ok {
					t.Errorf("reasoning_effort should be absent, got %v", m["reasoning_effort"])
				}

				if _, ok := m["reasoningEffort"]; ok {
					t.Errorf("reasoningEffort should be absent, got %v", m["reasoningEffort"])
				}

				return
			}

			got, ok := m[item.wantKey].(string)
			if !ok || got != item.wantVal {
				t.Errorf("%s: got %v (%T) want %q", item.wantKey, m[item.wantKey], m[item.wantKey], item.wantVal)
			}
		})
	}
}

// TestPrepareBodyStreamOptions body 未显式带 stream_options 时注入
// {include_usage: true}（D7，官方 CLI 流式必发）；body 已带则不覆盖。
func TestPrepareBodyStreamOptions(t *testing.T) {
	t.Parallel()

	out := PrepareBodyOptWithEfforts([]byte(`{"model":"glm-5.2","messages":[]}`), false, nil)

	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v (out=%s)", err, out)
	}

	table, ok := obj["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("stream_options not injected: %v", obj["stream_options"])
	}

	if table["include_usage"] != true {
		t.Errorf("stream_options.include_usage = %v want true", table["include_usage"])
	}

	out2 := PrepareBodyOptWithEffertsPreserve(t, `{"model":"glm-5.2","messages":[],"stream_options":{"include_usage":false}}`)

	obj2, err := decodeBody(out2)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	so2, ok := obj2["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("stream_options lost: %v", obj2["stream_options"])
	}

	if so2["include_usage"] != false {
		t.Errorf("stream_options.include_usage = %v want false (not overwritten)", so2["include_usage"])
	}
}

// PrepareBodyOptWithEffertsPreserve helper：PrepareBodyOptWithEfforts 包装。
func PrepareBodyOptWithEffertsPreserve(t *testing.T, body string) []byte {
	t.Helper()
	return PrepareBodyOptWithEfforts([]byte(body), false, nil)
}

// decodeBody helper：解析 body JSON。
func decodeBody(b []byte) (map[string]any, error) {
	var obj map[string]any

	err := json.Unmarshal(b, &obj)
	if err != nil {
		return obj, fmt.Errorf("decodeBody: %w", err)
	}

	return obj, nil
}

// TestPrepareBodyDeterministic 序列化稳定性：同输入跑多遍出站字节级一致
// （prompt_cache_key 前缀命中的前提——链中不得注入时间/随机/ID 类不确定源）。
func TestPrepareBodyDeterministic(t *testing.T) {
	t.Parallel()

	inputs := []string{
		`{"model":"glm-5.2","messages":[{"role":"system","content":"你是助手"},{"role":"user","content":"你好"}],"reasoning_effort":"high"}`,
		`{"model":"deepseek-v4","messages":[{"role":"user","content":"写个函数"}],"tool_choice":{"type":"auto"},"tools":[{"type":"function","function":{"name":"f"}}]}`,
		`{"model":"glm-5.3","messages":[{"role":"developer","content":"sys"},{"role":"user","content":[{"type":"text","text":"hi"}]}]}`,
	}
	for i, in := range inputs {
		var first []byte

		for round := range 5 {
			out := PrepareBodyOptWithEfforts([]byte(in), true, map[string][]string{"glm-5.2": {"off", "low", "high"}})
			if round == 0 {
				first = out
				continue
			}

			if string(out) != string(first) {
				t.Fatalf("input #%d round %d differs from round 0:\n%s\n%s", i, round, first, out)
			}
		}
	}
}

// TestNormalizeImageURL 覆盖 OpenAI chat 多模态内容的 image_url 兼容：
// 字符串形态必须转为上游需要的对象形态；对象形态及其中字段必须原样保留；
// 无效输入不补默认值，继续交给上游返回真实错误。
func TestNormalizeImageURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		want any
	}{
		{
			name: "data url string to object",
			body: `{"messages":[{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":"data:image/png;base64,QUJD"}]}]}`,
			want: map[string]any{"url": "data:image/png;base64,QUJD"},
		},
		{
			name: "http url string to object",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":"https://example.test/a.png"}]}]}`,
			want: map[string]any{"url": "https://example.test/a.png"},
		},
		{
			name: "object with detail preserved",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD","detail":"low","mime_type":"image/png"}}]}]}`,
			want: map[string]any{"url": "data:image/png;base64,QUJD", "detail": "low", "mime_type": "image/png"},
		},
		{
			name: "invalid object url type preserved",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":123}}]}]}`,
			want: map[string]any{"url": float64(123)},
		},
		{
			name: "missing image url preserved",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url"}]}]}`,
			want: nil,
		},
		{
			name: "empty string preserved",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":""}]}]}`,
			want: "",
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			for _, sanitize := range []bool{false, true} {
				out := PrepareBodyOptWithEfforts([]byte(testCase.body), sanitize, nil)

				part := findImageURLPart(t, out, sanitize)
				if testCase.want == nil {
					if _, exists := part["image_url"]; exists {
						t.Fatalf("sanitize=%v: missing image_url should stay missing, got %#v", sanitize, part)
					}

					continue
				}

				if got := part["image_url"]; !reflect.DeepEqual(got, testCase.want) {
					t.Errorf("sanitize=%v: image_url=%#v want %#v", sanitize, got, testCase.want)
				}
			}
		})
	}
}

// findImageURLPart 从 PrepareBodyOptWithEfforts 输出里定位第一个 image_url part。
func findImageURLPart(t *testing.T, out []byte, sanitize bool) map[string]any {
	t.Helper()

	obj, err := decodeBody(out)
	if err != nil {
		t.Fatalf("sanitize=%v unmarshal: %v (out=%s)", sanitize, err, out)
	}

	msgs := obj["messages"].([]any)

	content := msgs[0].(map[string]any)["content"].([]any)
	for _, rawPart := range content {
		if candidate, ok := rawPart.(map[string]any); ok && candidate["type"] == "image_url" {
			return candidate
		}
	}

	t.Fatal("image_url part not found")

	return nil
}

// lookupToolPattern 沿 path（string 键 / int 下标）下钻取 body 里的字符串值。
func lookupToolPattern(t *testing.T, body string, path ...any) string {
	t.Helper()

	var obj map[string]any
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("out not json: %v", err)
	}

	cur := any(obj)

	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := cur.(map[string]any)
			if !ok {
				t.Fatalf("path %v: want object, got %T", path, cur)
			}

			cur = m[k]
		case int:
			l, ok := cur.([]any)
			if !ok || k >= len(l) {
				t.Fatalf("path %v: bad array at %T", path, cur)
			}

			cur = l[k]
		}
	}

	s, _ := cur.(string)

	return s
}

// TestNormalizeToolPatterns 工具 schema pattern 的 `\_` 转义归一（11129 实案：
// exa agent_run 的 `^agent\_run\_` 被 deepseek 系确定性拒收，归一后上游 200）。
// 覆盖：function.parameters 与裸 parameters 两形态、嵌套 schema、patternProperties
// 键、消息正文不碰、无 tools no-op、sanitize 开关两态一致。
func TestNormalizeToolPatterns(t *testing.T) {
	t.Parallel()
	t.Run("exa agent_run pattern normalized", func(t *testing.T) {
		t.Parallel()
		testAgentRunPatternNormalized(t)
	})
	t.Run("nested anyOf items pattern normalized", func(t *testing.T) {
		t.Parallel()
		testNestedAnyOfPatternNormalized(t)
	})
	t.Run("bare tool parameters form normalized", func(t *testing.T) {
		t.Parallel()
		testBareToolParametersNormalized(t)
	})
	t.Run("patternProperties key normalized", func(t *testing.T) {
		t.Parallel()
		testPatternPropertiesKeyNormalized(t)
	})
	t.Run("message content backslash untouched", func(t *testing.T) {
		t.Parallel()
		testMessageContentBackslashUntouched(t)
	})
	t.Run("clean pattern untouched", func(t *testing.T) {
		t.Parallel()
		testCleanPatternUntouched(t)
	})
	t.Run("no tools is no-op", func(t *testing.T) {
		t.Parallel()
		testNoToolsNoop(t)
	})
	t.Run("sanitize on and off behave the same", func(t *testing.T) {
		t.Parallel()
		testSanitizeOnOffSame(t)
	})
}

// str 反斜杠：测试体经工具链多层转义易被吞，运行时拼装保真。
const str = `\`

func testAgentRunPatternNormalized(t *testing.T) {
	body := `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"agent_run","parameters":{"type":"object","properties":{"runId":{"type":"string","pattern":"^agent` + str + str + `_run` + str + str + `_"}` + `,"query":{"type":"string"}},"required":["query"]}}}]}` //nolint:lll // 实案 body 原样

	out := string(PrepareBodyOptWithEfforts([]byte(body), false, nil))
	if got := lookupToolPattern(t, out, "tools", 0, "function", "parameters", "properties", "runId", "pattern"); got != `^agent_run_` {
		t.Fatalf("pattern = %q, want %q", got, `^agent_run_`)
	}
	// 同 schema 的兄弟字段不受影响
	if got := lookupToolPattern(t, out, "tools", 0, "function", "parameters", "properties", "query", "type"); got != "string" {
		t.Fatalf("sibling field disturbed: %q", got)
	}
}

func testNestedAnyOfPatternNormalized(t *testing.T) {
	body := `{"messages":[],"tools":[{"type":"function","function":{"name":"t","parameters":{"anyOf":[{"properties":{"code":{"pattern":"x` + str + str + `_y"}}},{"type":"string"}]}}}]}` //nolint:lll

	out := string(PrepareBodyOptWithEfforts([]byte(body), false, nil))
	if got := lookupToolPattern(t, out, "tools", 0, "function", "parameters", "anyOf", 0, "properties", "code", "pattern"); got != "x_y" {
		t.Fatalf("nested pattern = %q, want %q", got, "x_y")
	}
}

func testBareToolParametersNormalized(t *testing.T) {
	body := `{"messages":[],"tools":[{"name":"t","parameters":{"properties":{"id":{"pattern":"p` + str + str + `_q"}}}}]}`

	out := string(PrepareBodyOptWithEfforts([]byte(body), false, nil))
	if got := lookupToolPattern(t, out, "tools", 0, "parameters", "properties", "id", "pattern"); got != "p_q" {
		t.Fatalf("bare parameters pattern = %q, want %q", got, "p_q")
	}
}

func testPatternPropertiesKeyNormalized(t *testing.T) {
	body := `{"messages":[],"tools":[{"type":"function","function":{"name":"t","parameters":{"patternProperties":{"^a` + str + str + `_b` + str + str + `_":{"type":"string"}}}}}]}` //nolint:lll

	var obj map[string]any

	out := PrepareBodyOptWithEfforts([]byte(body), false, nil)
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("out not json: %v", err)
	}

	tool := obj["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)

	props := tool["patternProperties"].(map[string]any)
	if _, ok := props["^a_b_"]; !ok {
		t.Fatalf("normalized key missing: %v", props)
	}

	for k := range props {
		if strings.Contains(k, str) {
			t.Fatalf("patternProperties key not normalized: %q", k)
		}
	}
}

func testMessageContentBackslashUntouched(t *testing.T) {
	raw := `{"messages":[{"role":"user","content":"path C:` + str + str + `_dir and regex a` + str + str + `_b"}],"tools":[{"type":"function","function":{"name":"t","parameters":{"properties":{"p":{"pattern":"q` + str + str + `_r"}}}}}]}` //nolint:lll
	out := string(PrepareBodyOptWithEfforts([]byte(raw), false, nil))

	var obj map[string]any
	if err := json.Unmarshal([]byte(out), &obj); err != nil {
		t.Fatalf("out not json: %v", err)
	}

	msg := obj["messages"].([]any)[0].(map[string]any)
	if got := msg["content"]; got != `path C:\_dir and regex a\_b` {
		t.Fatalf("message content mutated: %q", got)
	}
}

func testCleanPatternUntouched(t *testing.T) {
	body := `{"messages":[],"tools":[{"type":"function","function":{"name":"t","parameters":{"properties":{"p":{"pattern":"^[a-z]+` + str + str + `d_$"}}}}}]}` //nolint:lll

	out := string(PrepareBodyOptWithEfforts([]byte(body), false, nil))
	if got := lookupToolPattern(t, out, "tools", 0, "function", "parameters", "properties", "p", "pattern"); got != `^[a-z]+\d_$` {
		t.Fatalf("clean pattern mutated: %q", got)
	}
}

func testNoToolsNoop(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"a` + str + str + `_b"}]}`

	var inObj, outObj map[string]any
	if err := json.Unmarshal([]byte(body), &inObj); err != nil {
		t.Fatal(err)
	}

	out := PrepareBodyOptWithEfforts([]byte(body), false, nil)
	if err := json.Unmarshal(out, &outObj); err != nil {
		t.Fatalf("out not json: %v", err)
	}

	inMsg, _ := json.Marshal(inObj["messages"])

	outMsg, _ := json.Marshal(outObj["messages"])
	if string(inMsg) != string(outMsg) {
		t.Fatalf("messages changed without tools: %s -> %s", inMsg, outMsg)
	}

	if _, has := outObj["tools"]; has {
		t.Fatal("tools appeared out of nowhere")
	}
}

func testSanitizeOnOffSame(t *testing.T) {
	body := `{"messages":[],"tools":[{"type":"function","function":{"name":"t","parameters":{"properties":{"p":{"pattern":"a` + str + str + `_b"}}}}}]}` //nolint:lll
	for _, sanitize := range []bool{false, true} {
		out := string(PrepareBodyOptWithEfforts([]byte(body), sanitize, nil))
		if got := lookupToolPattern(t, out, "tools", 0, "function", "parameters", "properties", "p", "pattern"); got != "a_b" {
			t.Fatalf("sanitize=%v: pattern = %q, want %q", sanitize, got, "a_b")
		}
	}
}

// TestClampGPTMinMaxTokens GPT 系上游要求 max_tokens ≥ 16（实测 gpt-6-sol/gpt-6-luna/
// gpt-5.6-sol：15 → 400 code=11133 model_param_invalid，16 → 200）。Claude Code 切
// 模型时的探针请求 max_tokens 极小，全号轮转同样被拒 → 客户端 503。非 GPT 模型不动。
func TestClampGPTMinMaxTokens(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
		want any // nil 表示字段不存在
	}{
		{"gpt below floor", `{"model":"gpt-6-sol","max_tokens":1,"messages":[]}`, float64(16)},
		{"gpt global prefix", `{"model":"global:gpt-5.6-sol","max_tokens":15,"messages":[]}`, float64(16)},
		{"gpt at floor", `{"model":"gpt-6-luna","max_tokens":16,"messages":[]}`, float64(16)},
		{"gpt above floor", `{"model":"gpt-6-sol","max_tokens":32000,"messages":[]}`, float64(32000)},
		{"gpt alias translated then clamped", `{"model":"gpt-6-sol","max_completion_tokens":1,"messages":[]}`, float64(16)},
		{"gpt absent untouched", `{"model":"gpt-6-sol","messages":[]}`, nil},
		{"non-gpt untouched", `{"model":"hy4-preview-f","max_tokens":1,"messages":[]}`, float64(1)},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			t.Parallel()

			obj, err := decodeBody(PrepareBodyOptWithEffertsPreserve(t, item.body))
			if err != nil {
				t.Fatalf("unmarshal: %v", err)
			}

			got, has := obj["max_tokens"]
			if item.want == nil {
				if has {
					t.Fatalf("max_tokens = %v, want absent", got)
				}

				return
			}

			if got != item.want {
				t.Fatalf("max_tokens = %v, want %v", got, item.want)
			}
		})
	}
}

// TestDropReasoningContentParts 思考片段剥离规则（直调，隔离其它归一化步骤）。
// 背景：第三方把 Responses 的 reasoning_text / reasoning 当 content part 回放进
// chat/completions 时，上游 content 类型白名单不含它们 → 400 code=11101。
func TestDropReasoningContentParts(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
		want string
	}{
		{
			"reasoning_text 剥离、text 保留",
			`{"messages":[{"role":"assistant","content":[{"type":"reasoning_text","text":"think"},{"type":"text","text":"ans"}]}]}`,
			`[{"role":"assistant","content":[{"type":"text","text":"ans"}]}]`,
		},
		{
			"reasoning 别名同样剥离",
			`{"messages":[{"role":"assistant","content":[{"type":"reasoning","text":"think"},{"type":"output_text","text":"ans"}]}]}`,
			`[{"role":"assistant","content":[{"type":"output_text","text":"ans"}]}]`,
		},
		{
			"type 前后空白同样命中",
			`{"messages":[{"role":"assistant","content":[{"type":" reasoning_text ","text":"think"},{"type":"text","text":"ans"}]}]}`,
			`[{"role":"assistant","content":[{"type":"text","text":"ans"}]}]`,
		},
		{
			"剥离后为空且无 tool_calls：整条消息删除",
			`{"messages":[{"role":"assistant","content":[{"type":"reasoning_text","text":"think"}]},{"role":"user","content":"hi"}]}`,
			`[{"role":"user","content":"hi"}]`,
		},
		{
			"剥离后为空但有 tool_calls：保留空数组 content",
			`{"messages":[{"role":"assistant","content":[{"type":"reasoning_text","text":"think"}],"tool_calls":[{"id":"call_1","type":"function","function":{"name":"exec_command","arguments":"{}"}}]}]}`,
			`[{"role":"assistant","content":[],"tool_calls":[{"id":"call_1","type":"function","function":{"name":"exec_command","arguments":"{}"}}]}]`,
		},
		{
			"字符串 content 原样通过",
			`{"messages":[{"role":"assistant","content":"plain text"}]}`,
			`[{"role":"assistant","content":"plain text"}]`,
		},
		{
			"正常 content part 不受影响",
			`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AA"}}]}]}`,
			`[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AA"}}]}]`,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			obj, err := decodeBody([]byte(testCase.body))
			if err != nil {
				t.Fatalf("unmarshal: %v", err)
			}

			dropReasoningContentParts(obj)

			// 用解码后的结构比较：map 序列化键序不稳定，字符串比较会假失败。
			var got, want any
			if err := json.Unmarshal([]byte(mustMarshal(t, obj["messages"])), &got); err != nil {
				t.Fatalf("unmarshal got: %v", err)
			}

			if err := json.Unmarshal([]byte(testCase.want), &want); err != nil {
				t.Fatalf("unmarshal want: %v", err)
			}

			if !reflect.DeepEqual(got, want) {
				t.Errorf("messages=%v want %s", got, testCase.want)
			}
		})
	}
}

// TestPrepareBodyDropsReasoningParts 全链路（sanitize 开/关两态）：reasoning 片段
// 不出现在出站 body。
func TestPrepareBodyDropsReasoningParts(t *testing.T) {
	t.Parallel()

	body := `{"model":"glm-5.2","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":[{"type":"reasoning_text","text":"think"},{"type":"text","text":"ans"}]},
		{"role":"user","content":"again"}
	]}`

	for _, sanitize := range []bool{false, true} {
		out := PrepareBodyOptWithEfforts([]byte(body), sanitize, nil)
		if strings.Contains(string(out), "reasoning_text") {
			t.Fatalf("sanitize=%v: reasoning_text 未剥离: %s", sanitize, out)
		}

		obj, err := decodeBody(out)
		if err != nil {
			t.Fatalf("sanitize=%v unmarshal: %v", sanitize, err)
		}

		msgs, ok := obj["messages"].([]any)
		if !ok || len(msgs) != 3 {
			t.Fatalf("sanitize=%v: messages=%v want 3 条", sanitize, obj["messages"])
		}

		content, ok := msgs[1].(map[string]any)["content"].([]any)
		if !ok || len(content) != 1 {
			t.Fatalf("sanitize=%v: 助手 content 片段数=%v want 1", sanitize, msgs[1])
		}

		if got := content[0].(map[string]any)["text"]; got != "ans" {
			t.Fatalf("sanitize=%v: 保留片段 text=%v want ans", sanitize, got)
		}
	}
}

// mustMarshal 测试辅助：序列化失败直接 fail。
func mustMarshal(t *testing.T, v any) string {
	t.Helper()

	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	return string(raw)
}

// TestEnsureLeadingSystem 首条非 system 时补官方 fallback；已是 system 时逐字不动。
func TestEnsureLeadingSystem(t *testing.T) {
	t.Parallel()

	out := ensureLeadingSystem([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))

	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}

	msgs := obj["messages"].([]any)

	first := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != LeadingSystemFallback {
		t.Fatalf("first=%v", first)
	}

	in := []byte(`{"messages":[{"role":"system","content":"S"}]}`)
	if got := ensureLeadingSystem(in); string(got) != string(in) {
		t.Fatalf("system-first body changed: %s", got)
	}
}

// TestInjectConversationRequestID 只给当前轮最后一条 user 消息补字段；客户端自带值优先。
func TestInjectConversationRequestID(t *testing.T) {
	t.Parallel()

	in := `{"model":"m","messages":[` +
		`{"role":"user","content":"old"},` +
		`{"role":"assistant","content":"a"},` +
		`{"role":"user","content":"new","conversationRequestId":"client-id"}]}`

	out := injectConversationRequestID([]byte(in), "server-id")

	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}

	msgs := obj["messages"].([]any)

	if id := msgs[0].(map[string]any)["conversationRequestId"]; id != nil {
		t.Fatalf("historical turn must not be touched: %v", id)
	}

	last := msgs[2].(map[string]any)
	if last["conversationRequestId"] != "client-id" {
		t.Fatalf("client value must win: %v", last["conversationRequestId"])
	}

	if last["startsNewUserRequest"] != true {
		t.Fatalf("startsNewUserRequest=%v", last["startsNewUserRequest"])
	}
}

// TestChatStreamInjectsConversationRequestBody 开关开启后出站 body 带轮级字段；
// prepareBody 管线仍生效（stream:true 等）。
func TestChatStreamInjectsConversationRequestBody(t *testing.T) {
	t.Parallel()

	var got []byte

	client := testClient(func(r *http.Request) (*http.Response, error) {
		got, _ = io.ReadAll(r.Body)
		return jsonResp(200, "data: [DONE]\n\n"), nil
	})
	client.ConversationRequestBody.Store(true)

	account := &auth.Auth{UID: "u1", AccessToken: "at"}

	_, _, _, err := client.ChatStreamContext(context.Background(), account,
		[]byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`), "",
		ChatMeta{ConversationID: "conv", ConversationRequestID: "req-1"})
	if err != nil {
		t.Fatal(err)
	}

	var obj map[string]any
	if err := json.Unmarshal(got, &obj); err != nil {
		t.Fatal(err)
	}

	var msg map[string]any

	for _, raw := range obj["messages"].([]any) {
		candidate := raw.(map[string]any)
		if candidate["role"] == "user" {
			msg = candidate
		}
	}

	if msg == nil || msg["conversationRequestId"] != "req-1" || msg["startsNewUserRequest"] != true {
		t.Fatalf("body=%s", got)
	}
}

// TestNormalizeToolEnumTypes 工具 schema 只写 enum 不写 type 时补出可无损推断的 type
// （11133 实案：kimi-k3.1-pro / kimi-k3-1 对参数 schema 严格校验，属性只给 enum 直接
// 整单 400 code=11133；补上 type 即 200，hy3 / glm-5.3 不受影响）。
// 边界：推断不出唯一类型（混合标量 / 含 null / 空数组）保持原样，不猜。
func TestNormalizeToolEnumTypes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		enum string // properties.<name> 的 schema 原样 JSON
		want string // 期望补出的 type（空 = 不补）
	}{
		{name: "string", enum: `{"enum":["low","high"]}`, want: "string"},
		{name: "integer", enum: `{"enum":[1,2,3]}`, want: "integer"},
		{name: "number", enum: `{"enum":[0.5,1]}`, want: "number"},
		{name: "boolean", enum: `{"enum":[true,false]}`, want: "boolean"},
		{name: "explicit type kept", enum: `{"type":"string","enum":["a"]}`, want: "string"},
		{name: "mixed scalars untouched", enum: `{"enum":["1",2]}`, want: ""},
		{name: "null in enum untouched", enum: `{"enum":["a",null]}`, want: ""},
		{name: "empty enum untouched", enum: `{"enum":[]}`, want: ""},
		{name: "no enum untouched", enum: `{"description":"x"}`, want: ""},
		{name: "object enum member untouched", enum: `{"enum":[{"a":1}]}`, want: ""},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			body := `{"model":"kimi-k3.1-pro","messages":[{"role":"user","content":"hi"}],"tools":[` +
				`{"type":"function","function":{"name":"pick","parameters":{"type":"object","properties":{` +
				`"level":` + testCase.enum + `},"required":["level"]}}}]}`

			out := string(PrepareBodyOptWithEfforts([]byte(body), false, nil))

			got := lookupToolPattern(t, out, "tools", 0, "function", "parameters", "properties", "level", "type")
			if got != testCase.want {
				t.Fatalf("inferred type = %q, want %q (schema %s)", got, testCase.want, testCase.enum)
			}
		})
	}
}

// enum 取值一字不改，只是补类型；嵌套 schema（anyOf / items）同样生效。
func TestNormalizeToolEnumTypesKeepValuesAndNested(t *testing.T) {
	t.Parallel()

	body := `{"messages":[],"tools":[{"type":"function","function":{"name":"t","parameters":` +
		`{"type":"object","properties":{"level":{"enum":["low","high"]}},` +
		`"anyOf":[{"properties":{"tier":{"enum":[1,2]}}}],` +
		`"items":{"enum":[true]}}}}]}`

	out := string(PrepareBodyOptWithEfforts([]byte(body), false, nil))

	if got := lookupToolPattern(t, out, "tools", 0, "function", "parameters", "properties", "level", "type"); got != "string" {
		t.Fatalf("top-level enum type = %q, want string", got)
	}

	if got := lookupToolPattern(t, out, "tools", 0, "function", "parameters", "anyOf", 0, "properties", "tier", "type"); got != "integer" {
		t.Fatalf("nested anyOf enum type = %q, want integer", got)
	}

	if got := lookupToolPattern(t, out, "tools", 0, "function", "parameters", "items", "type"); got != "boolean" {
		t.Fatalf("items enum type = %q, want boolean", got)
	}

	var obj map[string]any
	if err := json.Unmarshal([]byte(out), &obj); err != nil {
		t.Fatalf("out not json: %v", err)
	}

	level := obj["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)["properties"].(map[string]any)["level"].(map[string]any)

	enum, _ := json.Marshal(level["enum"])
	if string(enum) != `["low","high"]` {
		t.Fatalf("enum values mutated: %s", enum)
	}
}

// response_format 与 const 是**输出**约束：补错类型会改语义，不在规整范围内。
func TestNormalizeToolEnumTypesOutputConstraintsUntouched(t *testing.T) {
	t.Parallel()

	body := `{"messages":[],"response_format":{"type":"json_schema","json_schema":{"schema":` +
		`{"type":"object","properties":{"level":{"enum":["low","high"]}}}}},` +
		`"tools":[{"type":"function","function":{"name":"t","parameters":{"type":"object","properties":{` +
		`"mode":{"const":"fast"}}}}}]}`

	out := string(PrepareBodyOptWithEfforts([]byte(body), false, nil))

	if got := lookupToolPattern(t, out, "response_format", "json_schema", "schema", "properties", "level", "type"); got != "" {
		t.Fatalf("response_format enum 被补了 type: %q", got)
	}

	if got := lookupToolPattern(t, out, "tools", 0, "function", "parameters", "properties", "mode", "type"); got != "" {
		t.Fatalf("const 节点被补了 type: %q", got)
	}

	if got := lookupToolPattern(t, out, "tools", 0, "function", "parameters", "properties", "mode", "const"); got != "fast" {
		t.Fatalf("const 取值被改: %q", got)
	}
}
