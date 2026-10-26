package clientprofile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func validProfile(realm string) *Profile {
	host := map[string]string{"cn": "download.codebuddy.cn", "global": "download.codebuddy.ai"}[realm]
	login := map[string]string{"cn": "workbuddy", "global": "workbuddy-ai"}[realm]

	return &Profile{
		Schema: schemaVersion,
		Source: Source{
			Realm:          realm,
			PackageURL:     "https://" + host + "/workbuddy/saas/win32-x64-user/WorkBuddy-win32-x64-user-0.0.0-aaaa.exe",
			SHA256:         strings.Repeat("a", 64),
			PackageVersion: "0.0.0",
			Version:        "0.0.0",
			CLIVersion:     "0.0.0",
			Commit:         strings.Repeat("b", 40),
			ReleaseDate:    time.Now().UnixMilli(),
			ObservedAt:     "2026-10-22T00:00:00Z",
			GeneratedBy:    "reverse-workbuddy@test",
		},
		Emit: Emit{
			ChatUA:            "WorkBuddy/0.0.0 WorkBuddy/0.0.0 CLI/0.0.0",
			DesktopUA:         "WorkBuddy/0.0.0 WorkBuddy/0.0.0 CLI/0.0.0",
			BillingUA:         "WorkBuddy/0.0.0",
			XIdeVersion:       "0.0.0",
			DesktopIdeVersion: "0.0.0",
			DesktopExtVersion: "0.0.0",
			LoginPlatform:     login,
		},
		Missing: []string{"loginUA"},
		Evidence: map[string]Evidence{
			"chatUA":            {Source: "package", File: "main/server.js", Snippet: "chat"},
			"desktopUA":         {Source: "package", File: "main/module-base.js", Snippet: "desktop"},
			"billingUA":         {Source: "package", File: "main/tar.js", Snippet: "billing"},
			"xIdeVersion":       {Source: "package", File: "main/server.js", Snippet: "xide"},
			"desktopIdeVersion": {Source: "package", File: "main/module-base.js", Snippet: "ide"},
			"desktopExtVersion": {Source: "package", File: "main/module-base.js", Snippet: "ext"},
			"loginPlatform":     {Source: "package", File: "cli/product.json", Snippet: "platform"},
		},
	}
}

func writeProfile(t *testing.T, dir, name string, profile *Profile) string {
	t.Helper()

	raw, err := json.Marshal(profile)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	return path
}

func TestLoadDirMissing(t *testing.T) {
	t.Parallel()

	set, err := LoadDir(filepath.Join(t.TempDir(), "missing"))
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}

	if !set.Empty() {
		t.Fatal("缺失目录应为空集合")
	}
}

func TestLoadDirPerRealm(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeProfile(t, dir, "cn.json", validProfile("cn"))
	writeProfile(t, dir, "global.json", validProfile("global"))

	set, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}

	if set.ForRealm("cn") == nil || set.ForRealm("global") == nil {
		t.Fatalf("两个 realm 都应加载: %+v", set)
	}

	if got := set.ForRealm("cn").Emit.LoginPlatform; got != "workbuddy" {
		t.Errorf("cn loginPlatform=%q", got)
	}

	if got := set.ForRealm("global").Emit.LoginPlatform; got != "workbuddy-ai" {
		t.Errorf("global loginPlatform=%q", got)
	}
}

func TestLoadDirRejectsRealmMismatch(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeProfile(t, dir, "cn.json", validProfile("global"))

	if _, err := LoadDir(dir); err == nil {
		t.Fatal("文件名与 source.realm 不一致应报错")
	}
}

func TestValidateRejectsUnsafeValues(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		edit func(*Profile)
	}{
		{"控制字符", func(p *Profile) { p.Emit.ChatUA += "\n" }},
		{"loginUA 控制字符", func(p *Profile) {
			ua := "WorkBuddy/0.0.0\r\nX-Injected: 1"
			p.Emit.LoginUA = &ua
		}},
		{"loginUA 空串", func(p *Profile) {
			ua := ""
			p.Emit.LoginUA = &ua
		}},
		{"extVersion 同前缀但不同值", func(p *Profile) { p.Emit.DesktopExtVersion = "0.0.0.1" }},
		{"macOS 包", func(p *Profile) {
			p.Source.PackageURL = "https://download.codebuddy.cn/workbuddy/saas/darwin-arm64/x.dmg"
		}},
		{"非 workbuddy/saas 路径", func(p *Profile) {
			p.Source.PackageURL = "https://download.codebuddy.cn/other/win32-x64-user/x.exe"
		}},
		{"非 https 安装包", func(p *Profile) {
			p.Source.PackageURL = "http://download.codebuddy.cn/workbuddy/saas/win32-x64-user/x.exe"
		}},
		{"非 exe 安装包", func(p *Profile) {
			p.Source.PackageURL = "https://download.codebuddy.cn/workbuddy/saas/win32-x64-user/x.zip"
		}},
		{"空必需 surface", func(p *Profile) { p.Emit.DesktopUA = "" }},
		{"missing 不一致", func(p *Profile) { p.Missing = nil }},
		{"证据路径穿越", func(p *Profile) {
			ev := p.Evidence["chatUA"]
			ev.File = "../main/server.js"
			p.Evidence["chatUA"] = ev
		}},
		{"未知来源拒绝", func(p *Profile) {
			p.Evidence["chatUA"] = Evidence{Source: "guessed"}
		}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			profile := validProfile("cn")
			testCase.edit(profile)

			if err := profile.Validate(); err == nil {
				t.Fatal("应校验失败")
			}
		})
	}
}

// TestValidateDesktopExtVersionExact 官方 pluginVersion 恒取 resolveProductVersion，
// 故 desktopExtVersion 必须与 source.version 完全相等；desktopIdeVersion 仍允许
// 带独立 platformVersion 后缀（只要求包含 source.version）。
func TestValidateDesktopExtVersionExact(t *testing.T) {
	t.Parallel()

	profile := validProfile("cn")
	profile.Emit.DesktopExtVersion = "0.0.0.1"

	if err := profile.Validate(); err == nil {
		t.Fatal("desktopExtVersion 同前缀但不同值应报错")
	}

	profile = validProfile("cn")
	profile.Emit.DesktopIdeVersion = "0.0.0.1"

	if err := profile.Validate(); err != nil {
		t.Fatalf("desktopIdeVersion 允许独立 platformVersion: %v", err)
	}
}

// TestValidateTrafficEvidenceByNote traffic 来源用 note 记日期与端点，不依赖包内文件。
func TestValidateTrafficEvidenceByNote(t *testing.T) {
	t.Parallel()

	profile := validProfile("cn")
	profile.Evidence["chatUA"] = Evidence{Source: "traffic", Note: "2026-10-22 POST /v2/chat"}

	if err := profile.Validate(); err != nil {
		t.Fatalf("traffic+note 应合法: %v", err)
	}

	profile.Evidence["chatUA"] = Evidence{Source: "traffic"}
	if err := profile.Validate(); err == nil {
		t.Fatal("traffic 缺 note 应报错")
	}
}

func TestLoadFileRejectsUnknownField(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(validProfile("cn"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	raw = []byte(strings.Replace(string(raw), `"schema":1`, `"schema":1,"extra":1`, 1))

	path := filepath.Join(t.TempDir(), "cn.json")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := LoadFile(path); err == nil {
		t.Fatal("未知字段应报错")
	}
}

func TestValidateLoginUANull(t *testing.T) {
	t.Parallel()

	profile := validProfile("cn")
	if err := profile.Validate(); err != nil {
		t.Fatalf("loginUA=null 应合法: %v", err)
	}

	ua := "Mozilla/5.0"
	profile.Emit.LoginUA = &ua
	profile.Missing = nil
	profile.Evidence["loginUA"] = Evidence{Source: "package", File: "main/login.js", Snippet: "ua"}

	if err := profile.Validate(); err != nil {
		t.Fatalf("loginUA 非 null 应合法: %v", err)
	}
}

func TestLoadFileFallsBackToBakOnCorruptJSON(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeProfile(t, dir, "cn.json.bak", validProfile("cn"))

	if err := os.WriteFile(filepath.Join(dir, "cn.json"), []byte(`{"schema":`), 0o644); err != nil {
		t.Fatalf("write corrupt: %v", err)
	}

	profile, err := LoadFile(filepath.Join(dir, "cn.json"))
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}

	if profile.Source.Realm != "cn" {
		t.Errorf("realm=%q want cn（应回退 .bak）", profile.Source.Realm)
	}
}

func TestLoadFileRejectsSemanticInvalidEvenWithBak(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeProfile(t, dir, "cn.json.bak", validProfile("cn"))

	broken := validProfile("cn")
	broken.Emit.DesktopUA = ""

	writeProfile(t, dir, "cn.json", broken)

	if _, err := LoadFile(filepath.Join(dir, "cn.json")); err == nil {
		t.Fatal("语义无效的正文不得回退 .bak，应直接报错")
	}
}
