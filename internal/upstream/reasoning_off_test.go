package upstream

import (
	"encoding/json"
	"testing"
)

// TestDeepSeekOffEffortDisablesThinking off/none/disabled（大小写、空白、camelCase）
// 在 DeepSeek 上必须出站 thinking.type=disabled 且不保留任何 effort 字段。
func TestDeepSeekOffEffortDisablesThinking(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
	}{
		{"off", `{"model":"deepseek-v4-flash","reasoning_effort":"off","messages":[]}`},
		{"none", `{"model":"deepseek-v4-flash","reasoning_effort":"none","messages":[]}`},
		{"disabled", `{"model":"deepseek-v4-flash","reasoning_effort":"disabled","messages":[]}`},
		{"大写与空白", `{"model":"deepseek-v4-flash","reasoning_effort":"  OFF  ","messages":[]}`},
		{"camelCase", `{"model":"deepseek-v4-flash","reasoningEffort":"None","messages":[]}`},
		{"thinking 对象 type 空", `{"model":"deepseek-v4-flash","thinking":{},"reasoning_effort":"off","messages":[]}`},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			t.Parallel()

			out := PrepareBodyOptWithEfforts([]byte(item.body), false, nil)

			typ, present := getThinkingType(t, out)
			if !present || typ != "disabled" {
				t.Fatalf("thinking.type=%q present=%v want disabled (out=%s)", typ, present, out)
			}

			assertEffortFieldsAbsent(t, out)
		})
	}
}

// TestNonDeepSeekOffEffortOnlyDeletesEffort 非 DeepSeek 不注入 thinking，只清理档位；
// 即使模型支持列表把 off 降成 low 也不能发生。
func TestNonDeepSeekOffEffortOnlyDeletesEffort(t *testing.T) {
	t.Parallel()

	efforts := map[string][]string{"glm-5.2": {"off", "low", "high"}}

	cases := []struct {
		name string
		body string
	}{
		{"snake", `{"model":"glm-5.2","reasoning_effort":"off","messages":[]}`},
		{"camel 大小写空白", `{"model":"glm-5.2","reasoningEffort":" DISABLED ","messages":[]}`},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			t.Parallel()

			out := PrepareBodyOptWithEfforts([]byte(item.body), false, efforts)

			if _, present := getThinkingType(t, out); present {
				t.Errorf("非 DeepSeek 不得注入 thinking (out=%s)", out)
			}

			assertEffortFieldsAbsent(t, out)
		})
	}
}

// TestOffEffortNormalizeEarlyExit 模型未缓存 / efforts 为空时也必须在降级前截住 off。
func TestOffEffortNormalizeEarlyExit(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		body    string
		efforts map[string][]string
	}{
		{"efforts 为空", `{"model":"glm-5.2","reasoning_effort":"off"}`, nil},
		{"模型未缓存", `{"model":"deepseek-v4-flash","reasoning_effort":"off"}`, map[string][]string{"glm-5.2": {"low"}}},
		{"模型为空", `{"model":"","reasoning_effort":"none"}`, map[string][]string{"": {"low"}}},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			t.Parallel()

			out := PrepareBodyOptWithEfforts([]byte(item.body), false, item.efforts)

			assertEffortFieldsAbsent(t, out)
		})
	}
}

// TestExplicitThinkingTypeWinsOverOff 显式 thinking.type 优先：不改写 type；
// effort 仍按关闭意图删除，不补默认档。
func TestExplicitThinkingTypeWinsOverOff(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		body    string
		wantTyp string
	}{
		{
			"显式 enabled 保留 type",
			`{"model":"deepseek-v4-flash","thinking":{"type":"enabled"},"reasoning_effort":"off"}`,
			"enabled",
		},
		{
			"显式 disabled 保留 type",
			`{"model":"deepseek-v4-flash","thinking":{"type":"disabled"},"reasoning_effort":"none"}`,
			"disabled",
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			t.Parallel()

			out := PrepareBodyOptWithEfforts([]byte(item.body), false, nil)

			typ, present := getThinkingType(t, out)
			if !present || typ != item.wantTyp {
				t.Fatalf("thinking.type=%q present=%v want %q (out=%s)", typ, present, item.wantTyp, out)
			}

			assertEffortFieldsAbsent(t, out)
		})
	}
}

func assertEffortFieldsAbsent(t *testing.T, out []byte) {
	t.Helper()

	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v (out=%s)", err, out)
	}

	for _, key := range []string{"reasoning_effort", "reasoningEffort"} {
		if _, ok := obj[key]; ok {
			t.Errorf("%s 应被删除 (out=%s)", key, out)
		}
	}
}
