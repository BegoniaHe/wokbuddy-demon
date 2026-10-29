package upstream

import (
	"net/http"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// paidLineCapJSON 官方 /v3/config 的付费线路配置片段（键名照包内常量）。
const paidLineCapJSON = `,"productFeaturesConfig":{"ModelRateLimitCap":{"lines":[{"freeId":"hy3","paidId":"hy3-x"}]}}`

// paidLineV3Body 构造 /v3/config 响应：目录含免费 / 付费两条线路，默认 agent 白名单
// 把两者都列进来（官方 injectPaidLineModels 会把 paidId 补进 agents[].models）。
func paidLineV3Body(productFeatures string) string {
	return `{"code":0,"data":{"models":[{"id":"hy3","maxOutputTokens":8192},` +
		`{"id":"hy3-x","maxOutputTokens":8192}],` +
		`"agents":[{"name":"cli","models":["hy3","hy3-x"]}]` + productFeatures + `}}`
}

// TestIsEligiblePaidLineUser 官方 isEligiblePaidLineUser 只看 account.type：
// personal / pro 命中，企业版与无版本号不命中（issue #402）。
func TestIsEligiblePaidLineUser(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		account *auth.Auth
		want    bool
	}{
		{"personal", &auth.Auth{UID: "u1", EditionType: "personal"}, true},
		{"pro", &auth.Auth{UID: "u1", EditionType: "pro"}, true},
		{"PRO 大写", &auth.Auth{UID: "u1", EditionType: " PRO "}, true},
		{"pro 带普通 enterpriseId", &auth.Auth{UID: "u1", EditionType: "pro", EnterpriseID: "ent-1"}, true},
		// 官方排除表 yae：与 isIOAEnterpriseId 同一份常量，命中即不 eligible。
		{
			"personal 命中排除表 A",
			&auth.Auth{UID: "u1", EditionType: "personal", EnterpriseID: "esoikz80kd8g"},
			false,
		},
		{
			"personal 命中排除表 B（首尾空白）",
			&auth.Auth{UID: "u1", EditionType: "personal", EnterpriseID: " etahzsqej0n4 "},
			false,
		},
		{"ultimate", &auth.Auth{UID: "u1", EditionType: "ultimate"}, false},
		{"exclusive", &auth.Auth{UID: "u1", EditionType: "Exclusive"}, false},
		{"无版本号", &auth.Auth{UID: "u1"}, false},
		{"nil 账号", nil, false},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			if got := isEligiblePaidLineUser(testCase.account); got != testCase.want {
				t.Fatalf("eligible=%v want %v", got, testCase.want)
			}
		})
	}
}

// TestApplyPaidLineFilterHidesOnlyHitPaidIDs 只隐藏命中的 paidId：freeId、未命中的
// id、非 eligible 账号、空配置一律不动（官方 filterVisiblePaidLineModels）。
func TestApplyPaidLineFilterHidesOnlyHitPaidIDs(t *testing.T) {
	t.Parallel()

	capHit := v3ModelRateLimitCap{Lines: []v3PaidLine{{FreeID: "hy3", PaidID: "hy3-x"}}}
	capMiss := v3ModelRateLimitCap{Lines: []v3PaidLine{{FreeID: "hy4", PaidID: "hy4-x"}}}

	cases := []struct {
		name       string
		account    *auth.Auth
		rateCap    v3ModelRateLimitCap
		wantHidden bool
	}{
		{"个人号命中", &auth.Auth{UID: "u1", EditionType: "personal"}, capHit, true},
		{"企业版不生效", &auth.Auth{UID: "u1", EditionType: "ultimate"}, capHit, false},
		{"无版本号不生效", &auth.Auth{UID: "u1"}, capHit, false},
		{"无配置不生效", &auth.Auth{UID: "u1", EditionType: "personal"}, v3ModelRateLimitCap{}, false},
		{"paidId 未命中目录", &auth.Auth{UID: "u1", EditionType: "personal"}, capMiss, false},
		{
			"排除表企业号不生效",
			&auth.Auth{UID: "u1", EditionType: "personal", EnterpriseID: "esoikz80kd8g"},
			capHit,
			false,
		},
		{
			"line 缺 paidId",
			&auth.Auth{UID: "u1", EditionType: "personal"},
			v3ModelRateLimitCap{Lines: []v3PaidLine{{FreeID: "hy3", PaidID: " "}}},
			false,
		},
		{"nil 账号", nil, capHit, false},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			out := map[string]ModelInfo{"hy3": {ID: "hy3"}, "hy3-x": {ID: "hy3-x"}}
			applyPaidLineFilter(out, testCase.account, testCase.rateCap)

			assertPaidLineHidden(t, out, testCase.wantHidden)
		})
	}
}

// assertPaidLineHidden 断言 paidId 变体（hy3-x）是否被判不可见，且 freeId（hy3）
// 永远不被本条规则改动——freeId 保持未判定（nil）。
func assertPaidLineHidden(t *testing.T, out map[string]ModelInfo, wantHidden bool) {
	t.Helper()

	paid, ok := out["hy3-x"]
	if !ok {
		t.Fatal("目录缺 hy3-x")
	}

	switch {
	case wantHidden && (paid.ChatVisible == nil || *paid.ChatVisible):
		t.Fatalf("hy3-x ChatVisible=%v want false", paid.ChatVisible)
	case !wantHidden && paid.ChatVisible != nil:
		t.Fatalf("hy3-x ChatVisible=%v want 未判定", *paid.ChatVisible)
	}

	if free, ok := out["hy3"]; ok && free.ChatVisible != nil {
		t.Fatalf("freeId hy3 不应被 paid-line 规则改动：%v", *free.ChatVisible)
	}
}

// TestFetchV3ConfigPaidLineFilter 端到端：/v3/config 目录里命中 paidId 的变体被打成
// 不可见，freeId 照常；两域同形，非 eligible 与无配置走势与旧行为一致（issue #402）。
func TestFetchV3ConfigPaidLineFilter(t *testing.T) {
	t.Parallel()

	globalAccount := func() *auth.Auth {
		return &auth.Auth{UID: "u-gl", AccessToken: "at", EditionType: "personal", Domain: "www.workbuddy.ai"}
	}

	cases := []struct {
		name       string
		account    *auth.Auth
		product    string
		wantHidden bool
	}{
		{"CN 个人号命中", &auth.Auth{UID: "u-cn", AccessToken: "at", EditionType: "personal"}, paidLineCapJSON, true},
		{"global 个人号命中", globalAccount(), paidLineCapJSON, true},
		// encoding/json 字段名大小写不敏感：小写键形态同样要吃到。
		{
			"小写键形态",
			&auth.Auth{UID: "u-cn-pro", AccessToken: "at", EditionType: "pro"},
			`,"productFeaturesConfig":{"modelRateLimitCap":{"lines":[{"freeId":"hy3","paidId":"hy3-x"}]}}`,
			true,
		},
		{"企业版不生效", &auth.Auth{UID: "u-cn-ent", AccessToken: "at", EditionType: "ultimate"}, paidLineCapJSON, false},
		{"上游不下发配置", &auth.Auth{UID: "u-cn-vanilla", AccessToken: "at", EditionType: "personal"}, "", false},
		{
			"lines 为空",
			&auth.Auth{UID: "u-cn-empty", AccessToken: "at", EditionType: "personal"},
			`,"productFeaturesConfig":{"ModelRateLimitCap":{"lines":[]}}`,
			false,
		},
		{
			"paidId 全部未命中",
			&auth.Auth{UID: "u-cn-miss", AccessToken: "at", EditionType: "personal"},
			`,"productFeaturesConfig":{"ModelRateLimitCap":{"lines":[{"freeId":"hy4","paidId":"hy4-x"}]}}`,
			false,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			client := testClient(func(req *http.Request) (*http.Response, error) {
				if !strings.HasSuffix(req.URL.Path, "/v3/config") {
					return jsonResp(http.StatusNotFound, `{}`), nil
				}

				return jsonResp(http.StatusOK, paidLineV3Body(testCase.product)), nil
			})

			out, err := client.fetchV3ConfigModelMap(testCase.account, "")
			if err != nil {
				t.Fatalf("fetchV3ConfigModelMap: %v", err)
			}

			assertV3PaidLineOutcome(t, out, testCase.wantHidden)
		})
	}
}

// assertV3PaidLineOutcome 断言 v3 目录里 paidId 变体（hy3-x）与 freeId（hy3）的判定。
func assertV3PaidLineOutcome(t *testing.T, out map[string]ModelInfo, wantHidden bool) {
	t.Helper()

	paid, ok := out["hy3-x"]
	if !ok {
		t.Fatalf("目录缺 hy3-x: %v", out)
	}

	// 未过滤时 hy3-x 由 #319 白名单判定为可见（它就在 agents[].models 里）。
	if wantHidden {
		if paid.ChatVisible == nil || *paid.ChatVisible {
			t.Fatalf("hy3-x ChatVisible=%v want false", paid.ChatVisible)
		}
	} else if paid.ChatVisible == nil || !*paid.ChatVisible {
		t.Fatalf("hy3-x ChatVisible=%v want true", paid.ChatVisible)
	}

	// freeId 与 #319 白名单判定不受影响：hit 时仍为可见。
	free := out["hy3"]
	if free.ChatVisible == nil || !*free.ChatVisible {
		t.Fatalf("hy3 ChatVisible=%v want true", free.ChatVisible)
	}
}
