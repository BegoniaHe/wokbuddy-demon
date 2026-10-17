package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
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
