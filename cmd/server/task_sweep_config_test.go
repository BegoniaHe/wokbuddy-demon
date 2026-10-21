package main

import (
	"slices"
	"testing"
	"time"
)

// TestTaskSweepDefaults 默认开启、15 分钟一轮。
func TestTaskSweepDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := ParseConfig([]byte(`{"listen": ":7863"}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if !cfg.Schedule.TaskSweepEnabled {
		t.Fatal("schedule.task_sweep_enabled 默认必须为 true")
	}

	if cfg.Schedule.TaskSweepMinutes != 15 {
		t.Fatalf("schedule.task_sweep_minutes=%d want 15", cfg.Schedule.TaskSweepMinutes)
	}

	// 派生间隔在 normalize 阶段计算（与 BalanceRefreshInterval 同口径）。
	if cfg.TaskSweepInterval != 15*time.Minute {
		t.Fatalf("TaskSweepInterval=%v want 15m", cfg.TaskSweepInterval)
	}
}

// TestTaskSweepNormalize 关闭时 interval 归零；分钟非法回落 15。
func TestTaskSweepNormalize(t *testing.T) {
	t.Parallel()

	cfg, err := ParseConfig([]byte(`{"listen": ":7863", "schedule": {"task_sweep_enabled": false}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if cfg.TaskSweepInterval != 0 {
		t.Fatalf("关闭时 interval 应为 0，got %v", cfg.TaskSweepInterval)
	}

	cfg, err = ParseConfig([]byte(`{"listen": ":7863", "schedule": {"task_sweep_minutes": -3}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if cfg.Schedule.TaskSweepMinutes != 15 || cfg.TaskSweepInterval != 15*time.Minute {
		t.Fatalf("非法分钟数应回落 15，got %d / %v", cfg.Schedule.TaskSweepMinutes, cfg.TaskSweepInterval)
	}
}

// TestTaskSweepHotApply 热生效：开关与间隔都不进「需重启」清单。
func TestTaskSweepHotApply(t *testing.T) {
	t.Parallel()

	oldCfg := Default()
	newCfg := Default()
	newCfg.Schedule.TaskSweepEnabled = false
	newCfg.Schedule.TaskSweepMinutes = 30

	got := restartRequiredFields(oldCfg, newCfg)
	if slices.Contains(got, "schedule.task_sweep_enabled") || slices.Contains(got, "schedule.task_sweep_minutes") {
		t.Fatalf("任务补领清扫应热生效，需重启清单=%v", got)
	}
}
