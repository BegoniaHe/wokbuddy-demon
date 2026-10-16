// checkin_result_test.go 钉住面板签到结果分类：活动未开启给出可区分文案且不标记已签
// （issue #27）。
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

func TestAccountCheckinInactiveMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-checkin"):
			w.Write([]byte(`{"code":14001,"msg":"活动未开启"}`))
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":1000,"CycleCapacityRemain":500}]}}}}`))
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999}
	p.Add(a)
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	pn := &Panel{cfg: Config{Pool: p, Upstream: up}}

	req := httptest.NewRequest(http.MethodPost, "/panel/api/accounts/u1/checkin", nil)
	req.SetPathValue("uid", "u1")
	rec := httptest.NewRecorder()
	pn.accountCheckin(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		CheckinDone    bool   `json:"checkin_done"`
		CheckinMessage string `json:"checkin_message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if got.CheckinDone {
		t.Error("活动未开启不应 checkin_done=true")
	}
	if !strings.Contains(got.CheckinMessage, "活动未开启") {
		t.Errorf("checkin_message=%q want 含活动未开启", got.CheckinMessage)
	}
}
