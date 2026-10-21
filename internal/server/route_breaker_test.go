package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// TestApplyErrorPolicyRecordsRouteFailures 11141（平台下线）与 6004（模型级限流）
// 都进入 (realm, model) 路由熔断计数；三次后 RouteRecovery 有值。
func TestApplyErrorPolicyRecordsRouteFailures(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		kind upstream.ErrKind
		body string
	}{
		{"model_retired", upstream.ErrModelAccess, `{"code":11141,"msg":"model retired"}`},
		{"model_rate_limit", upstream.ErrSoftRate, `{"code":6004,"msg":"model usage limit exceeded"}`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			state := pool.New("")
			state.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})

			h := NewHandler(&Config{Pool: state})
			for range 3 {
				h.applyErrorPolicy("u1", testCase.kind, testCase.body, "cn", "m1", nil)
			}

			if until := state.RouteRecovery("cn", "m1"); until.IsZero() {
				t.Fatal("route cooldown not recorded")
			}
		})
	}
}

// TestApplyErrorPolicy14003NoRouteCooldown 14003 是模型后端瞬时繁忙，只停
// (账号,模型) 短窗，不写路由熔断；否则三次后整条模型路由会被误冷却。
func TestApplyErrorPolicy14003NoRouteCooldown(t *testing.T) {
	t.Parallel()

	state := pool.New("")
	state.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})

	h := NewHandler(&Config{Pool: state})
	for range 3 {
		h.applyErrorPolicy("u1", upstream.ErrSoftRate, `{"code":14003,"msg":"too many requests"}`, "cn", "m1", nil)
	}

	if until := state.RouteRecovery("cn", "m1"); !until.IsZero() {
		t.Fatalf("14003 不应写路由熔断，got until=%v", until)
	}

	if st := statusOf(t, state, "u1"); st.Cooling {
		t.Fatalf("14003 只停模型，不应冷却账号: %+v", st)
	}
}

// TestApplyErrorPolicyModelNotFoundNoRouteCooldown 11102 是请求级事实（上游目录
// 无此模型），不写路由熔断：同模型后续请求应继续得到 404，而不是被冷却成 503。
func TestApplyErrorPolicyModelNotFoundNoRouteCooldown(t *testing.T) {
	t.Parallel()

	state := pool.New("")
	state.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})

	h := NewHandler(&Config{Pool: state})
	for range 3 {
		h.applyErrorPolicy("u1", upstream.ErrModelNotFound, `{"code":11102,"msg":"service info not found"}`, "cn", "m1", nil)
	}

	if until := state.RouteRecovery("cn", "m1"); !until.IsZero() {
		t.Fatalf("11102 不应写路由熔断，got until=%v", until)
	}

	if st := statusOf(t, state, "u1"); st.Cooling {
		t.Fatalf("11102 不应惩罚账号，status=%+v", st)
	}
}

// TestApplyErrorPolicyModelPermissionNoRouteCooldown 11136 是账号级权限缺失，
// 其他账号可能仍有权限：只写 (账号,模型) 负缓存，不把整条路由冷却。
func TestApplyErrorPolicyModelPermissionNoRouteCooldown(t *testing.T) {
	t.Parallel()

	state := pool.New("")
	state.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})

	h := NewHandler(&Config{Pool: state})
	for range 3 {
		h.applyErrorPolicy("u1", upstream.ErrModelBlocked, `{"code":11136,"msg":"no model permission"}`, "cn", "m1", nil)
	}

	if until := state.RouteRecovery("cn", "m1"); !until.IsZero() {
		t.Fatalf("11136 不应写路由熔断，got until=%v", until)
	}

	if st := state.ModelBlocked("m1"); !st.Blocked {
		t.Fatalf("11136 仍应写 (账号,模型) 负缓存，status=%+v", st)
	}
}

// TestModelPermissionFailuresDoNotStarvePermittedAccount 三个无权限账号的连续
// 11136 不应触发路由冷却：仍有权限的第四个账号必须能被选中，而不是整条路由 503。
func TestModelPermissionFailuresDoNotStarvePermittedAccount(t *testing.T) {
	t.Parallel()

	state := pool.New("")
	for _, uid := range []string{"u1", "u2", "u3", "u4"} {
		state.Add(&auth.Auth{UID: uid, AccessToken: "at", ExpiresAt: 9999999999})
	}

	h := NewHandler(&Config{Pool: state})
	for _, uid := range []string{"u1", "u2", "u3"} {
		h.applyErrorPolicy(uid, upstream.ErrModelBlocked, `{"code":11136,"msg":"no model permission"}`, "cn", "m1", nil)
	}

	if until := state.RouteRecovery("cn", "m1"); !until.IsZero() {
		t.Fatalf("11136 不应触发路由冷却，got until=%v", until)
	}

	acct := state.PickExcludingForRealm(nil, "m1", "cn")
	if acct == nil || acct.UID != "u4" {
		t.Fatalf("仍有权限的 u4 应可选，got %+v", acct)
	}
}

// TestChatAllAccountsLackModelPermission 全池 11136：每个号只试一次，随后由
// (账号,模型) 负缓存挡住选号，返回 400 model_unavailable；不写路由冷却。
func TestChatAllAccountsLackModelPermission(t *testing.T) {
	t.Parallel()

	var calls int

	client := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return http.StatusBadRequest, `{"code":11136,"msg":"no model permission"}`, false
	})
	state := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	handler := NewHandler(&Config{Pool: state, Upstream: client})

	rec := postChat(t, handler, `{"model":"glm-5.3","messages":[]}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "model_unavailable") {
		t.Fatalf("code=%d body=%s want 400 model_unavailable", rec.Code, rec.Body)
	}

	if calls != 2 {
		t.Errorf("upstream calls=%d want 2（每号一次，不逐号轮转）", calls)
	}

	rec = postChat(t, handler, `{"model":"glm-5.3","messages":[]}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "model_unavailable") {
		t.Errorf("second request: code=%d body=%s want 400 model_unavailable", rec.Code, rec.Body)
	}

	if calls != 2 {
		t.Errorf("second request must fast-fail without upstream calls, calls=%d", calls)
	}

	if until := state.RouteRecovery("cn", "glm-5.3"); !until.IsZero() {
		t.Errorf("11136 不应写路由熔断，got until=%v", until)
	}

	if st := state.ModelBlocked("glm-5.3"); !st.Blocked || st.Count != 2 {
		t.Errorf("全池应写入 (账号,模型) 负缓存，status=%+v", st)
	}
}

// TestChatAllAccountsForbiddenByUserType 全池 11158（账号类型级模型权限拒绝）：
// 与 11136 同机制——每号只打一次上游，写入 (账号,模型) 负缓存，返回 400
// model_unavailable + 上游原文，且不写路由熔断、不罚账号。
//
// 400+11158 归模型权限；429+11158 保留限流语义（见 upstream 侧分类表用例），
// 与上游一致，避免把限流误判成整池权限缺失。
func TestChatAllAccountsForbiddenByUserType(t *testing.T) {
	t.Parallel()

	var calls int

	client := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++

		return http.StatusBadRequest,
			`{"code":11158,"msg":"model [glm-5.3] is not allowed for user type [personal]"}`, false
	})
	state := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	handler := NewHandler(&Config{Pool: state, Upstream: client})

	rec := postChat(t, handler, `{"model":"glm-5.3","messages":[]}`)

	body := rec.Body.String()
	if rec.Code != http.StatusBadRequest || !strings.Contains(body, "model_unavailable") {
		t.Fatalf("code=%d body=%s want 400 model_unavailable", rec.Code, body)
	}

	if !strings.Contains(body, "is not allowed for user type") {
		t.Errorf("上游原文必须透传，body=%s", body)
	}

	if calls != 2 {
		t.Errorf("upstream calls=%d want 2（每号一次，不逐号轮转）", calls)
	}

	// 第二次请求走既有负缓存，不再打上游。
	postChat(t, handler, `{"model":"glm-5.3","messages":[]}`)

	if calls != 2 {
		t.Errorf("负缓存命中后不应再打上游，calls=%d", calls)
	}

	if until := state.RouteRecovery("cn", "glm-5.3"); !until.IsZero() {
		t.Errorf("11158 不应写路由熔断，got until=%v", until)
	}

	if st := state.ModelBlocked("glm-5.3"); !st.Blocked || st.Count != 2 {
		t.Fatalf("全池应写入 (账号,模型) 负缓存，status=%+v", st)
	}
}

// TestForbiddenByUserTypeNegativeCacheReason 11158 的负缓存 reason 落到
// upstream.ModelForbiddenReason 前缀（面板可区分「账号类型不允许」与「无权限」），
// 且不罚账号。
func TestForbiddenByUserTypeNegativeCacheReason(t *testing.T) {
	t.Parallel()

	state := pool.New("")
	state.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})

	h := NewHandler(&Config{Pool: state})
	h.applyErrorPolicy("u1", upstream.ErrModelBlocked,
		`{"code":11158,"msg":"model [m1] is not allowed for user type [personal]"}`, "cn", "m1", nil)

	rows := statusOf(t, state, "u1").RateLimitedModels
	if len(rows) != 1 || rows[0].Model != "m1" {
		t.Fatalf("应写 1 条 m1 避让台账，got %+v", rows)
	}

	if !strings.HasPrefix(rows[0].Reason, upstream.ModelForbiddenReason) {
		t.Errorf("reason=%q want 前缀 %q", rows[0].Reason, upstream.ModelForbiddenReason)
	}

	if rows[0].Kind != "model_unavailable" {
		t.Errorf("kind=%q want model_unavailable", rows[0].Kind)
	}

	if st := statusOf(t, state, "u1"); st.Cooling || st.ErrTotal != 0 {
		t.Errorf("11158 不应惩罚账号，status=%+v", st)
	}
}
