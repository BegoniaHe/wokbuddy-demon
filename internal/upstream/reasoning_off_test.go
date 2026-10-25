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

// TestNonDeepSeekSupportedOffEffortPreserved 模型支持列表声明 off 时，关闭档位
// 必须原样透传（按支持列表写法），不能被降级成最弱正档或被误删。
func TestNonDeepSeekSupportedOffEffortPreserved(t *testing.T) {
	t.Parallel()

	efforts := map[string][]string{"glm-5.2": {"off", "low", "high"}}

	cases := []struct {
		name string
		body string
		key  string
	}{
		{"snake", `{"model":"glm-5.2","reasoning_effort":"off","messages":[]}`, "reasoning_effort"},
		{"camel 大小写空白", `{"model":"glm-5.2","reasoningEffort":" DISABLED ","messages":[]}`, "reasoningEffort"},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			t.Parallel()

			out := PrepareBodyOptWithEfforts([]byte(item.body), false, efforts)

			if _, present := getThinkingType(t, out); present {
				t.Errorf("非 DeepSeek 不得注入 thinking (out=%s)", out)
			}

			var obj map[string]any
			if err := json.Unmarshal(out, &obj); err != nil {
				t.Fatalf("unmarshal: %v (out=%s)", err, out)
			}

			if got, _ := obj[item.key].(string); got != "off" {
				t.Errorf("%s=%q want off (out=%s)", item.key, got, out)
			}
		})
	}
}

// TestNonDeepSeekUnsupportedOffEffortDeleted 模型未声明支持关闭档时，删除 effort
// 字段，避免被降级管线抬成最弱正档。
func TestNonDeepSeekUnsupportedOffEffortDeleted(t *testing.T) {
	t.Parallel()

	efforts := map[string][]string{"glm-5.2": {"low", "high"}}

	for _, body := range []string{
		`{"model":"glm-5.2","reasoning_effort":"off","messages":[]}`,
		`{"model":"glm-5.2","reasoningEffort":"none","messages":[]}`,
	} {
		out := PrepareBodyOptWithEfforts([]byte(body), false, efforts)

		if _, present := getThinkingType(t, out); present {
			t.Errorf("非 DeepSeek 不得注入 thinking (out=%s)", out)
		}

		assertEffortFieldsAbsent(t, out)
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

// TestOffEffortOverridesExplicitThinkingType 关闭意图优先于显式 thinking.type：
// 官方 deepseek 分支在非 enabled 时直接写 disabled，客户端同时带 enabled 也以
// 关闭为准；effort 字段删除，不补默认档。
func TestOffEffortOverridesExplicitThinkingType(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		body    string
		wantTyp string
	}{
		{
			"显式 enabled 被关闭意图覆盖",
			`{"model":"deepseek-v4-flash","thinking":{"type":"enabled"},"reasoning_effort":"off"}`,
			"disabled",
		},
		{
			"显式 disabled 保持 disabled",
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

// TestExplicitThinkingTypeKeptWithoutOffEffort 没有关闭档位时，显式 type 语义不变。
func TestExplicitThinkingTypeKeptWithoutOffEffort(t *testing.T) {
	t.Parallel()

	out := PrepareBodyOptWithEfforts(
		[]byte(`{"model":"deepseek-v4-flash","thinking":{"type":"enabled"},"reasoning_effort":"high"}`),
		false, nil,
	)

	typ, present := getThinkingType(t, out)
	if !present || typ != "enabled" {
		t.Fatalf("thinking.type=%q present=%v want enabled (out=%s)", typ, present, out)
	}

	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v (out=%s)", err, out)
	}

	if got, _ := obj["reasoning_effort"].(string); got != "high" {
		t.Fatalf("reasoning_effort=%q want high (out=%s)", got, out)
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
