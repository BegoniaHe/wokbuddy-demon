// proxy_test.go 钉住出站代理层失败与账号健康隔离（issue #15）：代理拨号失败只终止
// 轮转、不喂账号连败降权。
package server

import (
	"net"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/outbound"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

func TestChatProxyDialFailureDoesNotPenalizeAccount(t *testing.T) {
	// 死代理地址：监听后立即关闭，拨号必失败。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	ln.Close()

	pool, err := outbound.New([]string{"http://" + dead})
	if err != nil {
		t.Fatal(err)
	}
	up := upstream.New()
	up.SetDialer(pool.DialContext)

	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := postChat(t, h, `{"model":"glm-5.2","messages":[]}`)
	if rec.Code == 200 {
		t.Fatalf("proxy failure should not succeed, code=%d body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("u1")
	if st.ConsecutiveFails != 0 {
		t.Errorf("proxy failure must not feed consecutive fails (degrade), got %d", st.ConsecutiveFails)
	}
	if st.BreakerFails != 0 {
		t.Errorf("proxy failure must not feed breaker, got %d", st.BreakerFails)
	}
}
