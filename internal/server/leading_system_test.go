package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// TestChatPassthroughAddsLeadingSystem passthrough + 首条 user 必须补 system，
// 不能把官方不会发出的 wire 形态原样送给上游。
func TestChatPassthroughAddsLeadingSystem(t *testing.T) {
	t.Parallel()

	var outbound []byte

	client := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			outbound, _ = io.ReadAll(r.Body)

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

	rec := postChat(t, h, `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}

	var obj map[string]any
	if err := json.Unmarshal(outbound, &obj); err != nil {
		t.Fatalf("outbound invalid: %v (%s)", err, outbound)
	}

	msgs, _ := obj["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatalf("outbound messages empty: %s", outbound)
	}

	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != upstream.LeadingSystemFallback {
		t.Fatalf("first message=%v want system/%q", first, upstream.LeadingSystemFallback)
	}
}
