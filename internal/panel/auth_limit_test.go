package panel

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/apikey"
)

// TestPanelAuthFailLimit 面板 API 连续失败达阈值后 429 + Retry-After；不同 IP 独立。
func TestPanelAuthFailLimit(t *testing.T) {
	p := panelWithKeys(apikey.New(""))
	do := func(key, remote string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/panel/api/keys", nil)
		req.RemoteAddr = remote
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		return rec
	}
	const ip = "198.51.100.9:1000"
	for i := range 10 {
		if code := do("wrong", ip).Code; code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次 code=%d", i+1, code)
		}
	}
	limited := do("wrong", ip)
	if limited.Code != http.StatusTooManyRequests || limited.Header().Get("Retry-After") == "" {
		t.Fatalf("超限 code=%d retry=%q", limited.Code, limited.Header().Get("Retry-After"))
	}
	if code := do("wrong", "203.0.113.5:1000").Code; code != http.StatusUnauthorized {
		t.Fatalf("其他 IP 被误伤 code=%d", code)
	}
}
