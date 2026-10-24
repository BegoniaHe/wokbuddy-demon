// timing.go 定时任务时序整形：整点抖动、账号间对数正态限速、每号每日相位。
//
// 三个维度都使用确定性派生（sha256 + 可选 salt），避免进程重启/面板重排时重掷
// 计划时刻；随机仅用于账号间间隔采样（rndFn 可注入）。
package scheduler

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/cst"
)

// defaultTimingSalt 用户未配置 timing_salt 时的内置常量。
const defaultTimingSalt = "wokbuddy-demon/timing/v1"

// Timing 时序整形参数。
type Timing struct {
	// SlotJitter 整点后的抖动窗口 W；0 = 整点触发。
	SlotJitter time.Duration
	// PacingSigma 账号间间隔的对数正态 σ；0 = 固定间隔。
	PacingSigma float64
	// PacingFloor/PacingCeiling 间隔夹逼边界；<=0 表示该侧不设限。
	PacingFloor   time.Duration
	PacingCeiling time.Duration
	// PhaseWindow 每号每日相位窗口 P；0 = 只排序不错峰。
	PhaseWindow time.Duration
	// Salt 可选派生盐；空 = defaultTimingSalt。
	Salt string
}

// batchTarget 一轮批量任务里的单个账号：phase 由 runPacedBatch 按当日批次填写。
type batchTarget struct {
	uid   string
	phase float64
	run   func() bool
}

// timingShape 读取当前时序参数快照（与热配置写在 schedMu 下同步）。
func (s *Scheduler) timingShape() Timing {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()

	return s.cfg.Timing
}

// SetTimingShape 热更新时序参数并触发排程重算。
func (s *Scheduler) SetTimingShape(t Timing) {
	s.schedMu.Lock()
	s.cfg.Timing = t
	s.schedMu.Unlock()
	poke(s.rearmSchedule)
}

// rndFloat 返回 [0,1) 随机数；测试可注入 s.rndFn。
func (s *Scheduler) rndFloat() float64 {
	if s.rndFn != nil {
		return s.rndFn()
	}

	return rand.Float64() //nolint:gosec // 排程抖动，非安全用途
}

// slotJitter 返回 [0, W) 的确定性抖动：sha256(salt|kind|day|hour) % W。
// 同日同槽稳定、跨日变化；W <= 0 返回 0。
func slotJitter(salt string, kind taskKind, day string, hour int, window time.Duration) time.Duration {
	if window <= 0 {
		return 0
	}

	if salt == "" {
		salt = defaultTimingSalt
	}

	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|jitter|%s|%s|%d", salt, kind, day, hour)))
	n := binary.BigEndian.Uint64(sum[:8])

	// W 已在配置层限制为 <= 59m，转换不会溢出。
	return time.Duration(n % uint64(window)) //nolint:gosec // W <= 59m
}

// jitterWithinHour 在 slotJitter 基础上把抖动截断到配置小时的剩余时长。
// 普通整点剩余 1h（W <= 59m 时无影响）；DST 拨快抹掉小时开头时 slotOn 可能返回
// 03:45 一类「小时内首个实际时刻」，此时窗口只剩 15m，必须收窄以免跨到 04:xx。
func jitterWithinHour(
	salt string, kind taskKind, hour int, base time.Time, window time.Duration,
) time.Duration {
	jitter := slotJitter(salt, kind, base.Format("2006-01-02"), hour, window)
	if jitter <= 0 {
		return 0
	}

	limit := hourRemaining(base, hour)
	if limit <= 0 {
		return 0
	}

	if jitter >= limit {
		return limit - time.Nanosecond
	}

	return jitter
}

// hourRemaining 返回 base 所在配置小时到下一个墙钟小时的间隔。下一小时同样可能
// 被 DST 抹掉，slotOn 会归一化到首个实际时刻；因此 DST 日返回 0/15m 等短窗口也
// 是正常结果。hour=23 时跨到下一天。
func hourRemaining(base time.Time, hour int) time.Duration {
	year, month, day := base.Date()
	next := time.Date(year, month, day, 12, 0, 0, 0, base.Location())
	nextHour := hour + 1

	if nextHour == 24 {
		nextHour = 0
		next = time.Date(year, month, day+1, 12, 0, 0, 0, base.Location())
	}

	return slotOn(next, nextHour).Sub(base)
}

// fireSlot 一次触发的实际时刻与未抖动基准时刻（用于日志展示抖动偏移）。
type fireSlot struct {
	at   time.Time
	base time.Time
}

// nextFireFor 返回 now 之后最近的抖动后触发时刻及其基准整点。
// 抖动只向前且 W 由调用方保证 <= 59m；W=0 时与 nextFireIn 完全一致。
func nextFireFor(
	now time.Time, kind taskKind, hours []int, loc *time.Location, window time.Duration, salt string,
) fireSlot {
	if loc == nil {
		loc = now.Location()
	}

	localNow := now.In(loc)

	var earliest fireSlot

	for _, hour := range hours {
		base := slotOn(localNow, hour)
		at := base.Add(jitterWithinHour(salt, kind, hour, base, window))

		if !at.After(now) {
			// 与 nextFireIn 同口径：按墙钟日推进，不做 Add(24h)。
			y, m, d := localNow.Date()
			base = slotOn(time.Date(y, m, d+1, 12, 0, 0, 0, loc), hour)
			at = base.Add(jitterWithinHour(salt, kind, hour, base, window))
		}

		if earliest.at.IsZero() || at.Before(earliest.at) {
			earliest = fireSlot{at: at, base: base}
		}
	}

	return earliest
}

// samplePacing 对数正态采样：median * exp(σZ - σ²/2)，均值仍等于 median，
// 再按 floor/ceiling 夹逼。median <= 0 直接返回 0；σ=0 且无夹逼时返回 median。
func samplePacing(
	rnd func() float64, median time.Duration, sigma float64, floor, ceiling time.Duration,
) time.Duration {
	if median <= 0 {
		return 0
	}

	if sigma <= 0 && floor <= 0 && ceiling <= 0 {
		return median
	}

	nanos := float64(median)
	if sigma > 0 {
		nanos = lognormalSample(rnd, median, sigma)
	}

	return clampPacing(nanos, floor, ceiling)
}

// lognormalSample 返回 median * exp(σZ - σ²/2)（Box–Muller 取 Z）。
func lognormalSample(rnd func() float64, median time.Duration, sigma float64) float64 {
	u1 := 1 - rnd()
	if u1 <= 0 {
		u1 = math.SmallestNonzeroFloat64
	}

	z := math.Sqrt(-2*math.Log(u1)) * math.Cos(2*math.Pi*rnd())

	return float64(median) * math.Exp(sigma*z-sigma*sigma/2)
}

// clampPacing 把采样值夹逼到 [floor, ceiling]（<=0 表示该侧不设限）。
func clampPacing(nanos float64, floor, ceiling time.Duration) time.Duration {
	if floor > 0 && nanos < float64(floor) {
		return floor
	}

	if ceiling > 0 && nanos > float64(ceiling) {
		return ceiling
	}

	if nanos > float64(math.MaxInt64) {
		return time.Duration(math.MaxInt64)
	}

	if nanos <= 0 {
		return 0
	}

	return time.Duration(nanos)
}

// samplePacingCurrent 按当前配置采样账号间间隔。
func (s *Scheduler) samplePacingCurrent(median time.Duration) time.Duration {
	t := s.timingShape()

	return samplePacing(s.rndFloat, median, t.PacingSigma, t.PacingFloor, t.PacingCeiling)
}

// jitterShiftBits 抖动哈希取高位后的右移位数。
const jitterShiftBits = 11

// dailyPhase 返回 [0,1) 的确定性相位：同 (salt,uid,day,kind,hour) 恒等。
func dailyPhase(salt, uid, day string, kind taskKind, hour int) float64 {
	if salt == "" {
		salt = defaultTimingSalt
	}

	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|phase|%s|%s|%s|%d", salt, uid, day, kind, hour)))
	n := binary.BigEndian.Uint64(sum[:8]) >> jitterShiftBits

	return float64(n) / float64(uint64(1)<<53)
}

// sortBatchTargets 按相位与 uid 稳定排序。
func sortBatchTargets(targets []batchTarget) {
	slices.SortStableFunc(targets, func(a, b batchTarget) int {
		if c := cmp.Compare(a.phase, b.phase); c != 0 {
			return c
		}

		return cmp.Compare(a.uid, b.uid)
	})
}

// runPacedBatch 执行一轮账号批次：
//  1. 按 dailyPhase 升序稳定排序（同相位回落 uid）；
//  2. send_i = max(batchStart + φ_i·P, last + pacing)；
//  3. 等待走墙钟分段（sleepCtx 的单调 timer 在机器睡眠时会冻结）。
//
// P=0 时只排序不错峰；ctx 取消立即停止剩余账号。
//
//nolint:funlen,gocognit // 排序/错峰/等待/记账线性展开。
func (s *Scheduler) runPacedBatch(
	ctx context.Context, kind taskKind, median time.Duration, targets []batchTarget,
) (total, success, failure int) {
	if len(targets) == 0 {
		return 0, 0, 0
	}

	timing := s.timingShape()
	now := s.now()
	day := travelDay(now)
	hour := now.In(cst.Loc).Hour()

	for i := range targets {
		targets[i].phase = dailyPhase(timing.Salt, targets[i].uid, day, kind, hour)
	}

	sortBatchTargets(targets)

	batchStart := time.Now()

	var last time.Time

	for i := range targets {
		target := batchStart
		if timing.PhaseWindow > 0 {
			target = batchStart.Add(time.Duration(targets[i].phase * float64(timing.PhaseWindow)))
		}

		if i > 0 {
			due := last.Add(s.samplePacingCurrent(median))
			if due.After(target) {
				target = due
			}
		}

		if !waitBatchTarget(ctx, target) {
			return total, success, failure
		}

		if targets[i].run() {
			success++
		} else {
			failure++
		}

		total++
		last = time.Now()
	}

	return total, success, failure
}

// waitBatchTarget 分段等待到墙钟 target；ctx 取消返回 false。
//
// target 来自 time.Now()（携带单调读数），若直接 time.Until 会按单调时钟计算：
// 机器睡眠时单调时钟冻结，醒来后仍会按剩余时长继续等，把整批账号顺延到墙钟
// 目标之后。Round(0) 去掉单调读数，让每段都按墙钟重判，与 waitSlot 同口径。
func waitBatchTarget(ctx context.Context, target time.Time) bool {
	for {
		remain := time.Until(target.Round(0))
		if remain <= 0 {
			return ctx.Err() == nil
		}

		if !sleepCtx(ctx, min(remain, wallclockCheckStep)) {
			return false
		}
	}
}
