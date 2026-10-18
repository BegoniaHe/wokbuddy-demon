package httpauth

import (
	"fmt"
	"net/http"
	"net/http/httptest"
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

func TestRemoteIPIgnoresForwardedHeaders(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "198.51.100.9:1234"
	req.Header.Set("X-Forwarded-For", "203.0.113.1")

	if got := RemoteIP(req); got != "198.51.100.9" {
		t.Fatalf("RemoteIP=%q", got)
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
	for i := range maxTrackedIPs + 100 {
		limiter.Fail(fmt.Sprintf("198.51.100.%d", i))
	}

	if got := len(limiter.byIP); got > maxTrackedIPs {
		t.Fatalf("byIP=%d，超过上限 %d", got, maxTrackedIPs)
	}
}
