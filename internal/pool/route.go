// route.go (realm, model) 路由级熔断（issue #149）。
//
// 账号级 modelCooldowns 只能表达「某账号在某模型不可用」；当全池对同一模型统一
// 返 11102/6004 时，网关仍会逐号重试到 rotate_max 才放弃。这里补一层全池级别的
// 组合短路：滑动窗口内失败达阈值 → 整条 (realm, model) 冷却一段时间。
package pool

import (
	"log"
	"time"
)

const (
	// routeFailureThreshold 滑动窗口内触发路由冷却的失败次数。
	routeFailureThreshold = 3
	// routeFailureWindow 失败计数窗口：只统计最近一分钟内的模型级失败。
	routeFailureWindow = time.Minute
	// routeCooldown 路由冷却时长；到期后自动恢复，不指数堆加。
	routeCooldownDuration = 5 * time.Minute
)

// routeState 单个 (realm, model) 的失败窗口与冷却截止。
type routeState struct {
	failures []time.Time
	until    time.Time
}

// routeKey 组合键：realm 为空表示「未指定域」，与选号时的 realm 参数同口径。
func routeKey(realm, model string) string { return realm + "\x1f" + model }

// NoteRouteFailure 记录一次模型级路由失败（11102 / 6004）。达阈值后整条路由冷却。
// model 为空时忽略（无模型维度无法定位路由）。
func (p *Pool) NoteRouteFailure(realm, model string) {
	if p == nil || model == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	key := routeKey(realm, model)
	st := p.routeCooldowns[key]
	if st == nil {
		st = &routeState{}
		p.routeCooldowns[key] = st
	}
	// 滑动窗口：丢弃过旧失败，避免低频偶发失败跨天累计后误熔断。
	cutoff := now.Add(-routeFailureWindow)
	kept := make([]time.Time, 0, len(st.failures)+1)
	for _, at := range st.failures {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	kept = append(kept, now)
	st.failures = kept
	if len(st.failures) < routeFailureThreshold {
		return
	}
	st.failures = nil
	st.until = now.Add(routeCooldownDuration)
	log.Printf("WARN: [pool] route cooldown realm=%q model=%q for %s after %d failures",
		realm, model, routeCooldownDuration, routeFailureThreshold)
}

// NoteRouteSuccess 清空该路由的失败窗口与冷却：实测成功即恢复。
func (p *Pool) NoteRouteSuccess(realm, model string) {
	if p == nil || model == "" {
		return
	}
	p.mu.Lock()
	delete(p.routeCooldowns, routeKey(realm, model))
	p.mu.Unlock()
}

// routeCoolingLocked 报告该路由是否处于冷却期；到期条目惰性删除。
// 调用方必须已持有 p.mu（读或写）。
func (p *Pool) routeCoolingLocked(realm, model string, now time.Time) bool {
	if p == nil || model == "" || len(p.routeCooldowns) == 0 {
		return false
	}
	key := routeKey(realm, model)
	st := p.routeCooldowns[key]
	if st == nil {
		return false
	}
	if now.Before(st.until) {
		return true
	}
	if !st.until.IsZero() {
		delete(p.routeCooldowns, key)
	}
	return false
}

// RouteRecovery 返回该路由冷却的截止时刻；无冷却返回零值。供 Retry-After 使用。
func (p *Pool) RouteRecovery(realm, model string) time.Time {
	if p == nil || model == "" {
		return time.Time{}
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	st := p.routeCooldowns[routeKey(realm, model)]
	if st == nil || !st.until.After(p.now()) {
		return time.Time{}
	}
	return st.until
}
