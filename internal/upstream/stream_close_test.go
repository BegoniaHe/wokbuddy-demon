// stream_close_test.go 流式收尾与连接复用（issue #454）。
package upstream

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// TestChatStreamCloseDrainsTailSoConnIsReused 上游在 [DONE] 之后才发 chunked
// 结束块：读循环停在 [DONE]，Close 必须限时读掉流尾，让 Transport 读到 EOF
// 把连接放回空闲池，第二个请求不再重新拨号。
func TestChatStreamCloseDrainsTailSoConnIsReused(t *testing.T) {
	t.Parallel()

	// 结束块的延迟要小于收尾预算（100ms），又足以让客户端在它到达前停读。
	const tailDelay = 20 * time.Millisecond

	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"x\"}\n\ndata: [DONE]\n\n")
		w.(http.Flusher).Flush()
		// [DONE] 之后上游才发 chunked 结束块（真机实测为十几毫秒）。
		time.Sleep(tailDelay)
	}))
	server.Start()
	t.Cleanup(server.Close)

	client := newProfileClient(t)
	client.ChatBaseCN = server.URL
	client.IdleTimeout = time.Minute

	var dials atomic.Int64

	client.SetDialer(countingDial(&dials))

	for i := range 2 {
		rc, status, _, err := client.ChatStream(
			&auth.Auth{AccessToken: "at", UID: "u1"},
			[]byte(`{"model":"glm-5.2","messages":[]}`), "", ChatMeta{},
		)
		if err != nil || status != http.StatusOK {
			t.Fatalf("request %d: status=%d err=%v", i, status, err)
		}

		// 复刻 handler：透传到 [DONE] 即停，随后关闭上游 body。
		if serr := StreamHint(httptest.NewRecorder(), rc, nil); serr != nil {
			t.Fatalf("request %d: stream: %v", i, serr)
		}

		if cerr := rc.Close(); cerr != nil {
			t.Fatalf("request %d: close: %v", i, cerr)
		}
	}

	if got := dials.Load(); got != 1 {
		t.Fatalf("dials=%d want 1（[DONE] 之后的结束块未被读掉，连接没回池）", got)
	}
}

// TestMonitorBodyCloseBoundedWhenStreamStaysOpen 流没结束（客户端中途断开/换号）
// 时，Close 不得为等 EOF 长期阻塞：超时后取消并关闭底流，收尾在预算量级内完成。
func TestMonitorBodyCloseBoundedWhenStreamStaysOpen(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	// 读到取消为止：drain 等不到 EOF，只能等定时器。
	body := monitorBody(&ctxBoundReader{ctx: ctx}, time.Minute, cancel)

	start := time.Now()

	if err := body.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("close took %v, want <= 500ms", elapsed)
	}
}

// TestMonitorBodyCloseIsIdempotent 多层收尾可能重复 Close：底流只关一次。
func TestMonitorBodyCloseIsIdempotent(t *testing.T) {
	t.Parallel()

	rc := &closeCountingBody{Reader: nopCloserBody{Reader: &oneByteReader{}}}
	_, cancel := context.WithCancel(t.Context())
	body := monitorBody(rc, time.Minute, cancel)

	_ = body.Close()
	_ = body.Close()

	if got := rc.closes.Load(); got != 1 {
		t.Fatalf("underlying close called %d times, want 1", got)
	}
}

// TestChatStreamDoErrorKeepsIdlePoolOnCallerCancel 调用方取消导致的 Do 失败不
// 说明连接坏了：不得清掉共享 Transport 里其他请求的空闲连接。
func TestChatStreamDoErrorKeepsIdlePoolOnCallerCancel(t *testing.T) {
	t.Parallel()

	counter := &closeIdleCounter{inner: newTransport()}
	t.Cleanup(counter.inner.CloseIdleConnections)

	client := &Client{
		HTTP:       &http.Client{Transport: counter},
		ChatHTTP:   &http.Client{Transport: counter},
		ChatBaseCN: "https://chat.example",
		Profile:    testProfileSet(t),
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // 客户端已断连

	_, _, _, err := client.ChatStreamContext(
		ctx, &auth.Auth{AccessToken: "at", UID: "u1"},
		[]byte(`{"model":"glm-5.2","messages":[]}`), "", ChatMeta{},
	)
	if err == nil {
		t.Fatal("expect transport error from canceled caller context")
	}

	if got := counter.calls.Load(); got != 0 {
		t.Fatalf("CloseIdleConnections called %d times on caller cancel, want 0", got)
	}
}

// TestChatStreamDoErrorClearsIdlePoolOnTransportFailure 真正的传输失败仍要清
// 空闲池：失败连接可能还躺在池里，下个请求会继续捡到它。
func TestChatStreamDoErrorClearsIdlePoolOnTransportFailure(t *testing.T) {
	t.Parallel()

	// 先占一个端口再释放：该地址必然拒连。
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	addr := listener.Addr().String()

	if cerr := listener.Close(); cerr != nil {
		t.Fatalf("close listener: %v", cerr)
	}

	counter := &closeIdleCounter{inner: newTransport()}
	t.Cleanup(counter.inner.CloseIdleConnections)

	client := &Client{
		HTTP:       &http.Client{Transport: counter},
		ChatHTTP:   &http.Client{Transport: counter},
		ChatBaseCN: "http://" + addr,
		Profile:    testProfileSet(t),
	}

	_, _, _, err = client.ChatStreamContext(
		context.Background(), &auth.Auth{AccessToken: "at", UID: "u1"},
		[]byte(`{"model":"glm-5.2","messages":[]}`), "", ChatMeta{},
	)
	if err == nil {
		t.Fatal("expect transport error from refused connection")
	}

	if got := counter.calls.Load(); got != 1 {
		t.Fatalf("CloseIdleConnections called %d times on transport failure, want 1", got)
	}
}

// closeIdleCounter 统计 CloseIdleConnections 调用次数。
type closeIdleCounter struct {
	inner *http.Transport
	calls atomic.Int64
}

func (c *closeIdleCounter) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := c.inner.RoundTrip(r)
	if err != nil {
		return nil, fmt.Errorf("round trip: %w", err)
	}

	return resp, nil
}

func (c *closeIdleCounter) CloseIdleConnections() { c.calls.Add(1) }

// closeCountingBody 记录底流被关闭的次数。
type closeCountingBody struct {
	io.Reader

	closes atomic.Int64
}

func (b *closeCountingBody) Close() error {
	b.closes.Add(1)

	return nil
}
