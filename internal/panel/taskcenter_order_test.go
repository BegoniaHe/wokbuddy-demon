package panel

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// TestCollectQueueAccountsPreservesPoolOrder 并发拉取待办后，队列账号顺序必须
// 与 Pool.List 一致，不随 goroutine 完成顺序漂移。
func TestCollectQueueAccountsPreservesPoolOrder(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"tasks":[{"task_code":"chat_5","target":1,"current":0}]}}`))
	}))
	t.Cleanup(srv.Close)

	client := &upstream.Client{
		HTTP:              srv.Client(),
		ChatBaseCN:        srv.URL,
		BillingBaseCN:     srv.URL,
		ChatBaseGlobal:    srv.URL,
		BillingBaseGlobal: srv.URL,
	}

	state := pool.New("")
	for _, uid := range []string{"u3", "u1", "u2"} {
		state.Add(&auth.Auth{UID: uid, AccessToken: "at-" + uid, RefreshToken: "rt-" + uid})
	}

	panel := &Panel{cfg: Config{Pool: state, Upstream: client}}

	accounts := panel.collectQueueAccounts(true)

	got := make([]string, 0, len(accounts))
	for i := range accounts {
		got = append(got, accounts[i].a.UID)
	}

	want := []string{"u1", "u2", "u3"} // Pool.List 按 UID 排序
	if len(got) != len(want) {
		t.Fatalf("accounts=%v want %v", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("accounts=%v want %v (order must follow Pool.List)", got, want)
		}
	}
}

// TestRunQueueRejectsMalformedJSON 畸形请求体不得触发全账号扫描与队列执行；
// 空 body 仍走默认值（保持既有语义）。
func TestRunQueueRejectsMalformedJSON(t *testing.T) {
	t.Parallel()

	panel := New(&Config{Version: "test", APIKey: "test-key"})

	rec := httptest.NewRecorder()
	panel.ServeHTTP(rec, authed(http.MethodPost, "/panel/api/tasks/run_queue", `{"concurrency":`))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
}
