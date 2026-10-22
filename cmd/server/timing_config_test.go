package main

import (
	"testing"
	"time"
)

// TestTimingShapeConfigDefaults 新键默认值与 config.example.json 一致。
func TestTimingShapeConfigDefaults(t *testing.T) {
	t.Parallel()

	config := Default()
	if err := config.normalize(); err != nil {
		t.Fatalf("default: %v", err)
	}

	if config.Timing.SlotJitter != 20*time.Minute {
		t.Fatalf("slot_jitter=%v want 20m", config.Timing.SlotJitter)
	}

	if config.Timing.PacingSigma != 0.4 {
		t.Fatalf("sigma=%v want 0.4", config.Timing.PacingSigma)
	}

	if config.Timing.PacingFloor != 250*time.Millisecond || config.Timing.PacingCeiling != 6*time.Second {
		t.Fatalf("clamp=%v~%v want 250ms~6s", config.Timing.PacingFloor, config.Timing.PacingCeiling)
	}

	if config.Timing.PhaseWindow != 10*time.Minute {
		t.Fatalf("phase_window=%v want 10m", config.Timing.PhaseWindow)
	}
}

// TestTimingShapeConfigZero 全 0 配置合法并完整回到旧行为。
func TestTimingShapeConfigZero(t *testing.T) {
	t.Parallel()

	config := Default()
	config.Schedule.SlotJitter = "0"
	zero := 0.0
	config.Schedule.AccountPacingSigma = &zero
	config.Schedule.AccountPhaseWindow = "0"

	if err := config.normalize(); err != nil {
		t.Fatalf("zero: %v", err)
	}

	if config.Timing.SlotJitter != 0 || config.Timing.PacingSigma != 0 || config.Timing.PhaseWindow != 0 {
		t.Fatalf("timing=%+v want all zero", config.Timing)
	}
}

// TestTimingShapeConfigImplicitClampAdapts 旧配置的 checkin_account_delay
// 超出默认夹逼区间时，缺省一侧自动让位而不是拒绝启动。
func TestTimingShapeConfigImplicitClampAdapts(t *testing.T) {
	t.Parallel()

	config := Default()
	config.Schedule.CheckinAccountDelay = "100ms"
	config.Schedule.AccountPacingFloor = ""

	if err := config.normalize(); err != nil {
		t.Fatalf("median 100ms 不应报错: %v", err)
	}

	if config.Timing.PacingFloor != 100*time.Millisecond {
		t.Fatalf("floor=%v want 100ms（默认值向 median 让位）", config.Timing.PacingFloor)
	}

	config = Default()
	config.Schedule.CheckinAccountDelay = "10s"
	config.Schedule.AccountPacingCeiling = ""

	if err := config.normalize(); err != nil {
		t.Fatalf("median 10s 不应报错: %v", err)
	}

	if config.Timing.PacingCeiling != 10*time.Second {
		t.Fatalf("ceiling=%v want 10s（默认值向 median 让位）", config.Timing.PacingCeiling)
	}
}

// TestTimingShapeConfigImplicitSideYieldsToExplicit 显式一侧优先：
// 显式 floor 抬高时隐式 ceiling 跟随，显式 ceiling 压低时隐式 floor 跟随。
func TestTimingShapeConfigImplicitSideYieldsToExplicit(t *testing.T) {
	t.Parallel()

	config := Default()
	config.Schedule.CheckinAccountDelay = "8s"
	config.Schedule.AccountPacingFloor = "7s"
	config.Schedule.AccountPacingCeiling = ""

	if err := config.normalize(); err != nil {
		t.Fatalf("显式 floor 7s 不应被隐式 ceiling 6s 拒绝: %v", err)
	}

	if config.Timing.PacingCeiling != 8*time.Second {
		t.Fatalf("ceiling=%v want 8s", config.Timing.PacingCeiling)
	}

	config = Default()
	config.Schedule.CheckinAccountDelay = "100ms"
	config.Schedule.AccountPacingFloor = ""
	config.Schedule.AccountPacingCeiling = "100ms"

	if err := config.normalize(); err != nil {
		t.Fatalf("显式 ceiling 100ms 不应被隐式 floor 250ms 拒绝: %v", err)
	}

	if config.Timing.PacingFloor != 100*time.Millisecond {
		t.Fatalf("floor=%v want 100ms", config.Timing.PacingFloor)
	}
}

// TestTimingShapeConfigMissingSigmaDefaults 旧配置缺 account_pacing_sigma 键时
// 应补默认 0.4（显式 0 仍在 TestTimingShapeConfigZero 中验证为关闭）。
func TestTimingShapeConfigMissingSigmaDefaults(t *testing.T) {
	t.Parallel()

	config := Default()
	config.Schedule.AccountPacingSigma = nil

	if err := config.normalize(); err != nil {
		t.Fatalf("sigma 缺键: %v", err)
	}

	if config.Schedule.AccountPacingSigma == nil || *config.Schedule.AccountPacingSigma != 0.4 {
		t.Fatalf("sigma=%v want 0.4", config.Schedule.AccountPacingSigma)
	}

	if config.Timing.PacingSigma != 0.4 {
		t.Fatalf("effective sigma=%v want 0.4", config.Timing.PacingSigma)
	}
}

// TestTimingShapeConfigRejectsInvalid 非法值报错而不静默回落。
func TestTimingShapeConfigRejectsInvalid(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		apply func(*Config)
	}{
		{"jitter 超 59m", func(c *Config) { c.Schedule.SlotJitter = "60m" }},
		{"jitter 负数", func(c *Config) { c.Schedule.SlotJitter = "-1s" }},
		{"jitter 非法", func(c *Config) { c.Schedule.SlotJitter = "abc" }},
		{"sigma 过大", func(c *Config) {
			sigma := 1.6
			c.Schedule.AccountPacingSigma = &sigma
		}},
		{"sigma 负数", func(c *Config) {
			sigma := -0.1
			c.Schedule.AccountPacingSigma = &sigma
		}},
		{"floor 大于 median", func(c *Config) { c.Schedule.AccountPacingFloor = "2s" }},
		{"ceiling 小于 median", func(c *Config) { c.Schedule.AccountPacingCeiling = "100ms" }},
		{"floor 大于 ceiling", func(c *Config) {
			c.Schedule.CheckinAccountDelay = "3s"
			c.Schedule.AccountPacingFloor = "4s"
			c.Schedule.AccountPacingCeiling = "2s"
		}},
		{"phase 负数", func(c *Config) { c.Schedule.AccountPhaseWindow = "-1m" }},
	}

	for _, tc := range cases {
		config := Default()
		tc.apply(config)

		if err := config.normalize(); err == nil {
			t.Errorf("%s: normalize 应报错", tc.name)
		}
	}
}

// TestTimingShapeConfigEmptyFallsBack 显式空串回落默认。
func TestTimingShapeConfigEmptyFallsBack(t *testing.T) {
	t.Parallel()

	config := Default()
	config.Schedule.SlotJitter = ""
	config.Schedule.AccountPacingFloor = ""
	config.Schedule.AccountPacingCeiling = ""
	config.Schedule.AccountPhaseWindow = ""

	if err := config.normalize(); err != nil {
		t.Fatalf("empty: %v", err)
	}

	if config.Timing.SlotJitter != 20*time.Minute || config.Timing.PhaseWindow != 10*time.Minute {
		t.Fatalf("timing=%+v", config.Timing)
	}
}
