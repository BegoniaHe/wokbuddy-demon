package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/httpauth"
)

func doStatus(h *Handler, key, remote string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	req.RemoteAddr = remote
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestStatusAuthFailLimit 同一 IP 连续失败达阈值后 429 + Retry-After；其他 IP 不受影响；
// 正确密钥成功后清零窗口。
func TestStatusAuthFailLimit(t *testing.T) {
	h := NewHandler(Config{
		Pool:   testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		APIKey: "secret",
	})
	const ip = "198.51.100.9:1000"
	for i := range 10 {
		if code := doStatus(h, "wrong", ip).Code; code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次失败 code=%d", i+1, code)
		}
	}
	limited := doStatus(h, "wrong", ip)
	if limited.Code != http.StatusTooManyRequests {
		t.Fatalf("超限 code=%d body=%s", limited.Code, limited.Body)
	}
	if limited.Header().Get("Retry-After") == "" {
		t.Fatal("429 缺少 Retry-After")
	}
	if code := doStatus(h, "wrong", "203.0.113.5:1000").Code; code != http.StatusUnauthorized {
		t.Fatalf("其他 IP 被误伤 code=%d", code)
	}
	if code := doStatus(h, "secret", "203.0.113.6:1000").Code; code != http.StatusOK {
		t.Fatalf("正确密钥被误伤 code=%d", code)
	}

	assertStatusLimiterReset(t, h)
}

// assertStatusLimiterReset 成功后窗口清零：后续失败重新从 1 开始计数。
func assertStatusLimiterReset(t *testing.T, h *Handler) {
	t.Helper()
	const ip = "203.0.113.7:1000"
	for range 5 {
		doStatus(h, "wrong", ip)
	}
	if code := doStatus(h, "secret", ip).Code; code != http.StatusOK {
		t.Fatalf("正确密钥 code=%d", code)
	}
	for i := range 9 {
		if code := doStatus(h, "wrong", ip).Code; code != http.StatusUnauthorized {
			t.Fatalf("清零后第 %d 次 code=%d", i+1, code)
		}
	}
}

func doV1(h *Handler, path, key, remote string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = remote
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestClientAuthFailLimit 网关 API 面鉴权失败同样按 IP 限速。
func TestClientAuthFailLimit(t *testing.T) {
	h := NewHandler(Config{
		Pool:   testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		APIKey: "secret",
	})
	const ip = "198.51.100.20:1000"
	for i := range 30 {
		if code := doV1(h, "/v1/models", "wrong", ip).Code; code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次失败 code=%d", i+1, code)
		}
	}
	limited := doV1(h, "/v1/models", "wrong", ip)
	if limited.Code != http.StatusTooManyRequests {
		t.Fatalf("超限 code=%d body=%s", limited.Code, limited.Body)
	}
	if limited.Header().Get("Retry-After") == "" {
		t.Fatal("429 缺少 Retry-After")
	}
	if code := doV1(h, "/v1/models", "wrong", "203.0.113.20:1000").Code; code != http.StatusUnauthorized {
		t.Fatalf("其他 IP 被误伤 code=%d", code)
	}
}

// TestClientAuthTrustedProxyIP 可信反代下，失败计数记在转发头给出的真实 IP，
// 而不是共享的反代地址。
func TestClientAuthTrustedProxyIP(t *testing.T) {
	trusted, err := httpauth.ParseTrustedProxies([]string{"127.0.0.1/32"})
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(Config{
		Pool:           testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		APIKey:         "secret",
		TrustedProxies: trusted,
	})
	for range 30 {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.RemoteAddr = "127.0.0.1:1234"
		req.Header.Set("X-Real-IP", "203.0.113.30")
		req.Header.Set("Authorization", "Bearer wrong")
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	// 同一真实 IP 再试一次应被限速。
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-Real-IP", "203.0.113.30")
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("可信代理的真实 IP 未被限速 code=%d", rec.Code)
	}
	// 另一个真实 IP 不受影响。
	other := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	other.RemoteAddr = "127.0.0.1:1234"
	other.Header.Set("X-Real-IP", "203.0.113.31")
	other.Header.Set("Authorization", "Bearer wrong")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, other)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("其他真实 IP 被误伤 code=%d", rec.Code)
	}
}
