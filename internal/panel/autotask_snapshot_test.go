// autotask_snapshot_test.go 钉住 issue #429：一键完成复用执行前任务快照，
// 列表外任务零动作请求，并保留动作后异步回读及单口径失败降级。
package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

type snapshotUpstream struct {
	mu                 sync.Mutex
	defaultCalls       int
	mpCalls            int
	acceptCalls        int
	actionCalls        int
	claimCalls         int
	defaultFailures    int
	mpFailures         int
	defaultTasks       []string
	mpTasks            []string
	progressOnReadback int
	// acceptedCodes 非 nil 时模拟 accept 落账：列表先返回 not_accepted，
	// accept 请求后才转 accepted。
	acceptedCodes map[string]bool
	// targets/currents 按 code 覆盖列表下发的进度，用于构造 target=0 形态。
	targets  map[string]int64
	currents map[string]int64
}

func (u *snapshotUpstream) panel(t *testing.T) *Panel {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		defer u.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.URL.Path == "/v2/activity/growth/tasks" && r.Method == http.MethodGet:
			u.handleTaskList(w, r)
		case r.URL.Path == "/v2/activity/growth/tasks/accept":
			u.acceptCalls++
			u.handleAccept(r)

			_, _ = w.Write([]byte(`{"code":0,"msg":"ok"}`))
		case strings.HasSuffix(r.URL.Path, "/claim"):
			u.claimCalls++
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"credit":10,"energy":1}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	account := &auth.Auth{UID: "u1", AccessToken: "at-u1", ExpiresAt: 9999999999}
	state := pool.New("")
	state.Add(account)

	client := &upstream.Client{
		HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL,
		WebBaseCN: srv.URL, ChatBaseGlobal: srv.URL, BillingBaseGlobal: srv.URL,
		RetryDelay: 0,
	}

	return &Panel{cfg: Config{Pool: state, Upstream: client}}
}

func (u *snapshotUpstream) handleTaskList(w http.ResponseWriter, r *http.Request) {
	codes, fail := u.beginTaskList(r)
	if fail {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"code":500,"msg":"list failed"}`))

		return
	}

	items := make([]string, 0, len(codes))
	for _, code := range codes {
		// 首次执行前读取返回未完成；动作后读取返回达标，模拟异步计分。
		current := int64(0)
		if u.progressOnReadback > 0 && u.defaultCalls+u.mpCalls > u.progressOnReadback {
			current = 1
		}

		if v, ok := u.currents[code]; ok {
			current = v
		}

		target := int64(1)
		if v, ok := u.targets[code]; ok {
			target = v
		}

		status := "accepted"
		if u.acceptedCodes != nil && !u.acceptedCodes[code] {
			status = "not_accepted"
		}

		items = append(items, `{"task_code":"`+code+`","target":`+
			strconv.FormatInt(target, 10)+`,"current":`+strconv.FormatInt(current, 10)+
			`,"accept_status":"`+status+`"}`)
	}

	_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"tasks":[` + strings.Join(items, ",") + `]}}`))
}

// handleAccept 把本次 accept 的 code 记为已接受（模拟上游状态流转）。
func (u *snapshotUpstream) handleAccept(r *http.Request) {
	if u.acceptedCodes == nil {
		return
	}

	var body struct {
		TaskCodes []string `json:"task_codes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return
	}

	for _, code := range body.TaskCodes {
		u.acceptedCodes[code] = true
	}
}

func (u *snapshotUpstream) beginTaskList(r *http.Request) ([]string, bool) {
	if r.Header.Get("X-Client-Platform") == "miniprogram" {
		u.mpCalls++

		if u.mpFailures > 0 {
			u.mpFailures--

			return u.mpTasks, true
		}

		return u.mpTasks, false
	}

	u.defaultCalls++

	if u.defaultFailures > 0 {
		u.defaultFailures--

		return u.defaultTasks, true
	}

	return u.defaultTasks, false
}

func (u *snapshotUpstream) counts() (defaultCalls, mpCalls, acceptCalls, actionCalls, claimCalls int) {
	u.mu.Lock()
	defer u.mu.Unlock()

	return u.defaultCalls, u.mpCalls, u.acceptCalls, u.actionCalls, u.claimCalls
}

func snapshotAction(code string, called *snapshotUpstream) autoAction {
	return autoAction{
		TaskCode: code,
		Desc:     "测试动作",
		run: func(*Panel, *auth.Auth) (string, error) {
			called.mu.Lock()
			called.actionCalls++
			called.mu.Unlock()

			return "动作完成", nil
		},
	}
}

func snapshotActionWithTask(
	code string, called *snapshotUpstream, seen *upstream.Task,
) autoAction {
	return autoAction{
		TaskCode: code,
		Desc:     "测试快照动作",
		run: func(*Panel, *auth.Auth) (string, error) {
			panic("快照命中时不得调用 run")
		},
		runWithTask: func(_ *Panel, _ *auth.Auth, before *upstream.Task) (string, error) {
			called.mu.Lock()
			called.actionCalls++
			called.mu.Unlock()

			*seen = *before

			return "动作完成", nil
		},
	}
}

func accountForSnapshot(t *testing.T, p *Panel) *auth.Auth {
	t.Helper()

	account := p.cfg.Pool.AuthByUID("u1")
	if account == nil {
		t.Fatal("account missing")
	}

	return account
}

// TestRunAutoAllReusesTaskSnapshot 执行前默认/mp 各只读一次；列表外动作不执行；
// 列表内动作完成后仍重新回读。
func TestRunAutoAllReusesTaskSnapshot(t *testing.T) {
	t.Parallel()

	upstreamStub := &snapshotUpstream{
		defaultTasks:       []string{"chat_5"},
		mpTasks:            []string{"Sequential_Tasks_1"},
		progressOnReadback: 2,
	}
	panel := upstreamStub.panel(t)

	// 找真实动作会触发网络行为链；用替身动作只验证快照分发和回读次数。
	actions := []autoAction{
		snapshotAction("chat_5", upstreamStub),
		snapshotAction("missing_code", upstreamStub),
		snapshotAction("Sequential_Tasks_1", upstreamStub),
	}
	results := panel.runAutoAllActions(accountForSnapshot(t, panel), actions)

	if len(results) != 3 {
		t.Fatalf("results=%d want 3", len(results))
	}

	if results[1]["status"] != "skipped" {
		t.Errorf("列表外任务 status=%v want skipped", results[1]["status"])
	}

	defaultCalls, mpCalls, acceptCalls, actionCalls, claimCalls := upstreamStub.counts()
	if defaultCalls != 2 || mpCalls != 2 {
		t.Fatalf("列表 GET default/mp=%d/%d want 2/2（执行前 1 次 + 动作后回读 1 次）",
			defaultCalls, mpCalls)
	}

	if acceptCalls != 0 {
		t.Errorf("已接受任务不应 accept，calls=%d", acceptCalls)
	}

	if actionCalls != 2 {
		t.Errorf("动作调用=%d want 2（列表外 code 不得执行）", actionCalls)
	}

	if claimCalls != 2 {
		t.Errorf("动作后两个达标任务各领取一次，claims=%d", claimCalls)
	}
}

// TestRunAutoAllSnapshotDefaultFailureFallsBack 默认列表失败时保留逐项查询；
// 成功读到 mp 的 mp 专属任务仍复用快照。
func TestRunAutoAllSnapshotDefaultFailureFallsBack(t *testing.T) {
	t.Parallel()

	upstreamStub := &snapshotUpstream{
		defaultFailures: 1,
		defaultTasks:    []string{"chat_5"},
		mpTasks:         []string{"Sequential_Tasks_1"},
	}
	panel := upstreamStub.panel(t)

	results := panel.runAutoAllActions(accountForSnapshot(t, panel), []autoAction{
		snapshotAction("chat_5", upstreamStub),
		snapshotAction("Sequential_Tasks_1", upstreamStub),
	})
	if len(results) != 2 {
		t.Fatalf("results=%d want 2", len(results))
	}

	// 默认口径首次失败后，逐项回退查询成功；mp 专属任务直接复用 mp 快照。
	defaultCalls, mpCalls, _, actionCalls, _ := upstreamStub.counts()
	if defaultCalls < 3 {
		t.Errorf("default GET=%d want >=3（执行前失败 + chat 回退查询 + 动作后回读）", defaultCalls)
	}

	if mpCalls != 2 {
		t.Errorf("mp GET=%d want 2（执行前 1 次 + 动作后回读 1 次）", mpCalls)
	}

	if actionCalls != 2 {
		t.Errorf("默认列表失败后两项仍应执行，actions=%d", actionCalls)
	}
}

// TestRunAutoAllSnapshotMPFailureFallsBack mp 列表失败时回退逐项查询 mp 任务。
func TestRunAutoAllSnapshotMPFailureFallsBack(t *testing.T) {
	t.Parallel()

	upstreamStub := &snapshotUpstream{
		defaultTasks: []string{"chat_5"},
		mpFailures:   1,
		mpTasks:      []string{"Sequential_Tasks_1"},
	}
	panel := upstreamStub.panel(t)

	results := panel.runAutoAllActions(accountForSnapshot(t, panel), []autoAction{
		snapshotAction("Sequential_Tasks_1", upstreamStub),
	})
	if len(results) != 1 {
		t.Fatalf("results=%d want 1", len(results))
	}

	defaultCalls, mpCalls, _, actionCalls, _ := upstreamStub.counts()
	if defaultCalls != 2 {
		t.Errorf("default GET=%d want 2（执行前 1 次 + 回退默认查询 1 次）", defaultCalls)
	}

	if mpCalls < 3 {
		t.Errorf("mp GET=%d want >=3（执行前失败 + 回退查询 + 动作后回读）", mpCalls)
	}

	if actionCalls != 1 {
		t.Errorf("mp 列表首次失败后仍应回退并执行，actions=%d", actionCalls)
	}
}

// TestRunAutoAllSnapshotPostActionReadback 动作前快照不冒充动作后状态：
// 返回进度与 claim 必须来自重新读取。
func TestRunAutoAllSnapshotPostActionReadback(t *testing.T) {
	t.Parallel()

	upstreamStub := &snapshotUpstream{
		defaultTasks:       []string{"chat_5"},
		progressOnReadback: 2,
	}
	panel := upstreamStub.panel(t)

	results := panel.runAutoAllActions(accountForSnapshot(t, panel), []autoAction{
		snapshotAction("chat_5", upstreamStub),
	})
	if len(results) != 1 {
		t.Fatalf("results=%d want 1", len(results))
	}

	if results[0]["progress_after"] != "1/1" {
		t.Errorf("progress_after=%v want 1/1（必须动作后回读）", results[0]["progress_after"])
	}

	defaultCalls, _, _, _, claimCalls := upstreamStub.counts()
	if defaultCalls != 2 {
		t.Errorf("default GET=%d want 2（快照一次 + 后置回读一次）", defaultCalls)
	}

	if claimCalls != 1 {
		t.Errorf("后置回读达标应 claim 一次，claims=%d", claimCalls)
	}
}

// TestRunAutoAllActionReusesSnapshotTask 动作执行器拿到与执行前检查相同的任务，
// 不再自行拉取任务列表。
func TestRunAutoAllActionReusesSnapshotTask(t *testing.T) {
	t.Parallel()

	upstreamStub := &snapshotUpstream{
		defaultTasks:       []string{"chat_5"},
		progressOnReadback: 2,
	}
	panel := upstreamStub.panel(t)

	var seen upstream.Task

	results := panel.runAutoAllActions(accountForSnapshot(t, panel), []autoAction{
		snapshotActionWithTask("chat_5", upstreamStub, &seen),
	})
	if len(results) != 1 || results[0]["status"] != "done" {
		t.Fatalf("results=%v want one done item", results)
	}

	if seen.TaskCode != "chat_5" || seen.Target != 1 || seen.AcceptStatus != "accepted" {
		t.Errorf("动作任务=%+v want 快照中的 chat_5", seen)
	}

	defaultCalls, _, _, actionCalls, _ := upstreamStub.counts()
	if defaultCalls != 2 {
		t.Errorf("default GET=%d want 2（快照 + 动作后回读）", defaultCalls)
	}

	if actionCalls != 1 {
		t.Errorf("动作调用=%d want 1", actionCalls)
	}
}

// TestRefreshAcceptedMPTaskReadsMissingTarget 快照 target=0 时按码回读真实目标；
// 覆盖批量 accept 前后的登记状态，动作侧不能沿用兜底目标。
func TestRefreshAcceptedMPTaskReadsMissingTarget(t *testing.T) {
	t.Parallel()

	upstreamStub := &snapshotUpstream{mpTasks: []string{"Sequential_Tasks_1"}}
	panel := upstreamStub.panel(t)
	account := accountForSnapshot(t, panel)

	for _, status := range []string{"accepted", "not_accepted", ""} {
		refreshed, err := panel.refreshAcceptedMPTask(
			account,
			"Sequential_Tasks_1",
			&upstream.Task{TaskCode: "Sequential_Tasks_1", AcceptStatus: status},
		)
		if err != nil {
			t.Fatalf("refreshAcceptedMPTask(%q): %v", status, err)
		}

		if refreshed == nil || refreshed.Target != 1 {
			t.Errorf("status=%q refreshed=%+v want target=1", status, refreshed)
		}
	}

	_, mpCalls, _, _, _ := upstreamStub.counts()
	if mpCalls != 3 {
		t.Errorf("mp GET=%d want 3", mpCalls)
	}
}

// TestRefreshAcceptedMPTaskKeepsKnownTarget 快照已有真实目标时不得额外读取。
func TestRefreshAcceptedMPTaskKeepsKnownTarget(t *testing.T) {
	t.Parallel()

	upstreamStub := &snapshotUpstream{mpTasks: []string{"Sequential_Tasks_1"}}
	panel := upstreamStub.panel(t)

	before := &upstream.Task{TaskCode: "Sequential_Tasks_1", Target: 5, AcceptStatus: "accepted"}

	refreshed, err := panel.refreshAcceptedMPTask(accountForSnapshot(t, panel), "Sequential_Tasks_1", before)
	if err != nil {
		t.Fatalf("refreshAcceptedMPTask: %v", err)
	}

	if refreshed != before {
		t.Errorf("refreshed=%+v want unchanged pointer", refreshed)
	}

	_, mpCalls, _, _, _ := upstreamStub.counts()
	if mpCalls != 0 {
		t.Errorf("mp GET=%d want 0", mpCalls)
	}
}

// TestRunAutoAllAcceptsSharedTaskOnce 默认列表接受后 mp 列表已是 accepted，
// 同一 code 不得被两套批量 accept 各提交一次。
func TestRunAutoAllAcceptsSharedTaskOnce(t *testing.T) {
	t.Parallel()

	upstreamStub := &snapshotUpstream{
		defaultTasks:  []string{"chat_5"},
		mpTasks:       []string{"chat_5"},
		acceptedCodes: map[string]bool{},
	}
	panel := upstreamStub.panel(t)

	results := panel.runAutoAllActions(accountForSnapshot(t, panel), nil)
	if len(results) != 1 || results[0]["status"] != "done" {
		t.Fatalf("results=%v want 一次批量接受结果", results)
	}

	_, _, acceptCalls, _, _ := upstreamStub.counts()
	if acceptCalls != 1 {
		t.Errorf("accept=%d want 1（默认批量接受后 mp 列表应已落账）", acceptCalls)
	}
}

// TestMPTaskBaseEntriesReadOnce 单任务/队列入口不得因快照包装多读一次
// mp 列表：target=0 时基础入口保留旧行为，只读一次。
func TestMPTaskBaseEntriesReadOnce(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		code string
		run  func(*Panel, *auth.Auth) (string, error)
	}{
		{
			name: "mini chat",
			code: "Sequential_Tasks_1",
			run: func(p *Panel, a *auth.Auth) (string, error) {
				return p.runMPMiniChatTask(a, "Sequential_Tasks_1", false)
			},
		},
		{
			name: "sequential event",
			code: "Sequential_Tasks_4",
			run: func(p *Panel, a *auth.Auth) (string, error) {
				return p.runSequentialEventTask(a, "Sequential_Tasks_4", func() error {
					panic("达标时不应上报判据事件")
				}, nil)
			},
		},
		{
			name: "mini expert",
			code: "Sequential_Tasks_2",
			run:  runMiniExpert,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			upstreamStub := &snapshotUpstream{
				mpTasks:  []string{testCase.code},
				targets:  map[string]int64{testCase.code: 0},
				currents: map[string]int64{testCase.code: 1},
			}
			panel := upstreamStub.panel(t)

			msg, err := testCase.run(panel, accountForSnapshot(t, panel))
			if err != nil {
				t.Fatalf("%s: %v", testCase.name, err)
			}

			if !strings.Contains(msg, "已领取") {
				t.Errorf("msg=%q want 已领取", msg)
			}

			_, mpCalls, _, _, _ := upstreamStub.counts()
			if mpCalls != 1 {
				t.Errorf("mp GET=%d want 1（基础入口不得额外 refresh）", mpCalls)
			}
		})
	}
}
