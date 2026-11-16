// modelrules_test.go 钉住模型规则语义（issue #569）：deny / allow / 两者叠加
// （deny 优先）/ cn: global: 前缀隔离 / 跨域 allow 空命中不封号 / 删除后引用同步
// 摘除 / 重启后规则与引用都在。
package pool

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// rulePool 构造 cn/global 两个账号的池（域由 domain 推断）。
func rulePool() *Pool {
	state := New("")
	state.Add(&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn"})
	state.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"})

	return state
}

// blockedFor 报告账号对该模型当前是否被规则挡住（等价 healthyForModel 的规则段）。
func blockedFor(t *testing.T, state *Pool, uid, model string) bool {
	t.Helper()

	state.mu.RLock()
	defer state.mu.RUnlock()

	entry2, ok := state.byUID[uid]
	if !ok {
		t.Fatalf("账号 %s 不存在", uid)
	}

	return entry2.ruleBlockedModel(model)
}

// TestModelRuleDeny 黑名单：命中模型不可选，未命中模型照常。
func TestModelRuleDeny(t *testing.T) {
	t.Parallel()

	state := rulePool()

	rule, err := state.UpsertRule(ModelRule{Name: "高价模型", Mode: ModelRuleDeny, Models: []string{"glm-5.2"}})
	if err != nil {
		t.Fatal(err)
	}

	if err := state.AssignRules("cn1", []string{rule.ID}); err != nil {
		t.Fatal(err)
	}

	if !blockedFor(t, state, "cn1", "glm-5.2") {
		t.Error("deny 命中的模型应被挡住")
	}

	if blockedFor(t, state, "cn1", "glm-4.6") {
		t.Error("deny 未命中的模型不应被挡住")
	}

	if blockedFor(t, state, "g1", "glm-5.2") {
		t.Error("未挂规则的账号不受影响")
	}
}

// TestModelRuleAllowOnlyListed 白名单：只放行列出的模型。
func TestModelRuleAllowOnlyListed(t *testing.T) {
	t.Parallel()

	state := rulePool()

	rule, err := state.UpsertRule(ModelRule{Name: "只跑便宜模型", Mode: ModelRuleAllow, Models: []string{"glm-4.6"}})
	if err != nil {
		t.Fatal(err)
	}

	if err := state.AssignRules("cn1", []string{rule.ID}); err != nil {
		t.Fatal(err)
	}

	if !blockedFor(t, state, "cn1", "glm-5.2") {
		t.Error("不在白名单的模型应被挡住")
	}

	if blockedFor(t, state, "cn1", "glm-4.6") {
		t.Error("白名单内的模型应放行")
	}
}

// TestModelRuleDenyWinsOverAllow deny 优先：同时命中时判阻挡。
func TestModelRuleDenyWinsOverAllow(t *testing.T) {
	t.Parallel()

	state := rulePool()

	allow, err := state.UpsertRule(ModelRule{Name: "白", Mode: ModelRuleAllow, Models: []string{"glm-4.6", "glm-5.2"}})
	if err != nil {
		t.Fatal(err)
	}

	deny, err := state.UpsertRule(ModelRule{Name: "黑", Mode: ModelRuleDeny, Models: []string{"glm-5.2"}})
	if err != nil {
		t.Fatal(err)
	}

	if err := state.AssignRules("cn1", []string{allow.ID, deny.ID}); err != nil {
		t.Fatal(err)
	}

	if !blockedFor(t, state, "cn1", "glm-5.2") {
		t.Error("deny 应压过 allow")
	}

	if blockedFor(t, state, "cn1", "glm-4.6") {
		t.Error("allow 命中且 deny 未命中的模型应放行")
	}
}

// TestModelRuleDomainPrefixIsolation cn: / global: 前缀只命中本域；裸名两域都命中。
func TestModelRuleDomainPrefixIsolation(t *testing.T) {
	t.Parallel()

	state := rulePool()

	rule, err := state.UpsertRule(ModelRule{
		Mode:   ModelRuleDeny,
		Models: []string{"cn:glm-5.2", "global:kimi-k3"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := state.AssignRules("cn1", []string{rule.ID}); err != nil {
		t.Fatal(err)
	}

	if err := state.AssignRules("g1", []string{rule.ID}); err != nil {
		t.Fatal(err)
	}

	// cn 号：cn: 前缀命中，global: 前缀不命中。
	if !blockedFor(t, state, "cn1", "glm-5.2") {
		t.Error("cn 号应被 cn:glm-5.2 挡住")
	}

	if blockedFor(t, state, "cn1", "kimi-k3") {
		t.Error("cn 号不应被 global: 前缀命中")
	}

	// global 号：同理反向。
	if !blockedFor(t, state, "g1", "kimi-k3") {
		t.Error("global 号应被 global:kimi-k3 挡住")
	}

	if blockedFor(t, state, "g1", "glm-5.2") {
		t.Error("global 号不应被 cn: 前缀命中")
	}
}

// TestModelRuleBareNameHitsBothRealms 裸名两域都命中。
func TestModelRuleBareNameHitsBothRealms(t *testing.T) {
	t.Parallel()

	state := rulePool()

	rule, err := state.UpsertRule(ModelRule{Mode: ModelRuleDeny, Models: []string{"glm-5.2"}})
	if err != nil {
		t.Fatal(err)
	}

	if err := state.AssignRules("cn1", []string{rule.ID}); err != nil {
		t.Fatal(err)
	}

	if err := state.AssignRules("g1", []string{rule.ID}); err != nil {
		t.Fatal(err)
	}

	if !blockedFor(t, state, "cn1", "glm-5.2") || !blockedFor(t, state, "g1", "glm-5.2") {
		t.Error("裸名应同时命中两个域")
	}
}

// TestModelRuleCrossDomainAllowIgnored allow 规则在本域一个模型都没命中时视作没挂：
// 跨域误挂不能把账号静默封死。
func TestModelRuleCrossDomainAllowIgnored(t *testing.T) {
	t.Parallel()

	state := rulePool()
	// 只列 global 模型的 allow 挂到 cn 号上：cn 域展开为空 → 不约束。
	rule, err := state.UpsertRule(ModelRule{Mode: ModelRuleAllow, Models: []string{"global:kimi-k3"}})
	if err != nil {
		t.Fatal(err)
	}

	if err := state.AssignRules("cn1", []string{rule.ID}); err != nil {
		t.Fatal(err)
	}

	if blockedFor(t, state, "cn1", "glm-5.2") {
		t.Error("跨域 allow 空命中不应把账号封死")
	}
}

// TestModelRuleDeleteRemovesReferences 删除规则后所有账号引用同步摘除。
func TestModelRuleDeleteRemovesReferences(t *testing.T) {
	t.Parallel()

	state := rulePool()

	rule, err := state.UpsertRule(ModelRule{Mode: ModelRuleDeny, Models: []string{"glm-5.2"}})
	if err != nil {
		t.Fatal(err)
	}

	if err := state.AssignRules("cn1", []string{rule.ID}); err != nil {
		t.Fatal(err)
	}

	if !state.DeleteRule(rule.ID) {
		t.Fatal("删除应成功")
	}

	if got := state.RuleAssignments(); len(got) != 0 {
		t.Errorf("删除后引用应摘除: %v", got)
	}

	if blockedFor(t, state, "cn1", "glm-5.2") {
		t.Error("删除规则后不应再挡模型")
	}
}

// TestModelRuleAssignRejectsUnknown 未知规则 id / 未知账号直接报错（不留悬挂引用）。
func TestModelRuleAssignRejectsUnknown(t *testing.T) {
	t.Parallel()

	state := rulePool()

	if err := state.AssignRules("cn1", []string{"rule-nope"}); !errors.Is(err, ErrRuleNotFound) {
		t.Errorf("未知规则 id 应报 ErrRuleNotFound, got %v", err)
	}

	if err := state.AssignRules("nope", nil); !errors.Is(err, ErrAccountNotFound) {
		t.Errorf("未知账号应报 ErrAccountNotFound, got %v", err)
	}
}

// TestModelRuleUpsertAppliesToAttached 改规则内容后，挂着它的账号下一请求即生效。
func TestModelRuleUpsertAppliesToAttached(t *testing.T) {
	t.Parallel()

	state := rulePool()

	rule, err := state.UpsertRule(ModelRule{Mode: ModelRuleDeny, Models: []string{"glm-4.6"}})
	if err != nil {
		t.Fatal(err)
	}

	if err := state.AssignRules("cn1", []string{rule.ID}); err != nil {
		t.Fatal(err)
	}

	if blockedFor(t, state, "cn1", "glm-5.2") {
		t.Fatal("前置：glm-5.2 不应被挡")
	}

	if _, err := state.UpsertRule(ModelRule{ID: rule.ID, Mode: ModelRuleDeny, Models: []string{"glm-5.2"}}); err != nil {
		t.Fatal(err)
	}

	if !blockedFor(t, state, "cn1", "glm-5.2") {
		t.Error("改规则内容后应立刻生效")
	}
}

// TestModelRulesPersistRoundTrip 规则本体与账号引用跨重启保留，展开结果一致。
func TestModelRulesPersistRoundTrip(t *testing.T) {
	t.Parallel()

	statePath := filepath.Join(t.TempDir(), "state.json")

	first := New(statePath)
	first.Add(&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn"})

	rule, err := first.UpsertRule(ModelRule{Name: "只在便宜模型上跑", Mode: ModelRuleAllow, Models: []string{"glm-4.6"}})
	if err != nil {
		t.Fatal(err)
	}

	if err := first.AssignRules("cn1", []string{rule.ID}); err != nil {
		t.Fatal(err)
	}

	first.Flush()
	first.Close()

	second := New(statePath)
	t.Cleanup(second.Close)
	second.Add(&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn"})

	rules := second.Rules()
	if len(rules) != 1 || rules[0].ID != rule.ID || rules[0].Mode != ModelRuleAllow {
		t.Fatalf("重启后规则丢失: %+v", rules)
	}

	assignments := second.RuleAssignments()
	if len(assignments["cn1"]) != 1 || assignments["cn1"][0] != rule.ID {
		t.Fatalf("重启后引用丢失: %v", assignments)
	}

	if !blockedFor(t, second, "cn1", "glm-5.2") {
		t.Error("重启后 allow 展开应恢复（glm-5.2 被挡）")
	}

	if blockedFor(t, second, "cn1", "glm-4.6") {
		t.Error("重启后 allow 展开应恢复（glm-4.6 放行）")
	}
}

// TestRuleBlockedForRealm 统计本域「账号级可选」与被规则挡住的账号数。
func TestRuleBlockedForRealm(t *testing.T) {
	t.Parallel()

	state := rulePool()

	rule, err := state.UpsertRule(ModelRule{Mode: ModelRuleDeny, Models: []string{"glm-5.2"}})
	if err != nil {
		t.Fatal(err)
	}

	if err := state.AssignRules("cn1", []string{rule.ID}); err != nil {
		t.Fatal(err)
	}

	status := state.RuleBlockedForRealm("glm-5.2", "cn")
	if status.Eligible != 1 || status.Blocked != 1 || !status.AllBlocked() {
		t.Errorf("cn 域应完全被挡: %+v", status)
	}

	// 未被挡的模型、另一域都不算「全挡」。
	if got := state.RuleBlockedForRealm("glm-4.6", "cn"); got.Blocked != 0 || got.AllBlocked() {
		t.Errorf("未命中的模型不应被挡: %+v", got)
	}

	if got := state.RuleBlockedForRealm("glm-5.2", "global"); got.Blocked != 0 || got.AllBlocked() {
		t.Errorf("global 域不受 cn 号规则影响: %+v", got)
	}
}

// TestRuleBlockedIgnoresTransientCooldown 「唯一没被规则挡的号在临时冷却」不能被读成
// 全池被规则挡住：冷却会自愈，规则不会，对外错误语义完全不同。
func TestRuleBlockedIgnoresTransientCooldown(t *testing.T) {
	t.Parallel()

	state := rulePool()
	state.Add(&auth.Auth{UID: "cn2", Domain: "www.codebuddy.cn"})

	rule, err := state.UpsertRule(ModelRule{Mode: ModelRuleDeny, Models: []string{"glm-5.2"}})
	if err != nil {
		t.Fatal(err)
	}

	if err := state.AssignRules("cn1", []string{rule.ID}); err != nil {
		t.Fatal(err)
	}

	// cn2 未被规则挡住，但正处于 429 软冷却：不可选，却仍算本域的规则可选项。
	state.Cooldown("cn2", CoolSoft, time.Minute, "429")

	if got := state.RuleBlockedForRealm("glm-5.2", "cn"); got.Eligible != 2 || got.Blocked != 1 || got.AllBlocked() {
		t.Errorf("冷却中的号不应被算成规则阻塞: %+v", got)
	}
}

// TestModelRuleDenySkipsPick 被规则挡住的账号在选号主路径上直接不可选（不依赖
// healthyForModel 之外的分支判断）。
func TestModelRuleDenySkipsPick(t *testing.T) {
	t.Parallel()

	state := rulePool()

	rule, err := state.UpsertRule(ModelRule{Mode: ModelRuleDeny, Models: []string{"glm-5.2"}})
	if err != nil {
		t.Fatal(err)
	}

	if err := state.AssignRules("cn1", []string{rule.ID}); err != nil {
		t.Fatal(err)
	}

	// cn1 是唯一的 cn 号且被规则挡住：本域没有可选账号。
	if acct := state.PickExcludingForRealm(map[string]bool{}, "glm-5.2", "cn"); acct != nil {
		t.Fatalf("被规则挡住的账号不应被选中: uid=%s", acct.UID)
	}

	// 未被规则挡住的模型照常选中。
	if acct := state.PickExcludingForRealm(map[string]bool{}, "glm-4.6", "cn"); acct == nil || acct.UID != "cn1" {
		t.Fatalf("未命中规则的模型应照常选中 cn1, got %+v", acct)
	}
}

// TestModelRuleDenyNotBypassedByFallback 全冷却兜底不能让规则号跑被禁模型：
// 规则号同时处于软冷却（兜底候选）、另一个号本请求已试过时，兜底必须返回 nil。
func TestModelRuleDenyNotBypassedByFallback(t *testing.T) {
	t.Parallel()

	state := rulePool()

	rule, err := state.UpsertRule(ModelRule{Mode: ModelRuleDeny, Models: []string{"glm-5.2"}})
	if err != nil {
		t.Fatal(err)
	}

	if err := state.AssignRules("cn1", []string{rule.ID}); err != nil {
		t.Fatal(err)
	}

	// cn1 有冷却截止（进得了兜底候选），g1 本请求已试过。
	state.Cooldown("cn1", CoolSoft, time.Minute, "429")

	if acct := state.PickExcludingForRealm(map[string]bool{"g1": true}, "glm-5.2", ""); acct != nil {
		t.Fatalf("兜底把规则号捞回来了: uid=%s", acct.UID)
	}
}
