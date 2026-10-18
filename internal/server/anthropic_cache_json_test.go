// anthropic_cache_json_test.go cache_control 保留与工具参数收尾校验（issue #148）。
package server

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAnthropicCacheControlPreserved system 与 user text block 的 cache_control
// 必须出现在转换后的 OpenAI content parts 中，不能被静默压成纯字符串。
func TestAnthropicCacheControlPreserved(t *testing.T) {
	body := []byte(`{
		"model":"claude-sonnet-4-5",
		"system":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral"}}],
		"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}]
	}`)
	raw, _, _, err := anthropicToOpenAI(body)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var got struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("messages=%d want 2", len(got.Messages))
	}
	for i, role := range []string{"system", "user"} {
		parts, ok := got.Messages[i].Content.([]any)
		if !ok || len(parts) != 1 {
			t.Fatalf("%s content=%#v want one part array", role, got.Messages[i].Content)
		}
		part := parts[0].(map[string]any)
		cc, ok := part["cache_control"].(map[string]any)
		if !ok || cc["type"] != "ephemeral" {
			t.Errorf("%s cache_control=%#v want ephemeral", role, part["cache_control"])
		}
	}
}

// TestAnthropicToolArgumentsValidationWarn 流收尾对聚合后的工具参数做 json.Valid，
// 非法时记 WARN；日志不得包含参数原文。
func TestAnthropicToolArgumentsValidationWarn(t *testing.T) {
	cases := []struct {
		name string
		args string
		warn bool
	}{
		{"valid", `{"city":"SF"}`, false},
		{"truncated", `{"city":`, true},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := log.Writer()
			log.SetOutput(&buf)
			defer log.SetOutput(prev)

			a := newAnthropicStreamWriter(httptest.NewRecorder(), "glm-5.2")
			block := &anthropicToolBlock{index: 0, id: "toolu_1", name: "get_weather"}
			block.args.WriteString(tc.args)
			a.toolBlocks[0] = block
			if err := a.finish(); err != nil {
				t.Fatalf("finish: %v", err)
			}
			out := buf.String()
			if gotWarn := strings.Contains(out, "invalid tool arguments"); gotWarn != tc.warn {
				t.Fatalf("warn=%v want %v log=%q", gotWarn, tc.warn, out)
			}
			if tc.args != "" && strings.Contains(out, tc.args) {
				t.Fatalf("log leaked tool arguments: %q", out)
			}
		})
	}
}
