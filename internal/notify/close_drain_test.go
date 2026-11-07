package notify

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestNotifierCloseDrainsQueuedEvents Close 必须排空已入队但未发送的事件再退出。
// 修复前 run() 的 select 在 done 关闭后随机 return，队列里剩余通知被静默丢弃。
func TestNotifierCloseDrainsQueuedEvents(t *testing.T) {
	t.Parallel()

	var delivered atomic.Int32

	// 发送端加一点延迟，制造「队列已积压、worker 仍在逐条发」的关停窗口。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p payload

		_ = json.NewDecoder(r.Body).Decode(&p)

		time.Sleep(5 * time.Millisecond)
		delivered.Add(1)

		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	n := New(Config{URL: srv.URL, DedupeWindow: 0})

	// 不同 UID 绕过去重，全部入队。
	for i := range 8 {
		n.Notify(&Event{
			Event: EventTaskFailed, UID: "uid-" + string(rune('a'+i)), Time: time.Time{},
			Nickname: "", Realm: "", Summary: "", Detail: nil,
		})
	}

	n.Close() // 必须等 worker 排空队列后才返回

	if got := delivered.Load(); got != 8 {
		t.Fatalf("Close 应排空全部已入队事件，delivered=%d want 8", got)
	}
}
