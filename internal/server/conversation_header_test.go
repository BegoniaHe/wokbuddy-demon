package server

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// TestChatEmitsConversationHeaderWithoutSessionKey metadata.user_id 会让
// session.ExtractKey 主动不派生会话键；出站仍必须带 X-Conversation-Id。
func TestChatEmitsConversationHeaderWithoutSessionKey(t *testing.T) {
	t.Parallel()

	var got string

	client := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			got = r.Header.Get("X-Conversation-Id")

			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(&Config{Pool: p, Upstream: client, PromptMode: "passthrough"})

	rec := postChat(t, h, `{"model":"glm-5.2","metadata":{"user_id":"u1"},"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}

	if got == "" {
		t.Fatal("outbound X-Conversation-Id is empty")
	}
}
