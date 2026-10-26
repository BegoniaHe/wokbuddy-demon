// 冷却与熔断：Cooldown（固定时长账号级冷却）、CooldownSoftRate（账号级软冷却，对齐
// 上游重置时间或有界退避）、CooldownSoftForModel（模型级软冷却，对齐重置墙钟）、
// BlockModelBackoff/Clear（11136/11141 模型不可用负缓存）、软冷却封顶、熔断失败累计、
// 签到解冻。
package pool

import (
	"log"
	"strings"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/cst"
	"github.com/BegoniaHe/wokbuddy-demon/internal/logfmt"
)

func (p *Pool) SetCredits(uid string, credits, total int64) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if e, ok := p.byUID[uid]; ok {
		e.credits = credits
		e.creditsTotal = total

		p.dirty.Store(true)
	}
}

// SetNickname 更新账号昵称并回写 auths 凭证文件（issue #94：上游改名后同步）。
// 昵称未变化时不写盘；uid 不存在 / 昵称为空返回 false。与 token 刷新共用
// auth 自身的锁与 SaveAtomic 原子写，无半更新窗口。
func (p *Pool) SetNickname(uid, nickname string) bool {
	if uid == "" || nickname == "" {
		return false
	}

	p.mu.RLock()
	entry2, ok := p.byUID[uid]
	p.mu.RUnlock()

	if !ok {
		return false
	}

	auth := entry2.a
	auth.Lock()

	changed := auth.Nickname != nickname
	if changed {
		auth.Nickname = nickname
	}
	auth.Unlock()

	if !changed {
		return false
	}

	if err := auth.SaveAtomic(); err != nil {
		log.Printf("WARN: [pool] nickname save %s: %v", uid, err)
		return false
	}

	return true
}

// NoteCheckinDone 标记账号今日已签到（签到成功与上游"今天已签到"幂等拒绝均算）。
// 记录本地日期，跨零点自然过期；不触碰冷却/禁用状态（签到与冷却域正交）。
func (p *Pool) NoteCheckinDone(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if e, ok := p.byUID[uid]; ok {
		day := cst.Day(p.now())
		if e.lastCheckinDay != day {
			e.lastCheckinDay = day

			p.dirty.Store(true)
		}
	}
}

// SetCreditsDetailed 更新账号余额/总额、配置窗口内的快过架子集，以及最早未来
// 到期批次。earliestAt 为零或不在未来时清空最早批次；expiring/earliestRemaining
// 均钳到 [0, credits]，避免上游脏数据污染选号。
func (p *Pool) SetCreditsDetailed(
	uid string, credits, total, expiring int64, earliestAt time.Time, earliestRemaining int64,
) {
	p.mu.Lock()
	defer p.mu.Unlock()

	entry2, ok := p.byUID[uid]
	if !ok {
		return
	}

	credits, expiring, earliestAt, earliestRemaining = clampCreditsDetailed(
		p.now(), credits, expiring, earliestAt, earliestRemaining,
	)
	entry2.credits = credits
	entry2.creditsTotal = total
	entry2.creditsExpiring = expiring
	entry2.creditsEarliestExpiry = earliestAt
	entry2.creditsEarliestRemaining = earliestRemaining
	// 本入口没有逐日批次信息（只给聚合值的外部/测试调用方）：清掉上一次周期刷新
	// 留下的批次，否则面板会把新余额/新最早批次与旧批次并排展示（快照与「检查」口径分裂）。
	// 生产路径（签到 / 单号刷新 / 后台刷新）统一走 SetCreditsSnapshot 保留完整批次。
	entry2.creditsExpiryBuckets = nil

	p.dirty.Store(true)
}

// SetCreditsSnapshot 写入一次余额快照（含逐日到期批次）：先按 SetCreditsDetailed
// 同口径钳制汇总值，再落批次并按批次重算「最早到期批次 / 剩余量」——
// 这样快照与面板「检查」对同一账号给出完全一致的批次（issue #200）。
// buckets 为空表示上游本次没有可用批次（清空旧批次，避免展示过期数据）。
func (p *Pool) SetCreditsSnapshot(uid string, credits, total, expiring int64, buckets []ExpiryBucket) {
	p.mu.Lock()
	defer p.mu.Unlock()

	entry2, ok := p.byUID[uid]
	if !ok {
		return
	}

	now := p.now()

	var earliestAt time.Time

	var earliestRemaining int64

	kept := make([]ExpiryBucket, 0, len(buckets))

	for _, bucket := range buckets {
		if bucket.Remain <= 0 || bucket.Date == "" {
			continue
		}

		kept = append(kept, bucket)

		if earliestAt.IsZero() || bucket.ExpireAt.Before(earliestAt) {
			earliestAt, earliestRemaining = bucket.ExpireAt, bucket.Remain
		} else if bucket.ExpireAt.Equal(earliestAt) {
			earliestRemaining += bucket.Remain
		}
	}

	credits, expiring, earliestAt, earliestRemaining = clampCreditsDetailed(
		now, credits, expiring, earliestAt, earliestRemaining,
	)

	entry2.credits = credits
	entry2.creditsTotal = total
	entry2.creditsExpiring = expiring
	entry2.creditsEarliestExpiry = earliestAt
	entry2.creditsEarliestRemaining = earliestRemaining

	if len(kept) == 0 {
		entry2.creditsExpiryBuckets = nil
	} else {
		entry2.creditsExpiryBuckets = kept
	}

	p.dirty.Store(true)
}

// clampCreditsDetailed 把上游余额/到期明细钳到合法区间：负数归零、子集不超过总额，
// 最早到期批次不在未来（或剩余为零）时整体清空。纯函数，便于单点校验。
func clampCreditsDetailed(
	now time.Time,
	credits, expiring int64,
	earliestAt time.Time,
	earliestRemaining int64,
) (int64, int64, time.Time, int64) {
	if credits < 0 {
		credits = 0
	}

	if expiring < 0 {
		expiring = 0
	}

	if expiring > credits {
		expiring = credits
	}

	if earliestRemaining < 0 {
		earliestRemaining = 0
	}

	if earliestRemaining > credits {
		earliestRemaining = credits
	}

	if earliestAt.IsZero() || !earliestAt.After(now) || earliestRemaining == 0 {
		earliestAt = time.Time{}
		earliestRemaining = 0
	}

	return credits, expiring, earliestAt, earliestRemaining
}

// ClearExpiringSnapshots 清空所有账号的快过期/最早到期缓存。配置窗口改变时调用，
// 避免在新快照写入前继续使用旧窗口得到的路由数据；下一次签到或余额刷新会重建。
func (p *Pool) ClearExpiringSnapshots() {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, e := range p.byUID {
		e.creditsExpiring = 0
		e.creditsEarliestExpiry = time.Time{}
		e.creditsEarliestRemaining = 0
	}

	p.dirty.Store(true)
}

// Cooldown 冷却账号至 now+d（即时冷却：CoolHard 余额耗尽 / CoolSoft 固定短冷却）。
//
// 重构后本入口是「固定时长的账号级冷却」，不再做两件旧事：
//   - 不再喂熔断器失败计数：熔断器只对「反复失败」（NoteError，5xx）退避。
//     软限流/余额耗尽各有权威恢复时刻（重置墙钟 / 04:00 签到），再并入"连续失败"
//     会让用户正常重试越堆越厚。熔断语义由 NoteError 唯一驱动（与 until 正交保持）。
//   - 不再做 softStreak 指数堆加：固定 d 即最终时长。CoolSoft 的精确对齐请用
//     CooldownSoftRate（有界、对齐上游重置时间）。
func (p *Pool) Cooldown(uid string, kind CoolKind, d time.Duration, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if entry2, ok := p.byUID[uid]; ok {
		entry2.until = p.now().Add(d)
		entry2.coolKind = kind
		entry2.reason = reason
		// 非模型级冷却入口：清空会参与路由的模型级冷却，避免上一次
		// 模型豁免泄漏到账号级冷却上；AuditOnly 条目不影响路由，保留展示。
		clearRoutingModelCooldownsLocked(entry2)
		p.dirty.Store(true)
	}
}

// CooldownSoftForModel 429 的软冷却入口（issue #31）：有模型维度且带上游重置
// 时间时只冷该模型，不冷整号。
//
//   - resetAt 非零且 model 非空 → modelCooldowns[model].Until = min(resetAt,
//     now+softRateMax)，ResetAt 记录上游原始墙钟（台账 ResetAt）。不做指数堆加，
//     切模型即可用。
//   - resetAt 零值（无时间文案）→ 无法确认是模型级还是套餐额度耗尽，保守按账号级
//     有界退避（同 CooldownSoftRate）。
//   - model 为空（无模型维度的防御路径）→ 同样退回账号级，绝不在 modelCooldowns
//     写空键。
func (p *Pool) CooldownSoftForModel(uid string, base time.Duration, resetAt time.Time, model, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	entry2, ok := p.byUID[uid]
	if !ok {
		return
	}

	now := p.now()

	if model == "" || resetAt.IsZero() {
		p.cooldownSoftRateLocked(entry2, now, base, resetAt, reason)
		p.dirty.Store(true)

		return
	}

	// 有上游重置时间：冷却截止 = min(resetAt, now+softRateMax)，不做指数放大。
	if entry2.modelCooldowns == nil {
		entry2.modelCooldowns = map[string]modelCooldown{}
	}

	entry2.modelCooldowns[model] = modelCooldown{
		Until:   p.cappedSoftUntilLocked(now, resetAt),
		ResetAt: resetAt,
		Reason:  reason,
	}
	if p.onModelRateLimit != nil {
		p.onModelRateLimit(uid, model, entry2.a.Realm(), resetAt)
	}

	p.dirty.Store(true)
}

// modelBusyBase/modelBusyMax 14003「模型繁忙」独立短窗口：10s 起、1m 封顶。
const (
	modelBusyBase = 10 * time.Second
	modelBusyMax  = time.Minute
)

// CooldownModelBusy 把 (账号, 模型) 停一个短窗口（14003 模型繁忙专用）。
// 只写模型级冷却，不动账号级 until；已有更晚的冷却时保留更晚者。
func (p *Pool) CooldownModelBusy(uid, model, reason string) {
	if uid == "" || model == "" {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	entry2, ok := p.byUID[uid]
	if !ok {
		return
	}

	now := p.now()

	old, exists := entry2.modelCooldowns[model]
	if !busyWindowAllowed(&old, exists, now) {
		return
	}

	hits, until := busyWindow(&old, exists, now)

	if entry2.modelCooldowns == nil {
		entry2.modelCooldowns = map[string]modelCooldown{}
	}

	entry2.modelCooldowns[model] = modelCooldown{Until: until, Reason: reason, Hits: hits + 1}

	p.dirty.Store(true)
}

// busyWindowAllowed 已有更权威的模型冷却（6004/11136/11141）时不降级成短窗。
// AuditOnly 台账不参与路由，不构成「更权威」；否则审计项会挡住真正的 14003 短窗。
func busyWindowAllowed(old *modelCooldown, exists bool, now time.Time) bool {
	if !exists || old.AuditOnly {
		return true
	}

	return strings.HasPrefix(old.Reason, "14003") || !old.Until.After(now)
}

// busyWindow 返回 14003 的累计命中次数与本次冷却截止（10s 起、1m 封顶）。
// AuditOnly 台账的 Hits/Until 不属于真实冷却，不得延长本次短窗。
func busyWindow(old *modelCooldown, exists bool, now time.Time) (int, time.Time) {
	if exists && old.AuditOnly {
		exists = false
	}

	hits := 0
	if exists && strings.HasPrefix(old.Reason, "14003") {
		hits = old.Hits
	}

	// hits 是本次之前的命中次数：第 1 次 10s、第 2 次 20s、第 3 次 40s，封顶 1m。
	duration := modelBusyBase
	for i := 0; i < hits && duration < modelBusyMax; i++ {
		duration *= 2
	}

	if duration > modelBusyMax {
		duration = modelBusyMax
	}

	until := now.Add(duration)
	if exists && old.Until.After(until) {
		until = old.Until
	}

	return hits, until
}

// RecordModelRateLimitAudit 记录无法参与模型路由的 6004 展示项。
// 典型场景是 6004 没有可解析重置时间：账号仍按原有有界退避冷却，
// 本方法只把模型名挂到 e.until 上供账号页展示，不影响 healthyForModel。
func (p *Pool) RecordModelRateLimitAudit(uid, model, reason string) {
	if uid == "" || model == "" {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	entry2, ok := p.byUID[uid]
	if !ok {
		return
	}

	now := p.now()
	if old, exists := entry2.modelCooldowns[model]; exists && !old.AuditOnly && old.Until.After(now) {
		return // 已有真实模型冷却，审计记录不得覆盖路由截止
	}

	until := entry2.until
	if until.IsZero() || !until.After(now) {
		until = now.Add(p.softRateMaxOr())
	}

	if entry2.modelCooldowns == nil {
		entry2.modelCooldowns = make(map[string]modelCooldown)
	}

	entry2.modelCooldowns[model] = modelCooldown{
		Until:     until,
		Reason:    reason,
		AuditOnly: true,
	}

	p.dirty.Store(true)
}

// clearRoutingModelCooldownsLocked 删除参与选号豁免的模型冷却，保留 AuditOnly 台账。
// 调用方必须已持有 p.mu 写锁。
func clearRoutingModelCooldownsLocked(entry3 *entry) {
	for model, mc := range entry3.modelCooldowns {
		if !mc.AuditOnly {
			delete(entry3.modelCooldowns, model)
		}
	}

	if len(entry3.modelCooldowns) == 0 {
		entry3.modelCooldowns = nil
	}
}

// modelBlock TTL 常量（11136/11141 模型不可用负缓存退避）：
// 首次命中 6h；半开到期后允许放行重试，再次命中 TTL = base × 2^min(hits-1, shift)；
// 封顶 24h（最多一天再试一次）。该模型请求成功即由 BlockModelClear 清除。
const (
	modelBlockBaseTTL = 6 * time.Hour
	modelBlockShift   = 4
	modelBlockMaxTTL  = 24 * time.Hour
)

// BlockModelBackoff 「(账号, 模型) 不可用」负缓存入口（handler.applyErrorPolicy 调用），
// 承载 11136 账号无该模型权限与 11141 模型下线两类原因。复用 modelCooldowns 机制
// （不新建平行状态）：写 modelCooldowns[model]，Until 为指数退避 TTL，选号侧
// healthyForModel 自动对该账号避开该模型。
//
// 语义与 6004 正交：6004 是「模型被限流、对齐重置墙钟」，本入口是「官方确定该后端
// 不可用、本账号重试无意义，换号仍可能成功（11136）或只会同样失败（11141）」。
// resetAt 无需传（两族都无重置文案），ResetAt 保持零值，与 6004 台账共用 Until
// 判定——条目以各自 reason 前缀出现在 /status 台账，运维可见。成功实测后由
// BlockModelClear 按前缀清除。
func (p *Pool) BlockModelBackoff(uid, model, reason string) {
	if uid == "" || model == "" {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	entry2, ok := p.byUID[uid]
	if !ok {
		return
	}

	now := p.now()

	hits := 0
	if entry2.modelCooldowns != nil {
		hits = entry2.modelCooldowns[model].Hits
	}

	hits++

	ttl := modelBlockBaseTTL
	if d := ttl * (1 << uint(min(hits-1, modelBlockShift))); d < modelBlockMaxTTL {
		ttl = d
	} else {
		ttl = modelBlockMaxTTL
	}

	if entry2.modelCooldowns == nil {
		entry2.modelCooldowns = map[string]modelCooldown{}
	}

	entry2.modelCooldowns[model] = modelCooldown{
		Until:  now.Add(ttl),
		Reason: reason,
		Hits:   hits,
	}

	p.dirty.Store(true)
}

// modelUnavailableReason 判定模型级冷却原因是否属于「模型不可用」负缓存：
// 11136 账号权限缺失（upstream.ModelBlockReason）、11158 账号类型级权限拒绝
// （upstream.ModelForbiddenReason）与 11141 模型下线（upstream.ModelAccessReason）
// 共用该口径；6004 限流不算。
func modelUnavailableReason(reason string) bool {
	return strings.HasPrefix(reason, "11136") || strings.HasPrefix(reason, "11158") ||
		strings.HasPrefix(reason, "11141")
}

// modelRateLimitReason 判定模型级冷却原因是否来自模型级限流/繁忙（6004/14003）。
// 与 modelUnavailableReason 互斥：前者是"等等再试"，后者是"换模型才有用"。
func modelRateLimitReason(reason string) bool {
	return strings.HasPrefix(reason, "6004") || strings.HasPrefix(reason, "14003")
}

// BlockModelClear 清除 (账号, 模型) 的模型避让条目（该模型实测又通了）。handler 在
// 成功路径调用。负缓存到期前选号不会命中被避让账号（全冷却兜底也已排除），所以常规
// 恢复靠 TTL 到期；本入口主要覆盖并发下「成功与负缓存新写入交错」的窗口。只清
// 11136/11158/11141 三类可恢复避让条目、不碰 6004 独立冷却表——6004 有自身上游
// 重置墙钟语义，成功不该抹掉。
func (p *Pool) BlockModelClear(uid, model string) {
	if uid == "" || model == "" {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	entry2, ok := p.byUID[uid]
	if !ok || len(entry2.modelCooldowns) == 0 {
		return
	}

	mc, exists := entry2.modelCooldowns[model]
	if !exists || !modelUnavailableReason(mc.Reason) {
		return
	}

	delete(entry2.modelCooldowns, model)

	if len(entry2.modelCooldowns) == 0 {
		entry2.modelCooldowns = nil
	}

	p.dirty.Store(true)
}

// 模型级阻塞的范围（issue #29）：
//   - platform：>=2 个被挡账号共享同一非零上游重置时刻 ResetAt——推断为上游**平台级**
//     限流（同一时刻对全池解封），运维应等上游恢复；
//   - account：各账号独立冷却 / 无重置时刻 / 仅单账号被挡——账号级问题，换号或等本号恢复。
const (
	ModelBlockScopePlatform = "platform"
	ModelBlockScopeAccount  = "account"
)

// ModelBlockStatus 描述某模型在全池范围内因模型级冷却而不可选的情况。
type ModelBlockStatus struct {
	Blocked bool      // true = 每个非禁用账号都对该模型处于冷却中
	Reason  string    // 冷却原因（通常为上游原文，如 "11136 no model permission"）
	Until   time.Time // 最早解封时间（零值 = 上游未给重置时刻）
	Count   int       // 因此被挡的账号数
	// RateLimited true = 被挡原因全部是 6004 模型级限流（等上游重置），
	// 而非 11136/11141 的模型不可用（换模型才有用）。上层据此选 429 还是 400。
	RateLimited bool
	// Busy true = 被挡原因全部是 14003「模型繁忙」短窗口：可短暂等待后重试，
	// 上层返回 503 model_busy 而不是 429 rate_limit_exceeded（issue #254）。
	Busy bool
	// Scope 阻塞范围：platform / account（见常量）。只读诊断，不参与选号。
	Scope string
	// SharedResetAt 平台级时的共享上游重置时刻（账号级为零值）。
	SharedResetAt time.Time
}

// modelBlockScope 由被挡账号的重置时刻一致性推断阻塞范围：全部被挡账号（>=2）
// 都带非零 ResetAt 且完全一致 → platform；否则 account。精确相等（无容差）是保守
// 选择——上游给语义相同但毫秒级不同的时刻时会判账号级，不会误报平台级。
func modelBlockScope(total, withReset, distinctResets int, shared time.Time) (string, time.Time) {
	if total >= 2 && withReset == total && distinctResets == 1 && !shared.IsZero() {
		return ModelBlockScopePlatform, shared
	}

	return ModelBlockScopeAccount, time.Time{}
}

// ModelBlocked 报告该模型是否在全池范围内被模型级冷却挡住。
//
// 为什么需要它：选号失败时客户端只会拿到 no_healthy_account（"没有可用账号"），
// 但真实原因常常是「号都在、只是都对这个模型关闭」。两者对调用方的处置完全不同
// ——前者该等，后者换个模型才有用——此前却无法区分：首次请求还能看到上游原文
// （lastErr 非空），一旦负缓存写入，后续请求 lastErr 为空，就只剩"池子没号"
// （issue #102 附带发现 1）。上游原文与解封时间在那里被丢掉。
//
// 口径必须与选号一致：用 modelCooled 而非直接查 map，这样 AuditOnly 条目
// （只审计不拦截）不会被误报成阻塞；选号按 realm 分流时也要按 realm 统计
// （见 ModelBlockedForRealm），不能用异域健康号否定本域的阻塞结论。
func (p *Pool) ModelBlocked(model string) ModelBlockStatus {
	return p.ModelBlockedForRealm(model, "")
}

// modelBlockIgnores 报告账号是否不参与全池模型阻塞统计：禁用/暂停号不可选，
// 异域账号（realm 非空且不匹配）与本次选号无关。
func modelBlockIgnores(entry2 *entry, realm string) bool {
	return entry2.disabled || entry2.paused || (realm != "" && entry2.a.Realm() != realm)
}

// ModelBlockedForRealm 同 ModelBlocked，但只统计 Realm()==realm 的账号
// （realm 空 = 不过滤）。选号只在本域内挑号，阻塞判定必须同口径：否则混合池里
// 「本域账号全被模型级冷却挡住、异域还有健康号」会返回 Blocked=false，把
// 11136/11141 的 400 与 6004 的 429 + Retry-After 退化成 503 no_healthy_account。
func (p *Pool) ModelBlockedForRealm(model, realm string) ModelBlockStatus {
	if model == "" {
		return ModelBlockStatus{}
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	now := p.now()

	// RateLimited/Busy 默认按限流起步，merge 遇到不匹配原因时清零。
	modelBlockStatus := ModelBlockStatus{RateLimited: true, Busy: true}
	// 重置时刻一致性统计（issue #29）：distinct 用集合去重，withReset 计带非零
	// ResetAt 的账号数，shared 记唯一值。
	var (
		withReset int
		distinct  = make(map[time.Time]struct{})
		shared    time.Time
	)

	for _, item := range p.byUID {
		// 暂停选号（paused）号与禁用号同口径跳过：它此刻不可选，「能服务」的
		// 证明不成立——否则一个暂停的健康号会掩盖「其余号全被模型级冷却挡住」。
		if modelBlockIgnores(item, realm) {
			continue
		}

		if !item.modelCooled(now, model) {
			// 还有账号能服务这个模型 → 不是模型级阻塞。
			return ModelBlockStatus{}
		}

		modelBlockStatus.Count++
		modelCooldown2, ok := item.modelCooldowns[model]
		modelBlockStatus.mergeModelCooldown(&modelCooldown2, ok)

		if ok && !modelCooldown2.ResetAt.IsZero() {
			withReset++
			distinct[modelCooldown2.ResetAt] = struct{}{}
			shared = modelCooldown2.ResetAt
		}
	}

	if modelBlockStatus.Count == 0 {
		// 池里压根没有非禁用账号：这是"真的没号"，不是模型问题。
		return ModelBlockStatus{}
	}

	modelBlockStatus.Blocked = true
	modelBlockStatus.Scope, modelBlockStatus.SharedResetAt = modelBlockScope(
		modelBlockStatus.Count, withReset, len(distinct), shared,
	)

	return modelBlockStatus
}

// mergeModelCooldown 把单个账号的模型级冷却并入全池阻塞画像：记录首个非空原因，
// 并取最早解封时刻（那才是"再等多久值得重试"的答案）。
func (st *ModelBlockStatus) mergeModelCooldown(modelCooldown2 *modelCooldown, ok bool) {
	if !ok {
		st.RateLimited = false
		st.Busy = false

		return
	}

	if st.Reason == "" {
		st.Reason = modelCooldown2.Reason
	}

	if !modelRateLimitReason(modelCooldown2.Reason) {
		st.RateLimited = false
	}

	if !strings.HasPrefix(modelCooldown2.Reason, "14003") {
		st.Busy = false
	}

	if !modelCooldown2.Until.IsZero() && (st.Until.IsZero() || modelCooldown2.Until.Before(st.Until)) {
		st.Until = modelCooldown2.Until
	}
}

// CooldownSoftRate 429/限流文案的**账号级**软冷却入口（handler.applyErrorPolicy 调用）。
//
// 语义：
//   - resetAt 非零（上游带权威重置时间，无论 6004 还是 11140 rate-limiting）→
//     账号级直到该墙钟（截断到 softRateMax，绝不指数堆加）；**不**在
//     modelCooldowns 记模型（账号级语义，不产生切模型豁免——普通账号级限流不该
//     因切模型绕过）。
//   - resetAt 零值且**不在冷却中**（首次/恢复后的新限流）→ 有界退避：按 softStreak
//     指数退避并封顶 softRateMax。softStreak 只在真正进入一次新冷却时计数，由
//     NoteSuccess/reviveCoolingLocked 清零（既有恢复语义）。
//   - resetAt 零值且**已在软冷却中**（兜底探测再次撞 429）→ 不推进 streak、不延长
//     until：用户重试/并发兜底探测不得把冷却越堆越厚——这正是旧实现「越重试越冷、
//     全池被推到 2h 封顶」的元凶（每次探测都 softStreak++ 指数翻倍）。
func (p *Pool) CooldownSoftRate(uid string, base time.Duration, resetAt time.Time, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if entry2, ok := p.byUID[uid]; ok {
		p.cooldownSoftRateLocked(entry2, p.now(), base, resetAt, reason)
		p.dirty.Store(true)
	}
}

// cooldownSoftRateLocked 账号级软冷却本体（语义见 CooldownSoftRate）。
// 调用方必须已持有 p.mu。
func (p *Pool) cooldownSoftRateLocked(
	entry2 *entry, now time.Time, base time.Duration, resetAt time.Time, reason string,
) {
	if !resetAt.IsZero() {
		entry2.until = p.cappedSoftUntilLocked(now, resetAt)
	} else if entry2.coolKind != CoolSoft || !now.Before(entry2.until) {
		// 新限流（不在有效软冷却中）：推进有界退避；兜底探测（仍在软冷却中）不翻倍。
		d := p.jitterSoftDurationLocked(base, entry2.softStreak+1)
		entry2.softStreak++
		entry2.until = now.Add(d)
	}

	entry2.coolKind = CoolSoft
	entry2.reason = reason
	clearRoutingModelCooldownsLocked(entry2) // 账号级软冷却：清路由豁免，保留审计台账
}

// cappedSoftUntilLocked 把上游重置墙钟截断到 softRateMax（now+softRateMax 与 resetAt
// 取较早者）。resetAt 已过期（时钟偏移/文案过期）时时长钳到时间零点附近，立即恢复。
// 调用方必须已持有 p.mu。
func (p *Pool) cappedSoftUntilLocked(now, resetAt time.Time) time.Time {
	capUntil := now.Add(p.softRateMaxOr())
	if resetAt.After(capUntil) {
		return capUntil
	}

	if resetAt.After(now) {
		return resetAt
	}

	return now.Add(time.Millisecond)
}

// softRateMaxOr 返回生效的 softRateMax（未注入时按默认 2h），供封顶计算。
// 调用方必须已持有 p.mu。
func (p *Pool) softRateMaxOr() time.Duration {
	if p.softRateMax > 0 {
		return p.softRateMax
	}

	return defaultSoftRateMax
}

// softDurationLocked 按连续软冷却次数把基数 d 指数放大：d << (streak-1)，封顶 softRateMax。
// softRateMax 未注入（<=0）时按 defaultSoftRateMax 算。streak<=1 时原样返回 d。
// 左移位数受 softStreakShiftMax 限制，避免 streak 极大时移位溢出。
// 调用方必须已持有 p.mu。
func (p *Pool) softDurationLocked(duration time.Duration, streak int) time.Duration {
	if streak <= 1 {
		return duration
	}

	shift := min(streak-1, softStreakShiftMax)
	duration <<= shift

	capMax := p.softRateMax
	if capMax <= 0 {
		capMax = defaultSoftRateMax
	}

	if duration > capMax || duration <= 0 { // d<=0：左移溢出成负数/零，同样按封顶兜底
		duration = capMax
	}

	return duration
}

// jitterSoftDurationLocked 给软冷却叠加 ±10% 抖动，并再次按 softRateMax 封顶，
// 保证抖动不突破既有最晚封顶。
func (p *Pool) jitterSoftDurationLocked(base time.Duration, streak int) time.Duration {
	d := jitterCooldown(p.softDurationLocked(base, streak))
	if capMax := p.softRateMaxOr(); d > capMax {
		return capMax
	}

	return d
}

// recordBreakerFailureLocked 累计一次熔断失败；达到阈值则按指数退避熔断。
// 熔断与冷却（until）解耦：冷却按错误类别给固定时长，熔断则对"反复失败"逐次加长封禁。
// 调用方必须已持有 p.mu。
func (p *Pool) recordBreakerFailureLocked(entry2 *entry) {
	entry2.fails++
	if entry2.fails < p.breakerThreshold {
		return
	}

	duration := p.breakerCooldown
	for range entry2.retryCount {
		duration *= 2
		if duration >= p.breakerCooldownMax {
			duration = p.breakerCooldownMax
			break
		}
	}
	// 触发熔断：重置失败计数供下一轮重新累计；retryCount 递增放大退避指数。
	entry2.fails = 0
	entry2.retryCount++
	entry2.breakerUntil = p.now().Add(duration)
	log.Printf("WARN: [pool] breaker open acct=%s until=%s retryCount=%d",
		logfmt.Label(entry2.a.UID, entry2.a.NicknameValue()),
		entry2.breakerUntil.Format(time.RFC3339), entry2.retryCount)
}

// CooldownUntilTomorrow4AM 冷却到下一个 04:00（CST，上游自然日）。
// 用于 ErrHardCredit 场景：积分耗尽账号等签到任务（09:00/21:00）恢复。
func (p *Pool) CooldownUntilTomorrow4AM(uid, reason string) {
	now := p.now()
	p.Cooldown(uid, CoolHard, nextDay4AM(now).Sub(now), reason)
}

// nextDay4AM 返回 now 之后最近的一个 04:00（固定 CST，上游自然日）。
// now 在当天 04:00 之前（凌晨 00:00~04:00）时返回当天 04:00——此时签到尚未执行，
// 该窗内触发的硬冷却等当天签到即可恢复；返回次日会白冷约一天。
// 04:00 整及之后返回次日 04:00。
// time.Date 对日溢出自动进位（月末→下月 1 号、年末→下年 1 号），天然覆盖跨日/跨月/跨年。
func nextDay4AM(now time.Time) time.Time {
	local := now.In(cst.Loc)
	if local.Hour() < 4 {
		return time.Date(local.Year(), local.Month(), local.Day(), 4, 0, 0, 0, cst.Loc)
	}

	return time.Date(local.Year(), local.Month(), local.Day()+1, 4, 0, 0, 0, cst.Loc)
}

// ReenableIfCredits 签到/余额刷新后解冻：仅当 remain > 0 且账号非禁用时，解冻
// **余额耗尽冷却**（CoolHard）。软限流（CoolSoft）与模型级台账（modelCooldowns）
// 不在此清除——它们的恢复证据是上游重置墙钟到期，不是余额恢复（余额刷新周期
// 任务每 5 分钟到达这里，全清会把限流冷却实际寿命压到一个刷新周期内）。
// 注意：不碰熔断器——熔断到期（breakerUntil 过期）或下次 chat 成功（NoteSuccess）才恢复。
// reviveCoolingLocked 已迁至 transition.go（状态机迁移唯一权威实现）。
