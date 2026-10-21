package main

import (
	"slices"
	"strings"
	"testing"
)

// TestModelsHiddenParse models.hidden 解析与归一：去空白、去重、拒绝前缀后空模型名。
func TestModelsHiddenParse(t *testing.T) {
	t.Parallel()

	cfg, err := ParseConfig([]byte(`{
		"listen": ":7863",
		"models": {"hidden": [" glm-5.2 ", "cn:kimi-k3", "glm-5.2", ""]}
	}`))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}

	if got := cfg.Models.Hidden; len(got) != 2 || got[0] != "glm-5.2" || got[1] != "cn:kimi-k3" {
		t.Fatalf("models.hidden=%v want [glm-5.2 cn:kimi-k3]", got)
	}
}

// TestModelsHiddenRejectsEmptyAfterPrefix "cn:" 单独出现会隐藏整域，必须在启动期拒绝。
func TestModelsHiddenRejectsEmptyAfterPrefix(t *testing.T) {
	t.Parallel()

	_, err := ParseConfig([]byte(`{"listen": ":7863", "models": {"hidden": ["cn:"]}}`))
	if err == nil {
		t.Fatal("want error for empty model name after realm prefix")
	}

	if !strings.Contains(err.Error(), "models.hidden") {
		t.Errorf("err=%v 应指明 models.hidden", err)
	}
}

// TestModelsHiddenHotApply 热生效：models.hidden 改动不进「需重启」清单。
func TestModelsHiddenHotApply(t *testing.T) {
	t.Parallel()

	oldCfg := Default()
	newCfg := Default()
	newCfg.Models.Hidden = []string{"glm-5.2"}

	if got := restartRequiredFields(oldCfg, newCfg); slices.Contains(got, "models.hidden") {
		t.Fatalf("models.hidden 应热生效，不应进需重启清单: %v", got)
	}
}
