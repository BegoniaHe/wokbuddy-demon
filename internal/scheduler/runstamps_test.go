package scheduler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/cst"
)

func TestMissedTodayAndRunStamp(t *testing.T) {
	t.Parallel()

	scheduler := New(&Config{
		CheckinHours:      []int{9, 21},
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		BlackcatDisabled:  true,
		GrowthDisabled:    true,
		RunStampFile:      filepath.Join(t.TempDir(), "scheduler-runs.json"),
	})

	now := time.Date(2026, 10, 21, 22, 0, 0, 0, time.Local)

	missed := scheduler.missedToday(now)
	if len(missed) != 1 || missed[0] != taskCheckin {
		t.Fatalf("missed=%v want [checkin]", missed)
	}

	if scheduler.ranToday(taskCheckin, now) {
		t.Fatal("should not be marked before markRan")
	}

	scheduler.markRan(missed, now)

	if !scheduler.ranToday(taskCheckin, now) {
		t.Fatal("markRan not persisted")
	}

	if got := scheduler.missedToday(now); len(got) != 0 {
		t.Fatalf("same day should not repeat: %v", got)
	}

	// 次日的时点尚未到，不补跑。
	next := now.Add(24 * time.Hour)
	if got := scheduler.missedToday(next); len(got) != 1 || got[0] != taskCheckin {
		t.Fatalf("next day 22:00 should miss again, got %v", got)
	}
}

// TestBlackcatRunStampUsesCSTDay 夜猫子补跑标记按 CST 日界，避免 UTC+14 等
// 宿主在窗口内重启时把同一晚重复补跑。
func TestBlackcatRunStampUsesCSTDay(t *testing.T) {
	t.Parallel()

	scheduler := New(&Config{RunStampFile: filepath.Join(t.TempDir(), "scheduler-runs.json")})
	host := time.FixedZone("UTC+14", 14*60*60)

	run := time.Date(2026, 10, 23, 5, 0, 0, 0, host) // CST 2026-10-22 23:00
	scheduler.markRan([]taskKind{taskBlackcat}, run)

	later := time.Date(2026, 10, 23, 5, 30, 0, 0, host) // CST 23:30 同一晚
	if !scheduler.ranToday(taskBlackcat, later) {
		t.Fatal("same CST night must stay marked")
	}

	next := time.Date(2026, 10, 24, 5, 0, 0, 0, host) // CST 2026-10-23 23:00
	if scheduler.ranToday(taskBlackcat, next) {
		t.Fatal("next CST night must not be marked")
	}
}

// TestBlackcatRunStampEarlyMorningDoesNotBlockNextNight 凌晨补跑属于前一晚的窗口，
// 不能把标记记到新日历日；否则当晚 23:00 停机后重启会误判「今晚已补跑」。
func TestBlackcatRunStampEarlyMorningDoesNotBlockNextNight(t *testing.T) {
	t.Parallel()

	scheduler := New(&Config{RunStampFile: filepath.Join(t.TempDir(), "scheduler-runs.json")})

	early := time.Date(2026, 10, 23, 2, 0, 0, 0, cst.Loc) // 属于 10-22 23:00 开启的窗口
	scheduler.markRan([]taskKind{taskBlackcat}, early)

	if !scheduler.ranToday(taskBlackcat, early.Add(2*time.Hour)) {
		t.Fatal("same window must stay marked")
	}

	nextNight := time.Date(2026, 10, 23, 23, 30, 0, 0, cst.Loc)
	if scheduler.ranToday(taskBlackcat, nextNight) {
		t.Fatal("early-morning catch-up must not mark the next night")
	}
}

// TestRunStampUsesSchedulingDay 补跑标记与排程同用 CST 日界（issue #252）：
// 宿主 UTC 日凌晨/晚上属于不同 CST 日，不能沿用旧的宿主本地日界。
func TestRunStampUsesSchedulingDay(t *testing.T) {
	t.Parallel()

	scheduler := New(&Config{RunStampFile: filepath.Join(t.TempDir(), "scheduler-runs.json")})

	early := time.Date(2026, 10, 21, 5, 0, 0, 0, time.UTC)  // CST 10-21 13:00
	later := time.Date(2026, 10, 21, 20, 0, 0, 0, time.UTC) // CST 10-22 04:00

	scheduler.markRan([]taskKind{taskCheckin}, early)

	if scheduler.ranToday(taskCheckin, later) {
		t.Fatal("different CST day must not stay marked")
	}

	if !scheduler.ranToday(taskCheckin, early.Add(2*time.Hour)) {
		t.Fatal("same CST day must stay marked")
	}
}

// TestMissedTodayBlackcatWithinWindow 跨午夜窗口内启动：今天的 23:00 还没到，
// 但上一晚的补跑不能漏（窗口仍开）。标记后同日不重复。
func TestMissedTodayBlackcatWithinWindow(t *testing.T) {
	t.Parallel()

	scheduler := New(&Config{
		BlackcatHours:     []int{23},
		CheckinDisabled:   true,
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		GrowthDisabled:    true,
		RunStampFile:      filepath.Join(t.TempDir(), "scheduler-runs.json"),
	})

	inWindow := time.Date(2026, 10, 22, 2, 0, 0, 0, cst.Loc) // CST 02:00，窗口内

	missed := scheduler.missedToday(inWindow)
	if len(missed) != 1 || missed[0] != taskBlackcat {
		t.Fatalf("missed=%v want [blackcat]", missed)
	}

	scheduler.markRan(missed, inWindow)

	if got := scheduler.missedToday(inWindow); len(got) != 0 {
		t.Fatalf("same window must not repeat: %v", got)
	}

	outOfWindow := time.Date(2026, 10, 22, 12, 0, 0, 0, cst.Loc)
	if got := scheduler.missedToday(outOfWindow); len(got) != 0 {
		t.Fatalf("out of window must not catch up blackcat: %v", got)
	}
}

// TestLoadStampsMigratesLegacyBoundary 旧版宿主本地日界标记整体作废。
func TestLoadStampsMigratesLegacyBoundary(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "scheduler-runs.json")
	if err := os.WriteFile(path, []byte(`{"checkin":"2026-10-21"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	scheduler := New(&Config{RunStampFile: path})
	now := time.Date(2026, 10, 21, 22, 0, 0, 0, time.UTC) // CST 10-22 06:00

	if scheduler.ranToday(taskCheckin, now) {
		t.Fatal("legacy local-day stamp must be ignored after CST migration")
	}

	scheduler.markRan([]taskKind{taskCheckin}, now)

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(raw), `"_boundary":"cst"`) {
		t.Fatalf("boundary marker missing: %s", raw)
	}

	if !scheduler.ranToday(taskCheckin, now) {
		t.Fatal("new CST stamp must be effective")
	}
}
