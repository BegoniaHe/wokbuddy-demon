// dedupe_release_test.go 钉住「发送失败必须释放去重窗口」：webhook 抖动时
// session_dead 这类终态告警只有一次机会，窗口被吃掉就再也不会重发。
package notify

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// waitHits 轮询等待计数到达 want（避免依赖固定 sleep 的时序脆弱性）。
func waitHits(t *testing.T, hits *atomic.Int32, want int32) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)

	for time.Now().Before(deadline) {
		if hits.Load() >= want {
			return
		}

		time.Sleep(5 * time.Millisecond)
	}

	t.Fatalf("发送次数=%d want %d", hits.Load(), want)
}

// TestNotifierSendFailureReleasesDedupeWindow 5xx 失败后，同一事件+账号应能立刻
// 重发，而不是被 10 分钟去重窗口压下。
func TestNotifierSendFailureReleasesDedupeWindow(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	// 去重窗口设成 1 小时：失败若不释放窗口，第二次 Notify 必然被丢弃。
	n := New(Config{URL: srv.URL, DedupeWindow: time.Hour})
	defer n.Close()

	event := &Event{Event: EventTaskFailed, UID: "u1", Time: time.Time{}, Nickname: "", Realm: "", Summary: "", Detail: nil}

	n.Notify(event)
	waitHits(t, &hits, 1)

	n.Notify(event)
	waitHits(t, &hits, 2)
}

// TestNotifierSendSuccessKeepsDedupeWindow 成功投递后窗口仍然有效（去重语义不变）。
func TestNotifierSendSuccessKeepsDedupeWindow(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	n := New(Config{URL: srv.URL, DedupeWindow: time.Hour})
	defer n.Close()

	event := &Event{Event: EventTaskFailed, UID: "u1", Time: time.Time{}, Nickname: "", Realm: "", Summary: "", Detail: nil}

	n.Notify(event)
	waitHits(t, &hits, 1)

	n.Notify(event) // 窗口内的重复事件：必须被丢弃
	time.Sleep(100 * time.Millisecond)

	if got := hits.Load(); got != 1 {
		t.Fatalf("成功投递后窗口内的重复事件被重发，hits=%d want 1", got)
	}
}
