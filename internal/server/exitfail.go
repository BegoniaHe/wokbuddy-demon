// exitfail.go 出口维度传输层失败归因（issue #493）。
//
// 拨号 / TLS / 传输层 EOF 这类失败与「当前选中的账号」无关：一次网络抖动会让轮转到
// 的每个账号都拨号失败。既有实现把它们记成每个号的一次连败（NoteFailures），抖动一
// 次就能把整池连败计数拉高、触发降权。这里改为记到**出口**维度：
//   - 直连 = 全域共用出口（所有未绑定代理槽、且未配置全局代理池的账号共享）；
//   - 全局代理池 = "global"；
//   - 命名代理槽 = "slot:<id>"。
//
// 记账只用于观测：窗口内同一出口的失败账号数达阈值打一条 WARN（与 wafIPGate 同风格，
// 进程内状态、重启清零）。轮转行为不变——换号仍然继续，只是不再把失败记到账号账上。
package server

import (
	"log"
	"sync"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// exitFailWindow 出口失败判定窗口：窗内同一出口的不同账号数达阈值即告警。
var exitFailWindow = 2 * time.Minute

// exitFailThreshold 判定阈值：同一出口窗内 ≥ 2 个不同账号失败即视为出口问题。
// 取 2 是「多号」的最小定义（与 wafIPThreshold 同口径）；单号偶发失败不告警。
const exitFailThreshold = 2

// exitFailureGate 出口维度失败台账（Handler 内嵌，零值可用）。
type exitFailureGate struct {
	mu     sync.Mutex
	hits   map[string]map[string]time.Time // exit → uid → 最近失败时刻（窗口内，惰性剪枝）
	warned map[string]time.Time            // exit → 上次告警时刻（每窗口一次，避免刷屏）
}

// note 记一次出口维度失败；窗内不同账号数达阈值时打一条 WARN（每窗口一次）。
func (g *exitFailureGate) note(exit, uid string) {
	now := time.Now()

	g.mu.Lock()
	defer g.mu.Unlock()

	if g.hits == nil {
		g.hits = map[string]map[string]time.Time{}
		g.warned = map[string]time.Time{}
	}

	window := g.hits[exit]
	if window == nil {
		window = map[string]time.Time{}
		g.hits[exit] = window
	}

	window[uid] = now
	for u, at := range window {
		if now.Sub(at) > exitFailWindow {
			delete(window, u)
		}
	}

	if len(window) < exitFailThreshold {
		return
	}

	if last, ok := g.warned[exit]; ok && now.Sub(last) < exitFailWindow {
		return
	}

	g.warned[exit] = now
	log.Printf("WARN: [server] exit %s: %d accounts hit transport failure within %s "+
		"(attributed to exit, accounts not penalized)", exit, len(window), exitFailWindow)
}

// exitLabel 返回账号当前出口的归因标签：代理槽优先，其次全局代理池，最后直连。
func (h *Handler) exitLabel(account *auth.Auth) string {
	if account != nil && h.cfg.Upstream != nil {
		if slot, _ := h.cfg.Upstream.AccountRoute(account); slot != "" {
			return "slot:" + slot
		}

		if reg := h.cfg.Upstream.ProxyRegistry(); reg != nil && reg.Global() != nil {
			return "global"
		}
	}

	return "direct"
}
