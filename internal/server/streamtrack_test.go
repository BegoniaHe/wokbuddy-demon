// streamtrack_test.go 钉住在途 SSE 流注册表（issue #19）：计数、带 ctx 的排空等待，
// 以及 handler 流式路径的 enter/leave 接入。
package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

func TestStreamTrackerDrain(t *testing.T) {
	tr := newStreamTracker()
	tr.enter()
	tr.enter()
	if tr.Active() != 2 {
		t.Fatalf("Active=%d want 2", tr.Active())
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan bool, 1)
	go func() { done <- tr.WaitDrained(ctx) }()
	tr.leave()
	tr.leave()
	if !<-done {
		t.Fatal("WaitDrained should return true after drain")
	}
	if tr.Active() != 0 {
		t.Fatalf("Active=%d want 0", tr.Active())
	}
}

func TestStreamTrackerWaitTimeout(t *testing.T) {
	tr := newStreamTracker()
	tr.enter()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if tr.WaitDrained(ctx) {
		t.Fatal("WaitDrained should return false while stream active and ctx expired")
	}
}

// TestActiveStreamsTracksInFlightStream 端到端：一个被挂起的上游 SSE 流在途时
// ActiveStreams()==1，释放后回到 0（issue #19）。
func TestActiveStreamsTracksInFlightStream(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		io.WriteString(w, `data: {"id":"1","object":"chat.completion.chunk","model":"glm-5.2","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}]}`+"\n\n")
		if fl != nil {
			fl.Flush()
		}
		<-release // 挂起：保持流在途
		io.WriteString(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}))
	defer srv.Close()

	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	h := NewHandler(Config{Pool: p, Upstream: up})

	done := make(chan struct{})
	go func() {
		defer close(done)
		postChat(t, h, `{"model":"glm-5.2","stream":true,"messages":[]}`)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for h.ActiveStreams() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if h.ActiveStreams() != 1 {
		t.Fatalf("ActiveStreams=%d want 1 while stream in flight", h.ActiveStreams())
	}
	close(release)
	<-done
	if h.ActiveStreams() != 0 {
		t.Fatalf("ActiveStreams=%d want 0 after stream done", h.ActiveStreams())
	}
}
