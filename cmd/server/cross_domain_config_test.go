package main

import (
	"slices"
	"testing"
)

// TestCrossDomainFallbackDefaultOff 默认关闭（保守：不改变既有选域行为）。
func TestCrossDomainFallbackDefaultOff(t *testing.T) {
	t.Parallel()

	if Default().Pool.CrossDomainFallback {
		t.Fatal("pool.cross_domain_fallback 默认必须为 false")
	}
}

// TestCrossDomainFallbackHotApply 热生效：改动不进「需重启」清单。
func TestCrossDomainFallbackHotApply(t *testing.T) {
	t.Parallel()

	oldCfg := Default()
	newCfg := Default()
	newCfg.Pool.CrossDomainFallback = true

	if got := restartRequiredFields(oldCfg, newCfg); slices.Contains(got, "pool.cross_domain_fallback") {
		t.Fatalf("pool.cross_domain_fallback 应热生效: %v", got)
	}
}

// TestCrossDomainFallbackFromJSON 解析层读到该字段。
func TestCrossDomainFallbackFromJSON(t *testing.T) {
	t.Parallel()

	cfg, err := ParseConfig([]byte(`{"listen": ":7863", "pool": {"cross_domain_fallback": true}}`))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}

	if !cfg.Pool.CrossDomainFallback {
		t.Fatal("pool.cross_domain_fallback 应从 JSON 读到 true")
	}
}
