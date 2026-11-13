// upstreamhealth.go 上游劣化滑窗（issue #488）。
//
// 目的：把「某 (域, 模型) 最近一段时间 TTFB 明显变慢 / 5xx 比例偏高」变成可观测
// 信号，而不是等用户报障。口径：
//   - 固定 20 个环形桶，桶宽 = 窗口 / 20，读写时惰性推进，无后台 goroutine；
//   - TTFB 只统计有观测的尝试（流式首帧）；5xx 比例的分母是该键的全部上游尝试；
//   - 阈值与最小样本数可配，默认保守（5s / 25% / 10 样本），样本不足不告警；
//   - 只观测与告警：不冷却账号、不改选号、不喂熔断/连败（与轮转逻辑完全解耦）。
package server

import (
	"cmp"
	"fmt"
	"log"
	"slices"
	"sync"
	"time"
)

const (
	// healthBuckets 滑窗桶数：窗口均分为这么多桶，远端桶随写入惰性清空。
	healthBuckets = 20
	// healthMaxKeys 最多跟踪多少个 (域, 模型) 键，防止异常模型名撑爆内存。
	healthMaxKeys = 256
	// globalHealthKey 全局汇总键（所有域与模型合并）。
	globalHealthKey = ""
	// healthKeySep 键内分隔符：模型名不含 \x1f。
	healthKeySep = "\x1f"
)

// healthBucket 一个时间桶内的计数。
type healthBucket struct {
	ttfbSumMs int64
	ttfbCount int
	attempts  int
	server5xx int
}

// healthSeries 单个键的环形桶序列。anchor 是 0 号桶所在窗口的起点。
type healthSeries struct {
	buckets []healthBucket
	anchor  time.Time
}

// advance 把序列推进到 now 所在窗口，清空中间经过的桶。
func (s *healthSeries) advance(now time.Time, period time.Duration) {
	if period <= 0 {
		return
	}

	slot := now.Truncate(period)
	if s.anchor.IsZero() || slot.Before(s.anchor) {
		s.anchor = slot

		return
	}

	steps := int(slot.Sub(s.anchor) / period)
	if steps <= 0 {
		return
	}

	if steps >= len(s.buckets) {
		for index := range s.buckets {
			s.buckets[index] = healthBucket{}
		}

		s.anchor = slot

		return
	}

	for step := 1; step <= steps; step++ {
		at := s.anchor.Add(time.Duration(step) * period)
		s.buckets[healthBucketIndex(at, period)] = healthBucket{}
	}

	s.anchor = slot
}

// current 返回 now 所在桶（调用前已推进）。
func (s *healthSeries) current(now time.Time, period time.Duration) *healthBucket {
	return &s.buckets[healthBucketIndex(now.Truncate(period), period)]
}

// total 汇总窗口内全部桶。
func (s *healthSeries) total() healthBucket {
	var sum healthBucket

	for _, bucket := range s.buckets {
		sum.ttfbSumMs += bucket.ttfbSumMs
		sum.ttfbCount += bucket.ttfbCount
		sum.attempts += bucket.attempts
		sum.server5xx += bucket.server5xx
	}

	return sum
}

// healthBucketIndex 绝对时间 → 环形下标（period 纳秒整除，跨时区一致）。
func healthBucketIndex(at time.Time, period time.Duration) int {
	return int(at.UnixNano()/int64(period)) % healthBuckets
}

// upstreamHealth 上游劣化统计与告警（Handler 内嵌，零值关闭）。
type upstreamHealth struct {
	mu       sync.Mutex
	window   time.Duration
	ttfb     time.Duration
	errRatio float64
	minCount int
	series   map[string]*healthSeries
	warned   map[string]time.Time
	nowFn    func() time.Time
	logger   *log.Logger
	// overflowOnce 键满（healthMaxKeys）告警只打一次：滑窗是有界观测，键溢出
	// 需要运维知道，但不该每个请求刷一行日志。
	overflowOnce sync.Once
}

// newUpstreamHealth 构造滑窗；window<=0 表示关闭（零行为差异）。
func newUpstreamHealth(window, ttfb time.Duration, errRatio float64, minCount int) *upstreamHealth {
	return &upstreamHealth{
		window:   window,
		ttfb:     ttfb,
		errRatio: errRatio,
		minCount: minCount,
		series:   map[string]*healthSeries{},
		warned:   map[string]time.Time{},
		nowFn:    time.Now,
		logger:   log.Default(),
	}
}

// newUpstreamHealthFromConfig 按 handler 配置构造滑窗（阈值已由
// applyUpstreamHealthDefaults 补齐）。
func newUpstreamHealthFromConfig(config *Config) *upstreamHealth {
	return newUpstreamHealth(
		config.UpstreamHealthWindow, config.UpstreamHealthTTFB,
		config.UpstreamHealth5xxRatio, config.UpstreamHealthMinSamples,
	)
}

// enabled 报告滑窗是否启用。
func (u *upstreamHealth) enabled() bool { return u != nil && u.window > 0 }

// period 单桶时长。
func (u *upstreamHealth) period() time.Duration {
	period := u.window / healthBuckets
	if period <= 0 {
		period = time.Second
	}

	return period
}

// observe 记录一次上游尝试：ttfb>0 时计入首帧耗时，server5xx 计入 5xx 计数。
// 同时更新模型键与全局键。
func (u *upstreamHealth) observe(realm, model string, ttfb time.Duration, server5xx bool) {
	if !u.enabled() {
		return
	}

	now := u.nowFn()

	u.mu.Lock()
	defer u.mu.Unlock()

	u.applyLocked(healthKey(realm, model), now, ttfb, server5xx)
	u.applyLocked(globalHealthKey, now, ttfb, server5xx)
}

// markServer5xx 只累加 5xx 计数（尝试数与 TTFB 已由 recordAttempt 计入）。
func (u *upstreamHealth) markServer5xx(realm, model string) {
	if !u.enabled() {
		return
	}

	now := u.nowFn()

	u.mu.Lock()
	defer u.mu.Unlock()

	for _, key := range []string{healthKey(realm, model), globalHealthKey} {
		u.withBucketLocked(key, now, func(bucket *healthBucket) { bucket.server5xx++ })
	}
}

// applyLocked 把一次观测写入指定键并即时评估阈值（调用方已持锁）。
func (u *upstreamHealth) applyLocked(key string, now time.Time, ttfb time.Duration, server5xx bool) {
	u.withBucketLocked(key, now, func(bucket *healthBucket) {
		bucket.attempts++

		if ttfb > 0 {
			bucket.ttfbSumMs += ttfb.Milliseconds()
			bucket.ttfbCount++
		}

		if server5xx {
			bucket.server5xx++
		}
	})
}

// withBucketLocked 取（或创建）键的当前桶，执行更新并即时评估阈值。
func (u *upstreamHealth) withBucketLocked(key string, now time.Time, update func(*healthBucket)) {
	series := u.series[key]
	if series == nil {
		if len(u.series) >= healthMaxKeys {
			// 键（realm|model）满额：此前静默 return，劣化观测对后来的模型完全失效
			// 而运维毫无线索。只告警一次，行为仍保持有界（不扩容 map）。
			u.overflowOnce.Do(func() {
				log.Printf("WARN: [server] upstream health: 观测键已达上限 %d，"+
					"新模型不再计入劣化滑窗（缩小模型数或调大 healthMaxKeys）", healthMaxKeys)
			})

			return
		}

		series = &healthSeries{buckets: make([]healthBucket, healthBuckets)}
		u.series[key] = series
	}

	period := u.period()
	series.advance(now, period)
	update(series.current(now, period))
	u.evaluateLocked(key, series, now)
}

// evaluateLocked 按窗口聚合判断是否劣化，超阈值且冷却已过时打一条 WARN。
func (u *upstreamHealth) evaluateLocked(key string, series *healthSeries, now time.Time) {
	total := series.total()
	ttfbDegraded, ratioDegraded := u.degraded(total)

	if !ttfbDegraded && !ratioDegraded {
		return
	}

	if last, ok := u.warned[key]; ok && now.Sub(last) < u.window {
		return
	}

	u.warned[key] = now

	avgTTFB := 0.0
	if total.ttfbCount > 0 {
		avgTTFB = float64(total.ttfbSumMs) / float64(total.ttfbCount)
	}

	u.logger.Printf(
		"WARN: [server] 上游劣化 %s：窗口 %s 内样本 %d，平均 TTFB %.0fms（阈值 %dms），"+
			"5xx %d/%d（%.0f%%，阈值 %.0f%%）；仅观测，不改变选号",
		describeHealthKey(key), u.window, total.attempts, avgTTFB,
		u.ttfb.Milliseconds(), total.server5xx, total.attempts,
		healthPercent(total.server5xx, total.attempts), u.errRatio*100,
	)
}

// degraded 判断窗口聚合是否越线（样本不足不判劣化）。
func (u *upstreamHealth) degraded(total healthBucket) (bool, bool) {
	ttfbDegraded := total.ttfbCount >= u.minCount &&
		u.ttfb > 0 &&
		total.ttfbSumMs/int64(total.ttfbCount) > u.ttfb.Milliseconds()
	ratioDegraded := total.attempts >= u.minCount &&
		healthPercent(total.server5xx, total.attempts)/100 > u.errRatio

	return ttfbDegraded, ratioDegraded
}

// UpstreamHealthRow 单个 (域, 模型) 或全局的窗口指标。
type UpstreamHealthRow struct {
	// Key 行键："域\x1f模型"，全局行为空。
	Key string `json:"key"`
	// Realm 账号域；全局行为 "*"。
	Realm string `json:"realm"`
	// Model 裸模型名；全局行为 "*"。
	Model string `json:"model"`
	// Attempts 窗口内上游尝试数。
	Attempts int `json:"attempts"`
	// TTFBSamples 有 TTFB 观测的样本数（流式首帧）。
	TTFBSamples int `json:"ttfb_samples"`
	// AvgTTFBMs 平均首帧耗时；无观测时为 0。
	AvgTTFBMs float64 `json:"avg_ttfb_ms"`
	// Server5xx 窗口内 5xx 次数。
	Server5xx int `json:"server_5xx"`
	// ErrorRatio 5xx 占尝试数比例（0~1）。
	ErrorRatio float64 `json:"error_ratio"`
	// TTFBDegraded / ErrorDegraded 各自是否越线。
	TTFBDegraded  bool `json:"ttfb_degraded"`
	ErrorDegraded bool `json:"error_degraded"`
	// Degraded 任一维度越线。
	Degraded bool `json:"degraded"`
}

// UpstreamHealthSnapshot 面板 overview 暴露的滑窗快照。
type UpstreamHealthSnapshot struct {
	Enabled bool   `json:"enabled"`
	Window  string `json:"window,omitempty"`
	// TTFBThresholdMs 与 ErrorRatioThreshold/MinSamples 是当前生效阈值。
	TTFBThresholdMs     int64   `json:"ttfb_threshold_ms,omitempty"`
	ErrorRatioThreshold float64 `json:"error_ratio_threshold,omitempty"`
	MinSamples          int     `json:"min_samples,omitempty"`
	// Rows 全局行在前，其余按 (域, 模型) 排序。
	Rows []UpstreamHealthRow `json:"rows"`
}

// snapshot 生成当前窗口指标快照（只读，推进陈旧桶但不写入观测）。
func (u *upstreamHealth) snapshot() UpstreamHealthSnapshot {
	if !u.enabled() {
		return UpstreamHealthSnapshot{Enabled: false, Rows: []UpstreamHealthRow{}}
	}

	u.mu.Lock()
	defer u.mu.Unlock()

	now := u.nowFn()
	period := u.period()

	rows := make([]UpstreamHealthRow, 0, len(u.series))

	for key, series := range u.series {
		series.advance(now, period)

		total := series.total()
		if total.attempts == 0 {
			continue
		}

		rows = append(rows, u.rowOf(key, total))
	}

	sortHealthRows(rows)

	return UpstreamHealthSnapshot{
		Enabled:             true,
		Window:              u.window.String(),
		TTFBThresholdMs:     u.ttfb.Milliseconds(),
		ErrorRatioThreshold: u.errRatio,
		MinSamples:          u.minCount,
		Rows:                rows,
	}
}

// rowOf 由窗口聚合构造一行指标。
func (u *upstreamHealth) rowOf(key string, total healthBucket) UpstreamHealthRow {
	ttfbDegraded, ratioDegraded := u.degraded(total)
	realm, model := splitHealthKey(key)

	avgTTFB := 0.0
	if total.ttfbCount > 0 {
		avgTTFB = float64(total.ttfbSumMs) / float64(total.ttfbCount)
	}

	return UpstreamHealthRow{
		Key:           key,
		Realm:         realm,
		Model:         model,
		Attempts:      total.attempts,
		TTFBSamples:   total.ttfbCount,
		AvgTTFBMs:     avgTTFB,
		Server5xx:     total.server5xx,
		ErrorRatio:    healthPercent(total.server5xx, total.attempts) / 100,
		TTFBDegraded:  ttfbDegraded,
		ErrorDegraded: ratioDegraded,
		Degraded:      ttfbDegraded || ratioDegraded,
	}
}

// sortHealthRows 全局行在前，其余按 (域, 模型) 升序。
func sortHealthRows(rows []UpstreamHealthRow) {
	slices.SortFunc(rows, func(left, right UpstreamHealthRow) int {
		if left.Key == globalHealthKey {
			return -1
		}

		if right.Key == globalHealthKey {
			return 1
		}

		return cmp.Or(cmp.Compare(left.Realm, right.Realm), cmp.Compare(left.Model, right.Model))
	})
}

// applyUpstreamHealthDefaults 补齐劣化滑窗的阈值缺省值（窗口 0 = 关闭，不兜底）。
func applyUpstreamHealthDefaults(config *Config) {
	if config.UpstreamHealthTTFB <= 0 {
		config.UpstreamHealthTTFB = 5 * time.Second
	}

	if config.UpstreamHealth5xxRatio <= 0 {
		config.UpstreamHealth5xxRatio = 0.25
	}

	if config.UpstreamHealthMinSamples <= 0 {
		config.UpstreamHealthMinSamples = 10
	}
}

// UpstreamHealthSnapshot 返回当前上游劣化滑窗（供面板 overview 挂载）。
func (h *Handler) UpstreamHealthSnapshot() UpstreamHealthSnapshot {
	return h.upstreamHealth.snapshot()
}

// healthKey 构造 (域, 模型) 键；全局键为空串。
func healthKey(realm, model string) string {
	if realm == "" && model == "" {
		return globalHealthKey
	}

	return realm + healthKeySep + model
}

// splitHealthKey 拆回展示用 (域, 模型)；全局键返回 "*"。
func splitHealthKey(key string) (string, string) {
	if key == globalHealthKey {
		return "*", "*"
	}

	realm, model, ok := cutHealthKey(key)
	if !ok {
		return key, key
	}

	return realm, model
}

// cutHealthKey 按分隔符拆分键。
func cutHealthKey(key string) (string, string, bool) {
	for index := 0; index+len(healthKeySep) <= len(key); index++ {
		if key[index:index+len(healthKeySep)] == healthKeySep {
			return key[:index], key[index+len(healthKeySep):], true
		}
	}

	return key, "", false
}

// describeHealthKey 日志展示：全局写「全局」，其余写「域:模型」。
func describeHealthKey(key string) string {
	realm, model := splitHealthKey(key)
	if realm == "*" {
		return "全局"
	}

	return fmt.Sprintf("%s:%s", realm, model)
}

// healthPercent 计算百分比（分母为 0 时返回 0）。
func healthPercent(part, total int) float64 {
	if total <= 0 {
		return 0
	}

	return float64(part) * 100 / float64(total)
}
