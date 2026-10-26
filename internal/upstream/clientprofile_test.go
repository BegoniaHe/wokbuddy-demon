package upstream

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/clientprofile"
)

const fakeCNProfile = `{
  "schema": 1,
  "source": {
    "realm": "cn",
    "packageUrl": "https://download.codebuddy.cn/workbuddy/saas/win32-x64-user/WorkBuddy-win32-x64-user-9.9.9-cccc.exe",
    "sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "packageVersion": "9.9.9",
    "version": "9.9.9",
    "cliVersion": "8.8.8",
    "commit": "cccccccccccccccccccccccccccccccccccccccc",
    "releaseDate": 1792000000000,
    "observedAt": "2026-10-22T00:00:00Z",
    "generatedBy": "reverse-workbuddy@test"
  },
  "emit": {
    "chatUA": "WorkBuddy/9.9.9 WorkBuddy/9.9.9 CLI/8.8.8",
    "desktopUA": "WorkBuddy/9.9.9 WorkBuddy/9.9.9 CLI/8.8.8",
    "billingUA": "WorkBuddy/9.9.9",
    "xIdeVersion": "9.9.9",
    "desktopIdeVersion": "9.9.9",
    "desktopExtVersion": "9.9.9",
    "loginPlatform": "workbuddy",
    "loginUA": null
  },
  "missing": ["loginUA"],
  "evidence": {
    "chatUA": {"source": "package", "file": "main/server.js", "snippet": "x"},
    "desktopUA": {"source": "package", "file": "main/module-base.js", "snippet": "x"},
    "billingUA": {"source": "package", "file": "main/tar.js", "snippet": "x"},
    "xIdeVersion": {"source": "package", "file": "main/server.js", "snippet": "x"},
    "desktopIdeVersion": {"source": "package", "file": "main/module-base.js", "snippet": "x"},
    "desktopExtVersion": {"source": "package", "file": "main/module-base.js", "snippet": "x"},
    "loginPlatform": {"source": "package", "file": "cli/product.json", "snippet": "x"}
  }
}`

// TestClientProfileOverridesFingerprint profile 存在时整份覆盖 UA / IDE 版本 /
// 构建指纹；desktopExtVersion 与 source.version 同源。
func TestClientProfileOverridesFingerprint(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cn.json"), []byte(fakeCNProfile), 0o644); err != nil {
		t.Fatalf("write profile: %v", err)
	}

	set, err := clientprofile.LoadDir(dir)
	if err != nil {
		t.Fatalf("load profile: %v", err)
	}

	client := New()
	client.Profile = set

	account := &auth.Auth{UID: "u-profile", AccessToken: "at"}

	if got, want := client.userAgent(account), "WorkBuddy/9.9.9 WorkBuddy/9.9.9 CLI/8.8.8"; got != want {
		t.Errorf("userAgent=%q want %q", got, want)
	}

	if got, want := client.billingUA(account), "WorkBuddy/9.9.9"; got != want {
		t.Errorf("billingUA=%q want %q", got, want)
	}

	if got, want := client.desktopUAFor(account), "WorkBuddy/9.9.9 WorkBuddy/9.9.9 CLI/8.8.8"; got != want {
		t.Errorf("desktopUA=%q want %q", got, want)
	}

	fingerprint := client.desktopFingerprint(account)
	if fingerprint["ideVersion"] != "9.9.9" || fingerprint["extVersion"] != "9.9.9" {
		t.Errorf("指纹版本 ide=%v ext=%v", fingerprint["ideVersion"], fingerprint["extVersion"])
	}

	if fingerprint["commit"] != "cccccccccccccccccccccccccccccccccccccccc" || fingerprint["releaseDate"] != int64(1792000000000) {
		t.Errorf("构建指纹 commit/date=%v/%v", fingerprint["commit"], fingerprint["releaseDate"])
	}
}

// TestClientProfileExplicitVersionWins 显式 client_version 必须压过 profile 字面量，
// 否则 UA 与 X-Ide-Version 会指向不同版本。
func TestClientProfileExplicitVersionWins(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cn.json"), []byte(fakeCNProfile), 0o644); err != nil {
		t.Fatalf("write profile: %v", err)
	}

	set, err := clientprofile.LoadDir(dir)
	if err != nil {
		t.Fatalf("load profile: %v", err)
	}

	client := New()
	client.Profile = set
	client.ClientVersion = "6.0.0"

	account := &auth.Auth{UID: "u-profile", AccessToken: "at"}

	if got, want := client.userAgent(account), "WorkBuddy/6.0.0 WorkBuddy/6.0.0 CLI/8.8.8"; got != want {
		t.Errorf("userAgent=%q want %q", got, want)
	}

	if got, want := client.billingUA(account), "WorkBuddy/6.0.0"; got != want {
		t.Errorf("billingUA=%q want %q", got, want)
	}
}

// TestClientProfileExplicitCLIWins 显式 cli_version 同理不得被 profile chatUA 覆盖。
func TestClientProfileExplicitCLIWins(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cn.json"), []byte(fakeCNProfile), 0o644); err != nil {
		t.Fatalf("write profile: %v", err)
	}

	set, err := clientprofile.LoadDir(dir)
	if err != nil {
		t.Fatalf("load profile: %v", err)
	}

	client := New()
	client.Profile = set
	client.CliVersion = "7.7.7"

	account := &auth.Auth{UID: "u-profile", AccessToken: "at"}

	if got, want := client.userAgent(account), "WorkBuddy/9.9.9 WorkBuddy/9.9.9 CLI/7.7.7"; got != want {
		t.Errorf("userAgent=%q want %q", got, want)
	}

	if got := client.UserAgent; got != "" {
		t.Errorf("unexpected explicit user agent %q", got)
	}

	if got := client.billingUA(account); !strings.Contains(got, "9.9.9") {
		t.Errorf("billingUA=%q 应继续使用 profile 版本", got)
	}
}
