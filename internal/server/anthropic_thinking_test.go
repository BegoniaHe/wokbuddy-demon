package server

import (
	"encoding/json"
	"errors"
	"testing"
)

// TestValidateAnthropicThinkingRejectsMalformed 畸形 thinking 不得静默放行：此前
// json.Unmarshal 失败直接返回 nil，客户端以为 budget 约束已生效、实际被忽略。
func TestValidateAnthropicThinkingRejectsMalformed(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		`"not-an-object"`,
		`[]`,
		`{`,
	} {
		req := &anthropicRequest{Model: "claude-sonnet-4-5", Thinking: json.RawMessage(raw)}
		if err := validateAnthropicThinking(req); !errors.Is(err, errThinkingInvalid) {
			t.Errorf("thinking=%s: err=%v want errThinkingInvalid", raw, err)
		}
	}
}

// TestValidateAnthropicThinkingAllowsValid 合法形态保持既有校验语义。
func TestValidateAnthropicThinkingAllowsValid(t *testing.T) {
	t.Parallel()

	cases := []struct {
		raw  string
		want error
	}{
		{``, nil},
		{`null`, nil},
		{`"not-an-object"`, errThinkingInvalid},
		{`{"type":"disabled"}`, nil},
		{`{"type":"enabled","budget_tokens":2048}`, nil},
		{`{"type":"enabled","budget_tokens":10}`, errThinkingBudgetTooSmall},
	}

	for _, item := range cases {
		req := &anthropicRequest{Model: "claude-sonnet-4-5", Thinking: json.RawMessage(item.raw)}
		if err := validateAnthropicThinking(req); !errors.Is(err, item.want) {
			t.Errorf("thinking=%s: err=%v want %v", item.raw, err, item.want)
		}
	}
}
