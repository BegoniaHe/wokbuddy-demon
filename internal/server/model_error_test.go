package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// TestChat14003StaysModelScopedShortWindow 14003 模型繁忙只停 (账号,模型)，
// 账号对其他模型仍可用，最终按可重试的 429 返回。
func TestChat14003StaysModelScopedShortWindow(t *testing.T) {
	t.Parallel()

	client := newFakeUpstream(t, func(string) (int, string, bool) {
		return http.StatusTooManyRequests, `{"code":14003,"msg":"too many requests"}`, false
	})
	poolState := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(&Config{Pool: poolState, Upstream: client, PromptMode: "passthrough", MaxRotate: 1})

	rec := postChat(t, h, `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d body=%s want 429", rec.Code, rec.Body.String())
	}

	status, _ := poolState.Status("u1")
	if status.Cooling {
		t.Fatalf("14003 must not cool the whole account: %+v", status)
	}

	if len(status.RateLimitedModels) != 1 || status.RateLimitedModels[0].Kind != "rate_limit" {
		t.Fatalf("rate limited models=%+v want one rate_limit row", status.RateLimitedModels)
	}
}

// TestChat11158ModelPermissionScoped 11158 账号类型无模型权限：只写 (账号,模型)
// 负缓存并返回 400，不喂连败降权。
func TestChat11158ModelPermissionScoped(t *testing.T) {
	t.Parallel()

	client := newFakeUpstream(t, func(string) (int, string, bool) {
		return http.StatusBadRequest,
			`{"code":11158,"msg":"model [x] is not allowed for user type [personal_free_user]"}`, false
	})
	poolState := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(&Config{Pool: poolState, Upstream: client, PromptMode: "passthrough", MaxRotate: 1})

	rec := postChat(t, h, `{"model":"x","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s want 400", rec.Code, rec.Body.String())
	}

	if !strings.Contains(rec.Body.String(), `"code":"model_unavailable"`) {
		t.Fatalf("body=%s want model_unavailable", rec.Body.String())
	}

	status, _ := poolState.Status("u1")
	if len(status.RateLimitedModels) != 1 || status.RateLimitedModels[0].Kind != "model_unavailable" {
		t.Fatalf("rate limited models=%+v want model_unavailable row", status.RateLimitedModels)
	}

	if status.ConsecutiveFails != 0 {
		t.Fatalf("11158 must not feed consecutive failures, got %d", status.ConsecutiveFails)
	}
}

// TestChat6004ModelCooldownReports429 全池被 6004 模型级冷却挡住时必须回
// 429 rate_limit_exceeded（等上游重置），不能退化成 400 model_unavailable。
// 回归点：冷却 reason 必须保持 "6004" 前缀，供 pool.modelRateLimitReason 识别。
func TestChat6004ModelCooldownReports429(t *testing.T) {
	t.Parallel()

	reset := time.Now().Add(5 * time.Minute)
	str := reset.In(upstream.SoftRateResetLoc()).Format("2006-01-02 15:04:05")

	client := newFakeUpstream(t, func(string) (int, string, bool) {
		return http.StatusTooManyRequests, `{"code":6004,"msg":"将在 ` + str + ` UTC+8 重置"}`, false
	})
	poolState := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	handler := NewHandler(&Config{Pool: poolState, Upstream: client, PromptMode: "passthrough", MaxRotate: 1})

	// 第一次：撞上游 6004，写入模型级冷却。
	if rec := postChat(t, handler, `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("first code=%d body=%s want 429", rec.Code, rec.Body.String())
	}

	// 第二次：池内已无可用账号，应按模型级限流返回 429 + Retry-After。
	rec := postChat(t, handler, `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), `"code":"rate_limit_exceeded"`) {
		t.Fatalf("second code=%d body=%s want 429 rate_limit_exceeded", rec.Code, rec.Body.String())
	}
}
