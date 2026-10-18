// handler_rotate_test.go 钉住轮换策略新增维度（issue #17）：总时长预算与状态码换号闸。
package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// TestChatRotateBudgetStopsEarly 预算耗尽即停止换号（不在任何账号上发起请求）。
func TestChatRotateBudgetStopsEarly(t *testing.T) {
	t.Parallel()

	calls := map[string]int{}
	client := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		if authz == "Bearer at-bad" {
			return 429, `{"code":1,"msg":"rate limit"}`, false
		}

		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	// 1ns 预算：循环顶部即判定超时，任何尝试都不发起。
	h := NewHandler(&Config{Pool: p, Upstream: client, RotateBudget: time.Nanosecond, SoftCooldown: time.Minute})

	rec := postChat(t, h, `{"model":"glm-5.2","messages":[]}`)
	if rec.Code == http.StatusOK {
		t.Fatalf("budget exhausted must not succeed, code=%d", rec.Code)
	}

	if len(calls) != 0 {
		t.Errorf("budget exhausted should make no upstream call, calls=%v", calls)
	}
}

// TestChatRotateStatusGate 状态码闸：402 不在 allowlist（仅 429）→ 不换号。
func TestChatRotateStatusGate(t *testing.T) {
	t.Parallel()

	calls := map[string]int{}
	client := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		if authz == "Bearer at-bad" {
			return 402, `{"code":1,"msg":"余额不足"}`, false
		}

		return 200, sseOK, true
	})
	pool := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	pool.SetCredits("bad", 2000, 0) // 让 bad 先被选中
	h := NewHandler(&Config{
		Pool: pool, Upstream: client, SoftCooldown: time.Minute,
		RotateStatuses: map[int]bool{http.StatusTooManyRequests: true},
	})

	rec := postChat(t, h, `{"model":"glm-5.2","messages":[]}`)
	if rec.Code == http.StatusOK {
		t.Fatalf("402 not in allowlist must not rotate to success, code=%d", rec.Code)
	}

	if calls["Bearer at-good"] != 0 {
		t.Errorf("good must not be tried when 402 not allowed, calls=%v", calls)
	}
	// 账号惩罚照常：bad 已进入冷却。
	if bad := statusOf(t, pool, "bad"); !bad.Cooling {
		t.Errorf("bad account should still be penalized: %+v", bad)
	}
}
