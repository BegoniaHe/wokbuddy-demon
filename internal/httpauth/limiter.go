// limiter.go 鉴权失败按来源 IP 限速（issue #61）：固定窗口内失败次数达阈值后
// 直接返回 429，成功鉴权清零。只用于面板与 /status、/metrics 等运维面；
// /healthz 无鉴权不经过限速。单实例进程内状态，不跨进程共享。
package httpauth

import (
	"sync"
	"time"
)

const (
	// DefaultFailLimit / DefaultFailWindow 默认阈值：10 次/分钟。
	DefaultFailLimit  = 10
	DefaultFailWindow = time.Minute
	// maxTrackedIPs 单进程常规保留的失败窗口数。
	maxTrackedIPs = 4096
	// maxTrackedIPsHard 极端洪水下的内存兜底上限。达到后才允许淘汰仍在窗口内
	// 的条目——正常清理只删过期项，避免攻击者用大量来源 IP 灌满表、顺手抹掉
	// 目标 IP 正在生效的锁定。
	maxTrackedIPsHard = maxTrackedIPs * 4
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
		if w == nil {
			l.evictLocked(now)
		}

		l.byIP[ip] = &failWindow{start: now, count: 1}

		return
	}

	w.count++
}

// evictLocked 在达到容量上限时为新的来源 IP 腾位置：先清过期窗口，仍满则
// 淘汰最早开始的窗口（调用方已持有锁）。只在新增 IP 时触发，重复失败不扫描。
func (l *Limiter) evictLocked(now time.Time) {
	if len(l.byIP) < maxTrackedIPs {
		return
	}

	for ip, w := range l.byIP {
		if now.Sub(w.start) >= l.window {
			delete(l.byIP, ip)
		}
	}

	if len(l.byIP) < maxTrackedIPsHard {
		return
	}

	oldestIP := ""

	var oldest time.Time
	for ip, w := range l.byIP {
		if oldestIP == "" || w.start.Before(oldest) {
			oldestIP, oldest = ip, w.start
		}
	}

	if oldestIP != "" {
		delete(l.byIP, oldestIP)
	}
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
