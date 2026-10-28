package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestIsEnterprise 企业版判定：enterpriseId 非空即企业版（纯空白视为非企业）。
// 该判定驱动 scheduler 五类任务与 panel 成长任务/签到入口的门控——企业版在这些
// 端点上被上游一律拒绝（400 code 10001 / 403 growth-only），误判为个人号会导致
// 每日对注定失败的端点空发请求。
func TestIsEnterprise(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		ent  string
		want bool
	}{
		{"个人号（空）", "", false},
		{"纯空白", "   ", false},
		{"企业号", "fn7xvyc51dkw", true},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			t.Parallel()

			a := &Auth{UID: "u1", EnterpriseID: item.ent}
			if got := a.IsEnterprise(); got != item.want {
				t.Errorf("EnterpriseID=%q IsEnterprise()=%v want %v", item.ent, got, item.want)
			}
		})
	}
}

// TestIsEnterpriseAndIsGlobalIndependent 企业标识与 realm 是正交维度：
// 企业号恒为 CN realm，但两者判定互不干扰（门控引用处并列书写）。
func TestIsEnterpriseAndIsGlobalIndependent(t *testing.T) {
	t.Parallel()

	a := &Auth{UID: "u1", EnterpriseID: "ent-1"}
	if !a.IsEnterprise() {
		t.Error("应判为企业版")
	}

	if a.IsGlobal() {
		t.Error("企业号不应被判为 global")
	}
}

// TestIsEnterpriseEdition 对齐官方 isEnterpriseEditionAccount：enterpriseId 非空，
// 或 account.type 为 ultimate / exclusive（大小写与空白容错）。
// 该判定只用于出站 X-Private-Data，不改变 IsEnterprise() 的任务门控。
func TestIsEnterpriseEdition(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		auth *Auth
		want bool
	}{
		{"个人号", &Auth{UID: "u1", EditionType: "personal"}, false},
		{"缺字段", &Auth{UID: "u1"}, false},
		{"enterpriseId", &Auth{UID: "u1", EnterpriseID: "ent-1"}, true},
		{"ultimate 无企业 ID", &Auth{UID: "u1", EditionType: "ultimate"}, true},
		{"exclusive 大写", &Auth{UID: "u1", EditionType: " Exclusive "}, true},
		{"其他版本", &Auth{UID: "u1", EditionType: "experience"}, false},
		{"nil", nil, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			if got := testCase.auth.IsEnterpriseEdition(); got != testCase.want {
				t.Errorf("IsEnterpriseEdition()=%v want %v", got, testCase.want)
			}
		})
	}
}

// TestParseEditionType account.type 从嵌套形 account.type / 扁平形 type 读取，
// 且只用于企业版判定，不影响 IsEnterprise()。
func TestParseEditionType(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "嵌套形",
			raw:  `{"auth":{"accessToken":"at"},"account":{"uid":"u1","type":"ultimate"}}`,
			want: "ultimate",
		},
		{
			name: "扁平形",
			raw:  `{"accessToken":"at","uid":"u1","type":"exclusive"}`,
			want: "exclusive",
		},
		{
			name: "缺字段",
			raw:  `{"auth":{"accessToken":"at"},"account":{"uid":"u1"}}`,
			want: "",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			account, err := Parse([]byte(testCase.raw))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}

			if account.EditionType != testCase.want {
				t.Errorf("EditionType=%q want %q", account.EditionType, testCase.want)
			}

			if account.IsEnterprise() {
				t.Error("IsEnterprise() 只看 enterpriseId，不应被 type 影响")
			}
		})
	}
}

// TestSaveAtomicWritesEditionType type 非空时写回嵌套 account.type，可重新解析；
// 空值不写该键（避免给手写账号塞空版本）。
func TestSaveAtomicWritesEditionType(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name      string
		edition   string
		wantInDoc bool
	}{
		{"有值写回", "ultimate", true},
		{"空值不写", "", false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			filePath := filepath.Join(t.TempDir(), "workbuddy-u1.json")

			account := &Auth{
				AccessToken: "at", UID: "u1", EditionType: testCase.edition, FilePath: filePath,
			}
			if err := account.SaveAtomic(); err != nil {
				t.Fatalf("save: %v", err)
			}

			raw, err := os.ReadFile(filePath)
			if err != nil {
				t.Fatalf("read: %v", err)
			}

			var doc struct {
				Account map[string]any `json:"account"`
			}
			if err = json.Unmarshal(raw, &doc); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}

			if _, ok := doc.Account["type"]; ok != testCase.wantInDoc {
				t.Errorf("account.type 存在=%v want %v", ok, testCase.wantInDoc)
			}
		})
	}
}
