package server

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/notify"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

type fakeNotifier struct {
	mu     sync.Mutex
	events []notify.Event
}

func (f *fakeNotifier) Notify(e *notify.Event) {
	f.mu.Lock()
	f.events = append(f.events, *e)
	f.mu.Unlock()
}

func (f *fakeNotifier) first() (notify.Event, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if len(f.events) == 0 {
		return notify.Event{Event: "", Time: time.Time{}, UID: "", Nickname: "", Realm: "", Summary: "", Detail: nil}, false
	}

	return f.events[0], true
}

// TestNotifyAccountIncludesPoolIdentity 账号级事件带昵称/realm，UID 保持完整交给通知层截断。
func TestNotifyAccountIncludesPoolIdentity(t *testing.T) {
	t.Parallel()

	p := testPoolWith(&auth.Auth{UID: "u1", Nickname: "昵称", AccessToken: "at", ExpiresAt: 9999999999})
	fn := &fakeNotifier{}
	h := NewHandler(&Config{Pool: p, Notifier: fn})
	h.notifyAccount("u1", notify.EventSessionDead, "dead", map[string]any{"stage": "chat"})

	event, ok := fn.first()
	if !ok {
		t.Fatal("未发送事件")
	}

	if event.Event != notify.EventSessionDead || event.UID != "u1" || event.Nickname != "昵称" || event.Realm != "cn" {
		t.Fatalf("event=%+v", event)
	}
}

// TestNotifyPoolUnavailable 无可用账号时发送池级事件，且不阻塞请求路径。
func TestNotifyPoolUnavailable(t *testing.T) {
	t.Parallel()

	fn := &fakeNotifier{}
	h := NewHandler(&Config{Pool: testPoolWith(), Upstream: upstream.New(), Notifier: fn})
	f := h.newChatFlow(&chatInput{body: []byte(`{"model":"glm-5.2","messages":[]}`), model: "glm-5.2"}, &fakeResponder{})
	f.attempt(context.Background(), 0)

	event, ok := fn.first()
	if !ok {
		t.Fatal("未发送 pool_unavailable")
	}

	if event.Event != notify.EventPoolUnavailable || event.Detail["model"] != "glm-5.2" {
		t.Fatalf("event=%+v", event)
	}
}
