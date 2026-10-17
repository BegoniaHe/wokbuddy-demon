// requestusage.go 上游逐笔用量查询（issue #31）：/billing/meter/get-user-request-usage
// 分页拉取某账号在时间范围内的请求用量，供面板显式触发对账/回填。
package upstream

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// get-user-request-usage 路径。实测 CN/global 的 Web 用量页均走无 /v2 的主路径；
// /v2 当前两域都返回 404 Route Not Found，仅保留为路径迁移时的 404 兜底。
const (
	requestUsagePath   = "/billing/meter/get-user-request-usage"
	requestUsagePathV2 = "/v2/billing/meter/get-user-request-usage"
)

// requestUsageTimeLayout 上游逐笔用量接口的墙钟时间格式（UTC+8，无时区后缀）。
const requestUsageTimeLayout = "2006-01-02 15:04:05"

// requestUsageMaxRespBytes 逐笔用量单页响应上限。响应包含完整 prompt（input），
// 实测 1000 条约 0.7~1.3 MiB；取 8 MiB 留足余量，同时避免无界缓冲。
const requestUsageMaxRespBytes int64 = 8 << 20

// requestUsagePaths 返回路径候选：先主路径，404 时再试 /v2（见上方注释）。
func (c *Client) requestUsagePaths() []string {
	return []string{requestUsagePath, requestUsagePathV2}
}

// RequestUsageRecord 单笔上游用量。
type RequestUsageRecord struct {
	RequestID string
	Time      time.Time
	Model     string
	Credit    float64
}

// RequestUsagePage 一页查询结果。
type RequestUsagePage struct {
	Records []RequestUsageRecord
	Total   int
	// NextPage 下一页页码（0 = 无下一页）。
	NextPage int
}

// RequestUsage 分页拉取账号 [start,end] 的逐笔用量。
//
// 上游请求契约（实测 Web /profile/usage）：
//
//	POST billing/meter/get-user-request-usage
//	{"startTime":"YYYY-MM-DD HH:mm:ss","endTime":"YYYY-MM-DD HH:mm:ss",
//	 "pageNum":n,"pageSize":m}
//
// 时间是 UTC+8 墙钟字符串（不是 Unix 秒），分页字段是 pageNum（不是 page）；响应
// data 含 data 列表 + total。解析对字段名宽容（parseRequestUsage），以适配上游命名
// 的可能差异。
func (c *Client) RequestUsage(a *auth.Auth, start, end time.Time, page, pageSize int) (*RequestUsagePage, error) {
	if pageSize <= 0 {
		pageSize = 100
	}
	if page <= 0 {
		page = 1
	}
	if start.IsZero() || end.IsZero() || !end.After(start) {
		return nil, fmt.Errorf("request usage: invalid time range %v..%v", start, end)
	}
	body := map[string]any{
		"startTime": start.In(softRateResetLoc).Format(requestUsageTimeLayout),
		"endTime":   end.In(softRateResetLoc).Format(requestUsageTimeLayout),
		"pageNum":   page,
		"pageSize":  pageSize,
	}
	data, err := c.billingMeterJSONLimit(a, c.requestUsagePaths(), body, requestUsageMaxRespBytes)
	if err != nil {
		return nil, err
	}
	return parseRequestUsage(data, page, pageSize)
}

// parseRequestUsage 宽容解析上游用量响应：列表字段名在常见命名中取首个命中，
// 时间兼容 unix 秒/毫秒与常见字符串格式；缺失字段跳过而非报错。
func parseRequestUsage(data json.RawMessage, page, pageSize int) (*RequestUsagePage, error) {
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("parse request usage: %w", err)
	}
	list := firstListField(root, "list", "records", "items", "rows", "data")
	reportedTotal := firstIntField(root, "total", "totalCount", "count", "totalNum")
	out := &RequestUsagePage{Total: reportedTotal}
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		out.Records = append(out.Records, RequestUsageRecord{
			RequestID: firstStringField(m, "requestId", "requestID", "id"),
			Model:     firstStringField(m, "model", "modelName", "requestModel", "requestModelName"),
			Credit:    firstFloatField(m, "credit", "credits", "cost", "usedCredit"),
			Time:      parseUsageTime(firstAny(m, "requestTime", "time", "createTime", "timestamp", "requestTimeMs")),
		})
	}
	if out.Total <= 0 {
		out.Total = len(out.Records)
	}
	// 正常路径按 total 判断；上游漏给 total 时，整页满即保守认为还有下一页，
	// 具体页数由调用方上限兜底，避免 silently 截断。
	if len(out.Records) > 0 &&
		(out.Total > page*pageSize || (reportedTotal <= 0 && len(out.Records) >= pageSize)) {
		out.NextPage = page + 1
	}
	return out, nil
}

// firstListField 返回首个命中键的数组值。
func firstListField(m map[string]any, keys ...string) []any {
	for _, k := range keys {
		if v, ok := m[k].([]any); ok {
			return v
		}
	}
	return nil
}

// firstAny 返回首个命中键的原始值。
func firstAny(m map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			return v
		}
	}
	return nil
}

// firstStringField 返回首个命中键的字符串值。
func firstStringField(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// firstIntField 返回首个命中键的整数值（兼容 json 数字与字符串）。
func firstIntField(m map[string]any, keys ...string) int {
	for _, k := range keys {
		switch v := m[k].(type) {
		case float64:
			return int(v)
		case string:
			if n, err := strconv.Atoi(v); err == nil {
				return n
			}
		}
	}
	return 0
}

// firstFloatField 返回首个命中键的浮点值。
func firstFloatField(m map[string]any, keys ...string) float64 {
	for _, k := range keys {
		switch v := m[k].(type) {
		case float64:
			return v
		case string:
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				return f
			}
		}
	}
	return 0
}

// parseUsageTime 宽容解析时间：unix 秒/毫秒（数字或数字串）或常见字符串格式。
//
// 上游 requestTime 是 UTC+8 墙钟（实测 "2026-10-16 22:40:00"），无时区后缀的格式
// 必须按 softRateResetLoc 解释；用 time.Local 会在非 UTC+8 部署上把日期算错。
// 带时区的 RFC3339 仍按自身 offset 解析。
func parseUsageTime(v any) time.Time {
	switch t := v.(type) {
	case float64:
		return timeFromUnix(int64(t))
	case string:
		s := strings.TrimSpace(t)
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return timeFromUnix(n)
		}
		if ts, err := time.Parse(time.RFC3339, s); err == nil {
			return ts
		}
		for _, layout := range []string{
			requestUsageTimeLayout, "2006-01-02T15:04:05", "2006-01-02",
		} {
			if ts, err := time.ParseInLocation(layout, s, softRateResetLoc); err == nil {
				return ts
			}
		}
	}
	return time.Time{}
}

// timeFromUnix 把秒/毫秒时间戳归一为 time.Time（>=1e12 视为毫秒）。
func timeFromUnix(n int64) time.Time {
	if n <= 0 {
		return time.Time{}
	}
	if n >= 1_000_000_000_000 {
		return time.UnixMilli(n)
	}
	return time.Unix(n, 0)
}
