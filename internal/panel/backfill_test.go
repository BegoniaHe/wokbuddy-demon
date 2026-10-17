// backfill_test.go 钉住上游用量回填端点（issue #31）：拉取逐笔用量、按天覆盖本地桶。
package panel

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
	"github.com/BegoniaHe/wokbuddy-demon/internal/usage"
)

func TestUsageBackfillEndpoint(t *testing.T) {
	nowUnix := strconv.FormatInt(time.Now().Unix(), 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/billing/meter/get-user-request-usage" {
			w.Write([]byte(`{"code":0,"data":{"total":2,"data":[` +
				`{"requestId":"r1","model":"glm-5.2","credit":1.5,"requestTime":` + nowUnix + `},` +
				`{"requestId":"r2","model":"glm-5.2","credit":0.5,"requestTime":` + nowUnix + `}]}}`))
			return
		}
		w.Write([]byte(`{"code":0,"data":{}}`))
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	rec := usage.New("")
	up := &upstream.Client{HTTP: srv.Client(), BillingBaseCN: srv.URL, ChatBaseCN: srv.URL}
	pn := &Panel{cfg: Config{Pool: p, Upstream: up, Usage: rec}}

	body, _ := json.Marshal(map[string]any{"uid": "u1", "days": 7})
	req := httptest.NewRequest(http.MethodPost, "/panel/api/usage/backfill", bytes.NewReader(body))
	w := httptest.NewRecorder()
	pn.usageBackfill(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body)
	}
	var res struct {
		Records  int `json:"records"`
		Buckets  int `json:"buckets"`
		Accounts int `json:"accounts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v body=%s", err, w.Body)
	}
	if res.Records != 2 || res.Buckets != 1 || res.Accounts != 1 {
		t.Fatalf("res=%+v want records=2 buckets=1 accounts=1", res)
	}
	s := rec.Snapshot(0, nil)
	if s.Totals.Requests != 2 || s.Totals.Credits != 2.0 {
		t.Fatalf("backfilled totals=%+v want requests=2 credits=2", s.Totals)
	}
}

// TestAddBackfillRecordDedup 同 requestId 跨页重复时只计一次，避免分页重叠放大用量。
func TestAddBackfillRecordDedup(t *testing.T) {
	rows := map[string]*usage.BackfillRow{}
	seen := map[string]struct{}{}
	rec := upstream.RequestUsageRecord{
		RequestID: "r1",
		Model:     "glm-5.2",
		Credit:    1.5,
		Time:      time.Now(),
	}
	if !addBackfillRecord(rows, seen, "cn", "u1", "2026-10-17", rec) {
		t.Fatal("first record should be accepted")
	}
	if addBackfillRecord(rows, seen, "cn", "u1", "2026-10-17", rec) {
		t.Fatal("duplicate request id should be skipped")
	}
	if len(rows) != 1 {
		t.Fatalf("rows=%d want 1", len(rows))
	}
	for _, row := range rows {
		if row.Requests != 1 || row.Credits != 1.5 {
			t.Fatalf("row=%+v want requests=1 credits=1.5", row)
		}
	}
}
