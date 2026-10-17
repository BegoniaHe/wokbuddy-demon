package scheduler

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// waitTaskDone 轮询等待任务族结束（超时 Fatal）。
func waitTaskDone(t *testing.T, s *Scheduler, task string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !s.TaskRunning(task) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("task %s did not finish", task)
}

// runStateOf 取任务族状态；不存在 Fatal。
func runStateOf(t *testing.T, s *Scheduler, task string) TaskRun {
	t.Helper()
	for _, r := range s.RunTaskStates() {
		if r.Task == task {
			return r
		}
	}
	t.Fatalf("run state for %s missing", task)
	return TaskRun{}
}

// blockingActivityScheduler 构造活跃上报被阻塞的调度器：entered/release 控制。
func blockingActivityScheduler(t *testing.T) (*Scheduler, chan struct{}, chan struct{}, *int64) {
	t.Helper()
	// activityAccountDelay 默认较大；测试置零避免等待。
	oldDelay := activityAccountDelay
	activityAccountDelay = 0
	t.Cleanup(func() { activityAccountDelay = oldDelay })

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/report" {
			atomic.AddInt64(&calls, 1)
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
	}))
	t.Cleanup(srv.Close)

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999, Domain: "workbuddy.cn"})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	return New(Config{Pool: p, Upstream: up}), entered, release, &calls
}

// TestStartTaskSingleFlight 同一任务族单飞：第二次 StartTask 返回 false，
// 不产生第二轮上游调用；结束后状态记录触发来源与账号摘要（issue #112）。
func TestStartTaskSingleFlight(t *testing.T) {
	s, entered, release, calls := blockingActivityScheduler(t)
	if !s.StartTask(TaskActivity, TriggerManual) {
		t.Fatal("first StartTask should start")
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first batch did not reach upstream")
	}
	if s.StartTask(TaskActivity, TriggerManual) {
		t.Error("second StartTask must be rejected while running")
	}
	close(release)
	waitTaskDone(t, s, TaskActivity)

	assertFinishedRun(t, s, TaskActivity, TriggerManual, 1, 1, 0)
	if n := atomic.LoadInt64(calls); n != 1 {
		t.Errorf("upstream calls=%d want 1（第二轮被守卫挡住）", n)
	}
}

// assertFinishedRun 断言任务族已结束且摘要匹配。
func assertFinishedRun(t *testing.T, s *Scheduler, task, trigger string, total, success, failure int) {
	t.Helper()
	run := runStateOf(t, s, task)
	if run.Running || run.Trigger != trigger || run.Total != total || run.Success != success || run.Failure != failure {
		t.Fatalf("run=%+v want finished %s total=%d success=%d failure=%d", run, trigger, total, success, failure)
	}
}

// TestSchedulerAndPanelShareGuard 调度器同步派发与面板手动触发共用守卫：
// 面板轮在跑时，调度器这一轮直接跳过，不并发打第二轮。
func TestSchedulerAndPanelShareGuard(t *testing.T) {
	s, entered, release, calls := blockingActivityScheduler(t)
	if !s.StartTask(TaskActivity, TriggerManual) {
		t.Fatal("manual StartTask should start")
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("manual batch did not reach upstream")
	}
	// 调度器到点派发：应被守卫挡住并立即返回。
	done := make(chan struct{})
	go func() {
		s.runTaskSync(t.Context(), TaskActivity, TriggerScheduler)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("scheduler run should return immediately when panel run is active")
	}
	close(release)
	waitTaskDone(t, s, TaskActivity)
	if n := atomic.LoadInt64(calls); n != 1 {
		t.Errorf("upstream calls=%d want 1", n)
	}
}

// TestTaskRunRecordsFailure 失败账号计入 failure（上游 500）。
func TestTaskRunRecordsFailure(t *testing.T) {
	oldDelay := activityAccountDelay
	activityAccountDelay = 0
	t.Cleanup(func() { activityAccountDelay = oldDelay })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999, Domain: "workbuddy.cn"})
	s := New(Config{Pool: p, Upstream: &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}})

	if !s.StartTask(TaskActivity, TriggerManual) {
		t.Fatal("StartTask should start")
	}
	waitTaskDone(t, s, TaskActivity)
	run := runStateOf(t, s, TaskActivity)
	if run.Total != 1 || run.Success != 0 || run.Failure != 1 {
		t.Fatalf("run=%+v want total=1 success=0 failure=1", run)
	}
}

// TestBlackcatManualRespectsWindow 面板手动触发夜猫子仍受 23:00–08:00 窗口门控：
// 窗口外不发起任何上游调用，状态 total=0（issue #112 验收）。
func TestBlackcatManualRespectsWindow(t *testing.T) {
	oldNow := nowForNightWindow
	nowForNightWindow = func() time.Time { return time.Date(2026, 10, 17, 12, 0, 0, 0, time.Local) }
	t.Cleanup(func() { nowForNightWindow = oldNow })

	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
	}))
	t.Cleanup(srv.Close)
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999, Domain: "workbuddy.cn"})
	s := New(Config{Pool: p, Upstream: &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}})

	if !s.StartTask(TaskBlackcat, TriggerManual) {
		t.Fatal("StartTask should start")
	}
	waitTaskDone(t, s, TaskBlackcat)
	if n := atomic.LoadInt64(&calls); n != 0 {
		t.Errorf("upstream calls=%d want 0（窗口外跳过）", n)
	}
	run := runStateOf(t, s, TaskBlackcat)
	if run.Total != 0 || run.Success != 0 || run.Failure != 0 {
		t.Fatalf("run=%+v want zero counters", run)
	}
}

// TestStreakManualGatesGlobalAndEnterprise 手动触发连登：global/企业版跳过，
// 不发起上游调用（与调度器同口径）。
func TestStreakManualGatesGlobalAndEnterprise(t *testing.T) {
	oldGlobal := auth.GlobalEnabled()
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(oldGlobal) })

	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
	}))
	t.Cleanup(srv.Close)

	p := pool.New("")
	global := &auth.Auth{UID: "g", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999}
	if _, err := auth.BackfillRealmFor(global, "global"); err != nil {
		t.Fatal(err)
	}
	enterprise := &auth.Auth{UID: "e", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999, EnterpriseID: "ent"}
	p.Add(global)
	p.Add(enterprise)
	s := New(Config{Pool: p, Upstream: &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}})

	if !s.StartTask(TaskStreak, TriggerManual) {
		t.Fatal("StartTask should start")
	}
	waitTaskDone(t, s, TaskStreak)
	if n := atomic.LoadInt64(&calls); n != 0 {
		t.Errorf("upstream calls=%d want 0（global/企业版门控）", n)
	}
	if run := runStateOf(t, s, TaskStreak); run.Total != 0 {
		t.Fatalf("run=%+v want total=0", run)
	}
}
