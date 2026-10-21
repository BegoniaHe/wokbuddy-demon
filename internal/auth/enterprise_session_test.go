package auth

import (
	"encoding/base64"
	"testing"
)

// jwtWithClaims 造一个「只填 payload」的假 JWT（本仓库只解 payload、不验签）。
func jwtWithClaims(t *testing.T, payload string) string {
	t.Helper()

	enc := base64.RawURLEncoding.EncodeToString([]byte(payload))

	return "eyJhbGciOiJIUzI1NiJ9." + enc + ".sig"
}

// TestIsEnterpriseSession 形态矩阵：企业身份 / token_source=enterprise_switch /
// 两者都无 / 畸形 token。
func TestIsEnterpriseSession(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		account *Auth
		want    bool
		wantEnt bool // IsEnterprise() 的结果（隔离断言）
	}{
		{"企业身份", &Auth{EnterpriseID: "ent-1"}, true, true},
		{
			name:    "个人身份 + 企业切换会话",
			account: &Auth{AccessToken: jwtWithClaims(t, `{"sub":"u1","token_source":"enterprise_switch"}`)},
			want:    true,
			wantEnt: false, // 计费口径不受 token_source 影响
		},
		{
			name:    "普通个人会话",
			account: &Auth{AccessToken: jwtWithClaims(t, `{"sub":"u1","token_source":"personal"}`)},
			want:    false,
			wantEnt: false,
		},
		{"无 token", &Auth{}, false, false},
		{"畸形 token", &Auth{AccessToken: "not-a-jwt"}, false, false},
		{"payload 非 JSON", &Auth{AccessToken: jwtWithClaims(t, `not json`)}, false, false},
		{
			name: "带 padding 的 base64 payload",
			account: &Auth{AccessToken: "h." + base64.URLEncoding.EncodeToString(
				[]byte(`{"token_source":"enterprise_switch"}`)) + ".s"},
			want:    true,
			wantEnt: false,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			if got := testCase.account.IsEnterpriseSession(); got != testCase.want {
				t.Errorf("IsEnterpriseSession()=%v want %v", got, testCase.want)
			}

			// 与计费口径刻意隔离：token_source 变化不得影响 IsEnterprise。
			if got := testCase.account.IsEnterprise(); got != testCase.wantEnt {
				t.Errorf("IsEnterprise()=%v want %v（与 IsEnterpriseSession 必须隔离）", got, testCase.wantEnt)
			}
		})
	}
}
