package server

import (
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// TestApplyErrorPolicyRecordsRouteFailures 11102 与 6004 都进入 (realm, model)
// 路由熔断计数；三次后 RouteRecovery 有值。
func TestApplyErrorPolicyRecordsRouteFailures(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		kind upstream.ErrKind
		body string
	}{
		{"model_blocked", upstream.ErrModelBlocked, `{"code":11102,"msg":"service info not found"}`},
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
