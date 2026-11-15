// notify_dedupe_release_test.go 钉住「被丢弃的事件不占去重窗口」：
// allow() 在入队前登记，队列满丢弃时若不撤销，同一事件 + 账号在窗口内
// 再也不会重试，真实告警被静默压制。
package notify

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestDroppedEventReleasesDedupeWindow(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})

	var once sync.Once

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		once.Do(func() { close(started) })
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))

	n := New(Config{URL: srv.URL, DedupeWindow: time.Minute})

	defer srv.Close()
	defer n.Close()
	defer close(release)

	// 先让 worker 卡在第一条发送上，从而把队列占满。
	n.Notify(&Event{Event: EventSessionDead, UID: "blocker", Time: time.Time{}, Nickname: "", Realm: "", Summary: "", Detail: nil})

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("worker 未开始发送")
	}

	for i := range queueSize {
		n.Notify(&Event{Event: EventSessionDead, UID: fmt.Sprintf("fill-%d", i), Time: time.Time{}, Nickname: "", Realm: "", Summary: "", Detail: nil})
	}

	dropped := &Event{Event: EventSessionDead, UID: "dropped-one", Time: time.Time{}, Nickname: "", Realm: "", Summary: "", Detail: nil}
	n.Notify(dropped)

	key := dropped.Event + "\x00" + dropped.UID

	n.mu.Lock()
	_, held := n.last[key]
	n.mu.Unlock()

	if held {
		t.Fatal("被丢弃的事件仍占住去重窗口（同一告警在窗口内不会再发）")
	}
}
