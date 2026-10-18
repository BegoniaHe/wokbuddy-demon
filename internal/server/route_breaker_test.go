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
	cases := []struct {
		name string
		kind upstream.ErrKind
		body string
	}{
		{"model_blocked", upstream.ErrModelBlocked, `{"code":11102,"msg":"service info not found"}`},
		{"model_rate_limit", upstream.ErrSoftRate, `{"code":6004,"msg":"model usage limit exceeded"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := pool.New("")
			p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
			h := NewHandler(Config{Pool: p})
			for range 3 {
				h.applyErrorPolicy("u1", tc.kind, tc.body, "cn", "m1", nil)
			}
			if until := p.RouteRecovery("cn", "m1"); until.IsZero() {
				t.Fatal("route cooldown not recorded")
			}
		})
	}
}
