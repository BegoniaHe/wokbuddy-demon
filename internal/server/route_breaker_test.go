package server

import (
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// TestApplyErrorPolicyRecordsRouteFailures 11136 与 6004 都进入 (realm, model)
// 路由熔断计数；三次后 RouteRecovery 有值。
func TestApplyErrorPolicyRecordsRouteFailures(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		kind upstream.ErrKind
		body string
	}{
		{"model_blocked", upstream.ErrModelBlocked, `{"code":11136,"msg":"no model permission"}`},
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
