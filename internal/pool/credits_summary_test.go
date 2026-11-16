// credits_summary_test.go 钉住积分语义分离（issue #571）：
// disabled 账号的 credits 报可用量 0、credits_raw 留真实快照；paused 两者不变；
// CreditsSummary 的 available/snapshot/quota 与 by_realm 分域口径。
package pool

import (
	"reflect"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

func TestStatusCreditsSplit(t *testing.T) {
	t.Parallel()

	state := New("")
	state.Add(&auth.Auth{UID: "live", Nickname: "live"})
	state.Add(&auth.Auth{UID: "dead", Nickname: "dead"})
	state.Add(&auth.Auth{UID: "paused", Nickname: "paused"})

	state.SetCredits("live", 120, 200)
	state.SetCredits("dead", 4552, 6000)
	state.SetCredits("paused", 30, 50)
	state.Disable("dead", "12153 session dead")
	state.Pause("paused")

	dead, ok := state.Status("dead")
	if !ok {
		t.Fatal("dead status missing")
	}

	if dead.Credits != 0 {
		t.Errorf("禁用号 credits=%d want 0", dead.Credits)
	}

	if dead.CreditsRaw != 4552 {
		t.Errorf("禁用号 credits_raw=%d want 4552", dead.CreditsRaw)
	}

	if dead.CreditsTotal != 0 {
		t.Errorf("禁用号 credits_total=%d want 0", dead.CreditsTotal)
	}

	paused, ok := state.Status("paused")
	if !ok {
		t.Fatal("paused status missing")
	}

	if paused.Credits != 30 || paused.CreditsRaw != 30 || paused.CreditsTotal != 50 {
		t.Errorf("暂停号 credits=%d raw=%d total=%d want 30/30/50", paused.Credits, paused.CreditsRaw, paused.CreditsTotal)
	}
}

// TestCreditsSummaryExcludesDisabledSnapshot available 不含禁用号冻结额度，
// snapshot 含；quota 只累计可用账号的正额度（企业版 -1 不计入）。
func TestCreditsSummaryExcludesDisabledSnapshot(t *testing.T) {
	t.Parallel()

	state := New("")
	state.Add(&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn"})
	state.Add(&auth.Auth{UID: "dead", Domain: "www.codebuddy.cn"})
	state.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"})
	state.Add(&auth.Auth{UID: "ent", Domain: "www.codebuddy.cn", EnterpriseID: "e1"})

	state.SetCredits("cn1", 120, 200)
	state.SetCredits("dead", 4552, 6000)
	state.SetCredits("g1", 80, 0)
	state.SetCredits("ent", 7, -1)
	state.Disable("dead", "12153 session dead")

	summary := state.CreditsSummary()

	want := CreditsTotals{Available: 207, Snapshot: 4759, Quota: 200}
	if summary.CreditsTotals != want {
		t.Errorf("credits=%+v want %+v（available 不含禁用号，snapshot 含，企业版 -1 不计额度）", summary.CreditsTotals, want)
	}

	wantRealm := map[string]CreditsTotals{
		"cn":     {Available: 127, Snapshot: 4679, Quota: 200},
		"global": {Available: 80, Snapshot: 80, Quota: 0},
	}

	if !reflect.DeepEqual(summary.ByRealm, wantRealm) {
		t.Errorf("by_realm=%+v want %+v", summary.ByRealm, wantRealm)
	}
}
