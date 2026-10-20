package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// TestRetryAfterSecondsFromAccountCooldown 429 取账号冷却截止（取整到秒）。
func TestRetryAfterSecondsFromAccountCooldown(t *testing.T) {
	t.Parallel()

	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.CooldownSoftRate("u1", time.Minute, time.Now().Add(90*time.Second), "429")
	h := NewHandler(&Config{Pool: p})
	chatFlow2 := &chatFlow{h: h, bareModel: "m", st: &chatStat{uid: "u1"}}

	got := chatFlow2.retryAfterSeconds(http.StatusTooManyRequests)
	if got < 89 || got > 91 {
		t.Fatalf("Retry-After=%d want ~90", got)
	}

	if got := chatFlow2.retryAfterSeconds(http.StatusBadRequest); got != 0 {
		t.Fatalf("400 不应带 Retry-After，got %d", got)
	}
}

// TestRetryAfterSecondsFromModelBlock 503 取模型级最早解封时间。
func TestRetryAfterSecondsFromModelBlock(t *testing.T) {
	t.Parallel()

	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(30*time.Second), "m", "6004")
	h := NewHandler(&Config{Pool: p})
	f := &chatFlow{h: h, bareModel: "m", st: &chatStat{}}

	got := f.retryAfterSeconds(http.StatusServiceUnavailable)
	if got < 29 || got > 30 {
		t.Fatalf("Retry-After=%d want ~30", got)
	}
}

// TestRetryAfterHeaderOn429 端到端：上游 429 + Retry-After → 客户端响应带该头。
func TestRetryAfterHeaderOn429(t *testing.T) {
	t.Parallel()

	client := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Header: http.Header{
					"Content-Type": {"application/json"},
					"Retry-After":  {"2"},
				},
				Body: io.NopCloser(strings.NewReader(`{"error":{"message":"rate limit"}}`)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(&Config{Pool: p, Upstream: client})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	got := rec.Header().Get("Retry-After")
	if got == "" {
		t.Fatal("429 响应缺少 Retry-After")
	}

	n, err := strconv.Atoi(got)
	if err != nil || n < 1 || n > 2 {
		t.Fatalf("Retry-After=%q", got)
	}
}

// TestRetryAfterAbsentWhenUnknown 503 无可靠解封时间时不带该头。
func TestRetryAfterAbsentWhenUnknown(t *testing.T) {
	t.Parallel()

	h := NewHandler(&Config{Pool: testPoolWith()})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Fatalf("不可确定时不应带 Retry-After，got %q", got)
	}
}

// TestRetryAfterSecondsOnlyCurrentModel 账号上有多个模型冷却时，只取当前模型的
// 截止时间，避免返回一个客户端按此重试仍会失败的过短等待。
func TestRetryAfterSecondsOnlyCurrentModel(t *testing.T) {
	t.Parallel()

	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(10*time.Second), "other", "6004")
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(90*time.Second), "m", "6004")
	h := NewHandler(&Config{Pool: p})
	f := &chatFlow{h: h, bareModel: "m", st: &chatStat{uid: "u1"}}

	got := f.retryAfterSeconds(http.StatusTooManyRequests)
	if got < 89 || got > 91 {
		t.Fatalf("Retry-After=%d want ~90（当前模型冷却）", got)
	}
}

// TestModelBlockOverrideRateLimited 全池 6004：模型级阻塞必须回 429
// rate_limit_exceeded 并带 Retry-After，不能报成 400 model_unavailable。
func TestModelBlockOverrideRateLimited(t *testing.T) {
	t.Parallel()

	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(45*time.Second), "m", "6004 model rate limit")
	h := NewHandler(&Config{Pool: p})
	flow := &chatFlow{h: h, bareModel: "m", st: &chatStat{}, modelBlock: p.ModelBlocked("m")}

	status, code, _, hint := flow.applyModelBlockOverride(http.StatusServiceUnavailable, "no_healthy_account", "x", "", false)
	if status != http.StatusTooManyRequests || code != "rate_limit_exceeded" {
		t.Fatalf("status=%d code=%q want 429 rate_limit_exceeded", status, code)
	}

	if hint == "" {
		t.Fatal("hint 应说明模型级冷却")
	}

	if got := flow.retryAfterSeconds(status); got < 44 || got > 46 {
		t.Fatalf("Retry-After=%d want ~45", got)
	}
}

// TestModelBlockRateLimitedScopedToRealm 混合池里 cn 账号全被 6004 挡住、
// global 账号健康：bare 模型走 cn 出口，必须回 429 而不是 503 no_healthy_account。
func TestModelBlockRateLimitedScopedToRealm(t *testing.T) {
	t.Parallel()

	client := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(
		&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "atg", ExpiresAt: 9999999999},
	)
	p.CooldownSoftForModel("cn1", time.Minute, time.Now().Add(time.Hour), "glm-5.2", "6004 model rate limit")
	h := NewHandler(&Config{Pool: p, Upstream: client})

	rec := postChat(t, h, `{"model":"glm-5.2","messages":[]}`)
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "rate_limit_exceeded") {
		t.Fatalf("code=%d body=%s want 429 rate_limit_exceeded", rec.Code, rec.Body)
	}

	if got := rec.Header().Get("Retry-After"); got == "" {
		t.Error("模型级 429 应带 Retry-After: header 缺失")
	}
}
