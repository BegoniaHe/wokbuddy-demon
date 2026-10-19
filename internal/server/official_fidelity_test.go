package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

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
