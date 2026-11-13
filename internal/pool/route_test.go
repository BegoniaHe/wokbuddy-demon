package pool

import (
	"strconv"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

func TestRouteCooldownTripsAndRecovers(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	pool := New("")
	pool.SetClock(func() time.Time { return now })
	pool.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	pool.SetCredits("u1", 1000, 0)

	if a := pool.PickExcludingForRealm(nil, "m1", "cn"); a == nil {
		t.Fatal("precondition: pick should succeed")
	}

	for range routeFailureThreshold {
		pool.NoteRouteFailure("cn", "m1")
	}

	if a := pool.PickExcludingForRealm(nil, "m1", "cn"); a != nil {
		t.Fatalf("route should be cooled, got %s", a.UID)
	}

	if until := pool.RouteRecovery("cn", "m1"); until.IsZero() {
		t.Fatal("RouteRecovery should report cooldown")
	}
	// 其他域/模型不受影响。
	if a := pool.PickExcludingForRealm(nil, "m2", "cn"); a == nil {
		t.Fatal("other model should still be routable")
	}

	now = now.Add(routeCooldownDuration + time.Second)

	if a := pool.PickExcludingForRealm(nil, "m1", "cn"); a == nil {
		t.Fatal("route should recover after cooldown")
	}
}

func TestRouteFailureWindowSlides(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	pool := New("")
	pool.SetClock(func() time.Time { return now })
	pool.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	pool.SetCredits("u1", 1000, 0)

	pool.NoteRouteFailure("cn", "m1")
	pool.NoteRouteFailure("cn", "m1")

	now = now.Add(routeFailureWindow + time.Second)

	pool.NoteRouteFailure("cn", "m1") // 前两次已滑出窗口，不应熔断

	if a := pool.PickExcludingForRealm(nil, "m1", "cn"); a == nil {
		t.Fatal("stale failures must not trip route cooldown")
	}
}

func TestRouteSuccessClearsFailures(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	pool := New("")
	pool.SetClock(func() time.Time { return now })
	pool.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	pool.SetCredits("u1", 1000, 0)

	pool.NoteRouteFailure("cn", "m1")
	pool.NoteRouteFailure("cn", "m1")
	pool.NoteRouteSuccess("cn", "m1")
	pool.NoteRouteFailure("cn", "m1") // 成功已清零，仅 1 次

	if a := pool.PickExcludingForRealm(nil, "m1", "cn"); a == nil {
		t.Fatal("success should reset failure window")
	}
}

// 台账条目上限：键里的模型名直接来自客户端请求体，任意名字都曾能建一条永久
// 条目（过期条目只在同一条路由再次失败/成功时清理），伪造模型名刷请求即可
// 无界增长。超限时必须清扫不在冷却中的条目，而不是继续插入。
func TestRouteLedgerStaysBounded(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	pool := New("")
	pool.SetClock(func() time.Time { return now })

	for i := range routeLedgerMax {
		pool.NoteRouteFailure("cn", "m"+strconv.Itoa(i))
	}

	if got := len(pool.routeCooldowns); got != routeLedgerMax {
		t.Fatalf("台账=%d want %d（前置条件不成立，用例无意义）", got, routeLedgerMax)
	}

	// 再失败一次：越过上限，先清扫未冷却的条目，台账规模必须回落而不是递增。
	now = now.Add(routeFailureWindow + time.Second)

	pool.NoteRouteFailure("cn", "fresh")

	if got := len(pool.routeCooldowns); got >= routeLedgerMax {
		t.Fatalf("台账=%d want < %d（超限未清扫）", got, routeLedgerMax)
	}

	if st := pool.routeCooldowns[routeKey("cn", "fresh")]; st == nil || len(st.failures) != 1 {
		t.Fatalf("清扫后新条目应记录 1 次失败，实际 %+v", st)
	}
}

// TestRouteLedgerDoesNotEvictOnExistingKey 已存在的路由再次失败时不得淘汰其他
// 条目：台账满且全部在冷却中时，无条件压缩会把一条仍在保护中的真实路由丢掉，
// 客户端用伪造模型名反复失败即可冲掉真实路由的短路。
func TestRouteLedgerDoesNotEvictOnExistingKey(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	pool := New("")
	pool.SetClock(func() time.Time { return now })

	// 全部条目都进入冷却：prune 无从清理，只剩「淘汰最早解锁」这一条路。
	for i := range routeLedgerMax {
		model := "m" + strconv.Itoa(i)
		for range routeFailureThreshold {
			pool.NoteRouteFailure("cn", model)
		}
	}

	before := len(pool.routeCooldowns)
	if before != routeLedgerMax {
		t.Fatalf("前置条件不成立：台账=%d want %d", before, routeLedgerMax)
	}

	// 已存在的键再次失败：不新增条目，就不该淘汰任何已有冷却。
	pool.NoteRouteFailure("cn", "m0")

	if after := len(pool.routeCooldowns); after != before {
		t.Fatalf("台账=%d want %d（已存在键触发了淘汰）", after, before)
	}

	if st := pool.routeCooldowns[routeKey("cn", "m1")]; st == nil || st.until.IsZero() {
		t.Fatalf("其他路由的冷却被清掉: %+v", st)
	}
}
