package outbound

import (
	"context"
	"errors"
	"fmt"
	"math"
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
	// probing 该代理正被一次半开探测占用（受 Pool.mu 保护）。
	//
	// 全冷却时的探测必须 single-flight：并发突发里 N 个请求同时读到「全部冷却」
	// 会一起打同一个死代理，N 次失败把 failures 从 1 顶到 N，冷却按指数立刻封顶
	// 到 10 分钟——一次抖动被放大成整段出口不可用。
	probing bool
	// 健康快照（issue #39）：面板只读展示用，与冷却判定同源更新。
	lastOK     time.Time
	lastErr    string // 已脱敏，≤200 字符
	lastFailAt time.Time
	// lastPenaltyAt 最近一次真正计入退避的代理失败时刻（recordFailure 写）。
	//
	// 时间衰减必须只看它：recordTargetError（CONNECT 非 200，单域故障）不改
	// failures，却会刷新 lastFailAt——若衰减也用 lastFailAt，频繁的目标侧错误就把
	// 「连续故障」窗口无限续期，一个每天偶发失败一次的健康代理会被顶到满额冷却
	// 并自我维持。lastFailAt 保留「最近一次错误（含目标侧）」的展示语义。
	lastPenaltyAt time.Time
}

// Health 单个代理的健康快照（只读展示用）。
type Health struct {
	Index       int    `json:"index"`
	Proxy       string `json:"proxy"` // Masked()
	Failures    int    `json:"failures"`
	CooldownSec int    `json:"cooldown_sec"` // 剩余冷却秒数，0 = 健康
	LastOK      string `json:"last_ok,omitempty"`
	LastError   string `json:"last_error,omitempty"`
	LastFailAt  string `json:"last_fail_at,omitempty"`
}

// healthErrLimit lastErr 的截断长度：足够定位原因，又不会把长 URL/堆栈塞进快照。
const healthErrLimit = 200

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

// Health 返回池内每个代理的健康快照（issue #39）：面板只读展示 + 出口探测共用。
// 时间一律 RFC3339 UTC；CooldownSec 用 pool.now() 计算剩余，0 = 当前健康。
func (pool *Pool) Health() []Health {
	pool.mu.Lock()
	defer pool.mu.Unlock()

	now := pool.now()
	out := make([]Health, 0, len(pool.states))

	for index, entry := range pool.states {
		health := Health{Index: index, Proxy: entry.proxy.Masked(), Failures: entry.failures}
		if remain := entry.cooldownTill.Sub(now); remain > 0 {
			health.CooldownSec = int(math.Ceil(remain.Seconds()))
		}

		if !entry.lastOK.IsZero() {
			health.LastOK = entry.lastOK.UTC().Format(time.RFC3339)
		}

		health.LastError = entry.lastErr
		if !entry.lastFailAt.IsZero() {
			health.LastFailAt = entry.lastFailAt.UTC().Format(time.RFC3339)
		}

		out = append(out, health)
	}

	return out
}

// Len 返回池内代理数（0 = 直连）。
func (pool *Pool) Len() int { return len(pool.states) }

// Masked 返回池内首个代理的脱敏地址（面板槽视图用）；空池返回空串。
func (pool *Pool) Masked() string {
	if len(pool.states) == 0 {
		return ""
	}

	return pool.states[0].proxy.Masked()
}

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

	lastErr := errNoProxyAvailable

	attempted := false

	n := len(pool.states)
	for i := range n {
		proxyState3 := pool.states[(start+i)%n]
		if pool.inCooldown(proxyState3, now) {
			continue
		}

		attempted = true

		conn, err := pool.dialProxy(ctx, proxyState3, now, network, addr)
		if err == nil {
			return conn, nil
		}

		lastErr = err

		if ctx.Err() != nil {
			break // 调用方已取消：不再轮换其余代理
		}
	}

	if !attempted {
		// 全部冷却：半开探测冷却最早者，避免整段冷却期完全不可用。
		conn, err := pool.dialHalfOpen(ctx, now, network, addr)
		if err == nil {
			return conn, nil
		}

		lastErr = err
	}

	return nil, fmt.Errorf("%w: %w", ErrProxyDial, lastErr)
}

// dialHalfOpen 抢占探测名额后拨号（全部代理都在冷却时的兜底路径）。
// 抢不到名额（已有探针在飞）返回 errNoProxyAvailable：调用方据此快速失败并轮转账号，
// 不排队等一个可能很慢的探测。
func (pool *Pool) dialHalfOpen(ctx context.Context, now time.Time, network, addr string) (net.Conn, error) {
	probe := pool.tryHalfOpen()
	if probe == nil {
		return nil, errNoProxyAvailable
	}
	// 名额必须 defer 释放：dialProxy 里跑的是代理握手（读 CONNECT 应答 / SOCKS5
	// 协商），panic 时显式 endProbe 不会执行，probing 永久为 true——此后全冷却的
	// 池再也抢不到探测名额，出口整段不可用，只能重启进程。
	defer pool.endProbe(probe)

	return pool.dialProxy(ctx, probe, now, network, addr)
}

// dialProxy 对单个代理拨号并据此更新健康状态。
//
// 调用方取消/超时导致的失败不算代理不健康：客户端断开或请求超时会让握手读
// 被 watcher 打断，错误形态是“连接已关闭”而非 context.Canceled，故以
// ctx.Err() 判定，不记失败冷却。
func (pool *Pool) dialProxy(
	ctx context.Context, proxyState2 *proxyState, now time.Time, network, addr string,
) (net.Conn, error) {
	conn, err := proxyState2.proxy.dial(ctx, network, addr)
	if err != nil {
		pool.noteDialFailure(ctx, proxyState2, now, err)

		return nil, err
	}

	pool.recordSuccess(proxyState2, now)

	return conn, nil
}

// noteDialFailure 按失败来源更新健康记账：调用方取消/超时不算代理不健康。
//
// 目标侧拒绝（CONNECT 非 200）说明代理本身可达，只是到不了这个上游域：记错误文本
// 但不计失败。否则单域故障会冷却整条出口、连累所有账号。
func (pool *Pool) noteDialFailure(ctx context.Context, state *proxyState, now time.Time, err error) {
	if ctx.Err() != nil {
		return
	}

	reason := state.proxy.redact(err.Error(), healthErrLimit)
	if errors.Is(err, errDialHTTPConnectFailed) {
		pool.recordTargetError(state, now, reason)

		return
	}

	pool.recordFailure(state, now, reason)
}

// inCooldown 报告代理是否处于有效冷却期。内部加锁，可在未持 pool.mu 时调用
// （DialContext 拨号前已释放锁，多路并发拨号会并发读写 cooldownTill）。
func (pool *Pool) inCooldown(st *proxyState, now time.Time) bool {
	pool.mu.Lock()
	defer pool.mu.Unlock()

	return now.Before(st.cooldownTill)
}

// tryHalfOpen 抢占冷却最早代理的探测名额；已有探针在飞时返回 nil。
func (pool *Pool) tryHalfOpen() *proxyState {
	pool.mu.Lock()
	defer pool.mu.Unlock()

	if len(pool.states) == 0 {
		return nil
	}

	best := pool.states[0]
	for _, st := range pool.states[1:] {
		if st.cooldownTill.Before(best.cooldownTill) {
			best = st
		}
	}

	if best.probing {
		return nil
	}

	best.probing = true

	return best
}

// endProbe 释放半开探测名额（无论探测成败都要释放）。
func (pool *Pool) endProbe(st *proxyState) {
	pool.mu.Lock()
	st.probing = false
	pool.mu.Unlock()
}

// recordFailure 记一次失败：failures++ 并按指数退避设置冷却截止。
func (pool *Pool) recordFailure(proxyState2 *proxyState, now time.Time, reason string) {
	pool.mu.Lock()
	defer pool.mu.Unlock()

	// 时间衰减：距上次**真失败**已超过最长冷却，说明不是连续故障（成功路径拿不到
	// 清零机会——它一直在冷却里），重新起算。否则「每天偶发失败一次」的健康代理
	// 会累积到满额 10 分钟冷却并自我维持。
	if !proxyState2.lastPenaltyAt.IsZero() && now.Sub(proxyState2.lastPenaltyAt) > proxyCooldownMax {
		proxyState2.failures = 0
	}

	proxyState2.failures++

	duration := proxyCooldownBase
	for i := 1; i < proxyState2.failures && duration < proxyCooldownMax; i++ {
		duration *= 2
	}

	if duration > proxyCooldownMax {
		duration = proxyCooldownMax
	}

	proxyState2.cooldownTill = now.Add(duration)
	proxyState2.lastFailAt = now
	proxyState2.lastPenaltyAt = now
	proxyState2.lastErr = reason
}

// recordTargetError 记录目标侧失败：只更新展示用的错误快照，不动失败计数与冷却。
func (pool *Pool) recordTargetError(proxyState2 *proxyState, now time.Time, reason string) {
	pool.mu.Lock()
	defer pool.mu.Unlock()

	proxyState2.lastErr = reason
	proxyState2.lastFailAt = now
}

// recordSuccess 记一次成功：清零失败与冷却。
func (pool *Pool) recordSuccess(entry *proxyState, now time.Time) {
	pool.mu.Lock()
	defer pool.mu.Unlock()

	entry.failures = 0
	entry.cooldownTill = time.Time{}
	entry.lastOK = now
	entry.lastErr = ""
}
