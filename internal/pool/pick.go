// 选号：Pick 簇（healthy 成本分层 + 快过期虚拟实例权重 + 全冷却兜底 + 在途占满过滤）。
package pool

import (
	"log"
	"math/rand/v2"
	"sort"
	"strconv"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/logfmt"
)

// Pick 单一选号入口（无请求级轮换、无 realm 过滤，模型感知缺省账号级）。
// 需要请求级轮换（tried）或分池（realm）时用 PickExcludingForRealm。
func (p *Pool) Pick() *auth.Auth {
	return p.pick(nil, "", "")
}

// PickExcluding 同上，但跳过 tried 中的 uid（请求级轮换）。
// 挑选策略：healthy 账号中按权重取前 5 名，再在 Top5 内按同一权重加权随机抽签，
// 意图是打散热点，避免永远打同一个账号。
func (p *Pool) PickExcluding(tried map[string]bool) *auth.Auth {
	return p.pick(tried, "", "")
}

// PickExcludingForModel 模型感知选号：等同 PickExcluding，但对「6004 模型级冷却中的
// 账号」进行模型豁免——请求模型与其 trigger 模型不同时视为可用（issue #31）。
// reqModel 为空时即普通 PickExcluding（不影响既有调用语义）。
func (p *Pool) PickExcludingForModel(tried map[string]bool, reqModel string) *auth.Auth {
	return p.pick(tried, reqModel, "")
}

// PickExcludingForRealm 模型感知 + 分池选号：候选集先按 Realm()==realm 过滤
// （realm 空 = 不过滤，退化为 PickExcludingForModel），再按模型健康口径判定。
// 供 handler 在 global/cn 双域下分流（global 模型请求只路由 global 账号）。
func (p *Pool) PickExcludingForRealm(tried map[string]bool, reqModel, realm string) *auth.Auth {
	return p.pick(tried, reqModel, realm)
}

// pick 在 healthy 候选集中按权重加权随机选出账号，并记录 lastUsed（防并发撞号）。
// reqModel 非空时把健康口径换成 healthyForModel（6004 模型豁免生效）。
// realm 非空时候选过滤叠加 Realm()==realm 谓词（分池选号域）。
// 拆成「收集候选 → 定成本层 → 排序 → 选号」四段，各段独立可读。
func (p *Pool) pick(tried map[string]bool, reqModel, realm string) *auth.Auth {
	return p.pickTraceWith(tried, reqModel, realm, nil)
}

// PickTrace 一次选号的决策快照（issue #63）：字段有界，不含凭证/提示词。
type PickTrace struct {
	Candidates int             `json:"candidates"`
	Excluded   map[string]int  `json:"excluded,omitempty"`
	CostTier   int             `json:"cost_tier"`
	Explored   bool            `json:"explored,omitempty"`
	Top        []PickCandidate `json:"top,omitempty"`
	FinalUID   string          `json:"final_uid,omitempty"`
}

// PickCandidate Top-N 候选的权重快照（uid8 + 权重 + 成本层）。
type PickCandidate struct {
	UID    string  `json:"uid"`
	Weight float64 `json:"weight"`
	Tier   int     `json:"tier"`
}

// PickExcludingForRealmTrace 同 PickExcludingForRealm，并返回决策链快照（issue #63）。
func (p *Pool) PickExcludingForRealmTrace(tried map[string]bool, reqModel, realm string) (*auth.Auth, PickTrace) {
	trace := PickTrace{Excluded: map[string]int{}}
	acct := p.pickTraceWith(tried, reqModel, realm, &trace)
	return acct, trace
}

// pickTraceWith 选号并收集决策链；trace=nil 时不分配排除原因 map（常规热路径）。
func (p *Pool) pickTraceWith(tried map[string]bool, reqModel, realm string, trace *PickTrace) *auth.Auth {
	var local PickTrace
	if trace == nil {
		trace = &local
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if p.routeCoolingLocked(realm, reqModel, now) {
		trace.addExcluded("route_cooldown")
		return nil
	}
	cands := p.pickCandidates(tried, reqModel, realm, now, trace)
	trace.Candidates = len(cands)
	if len(cands) == 0 {
		// 全冷却兜底：无 healthy 候选时，从冷却账号里选 until 最早到期的一个
		// （熔断/冷却共用 expiry 口径，取较早截止者）。禁用的账号永不参与兜底。
		acct := p.pickEarliestExpiryLocked(tried, now, realm, reqModel)
		if acct != nil {
			trace.FinalUID = logfmt.UID8(acct.UID)
		}
		return acct
	}
	maxCredits := maxCreditsOf(cands)
	bestTier, explored := p.resolveCostTier(cands, reqModel, realm, now)
	trace.CostTier, trace.Explored = bestTier, explored
	ordered, top := p.rankCandidates(cands, reqModel, bestTier, maxCredits, now)
	trace.Top = topTrace(top, reqModel, now, p, maxCredits)
	e := p.selectCandidate(ordered, top, reqModel, now)
	trace.FinalUID = logfmt.UID8(e.a.UID)
	if explored {
		// 探索事件日志（可观测性）：选中号此时才确定，故在选中点打出。
		// 毕业结果由相邻的既有日志闭环（免费号无日志、收费号走 NoteModelCost
		// 常规路径）。
		log.Printf("[pool] cost explore model=%s realm=%q acct=%s window=%s",
			reqModel, realm, logfmt.Label(e.a.UID, e.a.Nickname), p.costExploreInterval)
	}
	e.lastUsed = now // 锁内即时标记：下一个进入 pick 的 goroutine 立即看到本号已用
	p.pickSeq++
	e.usedSeq = p.pickSeq // 单调序号：保证 usedSeq 严格全序（防惊群/LRU 的权威依据）
	return e.a
}

// topTrace 截取 Top-N 候选的权重快照（最多 5 条）。
func topTrace(top []*entry, model string, now time.Time, p *Pool, maxCredits int64) []PickCandidate {
	if len(top) == 0 {
		return nil
	}
	limit := len(top)
	if limit > 5 {
		limit = 5
	}
	out := make([]PickCandidate, 0, limit)
	for _, e := range top[:limit] {
		tier, _ := e.costTier(model, now)
		out = append(out, PickCandidate{
			UID:    logfmt.UID8(e.a.UID),
			Weight: p.routingWeightOf(e, maxCredits, model, now),
			Tier:   tier,
		})
	}
	return out
}

// pickCandidates 收集本次 pick 的 healthy 候选：跳过 tried、惰性清理过期模型级
// 冷却/成本台账、按模型与域判定健康、应用积分保底拦截与在途占满过滤。
// 调用方必须已持有 p.mu 写锁。
func (p *Pool) pickCandidates(tried map[string]bool, reqModel, realm string, now time.Time, trace *PickTrace) []*entry {
	var cands []*entry
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			trace.addExcluded("tried")
			continue
		}
		// 惰性清理过期的模型级冷却与成本台账（两者的 map 都不无限膨胀；
		// status 只读遍历天然跳过过期项，但内存条目必须在此真正删除）。
		e.pruneExpiredModelCooldowns(now)
		e.pruneExpiredModelCosts(now)
		e.pruneExpiredHealth(now)
		if reason := p.excludeReason(e, reqModel, realm, now); reason != "" {
			trace.addExcluded(reason)
			continue
		}
		cands = append(cands, e)
	}
	return cands
}

// excludeReason 返回账号被排除的原因（空 = 入选）；供决策链统计。
func (p *Pool) excludeReason(e *entry, reqModel, realm string, now time.Time) string {
	if realm != "" && e.a.Realm() != realm {
		return "realm"
	}
	if reqModel != "" && e.modelCooled(now, reqModel) {
		return "model_cooldown"
	}
	if !e.healthy(now) {
		return "cooling"
	}
	if p.floorBlockedForRealmModel(e, reqModel, realm, now) {
		return "credit_floor"
	}
	if p.dailyBlocked(e, reqModel, realm, now) {
		return "daily_limit"
	}
	if p.inFlightFull(e) {
		return "in_flight_full"
	}
	return ""
}

// addExcluded 累加一种排除原因（nil/空 trace 安全）。
func (t *PickTrace) addExcluded(reason string) {
	if t == nil || t.Excluded == nil {
		return
	}
	t.Excluded[reason]++
}

// maxCreditsOf 返回候选集中的最大 credits（权重归一基准）。
func maxCreditsOf(cands []*entry) int64 {
	var maxCredits int64
	for _, e := range cands {
		if e.credits > maxCredits {
			maxCredits = e.credits
		}
	}
	return maxCredits
}

// costTier 按该模型的实测扣费把候选分层：
//
//	0 = 已实测免费（限免期/夜间免费的号，最强偏好）
//	1 = 无观测（含观测过期）
//	2 = 已实测收费
//
// "无观测"排在"已实测收费"之前：新号的限免状态只能靠实测发现，若已知收费的号
// 恒压过未知号，那台免费的号永远轮不到，也就永远学不到。
func (e *entry) costTier(model string, now time.Time) (int, float64) {
	mc, ok := e.modelCostOf(model, now)
	if !ok {
		return 1, 0
	}
	if mc.CostPer1k <= 0 {
		return 0, 0
	}
	return 2, mc.CostPer1k
}

// resolveCostTier 计算本次 pick 生效的成本层，并按条件探索（issue #136 方案 a′）
// 决定是否切到 tier 1-only。返回生效层与是否发生探索。
//
// 条件探索：tier 0 垄断层存在（bestTier==0 且 reqModel 非空）且候选含 tier 1
// （冻结存在）且距上次探索 ≥ 窗口（零值 timer=从未探索 → 首次满足即探）时，本次
// pick 生效层切 tier 1-only——探索=搭车改道，把一个既有真实用户请求改道给未知号
// （零新增上游请求；IP 维度零增量，WAF 友好）。timer 同锁写入：并发 pick 串行进入
// 写锁，只有一个进入者能通过窗口判定（天然防重复探索）。key = realm + "\x1f" +
// reqModel：同模型名可跨域，探索节奏按 (域, 模型) 独立；realm==""（Pick 老语义）单独成键。
func (p *Pool) resolveCostTier(cands []*entry, reqModel, realm string, now time.Time) (int, bool) {
	bestTier := 2
	for _, e := range cands {
		if ti, _ := e.costTier(reqModel, now); ti < bestTier {
			bestTier = ti
		}
	}
	if p.costExploreInterval <= 0 || bestTier != 0 || reqModel == "" {
		return bestTier, false
	}
	if !anyTier1(cands, reqModel, now) {
		return bestTier, false
	}
	key := realm + "\x1f" + reqModel
	if now.Sub(p.exploreLast[key]) < p.costExploreInterval {
		return bestTier, false
	}
	p.exploreLast[key] = now
	p.costExploreEvents++
	return 1, true
}

// anyTier1 报告候选中是否存在无观测（tier 1）账号。
func anyTier1(cands []*entry, model string, now time.Time) bool {
	for _, e := range cands {
		if ti, _ := e.costTier(model, now); ti == 1 {
			return true
		}
	}
	return false
}

// rankCandidates 过滤出 bestTier 层的候选并计算权重，排序后返回全量有序列表
// （ordered）与截断的 top5（top）。
//
// 权重只算一次：顶 5 截断要排序，若在 sort 比较器里现算 weightOf 会翻成 O(n log n)
// 次冗余浮点计算。等权重洗牌：仅当存在权重相等且候选数超过 top5 时才对 ws 做
// Fisher-Yates 洗牌（且**不消耗 p.randInt64N 注入源**，避免改变 pickWeighted 的
// 确定性语义），否则按字典序截断会让 uid 靠后的账号永远进不了 top5。
// weightedEntry 权重排序中间态（rankCandidates 用）。
type weightedEntry struct {
	e      *entry
	w      float64
	cost1k float64
}

func (p *Pool) rankCandidates(cands []*entry, reqModel string, bestTier int, maxCredits int64, now time.Time) (ordered, top []*entry) {
	ws := make([]weightedEntry, 0, len(cands))
	for _, e := range cands {
		ti, ci := e.costTier(reqModel, now)
		if ti == bestTier {
			ws = append(ws, weightedEntry{e: e, w: p.routingWeightOf(e, maxCredits, reqModel, now), cost1k: ci})
		}
	}
	if len(ws) > 5 && hasWeightTie(ws) {
		shuf := rand.New(rand.NewPCG(uint64(now.UnixNano()), uint64(len(ws))))
		shuf.Shuffle(len(ws), func(i, j int) { ws[i], ws[j] = ws[j], ws[i] })
	}
	sort.SliceStable(ws, func(i, j int) bool {
		// costTier 硬过滤后 ws 全员同层，但仍按 cost1k 升序排（tier 2 层内单价低者
		// 在前；tier 0/1 层 cost1k 恒 0，本比较退化为权重比较）——读缓存字段不现算。
		if ws[i].cost1k != ws[j].cost1k {
			return ws[i].cost1k < ws[j].cost1k // 收费层：单价低的在前
		}
		if ws[i].w != ws[j].w {
			return ws[i].w > ws[j].w
		}
		return ws[i].e.a.UID < ws[j].e.a.UID // 稳定兜底（洗牌后此项几乎不触发）
	})
	ordered = make([]*entry, len(ws))
	for i, c := range ws {
		ordered[i] = c.e
	}
	if len(ordered) > 5 {
		return ordered, ordered[:5]
	}
	return ordered, ordered
}

// hasWeightTie 报告是否存在与首元素权重并列的候选（存在并列时按字典序截断会饿死
// uid 靠后的等权重账号，故需先洗牌）。
func hasWeightTie(ws []weightedEntry) bool {
	for i := 1; i < len(ws); i++ {
		if ws[i].w == ws[0].w {
			return true
		}
	}
	return false
}

// selectCandidate 从有序候选中选出最终账号：先按 minPickGap 过滤（防并发撞号，
// 同一账号在窗口内不重复被选中），全部刚被用过时在**全候选**里按 usedSeq 取最旧者
// （LRU 兜底，避免 top5 字典序截断把等权重靠后账号饿死），否则在 top5 内加权抽签。
func (p *Pool) selectCandidate(ordered, top []*entry, model string, now time.Time) *entry {
	eligible := make([]*entry, 0, len(top))
	for _, c := range top {
		if now.Sub(c.lastUsed) >= minPickGap {
			eligible = append(eligible, c)
		}
	}
	if len(eligible) == 0 {
		// top5 全部刚被用过：LRU 兜底，在**全候选**里选最旧者。用 usedSeq 单调序号
		// 而非 lastUsed 墙钟比较：Windows 等平台 time.Now() 精度 ~0.5ms，快速连续
		// 选号时所有 lastUsed 完全相等，Before 全 false 会恒选 ordered[0] 导致集中。
		e := ordered[0]
		for _, c := range ordered[1:] {
			if c.usedSeq < e.usedSeq {
				e = c
			}
		}
		return e
	}
	return p.pickWeighted(eligible, model) // eligible 保序 = top5 降序子集
}

// floorBlockedForModel 积分保底拦截判定（pick 普通轮换与 PickByUIDForModel 粘性
// 路径的单一事实来源）：floor>0 且账号触底（credits < floor）且该模型**收费**
// 时为真。
//
// 收费判据两级（任一成立即判收费 → 拦）：
//  1. 本地实测台账（e.modelCostOf）：该号在该模型上实测 cost>0（tier 2）。
//  2. 上游目录倍率（p.modelRateOf）：本地无观测/观测过期（tier 1）时的兜底。
//     只看实测会让「无观测」恒等于「放行」——而高价新模型恰恰全池无观测
//     （kimi-k3-1 实案：x1.62、223 分/百万 token，两笔打穿 100 分的号并触发
//     硬冷却到次日 04:00）。倍率由上游随模型目录下发，请求前即已知，不必付学费。
//
// 不拦的情形：
//   - 模型免费：本地实测 cost<=0（tier 0），或目录倍率为 0/"0.00"。保底的目的
//     正是「留余额给免费模型用」——但若账号已归零，上游仍会 402（余额门禁是
//     账号级的，与模型无关），此时由 ErrHardCredit 冷却承接，与本判定无关。
//   - 模型倍率未知（台账无观测且目录未下发该模型）：无法判收费，按放行处理。
//     这是有意的保守选择——目录未覆盖的模型多为内部/别名模型，拦了会让号
//     永久失联；风险由「未知」本身承担，但已知收费的一律拦。
//   - model 为空（无模型上下文）不拦：无成本维度，floor 无从判收费。
//
// 余额用本地插值口径（签到权威值 - 每笔 usage.credit 实扣，见 NoteModelCost）：
// 只会偏低不会偏高（官方对账延迟方向安全），正是保底需要的安全方向。
// 调用方必须已持有 p.mu（读 e.credits / e.modelCost / p.modelRateOf）。

// floorBlockedForRealmModel 同上，但带 realm 上下文（倍率按 (realm, 模型) 分桶，
// 同名模型在 CN / global 两域倍率可不同）。realm 为空时按倍率表的空域键查。
func (p *Pool) floorBlockedForRealmModel(e *entry, model, realm string, now time.Time) bool {
	if p.creditFloor <= 0 || model == "" || float64(e.credits)-e.reservedCredits >= float64(p.creditFloor) {
		return false
	}
	return p.paidModel(e, model, realm, now)
}

// paidModel 报告模型是否收费：本地实测优先，缺失时用上游目录倍率兜底。
// 未知/空模型按不拦处理（与 credit_floor 的保守口径一致）。
func (p *Pool) paidModel(e *entry, model, realm string, now time.Time) bool {
	if model == "" {
		return false
	}
	// 1) 本地实测台账：最权威（真实扣费证据）。
	if mc, ok := e.modelCostOf(model, now); ok {
		return mc.CostPer1k > 0
	}
	// 2) 上游目录倍率兜底：无实测观测时用牌价判收费，堵住「无观测 = 放行」漏洞。
	if p.modelRateOf == nil {
		return false
	}
	rate := p.modelRateOf(realm, model)
	if rate == "" {
		return false // 目录未覆盖：未知，放行
	}
	v, err := strconv.ParseFloat(rate, 64)
	if err != nil {
		return false // 倍率非数值（异常形态）：不据此惩罚账号
	}
	return v > 0
}

// dailyBlocked 报告账号是否被当日护栏拦下（issue #95）。调用方需已持 p.mu。
func (p *Pool) dailyBlocked(e *entry, model, realm string, now time.Time) bool {
	if p.dailyUsage == nil {
		return false
	}
	return p.dailyBlockedUsage(p.dailyUsage(), e, model, realm, now)
}

// dailyBlockedUsage 用给定当日快照判定：token 达限 → 全模型；积分达限 →
// 仅收费模型；按模型 token 达限 → 仅该模型。
func (p *Pool) dailyBlockedUsage(usage map[string]DailyUsage, e *entry, model, realm string, now time.Time) bool {
	lim := p.dailyLimitsFor(e.a.Realm())
	if !lim.enabled() {
		return false
	}
	u := usage[e.a.UID]
	if lim.Tokens > 0 && u.Tokens >= lim.Tokens {
		return true
	}
	if lim.ModelTokens > 0 && model != "" && u.Models[model] >= lim.ModelTokens {
		return true
	}
	return lim.Credits > 0 && u.Credits >= lim.Credits && p.paidModel(e, model, realm, now)
}

// enabled 报告是否配置了任一每日护栏。
func (l DailyLimits) enabled() bool { return l.Tokens > 0 || l.Credits > 0 || l.ModelTokens > 0 }

// AllDailyBlocked 报告是否存在健康账号但全部被当日护栏拦下（issue #95）。
func (p *Pool) AllDailyBlocked(model, realm string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.dailyUsage == nil {
		return false
	}
	usage := p.dailyUsage()
	now := p.now()
	candidates := 0
	for _, e := range p.byUID {
		if e.disabled || e.paused {
			continue
		}
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		candidates++
		// 冷却/熔断/模型冷却可能在本地 0 点前恢复：此时应回 503 并由
		// Retry-After 指向更早的恢复时间，而不是声称「日限到次日」。
		// 硬冷却（余额耗尽，通常到次日 04:00）晚于日限重置，仍按日限处理。
		if !e.healthyForModel(now, model) {
			if resumesBeforeMidnight(e, now, model) {
				return false
			}
			continue
		}
		if !p.dailyBlockedUsage(usage, e, model, realm, now) {
			return false
		}
	}
	return candidates > 0
}

// DailyLimitedUIDs 返回达到日 token 或日积分上限的账号（面板徽章用）。
func (p *Pool) DailyLimitedUIDs() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.dailyUsage == nil {
		return nil
	}
	usage := p.dailyUsage()
	var out []string
	for uid, e := range p.byUID {
		if dailyLimitHit(p.dailyLimitsFor(e.a.Realm()), usage[uid]) {
			out = append(out, uid)
		}
	}
	sort.Strings(out)
	return out
}

// resumesBeforeMidnight 报告账号/模型冷却是否早于本地次日 0 点恢复。
func resumesBeforeMidnight(e *entry, now time.Time, model string) bool {
	resume := e.until
	if mc, ok := e.modelCooldowns[model]; ok && mc.Until.After(resume) {
		resume = mc.Until
	}
	if !resume.After(now) {
		resume = now
	}
	midnight := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, now.Location())
	return resume.Before(midnight)
}

// dailyLimitHit 报告账号是否命中任一日限额（含账号×模型 token）。
func dailyLimitHit(lim DailyLimits, u DailyUsage) bool {
	if lim.Tokens > 0 && u.Tokens >= lim.Tokens {
		return true
	}
	if lim.Credits > 0 && u.Credits >= lim.Credits {
		return true
	}
	if lim.ModelTokens > 0 {
		for _, tokens := range u.Models {
			if tokens >= lim.ModelTokens {
				return true
			}
		}
	}
	return false
}

// pickEarliestExpiryLocked 全冷却兜底：在非禁用的软冷却/熔断账号中选截止最早的一个。
// 分级：disabled 永不参与；CoolHard（余额耗尽，等签到的号）同样排除——调了必 402，浪费轮换并产生噪音日志；
// CoolSoft 与熔断号允许参与（可能已恢复，失败成本仅一轮换）。
// 被 tried 排除、在途占满的账号同样跳过（维持请求级轮换 + 租约语义）。无任何可用返回 nil。
//
// 积分保底同样在此生效（model 非空时）：floor 把健康号全部拦掉后 cands 为空会走到
// 这里，若兜底不看保底，触底号会被「捞回来」继续接收费模型——表现为同一条
// floor WARN 反复刷同一个号（实测：credits=1 < floor=150 仍持续中选）。
// 兜底是**最后一道**选号路径，保底在它之前挡不住就等于没挡。
func (p *Pool) pickEarliestExpiryLocked(tried map[string]bool, now time.Time, realm, model string) *auth.Auth {
	var best *entry
	for uid, e := range p.byUID {
		if !p.fallbackEligible(e, uid, tried, realm, model, now) {
			continue
		}
		exp := e.expiry(now)
		if exp.IsZero() {
			continue
		}
		if best == nil || exp.Before(best.expiry(now)) {
			best = e
		}
	}
	if best == nil {
		return nil
	}
	log.Printf("WARN: [pool] fallback_earliest_expiry acct=%s until=%s kind=%s",
		logfmt.Label(best.a.UID, best.a.Nickname), best.expiry(now).Format(time.RFC3339), best.fallbackKind(now))
	best.lastUsed = p.now()
	// 单飞探针（issue #116 P0-3）：刚解封账号只允许一个兜底请求先试探，其余并发
	// 在 probeWindow 内直接失败，避免二次踩爆。
	best.probeUntil = now.Add(probeWindow)
	// 兜底同样是「选中」，必须与 pick() 正常路径、粘性命中路径（PickByUIDForModel）
	// 一样推进 usedSeq/pickSeq：否则被兜底反复选中的账号 usedSeq 恒为 0，在 pick 的
	// LRU 兜底（按 usedSeq 取最旧）眼里永远是「最旧」，刚被用过就被立刻再选——
	// 防集中/防惊群失效（entry.usedSeq 契约：每次被选中时取 p.pickSeq 自增值）。
	p.pickSeq++
	best.usedSeq = p.pickSeq
	return best.a
}

// fallbackEligible 判定账号是否可参与全冷却兜底：排除 tried/跨域/禁用暂停/
// 有效 hard 冷却（调了必 402）/积分保底拦截/在途占满。调用方需已持 p.mu。
func (p *Pool) fallbackEligible(e *entry, uid string, tried map[string]bool, realm, model string, now time.Time) bool {
	if tried != nil && tried[uid] {
		return false
	}
	if realm != "" && e.a.Realm() != realm {
		return false // 域过滤：池内跨 realm 的冷却账号不参与本 realm 兜底
	}
	if e.disabled || e.paused {
		return false // 禁用/暂停选号的账号永不参与兜底
	}
	if e.hardCoolingActive(now) {
		return false // 余额耗尽号（处于有效 hard 冷却期）不参与兜底：等签到恢复，调了必 402
	}
	if p.floorBlockedForRealmModel(e, model, realm, now) {
		return false // 积分保底：触底号不接收费模型（兜底路径同判据）
	}
	if p.dailyBlocked(e, model, realm, now) {
		return false // 每日护栏（issue #95）：兜底路径同判据
	}
	return p.fallbackLoadOK(e, now)
}

// fallbackLoadOK 报告兜底候选是否可接收本次探针：不在单飞窗口且未占满在途。
func (p *Pool) fallbackLoadOK(e *entry, now time.Time) bool {
	return !now.Before(e.probeUntil) && !p.inFlightFull(e)
}

// inFlightFull 报告账号是否已占满在途名额（上限按 realm 分档，见 inFlightLimit；
// limit=0 不限 → 恒 false）。调用方需已持 p.mu（读锁或写锁均可，本方法只读上限）。
func (p *Pool) inFlightFull(e *entry) bool {
	limit := p.inFlightLimit(e)
	if limit <= 0 {
		return false
	}
	return e.inFlight.Load() >= int64(limit)
}

// minPickGap 防并发撞号窗口：同一账号在该窗口内不重复被选中（除非 top5 全部刚被用过）。
// 生产默认 100ms；纯加权分布测试可临时置 0 关闭防撞号。
var minPickGap = 100 * time.Millisecond

// pickWeighted 加权随机（claude-api selectWeightedRandom 参考口径）：
//
//		weight = credits 比例 × 10 + idleWeight
//
//	  - credits 比例 = 该号 credits / 候选集内最大 credits（避免量纲爆炸）
//	  - idleWeight = min(距 lastUsed 小时数 × idleWeightPerHour, idleWeightMax)；从未使用给满分
//
// credits 全 0 时仍按 idleWeight 加权（不退化均匀随机）。
// 权重为浮点，用 int64 定点（×1e6）抽签可保持确定性随机源注入（randInt64N 语义不变）。
// 随机源优先用 p.randInt64N（仅供测试注入确定性），nil 时回退 math/rand/v2 全局源。
func (p *Pool) pickWeighted(cands []*entry, model string) *entry {
	now := p.now()
	var maxCredits int64
	for _, e := range cands {
		if e.credits > maxCredits {
			maxCredits = e.credits
		}
	}
	const scale = 1_000_000 // 定点放大：int64 累加权重大整数抽签
	weights := make([]int64, len(cands))
	var total int64
	for i, e := range cands {
		w := p.routingWeightOf(e, maxCredits, model, now)
		weights[i] = int64(w * scale)
		total += weights[i]
	}
	rnd := rand.Int64N
	if p.randInt64N != nil {
		rnd = p.randInt64N
	}
	if total <= 0 {
		return cands[int(rnd(int64(len(cands))))]
	}
	r := rnd(total)
	var acc int64
	for i, e := range cands {
		acc += weights[i]
		if r < acc {
			return e
		}
	}
	return cands[len(cands)-1]
}

// weightOf 计算单个账号的普通加权分值（model 用于叠加该模型上的健康降权因子）。
func (p *Pool) weightOf(e *entry, maxCredits int64, model string, now time.Time) float64 {
	w := 1.0
	// 1. credits 比例 ×10（会计入 mid-credit 锚点，避免全员 0 时 credits 项为 0）。
	if maxCredits > 0 {
		w += float64(e.credits) / float64(maxCredits) * 10
	}
	// 2. 闲置补偿。
	if e.lastUsed.IsZero() {
		w += p.idleWeightMax // 从未使用 → 满分
	} else {
		hours := now.Sub(e.lastUsed).Hours()
		idleW := hours * p.idleWeightPerHour
		if idleW > p.idleWeightMax {
			idleW = p.idleWeightMax
		}
		if idleW < 0 {
			idleW = 0 // lastUsed 在未来（时钟回拨）时钳 0
		}
		w += idleW
	}
	// 3.（原「成功率 ×3」因子已删，对齐上游 success-ema-review：errTotal 是终身
	// 累计、只增不减，成功率 = successCount/(successCount+errTotal) 会让早期出过错
	// 的号被永久压权且永不恢复；瞬时健康信号已由冷却/熔断/连败降权承接。）
	// 4. 实时健康因子（issue #16）：TTFT 慢 / 错误率高的号按模型降权（半衰期衰减，
	// 可恢复）。无观测时因子恒 1，不改变既有权重。
	w *= e.healthFactor(model, now)
	// 5. 并发压力（issue #116 P0-2）：在途越接近上限权重越低，死区内不打折。
	return w * p.pressureFactor(e)
}

// expiringNow 报告账号是否存在当前仍有效的快过期积分批次。
func expiringNow(e *entry, now time.Time) bool {
	return e.creditsExpiring > 0 &&
		e.creditsEarliestRemaining > 0 &&
		!e.creditsEarliestExpiry.IsZero() &&
		e.creditsEarliestExpiry.After(now)
}

// routingWeightOf 在普通账号权重上叠加快过期虚拟实例数量。prefer_expiring=false
// 或账号无有效快过期批次时，实例数恒为 1，结果与旧 weightOf 完全一致。
func (p *Pool) routingWeightOf(e *entry, maxCredits int64, model string, now time.Time) float64 {
	w := p.weightOf(e, maxCredits, model, now)
	if p.preferExpiring && expiringNow(e, now) {
		return w * expiringVirtualSlots
	}
	return w
}

// SetCredits 更新账号余额。
