// health_test.go 钉住出口池健康记账的三个语义：
//   - 全冷却时的半开探测 single-flight（并发突发不得把失败数放大到 N）；
//   - 目标侧拒绝（CONNECT 非 200）不计代理健康（单域故障不得冷却整条出口）；
//   - 失败计数按时间衰减（偶发失败不得永久累积成满额冷却）。
package outbound

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// newHealthPool 构造只有一个代理的池（URL 用不可达回环地址，测试不实际拨号）。
func newHealthPool(t *testing.T) *Pool {
	t.Helper()

	pool, err := New([]string{"socks5://127.0.0.1:1"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return pool
}

func TestHalfOpenProbeIsSingleFlight(t *testing.T) {
	t.Parallel()

	pool := newHealthPool(t)

	first := pool.tryHalfOpen()
	if first == nil {
		t.Fatal("首个探测应抢占成功")
	}

	if second := pool.tryHalfOpen(); second != nil {
		t.Fatal("探测在飞时第二个请求必须快速失败，不得重复探测")
	}

	pool.endProbe(first)

	if again := pool.tryHalfOpen(); again == nil {
		t.Fatal("探测结束后应可再次抢占")
	}
}

func TestTargetErrorDoesNotTripCooldown(t *testing.T) {
	t.Parallel()

	pool := newHealthPool(t)
	state := pool.states[0]

	now := time.Now()
	pool.recordTargetError(state, now, "proxy CONNECT api.example: 502 Bad Gateway")

	if state.failures != 0 {
		t.Fatalf("failures=%d want 0（目标侧拒绝不计代理健康）", state.failures)
	}

	if !state.cooldownTill.IsZero() {
		t.Fatalf("目标侧拒绝不应触发冷却，cooldownTill=%v", state.cooldownTill)
	}

	if state.lastErr == "" {
		t.Fatal("错误文本仍应记录，便于面板定位")
	}
}

func TestFailureCountDecaysAfterWindow(t *testing.T) {
	t.Parallel()

	pool := newHealthPool(t)
	state := pool.states[0]

	now := time.Now()
	// 模拟「历史上偶发失败累积到 5 次」，但上一次**真失败**已远早于最长冷却。
	state.failures = 5
	state.lastPenaltyAt = now.Add(-2 * proxyCooldownMax)
	state.lastFailAt = state.lastPenaltyAt

	pool.recordFailure(state, now, "dial timeout")

	if state.failures != 1 {
		t.Fatalf("failures=%d want 1（跨窗口后重新起算）", state.failures)
	}

	if got := state.cooldownTill.Sub(now); got != proxyCooldownBase {
		t.Fatalf("冷却=%v want %v（不按历史失败数指数放大）", got, proxyCooldownBase)
	}
}

func TestParseProxyRejectsOutOfRangePort(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		"socks5://127.0.0.1:70000",
		"http://127.0.0.1:0",
	} {
		if _, err := ParseProxy(raw); err == nil {
			t.Fatalf("ParseProxy(%q) 应报端口错误", raw)
		}
	}
}

// TestTargetErrorDoesNotExtendFailureDecay 目标侧错误不得续期「连续故障」窗口。
//
// recordTargetError（CONNECT 非 200，单域故障）明确不计代理健康，却会刷新
// 展示用的 lastFailAt。若时间衰减也读 lastFailAt，频繁的目标侧错误就把衰减窗口
// 无限续期：一个真失败间隔已超过 10 分钟的健康代理，failures 仍会跨窗口累积，
// 最终顶到满额冷却并自我维持。衰减只看 lastPenaltyAt（真失败时刻）。
func TestTargetErrorDoesNotExtendFailureDecay(t *testing.T) {
	t.Parallel()

	pool := newHealthPool(t)
	state := pool.states[0]

	now := time.Now()
	// 上一次真失败在 15 分钟前（> proxyCooldownMax = 10 分钟）。
	state.failures = 1
	state.lastPenaltyAt = now.Add(-15 * time.Minute)
	state.lastFailAt = state.lastPenaltyAt

	// 期间反复出现目标侧错误（每 5 分钟一次），把展示用的 lastFailAt 刷新到最近。
	pool.recordTargetError(state, now.Add(-10*time.Minute), "proxy CONNECT api.example: 502 Bad Gateway")
	pool.recordTargetError(state, now.Add(-5*time.Minute), "proxy CONNECT api.example: 502 Bad Gateway")

	pool.recordFailure(state, now, "dial timeout")

	if state.failures != 1 {
		t.Fatalf("failures=%d want 1（距上次真失败已跨窗口，应重新起算；目标侧错误不得续期）", state.failures)
	}

	if got := state.cooldownTill.Sub(now); got != proxyCooldownBase {
		t.Fatalf("冷却=%v want %v（不按历史失败数指数放大）", got, proxyCooldownBase)
	}
}

// TestProxyAuthErrorTripsCooldown 代理自身拒绝鉴权（CONNECT 407/401）不是目标侧
// 故障：不冷却出口就会拿着错误凭据无限重试。
func TestProxyAuthErrorTripsCooldown(t *testing.T) {
	t.Parallel()

	pool := newHealthPool(t)
	state := pool.states[0]

	err := fmt.Errorf("proxy CONNECT api.example: 407 Proxy Authentication Required: %w: %w",
		errDialHTTPConnectFailed, errProxyAuthRequired)

	pool.noteDialFailure(context.Background(), state, time.Now(), err)

	if state.failures == 0 {
		t.Fatal("failures=0（代理鉴权失败必须计入出口健康）")
	}
}
