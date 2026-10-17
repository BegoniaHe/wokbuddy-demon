package httpauth

import (
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
