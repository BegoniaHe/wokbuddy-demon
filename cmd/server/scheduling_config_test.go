package main

import (
	"slices"
	"testing"
)

// TestSchedulingDefaults 默认 weighted_random（零回归）。
func TestSchedulingDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := ParseConfig([]byte(`{"listen": ":7863"}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if cfg.Pool.Strategy.CN != "weighted_random" || cfg.Pool.Strategy.Global != "weighted_random" {
		t.Fatalf("默认策略=%q/%q want weighted_random", cfg.Pool.Strategy.CN, cfg.Pool.Strategy.Global)
	}
}

// TestSchedulingParseAndNormalize 解析 + 归一（账号顺序去空白/去重）。
func TestSchedulingParseAndNormalize(t *testing.T) {
	t.Parallel()

	cfg, err := ParseConfig([]byte(`{
		"listen": ":7863",
		"pool": {
			"strategy": {"cn": "round_robin", "global": "SEQUENTIAL_EXHAUST"},
			"account_order": {"cn": [" u2 ", "u1", "u2", ""]}
		}
	}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if cfg.Pool.Strategy.CN != "round_robin" || cfg.Pool.Strategy.Global != "sequential_exhaust" {
		t.Fatalf("策略归一=%q/%q", cfg.Pool.Strategy.CN, cfg.Pool.Strategy.Global)
	}

	if want := []string{"u2", "u1"}; !slices.Equal(cfg.Pool.AccountOrder.CN, want) {
		t.Fatalf("account_order.cn=%v want %v", cfg.Pool.AccountOrder.CN, want)
	}
}

// TestSchedulingRejectsUnknownStrategy 拼错策略必须拒启动（静默退回默认更危险）。
func TestSchedulingRejectsUnknownStrategy(t *testing.T) {
	t.Parallel()

	if _, err := ParseConfig([]byte(`{"listen": ":7863", "pool": {"strategy": {"cn": "round-robbin"}}}`)); err == nil {
		t.Fatal("非法策略值应拒绝启动")
	}
}

// TestSchedulingHotApply 策略与顺序都热生效（不进需重启清单）。
func TestSchedulingHotApply(t *testing.T) {
	t.Parallel()

	oldCfg := Default()
	newCfg := Default()
	newCfg.Pool.Strategy.CN = "round_robin"
	newCfg.Pool.AccountOrder.CN = []string{"u1"}

	got := restartRequiredFields(oldCfg, newCfg)
	for _, field := range []string{"pool.strategy.cn", "pool.account_order.cn"} {
		if slices.Contains(got, field) {
			t.Fatalf("%s 应热生效，需重启清单=%v", field, got)
		}
	}
}
