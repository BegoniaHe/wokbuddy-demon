package httpauth

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLimiterWindowAndReset(t *testing.T) {
	l := NewLimiter(2, time.Minute)
	now := time.Unix(1000, 0)
	l.now = func() time.Time { return now }
	ip := "192.0.2.1"

	if ok, _ := l.Allow(ip); !ok {
		t.Fatal("初始应放行")
	}
	l.Fail(ip)
	l.Fail(ip)
	if ok, retry := l.Allow(ip); ok || retry <= 0 || retry > time.Minute {
		t.Fatalf("达阈值应拒绝，retry=%v", retry)
	}
	l.Reset(ip)
	if ok, _ := l.Allow(ip); !ok {
		t.Fatal("成功鉴权后应清零")
	}

	l.Fail(ip)
	l.Fail(ip)
	now = now.Add(time.Minute)
	if ok, _ := l.Allow(ip); !ok {
		t.Fatal("窗口过后应恢复")
	}
}

func TestRemoteIPIgnoresForwardedHeaders(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "198.51.100.9:1234"
	req.Header.Set("X-Forwarded-For", "203.0.113.1")
	if got := RemoteIP(req); got != "198.51.100.9" {
		t.Fatalf("RemoteIP=%q", got)
	}
}

func TestNilLimiter(t *testing.T) {
	var l *Limiter
	if ok, _ := l.Allow("x"); !ok {
		t.Fatal("nil limiter 应放行")
	}
	l.Fail("x")
	l.Reset("x")
}

// TestLimiterBoundsTrackedIPs 轮换来源 IP 时失败窗口表不会无界增长。
func TestLimiterBoundsTrackedIPs(t *testing.T) {
	l := NewLimiter(2, time.Hour)
	now := time.Unix(1000, 0)
	l.now = func() time.Time { return now }
	for i := range maxTrackedIPsHard + 100 {
		l.Fail(fmt.Sprintf("198.51.100.%d", i))
	}
	if got := len(l.byIP); got > maxTrackedIPsHard {
		t.Fatalf("byIP=%d，超过硬上限 %d", got, maxTrackedIPsHard)
	}
}

// TestLimiterFloodKeepsActiveLockout 灌入大量仍在窗口内的来源 IP 时，目标 IP
// 的锁定不能被顺带淘汰——否则攻击者轮换 IPv6 就能抹掉自己的失败计数。
func TestLimiterFloodKeepsActiveLockout(t *testing.T) {
	l := NewLimiter(2, time.Minute)
	now := time.Unix(1000, 0)
	l.now = func() time.Time { return now }
	const victim = "203.0.113.7"
	l.Fail(victim)
	l.Fail(victim)

	for i := range maxTrackedIPs + 100 {
		l.Fail(fmt.Sprintf("198.51.100.%d", i))
	}
	if ok, _ := l.Allow(victim); ok {
		t.Fatal("灌表后目标 IP 的锁定被抹掉了")
	}
}
