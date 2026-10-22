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
	config.Schedule.AccountPacingSigma = 0
	config.Schedule.AccountPhaseWindow = "0"

	if err := config.normalize(); err != nil {
		t.Fatalf("zero: %v", err)
	}

	if config.Timing.SlotJitter != 0 || config.Timing.PacingSigma != 0 || config.Timing.PhaseWindow != 0 {
		t.Fatalf("timing=%+v want all zero", config.Timing)
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
		{"sigma 过大", func(c *Config) { c.Schedule.AccountPacingSigma = 1.6 }},
		{"sigma 负数", func(c *Config) { c.Schedule.AccountPacingSigma = -0.1 }},
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
