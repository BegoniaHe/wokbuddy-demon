package panel

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// TestSyncNicknamesCanceledContextStopsDispatch 已取消的 ctx 不得再派发昵称
// 同步请求：sem 有空位时 select 会与 ctx.Done 随机二选一，必须显式先判取消。
func TestSyncNicknamesCanceledContextStopsDispatch(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)

		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"uid":"u00","nickname":"n"}}`))
	}))
	t.Cleanup(srv.Close)

	state := pool.New("")

	for i := range 20 {
		uid := fmt.Sprintf("u%02d", i)
		state.Add(&auth.Auth{UID: uid, AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	}

	client := &upstream.Client{HTTP: srv.Client(), WebBaseCN: srv.URL}
	p := &Panel{cfg: Config{Pool: state, Upstream: client}}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	p.syncNicknames(ctx)

	if got := calls.Load(); got != 0 {
		t.Fatalf("upstream calls=%d want 0 after canceled ctx", got)
	}
}
