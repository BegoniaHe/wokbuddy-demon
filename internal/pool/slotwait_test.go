package pool

import (
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// slotWaitPool 单账号 + 在途上限 1 的池，并按需占满唯一名额。
func slotWaitPool(t *testing.T, full bool) *Pool {
	t.Helper()

	state := newTestPool(t, "")
	state.Add(&auth.Auth{UID: "u1"})
	state.SetMaxInFlight(1)

	if full && !state.Acquire("u1") {
		t.Fatal("Acquire(u1) 失败，无法构造「名额占满」")
	}

	return state
}

// TestSlotWaitForRealmFull 本域健康号名额占满 → 值得等一次（issue #403）。
func TestSlotWaitForRealmFull(t *testing.T) {
	t.Parallel()

	if !slotWaitPool(t, true).SlotWaitForRealm("glm-5.2", "cn") {
		t.Fatal("本域唯一健康号名额占满，应当值得等一次")
	}
}

// TestSlotWaitForRealmSlotFree 名额没占满：选号本就能成功，不该进等待。
func TestSlotWaitForRealmSlotFree(t *testing.T) {
	t.Parallel()

	if slotWaitPool(t, false).SlotWaitForRealm("glm-5.2", "cn") {
		t.Fatal("名额没占满，不该进等待")
	}
}

// TestSlotWaitForRealmInFlightDisabled 在途上限关闭时不存在「名额满」，不该进等待。
func TestSlotWaitForRealmInFlightDisabled(t *testing.T) {
	t.Parallel()

	state := newTestPool(t, "")
	state.Add(&auth.Auth{UID: "u1"})
	state.SetMaxInFlight(0)

	if state.SlotWaitForRealm("glm-5.2", "cn") {
		t.Fatal("不限在途时不存在「名额满」，不该进等待")
	}
}

// TestSlotWaitForRealmAccountCooling 账号级冷却中的号等多久都不会接活。
func TestSlotWaitForRealmAccountCooling(t *testing.T) {
	t.Parallel()

	state := slotWaitPool(t, true)
	state.Cooldown("u1", CoolSoft, time.Minute, "429")

	if state.SlotWaitForRealm("glm-5.2", "cn") {
		t.Fatal("冷却中的号不该进等待判据")
	}
}

// TestSlotWaitForRealmModelCooling 模型级冷却只停那一个模型：同模型等不到，换模型仍值得等。
func TestSlotWaitForRealmModelCooling(t *testing.T) {
	t.Parallel()

	state := slotWaitPool(t, true)
	state.CooldownModelBusy("u1", "glm-5.2", "14003 model busy")

	if state.SlotWaitForRealm("glm-5.2", "cn") {
		t.Fatal("该模型在本域被限流，等名额也不会变好")
	}

	if !state.SlotWaitForRealm("glm-5.3", "cn") {
		t.Fatal("别的模型不受该模型级冷却影响，名额满时仍应值得等")
	}
}

// TestSlotWaitForRealmDailyLimit 日限到点的号即便名额占满也不会接活。
func TestSlotWaitForRealmDailyLimit(t *testing.T) {
	t.Parallel()

	state := slotWaitPool(t, true)
	state.SetDailyLimits(DailyLimits{Tokens: 100}, DailyLimits{}, DailyLimits{})
	state.SetDailyUsageProvider(func() map[string]DailyUsage {
		return map[string]DailyUsage{"u1": {Tokens: 100}}
	})

	if state.SlotWaitForRealm("glm-5.2", "cn") {
		t.Fatal("日限已到的号不该进等待判据")
	}
}

// TestSlotWaitForRealmRouteCooling 整条路由冷却时选号必然失败，等名额无意义。
func TestSlotWaitForRealmRouteCooling(t *testing.T) {
	t.Parallel()

	state := slotWaitPool(t, true)
	for range routeFailureThreshold {
		state.NoteRouteFailure("cn", "glm-5.2")
	}

	if state.SlotWaitForRealm("glm-5.2", "cn") {
		t.Fatal("路由冷却期间不该让请求等名额")
	}

	if !state.SlotWaitForRealm("glm-5.3", "cn") {
		t.Fatal("其他模型的路由未冷却，名额满时仍应值得等")
	}
}

// TestSlotWaitForRealmOtherRealm 异域账号不参与本域判定。
func TestSlotWaitForRealmOtherRealm(t *testing.T) {
	t.Parallel()

	state := newTestPool(t, "")
	state.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"}) // Domain 决定 realm=global
	state.SetMaxInFlight(1)

	if !state.Acquire("g1") {
		t.Fatal("Acquire(g1) 失败")
	}

	if state.SlotWaitForRealm("glm-5.2", "cn") {
		t.Fatal("cn 域没有账号，不该因为 global 号名额满而进等待")
	}

	if !state.SlotWaitForRealm("glm-5.2", "global") {
		t.Fatal("global 域该号名额占满，应当值得等一次")
	}
}

// TestSlotWaitForRealmDisabledAccount 禁用号即便名额占满也不该让请求等。
func TestSlotWaitForRealmDisabledAccount(t *testing.T) {
	t.Parallel()

	state := slotWaitPool(t, true)
	state.Disable("u1", "banned")

	if state.SlotWaitForRealm("glm-5.2", "cn") {
		t.Fatal("禁用号不该进等待判据")
	}
}
