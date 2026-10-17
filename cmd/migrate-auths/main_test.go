package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
	"unicode/utf8"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// legacySample 复刻旧版 login.sh / 早期导出工具的 snake_case 扁平形落盘内容。
const legacySample = `{
  "uid": "fa1380f4-df9a-4b31-8027-576a51b512fc",
  "nickname": "70983726",
  "enterprise_id": "",
  "realm": "cn",
  "domain": "www.codebuddy.cn",
  "access_token": "at",
  "refresh_token": "rt",
  "token_type": "Bearer",
  "expires_at": 1794642387000,
  "expires_at_iso": "2026-11-14T07:46:27Z",
  "expires_in": 2417787,
  "access_token_valid": true,
  "token_parse_ok": true,
  "source_file": "workbuddy-fa1380f4-df9a-4b31-8027-576a51b512fc.json"
}`

// currentSample 当前格式（嵌套形，与 auth.SaveAtomic 输出一致）。
const currentSample = `{
  "auth": {
    "accessToken": "at",
    "refreshToken": "rt",
    "expiresAt": 1794642387,
    "domain": "www.codebuddy.cn",
    "realm": "cn"
  },
  "account": {
    "uid": "fa1380f4-df9a-4b31-8027-576a51b512fc",
    "enterpriseId": "",
    "nickname": "70983726"
  }
}`

func TestConvertLegacy(t *testing.T) {
	a, err := convertLegacy([]byte(legacySample))
	if err != nil {
		t.Fatalf("convertLegacy: %v", err)
	}
	if a.UID != "fa1380f4-df9a-4b31-8027-576a51b512fc" {
		t.Errorf("uid = %q", a.UID)
	}
	if a.AccessToken != "at" || a.RefreshToken != "rt" {
		t.Errorf("tokens = %q/%q", a.AccessToken, a.RefreshToken)
	}
	if a.ExpiresAt != 1794642387 {
		t.Errorf("expiresAt = %d, want 1794642387（毫秒转秒）", a.ExpiresAt)
	}
	if a.Domain != "www.codebuddy.cn" {
		t.Errorf("domain = %q", a.Domain)
	}
	if a.Realm() != "cn" {
		t.Errorf("realm = %q", a.Realm())
	}
	if a.Nickname != "70983726" {
		t.Errorf("nickname = %q", a.Nickname)
	}
}

func TestConvertLegacyRealmInfer(t *testing.T) {
	tests := []struct {
		name   string
		realm  string
		domain string
		iss    string
		want   string
	}{
		{name: "显式 global", realm: "global", domain: "www.codebuddy.cn", want: "global"},
		{name: "显式 cn", realm: "cn", domain: "www.codebuddy.cn", want: "cn"},
		// Realm() 语义：仅显式 global 优先；显式 cn 不覆盖 global domain（D1）。
		{name: "显式 cn + global domain", realm: "cn", domain: "www.workbuddy.ai", want: "global"},
		{name: "domain 推断 global", domain: "www.workbuddy.ai", want: "global"},
		{name: "domain 推断 cn", domain: "www.codebuddy.cn", want: "cn"},
		{name: "iss 兜底 global", iss: "https://www.workbuddy.ai/auth/realms/copilot", want: "global"},
		{name: "iss 兜底 cn", iss: "https://www.codebuddy.cn/auth/realms/copilot", want: "cn"},
		{name: "全缺失回落 cn", want: "cn"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			at := "at"
			if tc.iss != "" {
				at = fakeJWT(tc.iss)
			}
			raw := `{"uid":"u1","access_token":"` + at + `","refresh_token":"rt",` +
				`"realm":"` + tc.realm + `","domain":"` + tc.domain + `"}`
			a, err := convertLegacy([]byte(raw))
			if err != nil {
				t.Fatalf("convertLegacy: %v", err)
			}
			if got := a.Realm(); got != tc.want {
				t.Errorf("realm = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestConvertLegacyInvalid(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{name: "非 JSON", raw: `not json`},
		{name: "缺 uid", raw: `{"access_token":"at","refresh_token":"rt"}`},
		{name: "缺 access_token", raw: `{"uid":"u1","refresh_token":"rt"}`},
		{name: "缺 refresh_token", raw: `{"uid":"u1","access_token":"at"}`},
		{name: "realm 非法", raw: `{"uid":"u1","access_token":"at","refresh_token":"rt","realm":"mars"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := convertLegacy([]byte(tc.raw)); err == nil {
				t.Fatal("want error, got nil")
			}
		})
	}
}

func TestNormalizeExpiry(t *testing.T) {
	tests := []struct{ in, want int64 }{
		{0, 0},
		{1794642387, 1794642387},
		{1794642387000, 1794642387},
		{100_000_000_000, 100_000_000_000},
		{100_000_000_001, 100_000_000},
	}
	for _, tc := range tests {
		if got := normalizeExpiry(tc.in); got != tc.want {
			t.Errorf("normalizeExpiry(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestMigrateFileApply(t *testing.T) {
	dir := t.TempDir()
	path := writeTemp(t, dir, "workbuddy-70983726.json", legacySample)

	requireStatus(t, migrateFile(path, false), statusWould)
	requireContent(t, path, legacySample)

	requireStatus(t, migrateFile(path, true), statusMigrated)
	a, err := auth.Parse([]byte(readTemp(t, path)))
	if err != nil {
		t.Fatalf("迁移产物无法被 auth.Parse 解析: %v", err)
	}
	if a.UID != "fa1380f4-df9a-4b31-8027-576a51b512fc" || a.ExpiresAt != 1794642387 {
		t.Errorf("迁移产物字段不符: uid=%q expiresAt=%d", a.UID, a.ExpiresAt)
	}
	requireContent(t, path+".legacy", legacySample)

	// 幂等：重跑应判定为当前格式。
	requireStatus(t, migrateFile(path, true), statusCurrent)
}

func TestMigrateFileCurrent(t *testing.T) {
	dir := t.TempDir()
	path := writeTemp(t, dir, "workbuddy-u1.json", currentSample)
	r := migrateFile(path, true)
	requireStatus(t, r, statusCurrent)
	if r.uid != "fa1380f4-df9a-4b31-8027-576a51b512fc" || r.nick != "70983726" {
		t.Errorf("当前格式应回填 uid/nick 供重复检测: uid=%q nick=%q", r.uid, r.nick)
	}
	if _, err := os.Stat(path + ".legacy"); !os.IsNotExist(err) {
		t.Error("当前格式不应产生 .legacy 备份")
	}
	requireContent(t, path, currentSample)
}

func TestWriteBackup(t *testing.T) {
	dir := t.TempDir()
	path := writeTemp(t, dir, "workbuddy-u1.json", "current")

	if err := writeBackup(path, []byte("v1")); err != nil {
		t.Fatalf("首次备份: %v", err)
	}
	if err := writeBackup(path, []byte("v1")); err != nil {
		t.Fatalf("同内容重跑应幂等: %v", err)
	}
	if err := writeBackup(path, []byte("v2")); err == nil {
		t.Fatal("内容不同应拒绝覆盖既有备份")
	}
	requireContent(t, path+".legacy", "v1")
}

func TestMigrateFileBackupConflict(t *testing.T) {
	dir := t.TempDir()
	path := writeTemp(t, dir, "workbuddy-70983726.json", legacySample)
	writeTemp(t, dir, "workbuddy-70983726.json.legacy", "other original")

	requireStatus(t, migrateFile(path, true), statusFailed)
	requireContent(t, path, legacySample)
}

func TestTruncUTF8(t *testing.T) {
	got := trunc("中文昵称", 2)
	if got != "中文" || !utf8.ValidString(got) {
		t.Errorf("trunc = %q（valid=%v），应按字符截断", got, utf8.ValidString(got))
	}
}

func writeTemp(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readTemp(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func requireStatus(t *testing.T, r row, want string) {
	t.Helper()
	if r.status != want {
		t.Fatalf("status = %s, want %s（detail: %s）", r.status, want, r.detail)
	}
}

func requireContent(t *testing.T, path, want string) {
	t.Helper()
	if got := readTemp(t, path); got != want {
		t.Fatalf("%s 内容不符（want %d bytes, got %d bytes）", path, len(want), len(got))
	}
}

// fakeJWT 构造仅含 iss claim 的 JWT（不校验签名，仅供 realm 兜底推断）。
func fakeJWT(iss string) string {
	h := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256"}`))
	p := base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"` + iss + `"}`))
	return h + "." + p + ".sig"
}
