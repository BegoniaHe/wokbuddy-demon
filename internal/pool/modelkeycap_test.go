package pool

import (
	"strconv"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// TestModelLedgersBoundedPerAccount 每账号的按模型台账（health / modelCost /
// modelCooldowns）必须有硬上限。
//
// 三类台账的键都是客户端请求体里的模型名（模型门禁默认关闭），而惰性清理在 pick
// 写锁路径上对每账号整表扫描一次：不设上限时伪造模型名即可撑爆内存，并把每次选号
// 变成 O(N)。本用例用**未过期**条目（冷却 1 分钟后到期）确保走到上限淘汰分支，
// 而不是只靠 TTL 清理。
func TestModelLedgersBoundedPerAccount(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

	pool := newTestPool(t, "")
	pool.SetClock(func() time.Time { return now })
	pool.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	pool.SetCredits("u1", 1000, 0)

	for i := range maxModelKeysPerAccount + 50 {
		model := "m" + strconv.Itoa(i)
		pool.RecordModelHealth("u1", model, 100, true, true)
		pool.NoteModelCost("u1", model, 1, 1000)
		// 带上游重置时刻才走模型级分支（写 modelCooldowns）；resetAt 零值会退回
		// 账号级退避，不产生本用例要压测的那张表。
		pool.CooldownSoftForModel("u1", time.Minute, now.Add(time.Hour), model, "6004 model rate limit")
	}

	// pick 路径触发三张表的惰性清理（含上限淘汰）。
	pool.PickExcludingForRealm(nil, "", "cn")

	pool.mu.RLock()
	entry2 := pool.byUID["u1"]
	healthLen, costLen, cooldownLen := len(entry2.health), len(entry2.modelCost), len(entry2.modelCooldowns)

	pool.mu.RUnlock()

	for name, got := range map[string]int{
		"health":         healthLen,
		"modelCost":      costLen,
		"modelCooldowns": cooldownLen,
	} {
		if got == 0 {
			t.Fatalf("%s 台账被清空，应保留到上限内的条目", name)
		}

		if got > maxModelKeysPerAccount {
			t.Fatalf("%s 条目数 %d 超过上限 %d", name, got, maxModelKeysPerAccount)
		}
	}
}

// TestModelLedgerKeepsFreshObservations 上限淘汰必须丢「最早失效」的条目，而不是
// 随机丢：新鲜观测（离失效更远）优先保留，否则刚实测到的价格/健康信号会被自己
// 刷掉，学到的知识永远收敛不了。
func TestModelLedgerKeepsFreshObservations(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	fresh := map[string]modelCostEntry{}

	stale := modelCostEntry{CostPer1k: 1, LastSeen: now.Add(-5 * time.Hour)}
	for i := range maxModelKeysPerAccount + 10 {
		fresh["stale"+strconv.Itoa(i)] = stale
	}
	// 一条刚刚观测到的条目：失效时刻最远，必须活下来。
	fresh["keep"] = modelCostEntry{CostPer1k: 2, LastSeen: now}

	pruneModelLedger(fresh, func(mc modelCostEntry) time.Time {
		if mc.LastSeen.IsZero() {
			return time.Time{}
		}

		return mc.LastSeen.Add(modelCostTTL)
	}, now)

	if len(fresh) != maxModelKeysPerAccount {
		t.Fatalf("台账条目数 %d want %d", len(fresh), maxModelKeysPerAccount)
	}

	if _, ok := fresh["keep"]; !ok {
		t.Fatal("最新观测不应被淘汰")
	}
}

// TestSchedulingCursorsBounded 有序策略的游标台账按 (域, 模型) 记账，模型名来自
// 客户端且随 state.json 落盘，必须有上限。
func TestSchedulingCursorsBounded(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

	pool := newTestPool(t, "")
	pool.SetClock(func() time.Time { return now })
	pool.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	pool.SetCredits("u1", 1000, 0)
	pool.SetScheduling(Scheduling{Strategy: map[string]Strategy{"cn": StrategyRoundRobin}})

	for i := range maxSchedulingCursors + 40 {
		if acct := pool.PickExcludingForRealm(nil, "m"+strconv.Itoa(i), "cn"); acct == nil {
			t.Fatalf("第 %d 次选号失败", i)
		}
	}

	pool.mu.RLock()
	cursors := len(pool.schedulingCursors)
	pool.mu.RUnlock()

	if cursors == 0 {
		t.Fatal("游标台账不应为空")
	}

	if cursors > maxSchedulingCursors {
		t.Fatalf("游标条目数 %d 超过上限 %d", cursors, maxSchedulingCursors)
	}
}

// TestRouteLedgerBoundedWhenAllCooling 全部条目都在冷却时，prune 删不掉任何一条，
// 台账仍会随新模型名增长。必须在淘汰「最早解锁」后硬顶在上限。
func TestRouteLedgerBoundedWhenAllCooling(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

	pool := newTestPool(t, "")
	pool.SetClock(func() time.Time { return now })

	for i := range routeLedgerMax + 20 {
		model := "m" + strconv.Itoa(i)
		for range routeFailureThreshold {
			pool.NoteRouteFailure("cn", model)
		}
	}

	pool.mu.RLock()
	n := len(pool.routeCooldowns)
	pool.mu.RUnlock()

	if n > routeLedgerMax {
		t.Fatalf("路由台账条目数 %d 超过上限 %d", n, routeLedgerMax)
	}
}
