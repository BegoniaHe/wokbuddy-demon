package cst

import (
	"testing"
	"time"
)

// TestLocIsFixedUTC8 固定 +08:00：上游「自然日」不跟随宿主时区，
// 也不随夏令时漂移（中国 1970 年后无夏令时）。
func TestLocIsFixedUTC8(t *testing.T) {
	t.Parallel()

	if got := Loc.String(); got != "CST" {
		t.Fatalf("Loc=%q want CST", got)
	}

	_, offset := time.Date(2026, 1, 15, 0, 0, 0, 0, Loc).Zone()
	if offset != 8*60*60 {
		t.Fatalf("offset=%d want %d", offset, 8*60*60)
	}

	// 冬夏两季同一偏移：没有任何 DST 切换。
	for _, month := range []time.Month{time.January, time.July} {
		_, summerOffset := time.Date(2026, month, 15, 0, 0, 0, 0, Loc).Zone()
		if summerOffset != offset {
			t.Fatalf("month=%v offset=%d want %d（不得有夏令时）", month, summerOffset, offset)
		}
	}
}

// TestDayUsesCSTBoundary 日界按北京时间零点切：UTC 前一天 16:00 之后
// 已属 CST 的次日，必须归到新的一天而不是 UTC 的旧日期。
func TestDayUsesCSTBoundary(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   time.Time
		want string
	}{
		{
			name: "UTC 15:59:59 仍是 CST 当日",
			in:   time.Date(2026, 3, 1, 15, 59, 59, 0, time.UTC),
			want: "2026-03-01",
		},
		{
			name: "UTC 16:00:00 已跨到 CST 次日",
			in:   time.Date(2026, 3, 1, 16, 0, 0, 0, time.UTC),
			want: "2026-03-02",
		},
		{
			name: "已是 CST 时区的输入不再平移",
			in:   time.Date(2026, 3, 1, 23, 30, 0, 0, Loc),
			want: "2026-03-01",
		},
		{
			name: "跨年边界",
			in:   time.Date(2025, 12, 31, 16, 0, 0, 0, time.UTC),
			want: "2026-01-01",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			if got := Day(testCase.in); got != testCase.want {
				t.Fatalf("Day(%s)=%s want %s", testCase.in.Format(time.RFC3339), got, testCase.want)
			}
		})
	}
}

// TestNowIsCST Now 返回的时刻必须落在 CST 时区（面板与调度按它取日界）。
func TestNowIsCST(t *testing.T) {
	t.Parallel()

	if _, offset := Now().Zone(); offset != 8*60*60 {
		t.Fatalf("Now() offset=%d want %d", offset, 8*60*60)
	}

	if got := Now().Location().String(); got != "CST" {
		t.Fatalf("Now() location=%q want CST", got)
	}
}
