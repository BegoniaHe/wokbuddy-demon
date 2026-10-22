package scheduler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// compensationFixture 假上游：status/claim 两个端点各自计数与可配返回。
type compensationFixture struct {
	statusCalls atomic.Int32
	claimCalls  atomic.Int32
	// statusBody 返回的状态信封（可含 error 形态由 httpStatus 决定）。
	httpStatus int
	statusBody string
	// statusBodyAfter 非空时，第 2 次及以后的 status 查询使用它（模拟领取后状态变化）。
	statusBodyAfter string
	claimBody       string
}

func (f *compensationFixture) scheduler(t *testing.T, accounts ...*auth.Auth) *Scheduler {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.URL.Path == "/v2/billing/meter/compensation-status" || r.URL.Path == "/billing/meter/compensation-status" {
			call := f.statusCalls.Add(1)
			body := f.statusBody
			if call > 1 && f.statusBodyAfter != "" {
				body = f.statusBodyAfter
			}

			w.WriteHeader(f.httpStatus)
			_, _ = w.Write([]byte(body))

			return
		}

		if r.URL.Path == "/v2/billing/meter/claim-compensation" || r.URL.Path == "/billing/meter/claim-compensation" {
			f.claimCalls.Add(1)
			_, _ = w.Write([]byte(f.claimBody))

			return
		}

		_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
	}))
	t.Cleanup(srv.Close)

	state := pool.New("")
	for _, account := range accounts {
		state.Add(account)
	}

	client := &upstream.Client{HTTP: srv.Client(), BillingBaseCN: srv.URL, ChatBaseCN: srv.URL}

	return New(&Config{Pool: state, Upstream: client})
}

// TestCompensationStatusAllKeepsPoolOrder 并发查询按池顺序写回（行序稳定）。
func TestCompensationStatusAllKeepsPoolOrder(t *testing.T) {
	t.Parallel()

	fixture := &compensationFixture{
		httpStatus: http.StatusOK,
		statusBody: `{"code":0,"data":{"active":true,"credit_num":100}}`,
	}
	sched := fixture.scheduler(t,
		&auth.Auth{UID: "c", AccessToken: "at"},
		&auth.Auth{UID: "a", AccessToken: "at"},
		&auth.Auth{UID: "b", AccessToken: "at"},
	)

	want := []string{"a", "b", "c"} // Pool.List 按 UID 排序

	for range 5 {
		rows := sched.CompensationStatusAll(context.Background())
		if len(rows) != len(want) {
			t.Fatalf("rows=%d want %d", len(rows), len(want))
		}

		for i := range want {
			if rows[i].UID != want[i] {
				t.Fatalf("行序=[%s] want %v", rows[i].UID, want)
			}
		}
	}
}

// TestCompensationGateSkipsGlobalAndEnterprise global/企业版账号零上游请求。
func TestCompensationGateSkipsGlobalAndEnterprise(t *testing.T) {
	t.Parallel()

	global := &auth.Auth{UID: "g1", AccessToken: "at", Domain: "www.workbuddy.ai"}
	enterprise := &auth.Auth{UID: "e1", AccessToken: "at", EnterpriseID: "ent-1"}
	cnAcct := &auth.Auth{UID: "c1", AccessToken: "at"}

	fixture := &compensationFixture{
		httpStatus: http.StatusOK,
		statusBody: `{"code":0,"data":{"active":false}}`,
	}
	sched := fixture.scheduler(t, global, enterprise, cnAcct)

	rows, _ := sched.ClaimCompensationNow()

	if got := fixture.statusCalls.Load(); got != 1 {
		t.Errorf("只有 CN 账号应查询状态，calls=%d", got)
	}

	skipped := 0

	for _, row := range rows {
		if row.Skipped {
			skipped++
		}
	}

	if skipped < 2 {
		t.Errorf("global/企业版应被跳过，rows=%+v", rows)
	}
}

// TestClaimCompensationForBranches 三种分支都显形：待领→领取、无活动→跳过、失败→error。
func TestClaimCompensationForBranches(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		httpStatus int
		statusBody string
		claimBody  string
		wantClaim  int32
		wantSkip   bool
		wantError  bool
		wantCredit int64
	}{
		{"待领 → 领取成功", http.StatusOK, `{"code":0,"data":{"active":true,"credit_num":500}}`, `{"code":0,"data":{"credit":500}}`, 1, false, false, 500},
		{"无活动 → 跳过", http.StatusOK, `{"code":0,"data":{"active":false}}`, `{"code":0,"data":{}}`, 0, true, false, 0},
		{"已领取 → 跳过", http.StatusOK, `{"code":0,"data":{"active":true,"claimed":true,"credit_num":500}}`, `{"code":0,"data":{}}`, 0, true, false, 0},
		{"查询失败 → error", http.StatusInternalServerError, `{"code":500,"msg":"boom"}`, ``, 0, false, true, 0},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			fixture := &compensationFixture{
				httpStatus: testCase.httpStatus,
				statusBody: testCase.statusBody,
				claimBody:  testCase.claimBody,
			}
			sched := fixture.scheduler(t, &auth.Auth{UID: "u1", AccessToken: "at"})

			row := sched.ClaimCompensationFor("u1")

			if got := fixture.claimCalls.Load(); got != testCase.wantClaim {
				t.Errorf("claim calls=%d want %d", got, testCase.wantClaim)
			}

			if row.Skipped != testCase.wantSkip {
				t.Errorf("skipped=%v want %v (row=%+v)", row.Skipped, testCase.wantSkip, row)
			}

			if (row.Error != "") != testCase.wantError {
				t.Errorf("error=%q want has-error=%v", row.Error, testCase.wantError)
			}

			if testCase.wantCredit > 0 && row.CreditNum != testCase.wantCredit {
				t.Errorf("credit=%d want %d", row.CreditNum, testCase.wantCredit)
			}
		})
	}
}

// TestSummarizeCompensationErroredFirst 全部失败时 errored 必须显形（不能算成「没活动」）。
func TestSummarizeCompensationErroredFirst(t *testing.T) {
	t.Parallel()

	summary := SummarizeCompensation([]CompensationRow{
		{Error: "boom"},
		{Error: "boom"},
		{Skipped: true},
		{Claimed: true, CreditNum: 100},
	})

	if summary.Errored != 2 || summary.Skipped != 1 || summary.Claimed != 1 || summary.Credit != 100 {
		t.Fatalf("summary=%+v", summary)
	}
}

// TestClaimCompensationZeroReceiptFallsBack 回执 credit=0 时回落状态额度（issue #251）。
func TestClaimCompensationZeroReceiptFallsBack(t *testing.T) {
	t.Parallel()

	fixture := &compensationFixture{
		httpStatus: http.StatusOK,
		statusBody: `{"code":0,"data":{"active":true,"credit_num":300,"end_time":"2026-10-31 23:59:59"}}`,
		claimBody:  `{"code":0,"data":{"credit":0}}`,
	}
	sched := fixture.scheduler(t, &auth.Auth{UID: "u1", AccessToken: "at"})

	row := sched.ClaimCompensationFor("u1")
	if !row.Claimed || !row.CreditKnown || row.CreditNum != 300 {
		t.Fatalf("row=%+v want claimed known 300", row)
	}

	if row.CreditNum == 0 {
		t.Fatal("must not report fake +0c")
	}
}

// TestClaimCompensationZeroReceiptUnknown 回执与状态都为 0 时标记额度未知。
func TestClaimCompensationZeroReceiptUnknown(t *testing.T) {
	t.Parallel()

	fixture := &compensationFixture{
		httpStatus:      http.StatusOK,
		statusBody:      `{"code":0,"data":{"active":true,"credit_num":1,"end_time":"2026-10-31 23:59:59"}}`,
		statusBodyAfter: `{"code":0,"data":{"active":false}}`,
		claimBody:       `{"code":0,"data":{}}`,
	}
	sched := fixture.scheduler(t, &auth.Auth{UID: "u1", AccessToken: "at"})

	row := sched.ClaimCompensationFor("u1")
	if !row.Claimed || row.CreditKnown {
		t.Fatalf("row=%+v want claimed unknown", row)
	}
}

// TestClaimCompensationIdempotent 同进程重复调用只 claim 一次（issue #251）。
func TestClaimCompensationIdempotent(t *testing.T) {
	t.Parallel()

	fixture := &compensationFixture{
		httpStatus: http.StatusOK,
		statusBody: `{"code":0,"data":{"active":true,"credit_num":500,"end_time":"2026-10-31 23:59:59"}}`,
		claimBody:  `{"code":0,"data":{"credit":500}}`,
	}
	sched := fixture.scheduler(t, &auth.Auth{UID: "u1", AccessToken: "at"})

	first := sched.ClaimCompensationFor("u1")
	second := sched.ClaimCompensationFor("u1")

	if !first.Claimed || !second.Skipped {
		t.Fatalf("first=%+v second=%+v", first, second)
	}

	if got := fixture.claimCalls.Load(); got != 1 {
		t.Fatalf("claim calls=%d want 1", got)
	}
}
