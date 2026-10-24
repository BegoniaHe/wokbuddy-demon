package scheduler

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/cst"
)

// TestSamplePacingDistribution 2000 次采样：均值钉在 median ±5%，
// ln(x/median) 标准差 ≈ σ（±15%），p1/p99 落在夹逼区间内。
func TestSamplePacingDistribution(t *testing.T) {
	t.Parallel()

	const (
		n       = 2000
		median  = 800 * time.Millisecond
		sigma   = 0.4
		floor   = 250 * time.Millisecond
		ceiling = 6 * time.Second
	)

	rng := rand.New(rand.NewPCG(1, 2))
	rnd := rng.Float64

	samples := make([]float64, 0, n)

	var sum float64

	for range n {
		d := samplePacing(rnd, median, sigma, floor, ceiling)
		if d < floor || d > ceiling {
			t.Fatalf("sample=%v 超出夹逼 [%v,%v]", d, floor, ceiling)
		}

		v := float64(d)
		samples = append(samples, v)
		sum += v
	}

	mean := sum / n
	if math.Abs(mean/float64(median)-1) > 0.05 {
		t.Fatalf("均值=%v 偏离 median %v 超过 5%%", time.Duration(mean), median)
	}

	var logSum, logSqSum float64

	for _, v := range samples {
		l := math.Log(v / float64(median))
		logSum += l
		logSqSum += l * l
	}

	logMean := logSum / n

	logStd := math.Sqrt(logSqSum/n - logMean*logMean)
	if math.Abs(logStd/sigma-1) > 0.15 {
		t.Fatalf("ln(x/median) 标准差=%.4f want ≈%.2f（±15%%）", logStd, sigma)
	}

	sort.Float64s(samples)

	if p1 := samples[n/100]; p1 < float64(floor) {
		t.Fatalf("p1=%v 小于 floor=%v", time.Duration(p1), floor)
	}

	if p99 := samples[n*99/100]; p99 > float64(ceiling) {
		t.Fatalf("p99=%v 大于 ceiling=%v", time.Duration(p99), ceiling)
	}

	if got := samplePacing(rnd, 0, sigma, floor, ceiling); got != 0 {
		t.Fatalf("median=0 应返回 0，got=%v", got)
	}
}

// TestSamplePacingFixedWhenSigmaZero σ=0 且无夹逼时保持固定间隔。
func TestSamplePacingFixedWhenSigmaZero(t *testing.T) {
	t.Parallel()

	if got := samplePacing(rand.Float64, 900*time.Millisecond, 0, 0, 0); got != 900*time.Millisecond {
		t.Fatalf("σ=0 got=%v want 900ms", got)
	}
}

// TestDailyPhaseStability 同 (uid,day,task,hour) 恒等，跨日/跨号不同，10 桶近似均匀。
func TestDailyPhaseStability(t *testing.T) {
	t.Parallel()

	base := dailyPhase("", "u1", "2026-10-22", taskCheckin, 9)
	if got := dailyPhase("", "u1", "2026-10-22", taskCheckin, 9); got != base {
		t.Fatalf("同输入相位不稳定: %v vs %v", base, got)
	}

	if got := dailyPhase("", "u1", "2026-10-23", taskCheckin, 9); got == base {
		t.Fatalf("跨日相位相同: %v", got)
	}

	if got := dailyPhase("", "u2", "2026-10-22", taskCheckin, 9); got == base {
		t.Fatalf("跨号相位相同: %v", got)
	}

	if got := dailyPhase("", "u1", "2026-10-22", taskTravel, 9); got == base {
		t.Fatalf("跨任务相位相同: %v", got)
	}
}

// TestDailyPhaseDistribution 10 分桶各约 10%（±3%）。
func TestDailyPhaseDistribution(t *testing.T) {
	t.Parallel()

	const n = 10000

	buckets := make([]int, 10)

	for i := range n {
		uid := fmt.Sprintf("phase-%d", i)

		phase := dailyPhase("", uid, "2026-10-22", taskCheckin, 9)
		if phase < 0 || phase >= 1 {
			t.Fatalf("phase=%v 超出 [0,1)", phase)
		}

		buckets[int(phase*10)]++
	}

	for i, count := range buckets {
		ratio := float64(count) / n
		if ratio < 0.07 || ratio > 0.13 {
			t.Fatalf("桶 %d 占比=%.3f want 0.10±0.03 (buckets=%v)", i, ratio, buckets)
		}
	}
}

// TestSlotJitterBoundsStable 抖动落在 [0,W)、同日同槽稳定、跨日变化。
func TestSlotJitterBoundsStable(t *testing.T) {
	t.Parallel()

	const window = 20 * time.Minute

	jitterA := slotJitter("", taskCheckin, "2026-10-22", 9, window)
	if jitterA < 0 || jitterA >= window {
		t.Fatalf("jitter=%v 超出 [0,%v)", jitterA, window)
	}

	if jitterB := slotJitter("", taskCheckin, "2026-10-22", 9, window); jitterB != jitterA {
		t.Fatalf("同日同槽不稳定: %v vs %v", jitterA, jitterB)
	}

	if jitterC := slotJitter("", taskCheckin, "2026-10-23", 9, window); jitterC == jitterA {
		t.Fatalf("跨日抖动相同: %v", jitterA)
	}

	if got := slotJitter("", taskCheckin, "2026-10-22", 9, 0); got != 0 {
		t.Fatalf("W=0 jitter=%v want 0", got)
	}
}

// TestNextFireForKeepsWallClockHour 抖动不早于整点、不跨出配置小时（含极端时区）。
func TestNextFireForKeepsWallClockHour(t *testing.T) {
	t.Parallel()

	const window = 20 * time.Minute

	for _, name := range []string{"America/Santiago", "Pacific/Chatham", "Asia/Kathmandu"} {
		loc, err := time.LoadLocation(name)
		if err != nil {
			t.Skipf("时区数据不可用: %v", err)
		}

		now := time.Date(2026, 7, 27, 8, 0, 0, 0, loc)

		fire := nextFireFor(now, taskCheckin, []int{9}, loc, window, "")
		if fire.at.Hour() != 9 {
			t.Errorf("%s: fire=%v 墙钟小时=%d want 9", name, fire.at, fire.at.Hour())
		}

		if fire.at.Before(fire.base) {
			t.Errorf("%s: fire=%v 早于整点 %v", name, fire.at, fire.base)
		}

		if d := fire.at.Sub(fire.base); d < 0 || d >= window {
			t.Errorf("%s: jitter=%v 超出 [0,%v)", name, d, window)
		}
	}
}

// TestNextFireForCapsJitterAtSkippedHourEnd DST 拨快只抹掉小时开头时（Chatham
// 2026-09-27 02:45 → 03:45），03 点槽位只剩 15 分钟窗口；抖动不得把触发时刻
// 推到 04:xx，否则「不跨出配置小时」的窗口语义（如 blackcat）会失效。
func TestNextFireForCapsJitterAtSkippedHourEnd(t *testing.T) {
	t.Parallel()

	loc, err := time.LoadLocation("Pacific/Chatham")
	if err != nil {
		t.Skipf("时区数据不可用: %v", err)
	}

	now := time.Date(2026, 9, 27, 2, 0, 0, 0, loc)
	hourEnd := time.Date(2026, 9, 27, 4, 0, 0, 0, loc)

	fire := nextFireFor(now, taskCheckin, []int{3}, loc, 59*time.Minute, "")
	if fire.at.Hour() != 3 || !fire.at.Before(hourEnd) {
		t.Fatalf("fire=%v want 03:45~03:59（抖动收窄到被抹掉小时之后的剩余窗口）", fire.at)
	}

	if want := time.Date(2026, 9, 27, 3, 45, 0, 0, loc); !fire.base.Equal(want) {
		t.Fatalf("base=%v want %v", fire.base, want)
	}
}

// TestNextWakeJitterOffMerges 抖动关闭时同小时任务仍在同一时刻合并。
func TestNextWakeJitterOffMerges(t *testing.T) {
	t.Parallel()

	scheduler := New(&Config{
		CheckinHours:      []int{9},
		TravelHours:       []int{9},
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		BlackcatDisabled:  true,
		GrowthDisabled:    true,
	})

	now := time.Date(2026, 10, 22, 8, 0, 0, 0, time.Local)

	at, kinds := scheduler.nextWake(now)
	if len(kinds) != 2 || !hasKind(kinds, taskCheckin) || !hasKind(kinds, taskTravel) {
		t.Fatalf("W=0 应合并同刻任务: at=%v kinds=%v", at, kinds)
	}
}

// TestNextWakeJitterOnSeparates 抖动开启后同小时任务返回两个独立唤醒时刻。
func TestNextWakeJitterOnSeparates(t *testing.T) {
	t.Parallel()

	scheduler := New(&Config{
		CheckinHours:      []int{9},
		TravelHours:       []int{9},
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		BlackcatDisabled:  true,
		GrowthDisabled:    true,
		Timing:            Timing{SlotJitter: 20 * time.Minute, Salt: "merge-test"},
	})

	now := time.Date(2026, 10, 22, 8, 0, 0, 0, time.Local)

	var checkinAt, travelAt time.Time

	for _, slot := range scheduler.candidateSlots(now) {
		if slot.kind == taskCheckin {
			checkinAt = slot.at
		}

		if slot.kind == taskTravel {
			travelAt = slot.at
		}
	}

	if checkinAt.IsZero() || travelAt.IsZero() {
		t.Fatalf("缺少候选槽: checkin=%v travel=%v", checkinAt, travelAt)
	}

	if checkinAt.Equal(travelAt) {
		t.Fatalf("抖动开启后仍同刻: %v", checkinAt)
	}

	at, kinds := scheduler.nextWake(now)
	if len(kinds) != 1 {
		t.Fatalf("抖动开启后首个唤醒应只含一类: at=%v kinds=%v", at, kinds)
	}

	at2, kinds2 := scheduler.nextWake(at.Add(time.Second))
	if len(kinds2) != 1 || kinds2[0] == kinds[0] {
		t.Fatalf("第二个唤醒=%v kinds=%v；首个 kinds=%v", at2, kinds2, kinds)
	}
}

// TestRunPacedBatchSortsWithoutWindow P=0 时只按相位排序，不额外等待。
func TestRunPacedBatchSortsWithoutWindow(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 22, 9, 0, 0, 0, time.Local)
	scheduler := New(&Config{})
	scheduler.nowFn = func() time.Time { return now }

	var order []string

	targets := []batchTarget{
		{uid: "c", run: func() bool { order = append(order, "c"); return true }},
		{uid: "a", run: func() bool { order = append(order, "a"); return true }},
		{uid: "b", run: func() bool { order = append(order, "b"); return true }},
	}
	original := append([]batchTarget(nil), targets...)

	total, success, failure := scheduler.runPacedBatch(context.Background(), taskCheckin, 0, targets)
	if total != 3 || success != 3 || failure != 0 {
		t.Fatalf("counters=%d/%d/%d", total, success, failure)
	}

	want := append([]batchTarget(nil), original...)
	day := travelDay(now)
	hour := now.In(cst.Loc).Hour()

	for i := range want {
		want[i].phase = dailyPhase("", want[i].uid, day, taskCheckin, hour)
	}

	sortBatchTargets(want)

	for i := range want {
		if order[i] != want[i].uid {
			t.Fatalf("执行顺序=%v want %v", order, uidsOf(want))
		}
	}
}

func uidsOf(targets []batchTarget) []string {
	out := make([]string, 0, len(targets))
	for _, target := range targets {
		out = append(out, target.uid)
	}

	return out
}

// TestSetTimingShapeKeepsSameDayPlan 同一天/同槽重算（面板 rearm）得到相同计划时刻。
func TestSetTimingShapeKeepsSameDayPlan(t *testing.T) {
	t.Parallel()

	timing := Timing{SlotJitter: 20 * time.Minute, Salt: "stable-test"}
	scheduler := New(&Config{
		CheckinHours: []int{9}, TravelDisabled: true, ActivityDisabled: true,
		KeepaliveDisabled: true, BlackcatDisabled: true, GrowthDisabled: true, Timing: timing,
	})

	now := time.Date(2026, 10, 22, 8, 0, 0, 0, time.Local)

	first, kinds := scheduler.nextWake(now)
	if len(kinds) != 1 || kinds[0] != taskCheckin {
		t.Fatalf("kinds=%v", kinds)
	}

	scheduler.SetTimingShape(timing)

	second, _ := scheduler.nextWake(now)
	if !first.Equal(second) {
		t.Fatalf("重排后计划时刻漂移: %v -> %v", first, second)
	}
}

// TestSetTimingShapePokesRearm 热更新必须唤醒 Run 重算，否则新参数要等下一轮才生效。
func TestSetTimingShapePokesRearm(t *testing.T) {
	t.Parallel()

	scheduler := New(&Config{})

	select {
	case <-scheduler.rearmSchedule:
		t.Fatal("初始化后不应有待处理的重排信号")
	default:
	}

	scheduler.SetTimingShape(Timing{SlotJitter: time.Minute})

	select {
	case <-scheduler.rearmSchedule:
	default:
		t.Fatal("SetTimingShape 未 poke rearmSchedule")
	}
}
