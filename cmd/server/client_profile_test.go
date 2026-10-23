package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// writeClientProfile 落一份最小合法 profile（版本 9.9.x 假值）。
func writeClientProfile(t *testing.T, dir, realm string) {
	t.Helper()

	host, ext := "download.codebuddy.cn", "-cccc.exe"
	if realm == "global" {
		host, ext = "download.codebuddy.ai", "-dddd.exe"
	}

	profile := map[string]any{
		"schema": 1,
		"source": map[string]any{
			"realm":          realm,
			"packageUrl":     "https://" + host + "/workbuddy/saas/win32-x64-user/WorkBuddy-win32-x64-user-9.9.9" + ext,
			"sha256":         strings.Repeat("a", 64),
			"packageVersion": "9.9.9",
			"version":        "9.9.9",
			"cliVersion":     "8.8.8",
			"commit":         strings.Repeat("b", 40),
			"releaseDate":    time.Now().UnixMilli(),
			"observedAt":     "2026-10-22T00:00:00Z",
			"generatedBy":    "reverse-workbuddy@test",
		},
		"emit": map[string]any{
			"chatUA":            "WorkBuddy/9.9.9 WorkBuddy/9.9.9 CLI/8.8.8",
			"desktopUA":         "WorkBuddy/9.9.9 WorkBuddy/9.9.9 CLI/8.8.8",
			"billingUA":         "WorkBuddy/9.9.9",
			"xIdeVersion":       "9.9.9",
			"desktopIdeVersion": "9.9.9",
			"desktopExtVersion": "9.9.9.7",
			"loginPlatform":     "workbuddy",
			"loginUA":           "WorkBuddy/9.9.9 WorkBuddy/9.9.9 CLI/8.8.8",
		},
		"missing": []string{},
		"evidence": map[string]any{
			"chatUA":            map[string]any{"source": "package", "file": "main/server.js", "snippet": "x"},
			"desktopUA":         map[string]any{"source": "package", "file": "main/module-base.js", "snippet": "x"},
			"billingUA":         map[string]any{"source": "package", "file": "main/tar.js", "snippet": "x"},
			"xIdeVersion":       map[string]any{"source": "package", "file": "main/server.js", "snippet": "x"},
			"desktopIdeVersion": map[string]any{"source": "package", "file": "main/module-base.js", "snippet": "x"},
			"desktopExtVersion": map[string]any{"source": "package", "file": "main/module-base.js", "snippet": "x"},
			"loginPlatform":     map[string]any{"source": "package", "file": "cli/product.json", "snippet": "x"},
			"loginUA":           map[string]any{"source": "package", "file": "main/module-base.js", "snippet": "x"},
		},
	}

	raw, err := json.Marshal(profile)
	if err != nil {
		t.Fatalf("marshal profile: %v", err)
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, realm+".json"), raw, 0o644); err != nil {
		t.Fatalf("write profile: %v", err)
	}
}

// TestLoadClientProfileRequiresCN 缺 cn profile 直接拒绝启动。
func TestLoadClientProfileRequiresCN(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	err := loadClientProfile(&Config{StateFile: filepath.Join(dir, "state.json")}, upstream.New())
	if err == nil || !strings.Contains(err.Error(), "cn.json") {
		t.Fatalf("err=%v want 缺失 cn.json", err)
	}
}

// TestLoadClientProfileGlobalOnlyWhenEnabled global profile 只在 global.enabled 时必需。
func TestLoadClientProfileGlobalOnlyWhenEnabled(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeClientProfile(t, filepath.Join(dir, "client-profiles"), "cn")

	cfg := &Config{StateFile: filepath.Join(dir, "state.json")}
	cfg.Global.Enabled = false

	if err := loadClientProfile(cfg, upstream.New()); err != nil {
		t.Fatalf("global 关闭时不应要求 global.json: %v", err)
	}

	cfg.Global.Enabled = true

	err := loadClientProfile(cfg, upstream.New())
	if err == nil || !strings.Contains(err.Error(), "global.json") {
		t.Fatalf("err=%v want 缺失 global.json", err)
	}

	writeClientProfile(t, filepath.Join(dir, "client-profiles"), "global")

	if err := loadClientProfile(cfg, upstream.New()); err != nil {
		t.Fatalf("两个 profile 齐全时应通过: %v", err)
	}
}
