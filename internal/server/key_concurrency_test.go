package server

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/apikey"
	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// TestNamedKeyConcurrencyErrorCode 并发超限是监管不是整形（issue #448）：
// 立刻 429，错误码与 RPM/TPM 分开（concurrency_limit_exceeded），带 Retry-After。
func TestNamedKeyConcurrencyErrorCode(t *testing.T) {
	t.Parallel()

	store := apikey.New("")

	rec, raw, err := store.Create("limited")
	if err != nil {
		t.Fatal(err)
	}

	if ok, err := store.UpdateLimits(rec.ID, apikey.Limits{MaxConcurrent: 1}); err != nil || !ok {
		t.Fatalf("limits: %v %v", ok, err)
	}

	release := make(chan struct{})

	var once sync.Once

	fake := newFakeUpstream(t, func(string) (int, string, bool) {
		once.Do(func() { <-release }) // 首条流挂住，占满在途
		return 200, sseOK, true
	})

	handler := NewHandler(&Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: fake,
		APIKey:   "secret",
		Keys:     store,
	})

	do := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
		req.Header.Set("Authorization", "Bearer "+raw)

		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)

		return recorder
	}

	done := make(chan *httptest.ResponseRecorder, 1)

	go func() { done <- do() }()

	// 等首条真正进入上游后再发第二条（轮换退避已置 0，睡眠极短即可）。
	time.Sleep(50 * time.Millisecond)

	second := do()
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("并发超限 code=%d body=%s", second.Code, second.Body)
	}

	if body := second.Body.String(); !strings.Contains(body, "concurrency_limit_exceeded") {
		t.Fatalf("错误码应为 concurrency_limit_exceeded: %s", body)
	}

	if ra := second.Header().Get("Retry-After"); ra == "" {
		t.Fatal("并发拒绝应带 Retry-After")
	} else if n, err := strconv.Atoi(ra); err != nil || n < 1 {
		t.Fatalf("Retry-After=%q", ra)
	}

	close(release)

	if first := <-done; first.Code != http.StatusOK {
		t.Fatalf("首条（放行）code=%d body=%s", first.Code, first.Body)
	}
}

// TestNamedKeyConcurrencyAnthropicShape Anthropic 面并发拒绝：Anthropic 信封，
// type 仍为 rate_limit_error，但 message 可区分。
func TestNamedKeyConcurrencyAnthropicShape(t *testing.T) {
	t.Parallel()

	store := apikey.New("")

	rec, raw, err := store.Create("limited")
	if err != nil {
		t.Fatal(err)
	}

	if ok, err := store.UpdateLimits(rec.ID, apikey.Limits{MaxConcurrent: 1}); err != nil || !ok {
		t.Fatalf("limits: %v %v", ok, err)
	}

	release := make(chan struct{})

	var once sync.Once

	fake := newFakeUpstream(t, func(string) (int, string, bool) {
		once.Do(func() { <-release })
		return 200, sseOK, true
	})

	handler := NewHandler(&Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: fake,
		APIKey:   "secret",
		Keys:     store,
	})

	do := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages",
			strings.NewReader(`{"model":"glm-5.2","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer "+raw)
		req.Header.Set("Anthropic-Version", "2023-06-01")

		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)

		return recorder
	}

	done := make(chan *httptest.ResponseRecorder, 1)

	go func() { done <- do() }()

	time.Sleep(50 * time.Millisecond)

	second := do()
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d body=%s", second.Code, second.Body)
	}

	body := second.Body.String()
	if !strings.Contains(body, `"type":"error"`) || !strings.Contains(body, `"rate_limit_error"`) {
		t.Fatalf("Anthropic 信封不正确: %s", body)
	}

	if !strings.Contains(body, "concurrency_limit_exceeded") {
		t.Fatalf("message 应可区分并发拒绝: %s", body)
	}

	close(release)

	if first := <-done; first.Code != http.StatusOK {
		t.Fatalf("首条 code=%d body=%s", first.Code, first.Body)
	}
}

// TestNamedKeyRateNoLongerRejects 新语义（issue #448）：RPM 超速不再 429，
// 请求照常放行（整形在写出侧）。默认密钥不受命名限制影响。
func TestNamedKeyRateNoLongerRejects(t *testing.T) {
	t.Parallel()

	store := apikey.New("")

	rec, raw, err := store.Create("limited")
	if err != nil {
		t.Fatal(err)
	}

	if ok, err := store.UpdateLimits(rec.ID, apikey.Limits{RPM: 600}); err != nil || !ok {
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

	// rpm=1 也连发三发：全部 200，无一 429。
	for i := range 3 {
		if code := do(raw).Code; code != http.StatusOK {
			t.Fatalf("第 %d 发 code=%d（RPM 不应再拒绝）", i, code)
		}
	}

	if code := do("secret").Code; code != http.StatusOK {
		t.Fatalf("默认密钥被误伤 code=%d", code)
	}
}
