package upstream

import (
	"testing"
	"time"
)

// TestInNightWindowUsesCST 上游 23:00–08:00 窗口按 CST，不随宿主时区漂移。
func TestInNightWindowUsesCST(t *testing.T) {
	t.Parallel()

	cases := []struct {
		utc  time.Time
		want bool
	}{
		{time.Date(2026, 10, 21, 16, 0, 0, 0, time.UTC), true},  // CST 00:00
		{time.Date(2026, 10, 21, 1, 0, 0, 0, time.UTC), false},  // CST 09:00
		{time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC), false},  // CST 16:00
		{time.Date(2026, 10, 21, 15, 30, 0, 0, time.UTC), true}, // CST 23:30
	}
	for _, tc := range cases {
		if got := InNightWindow(tc.utc); got != tc.want {
			t.Errorf("InNightWindow(%s)=%v want %v", tc.utc, got, tc.want)
		}
	}
}
