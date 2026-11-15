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
	// routeLedgerMax 路由台账条目上限。
	//
	// 键含 model，而 model 直接来自客户端请求体：任意模型名都曾能建一条永久条目
	// （过期条目只在「同一条路由再次失败/成功」时清理），伪造模型名刷请求即可无界
	// 增长。超限时在写锁内整体清扫已过期的冷却条目。
	routeLedgerMax = 512
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
		// 只在真正新增条目时压缩台账：无条件先压缩会在台账满时把一条仍在冷却
		// 的真实路由淘汰掉（即使本次 key 已存在、并不会新增条目），客户端用
		// 伪造模型名就能把真实路由的短路保护冲掉。
		p.boundRouteLedgerLocked(now)

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

// boundRouteLedgerLocked 把路由台账压到 routeLedgerMax 以内（调用方已持写锁）。
func (p *Pool) boundRouteLedgerLocked(now time.Time) {
	if len(p.routeCooldowns) < routeLedgerMax {
		return
	}

	p.pruneRouteCooldownsLocked(now)
	// 全部条目都在冷却中时上面的清扫删不掉任何东西（每个伪造模型名只要失败三次就
	// 能进冷却），台账仍会随新模型名继续增长。再淘汰「最早解锁」的一条——它保护力
	// 最弱，丢掉后重新计数即可——把规模硬顶在 routeLedgerMax。
	for len(p.routeCooldowns) >= routeLedgerMax {
		p.evictEarliestRouteLocked()
	}
}

// evictEarliestRouteLocked 淘汰「最早解锁」的一条路由冷却（调用方已持写锁）。
// 只在台账顶到 routeLedgerMax 且全部条目都在冷却中时使用（见 NoteRouteFailure）。
func (p *Pool) evictEarliestRouteLocked() {
	evictKey := ""
	evictUntil := time.Time{}

	first := true

	for key, st := range p.routeCooldowns {
		until := time.Time{}
		if st != nil {
			until = st.until
		}

		if first || until.Before(evictUntil) {
			evictKey, evictUntil, first = key, until, false
		}
	}

	delete(p.routeCooldowns, evictKey)
}

// pruneRouteCooldownsLocked 清扫不在冷却中的路由条目（调用方已持写锁）。
//
// 冷却中的条目全部保留——它们仍在短路请求，删掉就等于放行。其余条目承载的只有
// 「窗口内的失败次数」，清掉等价于把这条路由的失败计数从头开始，比无界增长好。
// 因此台账规模的上界是「同时处于冷却中的路由数」，与真实故障规模同量级，而不是
// 与客户端能编出的模型名数量同量级。
func (p *Pool) pruneRouteCooldownsLocked(now time.Time) {
	for key, st := range p.routeCooldowns {
		if st != nil && now.Before(st.until) {
			continue
		}

		delete(p.routeCooldowns, key)
	}
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
