package server

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/apikey"
	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// TestNamedKeyRPMAndDefaultKeyUnaffected 命名密钥超 RPM 返回 429 + Retry-After；
// 默认密钥不受影响。
func TestNamedKeyRPMAndDefaultKeyUnaffected(t *testing.T) {
	store := apikey.New("")
	rec, raw, err := store.Create("limited")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := store.UpdateLimits(rec.ID, apikey.Limits{RPM: 1}); err != nil || !ok {
		t.Fatalf("limits: %v %v", ok, err)
	}
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true }),
		APIKey:   "secret",
		Keys:     store,
	})
	do := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
		req.Header.Set("Authorization", "Bearer "+token)
		recorder := httptest.NewRecorder()
		h.ServeHTTP(recorder, req)
		return recorder
	}
	if code := do(raw).Code; code != http.StatusOK {
		t.Fatalf("第一个请求 code=%d", code)
	}
	limited := do(raw)
	if limited.Code != http.StatusTooManyRequests {
		t.Fatalf("超限 code=%d body=%s", limited.Code, limited.Body)
	}
	ra := limited.Header().Get("Retry-After")
	if n, err := strconv.Atoi(ra); err != nil || n < 1 {
		t.Fatalf("Retry-After=%q", ra)
	}
	if code := do("secret").Code; code != http.StatusOK {
		t.Fatalf("默认密钥被误伤 code=%d", code)
	}
}

// TestNamedKeyModelWhitelist 模型白名单拒绝未命中模型，命中时正常转发。
func TestNamedKeyModelWhitelist(t *testing.T) {
	store := apikey.New("")
	rec, raw, _ := store.Create("limited")
	if ok, err := store.UpdateLimits(rec.ID, apikey.Limits{Models: []string{"cn:glm-*"}}); err != nil || !ok {
		t.Fatalf("limits: %v %v", ok, err)
	}
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true }),
		APIKey:   "secret",
		Keys:     store,
	})
	do := func(model string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"`+model+`","messages":[]}`))
		req.Header.Set("Authorization", "Bearer "+raw)
		recorder := httptest.NewRecorder()
		h.ServeHTTP(recorder, req)
		return recorder
	}
	if code := do("cn:glm-5.2").Code; code != http.StatusOK {
		t.Fatalf("白名单模型 code=%d", code)
	}
	if code := do("cn:other").Code; code != http.StatusForbidden {
		t.Fatalf("白名单外模型 code=%d", code)
	}
}

// TestNamedKeyModelWhitelistAnthropic 白名单同样约束 Anthropic 端点：
// /v1/messages 与 /v1/messages/count_tokens 均不能绕过。
func TestNamedKeyModelWhitelistAnthropic(t *testing.T) {
	store := apikey.New("")
	rec, raw, _ := store.Create("limited")
	if ok, err := store.UpdateLimits(rec.ID, apikey.Limits{Models: []string{"glm-*"}}); err != nil || !ok {
		t.Fatalf("limits: %v %v", ok, err)
	}
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true }),
		APIKey:   "secret",
		Keys:     store,
	})
	do := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+raw)
		recorder := httptest.NewRecorder()
		h.ServeHTTP(recorder, req)
		return recorder
	}
	msg := func(model string) string {
		return `{"model":"` + model + `","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	}
	if code := do("/v1/messages", msg("glm-5.2")).Code; code != http.StatusOK {
		t.Fatalf("白名单模型 /v1/messages code=%d", code)
	}
	denied := do("/v1/messages", msg("claude-x"))
	if denied.Code != http.StatusForbidden {
		t.Fatalf("白名单外模型 /v1/messages code=%d", denied.Code)
	}
	if !strings.Contains(denied.Body.String(), `"type":"error"`) {
		t.Fatalf("/v1/messages 拒绝应为 Anthropic 错误体: %s", denied.Body)
	}
	if code := do("/v1/messages/count_tokens", `{"model":"glm-5.2"}`).Code; code != http.StatusOK {
		t.Fatalf("白名单模型 count_tokens code=%d", code)
	}
	denied = do("/v1/messages/count_tokens", `{"model":"claude-x"}`)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("白名单外模型 count_tokens code=%d", denied.Code)
	}
	if !strings.Contains(denied.Body.String(), `"type":"error"`) {
		t.Fatalf("count_tokens 拒绝应为 Anthropic 错误体: %s", denied.Body)
	}
}
