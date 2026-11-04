package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// uuidV4ConvHeader 官方 X-Conversation-ID 形态（标准带连字符 UUIDv4）。
var uuidV4ConvHeader = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// TestChatEmitsConversationHeaderWithoutSessionKey metadata.user_id 会让
// session.ExtractSession 主动不派生会话键；出站仍必须带 X-Conversation-Id。
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

// TestChatPassthroughConversationRequestID 入站自带 X-Conversation-Request-Id 时
// 原样透传；该路径不再解析轮级键（body 解析被跳过），透传值必须保持不变。
func TestChatPassthroughConversationRequestID(t *testing.T) {
	t.Parallel()

	var got string

	client := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			got = r.Header.Get("X-Conversation-Request-Id")

			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	handler := NewHandler(&Config{Pool: p, Upstream: client, PromptMode: "passthrough"})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("X-Conversation-Request-Id", "inbound-turn-1")

	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}

	if got != "inbound-turn-1" {
		t.Fatalf("outbound X-Conversation-Request-Id=%q want inbound-turn-1", got)
	}
}

// TestUserIDClientKeepsStableCacheSlot 带 metadata.user_id 的客户端不参与粘性，
// 但仍必须按对话复用上游缓存槽：同 conversation 跨轮 X-Conversation-Id 与
// prompt_cache_key 稳定，不同 conversation 互异，显式 conversationId 原值透传。
//
//nolint:cyclop // 端到端线性断言，拆开反而掩盖失败点。
func TestUserIDClientKeepsStableCacheSlot(t *testing.T) {
	t.Parallel()

	type outbound struct {
		convID   string
		cacheKey string
	}

	var got []outbound

	client := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(r.Body)

			var obj map[string]any

			_ = json.Unmarshal(body, &obj)

			cacheKey, _ := obj["prompt_cache_key"].(string)
			got = append(got, outbound{convID: r.Header.Get("X-Conversation-Id"), cacheKey: cacheKey})

			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	handler := NewHandler(&Config{Pool: p, Upstream: client, PromptMode: "passthrough"})

	send := func(body map[string]any) {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}

		rec := postChat(t, handler, string(raw))
		if rec.Code != http.StatusOK {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
		}
	}

	conversation := func(first, last string) map[string]any {
		return map[string]any{
			"model":    "glm-5.2",
			"metadata": map[string]any{"user_id": "u1"},
			"messages": []any{
				map[string]any{"role": "system", "content": "sys"},
				map[string]any{"role": "user", "content": first},
				map[string]any{"role": "assistant", "content": "ok"},
				map[string]any{"role": "user", "content": last},
			},
		}
	}

	send(conversation("first", "second")) // 0
	send(conversation("first", "third"))  // 1 同会话下一轮
	send(conversation("other", "second")) // 2 另一段对话

	if len(got) != 3 {
		t.Fatalf("outbound requests=%d want 3", len(got))
	}

	if got[0].convID != got[1].convID {
		t.Fatalf("同会话跨轮 X-Conversation-Id 必须稳定：%q vs %q", got[0].convID, got[1].convID)
	}

	if got[0].cacheKey != got[1].cacheKey {
		t.Fatalf("同会话跨轮 prompt_cache_key 必须稳定：%q vs %q", got[0].cacheKey, got[1].cacheKey)
	}

	if got[0].convID == got[2].convID {
		t.Fatalf("不同对话必须换缓存槽：both=%q", got[0].convID)
	}

	if got[0].cacheKey == got[2].cacheKey {
		t.Fatalf("不同对话必须换 prompt_cache_key：%q", got[0].cacheKey)
	}

	if !uuidV4ConvHeader.MatchString(got[0].convID) {
		t.Fatalf("X-Conversation-Id=%q want 官方 UUIDv4（不得回落到内部 d- 键）", got[0].convID)
	}

	explicit := conversation("first", "second")
	explicit["conversationId"] = "client-conv"
	send(explicit) // 3

	if got[3].convID != "client-conv" {
		t.Fatalf("客户端声明 conversationId 必须原值透传：got=%q", got[3].convID)
	}

	if got[3].cacheKey == "" || got[3].cacheKey == got[0].cacheKey {
		t.Fatalf("客户端声明会话时 prompt_cache_key 必须由它派生：got=%q derived=%q",
			got[3].cacheKey, got[0].cacheKey)
	}
}
