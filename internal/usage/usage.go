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

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
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

// Start 启动后台防抖落盘与折叠。Stop 前一直运行。
func (r *Recorder) Start() {
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

// Stop 停止后台循环并做最后一次落盘。
func (r *Recorder) Stop() {
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
func (r *Recorder) Add(now time.Time, realm, uid, model string, d Delta, ok bool) {
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
	key := scope + "|" + realm + "|" + uid + "|" + model + "|" + d.ModelRate

	r.mu.Lock()
	defer r.mu.Unlock()

	b := r.buckets[key]
	if b == nil {
		b = &bucket{Scope: scope, Realm: realm, UID: uid, Model: model, Rate: d.ModelRate}
		r.buckets[key] = b
	}
	b.apply(d, ok)
	r.dirty = true
}

// BackfillRow 上游逐笔用量按天聚合的一行（issue #31）。
type BackfillRow struct {
	Day      string // 本地日历日 "2006-01-02"
	Realm    string
	UID      string
	Model    string
	Requests int64
	Credits  float64
	Tokens   int64
}

// Backfill 用上游权威数据覆盖指定 (天, realm, uid, model) 的用量桶：删除该键的本地
// 小时桶与日桶后写入标记 Synced 的日桶，避免与本地实时记录重复计数（issue #31）。
// 返回覆盖的桶数。仅显式触发时调用。
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
		r.applyBackfillLocked(row)
		n++
	}
	if n > 0 {
		r.dirty = true
	}
	return n
}

// applyBackfillLocked 覆盖单个 (天, realm, uid, model) 的桶：删本地桶后写 Synced 日桶。
func (r *Recorder) applyBackfillLocked(row BackfillRow) {
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
	for k, b := range r.buckets {
		if b.UID == row.UID && b.Realm == realm && b.Model == model &&
			(b.Scope == dayScope || strings.HasPrefix(b.Scope, hourPrefix)) {
			delete(r.buckets, k)
		}
	}
	b := &bucket{
		Scope:  dayScope,
		Realm:  realm,
		UID:    row.UID,
		Model:  model,
		Req:    row.Requests,
		TT:     row.Tokens,
		Synced: true,
	}
	if row.Credits != 0 {
		b.CR = row.Credits
		b.CRN = row.Requests
		b.CRT = row.Tokens
	}
	r.buckets[dayScope+"|"+realm+"|"+row.UID+"|"+model+"|"] = b
}

// tokenTotal 返回本次尝试的 token 合计：优先上游给的 total，缺省用 pt+ct 兜底，
// 保证总量口径连续（两条链路——总量与积分比例分母——共用同一口径）。
func (d Delta) tokenTotal() int64 {
	if d.HasTotal {
		return d.TotalTokens
	}
	if d.HasPromptTokens || d.HasCompletion {
		return d.PromptTokens + d.CompletionTokens
	}
	return 0
}

// apply 把一次尝试的增量并入桶。
func (b *bucket) apply(d Delta, ok bool) {
	b.Req++
	if !ok {
		b.Err++
	}
	if d.HasPromptTokens {
		b.PT += d.PromptTokens
	}
	if d.HasCompletion {
		b.CT += d.CompletionTokens
	}
	b.TT += d.tokenTotal()
	if d.HasCredit {
		b.CR += d.Credit
		b.CRN++
		// 比例只使用同一次请求同时具备 credit 与 token 的样本，避免把
		// 仅 token 的旧记录或仅 credit 的观测混进分母。
		b.CRT += d.tokenTotal()
	}
	if d.HasCacheTokens {
		b.CH += d.CacheHitTokens
		b.CM += d.CacheMissTokens
	}
	if d.HasLatency {
		b.LatMs += d.LatencyMs
		b.LatN++
	}
	if d.HasTTFT {
		b.TFMs += d.TTFTMs
		b.TFN++
	}
	if d.HasTPS {
		b.TPS += d.TokensPerSecond
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
	for k, b := range r.buckets {
		if !strings.HasPrefix(b.Scope, "h:") {
			continue
		}
		ts, err := time.ParseInLocation(hourLayout, strings.TrimPrefix(b.Scope, "h:"), time.Local)
		if err != nil || !ts.Before(cutoff) {
			continue
		}
		day := "d:" + ts.Format(dayLayout)
		moves = append(moves, move{from: k, to: day + "|" + b.Realm + "|" + b.UID + "|" + b.Model + "|" + b.Rate})
	}
	for _, m := range moves {
		src := r.buckets[m.from]
		if src == nil {
			continue
		}
		dst := r.buckets[m.to]
		if dst == nil {
			cp := *src
			cp.Scope = strings.SplitN(m.to, "|", 2)[0]
			dst = &cp
			r.buckets[m.to] = dst
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
		delete(r.buckets, m.from)
	}
	if len(moves) > 0 {
		r.dirty = true
		log.Printf("[usage] 折叠 %d 个小时桶为日桶（保留 %v 细粒度）", len(moves), hourlyKeep)
	}
}

// ---------------------------------------------------------------- 持久化 ----

func (r *Recorder) load() error {
	raw, err := os.ReadFile(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		return err
	}
	for i := range f.Buckets {
		b := f.Buckets[i]
		r.buckets[b.Scope+"|"+b.Realm+"|"+b.UID+"|"+b.Model+"|"+b.Rate] = &b
	}
	log.Printf("[usage] 已恢复 %d 个用量桶（%s）", len(r.buckets), r.path)
	return nil
}

func (r *Recorder) flush(force bool) {
	if r == nil || r.path == "" {
		return
	}
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

	raw, err := json.Marshal(snap)
	if err != nil {
		log.Printf("[usage] 序列化失败: %v", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		log.Printf("[usage] 建目录失败: %v", err)
		return
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("[usage] 写临时文件失败: %v", err)
		return
	}
	if err := atomicwrite.Rename(tmp, r.path); err != nil {
		log.Printf("[usage] 原子替换失败: %v", err)
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

func (g *aggAcc) add(b *bucket) {
	g.Requests += b.Req
	g.Errors += b.Err
	g.PromptTokens += b.PT
	g.CompletionTok += b.CT
	g.TotalTokens += b.TT
	g.Credits += b.CR
	g.CreditSamples += b.CRN
	g.CreditTokens += b.CRT
	g.CacheHitTokens += b.CH
	g.CacheMissTokens += b.CM
	g.latSum += b.LatMs
	g.latSamples += b.LatN
	g.ttftSum += b.TFMs
	g.ttftSamples += b.TFN
	g.tpsSum += b.TPS
	g.tpsSamples += b.TPSN
}

func (g *aggAcc) finish() Agg {
	a := g.Agg
	if g.latSamples > 0 {
		a.AvgLatencyMs = float64(g.latSum) / float64(g.latSamples)
	}
	if g.ttftSamples > 0 {
		a.AvgTTFTMs = float64(g.ttftSum) / float64(g.ttftSamples)
	}
	if g.tpsSamples > 0 {
		a.AvgTPS = g.tpsSum / float64(g.tpsSamples)
	}
	if g.CreditTokens > 0 {
		a.CreditsPer1MTokens = g.Credits / float64(g.CreditTokens) * 1_000_000
	}
	if total := g.CacheHitTokens + g.CacheMissTokens; total > 0 {
		a.CacheHitRate = float64(g.CacheHitTokens) / float64(total) * 100
	}
	return a
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

func (a *creditAcc) add(b *bucket) {
	a.Requests += b.Req
	a.Credits += b.CR
	a.CreditSamples += b.CRN
	a.CreditTokens += b.CRT
	a.CacheHitTokens += b.CH
	a.CacheMissTokens += b.CM
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
//   - 否则 Hours>0     → 滚动窗口：当前整点往回 Hours-1 小时
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
	if !w.From.IsZero() || !w.To.IsZero() {
		return w.From, w.To
	}
	if w.Hours <= 0 {
		return time.Time{}, time.Time{}
	}
	h := w.Hours
	if h > 24*60 {
		h = 24 * 60
	}
	return time.Now().Truncate(time.Hour).Add(-time.Duration(h-1) * time.Hour), time.Time{}
}

// bucketTime 把桶 scope 解析成本地时间；脏 scope 返回 false（不进任何口径）。
func bucketTime(scope string) (time.Time, bool) {
	if strings.HasPrefix(scope, "h:") {
		ts, err := time.ParseInLocation(hourLayout, strings.TrimPrefix(scope, "h:"), time.Local)
		return ts, err == nil
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
	return r.SnapshotWindow(Window{Hours: hours}, nicks, currentRate)
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
	bs := make([]bucket, 0, len(r.buckets))
	for _, b := range r.buckets {
		bs = append(bs, *b)
	}
	r.mu.Unlock()

	acc := newSnapshotAcc(nicks, currentRate)
	// 数据起点（全库最早分片）：不受窗口影响，表示"记录自何时开始"。scope 字典序
	// 即时间序（同前缀内同格式排序；"d:" 恒早于 "h:"——日桶只来自 90 天前的小时折叠）。
	since := ""
	matched := 0
	for i := range bs {
		b := &bs[i]
		if since == "" || b.Scope < since {
			since = b.Scope
		}
		if !inWindow(b.Scope, from, to, windowed) {
			continue
		}
		matched++
		acc.add(b)
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
	for _, b := range r.buckets {
		acc := agg[b.Model]
		if acc == nil {
			acc = &aggAcc{}
			agg[b.Model] = acc
		}
		acc.add(b)
	}
	r.mu.Unlock()

	out := make([]KeyedAgg, 0, len(agg))
	for k, v := range agg {
		out = append(out, KeyedAgg{Key: k, Agg: v.finish()})
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
	ts, ok := bucketTime(scope)
	if !ok {
		return false
	}
	if !from.IsZero() && ts.Before(from) {
		return false
	}
	if !to.IsZero() && ts.After(to) {
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

func (a *snapshotAcc) add(b *bucket) {
	a.total.add(b)
	a.addKeyed(a.realmAgg, b.Realm, b)
	a.addKeyed(a.acctAgg, b.UID, b)
	// 一个账号只属于一个 realm，这里记下来供前端展示「域」列；keyed() 的 Realm
	// 字段默认是空的（它按 key 分组，不知道 realm）。
	if a.acctRealm[b.UID] == "" {
		a.acctRealm[b.UID] = b.Realm
	}
	a.addKeyed(a.modelAgg, b.Model, b)

	if strings.HasPrefix(b.Scope, "h:") {
		a.addKeyed(a.hourSeries, strings.TrimPrefix(b.Scope, "h:"), b)
	} else {
		a.addKeyed(a.daySeries, strings.TrimPrefix(b.Scope, "d:"), b)
	}
	if b.CRN > 0 {
		a.addCredit(b)
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
func (a *snapshotAcc) addCredit(b *bucket) {
	ca := a.creditAcctAgg[b.UID]
	if ca == nil {
		ca = &creditAcc{CreditAgg: CreditAgg{
			Key:      b.UID,
			Realm:    b.Realm,
			Nickname: a.nicks[b.UID],
		}}
		a.creditAcctAgg[b.UID] = ca
	}
	ca.add(b)

	model := canonicalUsageModel(b.Model)
	rate := a.resolveRate(b.Realm, model, b.Rate)
	modelKey := model + "\x00" + rate
	cm := a.creditModelAgg[modelKey]
	if cm == nil {
		cm = &creditAcc{CreditAgg: CreditAgg{Key: model, Rate: rate}}
		a.creditModelAgg[modelKey] = cm
	}
	cm.add(b)
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
		if strings.HasPrefix(model, prefix) {
			model = strings.TrimPrefix(model, prefix)
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
		out = append(out, KeyedAgg{Key: key, Extra: extra, Agg: v.finish()})
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
