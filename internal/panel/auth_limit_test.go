package panel

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/apikey"
)

// TestPanelAuthFailLimit 面板 API 连续失败达阈值后 429 + Retry-After；不同 IP 独立。
func TestPanelAuthFailLimit(t *testing.T) {
	t.Parallel()

	panel := panelWithKeys(apikey.New(""))
	do := func(key, remote string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/panel/api/keys", nil)

		req.RemoteAddr = remote
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}

		rec := httptest.NewRecorder()
		panel.ServeHTTP(rec, req)

		return rec
	}

	const ip = "198.51.100.9:1000"
	for i := range 10 {
		if code := do("wrong", ip).Code; code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次 code=%d", i+1, code)
		}
	}

	limited := do("wrong", ip)

	// Retry-After 必须是 1..60 秒：面板限流窗口是 1 分钟，换算向上取整且下限 1
	// （0 会让客户端立即重试，等于没有限流）。
	retryAfter, err := strconv.Atoi(limited.Header().Get("Retry-After"))
	if limited.Code != http.StatusTooManyRequests || err != nil || retryAfter < 1 || retryAfter > 60 {
		t.Fatalf("超限 code=%d retry=%q want 429 + 1..60 秒",
			limited.Code, limited.Header().Get("Retry-After"))
	}

	if code := do("wrong", "203.0.113.5:1000").Code; code != http.StatusUnauthorized {
		t.Fatalf("其他 IP 被误伤 code=%d", code)
	}
}
