// limiter.go 鉴权失败按来源 IP 限速（issue #61）：固定窗口内失败次数达阈值后
// 直接返回 429，成功鉴权清零。只用于面板与 /status、/metrics 等运维面；
// /healthz 无鉴权不经过限速。单实例进程内状态，不跨进程共享。
package httpauth

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultFailLimit / DefaultFailWindow 默认阈值：10 次/分钟。
	DefaultFailLimit  = 10
	DefaultFailWindow = time.Minute
)

// Limiter 按来源 IP 的失败窗口计数器。
type Limiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	now    func() time.Time
	byIP   map[string]*failWindow
}

type failWindow struct {
	start time.Time
	count int
}

// NewLimiter 构造；limit<=0 / window<=0 使用默认值。
func NewLimiter(limit int, window time.Duration) *Limiter {
	if limit <= 0 {
		limit = DefaultFailLimit
	}
	if window <= 0 {
		window = DefaultFailWindow
	}
	return &Limiter{limit: limit, window: window, now: time.Now, byIP: map[string]*failWindow{}}
}

// Allow 报告该 IP 当前是否仍可尝试鉴权；false 时返回建议等待时长。
func (l *Limiter) Allow(ip string) (bool, time.Duration) {
	if l == nil {
		return true, 0
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	w := l.byIP[ip]
	if w == nil || now.Sub(w.start) >= l.window {
		return true, 0
	}
	if w.count < l.limit {
		return true, 0
	}
	return false, w.start.Add(l.window).Sub(now)
}

// Fail 记一次鉴权失败（窗口从第一次失败起算）。
func (l *Limiter) Fail(ip string) {
	if l == nil {
		return
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	w := l.byIP[ip]
	if w == nil || now.Sub(w.start) >= l.window {
		l.byIP[ip] = &failWindow{start: now, count: 1}
		return
	}
	w.count++
}

// Reset 成功鉴权后清零该 IP 的失败窗口。
func (l *Limiter) Reset(ip string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	delete(l.byIP, ip)
	l.mu.Unlock()
}

// RemoteIP 提取来源 IP：只认 RemoteAddr，不信任未配置的转发头（X-Forwarded-For
// 可被客户端伪造，用于爆破限速会绕过）。反代场景由反代自身限速或后续信任配置处理。
func RemoteIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	addr := strings.TrimSpace(r.RemoteAddr)
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}
