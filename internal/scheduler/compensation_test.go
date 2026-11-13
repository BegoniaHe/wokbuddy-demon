package scheduler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

	rows, _ := sched.ClaimCompensationNow(context.Background())

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

// TestClaimKeyBoundedPerUID 幂等闸按 uid 只保留最近一次周期 key：同一周期重复
// 领取被拦，新周期放行，台账大小按账号数而非「账号数 × 周期数」增长。
func TestClaimKeyBoundedPerUID(t *testing.T) {
	t.Parallel()

	sched := &Scheduler{}
	oldKey := "u1|2026-10-24 23:59:59|500"
	newKey := "u1|2026-10-31 23:59:59|500"

	if !sched.beginClaim(oldKey) {
		t.Fatal("首个周期应占用")
	}

	sched.finishClaim(oldKey, true)

	if sched.beginClaim(oldKey) {
		t.Fatal("同一周期重复领取应被拦截")
	}

	if !sched.beginClaim(newKey) {
		t.Fatal("新周期应放行")
	}

	sched.finishClaim(newKey, true)

	if got := len(sched.claimedKeys); got != 1 {
		t.Fatalf("claimedKeys=%d want 1（每 uid 只留最近一条）", got)
	}

	// 另一账号不受影响。
	if !sched.beginClaim("u2|2026-10-31 23:59:59|500") {
		t.Fatal("不同账号应各自记账")
	}
}

// TestCompensationCancelledRowsAreVisible ctx 取消后未派发的行必须显形。
//
// 未派发的行若保持零值，与「已查询、当前无活动」完全不可区分：SummarizeCompensation
// 会把它算成三不靠（既非 claimed 也非 skipped/errored），批量结果看起来一切正常，
// 实际有 N 个账号根本没查——与本文件「不要静默失败」的命题相反。
func TestCompensationCancelledRowsAreVisible(t *testing.T) {
	t.Parallel()

	fixture := &compensationFixture{
		httpStatus: http.StatusOK,
		statusBody: `{"code":0,"data":{"active":true,"credit_num":100}}`,
	}
	sched := fixture.scheduler(t,
		&auth.Auth{UID: "a", AccessToken: "at"},
		&auth.Auth{UID: "b", AccessToken: "at"},
	)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	rows := sched.CompensationStatusAll(ctx)
	if len(rows) != 2 {
		t.Fatalf("rows=%d want 2", len(rows))
	}

	for i := range rows {
		if !rows[i].Skipped || rows[i].Reason == "" {
			t.Fatalf("未查询的行未显形: %+v", rows[i])
		}
	}

	if got := fixture.statusCalls.Load(); got != 0 {
		t.Fatalf("取消后不应再打上游，statusCalls=%d", got)
	}

	claimRows, summary := sched.ClaimCompensationNow(ctx)
	if len(claimRows) != 2 {
		t.Fatalf("claimRows=%d want 2", len(claimRows))
	}

	if summary.Errored != 0 || summary.Claimed != 0 || summary.Skipped != 2 {
		t.Fatalf("汇总应把未派发行计为 skipped: %+v", summary)
	}
}

// TestCompensationStatusAllDispatchesConcurrently 钉住「并发查询」：guarded 只做
// panic 隔离，同步调用会把批量查询退化成逐账号串行（面板 N × 上游 RTT）。
//
// 用闸门强制两个请求同时在飞：串行实现等不到第二个请求，只能等到超时后返回。
func TestCompensationStatusAllDispatchesConcurrently(t *testing.T) {
	t.Parallel()

	var (
		inFlight atomic.Int32
		peak     atomic.Int32
	)

	gate := make(chan struct{})

	var gateOnce sync.Once

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := inFlight.Add(1)
		defer inFlight.Add(-1)

		for {
			prev := peak.Load()
			if cur <= prev || peak.CompareAndSwap(prev, cur) {
				break
			}
		}

		if cur >= 2 {
			gateOnce.Do(func() { close(gate) })
		}

		select {
		case <-gate:
		case <-time.After(2 * time.Second):
			// 串行实现：第二个请求永远不会同时在飞。
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"active":false}}`))
	}))
	t.Cleanup(srv.Close)

	state := pool.New("")
	state.Add(&auth.Auth{UID: "a", AccessToken: "at"})
	state.Add(&auth.Auth{UID: "b", AccessToken: "at"})

	sched := New(&Config{Pool: state, Upstream: &upstream.Client{
		HTTP: srv.Client(), BillingBaseCN: srv.URL, ChatBaseCN: srv.URL,
	}})

	rows := sched.CompensationStatusAll(context.Background())
	if len(rows) != 2 {
		t.Fatalf("rows=%d want 2", len(rows))
	}

	if got := peak.Load(); got < 2 {
		t.Fatalf("同时在飞的上游请求峰值=%d want >=2（批量查询被串行化了）", got)
	}
}
