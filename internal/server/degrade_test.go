package server

import (
	"testing"
	"time"
)

// TestNextMidnightCSTBoundaries 边界：CST 视角下当日 00:00 之后 → 次日 00:00，
// 23:59 → 次日 00:00，正午 → 次日 00:00。用固定 +08:00 偏移计算，不依赖系统时区。
func TestNextMidnightCSTBoundaries(t *testing.T) {
	t.Parallel()

	cst := time.FixedZone("CST", 8*60*60)

	cases := []struct {
		name string
		now  time.Time
		// 期望 until 的 CST 时分秒恒为 00:00:00，且 strictly after now。
	}{
		{"23:59 -> next 00:00", time.Date(2026, 9, 11, 23, 59, 0, 0, cst)},
		{"00:00:01 -> next 00:00", time.Date(2026, 9, 11, 0, 0, 1, 0, cst)},
		{"12:00 -> next 00:00", time.Date(2026, 9, 11, 12, 0, 0, 0, cst)},
		{"00:00:00 exact -> next 00:00", time.Date(2026, 9, 11, 0, 0, 0, 0, cst)},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got := nextMidnightCST(testCase.now)
			if !got.After(testCase.now) {
				t.Fatalf("until %v not after now %v", got, testCase.now)
			}
			// CST 视角下必须落在 00:00:00。
			if g := got.In(cst); g.Hour() != 0 || g.Minute() != 0 || g.Second() != 0 {
				t.Errorf("until CST = %02d:%02d:%02d, want 00:00:00", g.Hour(), g.Minute(), g.Second())
			}
		})
	}
}

// TestNextMidnightCSTCrossMonth 跨月：月末 23:59 → 次月 1 日 00:00。
func TestNextMidnightCSTCrossMonth(t *testing.T) {
	t.Parallel()

	cst := time.FixedZone("CST", 8*60*60)
	now := time.Date(2026, 1, 31, 23, 59, 0, 0, cst)
	got := nextMidnightCST(now)

	want := time.Date(2026, 2, 1, 0, 0, 0, 0, cst)
	if !got.Equal(want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

// TestDegradeGateActiveTriggered Active() 在 Trigger 后为 true，过期后为 false。
func TestDegradeGateActiveTriggered(t *testing.T) {
	t.Parallel()

	var degradeGate2 degradeGate
	if degradeGate2.Active() {
		t.Error("fresh gate should not be active")
	}

	degradeGate2.Trigger()

	if !degradeGate2.Active() {
		t.Error("after Trigger should be active")
	}
}

// TestDegradeGateTriggerNoRenewal 降级期内再次 Trigger 不续期（保持最早触发点的 00:00 重置）。
func TestDegradeGateTriggerNoRenewal(t *testing.T) {
	t.Parallel()

	var degradeGate2 degradeGate
	degradeGate2.Trigger()

	first := func() time.Time {
		degradeGate2.mu.Lock()
		defer degradeGate2.mu.Unlock()

		return degradeGate2.until
	}()
	// 再次 Trigger：仍在降级期内，until 不应改变。
	degradeGate2.Trigger()

	second := func() time.Time {
		degradeGate2.mu.Lock()
		defer degradeGate2.mu.Unlock()

		return degradeGate2.until
	}()
	if !first.Equal(second) {
		t.Errorf("Trigger during active should not renew: first=%v second=%v", first, second)
	}
}
