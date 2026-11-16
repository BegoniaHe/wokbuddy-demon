// modelrules_gate_test.go 钉住「全池被模型规则挡住」的对外语义（issue #569）：
// 回 400 model_not_allowed 而不是 503 no_healthy_account（规则不会自愈，503 会被
// 客户端当成上游抖动反复重试），hint 带 blocked/eligible 计数；只要还有号可选就
// 照常请求。
package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

func TestModelRuleBlocksAllAccounts(t *testing.T) {
	t.Parallel()

	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})

	rule, err := state.UpsertRule(pool.ModelRule{Mode: pool.ModelRuleDeny, Models: []string{"glm-5.2"}})
	if err != nil {
		t.Fatal(err)
	}

	if err := state.AssignRules("u1", []string{rule.ID}); err != nil {
		t.Fatal(err)
	}

	h := NewHandler(&Config{Pool: state, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	body := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400（全池被规则挡住）, body=%s", rec.Code, rec.Body)
	}

	got := rec.Body.String()
	if !strings.Contains(got, "model_not_allowed") {
		t.Errorf("body=%s want model_not_allowed", got)
	}

	if !strings.Contains(got, "blocked=1") || !strings.Contains(got, "eligible=1") {
		t.Errorf("hint 应带 blocked/eligible 计数: %s", got)
	}
}

// TestModelRulePartialBlockStillRoutes 只要还有账号可选就照常请求（不被规则整体拦下）。
func TestModelRulePartialBlockStillRoutes(t *testing.T) {
	t.Parallel()

	// 假上游返回最小合法非流式响应，证明请求确实打到了上游。
	fake := newFakeUpstream(t, func(string) (int, string, bool) {
		return http.StatusOK, sseOK, true
	})

	state := testPoolWith(
		&auth.Auth{UID: "blocked", AccessToken: "at", ExpiresAt: 9999999999},
		&auth.Auth{UID: "free", AccessToken: "at", ExpiresAt: 9999999999},
	)

	rule, err := state.UpsertRule(pool.ModelRule{Mode: pool.ModelRuleDeny, Models: []string{"glm-5.2"}})
	if err != nil {
		t.Fatal(err)
	}

	if err := state.AssignRules("blocked", []string{rule.ID}); err != nil {
		t.Fatal(err)
	}

	h := NewHandler(&Config{Pool: state, Upstream: fake})
	rec := httptest.NewRecorder()
	body := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200（还有可选账号）, body=%s", rec.Code, rec.Body)
	}

	if strings.Contains(rec.Body.String(), "model_not_allowed") {
		t.Errorf("部分账号被挡不应整体拒绝: %s", rec.Body)
	}
}
