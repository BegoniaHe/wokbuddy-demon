// window.go 额度窗口探针台账（issue #492）。
//
// 6004 模型级限流带上游重置墙钟时，CooldownSoftForModel 只把 (账号, 模型) 冷到
// ResetAt。上游的额度窗口在重置后要等「第一条请求」才开始计时；如果一直没人请求，
// 新窗口的起点就跟着 drifted 到下一次真实流量上。这里把到点窗口登记成待探项，由
// 调度器在 ResetAt 到点后发一条最小请求把新窗口起点钉住。
//
// 口径：
//   - 只登记带非零 ResetAt 的模型级冷却，账号级冷却不登记；
//   - 探针是真实计费请求：出账前先过选号闸门（停用 / 暂停 / 积分保底），被闸门排除
//     的窗口跳过且不消耗尝试次数，等账号恢复后下一轮再探（issue #532）；
//   - 失败不罚号：探针结果只影响本台账的尝试计数，不改账号健康/冷却/连败；
//   - 分流重试：限流类失败按 windowProbeRetryBackoff 再试（最多
//     windowProbeMaxAttempts 次）；非限流失败快速出账，不再反复撞同一堵墙；
//   - 运行态、不持久化：进程重启时的台账丢失无关紧要，下一次 6004 会重新登记。
package pool

import (
	"slices"
	"strings"
	"time"
)

const (
	// windowProbeMaxAttempts 单个窗口最多探测次数。失败不罚号，但也不无限重试。
	windowProbeMaxAttempts = 2
	// windowProbeRetryBackoff 一次尝试后到下次尝试的最小间隔。
	windowProbeRetryBackoff = 10 * time.Minute
	// windowProbeMaxAge 重置时刻早于该年龄仍没探成说明进程久停/上游窗口已漂移，
	// 直接丢弃，避免重启后补发一堆过期探测。
	windowProbeMaxAge = 6 * time.Hour
	// maxWindowProbes 待探台账条目上限：键含客户端可任填的模型名，满额不再登记新窗口。
	maxWindowProbes = 1024
)

// WindowProbe 一个到点待执行的额度窗口探针：把上游重置后的新窗口起点钉住。
type WindowProbe struct {
	UID     string
	Model   string
	Realm   string
	ResetAt time.Time
}

// WindowProbeResult 一次探测的结论（决定台账出账还是保留重试）。
type WindowProbeResult int

const (
	// WindowProbePinned 读到首个有效帧：窗口起点已钉住，出账。
	WindowProbePinned WindowProbeResult = iota
	// WindowProbeRetry 限流类失败：窗口可能还没真正重置，按重试间隔再试。
	WindowProbeRetry
	// WindowProbeGiveUp 非限流失败（5xx / 传输错误 / 4xx）：快速出账，不再反复重试。
	WindowProbeGiveUp
)

// windowProbeState 单个 (账号, 模型) 窗口的探针台账（运行态）。
type windowProbeState struct {
	realm    string
	resetAt  time.Time
	attempts int
	nextAt   time.Time
}

// windowProbeKey 台账键：uid + 分隔符 + 模型名（模型名不含 \x1f）。
func windowProbeKey(uid, model string) string {
	return uid + "\x1f" + model
}

// registerWindowProbeLocked 登记一个到点窗口。同一 (账号, 模型) 同一 ResetAt 只登记
// 一次（重复的 6004 不重置尝试计数）。调用方必须已持有 p.mu。
//
// 表有硬上限：键含客户端可任填的模型名（与 route.go 的 routeLedgerMax 同一类风险），
// 满额时不再登记新窗口——只是这一轮不把新窗口起点钉住，不影响账号可用性与冷却。
func (p *Pool) registerWindowProbeLocked(uid, model, realm string, resetAt time.Time) {
	if resetAt.IsZero() || model == "" {
		return
	}

	key := windowProbeKey(uid, model)
	if st, ok := p.windowProbes[key]; ok && st.resetAt.Equal(resetAt) {
		return
	}

	if _, exists := p.windowProbes[key]; !exists && len(p.windowProbes) >= maxWindowProbes {
		return
	}

	p.windowProbes[key] = &windowProbeState{realm: realm, resetAt: resetAt}
}

// DueWindowProbes 取本轮可以探测的窗口（按 ResetAt 升序，最多 limit 条），并在同一
// 临界区内把本次尝试记入台账：调用方执行后必须调 WindowProbeDone 交回结果，中途
// 崩溃则这条窗口只损失一次尝试机会，不会无限重试。
func (p *Pool) DueWindowProbes(limit int) []WindowProbe {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := p.now()

	keys := make([]string, 0, len(p.windowProbes))

	for key, state := range p.windowProbes {
		uid, model, _ := strings.Cut(key, "\x1f")

		entry2 := p.byUID[uid]

		if !keepWindowProbeLocked(entry2, state, now) {
			delete(p.windowProbes, key) // 账号已删 / 过期台账 / 尝试用尽
			continue
		}

		// 选号闸门已排除的账号（停用 / 暂停 / 积分保底）不进本轮：真实计费请求不发，
		// 也不计入尝试次数，账号恢复后下一轮再探（issue #532）。
		if !p.windowProbeReadyLocked(entry2, state, model, now) {
			continue
		}

		keys = append(keys, key)
	}

	return p.claimDueWindowProbes(keys, limit, now)
}

// claimDueWindowProbes 排序（ResetAt 升序，同时刻按键名）、截断上限，并把本次尝试
// 记入台账。调用方必须已持有 p.mu。
func (p *Pool) claimDueWindowProbes(keys []string, limit int, now time.Time) []WindowProbe {
	if len(keys) == 0 {
		return nil
	}

	slices.SortFunc(keys, func(leftKey, rightKey string) int {
		left, right := p.windowProbes[leftKey], p.windowProbes[rightKey]
		if !left.resetAt.Equal(right.resetAt) {
			return left.resetAt.Compare(right.resetAt)
		}

		return strings.Compare(leftKey, rightKey)
	})

	if limit > 0 && len(keys) > limit {
		keys = keys[:limit]
	}

	out := make([]WindowProbe, 0, len(keys))

	for _, key := range keys {
		state := p.windowProbes[key]
		uid, model, _ := strings.Cut(key, "\x1f")

		state.attempts++
		state.nextAt = now.Add(windowProbeRetryBackoff)

		out = append(out, WindowProbe{UID: uid, Model: model, Realm: state.realm, ResetAt: state.resetAt})
	}

	return out
}

// keepWindowProbeLocked 报告台账是否保留：账号已删、重置时刻过老、尝试用尽都丢弃。
func keepWindowProbeLocked(entry2 *entry, state *windowProbeState, now time.Time) bool {
	if entry2 == nil {
		return false
	}

	if now.Sub(state.resetAt) > windowProbeMaxAge {
		return false
	}

	return state.attempts < windowProbeMaxAttempts
}

// windowProbeReadyLocked 报告窗口本轮是否可探测：到点、过了重试间隔，且账号仍在
// 选号闸门内。调用方必须已持有 p.mu。
func (p *Pool) windowProbeReadyLocked(
	entry2 *entry, state *windowProbeState, model string, now time.Time,
) bool {
	if now.Before(state.resetAt) || now.Before(state.nextAt) {
		return false
	}

	return p.windowProbeEligibleLocked(entry2, model, state.realm, now)
}

// WindowProbeDone 交回一次探测结果：成功或非限流失败立即出账；限流类失败保留台账
// 到下次重试间隔（尝试次数用尽后一并清理）。只动本台账，不碰账号健康与冷却。
func (p *Pool) WindowProbeDone(uid, model string, result WindowProbeResult) {
	key := windowProbeKey(uid, model)

	p.mu.Lock()
	defer p.mu.Unlock()

	state, exists := p.windowProbes[key]
	if !exists {
		return
	}

	if result != WindowProbeRetry || state.attempts >= windowProbeMaxAttempts {
		delete(p.windowProbes, key)
	}
}

// windowProbeEligibleLocked 报告窗口是否仍该探测：探针是真实计费请求，必须与选号
// 闸门同口径。停用 / 暂停 / 触及积分保底（含请求级预扣）一律跳过。
//
// 「免费号碰订阅专属模型」在本仓没有对应闸门（#529 明确未实现 accessMode 分流），
// 故没有可复用项。调用方必须已持有 p.mu。
func (p *Pool) windowProbeEligibleLocked(entry2 *entry, model, realm string, now time.Time) bool {
	if entry2.disabled || entry2.paused {
		return false
	}

	return !p.floorBlockedForRealmModel(entry2, model, realm, now)
}

// ReturnWindowProbe 归还一次「已出账但没真正发出去」的尝试（调度器拿不到在途名额时
// 走这里）：撤回 DueWindowProbes 记下的尝试次数与重试间隔，让下一轮立刻还能探。
//
// 与 WindowProbeDone 的区别是语义：Done 回答「探过、结果如何」，Return 回答「压根
// 没探」。混用会把「池子忙」记成「已钉住新窗口」或白烧掉一次 attempts 上限。
func (p *Pool) ReturnWindowProbe(uid, model string) {
	key := windowProbeKey(uid, model)

	p.mu.Lock()
	defer p.mu.Unlock()

	state, exists := p.windowProbes[key]
	if !exists {
		return
	}

	if state.attempts > 0 {
		state.attempts--
	}

	// 清掉重试间隔：本轮没发请求，不该按「限流后退避」等一个 windowProbeRetryBackoff。
	state.nextAt = time.Time{}
}

// dropWindowProbesLocked 清掉某账号的全部待探窗口（账号被移除时）。调用方必须已持有 p.mu。
func (p *Pool) dropWindowProbesLocked(uid string) {
	prefix := uid + "\x1f"

	for key := range p.windowProbes {
		if strings.HasPrefix(key, prefix) {
			delete(p.windowProbes, key)
		}
	}
}

// WindowProbeCount 当前待探窗口数（只读，面板/状态观测用）。
func (p *Pool) WindowProbeCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()

	return len(p.windowProbes)
}
