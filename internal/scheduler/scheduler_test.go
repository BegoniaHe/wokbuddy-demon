package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/cst"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

var errUnused = errors.New("unused")

func TestNextFire(t *testing.T) {
	t.Parallel()

	loc := time.Local
	now := time.Date(2026, 7, 27, 10, 0, 0, 0, loc)

	next := nextFireIn(now, []int{9, 21}, nil)
	if next.Hour() != 21 || next.Day() != 27 {
		t.Errorf("next=%v want 21:00 same day", next)
	}

	now = time.Date(2026, 7, 27, 22, 0, 0, 0, loc)

	next = nextFireIn(now, []int{9, 21}, nil)
	if next.Hour() != 9 || next.Day() != 28 {
		t.Errorf("next=%v want 09:00 next day", next)
	}

	now = time.Date(2026, 7, 27, 9, 0, 0, 0, loc)

	next = nextFireIn(now, []int{9}, nil)
	if next.Day() != 28 {
		t.Errorf("exact match should roll to next day: %v", next)
	}
}

func TestNextFireMergesSchedules(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 27, 20, 0, 0, 0, time.Local)

	next := nextFireIn(now, []int{9, 21, 22}, nil)
	if next.Hour() != 21 {
		t.Errorf("next=%v want 21 (earliest of 21/22)", next)
	}
}

// TestNextWakeBlackcatUsesCST 夜猫子按 CST 23:00 排程，不受宿主本地时区影响。
func TestNextWakeBlackcatUsesCST(t *testing.T) {
	t.Parallel()

	scheduler := New(&Config{
		BlackcatHours:     []int{23},
		CheckinDisabled:   true,
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		GrowthDisabled:    true,
	})

	host := time.FixedZone("UTC-5", -5*60*60)
	now := time.Date(2026, 9, 11, 9, 0, 0, 0, host) // 14:00 UTC = 22:00 CST

	at, kinds := scheduler.nextWake(now)
	if want := time.Date(2026, 9, 11, 23, 0, 0, 0, cst.Loc); !at.Equal(want) {
		t.Fatalf("next=%v want %v", at, want)
	}

	if !hasKind(kinds, taskBlackcat) {
		t.Fatalf("kinds=%v want blackcat", kinds)
	}

	// CST 23:30 已过窗口起点，下一次落到次日 CST 23:00。
	at, _ = scheduler.nextWake(time.Date(2026, 9, 11, 10, 30, 0, 0, host)) // 15:30 UTC = 23:30 CST
	if want := time.Date(2026, 9, 12, 23, 0, 0, 0, cst.Loc); !at.Equal(want) {
		t.Fatalf("next=%v want %v", at, want)
	}
}

// TestNextWakeKeepaliveOnly 签到已过点时按保活整点唤醒。
func TestNextWakeKeepaliveOnly(t *testing.T) {
	t.Parallel()

	s := New(&Config{
		CheckinHours: []int{9}, KeepaliveHours: []int{22},
		TravelDisabled: true, ActivityDisabled: true,
	})

	at, kinds := s.nextWake(time.Date(2026, 9, 11, 20, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 11, 22, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v", at, want)
	}

	if len(kinds) != 1 || kinds[0] != taskKeepalive {
		t.Errorf("kinds=%v want [keepalive]", kinds)
	}
}

// TestNextWakeSameInstantFiresAll 签到与保活配到同一整点时两类任务都要执行。
func TestNextWakeSameInstantFiresAll(t *testing.T) {
	t.Parallel()

	scheduler := New(&Config{
		CheckinHours:     []int{9, 22},
		TravelHours:      []int{}, // 禁用旅行时点干扰（仅测签到+保活同整点）
		ActivityHours:    []int{}, // 禁用活跃时点干扰
		KeepaliveHours:   []int{22},
		TravelDisabled:   true,
		ActivityDisabled: true,
		BlackcatDisabled: true,
	})

	at, kinds := scheduler.nextWake(time.Date(2026, 9, 11, 21, 30, 0, 0, time.Local))
	if want := time.Date(2026, 9, 11, 22, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v", at, want)
	}

	if !hasKind(kinds, taskCheckin) || !hasKind(kinds, taskKeepalive) {
		t.Errorf("kinds=%v want checkin+keepalive（同一时刻两任务）", kinds)
	}

	// 22 点过后下一次是次日 09:00，且只含签到（旅行/活跃已禁用）。
	at, kinds = scheduler.nextWake(time.Date(2026, 9, 11, 22, 30, 0, 0, time.Local))
	if want := time.Date(2026, 9, 12, 9, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v", at, want)
	}

	if len(kinds) != 1 || kinds[0] != taskCheckin {
		t.Errorf("kinds=%v want [checkin]", kinds)
	}
}

// TestNextWakeNothingScheduled 两类任务全空时返回零值，Run 只等退出信号。
func TestNextWakeNothingScheduled(t *testing.T) {
	t.Parallel()

	s := &Scheduler{cfg: Config{}}

	at, kinds := s.nextWake(time.Now())
	if !at.IsZero() || len(kinds) != 0 {
		t.Errorf("at=%v kinds=%v want zero/nil", at, kinds)
	}
}

// TestNextWakeCheckinDisabled 显式禁用签到后，排程里不再有签到时点（保活照常）。
func TestNextWakeCheckinDisabled(t *testing.T) {
	t.Parallel()

	s := New(&Config{
		CheckinDisabled: true, CheckinHours: []int{9, 21}, KeepaliveHours: []int{22},
		TravelDisabled: true, ActivityDisabled: true,
	})

	at, kinds := s.nextWake(time.Date(2026, 9, 11, 20, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 11, 22, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v（不应再有 21 点签到）", at, want)
	}

	if len(kinds) != 1 || kinds[0] != taskKeepalive {
		t.Errorf("kinds=%v want [keepalive]", kinds)
	}
}

// TestNextWakeKeepaliveDisabled 显式禁用保活后，排程里不再有保活时点（签到照常）。
func TestNextWakeKeepaliveDisabled(t *testing.T) {
	t.Parallel()

	s := New(&Config{
		KeepaliveDisabled: true, CheckinHours: []int{9, 21}, KeepaliveHours: []int{22},
		TravelDisabled: true, ActivityDisabled: true,
	})

	at, kinds := s.nextWake(time.Date(2026, 9, 11, 20, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 11, 21, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v（不应再有 22 点保活）", at, want)
	}

	if len(kinds) != 1 || kinds[0] != taskCheckin {
		t.Errorf("kinds=%v want [checkin]", kinds)
	}
}

// TestNextWakeBothDisabledNothingScheduled 五类任务都显式禁用 → 无可唤醒时点。
func TestNextWakeBothDisabledNothingScheduled(t *testing.T) {
	t.Parallel()

	scheduler := New(&Config{
		CheckinDisabled:   true,
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		BlackcatDisabled:  true,
		CheckinHours:      []int{9, 21},
		KeepaliveHours:    []int{22},
	})

	at, kinds := scheduler.nextWake(time.Now())
	if !at.IsZero() || len(kinds) != 0 {
		t.Errorf("at=%v kinds=%v want zero/nil", at, kinds)
	}
}

// TestRunAllDisabledNoSpinNoCalls 四类任务全禁用：Run 不空转（只等退出信号），
// 且不能触发任何上游请求。
func TestRunAllDisabledNoSpinNoCalls(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "no upstream call expected", http.StatusNotFound)
	}))
	defer srv.Close()

	state := pool.New("")
	state.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})

	client := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
	}
	scheduler := New(&Config{
		Pool:              state,
		Upstream:          client,
		CheckinDisabled:   true,
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
	})

	ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer cancel()

	start := time.Now()

	scheduler.Run(ctx) // 阻塞到 ctx 取消为止（无时点可等，不构造 timer）

	elapsed := time.Since(start)

	if calls.Load() != 0 {
		t.Errorf("upstream calls=%d want 0（四类全禁用）", calls.Load())
	}

	if elapsed < 200*time.Millisecond {
		t.Errorf("Run returned after %v, before ctx done（不应提前返回）", elapsed)
	}

	if elapsed > 2*time.Second {
		t.Errorf("Run took %v（不应空转/忙等）", elapsed)
	}
}

func hasKind(kinds []taskKind, k taskKind) bool {
	return slices.Contains(kinds, k)
}

// fakeUpstream 同时模拟 billing 与 refresh。
type fakeUpstream struct {
	checkinCalls     atomic.Int32
	checkinMu        sync.Mutex
	checkinAt        []time.Time
	refreshCalls     atomic.Int32
	travelCalls      atomic.Int32
	resourceCalls    atomic.Int32
	resourceInFlight atomic.Int32
	resourcePeak     atomic.Int32
	resourceDelay    time.Duration
	resourceRemain   int64
	resourceEnd      string
}

// beginResource 记录一次余额查询的并发峰值与调用数，返回递减在途计数的函数。
func (f *fakeUpstream) beginResource() func() {
	cur := f.resourceInFlight.Add(1)
	for {
		peak := f.resourcePeak.Load()
		if cur <= peak || f.resourcePeak.CompareAndSwap(peak, cur) {
			break
		}
	}

	if f.resourceDelay > 0 {
		time.Sleep(f.resourceDelay)
	}

	f.resourceCalls.Add(1)

	return func() { f.resourceInFlight.Add(-1) }
}

func (f *fakeUpstream) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-checkin"):
			f.checkinCalls.Add(1)
			f.checkinMu.Lock()
			f.checkinAt = append(f.checkinAt, time.Now())
			f.checkinMu.Unlock()
			w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			defer f.beginResource()()
			// remain 需 <= size（取数钳 [0,size]：脏数据 remain>size 会被钳到 size
			// ——上游真实数据恒一致，实测 Cycle{17,482,500}）。
			end := ""
			if f.resourceEnd != "" {
				end = `,"CycleEndTime":` + jsonString(f.resourceEnd)
			}

			w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":1000,"CycleCapacityRemain":` +
				jsonI64(f.resourceRemain) + `,"CycleCapacityUsed":0` + end + `}]}}}}`))
		case strings.HasSuffix(r.URL.Path, "/token/refresh"):
			f.refreshCalls.Add(1)
			w.Write([]byte(`{"code":0,"data":{"accessToken":"new","expiresIn":3600}}`))
		case strings.HasSuffix(r.URL.Path, "/activity/growth/buddy/info"):
			// 已领养（data.buddy 非空）：跳过领养前置，直接进旅行状态查询。
			w.Write([]byte(`{"code":0,"data":{"buddy":{"id":1,"name":"cat"}}}`))
		case strings.HasSuffix(r.URL.Path, "/activity/growth/buddy/travel/status"):
			// daily_limit_reached=true：状态查询计一次调用即止，不发 depart/claim。
			f.travelCalls.Add(1)
			w.Write([]byte(`{"code":0,"data":{"state":"idle","daily_limit_reached":true}}`))
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
}

// checkinTimes 返回签到请求到达时刻快照。
func (f *fakeUpstream) checkinTimes() []time.Time {
	f.checkinMu.Lock()
	defer f.checkinMu.Unlock()

	return append([]time.Time(nil), f.checkinAt...)
}

func jsonI64(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func jsonString(v string) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestRunCheckinReenablesCoolingAccount(t *testing.T) {
	t.Parallel()

	fakeUpstream2 := &fakeUpstream{resourceRemain: 500}

	srv := fakeUpstream2.server()
	defer srv.Close()

	state := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999}
	state.Add(a)
	state.Cooldown("u1", pool.CoolHard, time.Hour, "余额不足")

	client := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
	}
	scheduler := New(&Config{
		Pool:           state,
		Upstream:       client,
		CheckinHours:   []int{9, 21},
		KeepaliveHours: []int{22},
	})
	scheduler.RunCheckinNow()

	if fakeUpstream2.checkinCalls.Load() != 1 {
		t.Errorf("checkin calls=%d", fakeUpstream2.checkinCalls.Load())
	}

	status, _ := state.Status("u1")
	if status.Cooling {
		t.Errorf("account should be reenabled after checkin with credits: %+v", status)
	}

	if status.Credits != 500 {
		t.Errorf("credits=%d want 500", status.Credits)
	}
}

func TestRunKeepaliveRefreshesTokens(t *testing.T) {
	t.Parallel()

	fakeUpstream2 := &fakeUpstream{}

	srv := fakeUpstream2.server()
	defer srv.Close()

	state := pool.New("")
	account := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: 1}
	state.Add(account)

	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
	}
	s := New(&Config{Pool: state, Upstream: up})
	s.RunKeepaliveNow()

	if fakeUpstream2.refreshCalls.Load() != 1 {
		t.Errorf("refresh calls=%d", fakeUpstream2.refreshCalls.Load())
	}

	if account.AccessToken != "new" {
		t.Errorf("token not updated: %s", account.AccessToken)
	}
}

func TestRunKeepaliveSessionDeadDisables(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"code":12153,"msg":"Offline user session not found"}`))
	}))
	defer srv.Close()

	state := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: 1}
	state.Add(a)

	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
	}
	scheduler := New(&Config{Pool: state, Upstream: up})
	// P0-1：12153 连续 N 次才禁用。前 2 次刷新失败不应杀号（误判防护）。
	scheduler.RunKeepaliveNow()

	if st, _ := state.Status("u1"); st.Disabled {
		t.Fatalf("第 1 次 12153 不应禁用: %+v", st)
	}

	scheduler.RunKeepaliveNow()

	if st, _ := state.Status("u1"); st.Disabled {
		t.Fatalf("第 2 次 12153 不应禁用: %+v", st)
	}
	// 第 3 次连续 12153 → 禁用。
	scheduler.RunKeepaliveNow()

	status, _ := state.Status("u1")
	if !status.Disabled {
		t.Errorf("第 3 次连续 12153 应禁用: %+v", status)
	}

	if status.DisabledReason != "12153 session dead" {
		t.Errorf("disabled_reason=%q want 12153 session dead", status.DisabledReason)
	}
}

// TestRunKeepaliveSessionDeadResetBySuccess 两次 12153 后刷新成功 → 计数清零，
// 再来的 12153 从第 1 次重新计（不会因历史失败被继续追杀）。
func TestRunKeepaliveSessionDeadResetBySuccess(t *testing.T) {
	t.Parallel()

	var fails atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fails.Add(1) == 3 { // 第 3 次（本次调度循环的第二轮）刷新成功
			w.Write([]byte(`{"code":0,"data":{"accessToken":"new","expiresIn":3600}}`))
			return
		}

		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"code":12153,"msg":"Offline user session not found"}`))
	}))
	defer srv.Close()

	state := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: 1}
	state.Add(a)

	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
	}
	scheduler := New(&Config{Pool: state, Upstream: up})
	scheduler.RunKeepaliveNow() // 12153 #1
	scheduler.RunKeepaliveNow() // 12153 #2

	if st, _ := state.Status("u1"); st.Disabled {
		t.Fatalf("precondition: 前 2 次不应禁用: %+v", st)
	}

	scheduler.RunKeepaliveNow() // 刷新成功 → 清计数
	// 接下来连续 2 次 12153：从新计数重新算，仍不应禁用（历史计数已清）。
	scheduler.RunKeepaliveNow() // 12153 #1（新计数）
	scheduler.RunKeepaliveNow() // 12153 #2（新计数）

	if st, _ := state.Status("u1"); st.Disabled {
		t.Fatalf("刷新成功清计数后连续 2 次 12153 不应禁用: %+v", st)
	}

	scheduler.RunKeepaliveNow() // 12153 #3（新计数）→ 禁用

	if st, _ := state.Status("u1"); !st.Disabled {
		t.Fatalf("新计数第 3 次 12153 应禁用: %+v", st)
	}
}

func TestCheckinErrorDoesNotCrash(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`boom`))
	}))
	defer srv.Close()

	state := pool.New("")
	state.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})

	client := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
		RetryDelay:    time.Millisecond, // 注入短重试间隔，避免 500 重试拖慢用例（issue #20）
	}
	s := New(&Config{Pool: state, Upstream: client})
	// 不应 panic
	s.RunCheckinNow()
	s.RunKeepaliveNow()

	_ = errUnused
}

// TestRunBalanceRefreshNowUpdatesCreditsAndRevives 只查余额（不签到）即可更新 credits
// 并解冻余额恢复的冷却账号——面板手动刷新与后台周期任务共用该语义。
func TestRunBalanceRefreshNowUpdatesCreditsAndRevives(t *testing.T) {
	t.Parallel()

	fakeUpstream2 := &fakeUpstream{resourceRemain: 777}

	srv := fakeUpstream2.server()
	defer srv.Close()

	state := pool.New("")
	state.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	state.Add(&auth.Auth{UID: "u2", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	state.Cooldown("u1", pool.CoolHard, time.Hour, "余额不足")
	state.Disable("u2", "manual")

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(&Config{Pool: state, Upstream: up})
	s.RunBalanceRefreshNow()

	st, _ := state.Status("u1")
	if st.Cooling || st.Credits != 777 {
		t.Errorf("u1 want revived with credits=777: cooling=%v credits=%d", st.Cooling, st.Credits)
	}

	if fakeUpstream2.checkinCalls.Load() != 0 {
		t.Errorf("balance refresh must not checkin, got %d calls", fakeUpstream2.checkinCalls.Load())
	}
	// 禁用账号不参与：其 credits 保持 0（未被 UserResource 覆盖解冻）。
	if st2, _ := state.Status("u2"); !st2.Disabled {
		t.Errorf("u2 must stay disabled")
	}
}

// TestRunBalanceRefreshNowBoundsConcurrency 余额刷新逐号上游调用必须有界：
// 账号量大时不得一次性 fan-out 全部请求。用延迟保证并发窗口重叠，再断言峰值。
func TestRunBalanceRefreshNowBoundsConcurrency(t *testing.T) {
	t.Parallel()

	const accounts = 20

	fake := &fakeUpstream{resourceRemain: 1, resourceDelay: 20 * time.Millisecond}

	srv := fake.server()
	defer srv.Close()

	state := pool.New("")

	for i := range accounts {
		uid := fmt.Sprintf("u%02d", i)
		state.Add(&auth.Auth{UID: uid, AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	}

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(&Config{Pool: state, Upstream: up})
	s.RunBalanceRefreshNow()

	if got := fake.resourcePeak.Load(); got > balanceRefreshWorkers {
		t.Fatalf("peak concurrency=%d want <= %d", got, balanceRefreshWorkers)
	}

	if got := fake.resourceCalls.Load(); got != accounts {
		t.Fatalf("resource calls=%d want %d", got, accounts)
	}
}

// TestRunBalanceRefreshNowSharesConcurrencyAcrossCalls 后台周期刷新与面板手动
// 刷新可能并发触发：两个入口必须共享同一个并发上限，否则总在途请求翻倍。
func TestRunBalanceRefreshNowSharesConcurrencyAcrossCalls(t *testing.T) {
	t.Parallel()

	const accounts = 20

	fake := &fakeUpstream{resourceRemain: 1, resourceDelay: 20 * time.Millisecond}

	srv := fake.server()
	defer srv.Close()

	state := pool.New("")

	for i := range accounts {
		uid := fmt.Sprintf("u%02d", i)
		state.Add(&auth.Auth{UID: uid, AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	}

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(&Config{Pool: state, Upstream: up})

	var wg sync.WaitGroup

	for range 2 {
		wg.Go(func() { s.RunBalanceRefreshNowCtx(t.Context()) })
	}

	wg.Wait()

	if got := fake.resourcePeak.Load(); got > balanceRefreshWorkers {
		t.Fatalf("peak concurrency=%d want <= %d across concurrent refreshes", got, balanceRefreshWorkers)
	}

	if got := fake.resourceCalls.Load(); got != 2*accounts {
		t.Fatalf("resource calls=%d want %d", got, 2*accounts)
	}
}

// TestRunBalanceRefreshNowCtxCanceledStopsDispatch 已取消的 ctx 不得再派发逐号
// 上游请求：面板请求断开或进程停机时不应继续空跑。
func TestRunBalanceRefreshNowCtxCanceledStopsDispatch(t *testing.T) {
	t.Parallel()

	const accounts = 5

	fake := &fakeUpstream{resourceRemain: 1}

	srv := fake.server()
	defer srv.Close()

	state := pool.New("")

	for i := range accounts {
		uid := fmt.Sprintf("u%02d", i)
		state.Add(&auth.Auth{UID: uid, AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	}

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(&Config{Pool: state, Upstream: up})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	s.RunBalanceRefreshNowCtx(ctx)

	if got := fake.resourceCalls.Load(); got != 0 {
		t.Fatalf("resource calls=%d want 0 after canceled ctx", got)
	}
}

// TestStartBalanceRefreshHotEnableFromPaused 启动时关闭（interval=0）也必须起 worker：
// 运行期 SetBalanceInterval 热开后才会有周期刷新，无需重启。
func TestStartBalanceRefreshHotEnableFromPaused(t *testing.T) {
	t.Parallel()

	f := &fakeUpstream{resourceRemain: 777}

	srv := f.server()
	defer srv.Close()

	state := pool.New("")
	state.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})

	up := &upstream.Client{
		HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL,
		RetryDelay: time.Millisecond, // 避免测试收尾时卡在 2s/4s 的退避睡眠
	}
	scheduler := New(&Config{Pool: state, Upstream: up})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	scheduler.StartBalanceRefresh(ctx, 0)
	scheduler.SetBalanceInterval(5 * time.Millisecond)

	deadline := time.Now().Add(2 * time.Second)

	for {
		if st, ok := state.Status("u1"); ok && st.Credits == 777 {
			return
		}

		if time.Now().After(deadline) {
			t.Fatal("启动时暂停的余额刷新在热开 interval 后仍未运行")
		}

		time.Sleep(5 * time.Millisecond)
	}
}

// TestNextWakeGrowthSlot growth 排程进候选 + 禁用退场（每日自动执行成长任务队列）。
func TestNextWakeGrowthSlot(t *testing.T) {
	t.Parallel()

	s := New(&Config{GrowthHours: []int{1}})
	at, kinds := s.nextWake(time.Date(2026, 9, 27, 0, 10, 0, 0, time.Local))
	hasGrowth := false

	for _, k := range kinds {
		if k == taskGrowth {
			hasGrowth = true
		}
	}

	if !hasGrowth || at.Hour() != 1 || at.Day() != 27 {
		t.Fatalf("growth 槽位: at=%v kinds=%v（期望 09-27 01:00 含 taskGrowth）", at, kinds)
	}
	// 禁用后不进候选（其余 kind 为空 → nextWake 零值返回）
	s2 := New(&Config{GrowthHours: []int{1}, GrowthDisabled: true})

	_, kinds2 := s2.nextWake(time.Date(2026, 9, 27, 0, 10, 0, 0, time.Local))
	for _, k := range kinds2 {
		if k == taskGrowth {
			t.Fatal("禁用后 growth 仍在候选")
		}
	}
}

func TestRunBalanceRefreshReplacesExpirySnapshot(t *testing.T) {
	t.Parallel()

	end := time.Now().In(time.FixedZone("CST", 8*3600)).Add(24 * time.Hour).Format("2006-01-02 15:04:05")
	fakeUpstream2 := &fakeUpstream{resourceRemain: 100, resourceEnd: end}

	srv := fakeUpstream2.server()
	defer srv.Close()

	state := pool.New("")
	state.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	state.SetCreditsDetailed("u1", 999, 999, 999, time.Now().Add(time.Hour), 999)

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	scheduler := New(&Config{Pool: state, Upstream: up, ExpiringSoonWindow: 7 * 24 * time.Hour})
	scheduler.RunBalanceRefreshNow()

	status, _ := state.Status("u1")
	if status.Credits != 100 || status.CreditsExpiring != 100 || status.CreditsEarliestRemaining != 100 || status.CreditsEarliestExpiry.IsZero() {
		t.Fatalf("snapshot=%+v", status)
	}

	fakeUpstream2.resourceEnd = ""

	scheduler.RunBalanceRefreshNow()

	status, _ = state.Status("u1")
	if status.CreditsExpiring != 0 || status.CreditsEarliestRemaining != 0 || !status.CreditsEarliestExpiry.IsZero() {
		t.Fatalf("zero expiry refresh did not clear snapshot=%+v", status)
	}
}

// TestRunCheckinKeepsExpiryBuckets 签到链路写完整余额快照：不得把后台刷新刚写入的
// 逐日到期批次清空（SetCreditsDetailed 只带最早一批，会清旧批次）。
func TestRunCheckinKeepsExpiryBuckets(t *testing.T) {
	t.Parallel()

	end := time.Now().In(time.FixedZone("CST", 8*3600)).Add(24 * time.Hour).Format("2006-01-02 15:04:05")
	fakeUpstream2 := &fakeUpstream{resourceRemain: 100, resourceEnd: end}

	srv := fakeUpstream2.server()
	defer srv.Close()

	state := pool.New("")
	state.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	at := time.Now().Add(time.Hour)
	state.SetCreditsSnapshot("u1", 100, 1000, 100, []pool.ExpiryBucket{{
		Date: at.Format("2006-01-02"), ExpireAt: at, Remain: 100,
	}})

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	scheduler := New(&Config{Pool: state, Upstream: up, ExpiringSoonWindow: 7 * 24 * time.Hour})
	scheduler.RunCheckinNow()

	status, _ := state.Status("u1")
	if len(status.CreditsExpiryBuckets) == 0 {
		t.Fatalf("签到后到期批次被清空: %+v", status)
	}
}

func TestSetExpiringSoonWindowClearsSnapshot(t *testing.T) {
	t.Parallel()

	state := pool.New("")
	state.Add(&auth.Auth{UID: "u1"})
	state.SetCreditsDetailed("u1", 100, 100, 50, time.Now().Add(time.Hour), 50)
	s := New(&Config{Pool: state, ExpiringSoonWindow: 7 * 24 * time.Hour})

	s.SetExpiringSoonWindow(24 * time.Hour)

	if got := s.ExpiringSoonWindow(); got != 24*time.Hour {
		t.Fatalf("window=%v want 24h", got)
	}

	st, _ := state.Status("u1")
	if st.CreditsExpiring != 0 || st.CreditsEarliestRemaining != 0 {
		t.Fatalf("window change did not clear snapshot=%+v", st)
	}
}

// ---------------------------------------------------------------------------
// schedule.include_disabled_in_tasks：保号类四任务是否覆盖「已禁用」账号
// ---------------------------------------------------------------------------

// poolWithDisabledAccount 建一个两账号池：u1 可用、u2 被人工禁用（终态）。
func poolWithDisabledAccount() *pool.Pool {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Disable("u2", "manual disable (test)")

	return p
}

// TestRunCheckinSkipsDisabledByDefault 缺省（开关未开）下禁用账号不签到——锁定既有行为。
func TestRunCheckinSkipsDisabledByDefault(t *testing.T) {
	t.Parallel()

	fakeUpstream2 := &fakeUpstream{resourceRemain: 500}

	srv := fakeUpstream2.server()
	defer srv.Close()

	p := poolWithDisabledAccount()
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(&Config{Pool: p, Upstream: up, CheckinHours: []int{9, 21}, KeepaliveHours: []int{22}})

	s.RunCheckinNow()

	if got := fakeUpstream2.checkinCalls.Load(); got != 1 {
		t.Errorf("checkin calls=%d want 1（仅 u1；禁用号默认跳过）", got)
	}
}

// TestRunCheckinIncludesDisabledWhenConfigured 开关打开后禁用账号也签到，
// 但**不被解冻**（ReenableIfCredits 对 disabled 是 no-op），且**仍不参与选号**。
func TestRunCheckinIncludesDisabledWhenConfigured(t *testing.T) {
	t.Parallel()

	fakeUpstream2 := &fakeUpstream{resourceRemain: 500}

	srv := fakeUpstream2.server()
	defer srv.Close()

	state := poolWithDisabledAccount()
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	scheduler := New(&Config{
		Pool: state, Upstream: up,
		CheckinHours:           []int{9, 21},
		IncludeDisabledInTasks: true,
	})

	scheduler.RunCheckinNow()

	if got := fakeUpstream2.checkinCalls.Load(); got != 2 {
		t.Errorf("checkin calls=%d want 2（u1 + 禁用号 u2）", got)
	}

	if st, _ := state.Status("u2"); !st.Disabled {
		t.Errorf("禁用号签到后不应被自动解冻: %+v", st)
	}
	// 选号侧不受本开关影响：禁用号依旧不可选。
	if got := state.Pick(); got == nil || got.UID != "u1" {
		t.Errorf("选号应仍只给 u1, got %+v", got)
	}
}

// TestRunKeepaliveIncludesDisabledRenewsToken 开关打开后禁用账号也续期 token
// （轮换用法下保持闲置号可用的关键）。
func TestRunKeepaliveIncludesDisabledRenewsToken(t *testing.T) {
	t.Parallel()

	fakeUpstream2 := &fakeUpstream{}

	srv := fakeUpstream2.server()
	defer srv.Close()

	p := poolWithDisabledAccount()
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(&Config{Pool: p, Upstream: up, IncludeDisabledInTasks: true})

	s.RunKeepaliveNow()

	if got := fakeUpstream2.refreshCalls.Load(); got != 2 {
		t.Errorf("refresh calls=%d want 2（含禁用号续期 token）", got)
	}
}

// TestRunBalanceRefreshIncludesDisabledUpdatesCredits 开关打开后禁用账号的积分也被刷新
// （面板据此判断下一个该启用谁），但不会被解冻。
func TestRunBalanceRefreshIncludesDisabledUpdatesCredits(t *testing.T) {
	t.Parallel()

	f := &fakeUpstream{resourceRemain: 700}

	srv := f.server()
	defer srv.Close()

	state := poolWithDisabledAccount()
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(&Config{Pool: state, Upstream: up, IncludeDisabledInTasks: true})

	s.RunBalanceRefreshNow()

	status, _ := state.Status("u2")
	if status.Credits != 700 {
		t.Errorf("禁用号 credits=%d want 700（余额刷新应覆盖禁用号）", status.Credits)
	}

	if !status.Disabled {
		t.Errorf("余额刷新不得解冻禁用号: %+v", status)
	}
}

// TestSetIncludeDisabledInTasksHot setter 热更新立即生效（面板保存配置走这条路径）。
func TestSetIncludeDisabledInTasksHot(t *testing.T) {
	t.Parallel()

	fakeUpstream2 := &fakeUpstream{resourceRemain: 500}

	srv := fakeUpstream2.server()
	defer srv.Close()

	p := poolWithDisabledAccount()
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	scheduler := New(&Config{Pool: p, Upstream: up})

	scheduler.RunCheckinNow()

	if got := fakeUpstream2.checkinCalls.Load(); got != 1 {
		t.Fatalf("前置：缺省应只签 1 个，got %d", got)
	}

	scheduler.SetIncludeDisabledInTasks(true)
	scheduler.RunCheckinNow()

	if got := fakeUpstream2.checkinCalls.Load(); got != 3 {
		t.Errorf("热更新后 checkin calls=%d want 3（1 + u1 + u2）", got)
	}
}

// TestPausedAccountStillRunsKeepaliveTasks 暂停选号的账号**照常参与**保号任务
// （签到 / 保活 / 余额刷新）——这是「暂停选号」与「禁用」的核心区别，也是本功能
// 的存在理由：轮换用法下让位的号仍需养着，否则积分断档、token 过期要重新登录。
// 注意：**不开** IncludeDisabledInTasks——paused 不依赖那个全局开关。
func TestPausedAccountStillRunsKeepaliveTasks(t *testing.T) {
	t.Parallel()

	fakeUpstream2 := &fakeUpstream{resourceRemain: 700}

	srv := fakeUpstream2.server()
	defer srv.Close()

	state := pool.New("")
	state.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	state.Add(&auth.Auth{UID: "u2", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})

	if !state.Pause("u2") {
		t.Fatal("Pause 失败")
	}

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	scheduler := New(&Config{Pool: state, Upstream: up}) // 有意不设 IncludeDisabledInTasks

	scheduler.RunCheckinNow()

	if got := fakeUpstream2.checkinCalls.Load(); got != 2 {
		t.Errorf("checkin calls=%d want 2（暂停号也签到，且无需全局开关）", got)
	}

	scheduler.RunKeepaliveNow()

	if got := fakeUpstream2.refreshCalls.Load(); got != 2 {
		t.Errorf("refresh calls=%d want 2（暂停号也续期 token）", got)
	}

	scheduler.RunBalanceRefreshNow()

	if st, _ := state.Status("u2"); st.Credits != 700 {
		t.Errorf("暂停号 credits=%d want 700（余额刷新应覆盖）", st.Credits)
	}
	// 保号任务不得改变暂停状态（签到解冻的是冷却，不是 paused）
	if st, _ := state.Status("u2"); !st.Paused {
		t.Errorf("保号任务后暂停状态应保持: %+v", st)
	}
	// 选号侧始终排除暂停号
	if got := state.Pick(); got == nil || got.UID != "u1" {
		t.Errorf("选号应只给 u1, got %+v", got)
	}
}

// TestPausedVsDisabledTaskParticipation 固化二者对比：都退出选号，但禁用号默认
// 跳过保号（除非开 include_disabled_in_tasks），暂停号**无条件**参与。
func TestPausedVsDisabledTaskParticipation(t *testing.T) {
	t.Parallel()

	fakeUpstream2 := &fakeUpstream{resourceRemain: 700}

	srv := fakeUpstream2.server()
	defer srv.Close()

	state := pool.New("")
	state.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	state.Add(&auth.Auth{UID: "u2", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	state.Add(&auth.Auth{UID: "u3", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	state.Pause("u2")
	state.Disable("u3", "manual disable (test)")

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(&Config{Pool: state, Upstream: up}) // 全局开关有意保持关闭

	s.RunCheckinNow()
	// u1（正常）+ u2（暂停）签到；u3（禁用）跳过 ⇒ 2 次
	if got := fakeUpstream2.checkinCalls.Load(); got != 2 {
		t.Errorf("checkin calls=%d want 2（正常 + 暂停参与；禁用跳过）", got)
	}
}

// TestPausedStillTravels 暂停号照常跑旅行：旅行是纯 RPC（状态/派出/领奖 +
// 领养前置上报），不发模型对话，与「让位防风控」不冲突——唯一被跳过的
// 对话类任务只有夜猫子（RunNightChats 真实 ChatStream）。
func TestPausedStillTravels(t *testing.T) {
	t.Parallel()
	fastTravel(t)

	fakeUpstream2 := &fakeUpstream{}

	srv := fakeUpstream2.server()
	defer srv.Close()

	state := pool.New("")
	state.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	state.Add(&auth.Auth{UID: "u2", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	state.Pause("u2")

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(&Config{Pool: state, Upstream: up})

	s.RunTravelNow()

	if got := fakeUpstream2.travelCalls.Load(); got != 2 {
		t.Errorf("travel status calls=%d want 2（u1 + 暂停号 u2 照常旅行）", got)
	}
}

// TestNextFireAcrossDST 夏令时切换日 24h 不等于一个日历日：次日槽位必须仍是
// 同一墙钟小时，不能因为 Add(24h) 漂移到 08:00/10:00。
func TestNextFireAcrossDST(t *testing.T) {
	t.Parallel()

	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skipf("tzdata 不可用，跳过 DST 用例: %v", err)
	}

	// 2026-10-25 03:00 CEST 回拨到 02:00 CET：Add(24h) 会把 09:00 变成 08:00。
	now := time.Date(2026, 10, 24, 10, 0, 0, 0, loc)
	if next := nextFireIn(now, []int{9}, nil); next.Hour() != 9 || next.Day() != 25 {
		t.Errorf("回拨日 next=%v want 2026-10-25 09:00", next)
	}

	// 2026-03-29 02:00 CET 拨快到 03:00 CEST：Add(24h) 会把 09:00 变成 10:00。
	now = time.Date(2026, 3, 28, 10, 0, 0, 0, loc)
	if next := nextFireIn(now, []int{9}, nil); next.Hour() != 9 || next.Day() != 29 {
		t.Errorf("拨快日 next=%v want 2026-03-29 09:00", next)
	}
}

// TestNextFireSkippedMidnight 午夜拨快的时区（America/Santiago 2026-09-06
// 00:00 → 01:00）里次日 00:00 不存在，time.Date 会把它归一化到前一日 23:00：
// 未过 23:00 时在错误小时触发，已过 23:00 时是过去时刻——waitSlot 立即到点、
// 任务被反复补跑。应取被抹掉小时之后的第一个实际实例。
func TestNextFireSkippedMidnight(t *testing.T) {
	t.Parallel()

	loc, err := time.LoadLocation("America/Santiago")
	if err != nil {
		t.Skipf("tzdata 不可用，跳过 DST 用例: %v", err)
	}

	for _, now := range []time.Time{
		time.Date(2026, 9, 5, 22, 0, 0, 0, loc),  // 归一化结果 23:00 在未来但小时错误
		time.Date(2026, 9, 5, 23, 30, 0, 0, loc), // 归一化结果 23:00 已是过去时刻
	} {
		next := nextFireIn(now, []int{0}, nil)
		if !next.After(now) {
			t.Fatalf("now=%v next=%v 未晚于 now（过去时刻会让调度器空转）", now, next)
		}

		want := time.Date(2026, 9, 6, 1, 0, 0, 0, loc)
		if !next.Equal(want) {
			t.Fatalf("now=%v next=%v want %v（00:00 被拨快抹掉，取 01:00）", now, next, want)
		}
	}
}

// TestNextFirePartialSkippedHour 跳过区间起点不在整点时（Pacific/Chatham
// 2026-09-27 02:45 → 03:45），time.Date(03:00) 会越过目标小时落到 04:00。
// 03 点槽位应在 03:45 触发，而不是等到 04:00。
func TestNextFirePartialSkippedHour(t *testing.T) {
	t.Parallel()

	loc, err := time.LoadLocation("Pacific/Chatham")
	if err != nil {
		t.Skipf("tzdata 不可用，跳过 DST 用例: %v", err)
	}

	now := time.Date(2026, 9, 27, 2, 0, 0, 0, loc)

	next := nextFireIn(now, []int{3}, nil)
	if want := time.Date(2026, 9, 27, 3, 45, 0, 0, loc); !next.Equal(want) {
		t.Fatalf("now=%v next=%v want %v（03:45 是该小时首个实际时刻）", now, next, want)
	}
}

// TestStartTaskSweepHotEnableFromPaused 启动时暂停（interval=0）也必须起 worker：
// 运行期 SetTaskSweepInterval 热开后才会有周期清扫，无需重启。
func TestStartTaskSweepHotEnableFromPaused(t *testing.T) {
	t.Parallel()

	scheduler := New(&Config{Pool: pool.New("")})

	var calls atomic.Int32

	scheduler.SetTaskSweepHook(func(context.Context) { calls.Add(1) })

	ctx := t.Context()

	scheduler.StartTaskSweep(ctx, 0)
	scheduler.SetTaskSweepInterval(5 * time.Millisecond)

	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("启动时暂停的补领清扫在热开 interval 后仍未运行")
		}

		time.Sleep(5 * time.Millisecond)
	}

	// 热暂停后不再有新的调用。
	scheduler.SetTaskSweepInterval(0)
	time.Sleep(30 * time.Millisecond)

	before := calls.Load()

	time.Sleep(50 * time.Millisecond)

	if after := calls.Load(); after != before {
		t.Errorf("暂停后仍在执行：before=%d after=%d", before, after)
	}
}

// TestSetBalanceIntervalUpdatesMeta 热改余额刷新周期时同步「数据新鲜度」元数据：
// 关闭后不得继续显示旧周期（面板据此标注「每 N 分钟后台刷新」）。
func TestSetBalanceIntervalUpdatesMeta(t *testing.T) {
	t.Parallel()

	state := pool.New("")
	scheduler := New(&Config{Pool: state})

	refreshedAt := time.Now().Add(-time.Minute).Truncate(time.Second)
	state.SetBalanceRefreshMeta(300, refreshedAt)

	scheduler.SetBalanceInterval(0)

	sec, at := state.BalanceRefreshMeta()
	if sec != 0 || !at.Equal(refreshedAt) {
		t.Fatalf("关闭后 meta=%d/%v want 0/%v", sec, at, refreshedAt)
	}

	scheduler.SetBalanceInterval(5 * time.Minute)

	sec, at = state.BalanceRefreshMeta()
	if sec != 300 || !at.Equal(refreshedAt) {
		t.Fatalf("重开后 meta=%d/%v want 300/%v", sec, at, refreshedAt)
	}
}
