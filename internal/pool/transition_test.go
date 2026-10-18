package pool

import (
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// 状态机迁移正交性测试：聚焦 transition.go 收敛出的「单一权威状态机」语义。
// entry 的可选择性由四个正交维度决定（disabled / 冷却域 until+coolKind+softStreak+
// modelCooldowns / 熔断器 fails+retryCount+breakerUntil / sessionDeadFails），本文件
// 锁定迁移原语对这四个维度的边界，特别是旧实现的缺陷点：
//   - Disable 旧实现只置 disabled+reason，不碰 until/modelCooldowns → 「disabled 但
//     cooling」杂交态（疑点 4）。新语义：disableLocked 置 disabled 并清冷却域。
//   - reviveCoolingLocked（签到解冻）只清冷却域、不动熔断器（C5 语义）。
//
// 与 pool_test.go / modelcooldown_test.go / sessiondead_test.go 的差异：那些测试
// 锁定各维度的行为，本文件锁定「跨维度」的迁移边界（冷却↔禁用↔熔断互不越界）。

// 直接读取 entry 的冷却域原始字段（Status 不暴露 coolKind 数值与 modelCooldowns）。
func coolingDomain(t *testing.T, pool *Pool, uid string) (until time.Time, coolKind CoolKind, reason string, softStreak, modelCooldowns int) {
	t.Helper()

	pool.mu.RLock()
	defer pool.mu.RUnlock()

	e, ok := pool.byUID[uid]
	if !ok {
		t.Fatalf("uid %s missing", uid)
	}

	return e.until, e.coolKind, e.reason, e.softStreak, len(e.modelCooldowns)
}

// TestTransitionDisableClearsCoolingDomain 疑点 4 修正：先冷却（软冷却 + 6004 模型级
// 冷却）再 Disable → 冷却域全清（until/coolKind/reason/softStreak/modelCooldowns），
// 只留 disabled 终态。旧实现只置 disabled+reason，会出现「disabled=true 但 cooling=true」
// 与残留 modelCooldowns 的杂交态。
func TestTransitionDisableClearsCoolingDomain(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	pool.Cooldown("u1", CoolSoft, 600*time.Second, "429")                                          // until + coolKind=soft + softStreak=1
	pool.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "6004") // modelCooldowns=1（softStreak 增到 2）

	_, _, reason, _, num := coolingDomain(t, pool, "u1")
	if reason == "" || num != 1 {
		t.Fatalf("precondition: 应先处于软冷却+模型级冷却态 (reason=%q modelCooldowns=%d)", reason, num)
	}

	pool.Disable("u1", "account banned")

	status, _ := pool.Status("u1")

	until, kind, reason, streak, num := coolingDomain(t, pool, "u1")
	if !status.Disabled {
		t.Fatal("Disable 后应 disabled")
	}

	if !until.IsZero() {
		t.Errorf("Disable 后 until=%v 应为零值（冷却域清零）", until)
	}

	if kind != 0 {
		t.Errorf("Disable 后 coolKind=%v 应为零值", kind)
	}

	if reason != "account banned" {
		t.Errorf("Disable 后 reason=%q 应为禁用原因", reason)
	}

	if streak != 0 {
		t.Errorf("Disable 后 softStreak=%d 应为 0（冷却域清零）", streak)
	}

	if num != 0 {
		t.Errorf("Disable 后 modelCooldowns=%d 应为 0（模型豁免随冷却域清零）", num)
	}

	if status.Cooling {
		t.Errorf("Disable 后不应呈现 cooling（禁用是更强终态，绝不杂交）: %+v", status)
	}
}

// TestTransitionDisablePreservesBreaker 疑点 4 修正的另一半：Disable 只清冷却域、
// 不动熔断器。熔断是「连续 5xx 失败」信号（与授权/session 正交），禁用后再复活时
// 熔断观测仍有效，不应被禁用覆盖。
func TestTransitionDisablePreservesBreaker(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	pool.SetBreaker(1, time.Hour, time.Hour)
	pool.NoteError("u1") // 触发熔断（fails=0, retryCount=1, breakerUntil=1h）

	bt, ok := pool.breakerUntil("u1")
	if !ok || bt.IsZero() {
		t.Fatal("precondition: breaker should be open")
	}

	pool.Disable("u1", "session dead")

	if bt, ok := pool.breakerUntil("u1"); !ok || bt.IsZero() {
		t.Fatal("Disable 后 breakerUntil 应保留（熔断与禁用正交）")
	}

	if fails := pool.breakerFails("u1"); fails != 0 {
		t.Errorf("Disable 后 fails=%d（已置 0，不应被 Disable 刻意改动）", fails)
	}
	// disabled 优先：即使熔断仍在，账号也不可选。
	if got := pool.Pick(); got != nil {
		t.Fatalf("disabled 账号不可选（含熔断期），got %+v", got)
	}
}

// TestTransitionSessionDeadDisableClearsCooling session 死亡走 NoteSessionDead 连续
// 计数，达阈值后经 disableLocked：冷却域一并清零，不得留下「disabled 但仍 cooling」
// 的杂交态（此前 handler 走 Disable、阈值路径却残留冷却，同一信号不同处置）。
func TestTransitionSessionDeadDisableClearsCooling(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	pool.Cooldown("u1", CoolSoft, time.Hour, "429 rate limit") // 软冷却中

	until, _, _, _, _ := coolingDomain(t, pool, "u1")
	if until.IsZero() {
		t.Fatal("precondition: 应处于软冷却")
	}

	if pool.NoteSessionDead("u1") || pool.NoteSessionDead("u1") || !pool.NoteSessionDead("u1") {
		t.Fatal("第 3 次 NoteSessionDead 应达阈值禁用")
	}

	status, _ := pool.Status("u1")
	if !status.Disabled {
		t.Fatal("应 disabled")
	}

	if status.Cooling || !status.Until.IsZero() || status.SoftStreak != 0 {
		t.Errorf("session dead 禁用应清冷却域（无杂交态）: Cooling=%v Until=%v SoftStreak=%d",
			status.Cooling, status.Until, status.SoftStreak)
	}

	if status.DisabledReason != sessionDeadReason {
		t.Errorf("disabled_reason=%q want %q", status.DisabledReason, sessionDeadReason)
	}
}

// TestTransitionReviveKeepsSoftCoolingKeepsBreaker reviveCoolingLocked（签到/余额
// 刷新解冻）语义：只解冻余额耗尽冷却（CoolHard），软限流冷却与 6004 模型级台账
// **保留**（限流恢复证据是重置墙钟到期，不是余额恢复）+ 更新 credits，不动熔断。
// 既有单维度测试已各自锁定 reason/softStreak/modelCooldowns，本用例一次性断言完整
// 字段集，锁定迁移原语对冷却域/熔断域的处置永远一致。
func TestTransitionReviveKeepsSoftCoolingKeepsBreaker(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	// 冷却域：软冷却 + 6004 模型级冷却（softStreak 累计）。
	pool.Cooldown("u1", CoolSoft, 600*time.Second, "429")
	pool.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "6004")
	// 熔断域：独立信号，签到不解冻。
	pool.SetBreaker(1, time.Hour, time.Hour)
	pool.NoteError("u1")

	pool.ReenableIfCredits("u1", 700, 0)

	st, _ := pool.Status("u1")
	if st.Credits != 700 {
		t.Errorf("revive 后 credits=%d want 700", st.Credits)
	}

	until, kind, reason, streak, mc := coolingDomain(t, pool, "u1")
	if until.IsZero() || kind == 0 || reason == "" {
		t.Errorf("revive 不得解除软限流冷却：until=%v kind=%v reason=%q", until, kind, reason)
	}

	if mc == 0 {
		t.Error("revive 不得清 6004 模型级台账（余额恢复不构成限流解除证据）")
	}

	_ = streak // 本场景 streak 恒 0（Cooldown 固定时长入口不累计、带 resetAt 的模型级写入也不累计）

	if bt, ok := pool.breakerUntil("u1"); !ok || bt.IsZero() {
		t.Fatal("revive 不得清熔断（chat 通道健康未证明）")
	}
	// 熔断域保留 → 账号不进 normal 候选（仅全冷却兜底仍可能选中，与熔断兜底语义一致）。
	if pool.internalHealthy("u1") {
		t.Fatal("revive 后熔断期内不应 healthy（熔断域未被签到覆盖）")
	}
}

// TestTransitionReviveUnfreezesHardCooling 余额耗尽冷却（CoolHard）才是
// ReenableIfCredits 的解冻对象：余额恢复（remain>0）正是它的权威恢复证据。
func TestTransitionReviveUnfreezesHardCooling(t *testing.T) {
	t.Parallel()

	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownUntilTomorrow4AM("u1", "余额不足")
	p.ReenableIfCredits("u1", 700, 0)

	until, kind, reason, _, _ := coolingDomain(t, p, "u1")
	if !until.IsZero() || kind != 0 || reason != "" {
		t.Errorf("余额恢复应解冻 CoolHard：until=%v kind=%v reason=%q", until, kind, reason)
	}
}

func TestReviveClearsDegradeState(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	pool := New("")
	pool.SetClock(func() time.Time { return now })
	pool.Add(&auth.Auth{UID: "u1"})

	for range defaultDegradeThreshold {
		pool.NoteFailures("u1")
	}

	pool.mu.RLock()
	entry2 := pool.byUID["u1"]
	degraded := !entry2.degradeUntil.IsZero() && now.Before(entry2.degradeUntil)

	pool.mu.RUnlock()

	if !degraded {
		t.Fatal("precondition: 连败达阈后应进入降权期")
	}

	pool.Revive("u1")

	pool.mu.RLock()
	defer pool.mu.RUnlock()

	if entry2.consecutiveFails != 0 || !entry2.degradeUntil.IsZero() || !entry2.probeUntil.IsZero() {
		t.Fatalf("Revive 未清连败降权态: fails=%d degradeUntil=%v probeUntil=%v",
			entry2.consecutiveFails, entry2.degradeUntil, entry2.probeUntil)
	}
}
