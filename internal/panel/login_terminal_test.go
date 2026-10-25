package panel

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/clientprofile"
	"github.com/BegoniaHe/wokbuddy-demon/internal/logincode"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

type loginRoundTripFunc func(*http.Request) (*http.Response, error)

func (f loginRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// replaceLoginHTTP 串行替换设备授权 client，测试结束还原。
func replaceLoginHTTP(t *testing.T, fn loginRoundTripFunc) {
	t.Helper()

	old := loginHTTP
	loginHTTP = &http.Client{Transport: fn}

	t.Cleanup(func() { loginHTTP = old })
}

func loginResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func loginTestPanel(t *testing.T) *Panel {
	t.Helper()

	profiles, err := clientprofile.LoadDir("../upstream/testdata/profile")
	if err != nil {
		t.Fatalf("load test profiles: %v", err)
	}

	return &Panel{
		cfg: Config{Upstream: &upstream.Client{Profile: profiles}},
		logins: map[string]loginSession{
			"state-1": {created: time.Now(), realm: "cn"},
		},
	}
}

// TestPollLoginTokenPendingAndTerminal pending 码返回 pending=true；许可终态直接返回
// 带上游 msg 的错误，不再当作等待登录。
//
//nolint:paralleltest // 串行替换全局 loginHTTP，避免测试间互相干扰
func TestPollLoginTokenPendingAndTerminal(t *testing.T) {
	cases := []struct {
		name        string
		code        int
		msg         string
		wantPending bool
	}{
		{"token pending", logincode.RetryFetchToken, "login ing", true},
		{"席位超限", logincode.LicenseSeatLimit, "许可席位已满", false},
		{"许可过期", logincode.LicenseExpired, "许可已过期", false},
		{"试用过期", logincode.TrialExpired, "试用已结束", false},
	}
	for _, item := range cases { //nolint:paralleltest // 同上，子测试串行
		t.Run(item.name, func(t *testing.T) {
			// 串行：共享 loginHTTP 与 profile 目录，避免并行替换互相干扰。
			replaceLoginHTTP(t, func(*http.Request) (*http.Response, error) {
				return loginResponse(http.StatusOK, `{"code":`+strconv.Itoa(item.code)+`,"msg":"`+item.msg+`"}`), nil
			})

			panel := loginTestPanel(t)

			_, pending, _, err := panel.pollLoginToken("cn", "", "state-1")
			if item.wantPending {
				if err != nil || !pending {
					t.Fatalf("pending=%v err=%v want pending", pending, err)
				}

				return
			}

			if pending {
				t.Fatal("终态不应返回 pending")
			}

			var codeErr *logincode.Error
			if !errors.As(err, &codeErr) || codeErr.Code != item.code {
				t.Fatalf("err=%v want code=%d", err, item.code)
			}

			if !strings.Contains(err.Error(), item.msg) {
				t.Errorf("err=%q 未透出上游 msg", err.Error())
			}
		})
	}
}

// TestWriteLoginPollErrorTerminal 终态返回 400 + 上游 msg；其他业务错误仍按
// 200 done=false 处理（前端继续轮询）。
//
//nolint:paralleltest // 串行替换全局 loginHTTP，避免测试间互相干扰
func TestWriteLoginPollErrorTerminal(t *testing.T) {
	rec := httptest.NewRecorder()
	writeLoginPollError(rec, &logincode.Error{Code: logincode.LicenseExpired, Msg: "许可已过期", Status: 400})

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("terminal status=%d want 400 body=%s", rec.Code, rec.Body.String())
	}

	if !strings.Contains(rec.Body.String(), "许可已过期") {
		t.Errorf("terminal body=%s 未包含上游 msg", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	writeLoginPollError(rec, &logincode.Error{Code: 9999, Msg: "临时错误", Status: 500})

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"done":false`) {
		t.Errorf("非终态 status=%d body=%s want 200 done=false", rec.Code, rec.Body.String())
	}
}

// TestLoginPollAccountPendingAndTerminal 12151 继续返回 200 done=false；许可终态
// 返回非 200 并透出上游 msg。
//
//nolint:paralleltest // 串行替换全局 loginHTTP，避免测试间互相干扰
func TestLoginPollAccountPendingAndTerminal(t *testing.T) {
	cases := []struct {
		name        string
		code        int
		msg         string
		wantStatus  int
		wantPending bool
	}{
		{"account pending", logincode.RetryFetchAccount, "account ing", http.StatusOK, true},
		{"席位超限", logincode.LicenseSeatLimit, "许可席位已满", http.StatusBadRequest, false},
	}
	for _, item := range cases { //nolint:paralleltest // 同上，子测试串行
		t.Run(item.name, func(t *testing.T) {
			replaceLoginHTTP(t, func(r *http.Request) (*http.Response, error) {
				if strings.Contains(r.URL.Path, "/auth/token") {
					return loginResponse(http.StatusOK,
						`{"code":0,"data":{"accessToken":"at","refreshToken":"rt","expiresIn":3600}}`), nil
				}

				return loginResponse(http.StatusOK,
					`{"code":`+strconv.Itoa(item.code)+`,"msg":"`+item.msg+`"}`), nil
			})

			panel := loginTestPanel(t)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/panel/api/login/poll?state=state-1", nil)

			panel.loginPoll(rec, req)

			if rec.Code != item.wantStatus {
				t.Fatalf("status=%d want %d body=%s", rec.Code, item.wantStatus, rec.Body.String())
			}

			hasDone := strings.Contains(rec.Body.String(), `"done":false`)
			if hasDone != item.wantPending {
				t.Errorf("body=%s done=false=%v want %v", rec.Body.String(), hasDone, item.wantPending)
			}

			if !item.wantPending && item.wantStatus != http.StatusOK && !strings.Contains(rec.Body.String(), item.msg) {
				t.Errorf("body=%s 未透出上游 msg %q", rec.Body.String(), item.msg)
			}

			// pending 继续保留会话；终态前端已停止轮询，必须收敛会话。
			if _, known := panel.lookupLoginSession("state-1"); known != item.wantPending {
				t.Errorf("session known=%v want %v", known, item.wantPending)
			}
		})
	}
}

// TestLoginPollTokenTerminalDropsSession token 端终态同样要收敛会话：前端收到
// 非 200 后停止轮询，残留条目不会再被 TTL 查询清掉。
//
//nolint:paralleltest // 串行替换全局 loginHTTP，避免测试间互相干扰
func TestLoginPollTokenTerminalDropsSession(t *testing.T) {
	replaceLoginHTTP(t, func(*http.Request) (*http.Response, error) {
		return loginResponse(http.StatusOK,
			`{"code":`+strconv.Itoa(logincode.LicenseExpired)+`,"msg":"许可已过期"}`), nil
	})

	panel := loginTestPanel(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/panel/api/login/poll?state=state-1", nil)

	panel.loginPoll(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400 body=%s", rec.Code, rec.Body.String())
	}

	if _, known := panel.lookupLoginSession("state-1"); known {
		t.Error("终态失败后会话应被移除")
	}
}

// TestLoginPollPersistFailureDropsSession 凭证落盘失败同样返回非 200，前端会停止
// 轮询；会话必须在返回前移除，否则 p.logins 要等 TTL/下次 login start 才回收。
//
//nolint:paralleltest // 串行替换全局 loginHTTP，避免测试间互相干扰
func TestLoginPollPersistFailureDropsSession(t *testing.T) {
	replaceLoginHTTP(t, func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/auth/token") {
			return loginResponse(http.StatusOK,
				`{"code":0,"data":{"accessToken":"at","refreshToken":"rt","expiresIn":3600}}`), nil
		}

		// 含路径字符的 uid 会在 persistLoginAuth 落盘前被拒，模拟 account 成功但
		// 凭证无法保存的失败路径。
		return loginResponse(http.StatusOK,
			`{"code":0,"data":{"uid":"../evil","nickname":"evil"}}`), nil
	})

	panel := loginTestPanel(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/panel/api/login/poll?state=state-1", nil)

	panel.loginPoll(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d want 502 body=%s", rec.Code, rec.Body.String())
	}

	if _, known := panel.lookupLoginSession("state-1"); known {
		t.Error("落盘失败后会话应被移除")
	}
}
