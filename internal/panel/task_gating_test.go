package panel

import (
	"encoding/base64"
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

	account := &auth.Auth{UID: uid, AccessToken: "at-" + uid, RefreshToken: "rt-" + uid, ExpiresAt: 9999999999}

	switch kind {
	case "global":
		if _, err := auth.BackfillRealmFor(account, "global"); err != nil {
			t.Fatal(err)
		}

		account.Domain = "www.workbuddy.ai"
	case "enterprise":
		account.EnterpriseID = "ent-" + uid
		account.Domain = "workbuddy.cn"
	case "enterprise_switch":
		// 企业切换会话：可用（无 enterpriseId），但 JWT 带 token_source=enterprise_switch。
		account.AccessToken = switchSessionToken(uid)
		account.Domain = "workbuddy.cn"
	default:
		account.Domain = "workbuddy.cn"
	}

	return account
}

// switchSessionToken 造含 token_source=enterprise_switch 的假 JWT（只解 payload、不验签）。
func switchSessionToken(uid string) string {
	payload := base64.RawURLEncoding.EncodeToString(
		[]byte(`{"sub":"` + uid + `","token_source":"enterprise_switch"}`))

	return "eyJhbGciOiJIUzI1NiJ9." + payload + ".sig"
}

// newGatingPanel 构造带调用计数的面板（cn / global / 企业版 / 企业切换会话各一个）。
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
	client := &upstream.Client{
		HTTP:              srv.Client(),
		ChatBaseCN:        srv.URL,
		BillingBaseCN:     srv.URL,
		ChatBaseGlobal:    srv.URL,
		BillingBaseGlobal: srv.URL,
	}

	p := pool.New("")
	for _, kind := range []string{"cn", "global", "enterprise", "enterprise_switch"} {
		p.Add(gatingAccount(t, kind+"-u", kind))
	}

	return &Panel{cfg: Config{Pool: p, Upstream: client, AuthDir: t.TempDir()}}, &calls
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
	t.Parallel()

	for _, kind := range []string{"global", "enterprise"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			panel, calls := newGatingPanel(t)

			// 查询类：200 + skipped 空列表（前端按空态展示，不必按错误处理）。
			before := atomic.LoadInt64(calls)
			rec := invoke(panel.accountTasks, kind+"-u", "")
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
				{"accept", panel.accountTaskAccept},
				{"claim", panel.accountTaskClaim},
				{"auto", panel.accountTaskAuto},
				{"auto_all", panel.accountTaskAutoAll},
			}
			for _, act := range actions {
				t.Run(act.name, func(t *testing.T) {
					t.Parallel()

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
	t.Parallel()
	assertTaskEndpointsAllowed(t, "cn-u")
}

// TestPanelTaskEndpointsAllowEnterpriseSwitch token_source=enterprise_switch 且
// enterpriseId 为空的个人号按普通个人号放行（issue #275）。
func TestPanelTaskEndpointsAllowEnterpriseSwitch(t *testing.T) {
	t.Parallel()
	assertTaskEndpointsAllowed(t, "enterprise_switch-u")
}

func assertTaskEndpointsAllowed(t *testing.T, uid string) {
	t.Helper()

	panel, calls := newGatingPanel(t)
	for _, act := range []struct {
		name string
		h    func(http.ResponseWriter, *http.Request)
	}{
		{"accept", panel.accountTaskAccept},
		{"claim", panel.accountTaskClaim},
		{"auto", panel.accountTaskAuto},
	} {
		t.Run(act.name, func(t *testing.T) {
			t.Parallel()

			rec := invoke(act.h, uid, "")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("code=%d want 400（请求体校验，说明已通过门控）body=%s", rec.Code, rec.Body)
			}
		})
	}

	before := atomic.LoadInt64(calls)

	if rec := invoke(panel.accountTasks, uid, ""); rec.Code != http.StatusOK {
		t.Fatalf("tasks code=%d body=%s", rec.Code, rec.Body)
	}

	if atomic.LoadInt64(calls) == before {
		t.Errorf("个人号任务查询应调用上游列表")
	}
}

// TestPanelCheckinGating 企业版单号签到不发起签到调用，但仍刷新额度；
// CN 与 global 号照常签到（global 与调度器同口径，issue #108 的 global 门控
// 只适用于成长/任务体系，不适用于签到）。
func TestPanelCheckinGating(t *testing.T) {
	t.Parallel()
	t.Run("global", func(t *testing.T) {
		t.Parallel()
		assertCheckinAllowed(t, "global")
	})
	t.Run("enterprise", func(t *testing.T) {
		t.Parallel()
		assertCheckinSkipped(t, "enterprise")
	})
	t.Run("cn", func(t *testing.T) {
		t.Parallel()
		assertCheckinAllowed(t, "cn")
	})
}

// assertCheckinSkipped 断言企业版签到被跳过：checkin_done=false、
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

// assertCheckinAllowed 断言账号不被跳过，且签到 + 余额各发起一次上游调用。
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
		t.Errorf("账号不得被跳过: %s", rec.Body)
	}

	if n := atomic.LoadInt64(calls) - before; n < 2 {
		t.Errorf("签到应发起签到 + 余额两次上游调用，got %d", n)
	}
}
