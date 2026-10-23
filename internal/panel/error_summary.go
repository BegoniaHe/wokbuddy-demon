// error_summary.go 运行日志错误聚合（issue #258）。
//
// 只读、不新增存储：基于 log Ring 的文本做账号 × 错误类型聚合，输出计数、
// 最近时间与截断样例。解析函数独立可测；日志格式变化时只改这一类。
package panel

import (
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/logfmt"
)

// ErrorSummaryRow 一条聚合结果。
type ErrorSummaryRow struct {
	Account string    `json:"account"`
	Type    string    `json:"type"`
	Count   int       `json:"count"`
	LastAt  time.Time `json:"last_at"`
	Sample  string    `json:"sample"`
}

// errorAccountRe 账号标签：acct=/uid= 形态（chat 表格行、panel 动作行）。
var errorAccountRe = regexp.MustCompile(`(?i)(?:acct|uid)=([^\s:]+)`)

// taskAccountRe 任务行前缀：`checkin 昵称(uid8): ...`。
var taskAccountRe = regexp.MustCompile(`^(checkin|travel|activity|keepalive|blackcat|school|streak-bonus|balance|lottery)\s+([^\s:]+):`)

// errorSummaryHandler GET /panel/api/error_summary。
func (p *Panel) errorSummaryHandler(w http.ResponseWriter, r *http.Request) {
	rows := SummarizeErrors(p.logs.Snapshot())

	writeJSON(w, http.StatusOK, map[string]any{
		"generated_at": time.Now(),
		"entries":      rows,
	})
}

// SummarizeErrors 聚合错误行：账号 × 类型。空账号归 "-"。
func SummarizeErrors(entries []LogEntry) []ErrorSummaryRow {
	type key struct{ account, kind string }

	agg := make(map[key]*ErrorSummaryRow)

	for _, entry := range entries {
		text := strings.TrimSpace(entry.Text)
		if !isErrorLogLine(text) {
			continue
		}

		kind := classifyErrorText(text)
		if kind == "" {
			continue
		}

		account := extractErrorAccount(text)
		k := key{account: account, kind: kind}

		row := agg[k]
		if row == nil {
			row = &ErrorSummaryRow{Account: account, Type: kind}
			agg[k] = row
		}

		row.Count++
		if entry.TS.After(row.LastAt) {
			row.LastAt = entry.TS
		}

		row.Sample = logfmt.Truncate(text, 180)
	}

	out := make([]ErrorSummaryRow, 0, len(agg))
	for _, row := range agg {
		out = append(out, *row)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}

		if out[i].Account != out[j].Account {
			return out[i].Account < out[j].Account
		}

		return out[i].Type < out[j].Type
	})

	return out
}

// isErrorLogLine 只聚合失败/警告行，正常流转不计。
func isErrorLogLine(text string) bool {
	lower := strings.ToLower(text)

	return strings.Contains(text, "WARN:") || strings.Contains(text, "ERR:") ||
		strings.Contains(text, "失败") || strings.Contains(text, "错误") ||
		strings.Contains(lower, "error") || strings.Contains(text, "超时")
}

// classifyErrorText 把日志文本归到稳定错误类型（顺序即优先级）。
func classifyErrorText(text string) string {
	lower := strings.ToLower(text)

	switch {
	case strings.Contains(text, "session dead"), strings.Contains(text, "12153"), strings.Contains(text, "会话失效"):
		return "session_dead"
	case strings.Contains(text, "14003"), strings.Contains(lower, "model busy"), strings.Contains(text, "模型繁忙"):
		return "model_busy"
	case strings.Contains(lower, "hard_credit"),
		strings.Contains(text, "余额不足"),
		strings.Contains(text, "额度不足"),
		strings.Contains(lower, "402"):
		return "hard_credit"
	case strings.Contains(lower, "rate limit"), strings.Contains(lower, "429"), strings.Contains(text, "限流"), strings.Contains(text, "冷却"):
		return "rate_limit"
	case strings.Contains(lower, "timeout"), strings.Contains(lower, "timed out"), strings.Contains(text, "超时"):
		return "timeout"
	case strings.Contains(lower, "truncated"),
		strings.Contains(lower, "empty upstream"),
		strings.Contains(lower, "upstream_parse"),
		strings.Contains(lower, "continuation failed"),
		strings.Contains(text, "续写"):
		return "stream_error"
	case strings.Contains(lower, "error frame"), strings.Contains(lower, "upstream error"):
		return "upstream_error"
	case strings.Contains(text, "失败"), strings.Contains(lower, "error"), strings.Contains(lower, "err:"):
		return "failure"
	default:
		return ""
	}
}

// extractErrorAccount 从日志行提取账号标签；提取不到归 "-"。
func extractErrorAccount(text string) string {
	if m := errorAccountRe.FindStringSubmatch(text); len(m) == 2 {
		return cleanAccountLabel(m[1])
	}

	if m := taskAccountRe.FindStringSubmatch(text); len(m) == 3 {
		return cleanAccountLabel(m[2])
	}

	return "-"
}

// cleanAccountLabel 去掉昵称括号，保留 uid8：`示例昵称(a1b2c3d4)` → `a1b2c3d4`。
func cleanAccountLabel(label string) string {
	if i := strings.LastIndexByte(label, '('); i >= 0 && strings.HasSuffix(label, ")") {
		return label[i+1 : len(label)-1]
	}

	return label
}
