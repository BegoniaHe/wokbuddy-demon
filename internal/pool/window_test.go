// window_test.go 钉住额度窗口探针台账（issue #492）：到点才出账、失败有界重试、
// 成功即出清、账号级冷却不登记。
package pool

import (
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// windowProbePool 构造带注入时钟与单账号的池。
func windowProbePool(t *testing.T, now *time.Time) *Pool {
	t.Helper()

	testPool := newTestPool(t, "")
	testPool.SetClock(func() time.Time { return *now })
	testPool.Add(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})

	return testPool
}

// TestWindowProbeDueAfterReset 到点前不出账，到点后出一次；成功后不重复。
func TestWindowProbeDueAfterReset(t *testing.T) {
	t.Parallel()

	now := time.Now()
	testPool := windowProbePool(t, &now)
	resetAt := now.Add(30 * time.Minute)

	testPool.CooldownSoftForModel("u1", time.Minute, resetAt, "glm-5.2", "6004 model rate limit")

	if got := testPool.DueWindowProbes(8); len(got) != 0 {
		t.Fatalf("未到重置时刻不应出账: %+v", got)
	}

	now = resetAt.Add(time.Second)

	due := testPool.DueWindowProbes(8)
	if len(due) != 1 {
		t.Fatalf("due=%+v want 1", due)
	}

	if due[0].UID != "u1" || due[0].Model != "glm-5.2" {
		t.Errorf("due[0]=%+v want u1/glm-5.2", due[0])
	}

	if len(testPool.DueWindowProbes(8)) != 0 {
		t.Error("重试间隔内不应重复出账")
	}

	testPool.WindowProbeDone("u1", "glm-5.2", WindowProbePinned)

	if testPool.WindowProbeCount() != 0 {
		t.Errorf("成功后应出清台账, count=%d", testPool.WindowProbeCount())
	}
}

// TestWindowProbeReturnRestoresAttempt 拿不到在途名额时归还尝试：不消耗 attempts
// 上限，也不按「限流退避」等一个间隔，下一轮立刻还能探。
func TestWindowProbeReturnRestoresAttempt(t *testing.T) {
	t.Parallel()

	now := time.Now()
	testPool := windowProbePool(t, &now)
	resetAt := now.Add(time.Minute)

	testPool.CooldownSoftForModel("u1", time.Minute, resetAt, "glm-5.2", "6004 model rate limit")
	now = resetAt.Add(time.Second)

	// 连续两轮「出账后立刻归还」，attempts 上限不得被白烧掉。
	for i := range windowProbeMaxAttempts + 2 {
		if got := testPool.DueWindowProbes(8); len(got) != 1 {
			t.Fatalf("第 %d 轮应出账（归还后不消耗尝试）, got %+v", i+1, got)
		}

		testPool.ReturnWindowProbe("u1", "glm-5.2")
	}

	if got := testPool.WindowProbeCount(); got != 1 {
		t.Fatalf("归还后台账应保留, count=%d", got)
	}

	// 归还清掉了重试间隔：同一时刻再取仍可出账。
	now = now.Add(time.Nanosecond)

	if got := testPool.DueWindowProbes(8); len(got) != 1 {
		t.Fatalf("归还后应立刻可再探（不等退避）, got %+v", got)
	}
}

// TestWindowProbeRetriesBounded 失败按间隔重试，用尽次数后出清。
func TestWindowProbeRetriesBounded(t *testing.T) {
	t.Parallel()

	now := time.Now()
	testPool := windowProbePool(t, &now)
	resetAt := now.Add(time.Minute)

	testPool.CooldownSoftForModel("u1", time.Minute, resetAt, "glm-5.2", "6004 model rate limit")
	now = resetAt.Add(time.Second)

	if got := testPool.DueWindowProbes(8); len(got) != 1 {
		t.Fatalf("第一次应出账, got %+v", got)
	}

	testPool.WindowProbeDone("u1", "glm-5.2", WindowProbeRetry)

	// 重试间隔内不出账。
	if got := testPool.DueWindowProbes(8); len(got) != 0 {
		t.Fatalf("重试间隔内不应出账: %+v", got)
	}

	now = now.Add(windowProbeRetryBackoff + time.Second)

	if got := testPool.DueWindowProbes(8); len(got) != 1 {
		t.Fatalf("重试间隔后应再出账一次, got %+v", got)
	}

	testPool.WindowProbeDone("u1", "glm-5.2", WindowProbeRetry)

	now = now.Add(windowProbeRetryBackoff + time.Second)

	if got := testPool.DueWindowProbes(8); len(got) != 0 {
		t.Fatalf("尝试次数用尽后不应再出账: %+v", got)
	}

	if testPool.WindowProbeCount() != 0 {
		t.Errorf("用尽后应出清台账, count=%d", testPool.WindowProbeCount())
	}
}

// TestWindowProbeIgnoresAccountCooldown 账号级冷却不带模型维度，不登记探针。
func TestWindowProbeIgnoresAccountCooldown(t *testing.T) {
	t.Parallel()

	now := time.Now()
	testPool := windowProbePool(t, &now)

	testPool.CooldownSoftRate("u1", time.Minute, now.Add(time.Minute), "429 rate limit")
	now = now.Add(2 * time.Minute)

	if got := testPool.DueWindowProbes(8); len(got) != 0 {
		t.Fatalf("账号级冷却不应登记探针: %+v", got)
	}
}

// TestWindowProbeSkipsPausedAccount 暂停号已退出选号，真实计费的探针不能照发；
// 跳过期间不消耗尝试次数，恢复后到点仍会探测（issue #532）。
func TestWindowProbeSkipsPausedAccount(t *testing.T) {
	t.Parallel()

	now := time.Now()
	testPool := windowProbePool(t, &now)
	resetAt := now.Add(time.Minute)

	testPool.CooldownSoftForModel("u1", time.Minute, resetAt, "glm-5.2", "6004 model rate limit")

	if !testPool.Pause("u1") {
		t.Fatal("pause u1")
	}

	now = resetAt.Add(time.Second)

	if got := testPool.DueWindowProbes(8); len(got) != 0 {
		t.Fatalf("暂停号不应进入探针队列: %+v", got)
	}

	if !testPool.Resume("u1") {
		t.Fatal("resume u1")
	}

	if got := testPool.DueWindowProbes(8); len(got) != 1 {
		t.Fatalf("恢复后应补上探针（跳过不消耗尝试次数）: %+v", got)
	}
}

// TestWindowProbeSkipsCreditFloorAccount 触及积分保底的号不再发真实计费探针；
// 签到回血越过保底线后恢复探测（issue #532）。
func TestWindowProbeSkipsCreditFloorAccount(t *testing.T) {
	t.Parallel()

	now := time.Now()
	testPool := windowProbePool(t, &now)
	testPool.SetCreditFloor(100, nil)
	testPool.SetCredits("u1", 50, 100)
	testPool.NoteModelCost("u1", "glm-5.2", 1, 1000) // 实测收费 → 保底拦

	resetAt := now.Add(time.Minute)
	testPool.CooldownSoftForModel("u1", time.Minute, resetAt, "glm-5.2", "6004 model rate limit")

	now = resetAt.Add(time.Second)

	if got := testPool.DueWindowProbes(8); len(got) != 0 {
		t.Fatalf("触及积分保底的号不应进入探针队列: %+v", got)
	}

	testPool.SetCredits("u1", 200, 200)

	if got := testPool.DueWindowProbes(8); len(got) != 1 {
		t.Fatalf("回血越过保底后应恢复探测: %+v", got)
	}
}

// TestWindowProbeDoneOutcomes 限流失败保留台账待重试；非限流失败立刻出账，不再重试。
func TestWindowProbeDoneOutcomes(t *testing.T) {
	t.Parallel()

	now := time.Now()
	testPool := windowProbePool(t, &now)
	resetAt := now.Add(time.Minute)

	testPool.CooldownSoftForModel("u1", time.Minute, resetAt, "m-retry", "6004 model rate limit")
	testPool.CooldownSoftForModel("u1", time.Minute, resetAt, "m-give-up", "6004 model rate limit")

	now = resetAt.Add(time.Second)

	if got := testPool.DueWindowProbes(8); len(got) != 2 {
		t.Fatalf("due=%+v want 2", got)
	}

	testPool.WindowProbeDone("u1", "m-retry", WindowProbeRetry)
	testPool.WindowProbeDone("u1", "m-give-up", WindowProbeGiveUp)

	if got := testPool.WindowProbeCount(); got != 1 {
		t.Fatalf("非限流失败应立即出账，只剩限流窗口, count=%d", got)
	}

	now = now.Add(windowProbeRetryBackoff + time.Second)

	due := testPool.DueWindowProbes(8)
	if len(due) != 1 || due[0].Model != "m-retry" {
		t.Fatalf("只有限流失败窗口进入下一轮, due=%+v", due)
	}
}

// TestWindowProbeDropsStaleAndRemoved 过期台账与已删账号的台账都出清。
func TestWindowProbeDropsStaleAndRemoved(t *testing.T) {
	t.Parallel()

	now := time.Now()
	testPool := windowProbePool(t, &now)

	testPool.CooldownSoftForModel("u1", time.Minute, now.Add(time.Minute), "m-stale", "6004")
	testPool.CooldownSoftForModel("u1", time.Minute, now.Add(time.Hour), "m-removed", "6004")

	now = now.Add(windowProbeMaxAge + 2*time.Hour)

	if got := testPool.DueWindowProbes(8); len(got) != 0 {
		t.Fatalf("过期台账不应出账: %+v", got)
	}

	testPool.Add(&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999})
	testPool.CooldownSoftForModel("u2", time.Minute, now.Add(time.Minute), "m2", "6004")
	testPool.Remove("u2")

	if testPool.WindowProbeCount() != 0 {
		t.Errorf("账号移除后台账应一并清理, count=%d", testPool.WindowProbeCount())
	}
}

// TestWindowProbeDropsOnSyncToDir 目录重扫剔除账号时与 Remove 同口径清台账。
// 漏清会让 (uid, 模型) 探针留到 windowProbeMaxAge/尝试耗尽，期间占掉每轮额度。
func TestWindowProbeDropsOnSyncToDir(t *testing.T) {
	t.Parallel()

	now := time.Now()
	testPool := windowProbePool(t, &now)

	testPool.CooldownSoftForModel("u1", time.Minute, now.Add(30*time.Minute), "m1", "6004")

	if testPool.WindowProbeCount() == 0 {
		t.Fatal("前置：应已登记探针")
	}

	testPool.SyncToDir(nil) // 目录里已无该账号

	if testPool.WindowProbeCount() != 0 {
		t.Errorf("SyncToDir 剔除账号后台账应一并清理, count=%d", testPool.WindowProbeCount())
	}
}
