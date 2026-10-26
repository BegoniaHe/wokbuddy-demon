package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/apikey"
	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/httpauth"
)

func newLimitHandler(t *testing.T, lim apikey.Limits, trusted []string) (*Handler, *apikey.Store, string, *atomic.Int64) {
	t.Helper()

	store := apikey.New("")

	_, raw, err := store.Create("limited", lim)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	var calls atomic.Int64

	fake := newFakeUpstream(t, func(string) (int, string, bool) {
		calls.Add(1)
		return 200, sseOK, true
	})

	proxies, err := httpauth.ParseTrustedProxies(trusted)
	if err != nil {
		t.Fatalf("parse trusted proxies: %v", err)
	}

	handler := NewHandler(&Config{
		Pool:           testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream:       fake,
		APIKey:         "secret",
		Keys:           store,
		TrustedProxies: proxies,
	})

	return handler, store, raw, &calls
}

func chatAs(t *testing.T, handler *Handler, raw, remote, path string) *httptest.ResponseRecorder {
	t.Helper()

	body := `{"model":"glm-5.2","messages":[]}`

	if strings.HasPrefix(path, "/v1/messages") {
		body = `{"model":"glm-5.2","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`
	}

	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.RemoteAddr = remote
	req.Header.Set("Authorization", "Bearer "+raw)

	if strings.HasPrefix(path, "/v1/messages") {
		req.Header.Set("Anthropic-Version", "2023-06-01")
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	return rec
}

// TestNamedKeyExpiredRejected 过期密钥 401 泛化错误，不进入上游，且计入 Rejected。
func TestNamedKeyExpiredRejected(t *testing.T) {
	t.Parallel()

	past := time.Now().Add(-time.Minute)
	handler, store, raw, calls := newLimitHandler(t, apikey.Limits{ExpiresAt: &past}, nil)

	rec := chatAs(t, handler, raw, "198.51.100.1:1000", "/v1/chat/completions")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if !strings.Contains(rec.Body.String(), "invalid_api_key") {
		t.Fatalf("应返回泛化 401: %s", rec.Body)
	}

	if strings.Contains(rec.Body.String(), raw) {
		t.Fatal("错误体泄露密钥")
	}

	if calls.Load() != 0 {
		t.Fatalf("过期密钥不应进入上游: calls=%d", calls.Load())
	}

	if got := store.List()[0].Rejected; got != 1 {
		t.Fatalf("rejected=%d want 1", got)
	}
}

// TestNamedKeyQuotaExhausted 恰好等于/超过配额返回 429 insufficient_quota，
// 不带 Retry-After，不进入上游。
func TestNamedKeyQuotaExhausted(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		total int64
	}{
		{"恰好等于", 100},
		{"超额", 150},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			handler, store, raw, calls := newLimitHandler(t, apikey.Limits{QuotaTokens: 100}, nil)
			id := store.List()[0].ID
			store.RecordUsage(id, 0, testCase.total, testCase.total)

			rec := chatAs(t, handler, raw, "198.51.100.1:1000", "/v1/chat/completions")
			if rec.Code != http.StatusTooManyRequests {
				t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
			}

			if got := rec.Header().Get("Retry-After"); got != "" {
				t.Fatalf("配额耗尽不得带 Retry-After: %q", got)
			}

			if !strings.Contains(rec.Body.String(), "insufficient_quota") {
				t.Fatalf("错误码应为 insufficient_quota: %s", rec.Body)
			}

			if calls.Load() != 0 {
				t.Fatalf("配额耗尽不应进入上游: calls=%d", calls.Load())
			}
		})
	}
}

// TestNamedKeyIPAllowlist IPv4/IPv6/CIDR 放行；未命中 403 且不回显来源 IP。
func TestNamedKeyIPAllowlist(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		allow   []string
		remote  string
		allowed bool
	}{
		{"IPv4 命中", []string{"192.0.2.10"}, "192.0.2.10:1234", true},
		{"IPv4 未命中", []string{"192.0.2.10"}, "198.51.100.1:1234", false},
		{"IPv6 命中", []string{"2001:db8::1"}, "[2001:db8::1]:1234", true},
		{"IPv4 CIDR 命中", []string{"198.51.100.0/24"}, "198.51.100.9:1234", true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			handler, _, raw, calls := newLimitHandler(t, apikey.Limits{IPAllowlist: testCase.allow}, nil)

			rec := chatAs(t, handler, raw, testCase.remote, "/v1/chat/completions")
			if testCase.allowed {
				if rec.Code != http.StatusOK {
					t.Fatalf("应放行 code=%d body=%s", rec.Code, rec.Body)
				}

				return
			}

			if rec.Code != http.StatusForbidden {
				t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
			}

			if !strings.Contains(rec.Body.String(), "ip_not_allowed") {
				t.Fatalf("错误码应为 ip_not_allowed: %s", rec.Body)
			}

			if strings.Contains(rec.Body.String(), "198.51.100.1") {
				t.Fatalf("不应回显来源 IP: %s", rec.Body)
			}

			if calls.Load() != 0 {
				t.Fatalf("IP 拒绝不应进入上游: calls=%d", calls.Load())
			}
		})
	}
}

// TestNamedKeyIPAllowlistTrustedProxy 只有受信反代才采信转发头。
func TestNamedKeyIPAllowlistTrustedProxy(t *testing.T) {
	t.Parallel()

	limit := apikey.Limits{IPAllowlist: []string{"192.0.2.10"}}

	t.Run("受信反代采信 XFF", func(t *testing.T) {
		t.Parallel()

		handler, _, raw, _ := newLimitHandler(t, limit, []string{"10.0.0.0/8"})
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
		req.RemoteAddr = "10.0.0.5:1234"
		req.Header.Set("X-Forwarded-For", "192.0.2.10")
		req.Header.Set("Authorization", "Bearer "+raw)

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("受信反代命中白名单应放行 code=%d body=%s", rec.Code, rec.Body)
		}
	})

	t.Run("未受信反代忽略 XFF", func(t *testing.T) {
		t.Parallel()

		handler, _, raw, _ := newLimitHandler(t, limit, []string{"10.0.0.0/8"})
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
		req.RemoteAddr = "203.0.113.9:1234"
		req.Header.Set("X-Forwarded-For", "192.0.2.10")
		req.Header.Set("Authorization", "Bearer "+raw)

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Fatalf("未受信对端不得靠伪造 XFF 通过 code=%d body=%s", rec.Code, rec.Body)
		}
	})
}

// TestDefaultKeyUnaffectedByNamedLimits 默认 api_key 不受命名密钥过期/配额/IP 影响。
func TestDefaultKeyUnaffectedByNamedLimits(t *testing.T) {
	t.Parallel()

	past := time.Now().Add(-time.Minute)
	handler, _, _, _ := newLimitHandler(t, apikey.Limits{
		ExpiresAt: &past, QuotaTokens: 1, IPAllowlist: []string{"192.0.2.10"},
	}, nil)

	rec := chatAs(t, handler, "secret", "203.0.113.1:1234", "/v1/chat/completions")
	if rec.Code != http.StatusOK {
		t.Fatalf("默认密钥被误伤 code=%d body=%s", rec.Code, rec.Body)
	}
}

// TestNamedKeyQuotaAnthropicShape Anthropic 端点同样 429 且无 Retry-After。
func TestNamedKeyQuotaAnthropicShape(t *testing.T) {
	t.Parallel()

	handler, store, raw, _ := newLimitHandler(t, apikey.Limits{QuotaTokens: 1}, nil)
	store.RecordUsage(store.List()[0].ID, 0, 1, 1)

	rec := chatAs(t, handler, raw, "198.51.100.1:1000", "/v1/messages")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if rec.Header().Get("Retry-After") != "" {
		t.Fatal("配额耗尽不得带 Retry-After")
	}

	if body := rec.Body.String(); !strings.Contains(body, `"type":"error"`) ||
		!strings.Contains(body, `"rate_limit_error"`) {
		t.Fatalf("Anthropic 错误体不正确: %s", body)
	}
}

// TestNamedKeyLimitsHotUpdateAndRevoke UpdateLimits 下一个请求生效；撤销后即使
// 限制仍在也不再能鉴权。
func TestNamedKeyLimitsHotUpdateAndRevoke(t *testing.T) {
	t.Parallel()

	handler, store, raw, _ := newLimitHandler(t, apikey.Limits{}, nil)
	id := store.List()[0].ID

	if rec := chatAs(t, handler, raw, "198.51.100.1:1000", "/v1/chat/completions"); rec.Code != http.StatusOK {
		t.Fatalf("初始应放行 code=%d", rec.Code)
	}

	if ok, err := store.UpdateLimits(id, apikey.Limits{QuotaTokens: 1}); err != nil || !ok {
		t.Fatalf("update limits: %v %v", ok, err)
	}

	store.RecordUsage(id, 0, 1, 1)

	if rec := chatAs(t, handler, raw, "198.51.100.1:1000", "/v1/chat/completions"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("热更新后应拒绝 code=%d body=%s", rec.Code, rec.Body)
	}

	if ok, err := store.Revoke(id); err != nil || !ok {
		t.Fatalf("revoke: %v %v", ok, err)
	}

	if rec := chatAs(t, handler, raw, "198.51.100.1:1000", "/v1/chat/completions"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("撤销后应 401 code=%d body=%s", rec.Code, rec.Body)
	}
}
