package server

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// durationBuckets 请求耗时 histogram 的桶上界（秒，严格升序）。Prometheus 抓取端
// 由 le 标签重建分布；+Inf 桶在渲染时补，不在此列。
var durationBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60}

// 耗时 histogram 的 result 维度取值。成功/失败分开观测，避免 429 / 传输错误等
// 毫秒级失败样本压低整体 P50/P90。
const (
	resultOK    = "ok"
	resultError = "error"
)

// durationResults 是 result 维度的固定枚举，渲染时按此顺序输出保证稳定。
var durationResults = []string{resultOK, resultError}

// durationHist 单一 result 维度下的累积式耗时直方图。
type durationHist struct {
	// buckets[i] 是 le<=durationBuckets[i] 的累计观测数（累积式，observe 时同时
	// 递增所有满足上界的桶）。
	buckets []uint64
	sum     float64
	count   uint64
}

// metricsRegistry 进程内生命周期指标，只持有仓库其它包不提供的两类：请求耗时
// histogram 与上游轮换计数。账号 / 请求 / Token 三类直接读现有计数
// （pool.CountsDetailedWithPaused / usage.AggregateByModel），不在此重复记账
// （issue #13「不重复记账」）。进程重启清零，符合 Prometheus counter 语义。
type metricsRegistry struct {
	mu sync.Mutex

	// dur 按 result（ok/error）分流的耗时 histogram。
	dur map[string]*durationHist

	rotations map[string]uint64
}

func newMetricsRegistry() *metricsRegistry {
	return &metricsRegistry{
		dur:       map[string]*durationHist{},
		rotations: map[string]uint64{},
	}
}

// observeRequestDuration 记录一次上游尝试耗时（秒），按成功/失败分流。nil 接收者
// 安全（防御裸用）。
func (m *metricsRegistry) observeRequestDuration(seconds float64, ok bool) {
	if m == nil {
		return
	}
	result := resultError
	if ok {
		result = resultOK
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	hist := m.dur[result]
	if hist == nil {
		hist = &durationHist{buckets: make([]uint64, len(durationBuckets))}
		m.dur[result] = hist
	}
	for i, ub := range durationBuckets {
		if seconds <= ub {
			hist.buckets[i]++
		}
	}
	hist.sum += seconds
	hist.count++
}

// incRotation 记录一次换号；reason 为触发原因标签（调用方保证非空）。
func (m *metricsRegistry) incRotation(reason string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.rotations[reason]++
	m.mu.Unlock()
}

// metricsHandler 渲染 Prometheus 文本格式指标。鉴权与主服务同口径（withAuth 包裹）。
func (h *Handler) metricsHandler(w http.ResponseWriter, _ *http.Request) {
	var b strings.Builder
	h.writeAccountMetrics(&b)
	h.writeModelMetrics(&b)
	h.writeDurationMetrics(&b)
	h.writeRotationMetrics(&b)

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = io.WriteString(w, b.String())
}

// writeAccountMetrics 账号分状态计数（复用 pool.CountsDetailedWithPaused）。
// paused 与 disabled 分开：暂停只退出选号、照常保号，与禁用是两种运维状态
// （见 pool.CountsDetailedWithPaused 注释），合并会丢信号。
//
// state 是互斥分区，sum(wokbuddy_accounts) 恒等于账号总数。CountsDetailedWithPaused
// 的 healthy 含 in_flight_full（在途满载仍是状态机意义上的健康），故此处 ok 取
// healthy-in_flight_full，避免满载号被 ok 与 in_flight_full 重复计。
func (h *Handler) writeAccountMetrics(b *strings.Builder) {
	_, healthy, cooling, disabled, paused, inFlightFull := h.cfg.Pool.CountsDetailedWithPaused()
	writeHelpType(b, "wokbuddy_accounts", "Number of accounts by state.", "gauge")
	for _, s := range []struct {
		state string
		value int
	}{
		{"ok", healthy - inFlightFull},
		{"cooling", cooling},
		{"disabled", disabled},
		{"paused", paused},
		{"in_flight_full", inFlightFull},
	} {
		writeSample(b, "wokbuddy_accounts", label("state", s.state), float64(s.value))
	}
}

// writeModelMetrics 按模型导出请求数与 Token（复用 usage 全历史聚合）。
// 口径为「逐上游尝试」（含失败尝试），与 recordAttempt 同源，不另起账本。
func (h *Handler) writeModelMetrics(b *strings.Builder) {
	if h.cfg.Usage == nil {
		return
	}
	rows := h.cfg.Usage.AggregateByModel()
	// 每族样本连续输出（HELP/TYPE 紧邻其样本），符合 exposition 惯例。
	writeHelpType(b, "wokbuddy_requests_total", "Total upstream request attempts by model and result.", "counter")
	for i := range rows {
		row := &rows[i]
		ok := row.Requests - row.Errors
		if ok < 0 {
			ok = 0
		}
		writeSample(b, "wokbuddy_requests_total", labelPair("model", row.Key, "result", "ok"), float64(ok))
		writeSample(b, "wokbuddy_requests_total", labelPair("model", row.Key, "result", "error"), float64(row.Errors))
	}
	writeHelpType(b, "wokbuddy_tokens_total", "Total tokens by model and kind.", "counter")
	for i := range rows {
		row := &rows[i]
		writeSample(b, "wokbuddy_tokens_total", labelPair("model", row.Key, "kind", "prompt"), float64(row.PromptTokens))
		writeSample(b, "wokbuddy_tokens_total", labelPair("model", row.Key, "kind", "completion"), float64(row.CompletionTok))
		writeSample(b, "wokbuddy_tokens_total", labelPair("model", row.Key, "kind", "total"), float64(row.TotalTokens))
	}
}

// writeDurationMetrics 渲染按 result 分流的耗时 histogram（进程内累计）。
func (h *Handler) writeDurationMetrics(b *strings.Builder) {
	writeHelpType(b, "wokbuddy_request_duration_seconds", "Upstream attempt duration in seconds by result.", "histogram")
	// 先快照再渲染：ok/error 恒输出（无观测即全零），保证序列集稳定。
	snapshot := make(map[string]durationHist, len(durationResults))
	for _, result := range durationResults {
		snapshot[result] = durationHist{buckets: make([]uint64, len(durationBuckets))}
	}
	if m := h.metrics; m != nil {
		m.mu.Lock()
		for result, hist := range m.dur {
			snapshot[result] = durationHist{
				buckets: append([]uint64(nil), hist.buckets...),
				sum:     hist.sum,
				count:   hist.count,
			}
		}
		m.mu.Unlock()
	}
	for _, result := range durationResults {
		hist := snapshot[result]
		for i, ub := range durationBuckets {
			writeSample(b, "wokbuddy_request_duration_seconds_bucket", labelPair("result", result, "le", formatFloat(ub)), float64(hist.buckets[i]))
		}
		writeSample(b, "wokbuddy_request_duration_seconds_bucket", labelPair("result", result, "le", "+Inf"), float64(hist.count))
		writeSample(b, "wokbuddy_request_duration_seconds_sum", label("result", result), hist.sum)
		writeSample(b, "wokbuddy_request_duration_seconds_count", label("result", result), float64(hist.count))
	}
}

// writeRotationMetrics 渲染上游换号计数（进程内累计，按原因标签）。
func (h *Handler) writeRotationMetrics(b *strings.Builder) {
	m := h.metrics
	if m == nil {
		return
	}
	writeHelpType(b, "wokbuddy_upstream_rotations_total", "Total upstream account rotations by reason.", "counter")
	m.mu.Lock()
	reasons := make([]string, 0, len(m.rotations))
	for reason := range m.rotations {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	counts := make(map[string]uint64, len(m.rotations))
	for reason, n := range m.rotations {
		counts[reason] = n
	}
	m.mu.Unlock()
	for _, reason := range reasons {
		writeSample(b, "wokbuddy_upstream_rotations_total", label("reason", reason), float64(counts[reason]))
	}
}

// writeHelpType 输出一族的 # HELP 与 # TYPE 头。
func writeHelpType(b *strings.Builder, name, help, typ string) {
	fmt.Fprintf(b, "# HELP %s %s\n", name, help)
	fmt.Fprintf(b, "# TYPE %s %s\n", name, typ)
}

// writeSample 输出一行样本：name{labels} value。labels 为 "" 时不带花括号。
func writeSample(b *strings.Builder, name, labels string, value float64) {
	b.WriteString(name)
	b.WriteString(labels)
	b.WriteByte(' ')
	b.WriteString(formatFloat(value))
	b.WriteByte('\n')
}

// formatFloat 用最短往返表示格式化数值（整数不带小数点，Prometheus 可解析）。
func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// label 渲染单个标签 {k="v"}。
func label(k, v string) string { return labelPair(k, v) }

// labelPair 渲染 "k1=\"v1\",k2=\"v2\"..."，值按 Prometheus 文本格式转义。
func labelPair(kv ...string) string {
	var sb strings.Builder
	sb.WriteByte('{')
	for i := 0; i+1 < len(kv); i += 2 {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(kv[i])
		sb.WriteString(`="`)
		sb.WriteString(escapeLabelValue(kv[i+1]))
		sb.WriteByte('"')
	}
	sb.WriteByte('}')
	return sb.String()
}

// escapeLabelValue 转义标签值中的反斜杠、双引号与换行（Prometheus 文本格式）。
func escapeLabelValue(s string) string {
	if !strings.ContainsAny(s, "\\\"\n") {
		return s
	}
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}
