package scheduler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// TestStreakAllStopsOnCancelAtAccountBoundary 连登管家收到取消后必须在下一个账号
// 边界退出，不再为剩余账号发起上游调用（issue #422）。
func TestStreakAllStopsOnCancelAtAccountBoundary(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			// 第一个账号的上游调用期间触发取消（模拟停机）。
			cancel()
		}

		_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
	}))
	t.Cleanup(srv.Close)

	state := pool.New("")
	state.Add(&auth.Auth{UID: "u1", AccessToken: "at1", RefreshToken: "rt1", ExpiresAt: 9999999999})
	state.Add(&auth.Auth{UID: "u2", AccessToken: "at2", RefreshToken: "rt2", ExpiresAt: 9999999999})

	scheduler := New(&Config{
		Pool: state,
		Upstream: &upstream.Client{
			HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL,
		},
	})

	total, _, _ := scheduler.streakAll(ctx, TriggerManual)

	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls=%d want 1（取消后不得继续第二个账号）", got)
	}

	if total != 1 {
		t.Fatalf("total=%d want 1（只尝试第一个账号）", total)
	}
}
