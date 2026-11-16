// 模型规则面板接口（issue #569）：规则 CRUD + 账号维度批量挂载 / 摘除。
//
// 规则本体与「账号 → 规则 id」引用都由 pool 持久化到 state.json；面板只做校验与
// 转发，不维护第二份状态。
package panel

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
)

// modelRuleView 规则 + 账号挂载视图（GET /panel/api/model_rules）。
type modelRuleView struct {
	Rules       []pool.ModelRule    `json:"rules"`
	Assignments map[string][]string `json:"assignments"`
	Counts      map[string]int      `json:"counts"`
}

// modelRules 列出规则本体、账号挂载引用与各规则的使用账号数。
func (p *Panel) modelRules(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, modelRuleView{
		Rules:       p.cfg.Pool.Rules(),
		Assignments: p.cfg.Pool.RuleAssignments(),
		Counts:      p.cfg.Pool.RuleAccountCounts(),
	})
}

// modelRuleUpsert 新建或更新规则；改内容后所有挂载账号下一请求即生效。
func (p *Panel) modelRuleUpsert(w http.ResponseWriter, r *http.Request) {
	var body *struct {
		ID     string   `json:"id"`
		Name   string   `json:"name"`
		Mode   string   `json:"mode"`
		Models []string `json:"models"`
	}
	// 指针接收：空 body / 畸形 JSON / 字面量 null 都拒绝，避免静默建出空规则。
	if err := decodeJSONBody(r.Body, &body); err != nil || body == nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")

		return
	}

	rule, err := p.cfg.Pool.UpsertRule(pool.ModelRule{
		ID:     strings.TrimSpace(body.ID),
		Name:   strings.TrimSpace(body.Name),
		Mode:   pool.ModelRuleMode(strings.TrimSpace(body.Mode)),
		Models: body.Models,
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, "规则不合法：mode 需为 allow/deny，且至少列出一个模型")

		return
	}

	log.Printf("panel: 模型规则已保存 id=%s mode=%s models=%d", rule.ID, rule.Mode, len(rule.Models))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "rule": rule})
}

// modelRuleDelete 删除规则：所有账号上的引用同步摘除。
func (p *Panel) modelRuleDelete(w http.ResponseWriter, r *http.Request) {
	if !p.cfg.Pool.DeleteRule(r.PathValue("id")) {
		writeErr(w, http.StatusNotFound, "rule not found")

		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// modelRuleAssign 账号维度的规则挂载：
//   - replace：整体替换这些账号的规则引用（账号维度整体替换）；
//   - attach / detach：批量并集增删（面板「批量挂/摘」）。
func (p *Panel) modelRuleAssign(w http.ResponseWriter, r *http.Request) {
	var body *struct {
		UIDs    []string `json:"uids"`
		RuleIDs []string `json:"rule_ids"`
		Action  string   `json:"action"`
	}
	if err := decodeJSONBody(r.Body, &body); err != nil || body == nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")

		return
	}

	if len(body.UIDs) == 0 {
		writeErr(w, http.StatusBadRequest, "uids 不能为空")

		return
	}

	err := p.applyRuleAssignment(body.UIDs, body.RuleIDs, strings.TrimSpace(body.Action))
	if err != nil {
		writeErr(w, http.StatusBadRequest, ruleAssignErrorMessage(err))

		return
	}

	log.Printf("panel: 模型规则挂载已更新 accounts=%d rules=%d action=%q",
		len(body.UIDs), len(body.RuleIDs), body.Action)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"assignments": p.cfg.Pool.RuleAssignments(),
	})
}

// applyRuleAssignment 执行挂载动作；action 为空按 replace（账号维度整体替换）。
func (p *Panel) applyRuleAssignment(uids, ruleIDs []string, action string) error {
	switch action {
	case "", "replace":
		for _, uid := range uids {
			if err := p.cfg.Pool.AssignRules(uid, ruleIDs); err != nil {
				return fmt.Errorf("panel: 替换账号 %s 的模型规则失败: %w", uid, err)
			}
		}

		return nil
	case "attach":
		if err := p.cfg.Pool.AttachRulesForAccounts(uids, ruleIDs); err != nil {
			return fmt.Errorf("panel: 批量挂载模型规则失败: %w", err)
		}

		return nil
	case "detach":
		if err := p.cfg.Pool.DetachRulesForAccounts(uids, ruleIDs); err != nil {
			return fmt.Errorf("panel: 批量摘除模型规则失败: %w", err)
		}

		return nil
	}

	return pool.ErrRuleInvalid
}

// ruleAssignErrorMessage 把 pool 的哨兵错误转成可行动的面板提示。
func ruleAssignErrorMessage(err error) string {
	switch {
	case errors.Is(err, pool.ErrAccountNotFound):
		return "账号不存在"
	case errors.Is(err, pool.ErrRuleNotFound):
		return "规则不存在"
	case errors.Is(err, pool.ErrRuleInvalid):
		return "action 需为 replace / attach / detach"
	}

	return "更新模型规则失败"
}
