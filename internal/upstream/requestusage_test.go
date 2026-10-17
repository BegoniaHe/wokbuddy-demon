// requestusage_test.go 钉住上游逐笔用量查询的宽容解析与分页（issue #31）。
package upstream

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// TestRequestUsageRequestContract 钉住 Web 实测契约：无 /v2 主路径、UTC+8 墙钟字符串
// 时间、pageNum 分页字段。这三项任一漂移都会让回填整体 400/404。
func TestRequestUsageRequestContract(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	var decodeErr error
	c := testClient(func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		decodeErr = json.NewDecoder(r.Body).Decode(&gotBody)
		return jsonResp(200, `{"code":0,"data":{"total":1,"data":[
			{"requestId":"r1","model":"m","credit":0,"requestTime":"2026-10-16 22:40:00"}]}}`), nil
	})
	start := time.Date(2026, 10, 10, 0, 0, 0, 0, softRateResetLoc)
	end := time.Date(2026, 10, 17, 23, 59, 59, 0, softRateResetLoc)
	pg, err := c.RequestUsage(&auth.Auth{AccessToken: "at"}, start, end, 2, 1000)
	if err != nil {
		t.Fatalf("RequestUsage: %v", err)
	}
	if decodeErr != nil {
		t.Fatalf("decode body: %v", decodeErr)
	}
	if gotPath != requestUsagePath {
		t.Errorf("path=%q want %q", gotPath, requestUsagePath)
	}
	wantBody := map[string]any{
		"startTime": "2026-10-10 00:00:00",
		"endTime":   "2026-10-17 23:59:59",
		"pageNum":   float64(2),
		"pageSize":  float64(1000),
	}
	if !reflect.DeepEqual(gotBody, wantBody) {
		t.Errorf("body=%#v want %#v", gotBody, wantBody)
	}
	if pg.Total != 1 || len(pg.Records) != 1 || pg.Records[0].Model != "m" {
		t.Errorf("page=%+v want total=1 records=1 model=m", pg)
	}
}

func TestRequestUsageParseAndPaginate(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"total":250,"list":[
			{"requestId":"r1","model":"glm-5.2","credit":1.5,"requestTime":1753600000},
			{"requestId":"r2","modelName":"claude-4.6","cost":"2.5","time":"2026-07-27 10:00:00"},
			{"id":"r3","model":"glm-5.2","credit":0,"requestTimeMs":1753600000000}
		]}}`), nil
	})
	pg, err := c.RequestUsage(&auth.Auth{AccessToken: "at"}, time.Now().Add(-24*time.Hour), time.Now(), 1, 100)
	if err != nil {
		t.Fatalf("RequestUsage: %v", err)
	}
	if pg.Total != 250 {
		t.Errorf("total=%d want 250", pg.Total)
	}
	if pg.NextPage != 2 {
		t.Errorf("nextPage=%d want 2", pg.NextPage)
	}
	if len(pg.Records) != 3 {
		t.Fatalf("records=%d want 3", len(pg.Records))
	}
	checkUsageRecords(t, pg.Records)
}

// usageRecWant 期望的单笔记录（unix=0 表示不校验时间）。
type usageRecWant struct {
	id, model string
	credit    float64
	unix      int64
}

func checkUsageRecords(t *testing.T, got []RequestUsageRecord) {
	t.Helper()
	want := []usageRecWant{
		{"r1", "glm-5.2", 1.5, 1753600000},
		{"r2", "claude-4.6", 2.5, 0}, // 字符串时间：不校验 unix
		{"r3", "glm-5.2", 0, 1753600000},
	}
	for i, w := range want {
		if got[i].RequestID != w.id || got[i].Model != w.model || got[i].Credit != w.credit {
			t.Errorf("rec%d=%+v want id=%s model=%s credit=%v", i, got[i], w.id, w.model, w.credit)
		}
		if w.unix != 0 && got[i].Time.Unix() != w.unix {
			t.Errorf("rec%d time=%v want unix %d", i, got[i].Time, w.unix)
		}
	}
}

func TestRequestUsageNoNextPage(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"total":1,"list":[{"requestId":"r1","model":"m","credit":0}]}}`), nil
	})
	pg, err := c.RequestUsage(&auth.Auth{AccessToken: "at"}, time.Now().Add(-time.Hour), time.Now(), 1, 100)
	if err != nil {
		t.Fatal(err)
	}
	if pg.NextPage != 0 {
		t.Errorf("nextPage=%d want 0", pg.NextPage)
	}
}

func TestParseUsageTimeFormats(t *testing.T) {
	cases := []struct {
		in   any
		want int64
	}{
		{float64(1753600000), 1753600000},
		{"1753600000", 1753600000},
		{float64(1753600000000), 1753600000},
	}
	for _, c := range cases {
		if got := parseUsageTime(c.in).Unix(); got != c.want {
			t.Errorf("parseUsageTime(%v)=%d want %d", c.in, got, c.want)
		}
	}
}

// TestParseUsageTimeWallClockUTC8 上游无时区 requestTime 必须按 UTC+8 解释，不能
// 跟部署机器的 time.Local 走，否则非 UTC+8 环境会把记录分到错误日期。
func TestParseUsageTimeWallClockUTC8(t *testing.T) {
	want := time.Date(2026, 10, 16, 22, 40, 0, 0, softRateResetLoc)
	if got := parseUsageTime("2026-10-16 22:40:00"); !got.Equal(want) {
		t.Errorf("parseUsageTime(墙钟)=%v want %v", got, want)
	}
	withTZ := time.Date(2026, 10, 16, 22, 40, 0, 0, time.FixedZone("offset", 2*60*60))
	if got := parseUsageTime("2026-10-16T22:40:00+02:00"); !got.Equal(withTZ) {
		t.Errorf("parseUsageTime(RFC3339)=%v want %v", got, withTZ)
	}
}

// TestRequestUsageMissingTotalStillPaginates 上游漏给 total 时，整页满仍需继续翻页，
// 避免回填静默截断。
// TestDoJSONLimitRejectsOversize 响应超限时必须报明确错误，不能把半截 body 交给
// json.Unmarshal 变成 "unexpected end of JSON input"。
func TestDoJSONLimitRejectsOversize(t *testing.T) {
	c := testClient(func(_ *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{}}`), nil
	})
	req, err := http.NewRequest(http.MethodPost, "https://billing.example/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.doJSONLimit(req, 8); err == nil || !strings.Contains(err.Error(), "response body exceeds") {
		t.Fatalf("err=%v want body-limit error", err)
	}
}

func TestRequestUsageMissingTotalStillPaginates(t *testing.T) {
	var pages []int
	c := testClient(func(r *http.Request) (*http.Response, error) {
		var body struct {
			PageNum int `json:"pageNum"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			return nil, err
		}
		pages = append(pages, body.PageNum)
		return jsonResp(200, `{"code":0,"data":{"data":[{"requestId":"r","model":"m"}]}}`), nil
	})
	pg, err := c.RequestUsage(&auth.Auth{AccessToken: "at"}, time.Now().Add(-time.Hour), time.Now(), 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if pg.Total != 1 {
		t.Errorf("total=%d want fallback 1", pg.Total)
	}
	if pg.NextPage != 2 {
		t.Errorf("nextPage=%d want 2 when total missing and page is full", pg.NextPage)
	}
	if !reflect.DeepEqual(pages, []int{1}) {
		t.Errorf("pages=%v want [1]", pages)
	}
}
