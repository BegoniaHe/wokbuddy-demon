package main

import (
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
)

// TestRealmAwareStickyLowLoadArgumentOrder 粘性低水位闭包必须按
// StickyLowLoad(uid, model, realm) 传参；曾因共享 helper 误排成
// (model, realm, uid) 导致空闲账号也判为不可回纳。
func TestRealmAwareStickyLowLoadArgumentOrder(t *testing.T) {
	t.Parallel()

	state := pool.New("")
	state.SetMaxInFlight(4)
	state.Add(&auth.Auth{
		UID: "u1", AccessToken: "at", RefreshToken: "rt",
		ExpiresAt: 9999999999, Domain: "workbuddy.cn",
	})

	lowLoad := realmAwareStickyLowLoad(state)

	if !lowLoad("cn:glm-5.2", "u1") {
		t.Fatal("空闲 CN 账号应处于粘性低水位（参数顺序 uid/model/realm）")
	}

	if lowLoad("global:glm-5.2", "u1") {
		t.Fatal("realm 不匹配的账号不应通过低水位判定")
	}
}
