// modelrules_test.go 钉住模型规则面板接口（issue #569）：规则 CRUD、账号维度
// 整体替换与批量挂载 / 摘除、未知 id 的拒绝。
package panel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/jsonx"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
)

// modelRulePanel 构造带一个账号的面板；请求都带默认 api_key（单密钥即管理面幂等）。
func modelRulePanel(t *testing.T) (*Panel, *pool.Pool) {
	t.Helper()

	testPool := pool.New("")
	t.Cleanup(testPool.Close)
	testPool.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})

	return New(&Config{Version: "test", APIKey: "k", Pool: testPool}), testPool
}

func modelRuleDo(t *testing.T, panel *Panel, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer k")

	recorder := httptest.NewRecorder()
	panel.ServeHTTP(recorder, req)

	return recorder
}

func TestModelRulesCreateAndList(t *testing.T) {
	t.Parallel()

	panel, testPool := modelRulePanel(t)

	rec := modelRuleDo(t, panel, http.MethodPost, "/panel/api/model_rules",
		`{"name":"只跑便宜模型","mode":"allow","models":["glm-4.6","global:kimi-k3"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create code=%d body=%s", rec.Code, rec.Body)
	}

	rules := testPool.Rules()
	if len(rules) != 1 || rules[0].Name != "只跑便宜模型" || rules[0].Mode != pool.ModelRuleAllow {
		t.Fatalf("规则未落库: %+v", rules)
	}

	if rules[0].Models[0] != "glm-4.6" || rules[0].Models[1] != "global:kimi-k3" {
		t.Fatalf("模型名应保留域前缀与顺序: %+v", rules[0].Models)
	}

	var view modelRuleView
	if err := jsonx.Unmarshal(modelRuleDo(t, panel, http.MethodGet, "/panel/api/model_rules", "").Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}

	if len(view.Rules) != 1 || len(view.Assignments) != 0 {
		t.Fatalf("view=%+v", view)
	}
}

func TestModelRulesAssignAndDelete(t *testing.T) {
	t.Parallel()

	panel, testPool := modelRulePanel(t)

	rec := modelRuleDo(t, panel, http.MethodPost, "/panel/api/model_rules", `{"mode":"deny","models":["glm-5.2"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create code=%d body=%s", rec.Code, rec.Body)
	}

	ruleID := testPool.Rules()[0].ID

	// 整体替换（账号维度）：挂上该规则。
	rec = modelRuleDo(t, panel, http.MethodPost, "/panel/api/model_rules/assign",
		`{"uids":["u1"],"rule_ids":["`+ruleID+`"],"action":"replace"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("assign code=%d body=%s", rec.Code, rec.Body)
	}

	if got := testPool.RuleAssignments()["u1"]; len(got) != 1 || got[0] != ruleID {
		t.Fatalf("挂载未生效: %v", got)
	}

	// 删除规则：引用同步摘除。
	rec = modelRuleDo(t, panel, http.MethodPost, "/panel/api/model_rules/"+ruleID+"/delete", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete code=%d body=%s", rec.Code, rec.Body)
	}

	if len(testPool.Rules()) != 0 {
		t.Fatalf("删除后应清空规则: rules=%v", testPool.Rules())
	}

	if len(testPool.RuleAssignments()) != 0 {
		t.Fatalf("删除后应清空引用: assignments=%v", testPool.RuleAssignments())
	}
}

func TestModelRulesBatchAttachDetach(t *testing.T) {
	t.Parallel()

	panel, testPool := modelRulePanel(t)

	rec := modelRuleDo(t, panel, http.MethodPost, "/panel/api/model_rules", `{"mode":"deny","models":["glm-5.2"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create code=%d body=%s", rec.Code, rec.Body)
	}

	ruleID := testPool.Rules()[0].ID

	// 批量挂载（幂等并集）。
	for range 2 {
		rec = modelRuleDo(t, panel, http.MethodPost, "/panel/api/model_rules/assign",
			`{"uids":["u1"],"rule_ids":["`+ruleID+`"],"action":"attach"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("attach code=%d body=%s", rec.Code, rec.Body)
		}
	}

	if got := testPool.RuleAssignments()["u1"]; len(got) != 1 {
		t.Fatalf("重复 attach 应幂等: %v", got)
	}

	rec = modelRuleDo(t, panel, http.MethodPost, "/panel/api/model_rules/assign",
		`{"uids":["u1"],"rule_ids":["`+ruleID+`"],"action":"detach"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("detach code=%d body=%s", rec.Code, rec.Body)
	}

	if got := testPool.RuleAssignments()["u1"]; len(got) != 0 {
		t.Fatalf("detach 后不应再有引用: %v", got)
	}
}

func TestModelRulesRejectInvalid(t *testing.T) {
	t.Parallel()

	panel, _ := modelRulePanel(t)

	cases := []struct {
		name, path, body string
	}{
		{"空 body", "/panel/api/model_rules", ""},
		{"非法模式", "/panel/api/model_rules", `{"mode":"whatever","models":["glm-5.2"]}`},
		{"没有模型", "/panel/api/model_rules", `{"mode":"deny","models":[]}`},
		{"未知规则", "/panel/api/model_rules/assign", `{"uids":["u1"],"rule_ids":["rule-nope"]}`},
		{"空账号", "/panel/api/model_rules/assign", `{"uids":[],"rule_ids":[]}`},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			if rec := modelRuleDo(t, panel, http.MethodPost, testCase.path, testCase.body); rec.Code != http.StatusBadRequest {
				t.Fatalf("code=%d want 400, body=%s", rec.Code, rec.Body)
			}
		})
	}
}
