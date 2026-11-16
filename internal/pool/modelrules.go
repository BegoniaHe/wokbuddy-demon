// 模型规则（issue #569）：按账号批量挂模型黑白名单。
//
// 号池原本只能对账号整体处置（disabled / paused / 分组），模型维度只有自动的模型级
// 冷却（6004 / 11136 / 11141）。运维缺少「昂贵模型只给少数账号、其余账号只跑便宜
// 常用模型」这类手动、批量、跨账号的治理手段——只能逐个账号逐条点禁用。
//
// 判据（与参考实现同语义）：
//   - 一条规则 = 命名的一组模型 + allow / deny 模式；挂到账号上即刻影响选号。
//   - 多条 deny 取并集、多条 allow 取并集，deny 优先。
//   - allow 规则在账号所属域一个模型都没命中时视作没挂（跨域误挂不静默封死账号）。
//   - 模型名可带 cn: / global: 前缀区分域，裸名两域都命中；运行态展开成裸名集合，
//     选号链路（裸模型名）直接命中。
//   - 规则本体与「账号 → 规则 id」引用同存 state.json：加载时先账号后规则本体，
//     展开只改运行态，不改写引用（否则引用会被当成脏数据清空）。
package pool

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"slices"
	"strings"
)

// 规则相关的对外错误。
var (
	// ErrRuleNotFound 引用的规则 id 不存在。
	ErrRuleNotFound = errors.New("model rule not found")
	// ErrRuleInvalid 规则内容不合法（模式/模型集合）。
	ErrRuleInvalid = errors.New("model rule invalid")
	// ErrAccountNotFound 账号不存在（挂载/摘除规则时）。
	ErrAccountNotFound = errors.New("account not found")
)

// ModelRuleMode 规则模式：allow 只放行列出的模型，deny 排除列出的模型。
type ModelRuleMode string

const (
	// ModelRuleAllow 白名单：账号只跑列出的模型（deny 仍优先）。
	ModelRuleAllow ModelRuleMode = "allow"
	// ModelRuleDeny 黑名单：账号不跑列出的模型。
	ModelRuleDeny ModelRuleMode = "deny"
)

// 规则展开用的域标识（与 auth.Auth.Realm() 的返回值一致）。
const (
	realmCN     = "cn"
	realmGlobal = "global"
)

// ModelRule 一条模型规则：命名的模型集合 + allow / deny 模式。
type ModelRule struct {
	ID     string        `json:"id"`
	Name   string        `json:"name,omitempty,omitzero"`
	Mode   ModelRuleMode `json:"mode"`
	Models []string      `json:"models"` // 可带 cn:/global: 前缀；裸名两域都命中
}

// RuleBlockStatus 本域某模型被模型规则挡住的情况（供 400 model_not_allowed 判定）。
type RuleBlockStatus struct {
	// Eligible 本域账号级可选（healthy）的账号数。
	Eligible int `json:"eligible"`
	// Blocked 其中被模型规则挡住该模型的账号数。
	Blocked int `json:"blocked"`
}

// AllBlocked 报告本域所有可选账号是否都被规则挡住该模型。
func (s RuleBlockStatus) AllBlocked() bool {
	return s.Eligible > 0 && s.Blocked >= s.Eligible
}

// ruleBlockedModel 报告账号对该模型是否被模型规则挡住（realm 已在展开时算好）。
func (e *entry) ruleBlockedModel(model string) bool {
	if model == "" || (len(e.ruleDenied) == 0 && e.ruleAllowed == nil) {
		return false
	}

	if _, blocked := e.ruleDenied[model]; blocked {
		return true
	}

	if e.ruleAllowed == nil {
		return false
	}

	_, allowed := e.ruleAllowed[model]

	return !allowed
}

// Rules 返回当前全部规则（按名称、id 稳定排序）。
func (p *Pool) Rules() []ModelRule {
	p.mu.RLock()
	defer p.mu.RUnlock()

	return p.rulesLocked()
}

func (p *Pool) rulesLocked() []ModelRule {
	out := make([]ModelRule, 0, len(p.modelRules))
	for _, rule := range p.modelRules {
		rule.Models = slices.Clone(rule.Models)
		out = append(out, rule)
	}

	slices.SortFunc(out, func(a, b ModelRule) int {
		if c := strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); c != 0 {
			return c
		}

		return strings.Compare(a.ID, b.ID)
	})

	return out
}

// UpsertRule 新增或更新规则；更新后立即重算所有挂载账号的展开（下一请求即生效）。
// ID 为空时生成新 id；name 为空时回落 id。
func (p *Pool) UpsertRule(rule ModelRule) (ModelRule, error) {
	if strings.TrimSpace(rule.ID) == "" {
		rule.ID = newRuleID()
	}

	normalized, err := normalizeModelRule(rule)
	if err != nil {
		return ModelRule{}, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.modelRules == nil {
		p.modelRules = map[string]ModelRule{}
	}

	p.modelRules[normalized.ID] = normalized
	p.reapplyRulesFor(normalized.ID)
	p.dirty.Store(true)

	return normalized, nil
}

// DeleteRule 删除规则，并从所有账号的引用中摘除（不留悬挂 id）。
func (p *Pool) DeleteRule(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	if _, ok := p.modelRules[id]; !ok {
		return false
	}

	delete(p.modelRules, id)

	for _, entry2 := range p.byUID {
		if idx := slices.Index(entry2.ruleIDs, id); idx >= 0 {
			entry2.ruleIDs = slices.Delete(entry2.ruleIDs, idx, idx+1)
		}

		p.applyRulesLocked(entry2)
	}

	p.dirty.Store(true)

	return true
}

// AssignRules 整体替换账号挂载的规则引用；未知 id 直接报错（不写悬挂引用）。
func (p *Pool) AssignRules(uid string, ids []string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	entry2, ok := p.byUID[uid]
	if !ok {
		return ErrAccountNotFound
	}

	clean := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, known := p.modelRules[id]; !known {
			return ErrRuleNotFound
		}

		if !slices.Contains(clean, id) {
			clean = append(clean, id)
		}
	}

	entry2.ruleIDs = clean
	p.applyRulesLocked(entry2)
	p.dirty.Store(true)

	return nil
}

// AttachRules 给账号批量挂规则（并集，幂等）。
func (p *Pool) AttachRules(uid string, ids []string) error {
	return p.mutateAssignments([]string{uid}, ids, assignmentAttach)
}

// DetachRules 从账号摘除规则（幂等）。
func (p *Pool) DetachRules(uid string, ids []string) error {
	return p.mutateAssignments([]string{uid}, ids, assignmentDetach)
}

// AttachRulesForAccounts / DetachRulesForAccounts 批量挂载 / 摘除（面板「批量挂/摘」）。
func (p *Pool) AttachRulesForAccounts(uids, ids []string) error {
	return p.mutateAssignments(uids, ids, assignmentAttach)
}

// DetachRulesForAccounts 批量摘除规则。
func (p *Pool) DetachRulesForAccounts(uids, ids []string) error {
	return p.mutateAssignments(uids, ids, assignmentDetach)
}

type assignmentAction int

const (
	assignmentAttach assignmentAction = iota
	assignmentDetach
)

// mutateAssignments 批量增删引用；任一 uid 不存在或规则未知时整体不落（避免半套配置）。
func (p *Pool) mutateAssignments(uids, ids []string, action assignmentAction) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	entries, err := p.resolveMutationLocked(uids, ids)
	if err != nil {
		return err
	}

	for _, entry2 := range entries {
		p.applyAssignmentLocked(entry2, ids, action)
	}

	p.dirty.Store(true)

	return nil
}

// resolveMutationLocked 校验规则 id 与账号都存在，返回待变更的账号。调用方必须已持有 p.mu。
func (p *Pool) resolveMutationLocked(uids, ids []string) ([]*entry, error) {
	for _, id := range ids {
		if _, known := p.modelRules[id]; !known {
			return nil, ErrRuleNotFound
		}
	}

	entries := make([]*entry, 0, len(uids))

	for _, uid := range uids {
		entry2, ok := p.byUID[uid]
		if !ok {
			return nil, ErrAccountNotFound
		}

		entries = append(entries, entry2)
	}

	return entries, nil
}

// applyAssignmentLocked 按动作增删引用并重算展开。调用方必须已持有 p.mu。
func (p *Pool) applyAssignmentLocked(entry2 *entry, ids []string, action assignmentAction) {
	switch action {
	case assignmentAttach:
		for _, id := range ids {
			if !slices.Contains(entry2.ruleIDs, id) {
				entry2.ruleIDs = append(entry2.ruleIDs, id)
			}
		}
	case assignmentDetach:
		entry2.ruleIDs = slices.DeleteFunc(entry2.ruleIDs, func(id string) bool {
			return slices.Contains(ids, id)
		})
	}

	p.applyRulesLocked(entry2)
}

// RuleAssignments 返回 uid → 规则 id（只含非空引用；按 id 排序）。
func (p *Pool) RuleAssignments() map[string][]string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	out := make(map[string][]string, len(p.byUID))
	for uid, entry2 := range p.byUID {
		if len(entry2.ruleIDs) == 0 {
			continue
		}

		ids := slices.Clone(entry2.ruleIDs)
		slices.Sort(ids)
		out[uid] = ids
	}

	return out
}

// RuleAccountCounts 返回 rule id → 挂载账号数。
func (p *Pool) RuleAccountCounts() map[string]int {
	p.mu.RLock()
	defer p.mu.RUnlock()

	out := make(map[string]int, len(p.modelRules))
	for _, entry2 := range p.byUID {
		for _, id := range entry2.ruleIDs {
			out[id]++
		}
	}

	return out
}

// RuleBlockedForRealm 统计本域某模型被规则挡住的情况：Eligible 是账号级可选的账号数，
// Blocked 是其中被规则挡住该模型的账号数。
func (p *Pool) RuleBlockedForRealm(model, realm string) RuleBlockStatus {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if model == "" {
		return RuleBlockStatus{}
	}

	now := p.now()

	var status RuleBlockStatus

	for _, entry2 := range p.byUID {
		if realm != "" && entry2.a.Realm() != realm {
			continue
		}

		if !entry2.healthy(now) {
			continue
		}

		status.Eligible++

		if entry2.ruleBlockedModel(model) {
			status.Blocked++
		}
	}

	return status
}

// applyRulesLocked 重算账号的规则展开（引用不变）。调用方必须已持有 p.mu。
func (p *Pool) applyRulesLocked(entry2 *entry) {
	if len(entry2.ruleIDs) == 0 {
		entry2.ruleDenied = nil
		entry2.ruleAllowed = nil

		return
	}

	expanded := p.expandRulesLocked(entry2.ruleIDs, ruleRealm(entry2))

	entry2.ruleDenied = expanded.denied
	entry2.ruleAllowed = nil

	if expanded.hasAllow {
		entry2.ruleAllowed = expanded.allowed
	}
}

// ruleExpansion 账号的规则展开结果：裸模型名集合 + 是否挂了 allow 规则。
type ruleExpansion struct {
	denied   map[string]struct{}
	allowed  map[string]struct{}
	hasAllow bool
}

// ruleRealm 取账号所属域，未知域按 cn 处理（与上游默认路由一致）。
func ruleRealm(entry2 *entry) string {
	if realm := entry2.a.Realm(); realm != "" {
		return realm
	}

	return realmCN
}

// expandRulesLocked 把账号挂载的规则按域展开成裸模型名集合：deny 取并集，
// allow 取并集；本域空命中的规则视作没挂。
func (p *Pool) expandRulesLocked(ids []string, realm string) ruleExpansion {
	expandedRules := ruleExpansion{
		denied:  map[string]struct{}{},
		allowed: map[string]struct{}{},
	}

	for _, id := range ids {
		rule, ok := p.modelRules[id]
		if !ok {
			continue
		}

		models := expandRuleModels(rule.Models, realm)
		if len(models) == 0 {
			continue // 本域一个模型都没命中：视作没挂（跨域误挂不封号）
		}

		switch rule.Mode {
		case ModelRuleDeny:
			for model := range models {
				expandedRules.denied[model] = struct{}{}
			}
		case ModelRuleAllow:
			expandedRules.hasAllow = true

			for model := range models {
				expandedRules.allowed[model] = struct{}{}
			}
		}
	}

	return expandedRules
}

// reapplyRulesFor 规则内容变更后重算所有挂载它的账号。调用方必须已持有 p.mu。
func (p *Pool) reapplyRulesFor(ruleID string) {
	for _, entry2 := range p.byUID {
		if slices.Contains(entry2.ruleIDs, ruleID) {
			p.applyRulesLocked(entry2)
		}
	}
}

// applyModelRulesLocked 恢复规则本体并展开账号引用；必须在账号恢复之后调用
// （账号上只有 id 引用，规则本体的先后顺序不能反）。
func (p *Pool) applyModelRulesLocked(rules []ModelRule) {
	p.modelRules = make(map[string]ModelRule, len(rules))

	for _, raw := range rules {
		normalized, err := normalizeModelRule(raw)
		if err != nil || normalized.ID == "" {
			// 损坏条目跳过（不因一条坏规则拒绝启动，也不复活脏引用）。
			continue
		}

		p.modelRules[normalized.ID] = normalized
	}

	for _, entry2 := range p.byUID {
		p.applyRulesLocked(entry2)
	}
}

// expandRuleModels 按域把规则里的模型名展开成裸名集合：前缀名只命中该域，
// 裸名两域都命中。
func expandRuleModels(models []string, realm string) map[string]struct{} {
	out := make(map[string]struct{}, len(models))

	for _, raw := range models {
		domain, name := splitRuleModel(raw)
		if name == "" {
			continue
		}

		if domain != "" && domain != realm {
			continue
		}

		out[name] = struct{}{}
	}

	return out
}

// splitRuleModel 拆出 "cn:" / "global:" 前缀与裸模型名。未知前缀按裸名处理
// （模型名本身可能带冒号）。
func splitRuleModel(raw string) (domain, name string) {
	trimmed := strings.TrimSpace(raw)

	for _, realm := range []string{realmCN, realmGlobal} {
		rest, found := strings.CutPrefix(trimmed, realm+":")
		if found {
			return realm, strings.TrimSpace(rest)
		}
	}

	return "", trimmed
}

// normalizeModelRule 校验并归一化规则：模式合法、至少一个模型；模型名去空白、
// 去重且保持顺序；name 为空回落 id。
func normalizeModelRule(rule ModelRule) (ModelRule, error) {
	rule.ID = strings.TrimSpace(rule.ID)
	rule.Name = strings.TrimSpace(rule.Name)

	switch rule.Mode {
	case ModelRuleAllow, ModelRuleDeny:
	default:
		return ModelRule{}, ErrRuleInvalid
	}

	models := make([]string, 0, len(rule.Models))
	seen := make(map[string]struct{}, len(rule.Models))

	for _, raw := range rule.Models {
		domain, name := splitRuleModel(raw)
		if name == "" {
			continue
		}

		normalized := name
		if domain != "" {
			normalized = domain + ":" + name
		}

		if _, dup := seen[normalized]; dup {
			continue
		}

		seen[normalized] = struct{}{}
		models = append(models, normalized)
	}

	if len(models) == 0 {
		return ModelRule{}, ErrRuleInvalid
	}

	rule.Models = models

	if rule.Name == "" {
		rule.Name = rule.ID
	}

	return rule, nil
}

// newRuleID 生成规则 id（随机、可读前缀、不含用户输入）。
func newRuleID() string {
	raw := make([]byte, 6)
	_, _ = rand.Read(raw)

	return "rule-" + base64.RawURLEncoding.EncodeToString(raw)
}
