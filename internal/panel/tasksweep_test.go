// tasksweep_test.go 钉住任务补领清扫（上游同步窗口 issue #196）：
// 只补领「已达标未领」、双口径去重、第二轮幂等零请求、
// global/企业版账号零上游请求、忙账号整轮跳过。
package panel

import (
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// sweepUpstream 假上游：按「是否为 mp 请求」返回不同任务列表，并统计 claim 次数。
type sweepUpstream struct {
	mu sync.Mutex
	// claimed 已领取的 task_code 集合，按账号 token 分桶（上游状态本就是账号级，
	// 多账号并发清扫时共享一份会互相污染列表口径）。第二轮据此返回 claimed 形态。
	claimed map[string]map[string]bool
	// defaultTasks / mpTasks 任务模板（target/current）。
	defaultTasks []string
	mpTasks      []string
	claims       map[string]int
	listCalls    int
}

func (u *sweepUpstream) snapshotClaims() map[string]int {
	u.mu.Lock()
	defer u.mu.Unlock()

	out := make(map[string]int, len(u.claims))
	maps.Copy(out, u.claims)

	return out
}

// server 起 httptest 并返回 panel。
func (u *sweepUpstream) panel(t *testing.T, accounts ...*auth.Auth) *Panel {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		defer u.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")

		if strings.HasSuffix(r.URL.Path, "/claim") {
			code := taskCodeFromClaimPath(r.URL.Path)
			u.claims[code]++
			u.claimedFor(r)[code] = true
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"credit":10,"energy":1}}`))

			return
		}

		u.listCalls++

		codes := u.defaultTasks
		if r.Header.Get("X-Client-Platform") == "miniprogram" {
			codes = u.mpTasks
		}

		_, _ = w.Write([]byte(taskListJSON(codes, u.claimedFor(r))))
	}))
	t.Cleanup(srv.Close)

	client := &upstream.Client{
		HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL,
		WebBaseCN: srv.URL, ChatBaseGlobal: srv.URL, BillingBaseGlobal: srv.URL,
		RetryDelay: 0,
	}

	state := pool.New("")
	for _, acct := range accounts {
		state.Add(acct)
	}

	return &Panel{cfg: Config{Pool: state, Upstream: client}}
}

// claimedFor 返回该请求所属账号的已领取集合（按 Authorization 分桶）。
func (u *sweepUpstream) claimedFor(r *http.Request) map[string]bool {
	key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")

	set := u.claimed[key]
	if set == nil {
		set = map[string]bool{}
		u.claimed[key] = set
	}

	return set
}

// taskCodeFromClaimPath 从 .../tasks/<code>/claim 取 code。
func taskCodeFromClaimPath(path string) string {
	parts := strings.Split(strings.TrimSuffix(path, "/claim"), "/")
	if len(parts) == 0 {
		return ""
	}

	return parts[len(parts)-1]
}

// taskListJSON 生成任务列表响应：claimed 集合里的任务用 accept_status=claimed。
func taskListJSON(codes []string, claimed map[string]bool) string {
	items := make([]string, 0, len(codes))

	for _, code := range codes {
		status := "accepted"
		if claimed[code] {
			status = "claimed"
		}

		items = append(items, `{"task_code":"`+code+`","target":1,"current":1,"accept_status":"`+status+`"}`)
	}

	return `{"code":0,"msg":"ok","data":{"tasks":[` + strings.Join(items, ",") + `]}}`
}

func newSweepUpstream() *sweepUpstream {
	return &sweepUpstream{
		claimed: map[string]map[string]bool{},
		claims:  map[string]int{},
	}
}

// TestTaskSweepClaimsClaimableOnce 只补领「达标未领」，双口径重复列出也只领一次。
func TestTaskSweepClaimsClaimableOnce(t *testing.T) {
	t.Parallel()

	fake := newSweepUpstream()
	fake.defaultTasks = []string{"Model_chat_GLM5.2", "chat_5"}
	fake.mpTasks = []string{"Model_chat_GLM5.2", "Sequential_Tasks_1"} // 与默认口径重复一项

	panel := fake.panel(t, &auth.Auth{UID: "u1", AccessToken: "at-u1", ExpiresAt: 9999999999})

	stats := panel.RunTaskSweep(context.Background())

	if stats.Claimed != 3 {
		t.Fatalf("claimed=%d want 3（去重后 3 个任务）", stats.Claimed)
	}

	claims := fake.snapshotClaims()
	for code, n := range claims {
		if n != 1 {
			t.Errorf("%s claim 次数=%d want 1（双口径重复项只领一次）", code, n)
		}
	}

	if len(claims) != 3 {
		t.Errorf("claim 覆盖 %d 个任务 want 3: %v", len(claims), claims)
	}
}

// TestTaskSweepSecondRoundIdempotent 第二轮零 claim（上游已回 claimed）。
func TestTaskSweepSecondRoundIdempotent(t *testing.T) {
	t.Parallel()

	fake := newSweepUpstream()
	fake.defaultTasks = []string{"Model_chat_GLM5.2"}

	panel := fake.panel(t, &auth.Auth{UID: "u1", AccessToken: "at-u1", ExpiresAt: 9999999999})

	if stats := panel.RunTaskSweep(context.Background()); stats.Claimed != 1 {
		t.Fatalf("第一轮 claimed=%d want 1", stats.Claimed)
	}

	second := panel.RunTaskSweep(context.Background())
	if second.Claimed != 0 {
		t.Fatalf("第二轮 claimed=%d want 0（幂等）", second.Claimed)
	}

	if got := fake.snapshotClaims()["Model_chat_GLM5.2"]; got != 1 {
		t.Errorf("两轮累计 claim=%d want 1", got)
	}
}

// TestTaskSweepSkipsNonCNAccounts global 与企业版账号零上游请求。
func TestTaskSweepSkipsNonCNAccounts(t *testing.T) {
	t.Parallel()

	fake := newSweepUpstream()
	fake.defaultTasks = []string{"Model_chat_GLM5.2"}

	global := &auth.Auth{UID: "g1", AccessToken: "at-g1", ExpiresAt: 9999999999}
	if _, err := auth.BackfillRealmFor(global, "global"); err != nil {
		t.Fatalf("backfill realm: %v", err)
	}

	enterprise := &auth.Auth{UID: "e1", AccessToken: "at-e1", ExpiresAt: 9999999999, EnterpriseID: "ent-1"}

	panel := fake.panel(t, global, enterprise)

	stats := panel.RunTaskSweep(context.Background())

	if stats.Accounts != 0 || stats.Scanned != 0 || stats.Claimed != 0 {
		t.Fatalf("global/企业版账号不应参与清扫，stats=%+v", stats)
	}

	if fake.listCalls != 0 {
		t.Errorf("不应有上游列表请求，listCalls=%d", fake.listCalls)
	}

	if len(fake.snapshotClaims()) != 0 {
		t.Errorf("不应有 claim 请求：%v", fake.snapshotClaims())
	}
}

// TestTaskSweepSkipsBusyAccount 账号锁被占（手动任务在跑）时整轮跳过。
func TestTaskSweepSkipsBusyAccount(t *testing.T) {
	t.Parallel()

	fake := newSweepUpstream()
	fake.defaultTasks = []string{"Model_chat_GLM5.2"}

	panel := fake.panel(t, &auth.Auth{UID: "u1", AccessToken: "at-u1", ExpiresAt: 9999999999})

	// 模拟手动任务持锁。
	if !panel.tryLockAccount("u1") {
		t.Fatal("预取锁失败")
	}

	stats := panel.RunTaskSweep(context.Background())
	panel.unlockAccount("u1")

	if stats.Busy != 1 || stats.Claimed != 0 {
		t.Fatalf("忙账号应整轮跳过，stats=%+v", stats)
	}

	if fake.listCalls != 0 {
		t.Errorf("忙账号不应拉任务列表，listCalls=%d", fake.listCalls)
	}
}

// TestTaskSweepAggregatesStats 多账号并发汇总（积分/能量累计）。
func TestTaskSweepAggregatesStats(t *testing.T) {
	t.Parallel()

	fake := newSweepUpstream()
	fake.defaultTasks = []string{"Model_chat_GLM5.2"}

	panel := fake.panel(t,
		&auth.Auth{UID: "u1", AccessToken: "at-u1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at-u2", ExpiresAt: 9999999999},
	)

	stats := panel.RunTaskSweep(context.Background())

	if stats.Accounts != 2 || stats.Scanned != 2 || stats.Claimed != 2 {
		t.Fatalf("stats=%+v want accounts=2 scanned=2 claimed=2", stats)
	}

	if stats.Credit != 20 || stats.Energy != 2 {
		t.Errorf("credit/energy=%d/%d want 20/2", stats.Credit, stats.Energy)
	}
}

// TestTaskSweepStopsOnCancel ctx 取消后不再派发新的逐号请求。
func TestTaskSweepStopsOnCancel(t *testing.T) {
	t.Parallel()

	fake := newSweepUpstream()
	fake.defaultTasks = []string{"Model_chat_GLM5.2"}

	panel := fake.panel(t, &auth.Auth{UID: "u1", AccessToken: "at-u1", ExpiresAt: 9999999999})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	stats := panel.RunTaskSweep(ctx)
	if stats.Accounts != 0 {
		t.Fatalf("已取消的 ctx 不应派发请求，stats=%+v", stats)
	}
}

// TestClaimableTasksPrefersClaimableDuplicate 两口径同名任务的状态瞬时不一致时，
// 默认口径的「不可领」条目不能把 mp 口径的「可领」条目吃掉（去重须先判可领）。
func TestClaimableTasksPrefersClaimableDuplicate(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		body := `{"code":0,"msg":"ok","data":{"tasks":[{"task_code":"Model_chat_GLM5.2","target":1,"current":0,"accept_status":"accepted"}]}}`
		if r.Header.Get("X-Client-Platform") == "miniprogram" {
			body = `{"code":0,"msg":"ok","data":{"tasks":[{"task_code":"Model_chat_GLM5.2","target":1,"current":1,"accept_status":"accepted"}]}}`
		}

		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	account := &auth.Auth{UID: "u1", AccessToken: "at-u1", ExpiresAt: 9999999999}
	state := pool.New("")
	state.Add(account)

	panel := &Panel{cfg: Config{Pool: state, Upstream: &upstream.Client{
		HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL,
	}}}

	tasks, err := panel.claimableTasks(account)
	if err != nil {
		t.Fatalf("claimableTasks: %v", err)
	}

	if len(tasks) != 1 || tasks[0].TaskCode != "Model_chat_GLM5.2" || !tasks[0].Claimable {
		t.Fatalf("应保留 mp 口径的可领条目，got %+v", tasks)
	}
}
