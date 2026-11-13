// Package reqlog 记录脱敏的请求级指标与可选 JSONL 归档。
//
// 内存指标有界保存最近 100 条并维护进程级计数；磁盘归档只写请求元数据，
// 不写提示词、响应正文、Authorization 或其它凭证。可选的调用来源（客户端 IP /
// User-Agent，见 Event.ClientIP/UserAgent）由 server 按配置开关决定是否填充。
// 归档队列满时丢弃并计数，不允许日志写盘阻塞模型请求。
package reqlog

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/logrotate"
)

const (
	recentLimit         = 100
	defaultFileMax      = logrotate.DefaultFileMaxBytes
	defaultQueue        = 1024
	defaultReadMax      = 1000
	defaultReadFallback = 200
	defaultMaxBytes     = logrotate.DefaultMaxBytes
	// archivePrefix/archiveExt 归档文件命名：requests-<day>[.N].jsonl。
	archivePrefix = "requests"
	archiveExt    = ".jsonl"
)

// NewRequestID 生成不含用户信息的本地请求 ID。
func NewRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		return fmt.Sprintf("req-%x", b[:])
	}

	return fmt.Sprintf("req-%d", time.Now().UnixNano())
}

const (
	OutcomeSuccess     = "success"
	OutcomeHTTPError   = "http_error"
	OutcomeStreamError = "stream_error"
	OutcomeInterrupted = "interrupted"
)

// Config 归档参数。Enabled=false 时仍保留内存指标。
type Config struct {
	Dir           string
	Enabled       bool
	RetentionDays int
	MaxBytes      int64
	FileMaxBytes  int64
	QueueSize     int
}

// Event 是一条脱敏请求记录。Account 只保存“昵称(uid8)”标签，不保存完整 UID。
//
// ClientIP / UserAgent 是**调用来源**：面板「运行日志」用它回答"这条请求是谁打进来的"。
// 二者由 server 侧按 logging.request_client_info 开关决定是否填充（关掉即保持空串，
// 归档里不会出现来源字段）——来源信息比 token 计数敏感，运营可自行决定是否落盘。
// 仍然不写提示词、响应正文、Authorization 或其它凭证。
type Event struct {
	Time      time.Time `json:"time"`
	RequestID string    `json:"request_id"`
	Path      string    `json:"path"`
	Account   string    `json:"account,omitempty"`
	// KeyID 本次请求命中的命名密钥 ID（默认密钥 / 未鉴权为空）。只记 ID，
	// 不记密钥前缀或明文；面板可用它把请求归到具体分发对象。
	KeyID            string  `json:"key_id,omitempty"`
	Model            string  `json:"model,omitempty"`
	Status           int     `json:"status"`
	OK               bool    `json:"ok"`
	Outcome          string  `json:"outcome"`
	DurationMs       int64   `json:"duration_ms"`
	TTFBMs           int64   `json:"ttfb_ms,omitempty"`
	Attempts         int     `json:"attempts,omitempty"`
	PromptTokens     int64   `json:"prompt_tokens,omitempty"`
	CompletionTokens int64   `json:"completion_tokens,omitempty"`
	TotalTokens      int64   `json:"total_tokens,omitempty"`
	Credit           float64 `json:"credit,omitempty"`
	HasCredit        bool    `json:"credit_known"`
	// CacheHitTokens / CacheMissTokens 上游前缀缓存命中/未命中 token（issue #92）。
	// 上游未回该维度时两者皆零值省略；hit=0 + miss>0 即整段未命中。
	CacheHitTokens  int64 `json:"cache_hit_tokens,omitempty"`
	CacheMissTokens int64 `json:"cache_miss_tokens,omitempty"`
	// ToolsDeclared / ToolCalls 工具面统计（issue #329）：声明数只数本请求
	// tools/functions（不含会话历史），调用数只数实际产生的 tool_call 条数。
	// 两者都 omitempty：ToolsDeclared 缺失 = 本请求没声明 tools（不进分母），
	// ToolsDeclared 存在而 ToolCalls 缺失 = 声明了但零调用（这是要观察的形态）。
	ToolsDeclared int    `json:"tools_declared,omitempty"`
	ToolCalls     int    `json:"tool_calls,omitempty"`
	ClientIP      string `json:"client_ip,omitempty"`
	UserAgent     string `json:"user_agent,omitempty"`
	// Decision 可选选号决策链（logging.decision_trace，issue #63）；默认不记录。
	Decision *DecisionTrace `json:"decision,omitempty"`
}

// DecisionTrace 一次请求的选号决策快照：固定字段 + 有界数组，不含提示词/正文/凭证。
type DecisionTrace struct {
	Candidates int                 `json:"candidates"`
	Excluded   map[string]int      `json:"excluded,omitempty"`
	CostTier   int                 `json:"cost_tier"`
	Explored   bool                `json:"explored,omitempty"`
	StickyHit  bool                `json:"sticky_hit,omitempty"`
	Rotations  []string            `json:"rotations,omitempty"`
	Top        []DecisionCandidate `json:"top,omitempty"`
	FinalUID   string              `json:"final_uid,omitempty"`
	// Strategy 实际生效的选号策略（weighted_random 时省略；有序策略便于排障）。
	Strategy string `json:"strategy,omitempty"`
}

// DecisionCandidate Top-N 候选（uid8 + 权重 + 成本层）。
type DecisionCandidate struct {
	UID    string  `json:"uid"`
	Weight float64 `json:"weight"`
	Tier   int     `json:"tier"`
}

// Filter 用于从归档中筛选最近记录。字符串字段一律「包含」匹配（大小写不敏感），
// 便于面板用一段 IP 前缀或 UA 片段捞请求；From/To 是闭区间（零值 = 该侧不设界），
// 供「今天 / 近 7 天 / 自定义区间」这类时间查询使用。
type Filter struct {
	Outcome   string
	Account   string
	KeyID     string
	Model     string
	ClientIP  string
	UserAgent string
	From      time.Time
	To        time.Time
}

// ArchiveStats 归档存储状态。
type ArchiveStats struct {
	Enabled       bool   `json:"enabled"`
	Dir           string `json:"dir,omitempty"`
	Files         int    `json:"files"`
	Bytes         int64  `json:"bytes"`
	DroppedWrites uint64 `json:"dropped_writes"`
	LastError     string `json:"last_error,omitempty"`
}

// Snapshot 一次面板读取的完整指标快照。
type Snapshot struct {
	StartedAt       time.Time    `json:"started_at"`
	Completed       int64        `json:"completed"`
	InFlight        int64        `json:"in_flight"`
	Succeeded       int64        `json:"succeeded"`
	Failed          int64        `json:"failed"`
	SuccessRate     float64      `json:"success_rate"`
	HTTPSuccessRate float64      `json:"http_success_rate"`
	AvgDurationMs   float64      `json:"avg_duration_ms"`
	Recent          []Event      `json:"recent"`
	Archive         ArchiveStats `json:"archive"`
}

// Recorder 并发安全的有界请求指标与归档记录器。
type Recorder struct {
	mu          sync.Mutex
	started     time.Time
	inFlight    int64
	completed   int64
	succeeded   int64
	httpSuccess int64
	durationSum int64
	recent      []Event
	archive     *archiveWriter
}

// New 创建记录器；Dir 为空或 Enabled=false 时只启用内存指标。
func New(cfg Config) *Recorder {
	if cfg.FileMaxBytes <= 0 {
		cfg.FileMaxBytes = defaultFileMax
	}

	if cfg.QueueSize <= 0 {
		cfg.QueueSize = defaultQueue
	}

	r := &Recorder{started: time.Now(), recent: make([]Event, 0, recentLimit)}
	r.archive = newArchiveWriter(cfg)

	return r
}

// Begin 标记一个请求进入处理。
func (r *Recorder) Begin() {
	if r == nil {
		return
	}

	r.mu.Lock()
	r.inFlight++
	r.mu.Unlock()
}

// Record 记录一个请求完成事件并写入归档队列。
func (r *Recorder) Record(e *Event) {
	if r == nil {
		return
	}

	event := *e
	if event.Time.IsZero() {
		event.Time = time.Now()
	}

	event.normalizeOutcome()
	r.mu.Lock()
	r.observe(&event)
	r.mu.Unlock()

	if r.archive != nil {
		r.archive.enqueue(&event)
	}
}

// normalizeOutcome 在未显式指定结果时按状态码与成功标志推断 outcome。
func (e *Event) normalizeOutcome() {
	if e.Outcome != "" {
		return
	}

	if e.Status == 200 && e.OK {
		e.Outcome = OutcomeSuccess
		return
	}

	e.Outcome = OutcomeHTTPError
}

// observe 在持锁状态下累计内存指标并保留最近事件。
func (r *Recorder) observe(event3 *Event) {
	if r.inFlight > 0 {
		r.inFlight--
	}

	r.completed++
	if event3.OK {
		r.succeeded++
	}

	if event3.Status >= 200 && event3.Status < 300 {
		r.httpSuccess++
	}

	r.durationSum += event3.DurationMs

	r.recent = append([]Event{*event3}, r.recent...)
	if len(r.recent) > recentLimit {
		r.recent = r.recent[:recentLimit]
	}
}

// Snapshot 返回进程内指标和归档状态。
func (r *Recorder) Snapshot() Snapshot {
	if r == nil {
		return Snapshot{}
	}

	r.mu.Lock()

	snapshot := Snapshot{
		StartedAt: r.started,
		Completed: r.completed,
		InFlight:  r.inFlight,
		Succeeded: r.succeeded,
		Recent:    append([]Event(nil), r.recent...),
	}
	if r.completed > 0 {
		snapshot.Failed = r.completed - r.succeeded
		snapshot.SuccessRate = round1(float64(r.succeeded) / float64(r.completed) * 100)
		snapshot.HTTPSuccessRate = round1(float64(r.httpSuccess) / float64(r.completed) * 100)
		snapshot.AvgDurationMs = float64(r.durationSum) / float64(r.completed)
	}
	r.mu.Unlock()

	if r.archive != nil {
		snapshot.Archive = r.archive.stats()
	}

	return snapshot
}

// ReadArchive 返回最近的归档事件（按时间倒序）。limit<=0 时回落 200，最大 1000。
func (r *Recorder) ReadArchive(limit int, filter *Filter) ([]Event, error) {
	if r == nil || r.archive == nil {
		return nil, nil
	}

	return r.archive.read(limit, filter)
}

// Close 刷盘并停止后台归档。
func (r *Recorder) Close() {
	if r == nil || r.archive == nil {
		return
	}

	r.archive.close()
}

func round1(v float64) float64 {
	return float64(int(v*10+0.5)) / 10
}

type archiveWriter struct {
	cfg       Config
	ch        chan Event
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	dropped   atomic.Uint64
	lastErrMu sync.Mutex
	lastErr   string

	// out 按天/按大小轮转的共用写入器（internal/logrotate）；nil = 未启用。
	out *logrotate.Writer
}

func newArchiveWriter(cfg Config) *archiveWriter {
	if !cfg.Enabled || strings.TrimSpace(cfg.Dir) == "" {
		return &archiveWriter{cfg: cfg}
	}

	if cfg.RetentionDays <= 0 {
		cfg.RetentionDays = 7
	}

	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = defaultMaxBytes
	}

	if cfg.FileMaxBytes <= 0 {
		cfg.FileMaxBytes = defaultFileMax
	}

	if cfg.QueueSize <= 0 {
		cfg.QueueSize = defaultQueue
	}

	w := &archiveWriter{
		cfg:  cfg,
		ch:   make(chan Event, cfg.QueueSize),
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}

	out, err := logrotate.New(&logrotate.Config{
		Dir:           cfg.Dir,
		Prefix:        archivePrefix,
		Ext:           archiveExt,
		FileMaxBytes:  cfg.FileMaxBytes,
		MaxBytes:      cfg.MaxBytes,
		RetentionDays: cfg.RetentionDays,
	})
	if err != nil {
		w.setErr(err)
		w.cfg.Enabled = false
		close(w.done)

		return w
	}

	w.out = out

	go w.run()

	return w
}

func (w *archiveWriter) enabled() bool {
	return w != nil && w.cfg.Enabled && w.cfg.Dir != "" && w.done != nil
}

func (w *archiveWriter) enqueue(event *Event) {
	if !w.enabled() {
		return
	}

	select {
	case w.ch <- *event:
	default:
		w.dropped.Add(1)
	}
}

func (w *archiveWriter) run() {
	defer close(w.done)

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case e := <-w.ch:
			w.writeEvent(&e)
		case <-ticker.C:
			w.flush()
			w.prune()
		case <-w.stop:
			for {
				select {
				case e := <-w.ch:
					w.writeEvent(&e)
				default:
					w.flush()
					_ = w.out.Close()

					return
				}
			}
		}
	}
}

func (w *archiveWriter) writeEvent(event2 *Event) {
	if w.out == nil {
		return
	}

	raw, err := json.Marshal(event2)
	if err != nil {
		w.setErr(err)
		return
	}

	now := event2.Time
	if now.IsZero() {
		now = time.Now()
	}

	if _, err := w.out.WriteAt(now, append(raw, '\n')); err != nil {
		w.setErr(err)
	}
}

func (w *archiveWriter) flush() {
	if w.out == nil {
		return
	}

	if err := w.out.Flush(); err != nil {
		w.setErr(err)
	}
}

// prune 按保留期/总容量裁剪旧归档（共用 logrotate 的裁剪口径）。
func (w *archiveWriter) prune() {
	if !w.enabled() || w.out == nil {
		return
	}

	w.out.Prune(time.Now())
}

func (w *archiveWriter) read(limit int, filter *Filter) ([]Event, error) {
	if !w.enabled() || w.out == nil {
		return nil, nil
	}

	limit = clampReadLimit(limit)

	files, err := w.out.Files()
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	// 文件读取顺序无关紧要：结果一律按事件时间排序。不能依赖归档文件的 mtime 还原时间
	// 顺序——同一秒内连续轮转写出的多个文件 mtime 经常完全相同（Linux 文件时间戳粒度粗）；
	// 目录被整体拷贝 / 恢复备份后 mtime 更不可信。
	var out []Event

	for _, file := range files {
		rows, err := readFile(file.Path, filter)
		if err != nil {
			return out, err
		}

		out = append(out, rows...)
	}
	// 契约：按事件时间倒序返回最近 limit 条（同一时刻用 Stable 保留落盘先后）。
	slices.SortStableFunc(out, func(a, b Event) int { return b.Time.Compare(a.Time) })

	if len(out) > limit {
		out = out[:limit]
	}

	return out, nil
}

// clampReadLimit 归一化读取条数：<=0 回落 200，上限 1000。
func clampReadLimit(limit int) int {
	if limit <= 0 {
		return defaultReadFallback
	}

	if limit > defaultReadMax {
		return defaultReadMax
	}

	return limit
}

func readFile(path string, filter *Filter) ([]Event, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("readFile: %w", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)

	var out []Event

	for scanner.Scan() {
		var e Event
		if json.Unmarshal(scanner.Bytes(), &e) != nil || !filter.match(&e) {
			continue
		}

		out = append(out, e)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read archive: %w", err)
	}

	return out, nil
}

func (f *Filter) match(event4 *Event) bool {
	if f.Outcome != "" && event4.Outcome != f.Outcome {
		return false
	}

	if !f.matchTextFields(event4) {
		return false
	}

	return f.matchTimeRange(event4.Time)
}

// matchTextFields 对所有字符串字段做大小写不敏感的包含匹配，全部命中才返回 true。
func (f *Filter) matchTextFields(e *Event) bool {
	return containsFold(e.Account, f.Account) &&
		containsFold(e.KeyID, f.KeyID) &&
		containsFold(e.Model, f.Model) &&
		containsFold(e.ClientIP, f.ClientIP) &&
		containsFold(e.UserAgent, f.UserAgent)
}

// matchTimeRange 闭区间时间过滤，From/To 任一侧零值即该侧不设界。
func (f *Filter) matchTimeRange(t time.Time) bool {
	if !f.From.IsZero() && t.Before(f.From) {
		return false
	}

	if !f.To.IsZero() && t.After(f.To) {
		return false
	}

	return true
}

// containsFold 大小写不敏感的子串匹配；needle 为空视为命中（不筛该字段）。
func containsFold(haystack, needle string) bool {
	if needle == "" {
		return true
	}

	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}

func (w *archiveWriter) stats() ArchiveStats {
	archiveStats := ArchiveStats{
		Enabled:       w.enabled(),
		Dir:           w.cfg.Dir,
		DroppedWrites: w.dropped.Load(),
		LastError:     w.errString(),
		Files:         0,
		Bytes:         0,
	}
	if !archiveStats.Enabled || w.out == nil {
		return archiveStats
	}

	fileCount, bytes, err := w.out.Stats()
	if err != nil {
		archiveStats.LastError = err.Error()
		return archiveStats
	}

	archiveStats.Files = fileCount
	archiveStats.Bytes = bytes

	return archiveStats
}

func (w *archiveWriter) close() {
	if !w.enabled() {
		return
	}

	w.closeOnce.Do(func() {
		close(w.stop)
		<-w.done
	})
}

func (w *archiveWriter) setErr(err error) {
	if err == nil {
		return
	}

	w.lastErrMu.Lock()
	w.lastErr = err.Error()
	w.lastErrMu.Unlock()
}

func (w *archiveWriter) errString() string {
	w.lastErrMu.Lock()
	defer w.lastErrMu.Unlock()

	return w.lastErr
}
