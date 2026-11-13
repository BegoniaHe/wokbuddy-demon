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

	testPool.WindowProbeDone("u1", "glm-5.2", true)

	if testPool.WindowProbeCount() != 0 {
		t.Errorf("成功后应出清台账, count=%d", testPool.WindowProbeCount())
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

	testPool.WindowProbeDone("u1", "glm-5.2", false)

	// 重试间隔内不出账。
	if got := testPool.DueWindowProbes(8); len(got) != 0 {
		t.Fatalf("重试间隔内不应出账: %+v", got)
	}

	now = now.Add(windowProbeRetryBackoff + time.Second)

	if got := testPool.DueWindowProbes(8); len(got) != 1 {
		t.Fatalf("重试间隔后应再出账一次, got %+v", got)
	}

	testPool.WindowProbeDone("u1", "glm-5.2", false)

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
