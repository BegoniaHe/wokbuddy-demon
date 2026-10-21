// autotask_claim_test.go 钉住「一键完成」对「已达标未领」任务的补领行为
// （上游同步窗口 issue #195 / 上游 issue #174）：此前达标未领直接标 skipped，
// 奖励就一直躺在那里。
package panel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// claimTestPanel 造一个假上游：任务列表由 listBody 决定，claim 端点计数并可指定返回。
func claimTestPanel(t *testing.T, listBody string, claimStatus int, claimBody string) (*Panel, *atomic.Int32) {
	t.Helper()

	var claims atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if strings.HasSuffix(r.URL.Path, "/claim") {
			claims.Add(1)
			w.WriteHeader(claimStatus)
			_, _ = w.Write([]byte(claimBody))

			return
		}

		_, _ = w.Write([]byte(listBody))
	}))
	t.Cleanup(srv.Close)

	client := &upstream.Client{
		HTTP:              srv.Client(),
		ChatBaseCN:        srv.URL,
		BillingBaseCN:     srv.URL,
		WebBaseCN:         srv.URL,
		ChatBaseGlobal:    srv.URL,
		BillingBaseGlobal: srv.URL,
	}

	state := pool.New("")
	state.Add(&auth.Auth{UID: "u1", AccessToken: "at-u1"})

	return &Panel{cfg: Config{Pool: state, Upstream: client}}, &claims
}

// testAction 构造一个「行为链」替身：若被调用即说明不该走到执行分支。
func testAction(called *atomic.Int32) autoAction {
	return autoAction{
		TaskCode: "Model_chat_GLM5.2",
		Desc:     "测试任务",
		run: func(*Panel, *auth.Auth) (string, error) {
			called.Add(1)

			return "行为链已执行", nil
		},
	}
}

// TestAutoRunActionClaimsReachedUnclaimed 达成上限但未领取：先补领，再标 skipped，
// 且不执行行为链。
func TestAutoRunActionClaimsReachedUnclaimed(t *testing.T) {
	t.Parallel()

	panel, claims := claimTestPanel(t,
		`{"code":0,"msg":"ok","data":{"tasks":[{"task_code":"Model_chat_GLM5.2","target":1,"current":1,"accept_status":"accepted"}]}}`,
		http.StatusOK, `{"code":0,"msg":"ok","data":{"credit":100,"energy":5}}`)

	var ran atomic.Int32

	item := panel.runActionForTest(t, testAction(&ran))

	if got := claims.Load(); got != 1 {
		t.Fatalf("达标未领必须补领一次，claims=%d", got)
	}

	if ran.Load() != 0 {
		t.Error("已达标任务不应再执行行为链")
	}

	if item["claimed"] != true {
		t.Errorf("item.claimed=%v want true (%v)", item["claimed"], item)
	}

	if item["credit"] != int64(100) || item["energy"] != int64(5) {
		t.Errorf("credit/energy=%v/%v want 100/5", item["credit"], item["energy"])
	}

	if item["status"] != "skipped" {
		t.Errorf("status=%v want skipped", item["status"])
	}

	msg, _ := item["message"].(string)
	if !strings.Contains(msg, "已自动领奖") {
		t.Errorf("message 应说明已补领，got %q", msg)
	}
}

// TestAutoRunActionSkipsClaimed 已领取的任务不重复请求 claim。
func TestAutoRunActionSkipsClaimed(t *testing.T) {
	t.Parallel()

	panel, claims := claimTestPanel(t,
		`{"code":0,"msg":"ok","data":{"tasks":[{"task_code":"Model_chat_GLM5.2","target":1,"current":1,"accept_status":"claimed"}]}}`,
		http.StatusOK, `{"code":0,"msg":"ok","data":{"credit":100,"energy":5}}`)

	var ran atomic.Int32

	item := panel.runActionForTest(t, testAction(&ran))

	if got := claims.Load(); got != 0 {
		t.Fatalf("已领取任务不应再请求 claim，claims=%d", got)
	}

	if item["status"] != "skipped" || item["claimed"] == true {
		t.Errorf("item=%v want skipped 且未标 claimed", item)
	}
}

// TestAutoRunActionClaimFailureReportsError 补领失败时条目带可读 claim_error，
// 状态仍是 skipped（完成度事实不变）。
func TestAutoRunActionClaimFailureReportsError(t *testing.T) {
	t.Parallel()

	panel, claims := claimTestPanel(t,
		`{"code":0,"msg":"ok","data":{"tasks":[{"task_code":"Model_chat_GLM5.2","target":1,"current":1,"accept_status":"accepted"}]}}`,
		http.StatusInternalServerError, `{"code":500,"msg":"claim failed"}`)

	var ran atomic.Int32

	item := panel.runActionForTest(t, testAction(&ran))

	if got := claims.Load(); got != 1 {
		t.Fatalf("应尝试补领一次，claims=%d", got)
	}

	if _, ok := item["claim_error"]; !ok {
		t.Fatalf("补领失败必须带 claim_error，item=%v", item)
	}

	msg, _ := item["message"].(string)
	if !strings.Contains(msg, "领奖失败") {
		t.Errorf("message 应说明领奖失败，got %q", msg)
	}
}

// TestAutoRunActionRunsUnfinishedTask 未达标任务照旧执行行为链（零回归）。
func TestAutoRunActionRunsUnfinishedTask(t *testing.T) {
	t.Parallel()

	panel, claims := claimTestPanel(t,
		`{"code":0,"msg":"ok","data":{"tasks":[{"task_code":"Model_chat_GLM5.2","target":1,"current":0,"accept_status":"accepted"}]}}`,
		http.StatusOK, `{"code":0,"msg":"ok","data":{"credit":100,"energy":5}}`)

	var ran atomic.Int32

	item := panel.runActionForTest(t, testAction(&ran))

	if ran.Load() != 1 {
		t.Fatalf("未达标任务应执行行为链，ran=%d", ran.Load())
	}

	if item["status"] != "done" {
		t.Errorf("status=%v want done", item["status"])
	}

	// 行为链不改进度（假上游仍是 0/1）→ 不该抢跑补领。
	if got := claims.Load(); got != 0 {
		t.Errorf("未达标不应补领，claims=%d", got)
	}
}

// runActionForTest 调 autoRunAction（不改全局节流变量：并行用例共享进程级状态，
// 改写会引入数据竞争；单项 1s 节流对本测试可接受）。
func (p *Panel) runActionForTest(t *testing.T, act autoAction) map[string]any {
	t.Helper()

	state := p.cfg.Pool

	acct := state.AuthByUID("u1")
	if acct == nil {
		t.Fatal("account missing")
	}

	return p.autoRunAction(acct, act)
}
