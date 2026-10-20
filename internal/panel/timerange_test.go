package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/reqlog"
	"github.com/BegoniaHe/wokbuddy-demon/internal/usage"
)

// wallHourStarts 返回 now 所在本地整点往回 count 个互不相同的墙钟小时桶起点
// （升序）。桶 scope 按本地墙钟整点归档，用绝对 Add 取相邻小时会在夏令时切换日
// 失真：回拨日的重复小时生成重复 scope，拨快日不存在的整点被归一成同一时刻。
func wallHourStarts(now time.Time, count int) []time.Time {
	base := time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), 0, 0, 0, now.Location())

	out := make([]time.Time, 0, count)
	seen := make(map[int64]struct{}, count)

	for k := 0; len(out) < count; k++ {
		t := time.Date(base.Year(), base.Month(), base.Day(), base.Hour()-k, 0, 0, 0, base.Location())
		if _, dup := seen[t.Unix()]; dup {
			continue
		}

		seen[t.Unix()] = struct{}{}
		out = append(out, t)
	}

	slices.Reverse(out)

	return out
}

// parseTimeParam 是「今天 / 自定义」区间的唯一入口：前端默认发 unix 秒，手工
// 调接口时可以用 RFC3339 / datetime-local；非法值与空串一律回落"不设界"，
// 不能让一个拼错的参数把整页用量打成 500。
func TestParseTimeParam(t *testing.T) {
	t.Parallel()

	want := time.Date(2026, 9, 30, 14, 5, 0, 0, time.Local)

	cases := []struct {
		name string
		in   string
		want time.Time
	}{
		{"空串", "", time.Time{}},
		{"空白", "   ", time.Time{}},
		{"unix 秒", "1786000000", time.Unix(1786000000, 0)},
		{"unix 毫秒", "1786000000000", time.UnixMilli(1786000000000)},
		{"零值按不设界", "0", time.Time{}},
		{"负数按不设界", "-5", time.Time{}},
		{"RFC3339", want.Format(time.RFC3339), want},
		{"本地 datetime-local", "2026-09-30T14:05", want},
		{"非法", "not-a-time", time.Time{}},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			t.Parallel()

			got := parseTimeParam(item.in)
			if !got.Equal(item.want) {
				t.Fatalf("parseTimeParam(%q) = %v, want %v", item.in, got, item.want)
			}
		})
	}
}

// usage 接口的区间参数必须真的作用到聚合上：from/to 生效时 hours 被忽略，
// 且响应回显 window_from/window_to 供面板确认口径。
func TestUsageHandlerAcceptsExplicitRange(t *testing.T) {
	t.Parallel()

	rec := usage.New("")

	// 桶 scope 按本地墙钟整点归档，相邻桶必须按墙钟小时构造：绝对 Add 在
	// 夏令时回拨/拨快日会生成重复或缺失的整点，显式区间取不中两个桶。
	hours := wallHourStarts(time.Now(), 6)
	for _, at := range hours {
		rec.Add(at, "cn", "u1", "glm-5.2",
			&usage.Delta{PromptTokens: 10, HasPromptTokens: true}, true)
	}

	panel := New(&Config{Version: "test", APIKey: "k", Usage: rec, Pool: pool.New("")})

	from := hours[2].Unix()
	to := hours[3].Unix()
	// 同时带上 hours=720：区间参数应优先，否则会被算成 30 天全量。
	got := usageGET(t, panel, "?hours=720&from="+strconv.FormatInt(from, 10)+"&to="+strconv.FormatInt(to, 10))
	if reqs := usageRequests(t, got); reqs != 2 {
		t.Fatalf("区间请求数 = %v, want 2（from/to 应优先于 hours）", reqs)
	}

	assertUsageRangeEcho(t, got, true)

	// 无区间参数时回落默认 72h，且不回显区间。
	def := usageGET(t, panel, "")
	assertUsageRangeEcho(t, def, false)

	if r := usageRequests(t, def); r != 6 {
		t.Fatalf("默认窗口请求数 = %v, want 6", r)
	}

	// hours=0（全部历史）同样不回显区间。
	all := usageGET(t, panel, "?hours=0")
	if r := usageRequests(t, all); r != 6 {
		t.Fatalf("全部历史请求数 = %v, want 6", r)
	}
}

// usageGET 带鉴权请求 usage 端点并解析 JSON 响应。
func usageGET(t *testing.T, panel *Panel, query string) map[string]any {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "/panel/api/usage"+query, nil)
	req.Header.Set("Authorization", "Bearer k")

	responseRecorder := httptest.NewRecorder()
	panel.ServeHTTP(responseRecorder, req)

	if responseRecorder.Code != 200 {
		t.Fatalf("usage%s -> %d %s", query, responseRecorder.Code, responseRecorder.Body)
	}

	var out map[string]any
	if err := json.Unmarshal(responseRecorder.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}

	return out
}

// usageRequests 取出响应 totals.requests。
func usageRequests(t *testing.T, got map[string]any) float64 {
	t.Helper()

	totals, ok := got["totals"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺 totals: %+v", got)
	}

	reqs, ok := totals["requests"].(float64)
	if !ok {
		t.Fatalf("totals.requests 类型异常: %+v", totals)
	}

	return reqs
}

// assertUsageRangeEcho 断言 window_from/window_to 是否随请求回显。
func assertUsageRangeEcho(t *testing.T, got map[string]any, want bool) {
	t.Helper()

	hasFrom := got["window_from"] != nil

	hasTo := got["window_to"] != nil
	if hasFrom != want || hasTo != want {
		t.Fatalf("window 回显 = from:%v to:%v, want %v: %+v", hasFrom, hasTo, want, got)
	}
}

// 归档为空时接口必须回 []，而不是 JSON null：前端把 null 与"归档关闭"混在一起
// 会走错分支，把不满足时间区间的最近请求显示出来。
func TestRequestLogsEmptyRangeReturnsArray(t *testing.T) {
	t.Parallel()
	rec := reqlog.New(reqlog.Config{Enabled: true, Dir: t.TempDir(), MaxBytes: 1 << 20, RetentionDays: 7})
	rec.Record(&reqlog.Event{
		Time: time.Now().Add(-48 * time.Hour), RequestID: "old",
		Status: 200, OK: true, Outcome: reqlog.OutcomeSuccess,
	})
	rec.Close()

	panel := New(&Config{Version: "test", APIKey: "k", Pool: pool.New(""), RequestLog: rec})
	future := strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
	req := httptest.NewRequest(http.MethodGet, "/panel/api/request_logs?from="+future, nil)
	req.Header.Set("Authorization", "Bearer k")

	responseRecorder := httptest.NewRecorder()
	panel.ServeHTTP(responseRecorder, req)

	if responseRecorder.Code != 200 {
		t.Fatalf("code=%d body=%s", responseRecorder.Code, responseRecorder.Body)
	}

	var out struct {
		Entries []reqlog.Event `json:"entries"`
	}
	if err := json.Unmarshal(responseRecorder.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}

	if out.Entries == nil {
		t.Fatalf("空区间应回 []，得到 null: %s", responseRecorder.Body)
	}

	if len(out.Entries) != 0 {
		t.Fatalf("未来区间不该命中任何记录: %+v", out.Entries)
	}
}
