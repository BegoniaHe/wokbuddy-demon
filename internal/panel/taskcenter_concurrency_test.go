package panel

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// TestCollectQueueAccountsBoundsUpstreamConcurrency 队列收集不能为每个账号
// 无界并发上游调用：导入上限 1000 个账号时，瞬时打满会触发上游限流。
func TestCollectQueueAccountsBoundsUpstreamConcurrency(t *testing.T) {
	t.Parallel()

	var (
		inFlight atomic.Int64
		peak     atomic.Int64
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		current := inFlight.Add(1)
		defer inFlight.Add(-1)

		for {
			old := peak.Load()
			if current <= old || peak.CompareAndSwap(old, current) {
				break
			}
		}

		// 放大重叠窗口，让无界实现稳定暴露峰值。
		time.Sleep(20 * time.Millisecond)

		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"tasks":[]}}`))
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

	for i := range 12 {
		uid := "u" + string(rune('a'+i))
		state.Add(&auth.Auth{UID: uid, AccessToken: "at-" + uid, RefreshToken: "rt-" + uid})
	}

	panel := &Panel{cfg: Config{Pool: state, Upstream: client}}
	panel.collectQueueAccounts(true)

	if got := peak.Load(); got > scanWorkers {
		t.Fatalf("peak upstream concurrency=%d want <= %d", got, scanWorkers)
	}
}
