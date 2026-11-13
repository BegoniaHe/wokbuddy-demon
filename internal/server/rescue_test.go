// rescue_test.go 钉住响应形态判定与两类救援（issue #494）：reasoning-only 关思考
// 重发一次、长文 stop 续写一次，且救援不罚号。
package server

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

const sseReasoningOnly = "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"thinking hard\"}}]}\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: [DONE]\n\n"

const sseStopTail = "data: {\"id\":\"c2\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"续写部分\"}}]}\n\n" +
	"data: {\"id\":\"c2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: [DONE]\n\n"

func sseLongStop() string {
	chunk := strings.Repeat("长", 600)
	content := "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"" + chunk + "\"}}]}\n\n"

	return content + content + content + content +
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
}

// TestAggregateShape 判定入口的表驱动用例：长文续写只认「请求上限 ≥32k + 已成篇 +
// stop」，短答 stop 与低上限一律正常。
func TestAggregateShape(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("长", longFormContinuationMinChars)

	cases := []struct {
		name string
		body string
		resp map[string]any
		want responseShape
	}{
		{
			name: "reasoning-only",
			body: `{"max_tokens":40000}`,
			resp: map[string]any{"choices": []any{map[string]any{
				"finish_reason": "stop",
				"message":       map[string]any{"content": "", "reasoning_content": "thought"},
			}}},
			want: shapeReasoningOnly,
		},
		{
			name: "long-form-stop",
			body: `{"max_tokens":40000}`,
			resp: map[string]any{"choices": []any{map[string]any{
				"finish_reason": "stop",
				"message":       map[string]any{"content": long},
			}}},
			want: shapeLongFormTruncated,
		},
		{
			name: "small-max-tokens",
			body: `{"max_tokens":8000}`,
			resp: map[string]any{"choices": []any{map[string]any{
				"finish_reason": "stop",
				"message":       map[string]any{"content": long},
			}}},
			want: shapeNormal,
		},
		{
			name: "short-answer",
			body: `{"max_tokens":40000}`,
			resp: map[string]any{"choices": []any{map[string]any{
				"finish_reason": "stop",
				"message":       map[string]any{"content": "好的"},
			}}},
			want: shapeNormal,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			flow := &chatFlow{body: []byte(testCase.body)}
			if got := flow.aggregateShape(testCase.resp); got != testCase.want {
				t.Errorf("aggregateShape=%v want %v", got, testCase.want)
			}
		})
	}
}

// TestChatReasoningOnlyRescued 流式：只思考没正文时关思考重发一次，不换号不罚号。
func TestChatReasoningOnlyRescued(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	fake := newFakeUpstream(t, func(string) (int, string, bool) {
		if calls.Add(1) == 1 {
			return http.StatusOK, sseReasoningOnly, true
		}

		return http.StatusOK, sseOK, true
	})
	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(&Config{Pool: state, Upstream: fake})

	rec := postChat(t, h, `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}

	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls=%d want 2（关思考重发一次）", got)
	}

	if !strings.Contains(rec.Body.String(), "你好") {
		t.Errorf("救援后的正文未下发: %s", rec.Body.String())
	}

	st := statusOf(t, state, "u1")
	if st.ConsecutiveFails != 0 || st.BreakerFails != 0 {
		t.Errorf("救援不应罚号: fails=%d breaker=%d", st.ConsecutiveFails, st.BreakerFails)
	}
}

// TestChatReasoningOnlyGivesUp 仍只有思维链时回落到既有空流口径（502），且最多重发一次。
func TestChatReasoningOnlyGivesUp(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	fake := newFakeUpstream(t, func(string) (int, string, bool) {
		calls.Add(1)

		return http.StatusOK, sseReasoningOnly, true
	})
	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(&Config{Pool: state, Upstream: fake})

	rec := postChat(t, h, `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code=%d want 502, body=%s", rec.Code, rec.Body.String())
	}

	// 同一账号先重发一次（救援），之后才轮转；只有一个账号 → 共 2 次上游调用。
	if got := calls.Load(); got != 2 {
		t.Errorf("upstream calls=%d want 2（救援只做一次）", got)
	}
}

// TestChatLongFormStopContinued 流式长文：已输出成篇内容后 stop，续写一次并把
// 第二段拼进同一条流。
func TestChatLongFormStopContinued(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	fake := newFakeUpstream(t, func(string) (int, string, bool) {
		if calls.Add(1) == 1 {
			return http.StatusOK, sseLongStop(), true
		}

		return http.StatusOK, sseStopTail, true
	})
	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(&Config{Pool: state, Upstream: fake})

	rec := postChat(t, h, `{"model":"glm-5.2","stream":true,"max_tokens":40000,`+
		`"messages":[{"role":"user","content":"写长文"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}

	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls=%d want 2（长文续写一次）", got)
	}

	if !strings.Contains(rec.Body.String(), "续写部分") {
		t.Errorf("续写正文未拼进同一流: %s", rec.Body.String())
	}
}

// TestChatShortStopNotContinued 短答 stop 不续写：避免把每一句完整回答都续一遍。
func TestChatShortStopNotContinued(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	fake := newFakeUpstream(t, func(string) (int, string, bool) {
		calls.Add(1)

		return http.StatusOK, sseOK, true
	})
	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(&Config{Pool: state, Upstream: fake})

	rec := postChat(t, h, `{"model":"glm-5.2","stream":true,"max_tokens":40000,`+
		`"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}

	if got := calls.Load(); got != 1 {
		t.Errorf("upstream calls=%d want 1（短答不续写）", got)
	}
}

// TestChatNonStreamReasoningOnlyRescued 非流式同口径：只思考没正文时关思考重发一次。
func TestChatNonStreamReasoningOnlyRescued(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	// 上游恒走 SSE 形态（非流式由网关聚合），故两轮都给流式帧。
	fake := newFakeUpstream(t, func(string) (int, string, bool) {
		if calls.Add(1) == 1 {
			return http.StatusOK, sseReasoningOnly, true
		}

		return http.StatusOK, sseOK, true
	})
	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(&Config{Pool: state, Upstream: fake})

	rec := postChat(t, h, `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}

	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls=%d want 2（关思考重发一次）", got)
	}

	if !strings.Contains(rec.Body.String(), "你好") {
		t.Errorf("救援后的正文未下发: %s", rec.Body.String())
	}
}
