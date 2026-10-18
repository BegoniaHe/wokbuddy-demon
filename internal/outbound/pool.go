package outbound

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// 代理失败冷却参数（常量：当前无需运维调参，KISS）。指数退避：base × 2^(failures-1)，
// 封顶 max。成功即清零（代理恢复的最强证据）。
const (
	proxyCooldownBase = 30 * time.Second
	proxyCooldownMax  = 10 * time.Minute
)

var errNoProxyAvailable = errors.New("no proxy available")

// Pool 出站代理池：按健康轮换，失败代理进入指数冷却，成功重置。
// 无代理时退化为直连（与未配置代理的行为一致）。
type Pool struct {
	mu     sync.Mutex
	states []*proxyState
	next   uint64
	now    func() time.Time
}

type proxyState struct {
	proxy        *Proxy
	failures     int
	cooldownTill time.Time
}

// New 由代理 URL 列表构造池；任一 URL 非法即报错。空列表返回直连池。
func New(rawURLs []string) (*Pool, error) {
	states := make([]*proxyState, 0, len(rawURLs))
	for _, raw := range rawURLs {
		p, err := ParseProxy(raw)
		if err != nil {
			return nil, err
		}

		states = append(states, &proxyState{proxy: p})
	}

	return &Pool{states: states, now: time.Now}, nil
}

// SetClock 仅供测试注入确定性时钟。
func (pool *Pool) SetClock(fn func() time.Time) {
	pool.mu.Lock()
	defer pool.mu.Unlock()

	pool.now = fn
}

// Len 返回池内代理数（0 = 直连）。
func (pool *Pool) Len() int { return len(pool.states) }

// DialContext 经健康代理建立到 addr 的连接：从轮转游标起依次尝试健康代理，
// 失败者记冷却并回落下一个；全部冷却时半开探测冷却最早者。无代理则直连。
// 全部失败返回包装 ErrProxyDial 的错误（调用方据此不惩罚账号）。
func (pool *Pool) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if len(pool.states) == 0 {
		conn, err := (&net.Dialer{Timeout: dialTimeout, KeepAlive: dialKeepAlive}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, fmt.Errorf("direct dial: %w", err)
		}

		return conn, nil
	}

	pool.mu.Lock()
	start := int(pool.next % uint64(len(pool.states))) //nolint:gosec // len(states) 远小于 int 上限
	pool.next++
	now := pool.now()
	pool.mu.Unlock()

	var lastErr error

	attempted := false

	n := len(pool.states)
	for i := range n {
		proxyState3 := pool.states[(start+i)%n]
		if pool.inCooldown(proxyState3, now) {
			continue
		}

		attempted = true

		conn, err := proxyState3.proxy.dial(ctx, network, addr)
		if err == nil {
			pool.recordSuccess(proxyState3)
			return conn, nil
		}

		lastErr = err

		pool.recordFailure(proxyState3, now)
	}

	if !attempted {
		// 全部冷却：半开探测冷却最早者，避免整段冷却期完全不可用。
		proxyState2 := pool.earliest()

		conn, err := proxyState2.proxy.dial(ctx, network, addr)
		if err == nil {
			pool.recordSuccess(proxyState2)
			return conn, nil
		}

		lastErr = err

		pool.recordFailure(proxyState2, now)
	}

	if lastErr == nil {
		lastErr = errNoProxyAvailable
	}

	return nil, fmt.Errorf("%w: %w", ErrProxyDial, lastErr)
}

// inCooldown 报告代理是否处于有效冷却期。内部加锁，可在未持 pool.mu 时调用
// （DialContext 拨号前已释放锁，多路并发拨号会并发读写 cooldownTill）。
func (pool *Pool) inCooldown(st *proxyState, now time.Time) bool {
	pool.mu.Lock()
	defer pool.mu.Unlock()

	return now.Before(st.cooldownTill)
}

// earliest 返回冷却截止最早的代理（全部冷却时的半开探测目标）。内部加锁。
func (pool *Pool) earliest() *proxyState {
	pool.mu.Lock()
	defer pool.mu.Unlock()

	best := pool.states[0]
	for _, st := range pool.states[1:] {
		if st.cooldownTill.Before(best.cooldownTill) {
			best = st
		}
	}

	return best
}

// recordFailure 记一次失败：failures++ 并按指数退避设置冷却截止。
func (pool *Pool) recordFailure(proxyState2 *proxyState, now time.Time) {
	pool.mu.Lock()
	defer pool.mu.Unlock()

	proxyState2.failures++

	duration := proxyCooldownBase
	for i := 1; i < proxyState2.failures && duration < proxyCooldownMax; i++ {
		duration *= 2
	}

	if duration > proxyCooldownMax {
		duration = proxyCooldownMax
	}

	proxyState2.cooldownTill = now.Add(duration)
}

// recordSuccess 记一次成功：清零失败与冷却。
func (pool *Pool) recordSuccess(st *proxyState) {
	pool.mu.Lock()
	defer pool.mu.Unlock()

	st.failures = 0
	st.cooldownTill = time.Time{}
}
