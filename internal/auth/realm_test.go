package auth

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

// jwtWithIss 构造仅含 iss 的伪 JWT（三段，payload 为 base64url，无签名校验需求）。
func jwtWithIss(iss string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"` + iss + `"}`))
	return header + "." + payload + ".sig"
}

// withGlobalEnabled 临时打开 global realm 开关（生产缺省即开，此辅助仅显式确保），
// 测试结束复位为开启态（缺省）。
func withGlobalEnabled(t *testing.T) {
	t.Helper()
	globalEnabled.Store(true)
	t.Cleanup(func() { globalEnabled.Store(true) })
}

// withGlobalDisabled 临时关闭 global realm 开关（逃生门），测试结束复位为开启态（缺省）。
func withGlobalDisabled(t *testing.T) {
	t.Helper()
	globalEnabled.Store(false)
	t.Cleanup(func() { globalEnabled.Store(true) })
}

func TestRealmExplicitGlobal(t *testing.T) {
	withGlobalEnabled(t)
	a := &Auth{realm: "global"}
	if got := a.Realm(); got != "global" {
		t.Errorf("Realm()=%q want global", got)
	}
	if !a.IsGlobal() {
		t.Error("IsGlobal()=false want true")
	}
}

func TestRealmExplicitCN(t *testing.T) {
	a := &Auth{realm: "cn"}
	if got := a.Realm(); got != "cn" {
		t.Errorf("Realm()=%q want cn", got)
	}
	if a.IsGlobal() {
		t.Error("IsGlobal()=true want false")
	}
}

func TestRealmDomainFallback(t *testing.T) {
	withGlobalEnabled(t)
	cases := []struct{ domain, want string }{
		{"www.workbuddy.ai", "global"},
		{"workbuddy.ai", "global"},
		{"sub.workbuddy.ai", "global"},
		{"www.codebuddy.cn", "cn"},
		{"", "cn"},
	}
	for _, c := range cases {
		a := &Auth{Domain: c.domain}
		if got := a.Realm(); got != c.want {
			t.Errorf("Domain=%q Realm()=%q want %q", c.domain, got, c.want)
		}
	}
}

func TestRealmEmptyFallsBackToCN(t *testing.T) {
	// 开关缺省开启（零回归前提）：空 realm + 空 domain → cn（老 CN 凭证的核心）。
	a := &Auth{}
	if got := a.Realm(); got != "cn" {
		t.Errorf("Realm()=%q want cn", got)
	}
	// 显式 global → global（缺省开启，Realm() 不再因"未配置"而恒 cn）。
	ag := &Auth{realm: "global"}
	if got := ag.Realm(); got != "global" {
		t.Errorf("Realm()=%q want global", got)
	}
	// domain 回落照常（缺省开启识别 workbuddy.ai）。
	ad := &Auth{Domain: "www.workbuddy.ai"}
	if got := ad.Realm(); got != "global" {
		t.Errorf("Realm()=%q want global", got)
	}
}

// TestRealmDefaultOnForCNZeroRegression 开关缺省开启时，老 CN 凭证（无 realm、无 domain）
// Realm() 恒为 cn——「默认开启」不影响纯 CN 部署行为。
func TestRealmDefaultOnForCNZeroRegression(t *testing.T) {
	withGlobalEnabled(t)
	cases := []*Auth{
		{},
		{Domain: "www.codebuddy.cn"},
		{Domain: "codebuddy.cn"},
		{realm: "cn"},
		{realm: "cn", Domain: "www.codebuddy.cn"},
	}
	for _, a := range cases {
		if got := a.Realm(); got != "cn" {
			t.Errorf("%+v Realm()=%q want cn", a, got)
		}
		if a.IsGlobal() {
			t.Errorf("%+v IsGlobal()=true want false", a)
		}
	}
}

// TestRealmExplicitOffEscapeHatch 逃生门：SetGlobalEnabled(false) 后恒 cn，
// 即便 realm=global / domain=workbuddy.ai（纯 CN 锁定，与旧缺省行为等价）。
func TestRealmExplicitOffEscapeHatch(t *testing.T) {
	withGlobalDisabled(t)
	cases := []struct {
		auth *Auth
	}{
		{&Auth{realm: "global"}},
		{&Auth{realm: "global", Domain: "www.workbuddy.ai"}},
		{&Auth{Domain: "www.workbuddy.ai"}},
	}
	for _, tc := range cases {
		if got := tc.auth.Realm(); got != "cn" {
			t.Errorf("%+v Realm()=%q want cn (switch off)", tc.auth, got)
		}
		if tc.auth.IsGlobal() {
			t.Errorf("%+v IsGlobal()=true want false (switch off)", tc.auth)
		}
	}
}

func TestParseNestedRealm(t *testing.T) {
	raw := []byte(`{"auth":{"accessToken":"at","refreshToken":"rt","expiresAt":1,"domain":"www.workbuddy.ai","realm":"global"},"account":{"uid":"u1"}}`)
	sa, err := Parse(raw)
	if err != nil {
		t.Fatalf("nested parse err: %v", err)
	}
	if sa.realm != "global" {
		t.Errorf("nested realm=%q want global", sa.realm)
	}
	// 嵌套形显式 realm 空 → 读不到，靠 domain 回落。
	raw2 := []byte(`{"auth":{"accessToken":"at","refreshToken":"rt","expiresAt":1},"account":{"uid":"u1"}}`)
	sa2, err := Parse(raw2)
	if err != nil {
		t.Fatalf("nested no-realm parse err: %v", err)
	}
	if sa2.realm != "" {
		t.Errorf("nested missing realm key should be zero, got %q", sa2.realm)
	}
}

func TestParseFlatRealm(t *testing.T) {
	withGlobalEnabled(t)
	raw := []byte(`{"accessToken":"at","refreshToken":"rt","expiresAt":1,"uid":"u2","realm":"global"}`)
	fa, err := Parse(raw)
	if err != nil {
		t.Fatalf("flat parse err: %v", err)
	}
	if fa.realm != "global" {
		t.Errorf("flat realm=%q want global", fa.realm)
	}
	if !fa.IsGlobal() {
		t.Error("flat global account IsGlobal()=false want true")
	}
	// 扁平形缺 realm 键 → 零值 → CN。
	flatCN := []byte(`{"accessToken":"at","refreshToken":"rt","expiresAt":1,"uid":"u3"}`)
	fc, err := Parse(flatCN)
	if err != nil {
		t.Fatalf("flat cn parse err: %v", err)
	}
	if fc.realm != "" {
		t.Errorf("flat missing realm should be zero, got %q", fc.realm)
	}
}

func TestSaveAtomicWritesRealm(t *testing.T) {
	withGlobalEnabled(t)
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-global.json")
	a := &Auth{
		AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1,
		UID: "g1", realm: "global", FilePath: fp,
	}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	b, err := Parse(raw)
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if b.realm != "global" {
		t.Errorf("roundtrip realm=%q want global", b.realm)
	}
	if !b.IsGlobal() {
		t.Error("roundtrip global account IsGlobal()=false")
	}
}

// TestBackfillRealmDomain CN/global 按原始 domain 推断（backfill 为导出空 realm 字段服务）。
func TestBackfillRealmDomain(t *testing.T) {
	t.Parallel() // 不触碰全局开关
	cases := []struct {
		domain string
		want   string
	}{
		{"www.workbuddy.ai", "global"},
		{"workbuddy.ai", "global"},
		{"sub.workbuddy.ai", "global"},
		{"www.codebuddy.cn", "cn"},
		{"codebuddy.cn", "cn"},
		{"", "cn"}, // 空 domain + 空 realm → cn（老 CN 凭证核心）
	}
	for _, c := range cases {
		a := &Auth{Domain: c.domain}
		changed, got := a.BackfillRealm()
		if !changed {
			t.Errorf("Domain=%q backfill changed=false want true", c.domain)
		}
		assertRealmStored(a, c.want, t)
		if got != c.want {
			t.Errorf("Domain=%q backfill got realm=%q want %q", c.domain, got, c.want)
		}
	}
}

// TestBackfillRealmEscapeHatchFree 逃生门关闭时 backfill 仍按原始 domain 推断（不受 Realm() 降级影响）。
func TestBackfillRealmEscapeHatchFree(t *testing.T) {
	withGlobalDisabled(t) // 逃生门关闭：Realm() 恒 cn，但 backfill 不得被污染
	a := &Auth{Domain: "www.workbuddy.ai"}
	if g := a.Realm(); g != "cn" {
		t.Fatalf("precondition Realm()=%q want cn (escape hatch on)", g)
	}
	changed, got := a.BackfillRealm()
	if !changed {
		t.Fatal("backfill changed=false want true (escape hatch free)")
	}
	assertRealmStored(a, "global", t)
	if got != "global" {
		t.Errorf("backfill got=%q want global (escape hatch must not corrupt backfill)", got)
	}
}

// TestBackfillRealmIdempotent 已有 realm 标识的文件不做修改（幂等）。
func TestBackfillRealmIdempotent(t *testing.T) {
	t.Parallel()
	a := &Auth{Domain: "www.workbuddy.ai", realm: "cn"} // 已有 cn，domain 会推断 global——绝不覆盖
	changed, got := a.BackfillRealm()
	if changed {
		t.Errorf("backfill changed=true want false (existing realm must win)")
	}
	if got != "cn" {
		t.Errorf("backfill got=%q want cn (existing realm preserved)", got)
	}
	assertRealmStored(a, "cn", t)
}

// TestRealmFromIssuer 覆盖 iss 兜底判定：global 家族命中 / 未知 / 非法 token。
func TestRealmFromIssuer(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, token, want string
	}{
		{"global-www", jwtWithIss("https://www.workbuddy.ai/auth/realms/copilot"), "global"},
		{"global-bare", jwtWithIss("https://workbuddy.ai/realms/x"), "global"},
		{"cn-host", jwtWithIss("https://www.codebuddy.cn/auth/realms/copilot"), ""},
		{"unknown-host", jwtWithIss("https://example.com/realms/x"), ""},
		{"empty-iss", jwtWithIss(""), ""},
		{"not-jwt", "not-a-jwt", ""},
		{"bad-base64", "aaa.!!!.ccc", ""},
		{"padded", base64.URLEncoding.EncodeToString([]byte(`{"iss":"https://www.workbuddy.ai"}`)), "global"},
	}
	for _, c := range cases {
		// padded 用例需补 header 段才成三段。
		tok := c.token
		if c.name == "padded" {
			tok = "h." + c.token + ".s"
		}
		if got := realmFromIssuer(tok); got != c.want {
			t.Errorf("%s: realmFromIssuer=%q want %q", c.name, got, c.want)
		}
	}
}

// TestRealmIssFallback realm 与 domain 均空时从 iss 兜底；任一非空则不启用兜底。
func TestRealmIssFallback(t *testing.T) {
	withGlobalEnabled(t)
	global := jwtWithIss("https://www.workbuddy.ai/auth/realms/copilot")

	// 均空 + global iss → global。
	if got := (&Auth{AccessToken: global}).Realm(); got != "global" {
		t.Errorf("empty realm/domain + global iss: Realm()=%q want global", got)
	}
	// 均空 + 未知 iss → cn。
	if got := (&Auth{AccessToken: jwtWithIss("https://example.com")}).Realm(); got != "cn" {
		t.Errorf("unknown iss: Realm()=%q want cn", got)
	}
	// domain 非空（cn）→ 不启用 iss 兜底。
	if got := (&Auth{Domain: "www.codebuddy.cn", AccessToken: global}).Realm(); got != "cn" {
		t.Errorf("cn domain must win over iss: Realm()=%q want cn", got)
	}
	// 显式 realm=cn → 不启用 iss 兜底。
	if got := (&Auth{realm: "cn", AccessToken: global}).Realm(); got != "cn" {
		t.Errorf("explicit cn must win over iss: Realm()=%q want cn", got)
	}
}

// TestBackfillRealmIssFallback domain 与 realm 均空时 backfill 落盘 global（来自 iss）。
func TestBackfillRealmIssFallback(t *testing.T) {
	t.Parallel()
	a := &Auth{AccessToken: jwtWithIss("https://www.workbuddy.ai/auth/realms/copilot")}
	changed, got := a.BackfillRealm()
	if !changed || got != "global" {
		t.Fatalf("backfill changed=%v got=%q want true/global", changed, got)
	}
	assertRealmStored(a, "global", t)

	// 未知 iss → 仍落盘 cn（保持现有默认）。
	b := &Auth{AccessToken: jwtWithIss("https://example.com")}
	changed, got = b.BackfillRealm()
	if !changed || got != "cn" {
		t.Fatalf("unknown iss backfill changed=%v got=%q want true/cn", changed, got)
	}
}

func assertRealmStored(a *Auth, want string, t *testing.T) {
	t.Helper()
	if a.realm != want {
		t.Errorf("stored realm field=%q want %q", a.realm, want)
	}
}

// TestResolveRealm 纯函数归一化显式 realm，缺失时按 domain 推断（与 BackfillRealm
// 共用同一来源；不受逃生门影响）。显式值优先于 domain 推断。
func TestResolveRealm(t *testing.T) {
	t.Parallel() // 纯函数：不触碰全局开关
	cases := []struct {
		explicit, domain, want string
	}{
		{"global", "www.codebuddy.cn", "global"}, // 显式优先：cn domain 也写 global
		{"cn", "www.workbuddy.ai", "cn"},         // 显式优先：global domain 也写 cn
		{"", "www.workbuddy.ai", "global"},       // 缺省按 domain 推断
		{"", "workbuddy.ai", "global"},
		{"", "codebuddy.cn", "cn"},
		{"", "", "cn"}, // 空 domain → cn（老 CN 凭证零回归）
	}
	for _, c := range cases {
		if got := ResolveRealm(c.explicit, c.domain); got != c.want {
			t.Errorf("ResolveRealm(%q,%q)=%q want %q", c.explicit, c.domain, got, c.want)
		}
	}
}
