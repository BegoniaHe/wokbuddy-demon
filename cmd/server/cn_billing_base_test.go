package main

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// TestCNBillingBaseConfig cn.billing_base 默认必须为空：空 = 走内置默认
// workbuddy.cn（与官方 5.9.3 桌面一致），显式填 codebuddy.cn 才退回旧口径。
func TestCNBillingBaseConfig(t *testing.T) {
	t.Parallel()

	if base := Default().CN.BillingBase; base != "" {
		t.Fatalf("默认 cn.billing_base 必须为空（空 = 内置 workbuddy.cn），got %q", base)
	}
}

// TestCNBillingBaseDefaultIsWorkbuddyCN 内置默认 billing base 指向 workbuddy.cn。
func TestCNBillingBaseDefaultIsWorkbuddyCN(t *testing.T) {
	t.Parallel()

	client := upstream.New()
	if base := client.BillingBaseCN; base != "https://www.workbuddy.cn" {
		t.Fatalf("内置 BillingBaseCN=%q want https://www.workbuddy.cn", base)
	}
}

// TestCNBillingBaseRestartRequired cn.billing_base 属装配期字段：改动必须进
// 「需重启」清单，且不影响 global.billing_base。
func TestCNBillingBaseRestartRequired(t *testing.T) {
	t.Parallel()

	oldCfg := Default()
	newCfg := Default()
	newCfg.CN.BillingBase = "https://www.workbuddy.cn"

	got := restartRequiredFields(oldCfg, newCfg)
	if !slices.Contains(got, "cn.billing_base") {
		t.Fatalf("cn.billing_base 改动必须在需重启清单，got %v", got)
	}

	if slices.Contains(got, "global.billing_base") {
		t.Errorf("cn.billing_base 改动不应牵连 global.billing_base，got %v", got)
	}
}

// TestCNBillingBaseLoadFromJSON 解析层：cn.billing_base 只写入 CN 段。
func TestCNBillingBaseLoadFromJSON(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"listen": ":7863",
		"cn": {"billing_base": "https://www.workbuddy.cn"},
		"global": {"enabled": true, "billing_base": "https://global.example.test"}
	}`)

	cfg, err := ParseConfig(raw)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}

	if cfg.CN.BillingBase != "https://www.workbuddy.cn" {
		t.Errorf("cn.billing_base=%q want https://www.workbuddy.cn", cfg.CN.BillingBase)
	}

	if cfg.Global.BillingBase != "https://global.example.test" {
		t.Errorf("global.billing_base=%q 不应被 cn 段影响", cfg.Global.BillingBase)
	}
}

// TestCNBillingBaseOverridesClient 非空时覆盖 Client.BillingBaseCN，且不动 global。
func TestCNBillingBaseOverridesClient(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeClientProfile(t, filepath.Join(dir, "client-profiles"), "cn")
	writeClientProfile(t, filepath.Join(dir, "client-profiles"), "global")

	cfg := Default()
	cfg.StateFile = filepath.Join(dir, "state.json")
	cfg.CN.BillingBase = "https://www.workbuddy.cn"

	client := upstream.New()
	if err := configureUpstream(cfg, client); err != nil {
		t.Fatalf("configureUpstream: %v", err)
	}

	if got := client.BillingBaseCN; got != "https://www.workbuddy.cn" {
		t.Errorf("BillingBaseCN = %q want https://www.workbuddy.cn", got)
	}

	if got := client.BillingBaseGlobal; got != "" {
		t.Errorf("BillingBaseGlobal = %q 不应被 cn.billing_base 影响", got)
	}
}
