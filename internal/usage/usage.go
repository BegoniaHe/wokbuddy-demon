// Package usage 记录并聚合逐请求 token 用量，供面板「用量」视图展示。
//
// 与 internal/pool 的 TokenUsage 的区别：
//   - pool 的 TokenUsage 是**每账号一个累计计数器**，只保留总量与「最近一次」，
//     没有时间维度，也无法按模型/时间下钻；
//   - 本包按 (时间片, realm, uid, model, rate) 分桶累计，因此可以出「今天各模型各用了多少」
//     「这一小时 prompt 涨得多快」这类问题，且能长期保留。
//
// 保留策略（分片粒度自动降级，总量因此有界）：
//   - 近 hourlyKeep 小时内：小时桶（细粒度，看尖峰）
//   - 更早：折叠为日桶，**永久保留**（看长期趋势）
//
// 落盘：data/usage.json，原子替换 + 防抖刷新（默认 30s），重启不丢。
// 桶数上界 ≈ 账号数 × 模型数 × (hourlyKeep + 已过天数)，实测单桶约 90 字节。
package usage

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/atomicwrite"
)

// hourlyKeep 小时桶的保留时长；超出后折叠为日桶。
const hourlyKeep = 90 * 24 * time.Hour

// flushInterval 防抖落盘间隔。
const flushInterval = 30 * time.Second

// maxBuckets 桶数硬上限。超过时立即触发一次折叠，避免异常流量把内存/文件撑爆。
const maxBuckets = 400_000

// hourLayout / dayLayout 分片键的时间格式（本地时区，与用户直觉一致）。
const (
	hourLayout = "2006-01-02T15"
	dayLayout  = "2006-01-02"
)

// fileVersion 是 usage.json 的当前格式版本。版本 2 增加积分观测字段，版本 3
// 增加模型生效倍率分区，版本 4 增加前缀缓存命中/未命中累计；
// 旧版本缺失字段按零值加载，旧数据不会丢弃。
const fileVersion = 4

// bucket 一个 (时间片, realm, uid, model, rate) 的累计量。
// JSON 字段名刻意取短，因为桶数量会随时间增长。
type bucket struct {
	Scope string  `json:"s"`             // "h:2006-01-02T15" 或 "d:2006-01-02"
	Realm string  `json:"r"`             // cn / global
	UID   string  `json:"u"`             // 账号 uid
	Model string  `json:"m"`             // 上游裸模型名
	Rate  string  `json:"x,omitempty"`   // 请求时生效积分倍率（规范化数值；旧桶为空）
	Req   int64   `json:"q"`             // 请求数（含失败）
	Err   int64   `json:"e"`             // 失败数
	PT    int64   `json:"p"`             // prompt tokens
	CT    int64   `json:"c"`             // completion tokens
	TT    int64   `json:"t"`             // total tokens（上游给什么用什么的合计）
	LatMs int64   `json:"l"`             // 延迟累计（ms）
	LatN  int64   `json:"ln"`            // 延迟样本数
	TFMs  int64   `json:"tf,omitempty"`  // 首字节延迟（TTFT）累计（ms，仅流式观测）
	TFN   int64   `json:"tfn,omitempty"` // TTFT 样本数
	TPS   float64 `json:"v"`             // 吐字速率累计
	TPSN  int64   `json:"vn"`            // 速率样本数
	CR    float64 `json:"cr,omitempty"`  // usage.credit 累计（仅明确存在的观测）
	CRN   int64   `json:"cn,omitempty"`  // usage.credit 样本数（区分缺字段与真实 0）
	CRT   int64   `json:"ct,omitempty"`  // 同时具备 credit 与 token 的 Token 合计
	CH    int64   `json:"ch,omitempty"`  // 前缀缓存命中 token 累计（上游回该维度才累计）
	CM    int64   `json:"cm,omitempty"`  // 前缀缓存未命中 token 累计
	// Synced 标记该桶来自上游逐笔用量回填（issue #31）：覆盖本地记录，供聚合与
	// 面板区分「本地实时记录」与「上游权威对账」。
	Synced bool `json:"syn,omitempty"`
}

// file 落盘结构。
type file struct {
	Version int      `json:"version"`
	Saved   string   `json:"saved"`
	Buckets []bucket `json:"buckets"`
}

// Recorder 并发安全的用量记录器。
type Recorder struct {
	mu      sync.Mutex
	path    string
	buckets map[string]*bucket // key: scope|realm|uid|model|rate
	dirty   bool
	started time.Time

	// flushMu 串行化「快照 + 写临时文件 + rename」：ticker 的 flush 与面板 Save
	// 可并发，共用同名 tmp 会互相截断/残留尾部，最终把残缺 JSON 发布为 usage.json。
	flushMu sync.Mutex

	runStarted atomic.Bool
	stopOnce   sync.Once
	stop       chan struct{}
	done       chan struct{}
	// dailyCache 当日聚合的 1 秒缓存（issue #95）：选号过滤会在一次请求内
	// 多次读取，避免反复遍历全部桶。
	dailyCache atomic.Pointer[dailyCacheEntry]
}

// DailyUsage 一个账号当日的用量聚合。
type DailyUsage struct {
	Tokens  int64
	Credits float64
	Models  map[string]int64
}

type dailyCacheEntry struct {
	at   time.Time
	data map[string]DailyUsage
}

// DailyTotals 聚合本地时区「今天」的小时桶与日桶（issue #95）。
func (r *Recorder) DailyTotals(now time.Time) map[string]DailyUsage {
	if r == nil {
		return nil
	}

	day := now.Format(dayLayout)
	hourPrefix := "h:" + day

	r.mu.Lock()
	defer r.mu.Unlock()

	out := make(map[string]DailyUsage, len(r.buckets))
	for _, item := range r.buckets {
		if item.Scope != "d:"+day && !strings.HasPrefix(item.Scope, hourPrefix) {
			continue
		}

		dailyUsage := out[item.UID]
		dailyUsage.Tokens += item.TT

		dailyUsage.Credits += item.CR
		if dailyUsage.Models == nil {
			dailyUsage.Models = map[string]int64{}
		}

		dailyUsage.Models[item.Model] += item.TT
		out[item.UID] = dailyUsage
	}

	return out
}

// DailyTotalsCached 同 DailyTotals，但 1 秒内复用同一份快照。
func (r *Recorder) DailyTotalsCached(now time.Time) map[string]DailyUsage {
	if r == nil {
		return nil
	}

	if c := r.dailyCache.Load(); c != nil && now.Sub(c.at) < time.Second {
		return c.data
	}

	data := r.DailyTotals(now)
	r.dailyCache.Store(&dailyCacheEntry{at: now, data: data})

	return data
}

// New 创建记录器。path 为空时禁用落盘（纯内存，测试用）。
func New(path string) *Recorder {
	r := &Recorder{
		path:    path,
		buckets: make(map[string]*bucket),
		started: time.Now(),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	if path != "" {
		if err := r.load(); err != nil {
			log.Printf("[usage] 读取 %s 失败（从零开始）: %v", path, err)
		}
	}

	return r
}

// Start 启动后台防抖落盘与折叠。Stop 前一直运行；重复调用幂等。
func (r *Recorder) Start() {
	if !r.runStarted.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer close(r.done)

		t := time.NewTicker(flushInterval)
		defer t.Stop()

		for {
			select {
			case <-r.stop:
				r.flush(true)
				return
			case <-t.C:
				r.mu.Lock()
				n := len(r.buckets)
				r.mu.Unlock()

				if n > maxBuckets {
					r.Rollup(time.Now())
				}

				r.flush(false)
			}
		}
	}()
}

// Stop 停止后台循环并做最后一次落盘（未 Start 时只做一次落盘，不阻塞）。
func (r *Recorder) Stop() {
	if !r.runStarted.Load() {
		r.flush(true)
		return
	}

	r.stopOnce.Do(func() { close(r.stop) })
	<-r.done
}

// Delta 一次请求尝试的用量增量（与 pool.TokenUsageDelta 同形，避免包间依赖）。
type Delta struct {
	PromptTokens     int64
	HasPromptTokens  bool
	CompletionTokens int64
	HasCompletion    bool
	TotalTokens      int64
	HasTotal         bool
	Credit           float64
	HasCredit        bool
	ModelRate        string
	// CacheHitTokens / CacheMissTokens 前缀缓存命中/未命中观测（issue #92）。
	HasCacheTokens  bool
	CacheHitTokens  int64
	CacheMissTokens int64
	LatencyMs       int64
	HasLatency      bool
	TTFTMs          int64
	HasTTFT         bool
	TokensPerSecond float64
	HasTPS          bool
}

// Add 记录一次请求尝试。
//
// ok=false 表示该次尝试失败（传输错误 / 上游 >=400 / 解析失败）。失败尝试通常
// 没有 usage，但**仍要计入请求数与失败数**——重试放大正是靠这一列才看得出来。
func (r *Recorder) Add(now time.Time, realm, uid, model string, delta *Delta, ok bool) {
	if r == nil {
		return
	}

	if realm == "" {
		realm = "cn"
	}

	if model == "" {
		model = "(unknown)"
	}

	scope := "h:" + now.Format(hourLayout)
	key := scope + "|" + realm + "|" + uid + "|" + model + "|" + delta.ModelRate

	r.mu.Lock()
	defer r.mu.Unlock()

	bucket2 := r.buckets[key]
	if bucket2 == nil {
		bucket2 = &bucket{Scope: scope, Realm: realm, UID: uid, Model: model, Rate: delta.ModelRate}
		r.buckets[key] = bucket2
	}

	bucket2.apply(delta, ok)

	r.dirty = true
}

// TokensByAccountModel 汇总自 since 起每个账号×模型的成功 token（issue #101）。
func (r *Recorder) TokensByAccountModel(since time.Time) map[string]map[string]int64 {
	if r == nil {
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	out := map[string]map[string]int64{}

	for _, item := range r.buckets {
		if !strings.HasPrefix(item.Scope, "h:") {
			continue
		}

		t, err := time.ParseInLocation(hourLayout, strings.TrimPrefix(item.Scope, "h:"), time.Local)
		if err != nil || t.Before(since) {
			continue
		}

		if out[item.UID] == nil {
			out[item.UID] = map[string]int64{}
		}

		out[item.UID][item.Model] += item.TT
	}

	return out
}

// TokensSince 汇总某账号×模型自 since 起的成功 token（issue #101）。
func (r *Recorder) TokensSince(realm, uid, model string, since time.Time) int64 {
	if r == nil {
		return 0
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	var total int64

	for _, item := range r.buckets {
		if item.Realm != realm || item.UID != uid || item.Model != model || !strings.HasPrefix(item.Scope, "h:") {
			continue
		}

		t, err := time.ParseInLocation(hourLayout, strings.TrimPrefix(item.Scope, "h:"), time.Local)
		if err != nil || t.Before(since) {
			continue
		}

		total += item.TT
	}

	return total
}

// BackfillRow 上游逐笔用量按天聚合的一行（issue #31）。
//
// 只承载上游逐笔接口能提供的权威维度（请求数 / 积分）；token、延迟、缓存等维度
// 上游不返回，回填时保留本地观测（见 applyBackfillLocked），故不在此声明。
type BackfillRow struct {
	Day      string // 本地日历日 "2006-01-02"
	Realm    string
	UID      string
	Model    string
	Requests int64
	Credits  float64
}

// Backfill 用上游权威数据覆盖指定 (天, realm, uid, model) 的用量桶：删除该键的本地
// 小时桶与日桶后写入标记 Synced 的日桶，避免与本地实时记录重复计数（issue #31）。
// 仅覆盖上游能提供的维度（请求数 / 积分），token / 延迟 / 缓存等本地维度在覆盖前
// 聚合保留，避免把无法从上游恢复的本地观测清零。返回覆盖的桶数。仅显式触发时调用。
func (r *Recorder) Backfill(rows []BackfillRow) int {
	if r == nil {
		return 0
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	n := 0

	for _, row := range rows {
		if row.Day == "" || row.UID == "" {
			continue
		}

		r.applyBackfillLocked(&row)

		n++
	}

	if n > 0 {
		r.dirty = true
	}

	return n
}

// applyBackfillLocked 覆盖单个 (天, realm, uid, model) 的桶：先聚合本地不可从上游
// 恢复的维度（token / 延迟 / 缓存），删除本地桶后写入携带这些维度、标记 Synced 的日桶。
func (r *Recorder) applyBackfillLocked(row *BackfillRow) {
	realm := row.Realm
	if realm == "" {
		realm = "cn"
	}

	model := row.Model
	if model == "" {
		model = "(unknown)"
	}

	dayScope := "d:" + row.Day
	hourPrefix := "h:" + row.Day + "T"

	var local bucket

	for strVal, bucket2 := range r.buckets {
		if bucket2.UID != row.UID || bucket2.Realm != realm || bucket2.Model != model ||
			(bucket2.Scope != dayScope && !strings.HasPrefix(bucket2.Scope, hourPrefix)) {
			continue
		}

		local.PT += bucket2.PT
		local.CT += bucket2.CT
		local.TT += bucket2.TT
		local.LatMs += bucket2.LatMs
		local.LatN += bucket2.LatN
		local.TFMs += bucket2.TFMs
		local.TFN += bucket2.TFN
		local.TPS += bucket2.TPS
		local.TPSN += bucket2.TPSN
		local.CH += bucket2.CH
		local.CM += bucket2.CM

		delete(r.buckets, strVal)
	}

	r.buckets[dayScope+"|"+realm+"|"+row.UID+"|"+model+"|"] = backfilledBucket(row, realm, model, dayScope, &local)
}

func backfilledBucket(row *BackfillRow, realm, model, dayScope string, local *bucket) *bucket {
	bucket2 := &bucket{
		Scope:  dayScope,
		Realm:  realm,
		UID:    row.UID,
		Model:  model,
		Req:    row.Requests,
		PT:     local.PT,
		CT:     local.CT,
		TT:     local.TT,
		LatMs:  local.LatMs,
		LatN:   local.LatN,
		TFMs:   local.TFMs,
		TFN:    local.TFN,
		TPS:    local.TPS,
		TPSN:   local.TPSN,
		CH:     local.CH,
		CM:     local.CM,
		Synced: true,
	}
	if row.Credits != 0 {
		bucket2.CR = row.Credits
		bucket2.CRN = row.Requests
		bucket2.CRT = local.TT
	}

	return bucket2
}

// tokenTotal 返回本次尝试的 token 合计：优先上游给的 total，缺省用 pt+ct 兜底，
// 保证总量口径连续（两条链路——总量与积分比例分母——共用同一口径）。
func (d *Delta) tokenTotal() int64 {
	if d.HasTotal {
		return d.TotalTokens
	}

	if d.HasPromptTokens || d.HasCompletion {
		return d.PromptTokens + d.CompletionTokens
	}

	return 0
}

// apply 把一次尝试的增量并入桶。
func (b *bucket) apply(delta2 *Delta, ok bool) {
	b.Req++
	if !ok {
		b.Err++
	}

	if delta2.HasPromptTokens {
		b.PT += delta2.PromptTokens
	}

	if delta2.HasCompletion {
		b.CT += delta2.CompletionTokens
	}

	b.TT += delta2.tokenTotal()
	if delta2.HasCredit {
		b.CR += delta2.Credit
		b.CRN++
		// 比例只使用同一次请求同时具备 credit 与 token 的样本，避免把
		// 仅 token 的旧记录或仅 credit 的观测混进分母。
		b.CRT += delta2.tokenTotal()
	}

	if delta2.HasCacheTokens {
		b.CH += delta2.CacheHitTokens
		b.CM += delta2.CacheMissTokens
	}

	if delta2.HasLatency {
		b.LatMs += delta2.LatencyMs
		b.LatN++
	}

	if delta2.HasTTFT {
		b.TFMs += delta2.TTFTMs
		b.TFN++
	}

	if delta2.HasTPS {
		b.TPS += delta2.TokensPerSecond
		b.TPSN++
	}
}

// Rollup 把超出 hourlyKeep 的小时桶折叠为日桶（按本地日历日）。
// 幂等：同一小时反复折叠不会重复计数（先累加再删源桶）。
func (r *Recorder) Rollup(now time.Time) {
	if r == nil {
		return
	}

	cutoff := now.Add(-hourlyKeep)

	r.mu.Lock()
	defer r.mu.Unlock()

	type move struct{ from, to string }

	var moves []move

	for strVal, bucket2 := range r.buckets {
		if !strings.HasPrefix(bucket2.Scope, "h:") {
			continue
		}

		ts, err := time.ParseInLocation(hourLayout, strings.TrimPrefix(bucket2.Scope, "h:"), time.Local)
		if err != nil || !ts.Before(cutoff) {
			continue
		}

		day := "d:" + ts.Format(dayLayout)
		moves = append(moves, move{from: strVal, to: day + "|" + bucket2.Realm + "|" + bucket2.UID + "|" + bucket2.Model + "|" + bucket2.Rate})
	}

	for _, m := range moves {
		r.mergeHourly(m.from, m.to)
	}

	if len(moves) > 0 {
		r.dirty = true

		log.Printf("[usage] 折叠 %d 个小时桶为日桶（保留 %v 细粒度）", len(moves), hourlyKeep)
	}
}

func (r *Recorder) mergeHourly(from, to string) {
	src := r.buckets[from]
	if src == nil {
		return
	}

	dst := r.buckets[to]
	if dst == nil {
		cp := *src
		cp.Scope = strings.SplitN(to, "|", 2)[0]
		dst = &cp
		r.buckets[to] = dst
	} else {
		dst.Req += src.Req
		dst.Err += src.Err
		dst.PT += src.PT
		dst.CT += src.CT
		dst.TT += src.TT
		dst.LatMs += src.LatMs
		dst.LatN += src.LatN
		dst.TFMs += src.TFMs
		dst.TFN += src.TFN
		dst.TPS += src.TPS
		dst.TPSN += src.TPSN
		dst.CR += src.CR
		dst.CRN += src.CRN
		dst.CRT += src.CRT
		dst.CH += src.CH
		dst.CM += src.CM
	}

	delete(r.buckets, from)
}

// ---------------------------------------------------------------- 持久化 ----

func (r *Recorder) load() error {
	raw, err := os.ReadFile(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}

		return fmt.Errorf("load: %w", err)
	}

	var file2 file
	if err := json.Unmarshal(raw, &file2); err != nil {
		return fmt.Errorf("load: %w", err)
	}

	for i := range file2.Buckets {
		b := file2.Buckets[i]
		r.buckets[b.Scope+"|"+b.Realm+"|"+b.UID+"|"+b.Model+"|"+b.Rate] = &b
	}

	log.Printf("[usage] 已恢复 %d 个用量桶（%s）", len(r.buckets), r.path)

	return nil
}

func (r *Recorder) flush(force bool) {
	if r == nil || r.path == "" {
		return
	}

	r.flushMu.Lock()
	defer r.flushMu.Unlock()

	r.mu.Lock()
	if !r.dirty && !force {
		r.mu.Unlock()
		return
	}

	snap := file{Version: fileVersion, Saved: time.Now().Format(time.RFC3339), Buckets: make([]bucket, 0, len(r.buckets))}
	for _, b := range r.buckets {
		snap.Buckets = append(snap.Buckets, *b)
	}

	r.dirty = false
	r.mu.Unlock()

	// 落盘失败后重新置脏：否则下一轮 tick 因 dirty=false 直接跳过，最新用量
	// 只能等优雅退出时的强制 flush。并发写入者已在此期间置脏，这里只追加不覆盖。
	markDirty := func() {
		r.mu.Lock()
		r.dirty = true
		r.mu.Unlock()
	}

	raw, err := json.Marshal(snap)
	if err != nil {
		log.Printf("[usage] 序列化失败: %v", err)
		markDirty()

		return
	}

	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		log.Printf("[usage] 建目录失败: %v", err)
		markDirty()

		return
	}

	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("[usage] 写临时文件失败: %v", err)
		markDirty()

		return
	}

	if err := atomicwrite.Rename(tmp, r.path); err != nil {
		log.Printf("[usage] 原子替换失败: %v", err)

		_ = os.Remove(tmp)

		markDirty()
	}
}

// Save 立即落盘（面板「刷新」或关闭前调用）。
func (r *Recorder) Save() { r.flush(true) }

// ---------------------------------------------------------------- 聚合 ----

// Agg 一组累计量。
type Agg struct {
	Requests           int64   `json:"requests"`
	Errors             int64   `json:"errors"`
	PromptTokens       int64   `json:"prompt_tokens"`
	CompletionTok      int64   `json:"completion_tokens"`
	TotalTokens        int64   `json:"total_tokens"`
	Credits            float64 `json:"credits"`
	CreditSamples      int64   `json:"credit_samples"`
	CreditTokens       int64   `json:"credit_tokens"`
	CreditsPer1MTokens float64 `json:"credits_per_1m_tokens"`
	CacheHitTokens     int64   `json:"cache_hit_tokens,omitempty"`
	CacheMissTokens    int64   `json:"cache_miss_tokens,omitempty"`
	CacheHitRate       float64 `json:"cache_hit_rate,omitempty"`
	AvgLatencyMs       float64 `json:"avg_latency_ms"`
	AvgTTFTMs          float64 `json:"avg_ttft_ms,omitempty"`
	AvgTPS             float64 `json:"avg_tokens_per_second"`
}

// aggAcc 是聚合过程中的累加器：Agg 只放已算好的结果，均值需要样本数才能
// 正确加权（不能对每桶的均值再取平均），所以样本数留在这里。
type aggAcc struct {
	Agg

	latSum      int64
	latSamples  int64
	ttftSum     int64
	ttftSamples int64
	tpsSum      float64
	tpsSamples  int64
}

func (g *aggAcc) add(bucket5 *bucket) {
	g.Requests += bucket5.Req
	g.Errors += bucket5.Err
	g.PromptTokens += bucket5.PT
	g.CompletionTok += bucket5.CT
	g.TotalTokens += bucket5.TT
	g.Credits += bucket5.CR
	g.CreditSamples += bucket5.CRN
	g.CreditTokens += bucket5.CRT
	g.CacheHitTokens += bucket5.CH
	g.CacheMissTokens += bucket5.CM
	g.latSum += bucket5.LatMs
	g.latSamples += bucket5.LatN
	g.ttftSum += bucket5.TFMs
	g.ttftSamples += bucket5.TFN
	g.tpsSum += bucket5.TPS
	g.tpsSamples += bucket5.TPSN
}

func (g *aggAcc) finish() Agg {
	agg := g.Agg
	if g.latSamples > 0 {
		agg.AvgLatencyMs = float64(g.latSum) / float64(g.latSamples)
	}

	if g.ttftSamples > 0 {
		agg.AvgTTFTMs = float64(g.ttftSum) / float64(g.ttftSamples)
	}

	if g.tpsSamples > 0 {
		agg.AvgTPS = g.tpsSum / float64(g.tpsSamples)
	}

	if g.CreditTokens > 0 {
		agg.CreditsPer1MTokens = g.Credits / float64(g.CreditTokens) * 1_000_000
	}

	if total := g.CacheHitTokens + g.CacheMissTokens; total > 0 {
		agg.CacheHitRate = float64(g.CacheHitTokens) / float64(total) * 100
	}

	return agg
}

// KeyedAgg 按某个维度聚合的一行。
type KeyedAgg struct {
	Agg

	Key   string `json:"key"`
	Realm string `json:"realm,omitempty"`
	Extra string `json:"extra,omitempty"` // 账号行放昵称
}

// Point 时序上的一个点。
type Point struct {
	Agg

	T     string `json:"t"`
	Scope string `json:"scope"` // "hour" | "day"
}

// CreditAgg 积分扣除统计的一行。Key 在账号维度是 UID，在模型维度是裸模型名；
// Rate 仅模型维度使用；比例分母只统计与 credit 同时存在的 Token 样本。
type CreditAgg struct {
	Key                string  `json:"key"`
	Realm              string  `json:"realm,omitempty"`
	Nickname           string  `json:"nickname,omitempty"`
	Rate               string  `json:"rate,omitempty"`
	Requests           int64   `json:"requests"`
	Credits            float64 `json:"credits"`
	CreditSamples      int64   `json:"credit_samples"`
	CreditTokens       int64   `json:"credit_tokens"`
	CreditsPer1MTokens float64 `json:"credits_per_1m_tokens"`
	CacheHitTokens     int64   `json:"cache_hit_tokens,omitempty"`
	CacheMissTokens    int64   `json:"cache_miss_tokens,omitempty"`
	CacheHitRate       float64 `json:"cache_hit_rate,omitempty"`
}

type creditAcc struct {
	CreditAgg
}

func (a *creditAcc) add(bucket4 *bucket) {
	a.Requests += bucket4.Req
	a.Credits += bucket4.CR
	a.CreditSamples += bucket4.CRN
	a.CreditTokens += bucket4.CRT
	a.CacheHitTokens += bucket4.CH
	a.CacheMissTokens += bucket4.CM
}

func (a *creditAcc) finish() CreditAgg {
	out := a.CreditAgg
	if a.CreditTokens > 0 {
		out.CreditsPer1MTokens = a.Credits / float64(a.CreditTokens) * 1_000_000
	}

	if total := a.CacheHitTokens + a.CacheMissTokens; total > 0 {
		out.CacheHitRate = float64(a.CacheHitTokens) / float64(total) * 100
	}

	return out
}

// Snapshot 面板一次拉取的全部用量视图数据。
type Snapshot struct {
	Totals          Agg         `json:"totals"`
	ByRealm         []KeyedAgg  `json:"by_realm"`
	ByAccount       []KeyedAgg  `json:"by_account"`
	ByModel         []KeyedAgg  `json:"by_model"`
	Series          []Point     `json:"series"`
	CreditByAccount []CreditAgg `json:"credit_by_account"`
	CreditByModel   []CreditAgg `json:"credit_by_model"`
	Buckets         int         `json:"buckets"`
	FileBytes       int64       `json:"file_bytes"`
	Since           string      `json:"since,omitempty"`
	// WindowFrom/WindowTo 本次实际生效的统计区间（本地时间，RFC3339），供面板
	// 回显口径——「自定义」区间下用户必须能确认服务端到底按哪段算的。
	// 空串 = 该侧不设界（全部历史 / 到今天为止）。
	WindowFrom string `json:"window_from,omitempty"`
	WindowTo   string `json:"window_to,omitempty"`
	Generated  string `json:"generated"`
}

// Window 用量统计窗口。三种口径按优先级解析（见 bounds）：
//   - From/To 任一非零 → 显式区间 [From, To]（To 零值 = 不设上界）
//   - 否则 Hours>0     → 滚动窗口：当前整点桶往回 Hours-1 个整点桶（按墙钟；
//     夏令时回拨的重复小时只算一个桶）
//   - 否则             → 全部历史
//
// 为什么显式区间用「桶起点落在 [From, To] 内」判定而不是求交集：小时桶的粒度
// 就是一小时，用户选到 14:00 时把 14:00 这一小时的桶算进来符合直觉；同时这也
// 让 Hours 口径与历史行为逐位一致（原实现就是 ts.Before(from) 即跳过）。
type Window struct {
	Hours int
	From  time.Time
	To    time.Time
}

// bounds 解析出实际生效的 [from, to]；零值表示该侧不设界。
func (w Window) bounds() (time.Time, time.Time) {
	return w.boundsAt(time.Now())
}

// boundsAt 同 bounds，但用调用方给定的 now 计算滚动窗口起点（单测注入）。
func (w Window) boundsAt(now time.Time) (time.Time, time.Time) {
	if !w.From.IsZero() || !w.To.IsZero() {
		return w.From, w.To
	}

	if w.Hours <= 0 {
		return time.Time{}, time.Time{}
	}

	h := min(w.Hours, 24*60)

	start := localHourStart(now)
	for range h - 1 {
		start = previousHourStart(start)
	}

	return start, time.Time{}
}

// previousHourStart 返回 t（墙钟整点）之前的上一个整点桶起点。
//
// 不能只做 Add(-time.Hour)：夏令时回拨的重复小时里绝对回退一小时仍落在同一个
// 墙钟小时（02:00 CET - 1h = 02:00 CEST），起点不前进，hours>=2 会少算上一个
// 整点桶。优先取 t 的墙钟上一小时的首个实际时刻；该小时被拨快整段抹掉或
// time.Date 选到的实例不早于 t（2 小时回拨）时，再按绝对回退实例去掉分秒，
// 兼容 30 分钟偏移的夏令时（如 Australia/Lord_Howe，Add 后会落在 :30）。
func previousHourStart(t time.Time) time.Time {
	year, month, day := t.Date()

	wantHour := t.Hour() - 1
	if wantHour < 0 {
		wantHour = 23
		// 午夜要回退到前一日 23 点。目标日期取 t-1h 的日期而不是 t.Date()：
		// 午夜拨回当日 00:00 不存在时（America/Santiago 2026-04-05 00:00 -03
		// → 前一日 23:00 -04），按 t.Date() 会取到当日 23 点（未来），再落到
		// 绝对回退的第二次 23 点，与桶键解析出的首次 occurrence 不一致，
		// hours>=2 会漏掉整个前一日 23 点桶。
		year, month, day = t.Add(-time.Hour).Date()
	}

	// 优先取墙钟上一小时的首个实际时刻。hourFirst 处理两种不存在的整点：
	// 被拨快整段抹掉（ok=false，走绝对回退），以及只跳过部分分钟
	// （Pacific/Chatham 03:00 → 03:45，返回该桶真正的起点）。
	if prev, ok := hourFirst(year, month, day, wantHour, t.Location()); ok && prev.Before(t) {
		return prev
	}
	// 上一墙钟小时整段被抹掉，或 2 小时回拨（Antarctica/Troll：03:00+02 →
	// 01:00+00）里墙上一个小时的实例不早于 t。以绝对回退一小时选中的实例为
	// 基准，去掉分秒、保留该实例的偏移——不能重新按墙钟字段取整，那会再次落到
	// 更晚的 occurrence，起点反而前进。
	prev := t.Add(-time.Hour)

	return prev.Add(-(time.Duration(prev.Minute())*time.Minute +
		time.Duration(prev.Second())*time.Second + time.Duration(prev.Nanosecond())))
}

// hourFirst 返回本地日期 year-month-day 的墙钟 hour 点首个实际时刻；该小时被
// 夏令时拨快整段抹掉时 ok=false。
//
// time.Date 对不存在的墙钟时刻按 DST 差值平移，跳过区间起点不在整点时不保证
// 落在区间之后：Pacific/Chatham 的 03:00 会被归一化成 04:00，而该小时真正的
// 起点是 03:45。先信任 time.Date 落在目标小时的常见路径；只有它落到别的
// 小时/日期时才在附近逐分钟找回目标小时的首个实际时刻。
func hourFirst(year int, month time.Month, day, hour int, loc *time.Location) (time.Time, bool) {
	nominal := time.Date(year, month, day, hour, 0, 0, 0, loc)
	if !sameWallDate(nominal, year, month, day) || nominal.Hour() != hour {
		return hourFirstNear(nominal, year, month, day, hour)
	}

	if nominal.Minute() == 0 && nominal.Second() == 0 && nominal.Nanosecond() == 0 {
		return nominal, true
	}

	// 归一化落在目标小时的中间：跳过区间末端就在 nominal 之前，向前回退到
	// 该小时首个实际分钟（30 分钟拨快 02:45 → 03:15 时 Date(03:00)=03:30）。
	// 被回拨的重复小时落到 :00，不会进入这里，仍保留 time.Date 选的 occurrence。
	return hourFirstMinute(nominal, year, month, day, hour), true
}

// hourFirstMinute 从 t 向前回退到同一墙钟小时的首个实际分钟。
func hourFirstMinute(t time.Time, year int, month time.Month, day, hour int) time.Time {
	for {
		prev := t.Add(-time.Minute)
		if !sameWallDate(prev, year, month, day) || prev.Hour() != hour {
			return t
		}

		t = prev
	}
}

// hourFirstNear 在 nominal 附近逐分钟找目标墙钟小时的首个实际时刻。
//
// 现代 tzdata 的 DST 差值不超过 2 小时；±4 小时足以覆盖归一化落点与跳过区间
// （切换点都在整分，步进不引入误差）。
func hourFirstNear(nominal time.Time, year int, month time.Month, day, hour int) (time.Time, bool) {
	start := nominal.Add(-4 * time.Hour)
	end := nominal.Add(4 * time.Hour)

	for t := start; !t.After(end); t = t.Add(time.Minute) {
		if sameWallDate(t, year, month, day) && t.Hour() == hour {
			return t, true
		}
	}

	return time.Time{}, false
}

func sameWallDate(t time.Time, year int, month time.Month, day int) bool {
	y, m, d := t.Date()

	return y == year && m == month && d == day
}

// localHourStart 返回 now 所在本地墙钟整点。
//
// 不能用 time.Truncate(time.Hour)：它按「自零值起的小时倍数」截断，而零值是
// UTC 基准——非整小时偏移的时区（如 +05:30）会得到 :30 的“整点”。滚动窗口
// 起点随之后移半小时，当前小时的桶（:00）被整段排除在窗口外，面板「最近 1 小时」
// 等短窗口会直接显示为空。
func localHourStart(now time.Time) time.Time {
	// 取当前墙钟小时的首个实际时刻；部分小时被拨快跳过时它是跳过区间末端
	// （Chatham 03:45），保证 hours=1 不会把上一小时的桶带进来。
	if start, ok := hourFirst(now.Year(), now.Month(), now.Day(), now.Hour(), now.Location()); ok && !start.After(now) {
		return start
	}
	// 夏令时回拨的重复小时里 time.Date 可能选到更晚的那次 occurrence，得到晚于
	// now 的「起点」——滚动窗口落到未来，hours=1 尚能命中桶，hours>=2 会少算上一
	// 小时。此时按 now 的实际偏移回退到当前小时的起点。
	return now.Add(-(time.Duration(now.Minute())*time.Minute +
		time.Duration(now.Second())*time.Second + time.Duration(now.Nanosecond()))).Round(0)
}

// bucketTime 把桶 scope 解析成本地时间；脏 scope 返回 false（不进任何口径）。
func bucketTime(scope string) (time.Time, bool) {
	if hour, ok := strings.CutPrefix(scope, "h:"); ok {
		parsed, err := time.ParseInLocation(hourLayout, hour, time.Local)
		if err != nil {
			return parsed, false
		}
		// ParseInLocation 与 time.Date 同样会把不存在的整点平移（Chatham 的
		// 03:00 → 04:00、午夜拨快日的 23:00 → 次日 00:00），平移后的小时/
		// 日期已不是 key 表达的那一小时。用 UTC 再解析一次拿到字面墙钟字段，
		// 找回该桶真正的起点，窗口入界才与 previousHourStart 一致；整小时被
		// 抹掉时保留平移结果（即跳过区间之后的第一个实际时刻）。
		if wall, err := time.ParseInLocation(hourLayout, hour, time.UTC); err == nil {
			if start, ok := hourFirst(wall.Year(), wall.Month(), wall.Day(), wall.Hour(), time.Local); ok {
				return start, true
			}
		}

		return parsed, true
	}

	ts, err := time.ParseInLocation(dayLayout, strings.TrimPrefix(scope, "d:"), time.Local)

	return ts, err == nil
}

// Snapshot 聚合**所选窗口内**的桶，产出面板一次拉取的全部用量视图数据。
//
// hours>0：窗口 = [当前整点-(hours-1)小时, now]，卡片汇总/按域/按账号/按模型/
// 时序**全部**按同一窗口口径统计——切窗口时所有数字随之变化（曾长期是"卡片为
// 全部历史累计、hours 只改时序分片"的口径，界面上被读成"筛选没生效"，已废弃）。
// 小时桶按整点入窗；日桶（Rollup 折叠出的长期数据）按日起点入窗，故小时窗口
// 天然不含更早的日桶。
// hours<=0：全部历史（含已折叠日桶），供「全部历史」选项看长期趋势。
//
// nicks 是 uid→昵称映射，仅用于展示。
func (r *Recorder) Snapshot(hours int, nicks map[string]string) Snapshot {
	return r.SnapshotWithRates(hours, nicks, nil)
}

// SnapshotWithRates 与 Snapshot 相同，但允许为缺少历史倍率的旧桶提供当前
// 模型倍率回填。currentRate 返回空串时该行按“未知倍率”聚合，不伪造价格。
func (r *Recorder) SnapshotWithRates(hours int, nicks map[string]string, currentRate func(realm, model string) string) Snapshot {
	return r.SnapshotWindow(Window{Hours: hours, From: time.Time{}, To: time.Time{}}, nicks, currentRate)
}

// SnapshotWindow 聚合**所选窗口内**的桶，产出面板一次拉取的全部用量视图数据。
//
// 窗口语义见 Window：滚动窗口（Hours）/ 显式区间（From-To）/ 全部历史。卡片汇总、
// 按域、按账号、按模型、时序**全部**按同一窗口口径统计——切窗口时所有数字随之变化
// （曾长期是"卡片为全部历史累计、hours 只改时序分片"的口径，界面上被读成"筛选没
// 生效"，已废弃）。小时桶按整点入窗；日桶（Rollup 折叠出的长期数据）按日起点入窗，
// 故小时窗口天然不含更早的日桶。
func (r *Recorder) SnapshotWindow(w Window, nicks map[string]string, currentRate func(realm, model string) string) Snapshot {
	if r == nil {
		return Snapshot{Generated: time.Now().Format(time.RFC3339)}
	}

	from, to := w.bounds()
	windowed := !from.IsZero() || !to.IsZero()

	r.mu.Lock()

	buckets2 := make([]bucket, 0, len(r.buckets))
	for _, b := range r.buckets {
		buckets2 = append(buckets2, *b)
	}
	r.mu.Unlock()

	acc := newSnapshotAcc(nicks, currentRate)
	// 数据起点（全库最早分片）：不受窗口影响，表示"记录自何时开始"。scope 字典序
	// 即时间序（同前缀内同格式排序；"d:" 恒早于 "h:"——日桶只来自 90 天前的小时折叠）。
	since := ""
	matched := 0

	for i := range buckets2 {
		bucket2 := &buckets2[i]
		if since == "" || bucket2.Scope < since {
			since = bucket2.Scope
		}

		if !inWindow(bucket2.Scope, from, to, windowed) {
			continue
		}

		matched++

		acc.add(bucket2)
	}

	snap := acc.snapshot(matched)
	snap.Since = strings.TrimPrefix(strings.TrimPrefix(since, "h:"), "d:")
	snap.FileBytes = r.fileBytes()
	snap.WindowFrom, snap.WindowTo = w.echoBounds()

	return snap
}

// AggregateByModel 返回按裸模型名分组的**全历史**累计（请求数/失败数/Token），
// 供 /metrics 导出。与 SnapshotWindow 的区别：只保留 model 一维，恒为全部历史
// （含已折叠日桶），不构造时序/账号/积分维度。与 SnapshotWindow 的整体拷贝不同，
// 这里在锁内直接遍历聚合（模型维度小、聚合是纯 CPU），避免 maxBuckets 逼近上界时
// 每次抓取拷贝数十 MB；代价是聚合期间阻塞写入，单次抓取量级可接受。
// 空记录器返回空切片（非 nil），调用方无需判空。
func (r *Recorder) AggregateByModel() []KeyedAgg {
	if r == nil {
		return []KeyedAgg{}
	}

	agg := make(map[string]*aggAcc)

	r.mu.Lock()
	for _, item := range r.buckets {
		acc := agg[item.Model]
		if acc == nil {
			acc = &aggAcc{}
			agg[item.Model] = acc
		}

		acc.add(item)
	}
	r.mu.Unlock()

	out := make([]KeyedAgg, 0, len(agg))
	for k, v := range agg {
		out = append(out, KeyedAgg{Key: k, Agg: v.finish(), Realm: "", Extra: ""})
	}
	// 按 key 升序，保证 /metrics 输出稳定（Prometheus 虽无序，但稳定输出便于 diff）。
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })

	return out
}

// inWindow 判断桶 scope 是否落在窗口 [from, to] 内（零值侧不设界）；windowed=false
// 时全收（全部历史）。scope 解析失败的脏桶不进窗口聚合（也不该出现在任何口径里）。
func inWindow(scope string, from, to time.Time, windowed bool) bool {
	if !windowed {
		return true
	}

	stamp, ok := bucketTime(scope)
	if !ok {
		return false
	}

	if !from.IsZero() && stamp.Before(from) {
		return false
	}

	if !to.IsZero() && stamp.After(to) {
		return false
	}

	return true
}

// echoBounds 返回需要回显给面板的显式区间（仅显式口径；滚动窗口由 hours 表达，
// 全部历史没有区间——两者回显都会变成噪音）。
func (w Window) echoBounds() (string, string) {
	if w.From.IsZero() && w.To.IsZero() {
		return "", ""
	}

	var from, to string
	if !w.From.IsZero() {
		from = w.From.Format(time.RFC3339)
	}

	if !w.To.IsZero() {
		to = w.To.Format(time.RFC3339)
	}

	return from, to
}

// fileBytes 返回落盘文件大小（未落盘或读取失败返回 0）。
func (r *Recorder) fileBytes() int64 {
	if r.path == "" {
		return 0
	}

	if fi, err := os.Stat(r.path); err == nil {
		return fi.Size()
	}

	return 0
}

// snapshotAcc 快照聚合累加器：把逐桶累计分摊到各维度，避免单函数堆叠全部维度。
type snapshotAcc struct {
	total          aggAcc
	realmAgg       map[string]*aggAcc
	acctAgg        map[string]*aggAcc
	acctRealm      map[string]string
	modelAgg       map[string]*aggAcc
	hourSeries     map[string]*aggAcc
	daySeries      map[string]*aggAcc
	creditAcctAgg  map[string]*creditAcc
	creditModelAgg map[string]*creditAcc
	rateCache      map[string]string
	nicks          map[string]string
	currentRate    func(realm, model string) string
}

func newSnapshotAcc(nicks map[string]string, currentRate func(realm, model string) string) *snapshotAcc {
	return &snapshotAcc{
		realmAgg:       map[string]*aggAcc{},
		acctAgg:        map[string]*aggAcc{},
		acctRealm:      map[string]string{},
		modelAgg:       map[string]*aggAcc{},
		hourSeries:     map[string]*aggAcc{},
		daySeries:      map[string]*aggAcc{},
		creditAcctAgg:  map[string]*creditAcc{},
		creditModelAgg: map[string]*creditAcc{},
		rateCache:      map[string]string{},
		nicks:          nicks,
		currentRate:    currentRate,
	}
}

func (a *snapshotAcc) add(bucket2 *bucket) {
	a.total.add(bucket2)
	a.addKeyed(a.realmAgg, bucket2.Realm, bucket2)
	a.addKeyed(a.acctAgg, bucket2.UID, bucket2)
	// 一个账号只属于一个 realm，这里记下来供前端展示「域」列；keyed() 的 Realm
	// 字段默认是空的（它按 key 分组，不知道 realm）。
	if a.acctRealm[bucket2.UID] == "" {
		a.acctRealm[bucket2.UID] = bucket2.Realm
	}

	a.addKeyed(a.modelAgg, bucket2.Model, bucket2)

	if hour, ok := strings.CutPrefix(bucket2.Scope, "h:"); ok {
		a.addKeyed(a.hourSeries, hour, bucket2)
	} else {
		a.addKeyed(a.daySeries, strings.TrimPrefix(bucket2.Scope, "d:"), bucket2)
	}

	if bucket2.CRN > 0 {
		a.addCredit(bucket2)
	}
}

// addKeyed 按 key 取（或新建）该维度的累加器后并入桶。
func (a *snapshotAcc) addKeyed(m map[string]*aggAcc, key string, b *bucket) {
	if m[key] == nil {
		m[key] = &aggAcc{}
	}

	m[key].add(b)
}

// addCredit 并入积分维度：账号行与「裸模型名 + 生效倍率」行。
func (a *snapshotAcc) addCredit(bucket3 *bucket) {
	creditAcc3 := a.creditAcctAgg[bucket3.UID]
	if creditAcc3 == nil {
		creditAcc3 = &creditAcc{Key: bucket3.UID, Realm: bucket3.Realm, Nickname: a.nicks[bucket3.UID]}
		a.creditAcctAgg[bucket3.UID] = creditAcc3
	}

	creditAcc3.add(bucket3)

	model := canonicalUsageModel(bucket3.Model)
	rate := a.resolveRate(bucket3.Realm, model, bucket3.Rate)
	modelKey := model + "\x00" + rate

	creditAcc2 := a.creditModelAgg[modelKey]
	if creditAcc2 == nil {
		creditAcc2 = &creditAcc{Key: model, Rate: rate}
		a.creditModelAgg[modelKey] = creditAcc2
	}

	creditAcc2.add(bucket3)
}

// resolveRate 旧桶缺倍率时用当前目录倍率回填（带缓存）。rate 非空或未注入
// currentRate 时原样返回。
func (a *snapshotAcc) resolveRate(realm, model, rate string) string {
	if rate != "" || a.currentRate == nil {
		return rate
	}

	cacheKey := realm + "\x00" + model
	if cached, ok := a.rateCache[cacheKey]; ok {
		return cached
	}

	rate = a.currentRate(realm, model)
	a.rateCache[cacheKey] = rate

	return rate
}

// snapshot 把累加器定型为对外快照（matched 为窗口内命中的桶数）。
func (a *snapshotAcc) snapshot(matched int) Snapshot {
	snap := Snapshot{
		Totals: a.total.finish(),
		ByRealm: keyed(a.realmAgg, func(k string) (string, string) {
			return k, ""
		}),
		ByAccount: keyed(a.acctAgg, func(k string) (string, string) {
			return k, a.nicks[k]
		}),
		ByModel:         keyed(a.modelAgg, func(k string) (string, string) { return k, "" }),
		CreditByAccount: creditKeyed(a.creditAcctAgg),
		CreditByModel:   creditKeyed(a.creditModelAgg),
		Buckets:         matched,
		Generated:       time.Now().Format(time.RFC3339),
	}
	for i := range snap.ByAccount {
		snap.ByAccount[i].Realm = a.acctRealm[snap.ByAccount[i].Key]
	}
	// 日点（升序）+ 小时点（升序）拼成一条连续时序。
	snap.Series = append(seriesPoints(a.daySeries, "day"), seriesPoints(a.hourSeries, "hour")...)

	return snap
}

// seriesPoints 把某分片维度的累加器转成升序时序点；无数据返回 nil（保持 JSON
// 序列化的 null 语义，与历史行为一致）。
func seriesPoints(m map[string]*aggAcc, scope string) []Point {
	if len(m) == 0 {
		return nil
	}

	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	pts := make([]Point, 0, len(keys))
	for _, k := range keys {
		pts = append(pts, Point{T: k, Scope: scope, Agg: m[k].finish()})
	}

	return pts
}

func canonicalUsageModel(model string) string {
	model = strings.TrimSpace(model)
	for _, prefix := range []string{"cn:", "global:"} {
		if rest, ok := strings.CutPrefix(model, prefix); ok {
			model = rest
			break
		}
	}

	if model == "" {
		return "(unknown)"
	}

	return model
}

func creditKeyed(m map[string]*creditAcc) []CreditAgg {
	out := make([]CreditAgg, 0, len(m))
	for _, v := range m {
		out = append(out, v.finish())
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Credits != out[j].Credits {
			return out[i].Credits > out[j].Credits
		}

		if out[i].CreditTokens != out[j].CreditTokens {
			return out[i].CreditTokens > out[j].CreditTokens
		}

		if out[i].Key != out[j].Key {
			return out[i].Key < out[j].Key
		}

		return out[i].Rate < out[j].Rate
	})

	return out
}

func keyed(m map[string]*aggAcc, label func(string) (string, string)) []KeyedAgg {
	out := make([]KeyedAgg, 0, len(m))
	for k, v := range m {
		key, extra := label(k)
		out = append(out, KeyedAgg{Key: key, Extra: extra, Agg: v.finish(), Realm: ""})
	}
	// 按总量降序；同量按 key 升序，保证输出稳定（前端 diff 不抖）。
	sort.Slice(out, func(i, j int) bool {
		if out[i].TotalTokens != out[j].TotalTokens {
			return out[i].TotalTokens > out[j].TotalTokens
		}

		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}

		return out[i].Key < out[j].Key
	})

	return out
}

// Describe 返回一行人类可读的占用摘要（启动日志用）。
func (r *Recorder) Describe() string {
	if r == nil {
		return "disabled"
	}

	r.mu.Lock()
	n := len(r.buckets)
	r.mu.Unlock()
	sz := r.fileBytes()

	return fmt.Sprintf("%d buckets, file %d bytes", n, sz)
}
