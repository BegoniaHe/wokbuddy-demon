// strategy.go 按域独立的选号策略（上游同步窗口 issue #205）。
//
// 三档策略（默认 weighted_random = 现状）：
//   - weighted_random    ：既有加权随机（成本分层 + 快过期虚拟权重 + Top5 抽签）；
//   - round_robin        ：按账号顺序严格轮转（每次选中后游标前进一位）；
//   - sequential_exhaust ：按账号顺序「用完一个再下一个」（游标钉在当前号，只有该号
//     不再可服务时才顺延），并**旁路全冷却兜底与防撞号窗口**——有序策略的语义就是
//     「明确地用下一个号」，兜底抽号会破坏这个语义。
//
// 游标按 (realm, model) 维度持久化：同一域不同模型的冷却/日限互不影响，游标也应分开。
// 切换策略时清空该域游标（旧游标对新策略没有意义）。
package pool

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/logfmt"
)

// errUnknownStrategy 未知策略值（config 校验据此拒绝启动）。
var errUnknownStrategy = errors.New("未知选号策略")

// Strategy 选号策略枚举（字符串入库/入配置，小写枚举、大小写不敏感解析）。
type Strategy string

const (
	StrategyWeightedRandom    Strategy = "weighted_random"
	StrategyRoundRobin        Strategy = "round_robin"
	StrategySequentialExhaust Strategy = "sequential_exhaust"
)

// ParseStrategy 归一策略字符串（大小写不敏感、去首尾空白）：空 = 默认
// weighted_random；只接受三个枚举值，拼错/未知值返回错误由 config 拒启动
// （静默退回默认会让运维以为配置生效了）。域口径统一用 cn / global，
// 不接受 domestic / international 之类的别名。
func ParseStrategy(raw string) (Strategy, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", string(StrategyWeightedRandom):
		return StrategyWeightedRandom, nil
	case string(StrategyRoundRobin):
		return StrategyRoundRobin, nil
	case string(StrategySequentialExhaust):
		return StrategySequentialExhaust, nil
	default:
		return "", fmt.Errorf("%w: %q（可选 weighted_random / round_robin / sequential_exhaust）",
			errUnknownStrategy, raw)
	}
}

// Scheduling 按域的调度设置（realm → 策略 / 账号顺序）。
type Scheduling struct {
	Strategy map[string]Strategy
	Order    map[string][]string
}

// SetScheduling 注入调度设置（main 从 config 解析后调用，热生效）。
// 策略发生变化的域会清空该域游标：旧策略留下的位置对新策略没有意义。
func (p *Pool) SetScheduling(sched Scheduling) {
	p.mu.Lock()
	defer p.mu.Unlock()

	old := p.schedulingStrategy

	p.schedulingStrategy = make(map[string]Strategy, len(sched.Strategy))

	p.schedulingOrder = make(map[string][]string, len(sched.Order))
	maps.Copy(p.schedulingStrategy, sched.Strategy)

	for realm, order := range sched.Order {
		kept := make([]string, 0, len(order))
		for _, uid := range order {
			if trimmed := strings.TrimSpace(uid); trimmed != "" {
				kept = append(kept, trimmed)
			}
		}

		p.schedulingOrder[realm] = kept
	}

	// 策略**实际变化**的域清空游标。首次配置（旧值为空）不清：启动装配发生在
	// state.json 加载之后，若这里清空会把刚恢复的游标抹掉，顺序耗尽进度每次重启归零。
	for realm, strategy := range p.schedulingStrategy {
		if prev, had := old[realm]; had && prev != strategy {
			p.clearCursorsForRealmLocked(realm)
		}
	}

	for realm := range old {
		if _, kept := p.schedulingStrategy[realm]; !kept {
			p.clearCursorsForRealmLocked(realm)
		}
	}
}

// clearCursorsForRealmLocked 清掉某域全部 (realm, model) 游标（调用方已持锁）。
func (p *Pool) clearCursorsForRealmLocked(realm string) {
	prefix := realm + ":"
	cleared := false

	for key := range p.schedulingCursors {
		if strings.HasPrefix(key, prefix) {
			delete(p.schedulingCursors, key)

			cleared = true
		}
	}

	if cleared {
		// 游标属于持久化状态：清空也要置脏，否则旧游标会留在 state.json 里，
		// 进程重启后又被恢复（切策略=清游标的语义丢失）。
		p.dirty.Store(true)
	}
}

// strategyForLocked 该域当前策略（缺省 weighted_random；调用方已持锁）。
func (p *Pool) strategyForLocked(realm string) Strategy {
	if strategy, ok := p.schedulingStrategy[realm]; ok && strategy != "" {
		return strategy
	}

	return StrategyWeightedRandom
}

// StrategyForRealm 该域的选号策略（对外只读，供 handler 决定会话粘性解析路径）。
func (p *Pool) StrategyForRealm(realm string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	return string(p.strategyForLocked(realm))
}

// orderedUIDsLocked 该域的账号顺序：配置顺序优先（不存在/异域/重复的 uid 自动跳过），
// 其余账号按 uid 升序追加——保证「配了几个」与「池里还有谁」都能被轮询覆盖。
func (p *Pool) orderedUIDsLocked(realm string) []string {
	seen := make(map[string]bool, len(p.byUID))

	out := make([]string, 0, len(p.byUID))

	for _, uid := range p.schedulingOrder[realm] {
		if seen[uid] {
			continue
		}

		if entry2, ok := p.byUID[uid]; ok && (realm == "" || entry2.a.Realm() == realm) {
			seen[uid] = true

			out = append(out, uid)
		}
	}

	rest := make([]string, 0, len(p.byUID))

	for uid, entry2 := range p.byUID {
		if seen[uid] || (realm != "" && entry2.a.Realm() != realm) {
			continue
		}

		rest = append(rest, uid)
	}

	slices.Sort(rest)

	return append(out, rest...)
}

// orderedCandidateLocked 从 start 处顺延找顺序里第一个「可服务且落在 bestTier」
// 的账号（返回账号与其在 order 里的下标）。servable 由 pickCandidates 提供，
// 已排除 tried/健康/在途/保底；本函数只再过滤成本层。
func (*Pool) orderedCandidateLocked(
	order []string, servable map[string]*entry, start int, reqModel string,
	now time.Time, bestTier int, trace *PickTrace,
) (*entry, int) {
	for offset := range order {
		idx := (start + offset) % len(order)

		entry2, ok := servable[order[idx]]
		if !ok {
			continue
		}

		if tier, _ := entry2.costTier(reqModel, now); tier != bestTier {
			trace.addExcluded("cost_tier")

			continue
		}

		return entry2, idx
	}

	return nil, 0
}

// pickOrderedLocked 有序策略选号（调用方已持锁）。
//
// 入参 cands 是已完成健康/保底/在途/tried 过滤的候选；本函数再按成本层过滤到
// bestTier（与加权随机同一分层口径，见 resolveCostTier），只在同层内按账号顺序
// 选号——顺序策略不应把已实测收费的号排在免费号之前。
//
// 语义：
//   - 从 (realm, model) 游标处开始顺延，取最优成本层里第一个账号；
//   - round_robin：选中后游标 = 下一位置（严格轮转）；
//   - sequential_exhaust：游标钉在选中账号（该号不可服务或成本层变化时才顺延）；
//   - 有序策略**不**走全冷却兜底（候选为空就返回 nil，由上层按 no_healthy 处理）。
func (p *Pool) pickOrderedLocked(
	cands []*entry, reqModel, realm string, now time.Time, trace *PickTrace, strategy Strategy, bestTier int,
) *entry {
	order := p.orderedUIDsLocked(realm)
	if len(order) == 0 {
		trace.addExcluded("no_account")

		return nil
	}

	servable := make(map[string]*entry, len(cands))
	for _, entry2 := range cands {
		servable[entry2.a.UID] = entry2
	}

	key := realm + ":" + reqModel

	start := p.schedulingCursors[key]
	if start < 0 || start >= len(order) {
		start = 0
	}

	chosen, chosenIdx := p.orderedCandidateLocked(order, servable, start, reqModel, now, bestTier, trace)
	if chosen == nil {
		return nil
	}

	if p.schedulingCursors == nil {
		p.schedulingCursors = map[string]int{}
	}

	if strategy == StrategyRoundRobin {
		p.schedulingCursors[key] = (chosenIdx + 1) % len(order)
	} else {
		p.schedulingCursors[key] = chosenIdx
	}
	// 游标属于持久化状态：选号本身不写 lastUsed（运行态），必须显式置脏，
	// 否则连续请求之间没有其它状态变更时游标会一直停留在内存里。
	p.dirty.Store(true)

	chosen.lastUsed = now
	p.pickSeq++
	chosen.usedSeq = p.pickSeq

	// 与加权随机路径同口径：决策链只记 uid8（完整 uid 只留在账号池/凭证侧）。
	trace.FinalUID = logfmt.UID8(chosen.a.UID)

	return chosen
}
