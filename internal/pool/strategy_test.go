package pool

import (
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/logfmt"
)

// strategyPool 造一个含三号、无冷却的池。
func strategyPool(t *testing.T) *Pool {
	t.Helper()
	withNoPickGap(t)

	pool := New("")
	for _, uid := range []string{"u1", "u2", "u3"} {
		pool.Add(&auth.Auth{UID: uid, AccessToken: "at-" + uid})
		pool.SetCredits(uid, 1000, 0)
	}

	return pool
}

// TestParseStrategy 策略归一（含空值默认与别名写法）。
func TestParseStrategy(t *testing.T) {
	t.Parallel()

	cases := map[string]Strategy{
		"":                      StrategyWeightedRandom,
		"weighted_random":       StrategyWeightedRandom,
		"ROUND_ROBIN":           StrategyRoundRobin,
		" round_robin ":         StrategyRoundRobin,
		"sequential_exhaust":    StrategySequentialExhaust,
		"Sequential_Exhaust":    StrategySequentialExhaust,
		"weighted_random_extra": "", // 非法（返回错误时策略为空）
	}

	for raw, want := range cases {
		got, err := ParseStrategy(raw)
		if want == "" {
			if err == nil {
				t.Errorf("ParseStrategy(%q) 应报错", raw)
			}

			continue
		}

		if err != nil || got != want {
			t.Errorf("ParseStrategy(%q)=%v,%v want %v", raw, got, err, want)
		}
	}
}

// TestRoundRobinAdvancesCursor 严格轮转：连续选号依次落到配置顺序上，并回绕。
func TestRoundRobinAdvancesCursor(t *testing.T) {
	t.Parallel()

	pool := strategyPool(t)
	pool.SetScheduling(Scheduling{
		Strategy: map[string]Strategy{"cn": StrategyRoundRobin},
		Order:    map[string][]string{"cn": {"u2", "u3", "u1"}},
	})

	want := []string{"u2", "u3", "u1", "u2"}

	for i, wantUID := range want {
		acct := pool.PickExcludingForRealm(nil, "m1", "cn")
		if acct == nil {
			t.Fatalf("第 %d 次选号返回 nil", i)
		}

		if acct.UID != wantUID {
			t.Fatalf("第 %d 次选号=%s want %s（严格轮转）", i, acct.UID, wantUID)
		}
	}
}

// TestOrderedPickTraceUsesShortUID 有序策略的决策链与加权随机同口径：FinalUID 记
// uid8，且带上生效策略（完整 uid 只留在账号池/凭证侧）。
func TestOrderedPickTraceUsesShortUID(t *testing.T) {
	t.Parallel()

	pool := strategyPool(t)
	pool.SetScheduling(Scheduling{
		Strategy: map[string]Strategy{"cn": StrategyRoundRobin},
		Order:    map[string][]string{"cn": {"u1", "u2"}},
	})

	acct, trace := pool.PickExcludingForRealmTrace(nil, "m1", "cn")
	if acct == nil {
		t.Fatal("pick returned nil")
	}

	if trace.FinalUID != logfmt.UID8(acct.UID) {
		t.Errorf("FinalUID=%q want %q（uid8）", trace.FinalUID, logfmt.UID8(acct.UID))
	}

	if trace.Strategy != string(StrategyRoundRobin) {
		t.Errorf("Strategy=%q want %q", trace.Strategy, StrategyRoundRobin)
	}
}

// TestSequentialExhaustHoldsUntilUnavailable 顺序耗尽：游标钉在当前号，只有它不可用时才顺延。
func TestSequentialExhaustHoldsUntilUnavailable(t *testing.T) {
	t.Parallel()

	pool := strategyPool(t)
	pool.SetScheduling(Scheduling{
		Strategy: map[string]Strategy{"cn": StrategySequentialExhaust},
		Order:    map[string][]string{"cn": {"u1", "u2", "u3"}},
	})

	for range 3 {
		acct := pool.PickExcludingForRealm(nil, "m1", "cn")
		if acct == nil || acct.UID != "u1" {
			t.Fatalf("顺序耗尽应一直用 u1，got %+v", acct)
		}
	}

	// u1 进入长时间冷却 → 顺延到 u2。
	pool.CooldownSoftRate("u1", time.Hour, time.Time{}, "429 rate limit")

	acct := pool.PickExcludingForRealm(nil, "m1", "cn")
	if acct == nil || acct.UID != "u2" {
		t.Fatalf("u1 冷却后应顺延到 u2，got %+v", acct)
	}
}

// TestOrderedStrategySkipsFullCooldownFallback 有序策略不兜底：全部不可用就返回 nil。
func TestOrderedStrategySkipsFullCooldownFallback(t *testing.T) {
	t.Parallel()

	pool := strategyPool(t)
	pool.SetScheduling(Scheduling{Strategy: map[string]Strategy{"cn": StrategySequentialExhaust}})

	for _, uid := range []string{"u1", "u2", "u3"} {
		pool.CooldownSoftRate(uid, time.Hour, time.Time{}, "429 rate limit")
	}

	if acct := pool.PickExcludingForRealm(nil, "m1", "cn"); acct != nil {
		t.Fatalf("有序策略不应兜底选冷却号，got %s", acct.UID)
	}

	// 对照：加权随机策略下同样场景会兜底选中最早到期的冷却号。
	weighted := strategyPool(t)
	for _, uid := range []string{"u1", "u2", "u3"} {
		weighted.CooldownSoftRate(uid, time.Hour, time.Time{}, "429 rate limit")
	}

	if acct := weighted.PickExcludingForRealm(nil, "m1", "cn"); acct == nil {
		t.Fatal("加权随机策略应保持全冷却兜底（零回归）")
	}
}

// TestStrategySwitchClearsCursors 切策略清空该域游标（旧位置对新策略无意义）。
func TestStrategySwitchClearsCursors(t *testing.T) {
	t.Parallel()

	pool := strategyPool(t)
	pool.SetScheduling(Scheduling{Strategy: map[string]Strategy{"cn": StrategyRoundRobin}})

	pool.PickExcludingForRealm(nil, "m1", "cn")

	pool.mu.RLock()
	before := len(pool.schedulingCursors)
	pool.mu.RUnlock()

	if before == 0 {
		t.Fatal("轮转后应有游标")
	}

	// 模拟「游标已落盘」：清掉脏标记，验证切策略清游标本身会重新置脏。
	pool.dirty.Store(false)
	pool.SetScheduling(Scheduling{Strategy: map[string]Strategy{"cn": StrategySequentialExhaust}})

	pool.mu.RLock()
	after := len(pool.schedulingCursors)
	pool.mu.RUnlock()

	if after != 0 {
		t.Fatalf("切策略后游标应清空，got %d", after)
	}

	// 清游标必须置脏，否则旧游标会留在 state.json 里被重启恢复。
	if !pool.dirty.Load() {
		t.Error("切策略清空游标后必须置脏（否则旧游标会被持久化恢复）")
	}
}

// TestOrderedStrategyRespectsCostTier 有序策略仍需遵守成本分层：已实测免费的号
// 优先于已实测收费的号（与加权随机同口径），账号顺序只在同层内生效。
func TestOrderedStrategyRespectsCostTier(t *testing.T) {
	t.Parallel()

	pool := strategyPool(t)
	pool.SetScheduling(Scheduling{
		Strategy: map[string]Strategy{"cn": StrategyRoundRobin},
		Order:    map[string][]string{"cn": {"u1", "u2", "u3"}},
	})

	// 关闭条件探索：本节只验证成本层过滤，探索会把生效层临时切到 tier 1。
	pool.SetCostExploreInterval(0)

	// u1 已实测收费（tier 2），u2 已实测免费（tier 0）：即使 u1 排在顺序最前，
	// 也只能在免费层里选 u2。
	pool.NoteModelCost("u1", "m1", 10, 1000)
	pool.NoteModelCost("u2", "m1", 0, 1000)

	for i := range 3 {
		acct := pool.PickExcludingForRealm(nil, "m1", "cn")
		if acct == nil || acct.UID != "u2" {
			t.Fatalf("第 %d 次选号=%+v want u2（免费层优先）", i, acct)
		}
	}
}

// TestSchedulingCursorsPersist 游标随 state.json 落盘并恢复。
func TestSchedulingCursorsPersist(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := dir + "/state.json"

	pool := newTestPool(t, path)
	for _, uid := range []string{"u1", "u2", "u3"} {
		pool.Add(&auth.Auth{UID: uid, AccessToken: "at"})
		pool.SetCredits(uid, 100, 0)
	}

	pool.SetScheduling(Scheduling{
		Strategy: map[string]Strategy{"cn": StrategyRoundRobin},
		Order:    map[string][]string{"cn": {"u3", "u1", "u2"}},
	})

	pool.PickExcludingForRealm(nil, "m1", "cn")
	pool.Flush()

	restored := New(path)
	t.Cleanup(restored.Close)
	restored.SetScheduling(Scheduling{
		Strategy: map[string]Strategy{"cn": StrategyRoundRobin},
		Order:    map[string][]string{"cn": {"u3", "u1", "u2"}},
	})

	// 恢复后应接着 u3 的下一位（u1）。
	acct := restored.PickExcludingForRealm(nil, "m1", "cn")
	if acct == nil || acct.UID != "u1" {
		t.Fatalf("游标未恢复：got %+v want u1", acct)
	}
}
