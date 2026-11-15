// creditlow_test.go 钉住 credit_low 的「跨阈值」语义：告警条件是电平（余额长期
// 低于保底），若每轮余额刷新都发，就是一个持续状态变成无限期每 10 分钟一条的推送，
// 且永远没有恢复事件。
package scheduler

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/notify"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
)

func TestNotifyCreditLowOnlyOnCrossing(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	state := pool.New("")
	floor := int64(100)
	state.SetCreditFloor(0, map[string]*int64{"cn": &floor, "global": &floor})

	notifier := notify.New(notify.Config{URL: srv.URL, DedupeWindow: 0})
	defer notifier.Close()

	scheduler := New(&Config{Pool: state, Notifier: notifier})
	account := &auth.Auth{UID: "u1"}

	waitCount := func(want int32) {
		t.Helper()

		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if hits.Load() >= want {
				return
			}

			time.Sleep(5 * time.Millisecond)
		}

		t.Fatalf("告警数=%d want %d", hits.Load(), want)
	}

	// 首次跌破保底：告警一次。
	scheduler.notifyCreditLow(account, 10)
	waitCount(1)

	// 持续低于保底：不再重复推送。
	scheduler.notifyCreditLow(account, 20)
	time.Sleep(100 * time.Millisecond)

	if got := hits.Load(); got != 1 {
		t.Fatalf("持续低余额不应重复告警，hits=%d want 1", got)
	}

	// 回到保底以上：重新武装（标记清除）。
	scheduler.notifyCreditLow(account, 500)

	if _, fired := scheduler.creditLowFired.Load("u1"); fired {
		t.Fatal("余额回到保底以上后标记应清除")
	}

	// 再次跌破：重新进入告警状态。这里只断言标记重新置位——notify 自身的
	// 10 分钟去重窗口仍在生效，实际 HTTP 投递由 TestNotifierSend* 单独覆盖。
	scheduler.notifyCreditLow(account, 10)

	if _, fired := scheduler.creditLowFired.Load("u1"); !fired {
		t.Fatal("再次跌破后标记应重新置位")
	}
}
