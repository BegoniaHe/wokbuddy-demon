// route.go (realm, model) 路由级熔断（issue #149）。
//
// 账号级 modelCooldowns 只能表达「某账号在某模型不可用」；当全池对同一模型统一
// 失败时（11141 模型已下线 / 6004 模型级限流），每个请求仍会打一次上游——6004
// 还会逐号重试到 rotate_max。这里补一层全池级别的组合短路：滑动窗口内失败达
// 阈值 → 整条 (realm, model) 冷却一段时间。
//
// 11136（账号无该模型权限）不进本层：权限是账号维度，换号仍可能成功；把少数
// 无权限账号的连续失败升级成整条路由冷却会饿死仍有权限的账号。
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

// NoteRouteFailure 记录一次平台级模型路由失败（11141 / 6004）。达阈值后整条路由冷却。
// model 为空时忽略（无模型维度无法定位路由）。
func (p *Pool) NoteRouteFailure(realm, model string) {
	if p == nil || model == "" {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	now := p.now()
	key := routeKey(realm, model)

	routeState2 := p.routeCooldowns[key]
	if routeState2 == nil {
		routeState2 = &routeState{}
		p.routeCooldowns[key] = routeState2
	}
	// 冷却已过期：在写锁内清理残留的截止时间与失败窗口。
	if !routeState2.until.IsZero() && !now.Before(routeState2.until) {
		routeState2.until = time.Time{}
		routeState2.failures = nil
	}
	// 滑动窗口：丢弃过旧失败，避免低频偶发失败跨天累计后误熔断。
	cutoff := now.Add(-routeFailureWindow)

	kept := make([]time.Time, 0, len(routeState2.failures)+1)
	for _, at := range routeState2.failures {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}

	kept = append(kept, now)

	routeState2.failures = kept
	if len(routeState2.failures) < routeFailureThreshold {
		return
	}

	routeState2.failures = nil
	routeState2.until = now.Add(routeCooldownDuration)
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

// routeCoolingLocked 报告该路由是否处于冷却期。
// 调用方可能只持读锁（WeightedAvailableUIDsForModelRealm），因此这里不得删除
// 条目；过期条目由 NoteRouteFailure 在写锁内清理（或一直保留到该路由再次失败/成功）。
func (p *Pool) routeCoolingLocked(realm, model string, now time.Time) bool {
	if p == nil || model == "" || len(p.routeCooldowns) == 0 {
		return false
	}

	st := p.routeCooldowns[routeKey(realm, model)]

	return st != nil && now.Before(st.until)
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
