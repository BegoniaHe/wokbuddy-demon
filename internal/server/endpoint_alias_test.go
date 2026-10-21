package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/apikey"
	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// TestMessagesAlias 兼容把 base URL 配成 .../v1 的 Anthropic 客户端。
func TestMessagesAlias(t *testing.T) {
	t.Parallel()

	client := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(&Config{Pool: p, Upstream: client, PromptMode: "passthrough"})

	body := `{"model":"glm-5.2","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/messages", strings.NewReader(body)))

	if rec.Code == http.StatusNotFound {
		t.Fatalf("POST /messages should be routed, got 404")
	}
}

// TestResponsesWebsocketProbe 返回 426，让 Codex 回落 HTTPS POST。
func TestResponsesWebsocketProbe(t *testing.T) {
	t.Parallel()

	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(&Config{Pool: p})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/responses", nil))

	if rec.Code != http.StatusUpgradeRequired {
		t.Fatalf("code=%d want 426", rec.Code)
	}

	if got := rec.Header().Get("Upgrade"); got != "websocket" {
		t.Fatalf("Upgrade=%q want websocket", got)
	}
}

// TestResponsesWebsocketProbeAcceptsNamedKey 命名密钥客户端也要拿到 426
// （回落 HTTPS POST），不能因鉴权口径不同先被判 401。
func TestResponsesWebsocketProbeAcceptsNamedKey(t *testing.T) {
	t.Parallel()

	store := apikey.New("")

	_, raw, err := store.Create("codex")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	handler := NewHandler(&Config{Pool: p, APIKey: "secret", Keys: store})

	req := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	req.Header.Set("Authorization", "Bearer "+raw)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUpgradeRequired {
		t.Fatalf("code=%d body=%s want 426", rec.Code, rec.Body.String())
	}
}
