package main

import (
	"slices"
	"testing"
)

// TestCNChatBaseConfig CN Chat Base 覆盖：空值零回归、非空只影响 CN 域。
func TestCNChatBaseConfig(t *testing.T) {
	t.Parallel()

	if base := Default().CN.ChatBase; base != "" {
		t.Fatalf("默认 cn.chat_base 必须为空（回落内置默认 + 回落链），got %q", base)
	}
}

// TestCNChatBaseRestartRequired cn.chat_base 属装配期字段：改动必须进「需重启」清单，
// 且不影响 global 段。
func TestCNChatBaseRestartRequired(t *testing.T) {
	t.Parallel()

	oldCfg := Default()
	newCfg := Default()
	newCfg.CN.ChatBase = "https://cn.example.test"

	got := restartRequiredFields(oldCfg, newCfg)
	if !slices.Contains(got, "cn.chat_base") {
		t.Fatalf("cn.chat_base 改动必须在需重启清单，got %v", got)
	}

	if slices.Contains(got, "global.chat_base") {
		t.Errorf("cn.chat_base 改动不应牵连 global.chat_base，got %v", got)
	}
}

// TestCNChatBaseLoadFromJSON 解析层：cn.chat_base 读到 Config.CN，global 段保持不变。
func TestCNChatBaseLoadFromJSON(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"listen": ":7863",
		"cn": {"chat_base": "https://cn.example.test"},
		"global": {"enabled": true, "chat_base": "https://global.example.test"}
	}`)

	cfg, err := ParseConfig(raw)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}

	if cfg.CN.ChatBase != "https://cn.example.test" {
		t.Errorf("cn.chat_base=%q want https://cn.example.test", cfg.CN.ChatBase)
	}

	if cfg.Global.ChatBase != "https://global.example.test" {
		t.Errorf("global.chat_base=%q 不应被 cn 段影响", cfg.Global.ChatBase)
	}
}
