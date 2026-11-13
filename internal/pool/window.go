// window.go 额度窗口探针台账（issue #492）。
//
// 6004 模型级限流带上游重置墙钟时，CooldownSoftForModel 只把 (账号, 模型) 冷到
// ResetAt。上游的额度窗口在重置后要等「第一条请求」才开始计时；如果一直没人请求，
// 新窗口的起点就跟着 drifted 到下一次真实流量上。这里把到点窗口登记成待探项，由
// 调度器在 ResetAt 到点后发一条最小请求把新窗口起点钉住。
//
// 口径：
//   - 只登记带非零 ResetAt 的模型级冷却，账号级冷却不登记；
//   - 失败不罚号：探针结果只影响本台账的尝试计数，不改账号健康/冷却/连败；
//   - 有界重试：同一窗口最多 windowProbeMaxAttempts 次，两次之间至少
//     windowProbeRetryBackoff；成功后立即出账，不重复触发；
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
//
//nolint:cyclop // 台账筛选按「过期 → 用尽 → 未到点 → 可探测」四档线性展开。
func (p *Pool) DueWindowProbes(limit int) []WindowProbe {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := p.now()

	keys := make([]string, 0, len(p.windowProbes))

	for key, state := range p.windowProbes {
		switch {
		case now.Sub(state.resetAt) > windowProbeMaxAge:
			delete(p.windowProbes, key) // 过期台账：重启补跑无意义
		case state.attempts >= windowProbeMaxAttempts:
			delete(p.windowProbes, key) // 尝试用尽
		case now.Before(state.resetAt), now.Before(state.nextAt):
			// 未到点或仍在重试间隔内
		default:
			keys = append(keys, key)
		}
	}

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

// WindowProbeDone 交回一次探测结果：成功即出账（窗口已钉住），失败保留台账到下次
// 重试间隔（尝试次数用尽后一并清理）。只动本台账，不碰账号健康与冷却。
func (p *Pool) WindowProbeDone(uid, model string, ok bool) {
	key := windowProbeKey(uid, model)

	p.mu.Lock()
	defer p.mu.Unlock()

	state, exists := p.windowProbes[key]
	if !exists {
		return
	}

	if ok || state.attempts >= windowProbeMaxAttempts {
		delete(p.windowProbes, key)
	}
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
