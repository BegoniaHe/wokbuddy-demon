package pool

import (
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// TestSetCreditsSnapshotDerivesEarliestAndBuckets 快照写入同时落批次与「最早一批」，
// 保证面板快照路径与「检查」同口径（issue #200）。
func TestSetCreditsSnapshotDerivesEarliestAndBuckets(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})

	day1 := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	day2 := day1.Add(48 * time.Hour)

	pool.SetCreditsSnapshot("u1", 1000, 1200, 300, []ExpiryBucket{
		{Date: day2.Format("2006-01-02"), ExpireAt: day2, Remain: 400},
		{Date: day1.Format("2006-01-02"), ExpireAt: day1, Remain: 600},
	})

	status, ok := pool.Status("u1")
	if !ok {
		t.Fatal("status missing")
	}

	if len(status.CreditsExpiryBuckets) != 2 {
		t.Fatalf("buckets=%v want 2", status.CreditsExpiryBuckets)
	}

	if !status.CreditsEarliestExpiry.Equal(day1) || status.CreditsEarliestRemaining != 600 {
		t.Errorf("earliest=%v/%d want %v/600", status.CreditsEarliestExpiry, status.CreditsEarliestRemaining, day1)
	}

	if status.Credits != 1000 || status.CreditsTotal != 1200 || status.CreditsExpiring != 300 {
		t.Errorf("credits=%d/%d/%d want 1000/1200/300", status.Credits, status.CreditsTotal, status.CreditsExpiring)
	}
}

// TestSetCreditsSnapshotClearsStaleBuckets 空批次写入清旧批次（不展示过期数据）。
func TestSetCreditsSnapshotClearsStaleBuckets(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})

	at := time.Now().Add(time.Hour).Truncate(time.Second)
	pool.SetCreditsSnapshot("u1", 100, 100, 0, []ExpiryBucket{{Date: at.Format("2006-01-02"), ExpireAt: at, Remain: 100}})

	first, _ := pool.Status("u1")
	if len(first.CreditsExpiryBuckets) != 1 {
		t.Fatal("首次写入应有 1 条批次")
	}

	pool.SetCreditsSnapshot("u1", 0, 100, 0, nil)

	status, _ := pool.Status("u1")
	if len(status.CreditsExpiryBuckets) != 0 {
		t.Errorf("空批次应清旧批次，got %v", status.CreditsExpiryBuckets)
	}

	if !status.CreditsEarliestExpiry.IsZero() {
		t.Errorf("earliest 应清零，got %v", status.CreditsEarliestExpiry)
	}
}

// TestCreditsExpiryBucketsPersist 批次随 state.json 落盘并可恢复（旧 state 缺失该键
// 视为空，不报错）。
func TestCreditsExpiryBucketsPersist(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	now := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	date := now.Format("2006-01-02")

	pool := newTestPool(t, dir+"/state.json")
	pool.Add(&auth.Auth{UID: "u1"})
	pool.SetCreditsSnapshot("u1", 500, 500, 0, []ExpiryBucket{{Date: date, ExpireAt: now, Remain: 500}})
	pool.Flush()

	// New(stateFp) 会同步 load() 已有 state.json。
	restored := New(dir + "/state.json")
	t.Cleanup(restored.Close)

	restoredStatus, ok := restored.Status("u1")
	if !ok {
		t.Fatal("restored status missing")
	}

	buckets := restoredStatus.CreditsExpiryBuckets
	if len(buckets) != 1 || buckets[0].Date != date || buckets[0].Remain != 500 {
		t.Fatalf("buckets=%v want [%s/500]", buckets, date)
	}
}

// TestBalanceRefreshMeta 周期与最近刷新时刻可读回。
func TestBalanceRefreshMeta(t *testing.T) {
	t.Parallel()

	pool := New("")

	if sec, at := pool.BalanceRefreshMeta(); sec != 0 || !at.IsZero() {
		t.Fatalf("默认应为零值，got %d/%v", sec, at)
	}

	now := time.Now().Truncate(time.Second)
	pool.SetBalanceRefreshMeta(300, now)

	if sec, at := pool.BalanceRefreshMeta(); sec != 300 || !at.Equal(now) {
		t.Fatalf("got %d/%v want 300/%v", sec, at, now)
	}
}

// TestSetCreditsDetailedClearsStaleBuckets 无逐日批次的入口（外部/测试调用方）
// 更新余额时必须清掉旧批次：否则快照会同时展示新 credits/最早批次与上一次周期
// 刷新留下的过期批次（issue #200 快照/检查口径一致性）。
func TestSetCreditsDetailedClearsStaleBuckets(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})

	at := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	pool.SetCreditsSnapshot("u1", 100, 100, 0, []ExpiryBucket{
		{Date: at.Format("2006-01-02"), ExpireAt: at, Remain: 100},
	})

	// 模拟只带最早批次、不带逐日明细的调用方。
	pool.SetCreditsDetailed("u1", 60, 100, 0, at, 60)

	status, ok := pool.Status("u1")
	if !ok {
		t.Fatal("status missing")
	}

	if len(status.CreditsExpiryBuckets) != 0 {
		t.Errorf("无批次路径更新后不应保留旧批次，got %v", status.CreditsExpiryBuckets)
	}

	if !status.CreditsEarliestExpiry.Equal(at) || status.CreditsEarliestRemaining != 60 {
		t.Errorf("最早批次应更新为 %v/60，got %v/%d", at, status.CreditsEarliestExpiry, status.CreditsEarliestRemaining)
	}
}
