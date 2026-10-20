package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
)

func TestDailyLimitAllBlockedReturns429(t *testing.T) {
	t.Parallel()

	var calls int

	client := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetDailyLimits(pool.DailyLimits{Tokens: 1}, pool.DailyLimits{}, pool.DailyLimits{})
	p.SetDailyUsageProvider(func() map[string]pool.DailyUsage {
		return map[string]pool.DailyUsage{"u1": {Tokens: 10}}
	})
	h := NewHandler(&Config{Pool: p, Upstream: client})
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

// TestModelCooldownPastMidnightNotDaily 全池模型级冷却跨过本地 0 点时，
// 必须回 429 rate_limit_exceeded（模型级限流），不能退化成 daily_limit_reached。
func TestModelCooldownPastMidnightNotDaily(t *testing.T) {
	t.Parallel()

	poolState := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	poolState.SetSoftRateMax(48 * time.Hour)
	poolState.SetDailyUsageProvider(func() map[string]pool.DailyUsage {
		return map[string]pool.DailyUsage{}
	})

	now := time.Now()
	midnight := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, now.Location())
	poolState.CooldownSoftForModel("u1", time.Minute, midnight.Add(time.Hour), "glm-5.2", "6004 model rate limit")

	h := NewHandler(&Config{Pool: poolState})
	rec := postChat(t, h, `{"model":"glm-5.2","messages":[]}`)

	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "rate_limit_exceeded") {
		t.Fatalf("code=%d body=%s want 429 rate_limit_exceeded", rec.Code, rec.Body)
	}

	if strings.Contains(rec.Body.String(), "daily_limit_reached") {
		t.Fatalf("模型级冷却不应报成日限: %s", rec.Body)
	}

	if rec.Header().Get("Retry-After") == "" {
		t.Error("模型级 429 应带 Retry-After: header 缺失")
	}
}
