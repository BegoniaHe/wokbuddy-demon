package panel

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// TestRunExpertUseAllFailedReturnsError 全部召唤上报失败时必须返回错误：
// 返回 nil 会让 autoRunAction 把「0 位完成」标成 done，失败被静默吞掉。
func TestRunExpertUseAllFailedReturnsError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/portal/operation-platform/market/expert/list":
			_, _ = w.Write([]byte(`{"code":0,"data":{"experts":[{"expert_id":"ex_1",` +
				`"expert_type":"agent","display_name_zh":"专家","profession_zh":"专家",` +
				`"version":"1.0.0","categories":["expert-all"]}]}}`))
		case "/v2/report":
			http.Error(w, `{"code":500,"msg":"rejected"}`, http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	client := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
		WebBaseCN:     srv.URL,
	}
	state := pool.New("")
	state.Add(&auth.Auth{UID: "u1", AccessToken: "at-u1"})

	msg, err := runExpertUse(&Panel{cfg: Config{Pool: state, Upstream: client}}, state.AuthByUID("u1"))
	if err == nil {
		t.Fatalf("全部失败时应返回错误，got msg=%q", msg)
	}

	if !errors.Is(err, errExpertAllFailed) {
		t.Fatalf("err=%v want wrap errExpertAllFailed", err)
	}

	if msg != "" {
		t.Fatalf("失败时 msg=%q want empty", msg)
	}

	if !strings.Contains(err.Error(), "1 次尝试") {
		t.Fatalf("err=%v 应带上失败尝试数", err)
	}
}
