// upstream_overload_test.go 钉住 11134 供应商侧过载的零账号动作收尾（issue #476）。
package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/session"
)

// TestChatUpstreamOverloadKeepsAccountAndSticky 11134 供应商过载（issue #476）：
// 末端 503 + Retry-After + error.code=upstream_overload，账号零惩罚（不冷却、
// 不喂连败、不换号），并且**不解绑**会话粘性——过载与账号无关，绑定必须保住，
// 重试才能命中同号前缀缓存。
func TestChatUpstreamOverloadKeepsAccountAndSticky(t *testing.T) {
	t.Parallel()

	binds := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     binds,
		Available: func() []string { return []string{"one", "two"} },
	})
	state := testPoolWith(
		&auth.Auth{UID: "one", AccessToken: "at-one", ExpiresAt: 9999999999},
		&auth.Auth{UID: "two", AccessToken: "at-two", ExpiresAt: 9999999999},
	)

	calls := map[string]int{}
	client := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		// 上游以 5xx 携带 11134（供应商侧过载的实测形态之一）。
		return http.StatusInternalServerError,
			`{"code":11134,"msg":"模型服务不可用，请切换模型重试"}`, false
	})
	handler := NewHandler(&Config{
		Pool:         state,
		Upstream:     client,
		Session:      sess,
		SoftCooldown: time.Minute,
	})
	// 预绑定：会话粘在 one 上。
	sess.Bind("conv-overload", "one")

	rec := postChat(t, handler,
		`{"model":"glm-5.2","messages":[],"metadata":{"conversation_id":"conv-overload"}}`)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503, body=%s", rec.Code, rec.Body)
	}

	if !strings.Contains(rec.Body.String(), "upstream_overload") {
		t.Errorf("body=%s want error.code=upstream_overload", rec.Body)
	}

	if rec.Header().Get("Retry-After") == "" {
		t.Errorf("missing Retry-After: 过载必须给可重试语义")
	}

	// 零换号：只碰了粘性号一次。
	if calls["Bearer at-two"] != 0 {
		t.Errorf("provider overload must not rotate to another account, calls=%v", calls)
	}

	if total := calls["Bearer at-one"]; total != 1 {
		t.Errorf("attempts on sticky account=%d want 1 (no retry loop)", total)
	}

	// 零账号惩罚：不冷却、不喂连败。
	assertOverloadCleanAccount(t, state, "one")
	assertOverloadCleanAccount(t, state, "two")

	// 粘性未解绑：绑定与解绑计数都不动。
	if uid, ok := binds.lastUID("conv-overload"); !ok || uid != "one" {
		t.Errorf("sticky binding must survive provider overload, got %q ok=%v", uid, ok)
	}

	if binds.delCnt != 0 {
		t.Errorf("unbind called %d times; provider overload must not unbind sticky", binds.delCnt)
	}
}

// TestChatUpstreamOverloadEnvelopeWithoutStatus 200 业务信封携带 11134 时同样
// 不落 ErrClient 的换号/连败路径（issue #476 的第二种入口）。
func TestChatUpstreamOverloadEnvelopeWithoutStatus(t *testing.T) {
	t.Parallel()

	state := testPoolWith(
		&auth.Auth{UID: "one", AccessToken: "at-one", ExpiresAt: 9999999999},
		&auth.Auth{UID: "two", AccessToken: "at-two", ExpiresAt: 9999999999},
	)

	calls := map[string]int{}
	client := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		// 200 + SSE 业务错误帧：首帧分类路径（handlePreStreamErrorFrame）。
		return http.StatusOK,
			"data: {\"code\":11134,\"msg\":\"模型服务不可用，请切换模型重试\"}\n\n", true
	})
	handler := NewHandler(&Config{Pool: state, Upstream: client, SoftCooldown: time.Minute})

	rec := postChat(t, handler, `{"model":"glm-5.2","messages":[],"stream":true}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503, body=%s", rec.Code, rec.Body)
	}

	if total := calls["Bearer at-one"]; total != 1 {
		t.Errorf("attempts=%d want 1 (provider overload must not rotate)", total)
	}

	assertOverloadCleanAccount(t, state, "one")
}

// assertOverloadCleanAccount 断言供应商过载后账号台账无任何惩罚痕迹。
func assertOverloadCleanAccount(t *testing.T, state *pool.Pool, uid string) {
	t.Helper()

	st := statusOf(t, state, uid)
	if st.Cooling || st.ConsecutiveFails != 0 {
		t.Errorf("account %s must stay clean: cooling=%v fails=%d", uid, st.Cooling, st.ConsecutiveFails)
	}
}
