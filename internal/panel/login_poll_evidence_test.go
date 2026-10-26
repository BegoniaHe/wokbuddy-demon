package panel

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/logincode"
)

// TestLoginPollPendingLogDedup pending 的上游 code/msg 只在首次或变化时记录，
// 相同结果不重复刷屏；日志含 state 前缀与 code/msg。
//
//nolint:paralleltest // 串行替换全局 loginHTTP 与 logger，避免测试间互相干扰。
func TestLoginPollPendingLogDedup(t *testing.T) {
	var buf bytes.Buffer

	prevOutput := log.Writer()
	prevFlags := log.Flags()

	log.SetOutput(&buf)
	log.SetFlags(0)

	t.Cleanup(func() {
		log.SetOutput(prevOutput)
		log.SetFlags(prevFlags)
	})

	msg := "login ing"

	replaceLoginHTTP(t, func(*http.Request) (*http.Response, error) {
		return loginResponse(
			http.StatusOK,
			`{"code":`+strconv.Itoa(logincode.RetryFetchToken)+`,"msg":"`+msg+`"}`,
		), nil
	})

	panel := loginTestPanel(t)

	poll := func() {
		rec := httptest.NewRecorder()
		panel.loginPoll(rec, httptest.NewRequest(http.MethodGet, "/panel/api/login/poll?state=state-1", nil))

		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"done":false`) {
			t.Fatalf("pending poll status=%d body=%s", rec.Code, rec.Body.String())
		}
	}

	poll() // 首次记录
	poll() // 相同结果不记录

	msg = "login ing (state refreshed)"

	poll() // 变化后再次记录

	out := buf.String()
	if got := strings.Count(out, "panel: 登录等待中"); got != 2 {
		t.Fatalf("pending log count=%d want 2: %s", got, out)
	}

	for _, want := range []string{"state-1...", "code=" + strconv.Itoa(logincode.RetryFetchToken), `msg="login ing"`, `msg="login ing (state refreshed)"`} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q: %s", want, out)
		}
	}
}
