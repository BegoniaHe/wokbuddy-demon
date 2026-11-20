package pool

import (
	"maps"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// TestExclusionSummary 四类成因各 1 个账号（在途满 / 积分保底 / 冷却 / 已试过），
// 归类与 pick 的排除顺序一致（issue #607）。
func TestExclusionSummary(t *testing.T) {
	t.Parallel()

	pool := New("")
	for _, uid := range []string{"busy", "poor", "cool", "tried"} {
		pool.Add(&auth.Auth{UID: uid, AccessToken: "at", ExpiresAt: 9999999999})
		pool.SetCredits(uid, 1_000_000, 0)
	}

	// 在途满：单号上限 1，先占满 busy。
	pool.SetMaxInFlight(1)

	if !pool.Acquire("busy") {
		t.Fatal("acquire busy failed")
	}

	t.Cleanup(func() { pool.Release("busy") })

	// 积分保底：余额低于保底线且模型已知收费。
	pool.SetCreditFloor(100, nil)
	pool.SetCredits("poor", 30, 0)
	pool.NoteModelCost("poor", "paid-model", 2.9, 1000)

	// 冷却：硬冷却未到期。
	pool.Cooldown("cool", CoolHard, time.Hour, "test")

	summary := pool.ExclusionSummary(map[string]bool{"tried": true}, "paid-model", "cn", false)

	want := map[string]int{"tried": 1, "cooling": 1, "credit_floor": 1, "in_flight_full": 1}
	if !maps.Equal(summary, want) {
		t.Fatalf("summary = %v, want %v", summary, want)
	}

	// 展示顺序与 excludeReason 的判定顺序一致（tried 最前，其后按判定次序）。
	if got, wantStr := NoPickSummary(summary),
		"no_pick{total=4 tried=1 cooling=1 credit_floor=1 in_flight_full=1}"; got != wantStr {
		t.Fatalf("summary string = %q, want %q", got, wantStr)
	}
}

// TestExclusionSummaryFirstReasonWins 先命中者归类：同时冷却且余额不足的账号只算
// cooling——与 excludeReason 里 healthy 先于 credit_floor 的判定次序一致。
func TestExclusionSummaryFirstReasonWins(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "both", AccessToken: "at", ExpiresAt: 9999999999})
	pool.SetCreditFloor(100, nil)
	pool.SetCredits("both", 10, 0)
	pool.NoteModelCost("both", "paid-model", 2.9, 1000)
	pool.Cooldown("both", CoolHard, time.Hour, "test")

	summary := pool.ExclusionSummary(nil, "paid-model", "cn", false)
	if len(summary) != 1 || summary["cooling"] != 1 {
		t.Fatalf("summary = %v, want only cooling", summary)
	}
}

// TestExclusionSummaryEmpty 没有账号可排除时返回空 map：503 hint 保持原样，不加噪声。
func TestExclusionSummaryEmpty(t *testing.T) {
	t.Parallel()

	pool := New("")

	if summary := pool.ExclusionSummary(nil, "m", "cn", false); len(summary) != 0 {
		t.Fatalf("empty pool summary = %v, want empty", summary)
	}

	pool.Add(&auth.Auth{UID: "ok", AccessToken: "at", ExpiresAt: 9999999999})

	if summary := pool.ExclusionSummary(nil, "m", "cn", false); len(summary) != 0 {
		t.Fatalf("healthy pool summary = %v, want empty", summary)
	}
}

// TestNoPickSummaryFormat 空 map 返回空串；未知成因按字典序追加在已知成因之后。
func TestNoPickSummaryFormat(t *testing.T) {
	t.Parallel()

	if got := NoPickSummary(nil); got != "" {
		t.Fatalf("empty summary = %q, want empty", got)
	}

	got := NoPickSummary(map[string]int{"zeta": 2, "alpha": 1, "cooling": 3})
	if want := "no_pick{total=6 cooling=3 alpha=1 zeta=2}"; got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
}
