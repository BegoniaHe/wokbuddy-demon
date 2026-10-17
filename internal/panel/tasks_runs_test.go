package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/livecfg"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/scheduler"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// postAuthed 发一个带面板密钥的 POST。
func postAuthed(pn *Panel, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)
	return rec
}

// getAuthed 发一个带面板密钥的 GET。
func getAuthed(pn *Panel, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)
	return rec
}

// runsOf 解析 tasks/runs 响应。
func runsOf(t *testing.T, pn *Panel) []scheduler.TaskRun {
	t.Helper()
	rec := getAuthed(pn, "/panel/api/tasks/runs")
	if rec.Code != http.StatusOK {
		t.Fatalf("runs code=%d body=%s", rec.Code, rec.Body)
	}
	var resp struct {
		Runs []scheduler.TaskRun `json:"runs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp.Runs
}

// findRun 取任务族状态。
func findRun(runs []scheduler.TaskRun, task string) *scheduler.TaskRun {
	for i := range runs {
		if runs[i].Task == task {
			return &runs[i]
		}
	}
	return nil
}

// TestPanelTaskRunsMutexAndState 面板批量入口的互斥与运行状态（issue #112）：
// 第二次触发 409，GET tasks/runs 可查当前轮与最近一轮摘要。
func TestPanelTaskRunsMutexAndState(t *testing.T) {
	pn, entered, release, calls := newBlockingPanel(t)
	assertStarted(t, postAuthed(pn, "/panel/api/activity_all"))
	waitEntered(t, entered)
	assertConflictRunning(t, postAuthed(pn, "/panel/api/activity_all"))
	assertRunRunning(t, pn, scheduler.TaskActivity)
	close(release)
	assertPanelRunSucceeded(t, pn, scheduler.TaskActivity, calls)
}

// assertStarted 断言手动触发成功。
func assertStarted(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200 body=%s", rec.Code, rec.Body)
	}
}

// waitEntered 等上游收到请求（超时 Fatal）。
func waitEntered(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("activity batch did not reach upstream")
	}
}

// assertRunRunning 断言任务族处于本轮运行中且来源为面板。
func assertRunRunning(t *testing.T, pn *Panel, task string) {
	t.Helper()
	run := findRun(runsOf(t, pn), task)
	if run == nil || !run.Running || run.Trigger != scheduler.TriggerManual {
		t.Fatalf("running state=%+v want manual+running", run)
	}
}

// assertPanelRunSucceeded 等任务结束并断言摘要与上游调用次数。
func assertPanelRunSucceeded(t *testing.T, pn *Panel, task string, calls *int64) {
	t.Helper()
	run := waitPanelRunDone(t, pn, task)
	if run.Total != 1 || run.Success != 1 || run.Failure != 0 || run.FinishedAt.IsZero() {
		t.Fatalf("finished run=%+v want total=1 success=1", run)
	}
	if n := atomic.LoadInt64(calls); n != 1 {
		t.Errorf("upstream calls=%d want 1", n)
	}
}

// newBlockingPanel 构造活跃上报被阻塞的面板；返回 entered/release/calls。
func newBlockingPanel(t *testing.T) (*Panel, chan struct{}, chan struct{}, *int64) {
	t.Helper()
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
	sch := scheduler.New(scheduler.Config{Pool: p, Upstream: up})
	pn := New(Config{Version: "test", APIKey: "k", Pool: p, Upstream: up, Scheduler: sch, Live: livecfg.New(livecfg.Snapshot{})})
	return pn, entered, release, &calls
}

// assertConflictRunning 断言 409 + started=false + running=true。
func assertConflictRunning(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusConflict {
		t.Fatalf("code=%d want 409 body=%s", rec.Code, rec.Body)
	}
	var conflict struct {
		Started bool `json:"started"`
		Running bool `json:"running"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &conflict); err != nil {
		t.Fatal(err)
	}
	if conflict.Started || !conflict.Running {
		t.Fatalf("conflict=%+v want started=false running=true", conflict)
	}
}

// waitPanelRunDone 轮询直到任务族结束，返回最近一轮状态。
func waitPanelRunDone(t *testing.T, pn *Panel, task string) *scheduler.TaskRun {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if r := findRun(runsOf(t, pn), task); r != nil && !r.Running {
			return r
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("task %s did not finish", task)
	return nil
}

// TestPanelBlackcatAndStreakEndpoints 新增夜猫子/连登手动入口可用（issue #112）。
func TestPanelBlackcatAndStreakEndpoints(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
	}))
	t.Cleanup(srv.Close)
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999, Domain: "workbuddy.cn"})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	sch := scheduler.New(scheduler.Config{Pool: p, Upstream: up})
	pn := New(Config{Version: "test", APIKey: "k", Pool: p, Upstream: up, Scheduler: sch, Live: livecfg.New(livecfg.Snapshot{})})

	if rec := postAuthed(pn, "/panel/api/blackcat_all"); rec.Code != http.StatusOK {
		t.Fatalf("blackcat code=%d body=%s", rec.Code, rec.Body)
	}
	if rec := postAuthed(pn, "/panel/api/streak_bonus_all"); rec.Code != http.StatusOK {
		t.Fatalf("streak code=%d body=%s", rec.Code, rec.Body)
	}
	// 状态最终会出现这两个任务族（异步执行；轮询到结束）。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		runs := runsOf(t, pn)
		b, s := findRun(runs, scheduler.TaskBlackcat), findRun(runs, scheduler.TaskStreak)
		if b != nil && s != nil && !b.Running && !s.Running {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("blackcat/streak runs did not finish: %+v", runsOf(t, pn))
}
