package panel

import (
	"testing"
)

// TestTaskAccountLockSameAccountExclusive 同一账号的任务锁互斥：第二次 tryLock 必须失败，
// 解锁后可再次获取。这是「重复点一键完成不并发重跑」的核心保障。
func TestTaskAccountLockSameAccountExclusive(t *testing.T) {
	t.Parallel()

	panel := &Panel{}
	uid := "u1"

	if !panel.tryLockAccount(uid) {
		t.Fatal("首次加锁应成功")
	}

	if panel.tryLockAccount(uid) {
		t.Fatal("同账号第二次加锁应失败（互斥）")
	}

	panel.unlockAccount(uid)

	if !panel.tryLockAccount(uid) {
		t.Fatal("解锁后应可再次加锁")
	}

	panel.unlockAccount(uid)
}

// TestTaskAccountLockDifferentAccountsIndependent 不同账号的锁互不影响（并行照旧）。
func TestTaskAccountLockDifferentAccountsIndependent(t *testing.T) {
	t.Parallel()

	panel := &Panel{}
	if !panel.tryLockAccount("u1") {
		t.Fatal("u1 加锁应成功")
	}

	if !panel.tryLockAccount("u2") {
		t.Fatal("u2 加锁应成功（不同账号不互斥）")
	}

	panel.unlockAccount("u2")
	panel.unlockAccount("u1")
}

// TestTaskAccountLockCrossEntryShared 单任务 auto 与全量 auto_all 共用同一把账号锁
// （在 handler 层都走 tryLockAccount，这里验证锁命名空间一致）。
func TestTaskAccountLockCrossEntryShared(t *testing.T) {
	t.Parallel()

	panel := &Panel{}
	if !panel.tryLockAccount("u1") {
		t.Fatal("u1 加锁应成功")
	}
	// 模拟全量入口对同一 uid 加锁——必须被挡（否则两入口可并发）。
	if panel.tryLockAccount("u1") {
		t.Fatal("同 uid 跨入口加锁应失败（共用锁）")
	}

	panel.unlockAccount("u1")
}

// TestAutoActionValorantRegistered WorkBuddy_Valorant 进入动作表，复用
// Expert_lighthouse 的真实召唤+使用链（issue #257）。
func TestAutoActionValorantRegistered(t *testing.T) {
	t.Parallel()

	act := autoActionFor("WorkBuddy_Valorant")
	if act == nil {
		t.Fatal("WorkBuddy_Valorant must be registered")
	}

	if act.Attempt {
		t.Fatal("Valorant 已接入已验证的专家事件链，不应再标记为 attempt-only")
	}

	if act.run == nil {
		t.Fatal("Valorant action run must be set")
	}
}
