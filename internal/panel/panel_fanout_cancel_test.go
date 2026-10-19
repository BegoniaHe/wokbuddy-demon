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

// TestReadonlyFanoutStopsOnCanceledContext 只读扇出接口在请求已取消时不得再
// 派发逐号上游调用：否则用户切走页面后，账号多时仍会空跑整轮请求。
func TestReadonlyFanoutStopsOnCanceledContext(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)

		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
	}))
	t.Cleanup(srv.Close)

	state := pool.New("")

	for i := range 20 {
		state.Add(&auth.Auth{
			UID: fmt.Sprintf("u%02d", i), AccessToken: "at", RefreshToken: "rt",
			ExpiresAt: 9999999999,
		})
	}

	client := &upstream.Client{HTTP: srv.Client(), BillingBaseCN: srv.URL, WebBaseCN: srv.URL}
	panel := &Panel{cfg: Config{Pool: state, Upstream: client}}

	handlers := map[string]http.HandlerFunc{
		"packages":       panel.packages,
		"schoolVouchers": panel.schoolVouchers,
		"tasksScanAll":   panel.tasksScanAll,
	}

	for name, handler := range handlers {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		req := httptest.NewRequest(http.MethodGet, "/panel/api/test", nil).WithContext(ctx)
		rec := httptest.NewRecorder()
		handler(rec, req)

		if got := calls.Load(); got != 0 {
			t.Fatalf("%s: upstream calls=%d want 0 after canceled ctx", name, got)
		}
	}
}
