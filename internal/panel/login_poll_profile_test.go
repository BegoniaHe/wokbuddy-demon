package panel

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestLoginPollProfileMissingReturns503 profile 缺失属于部署错误，poll 不能按
// 「等待登录」返回 200，否则前端会无限轮询。
func TestLoginPollProfileMissingReturns503(t *testing.T) {
	t.Parallel()

	panel := &Panel{
		logins: map[string]loginSession{
			"state-1": {created: time.Now(), realm: "cn"},
		},
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/panel/api/login/poll?state=state-1", nil)

	panel.loginPoll(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503 body=%s", rec.Code, rec.Body.String())
	}
}
