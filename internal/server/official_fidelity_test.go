package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// TestRefreshSessionDeadHonorsThreshold refresh 401/403 进入会话失效分类后仍按连续
// 阈值禁用，不能一次失败就杀号。
func TestRefreshSessionDeadHonorsThreshold(t *testing.T) {
	t.Parallel()

	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt"})
	h := NewHandler(&Config{Pool: state, Upstream: &upstream.Client{}})
	flow := &chatFlow{h: h}

	acct := state.AuthByUID("u1")
	if acct == nil {
		t.Fatal("account missing")
	}

	refreshErr := &upstream.Error{Kind: upstream.ErrSessionDead, Status: http.StatusUnauthorized}

	for i := 1; i < pool.SessionDeadThreshold(); i++ {
		flow.penalizeRefreshFailure(acct, refreshErr)

		if statusOf(t, state, "u1").Disabled {
			t.Fatalf("disabled after %d failure(s), want threshold %d", i, pool.SessionDeadThreshold())
		}
	}

	flow.penalizeRefreshFailure(acct, refreshErr)

	if !statusOf(t, state, "u1").Disabled {
		t.Fatalf("not disabled after %d consecutive failures", pool.SessionDeadThreshold())
	}
}

// TestChat401RefreshesAndReplaysOnce 上游 401 后同号刷新并重放一次。
func TestChat401RefreshesAndReplaysOnce(t *testing.T) {
	t.Parallel()

	var chatCalls atomic.Int32

	client := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.URL.Path, "/token/refresh") {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"code":0,"data":{"accessToken":"at2","refreshToken":"rt2","expiresIn":3600}}`)),
				}, nil
			}

			chatCalls.Add(1)

			if r.Header.Get("Authorization") == "Bearer at1" {
				return &http.Response{
					StatusCode: http.StatusUnauthorized,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"code":401,"msg":"unauthorized"}`)),
				}, nil
			}

			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body: io.NopCloser(strings.NewReader(
					"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n" +
						"data: [DONE]\n\n")),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}

	pool := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", RefreshToken: "rt1", ExpiresAt: 9999999999})
	h := NewHandler(&Config{Pool: pool, Upstream: client})

	rec := postChat(t, h, `{"model":"glm-5.2","messages":[]}`)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if got := chatCalls.Load(); got != 2 {
		t.Errorf("chat calls=%d want 2 (401 + replay)", got)
	}

	if st := statusOf(t, pool, "u1"); st.Disabled {
		t.Errorf("account must stay enabled after successful replay: %+v", st)
	}
}

// TestChatQueueFullIsChannelLevel 队列满载（6021 / 只有 queue.full 文案）是渠道级
// 容量事件：只打一次上游、不轮转、账号零惩罚，返回 429 + Retry-After + queue_full
// 透传上游原文（issue #391：503 会让调用侧按「服务不可用」中断任务）。
func TestChatQueueFullIsChannelLevel(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"429+6021", http.StatusTooManyRequests, `{"code":6021,"msg":"queue.full.title"}`},
		{"500+6021", http.StatusInternalServerError, `{"code":6021,"msg":"queue.full.title"}`},
		{"仅文案外卡", http.StatusTooManyRequests, `{"msg":"queue.full.title"}`},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			assertQueueFullChannelLevel(t, testCase.status, testCase.body)
		})
	}
}

// assertRetryAfterHeader 断言 429 带 Retry-After：调用侧据它退避，而不是把 4xx 当致命
// 错误（issue #391：队列语义不能被翻译成「服务不可用」）。
func assertRetryAfterHeader(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()

	if rec.Header().Get("Retry-After") == "" {
		t.Errorf("429 must carry Retry-After, headers=%v", rec.Header())
	}
}

// assertQueueFullChannelLevel 单次断言：只打一次上游、429 + queue_full、账号零惩罚。
func assertQueueFullChannelLevel(t *testing.T, status int, body string) {
	t.Helper()

	var calls atomic.Int32

	client := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls.Add(1)

			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	// 两个健康号：若被误判成软限流就会轮转到第二个号（calls 变 2）。
	state := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(&Config{Pool: state, Upstream: client})

	rec := postChat(t, h, `{"model":"glm-5.2","messages":[]}`)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d want 429, body=%s", rec.Code, rec.Body)
	}

	assertRetryAfterHeader(t, rec)

	got := rec.Body.String()
	if !strings.Contains(got, "queue_full") || !strings.Contains(got, "queue.full.title") {
		t.Errorf("body must carry queue_full + upstream text, got %s", got)
	}

	if n := calls.Load(); n != 1 {
		t.Errorf("upstream calls=%d want 1 (channel-level busy must not rotate)", n)
	}

	for _, uid := range []string{"u1", "u2"} {
		st := statusOf(t, state, uid)
		if st.Cooling || st.Disabled || st.ErrTotal != 0 || st.BreakerFails != 0 {
			t.Errorf("account %s must stay unpunished: %+v", uid, st)
		}
	}
}

// TestInboundSessionMetadataForwarded 入站官方会话头进入 ChatMeta。
//
//nolint:cyclop // 覆盖多个透传字段。
func TestInboundSessionMetadataForwarded(t *testing.T) {
	t.Parallel()

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	for key, value := range map[string]string{
		"X-Conversation-ID":        "conv-1",
		"X-Root-Request-ID":        "root-1",
		"X-Parent-Conversation-ID": "parent-1",
		"X-Agent-Intent":           "craft",
		"X-Agent-Purpose":          "conversation",
		"X-Agent-Type":             "main",
		"X-Compaction":             "auto",
		"X-Context-Compacted":      "true",
	} {
		r.Header.Set(key, value)
	}

	input, _, ok := newChatInput(httptest.NewRecorder(), r, 0, 0)
	if !ok {
		t.Fatal("newChatInput failed")
	}

	h := NewHandler(&Config{Pool: testPoolWith(), Upstream: &upstream.Client{}})
	flow := h.newChatFlow(&input, &fakeResponder{})

	if flow.chatMeta.ConversationID != "conv-1" || flow.chatMeta.RootRequestID != "root-1" ||
		flow.chatMeta.ParentConversationID != "parent-1" || flow.chatMeta.AgentIntent != "craft" ||
		flow.chatMeta.AgentPurpose != "conversation" || flow.chatMeta.AgentType != "main" ||
		flow.chatMeta.Compaction != "auto" || flow.chatMeta.ContextCompacted != "true" {
		t.Errorf("chatMeta=%+v", flow.chatMeta)
	}
}
