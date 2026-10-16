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

// get-user-request-usage 路径（realm 双路径 fallback 同 get-user-resource 族）。
const (
	requestUsagePath   = "/billing/meter/get-user-request-usage"
	requestUsagePathV2 = "/v2/billing/meter/get-user-request-usage"
)

// requestUsagePaths 按 realm 返回路径候选（global 先无 /v2 前缀，404 时回落 /v2）。
func (c *Client) requestUsagePaths(a *auth.Auth) []string {
	if c.globalOn(a) {
		return []string{requestUsagePath, requestUsagePathV2}
	}
	return []string{requestUsagePathV2}
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

// RequestUsage 分页拉取账号 [start,end) 的逐笔用量。
//
// 上游请求契约（假定，issue #31）：POST billing/meter/get-user-request-usage，
// body {"startTime":unix秒,"endTime":unix秒,"page":n,"pageSize":m}；响应 data 含
// 列表 + total。解析对字段名宽容（parseRequestUsage），以适配上游命名的可能差异。
func (c *Client) RequestUsage(a *auth.Auth, start, end time.Time, page, pageSize int) (*RequestUsagePage, error) {
	if pageSize <= 0 {
		pageSize = 100
	}
	if page <= 0 {
		page = 1
	}
	body := map[string]any{
		"startTime": start.Unix(),
		"endTime":   end.Unix(),
		"page":      page,
		"pageSize":  pageSize,
	}
	data, err := c.billingMeterJSON(a, c.requestUsagePaths(a), body)
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
	out := &RequestUsagePage{Total: firstIntField(root, "total", "totalCount", "count", "totalNum")}
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
	if out.Total == 0 {
		out.Total = len(out.Records)
	}
	if out.Total > page*pageSize && len(out.Records) > 0 {
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
func parseUsageTime(v any) time.Time {
	switch t := v.(type) {
	case float64:
		return timeFromUnix(int64(t))
	case string:
		if n, err := strconv.ParseInt(t, 10, 64); err == nil {
			return timeFromUnix(n)
		}
		for _, layout := range []string{
			"2006-01-02 15:04:05", "2006-01-02T15:04:05Z07:00",
			"2006-01-02T15:04:05", "2006-01-02",
		} {
			if ts, err := time.ParseInLocation(layout, strings.TrimSpace(t), time.Local); err == nil {
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
