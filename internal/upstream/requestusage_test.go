// requestusage_test.go 钉住上游逐笔用量查询的宽容解析与分页（issue #31）。
package upstream

import (
	"net/http"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

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
