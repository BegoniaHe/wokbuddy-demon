// checkin_states_test.go 钉住面板签到回执四态（issue #511）：重复签到不再报成
// 「签到完成」、失败文案不带上游信封与 requestId、积分只在新签到时显示。
package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

func checkinPanelResponse(t *testing.T, checkinStatus int, checkinBody string) map[string]any {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-checkin"):
			w.WriteHeader(checkinStatus)
			_, _ = w.Write([]byte(checkinBody))
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[` +
				`{"CycleCapacitySize":1000,"CycleCapacityRemain":500}]}}}}`))
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999}
	p.Add(a)

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	panel := &Panel{cfg: Config{Pool: p, Upstream: up}}

	req := httptest.NewRequest(http.MethodPost, "/panel/api/accounts/u1/checkin", nil)
	req.SetPathValue("uid", "u1")

	rec := httptest.NewRecorder()
	panel.accountCheckin(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}

	return got
}

func TestAccountCheckinStates(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		status     int
		body       string
		wantState  string
		wantDone   bool
		wantCredit float64
	}{
		{
			name:      "already",
			status:    http.StatusBadRequest,
			body:      `{"code":10001,"msg":"今天已签到，请明天再来","requestId":"req-1"}`,
			wantState: "already",
			wantDone:  true,
		},
		{
			name:       "checked-with-credit",
			status:     http.StatusOK,
			body:       `{"credit":100,"streak_days":3,"is_streak_day":true}`,
			wantState:  "checked",
			wantDone:   true,
			wantCredit: 100,
		},
		{
			name:      "checked-without-credit",
			status:    http.StatusOK,
			body:      `{"streak_days":3}`,
			wantState: "checked",
			wantDone:  true,
		},
		{
			name:      "failed-with-already-substring",
			status:    http.StatusBadRequest,
			body:      `{"code":11101,"msg":"签到失败，已签到天数不足 7 天","requestId":"req-2"}`,
			wantState: "failed",
			wantDone:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assertCheckinResponse(t, checkinPanelResponse(t, tc.status, tc.body),
				tc.wantState, tc.wantDone, tc.wantCredit)
		})
	}
}

// assertCheckinResponse 断言回执状态的三个可见面：状态、是否算已签、积分增量。
func assertCheckinResponse(
	t *testing.T, got map[string]any, wantState string, wantDone bool, wantCredit float64,
) {
	t.Helper()

	if state, _ := got["checkin_state"].(string); state != wantState {
		t.Errorf("checkin_state=%v want %s", got["checkin_state"], wantState)
	}

	if done, _ := got["checkin_done"].(bool); done != wantDone {
		t.Errorf("checkin_done=%v want %v", got["checkin_done"], wantDone)
	}

	credit, hasCredit := got["checkin_credit"]
	if wantCredit == 0 {
		if hasCredit {
			t.Errorf("不应出现 checkin_credit，got %v", credit)
		}
	} else if credit != wantCredit {
		t.Errorf("checkin_credit=%v want %v", credit, wantCredit)
	}

	if msg, _ := got["checkin_message"].(string); strings.Contains(msg, "requestId") ||
		strings.Contains(msg, "req-") || strings.Contains(msg, "upstream client (") ||
		strings.Contains(msg, "{") {
		t.Errorf("checkin_message=%q 不应回显上游信封/请求号", msg)
	}
}
