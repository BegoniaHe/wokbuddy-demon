package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// gatingAccount 构造三类账号：cn 个人号 / global 国际版 / 企业版。
func gatingAccount(t *testing.T, uid, kind string) *auth.Auth {
	t.Helper()
	a := &auth.Auth{UID: uid, AccessToken: "at-" + uid, RefreshToken: "rt-" + uid, ExpiresAt: 9999999999}
	switch kind {
	case "global":
		if _, err := auth.BackfillRealmFor(a, "global"); err != nil {
			t.Fatal(err)
		}
		a.Domain = "www.workbuddy.ai"
	case "enterprise":
		a.EnterpriseID = "ent-" + uid
		a.Domain = "workbuddy.cn"
	default:
		a.Domain = "workbuddy.cn"
	}
	return a
}

// newGatingPanel 构造带调用计数的面板（三类账号各一个）。
func newGatingPanel(t *testing.T) (*Panel, *int64) {
	t.Helper()
	old := auth.GlobalEnabled()
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(old) })

	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
	}))
	t.Cleanup(srv.Close)
	up := &upstream.Client{
		HTTP:              srv.Client(),
		ChatBaseCN:        srv.URL,
		BillingBaseCN:     srv.URL,
		ChatBaseGlobal:    srv.URL,
		BillingBaseGlobal: srv.URL,
	}
	p := pool.New("")
	for _, kind := range []string{"cn", "global", "enterprise"} {
		p.Add(gatingAccount(t, kind+"-u", kind))
	}
	return &Panel{cfg: Config{Pool: p, Upstream: up, AuthDir: t.TempDir()}}, &calls
}

// invoke 以 uid 路径参数调用 handler。
func invoke(h func(http.ResponseWriter, *http.Request), uid, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.SetPathValue("uid", uid)
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// assertSkipped 断言响应是「跳过」形态且期间无上游调用。
func assertSkipped(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, before, after int64) {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("code=%d want %d body=%s", rec.Code, wantStatus, rec.Body)
	}
	if n := after - before; n != 0 {
		t.Errorf("upstream calls=%d want 0 (denied before any call)", n)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["skipped"] != true {
		t.Errorf("resp=%s want skipped=true", rec.Body)
	}
}

// TestPanelTaskEndpointsDenyGlobalAndEnterprise 单号任务入口对 global/企业版统一
// 跳过，且不发起任何上游调用（issue #108）。
func TestPanelTaskEndpointsDenyGlobalAndEnterprise(t *testing.T) {
	for _, kind := range []string{"global", "enterprise"} {
		t.Run(kind, func(t *testing.T) {
			pn, calls := newGatingPanel(t)

			// 查询类：200 + skipped 空列表（前端按空态展示，不必按错误处理）。
			before := atomic.LoadInt64(calls)
			rec := invoke(pn.accountTasks, kind+"-u", "")
			assertSkipped(t, rec, http.StatusOK, before, atomic.LoadInt64(calls))
			var listResp struct {
				Skipped bool  `json:"skipped"`
				Tasks   []any `json:"tasks"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &listResp); err != nil {
				t.Fatal(err)
			}
			if !listResp.Skipped || len(listResp.Tasks) != 0 {
				t.Errorf("resp=%s want skipped=true + empty tasks", rec.Body)
			}

			// 动作类：409 + skipped，不发上游调用。
			actions := []struct {
				name string
				h    func(http.ResponseWriter, *http.Request)
			}{
				{"accept", pn.accountTaskAccept},
				{"claim", pn.accountTaskClaim},
				{"auto", pn.accountTaskAuto},
				{"auto_all", pn.accountTaskAutoAll},
			}
			for _, act := range actions {
				t.Run(act.name, func(t *testing.T) {
					before := atomic.LoadInt64(calls)
					rec := invoke(act.h, kind+"-u", "")
					assertSkipped(t, rec, http.StatusConflict, before, atomic.LoadInt64(calls))
				})
			}
		})
	}
}

// TestPanelTaskEndpointsAllowCN CN 个人号不受门控影响：请求到达既有校验逻辑
// （缺 task_code 照常 400），且任务查询照常调用上游（issue #108）。
func TestPanelTaskEndpointsAllowCN(t *testing.T) {
	pn, calls := newGatingPanel(t)
	for _, act := range []struct {
		name string
		h    func(http.ResponseWriter, *http.Request)
	}{
		{"accept", pn.accountTaskAccept},
		{"claim", pn.accountTaskClaim},
		{"auto", pn.accountTaskAuto},
	} {
		t.Run(act.name, func(t *testing.T) {
			rec := invoke(act.h, "cn-u", "")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("code=%d want 400（请求体校验，说明已通过门控）body=%s", rec.Code, rec.Body)
			}
		})
	}
	before := atomic.LoadInt64(calls)
	if rec := invoke(pn.accountTasks, "cn-u", ""); rec.Code != http.StatusOK {
		t.Fatalf("tasks code=%d body=%s", rec.Code, rec.Body)
	}
	if atomic.LoadInt64(calls) == before {
		t.Errorf("CN 任务查询应调用上游列表")
	}
}

// TestPanelCheckinGating global/企业版单号签到不发起签到调用，但仍刷新额度；
// CN 号照常签到（issue #108）。
func TestPanelCheckinGating(t *testing.T) {
	t.Run("global", func(t *testing.T) { assertCheckinSkipped(t, "global") })
	t.Run("enterprise", func(t *testing.T) { assertCheckinSkipped(t, "enterprise") })
	t.Run("cn", func(t *testing.T) { assertCheckinAllowed(t, "cn") })
}

// assertCheckinSkipped 断言 global/企业版签到被跳过：checkin_done=false、
// 有可读原因，但额度刷新照常发起上游调用。
func assertCheckinSkipped(t *testing.T, kind string) {
	t.Helper()
	pn, calls := newGatingPanel(t)
	before := atomic.LoadInt64(calls)
	rec := invoke(pn.accountCheckin, kind+"-u", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200 body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["skipped"] != true {
		t.Errorf("resp=%s want skipped=true", rec.Body)
	}
	if resp["checkin_done"] != false {
		t.Errorf("checkin_done=%v want false（未发起签到）", resp["checkin_done"])
	}
	if reason, _ := resp["reason"].(string); reason == "" {
		t.Errorf("resp=%s want readable reason", rec.Body)
	}
	if atomic.LoadInt64(calls) == before {
		t.Errorf("额度刷新应调用上游（got 0 calls）")
	}
}

// assertCheckinAllowed 断言 CN 号不被跳过，且签到 + 余额各发起一次上游调用。
func assertCheckinAllowed(t *testing.T, kind string) {
	t.Helper()
	pn, calls := newGatingPanel(t)
	before := atomic.LoadInt64(calls)
	rec := invoke(pn.accountCheckin, kind+"-u", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["skipped"] == true {
		t.Errorf("CN 号不得被跳过: %s", rec.Body)
	}
	if n := atomic.LoadInt64(calls) - before; n < 2 {
		t.Errorf("CN 签到应发起签到 + 余额两次上游调用，got %d", n)
	}
}
