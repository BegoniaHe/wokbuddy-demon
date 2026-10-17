package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
)

func TestDailyLimitAllBlockedReturns429(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetDailyLimits(pool.DailyLimits{Tokens: 1}, pool.DailyLimits{}, pool.DailyLimits{})
	p.SetDailyUsageProvider(func() map[string]pool.DailyUsage {
		return map[string]pool.DailyUsage{"u1": {Tokens: 10}}
	})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	body := `{"model":"glm-5.2","messages":[]}`
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d body=%s want 429", rec.Code, rec.Body)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("missing Retry-After for daily limit reset")
	}
	if calls != 0 {
		t.Errorf("upstream calls=%d want 0", calls)
	}
}
