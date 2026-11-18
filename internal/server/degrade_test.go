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

// TestDegradeGateActiveTriggered Active() 只对触发源（uid + system 指纹）为 true。
func TestDegradeGateActiveTriggered(t *testing.T) {
	t.Parallel()

	var degradeGate2 degradeGate
	if degradeGate2.Active("u1", "fp-a") {
		t.Error("fresh gate should not be active")
	}

	degradeGate2.Trigger("u1", "fp-a")

	if !degradeGate2.Active("u1", "fp-a") {
		t.Error("after Trigger the same source should be active")
	}
	// 跨账号 / 跨 system 指纹一律不继承（issue #591）。
	if degradeGate2.Active("u2", "fp-a") {
		t.Error("another account must not inherit the degrade")
	}

	if degradeGate2.Active("u1", "fp-b") {
		t.Error("another system fingerprint must not inherit the degrade")
	}

	if degradeGate2.Active("u1", "") {
		t.Error("no-system bucket must not inherit the degrade")
	}
}

// TestDegradeGateTriggerNoRenewal 降级期内再次 Trigger 不续期（保持最早触发点的 00:00 重置）。
func TestDegradeGateTriggerNoRenewal(t *testing.T) {
	t.Parallel()

	var degradeGate2 degradeGate
	degradeGate2.Trigger("u1", "fp-a")

	first := degradeUntil(&degradeGate2, "u1", "fp-a")

	// 再次 Trigger：仍在降级期内，until 不应改变。
	degradeGate2.Trigger("u1", "fp-a")

	second := degradeUntil(&degradeGate2, "u1", "fp-a")
	if !first.Equal(second) {
		t.Errorf("Trigger during active should not renew: first=%v second=%v", first, second)
	}
}

// TestDegradeGatePrunesExpired 过期条目在下次 Active/Trigger 时被清理，map 不无界增长。
func TestDegradeGatePrunesExpired(t *testing.T) {
	t.Parallel()

	var degradeGate2 degradeGate
	// 直接注入一条已过期的条目，模拟跨过 00:00 之后的进程内残留。
	expired := time.Now().Add(-time.Minute)
	degradeGate2.until = map[degradeSource]time.Time{
		{uid: "u-old", sysFP: "fp-old"}: expired,
	}

	if degradeGate2.Active("u-old", "fp-old") {
		t.Fatal("expired entry must not be active")
	}

	if got := len(degradeGate2.until); got != 0 {
		t.Fatalf("expired entry should be pruned on Active, got %d entries", got)
	}

	degradeGate2.until = map[degradeSource]time.Time{
		{uid: "u-old", sysFP: "fp-old"}: expired,
	}
	degradeGate2.Trigger("u1", "fp-a")

	if _, ok := degradeGate2.until[degradeSource{uid: "u-old", sysFP: "fp-old"}]; ok {
		t.Fatal("expired entry should be pruned on Trigger")
	}
}

// degradeUntil 读某触发源的降级截止时刻（无条目返回零值）。
func degradeUntil(g *degradeGate, uid, sysFP string) time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.until[degradeSource{uid: uid, sysFP: sysFP}]
}
