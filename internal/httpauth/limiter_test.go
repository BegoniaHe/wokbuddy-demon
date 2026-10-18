package httpauth

import (
	"fmt"
	"testing"
	"time"
)

func TestLimiterWindowAndReset(t *testing.T) {
	t.Parallel()

	limiter := NewLimiter(2, time.Minute)
	now := time.Unix(1000, 0)
	limiter.now = func() time.Time { return now }
	ip := "192.0.2.1"

	if ok, _ := limiter.Allow(ip); !ok {
		t.Fatal("初始应放行")
	}

	limiter.Fail(ip)
	limiter.Fail(ip)

	if ok, retry := limiter.Allow(ip); ok || retry <= 0 || retry > time.Minute {
		t.Fatalf("达阈值应拒绝，retry=%v", retry)
	}

	limiter.Reset(ip)

	if ok, _ := limiter.Allow(ip); !ok {
		t.Fatal("成功鉴权后应清零")
	}

	limiter.Fail(ip)
	limiter.Fail(ip)

	now = now.Add(time.Minute)

	if ok, _ := limiter.Allow(ip); !ok {
		t.Fatal("窗口过后应恢复")
	}
}

func TestNilLimiter(t *testing.T) {
	t.Parallel()

	var limiter *Limiter
	if ok, _ := limiter.Allow("x"); !ok {
		t.Fatal("nil limiter 应放行")
	}

	limiter.Fail("x")
	limiter.Reset("x")
}

// TestLimiterBoundsTrackedIPs 轮换来源 IP 时失败窗口表不会无界增长。
func TestLimiterBoundsTrackedIPs(t *testing.T) {
	t.Parallel()

	limiter := NewLimiter(2, time.Hour)
	now := time.Unix(1000, 0)

	limiter.now = func() time.Time { return now }
	for i := range maxTrackedIPsHard + 100 {
		limiter.Fail(fmt.Sprintf("198.51.100.%d", i))
	}

	if got := len(limiter.byIP); got > maxTrackedIPsHard {
		t.Fatalf("byIP=%d，超过硬上限 %d", got, maxTrackedIPsHard)
	}
}

// TestLimiterFloodKeepsActiveLockout 灌入大量仍在窗口内的来源 IP 时，目标 IP
// 的锁定不能被顺带淘汰——否则攻击者轮换 IPv6 就能抹掉自己的失败计数。
func TestLimiterFloodKeepsActiveLockout(t *testing.T) {
	t.Parallel()

	limiter := NewLimiter(2, time.Minute)
	now := time.Unix(1000, 0)

	limiter.now = func() time.Time { return now }

	const victim = "203.0.113.7"
	limiter.Fail(victim)
	limiter.Fail(victim)

	for i := range maxTrackedIPs + 100 {
		limiter.Fail(fmt.Sprintf("198.51.100.%d", i))
	}

	if ok, _ := limiter.Allow(victim); ok {
		t.Fatal("灌表后目标 IP 的锁定被抹掉了")
	}
}
