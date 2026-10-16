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

// metricsRegistry 进程内生命周期指标，只持有仓库其它包不提供的两类：请求耗时
// histogram 与上游轮换计数。账号 / 请求 / Token 三类直接读现有计数
// （pool.CountsDetailedWithPaused / usage.AggregateByModel），不在此重复记账
// （issue #13「不重复记账」）。进程重启清零，符合 Prometheus counter 语义。
type metricsRegistry struct {
	mu sync.Mutex

	// 耗时 histogram：durBuckets[i] 是 le<=durationBuckets[i] 的累计观测数
	// （累积式，observe 时同时递增所有满足上界的桶）。
	durBuckets []uint64
	durSum     float64
	durCount   uint64

	rotations map[string]uint64
}

func newMetricsRegistry() *metricsRegistry {
	return &metricsRegistry{
		durBuckets: make([]uint64, len(durationBuckets)),
		rotations:  map[string]uint64{},
	}
}

// observeRequestDuration 记录一次上游尝试耗时（秒）。nil 接收者安全（防御裸用）。
func (m *metricsRegistry) observeRequestDuration(seconds float64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, ub := range durationBuckets {
		if seconds <= ub {
			m.durBuckets[i]++
		}
	}
	m.durSum += seconds
	m.durCount++
}

// incRotation 记录一次换号；reason 为触发原因标签，空值回落 "other"。
func (m *metricsRegistry) incRotation(reason string) {
	if m == nil {
		return
	}
	if reason == "" {
		reason = "other"
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
// （见 pool.CountsDetailedWithPaused 注释），合并会丢信号。healthy 含 in_flight_full。
func (h *Handler) writeAccountMetrics(b *strings.Builder) {
	_, healthy, cooling, disabled, paused, inFlightFull := h.cfg.Pool.CountsDetailedWithPaused()
	writeHelpType(b, "wokbuddy_accounts", "Number of accounts by state.", "gauge")
	for _, s := range []struct {
		state string
		value int
	}{
		{"ok", healthy},
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
	writeHelpType(b, "wokbuddy_requests_total", "Total upstream request attempts by model and result.", "counter")
	writeHelpType(b, "wokbuddy_tokens_total", "Total tokens by model and kind.", "counter")
	for i := range rows {
		row := &rows[i]
		ok := row.Requests - row.Errors
		if ok < 0 {
			ok = 0
		}
		writeSample(b, "wokbuddy_requests_total", labelPair("model", row.Key, "result", "ok"), float64(ok))
		writeSample(b, "wokbuddy_requests_total", labelPair("model", row.Key, "result", "error"), float64(row.Errors))
		writeSample(b, "wokbuddy_tokens_total", labelPair("model", row.Key, "kind", "prompt"), float64(row.PromptTokens))
		writeSample(b, "wokbuddy_tokens_total", labelPair("model", row.Key, "kind", "completion"), float64(row.CompletionTok))
		writeSample(b, "wokbuddy_tokens_total", labelPair("model", row.Key, "kind", "total"), float64(row.TotalTokens))
	}
}

// writeDurationMetrics 渲染耗时 histogram（进程内累计）。
func (h *Handler) writeDurationMetrics(b *strings.Builder) {
	m := h.metrics
	writeHelpType(b, "wokbuddy_request_duration_seconds", "Upstream attempt duration in seconds.", "histogram")
	if m == nil {
		writeSample(b, "wokbuddy_request_duration_seconds_bucket", label("le", "+Inf"), 0)
		writeSample(b, "wokbuddy_request_duration_seconds_sum", "", 0)
		writeSample(b, "wokbuddy_request_duration_seconds_count", "", 0)
		return
	}
	m.mu.Lock()
	buckets := append([]uint64(nil), m.durBuckets...)
	sum, count := m.durSum, m.durCount
	m.mu.Unlock()
	for i, ub := range durationBuckets {
		writeSample(b, "wokbuddy_request_duration_seconds_bucket", label("le", formatFloat(ub)), float64(buckets[i]))
	}
	writeSample(b, "wokbuddy_request_duration_seconds_bucket", label("le", "+Inf"), float64(count))
	writeSample(b, "wokbuddy_request_duration_seconds_sum", "", sum)
	writeSample(b, "wokbuddy_request_duration_seconds_count", "", float64(count))
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
