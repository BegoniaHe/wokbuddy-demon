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
	t.Parallel()

	store := apikey.New("")

	rec, raw, err := store.Create("limited")
	if err != nil {
		t.Fatal(err)
	}

	if ok, err := store.UpdateLimits(rec.ID, apikey.Limits{RPM: 1, TPM: 0, MaxConcurrent: 0, Models: nil}); err != nil || !ok {
		t.Fatalf("limits: %v %v", ok, err)
	}

	handler := NewHandler(&Config{
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
		handler.ServeHTTP(recorder, req)

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

// TestNamedKeyRPMAnthropicErrorShape 命名密钥超限时 /v1/messages 必须返回
// Anthropic 错误信封，而不是 OpenAI 形态。
func TestNamedKeyRPMAnthropicErrorShape(t *testing.T) {
	t.Parallel()

	store := apikey.New("")

	rec, raw, err := store.Create("limited")
	if err != nil {
		t.Fatal(err)
	}

	if ok, err := store.UpdateLimits(rec.ID, apikey.Limits{RPM: 1, TPM: 0, MaxConcurrent: 0, Models: nil}); err != nil || !ok {
		t.Fatalf("limits: %v %v", ok, err)
	}

	handler := NewHandler(&Config{APIKey: "secret", Keys: store})

	do := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens",
			strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer "+raw)

		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)

		return recorder
	}

	if code := do().Code; code != http.StatusOK {
		t.Fatalf("第一个请求 code=%d", code)
	}

	limited := do()
	if limited.Code != http.StatusTooManyRequests {
		t.Fatalf("超限 code=%d body=%s", limited.Code, limited.Body)
	}

	if limited.Header().Get("Retry-After") == "" {
		t.Fatal("缺少 Retry-After")
	}

	if body := limited.Body.String(); !strings.Contains(body, `"type":"error"`) ||
		!strings.Contains(body, `"rate_limit_error"`) {
		t.Fatalf("超限应返回 Anthropic 错误体: %s", body)
	}
}

// TestNamedKeyModelWhitelist 模型白名单拒绝未命中模型，命中时正常转发。
func TestNamedKeyModelWhitelist(t *testing.T) {
	t.Parallel()

	store := apikey.New("")

	rec, raw, _ := store.Create("limited")
	if ok, err := store.UpdateLimits(rec.ID, apikey.Limits{Models: []string{"cn:glm-*"}, RPM: 0, TPM: 0, MaxConcurrent: 0}); err != nil || !ok {
		t.Fatalf("limits: %v %v", ok, err)
	}

	handler := NewHandler(&Config{
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
		handler.ServeHTTP(recorder, req)

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
	t.Parallel()

	store := apikey.New("")

	rec, raw, _ := store.Create("limited")
	if ok, err := store.UpdateLimits(rec.ID, apikey.Limits{Models: []string{"glm-*"}, RPM: 0, TPM: 0, MaxConcurrent: 0}); err != nil || !ok {
		t.Fatalf("limits: %v %v", ok, err)
	}

	handler := NewHandler(&Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true }),
		APIKey:   "secret",
		Keys:     store,
	})
	do := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+raw)

		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)

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

	if code := do("/v1/messages/count_tokens", `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`).Code; code != http.StatusOK {
		t.Fatalf("白名单模型 count_tokens code=%d", code)
	}

	denied = do("/v1/messages/count_tokens", `{"model":"claude-x","messages":[{"role":"user","content":"hi"}]}`)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("白名单外模型 count_tokens code=%d", denied.Code)
	}

	if !strings.Contains(denied.Body.String(), `"type":"error"`) {
		t.Fatalf("count_tokens 拒绝应为 Anthropic 错误体: %s", denied.Body)
	}
}

// TestNamedKeyModelWhitelistBackgroundAnthropic 后台模型改写后的目标同样参与
// Anthropic 两个端点的白名单校验，避免 count_tokens 与 /v1/messages 口径不一致。
func TestNamedKeyModelWhitelistBackgroundAnthropic(t *testing.T) {
	t.Parallel()

	store := apikey.New("")
	rec, raw, _ := store.Create("limited")
	setModels := func(models ...string) {
		t.Helper()

		if _, err := store.UpdateLimits(rec.ID, apikey.Limits{Models: models, RPM: 0, TPM: 0, MaxConcurrent: 0}); err != nil {
			t.Fatal(err)
		}
	}
	handler := NewHandler(&Config{
		Pool:             testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:         newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true }),
		APIKey:           "secret",
		Keys:             store,
		BackgroundNames:  map[string]bool{"gpt-5-background": true},
		BackgroundTarget: "deepseek-v4.1-flash",
	})
	do := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+raw)

		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)

		return recorder
	}
	msg := `{"model":"gpt-5-background","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`

	setModels("deepseek-v4.1-flash")

	if code := do("/v1/messages/count_tokens", `{"model":"gpt-5-background","messages":[{"role":"user","content":"hi"}]}`).Code; code != http.StatusOK {
		t.Fatalf("白名单目标模型 count_tokens code=%d", code)
	}

	// 只写原模型名不再生效：两个端点都按改写目标判定。
	setModels("gpt-5-background")

	if code := do("/v1/messages", msg).Code; code != http.StatusForbidden {
		t.Fatalf("原模型名不应绕过 /v1/messages 白名单 code=%d", code)
	}

	if code := do("/v1/messages/count_tokens", `{"model":"gpt-5-background","messages":[{"role":"user","content":"hi"}]}`).Code; code != http.StatusForbidden {
		t.Fatalf("原模型名不应绕过 count_tokens 白名单 code=%d", code)
	}
}

// TestNamedKeyModelWhitelistUsesFinalRealm 白名单按最终出口匹配：key 绑定 global
// 时裸模型名不得被固定按 cn 误判。
func TestNamedKeyModelWhitelistUsesFinalRealm(t *testing.T) {
	t.Parallel()

	store := apikey.New("")

	rec, raw, _ := store.Create("global-key")
	if _, err := store.UpdateLimits(rec.ID, apikey.Limits{Models: []string{"global:glm-*"}, RPM: 0, TPM: 0, MaxConcurrent: 0}); err != nil {
		t.Fatal(err)
	}

	if _, err := store.SetRealm(rec.ID, "global"); err != nil {
		t.Fatal(err)
	}

	acct := &auth.Auth{UID: "g1", AccessToken: "at", ExpiresAt: 9999999999}
	if _, err := auth.BackfillRealmFor(acct, "global"); err != nil {
		t.Fatal(err)
	}

	handler := NewHandler(&Config{
		Pool:     testPoolWith(acct),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true }),
		APIKey:   "secret",
		Keys:     store,
	})

	do := func(model string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"`+model+`","messages":[]}`))
		req.Header.Set("Authorization", "Bearer "+raw)

		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)

		return recorder
	}
	if code := do("glm-5.2").Code; code != http.StatusOK {
		t.Fatalf("global key 裸名应命中 global 白名单 code=%d", code)
	}
	// 改成只允许 cn：同一请求必须被拒，不能因裸名默认 cn 而放行。
	if _, err := store.UpdateLimits(rec.ID, apikey.Limits{Models: []string{"cn:glm-*"}, RPM: 0, TPM: 0, MaxConcurrent: 0}); err != nil {
		t.Fatal(err)
	}

	if code := do("glm-5.2").Code; code != http.StatusForbidden {
		t.Fatalf("global key 不应命中 cn 白名单 code=%d", code)
	}
}

// TestNamedKeyModelWhitelistUsesAliasTarget 白名单校验别名解析后的目标模型，
// 避免用别名绕过限制。
func TestNamedKeyModelWhitelistUsesAliasTarget(t *testing.T) {
	t.Parallel()

	store := apikey.New("")

	rec, raw, _ := store.Create("alias-key")
	if _, err := store.UpdateLimits(rec.ID, apikey.Limits{Models: []string{"cn:glm-*"}, RPM: 0, TPM: 0, MaxConcurrent: 0}); err != nil {
		t.Fatal(err)
	}

	handler := NewHandler(&Config{
		Pool:         testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream:     newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true }),
		APIKey:       "secret",
		Keys:         store,
		ModelAliases: map[string]string{"fast": "cn:gpt-5"},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"fast","messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+raw)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("别名目标不在白名单内应拒绝 code=%d", recorder.Code)
	}
}
