package panel

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestLoginPollRejectsExpiredState 超时 state 必须在 poll 入口回收并返回 404，
// 不得继续轮询上游（loginTTL 是授权 URL 的最长有效期）。
func TestLoginPollRejectsExpiredState(t *testing.T) {
	t.Parallel()

	const state = "expired-state"

	panel := &Panel{logins: map[string]loginSession{
		state: {created: time.Now().Add(-loginTTL - time.Second), realm: "cn"},
	}}

	req := httptest.NewRequest(http.MethodGet, "/panel/api/login/poll?state="+state, nil)
	rec := httptest.NewRecorder()

	panel.loginPoll(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d want %d body=%s", rec.Code, http.StatusNotFound, rec.Body.String())
	}

	panel.loginMu.Lock()
	_, stillStored := panel.logins[state]
	panel.loginMu.Unlock()

	if stillStored {
		t.Error("expired login state still stored after poll")
	}
}
