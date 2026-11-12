// ttfb_keepalive_test.go 钉住首帧等待期的 SSE 注释帧（issue #500）：慢首帧按间隔
// 补注释帧、快首帧零注释帧、注释帧不切碎真实帧、已提交响应头后错误降级为 200 SSE。
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// delayFirstReadBody 首次 Read 前等待 delay（模拟大上下文的首帧 TTFB），之后把
// 底层流原样交出。
type delayFirstReadBody struct {
	io.Reader

	delay time.Duration
	once  sync.Once
}

func (b *delayFirstReadBody) Read(p []byte) (int, error) {
	b.once.Do(func() { time.Sleep(b.delay) })

	return b.Reader.Read(p) //nolint:wrapcheck // io.EOF 语义必须原样交给读取方
}

func (b *delayFirstReadBody) Close() error { return nil }

// delayedSSEClient 每个请求返回一条延迟首字节的 SSE 流（body 每次新建）。
func delayedSSEClient(delay time.Duration, raw string) *upstream.Client {
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       &delayFirstReadBody{Reader: strings.NewReader(raw), delay: delay},
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

// assertCompleteSSEFrames 断言响应体只由完整 SSE 行组成：注释帧以 ":" 起始，
// 其余非空行必须是完整的 "data: " 帧（除 [DONE] 外单个合法 JSON）。注释帧若插进
// 写了一半的事件帧中间，这里会看到非法的 data 载荷。
func assertCompleteSSEFrames(t *testing.T, body string) {
	t.Helper()

	for line := range strings.SplitSeq(body, "\n") {
		switch {
		case line == "", strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "data: "):
			payload := strings.TrimPrefix(line, "data: ")
			if payload == "[DONE]" {
				continue
			}

			if !json.Valid([]byte(payload)) {
				t.Errorf("data 帧不是完整合法 JSON: %q", payload)
			}
		default:
			t.Errorf("非完整 SSE 行: %q", line)
		}
	}
}

// TestChatStreamTTFBHeartbeat 慢首帧：按间隔写注释帧，真实帧完整、内容不丢、
// [DONE] 恰好一次。
func TestChatStreamTTFBHeartbeat(t *testing.T) {
	t.Parallel()

	handler := NewHandler(&Config{
		Pool:                  testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream:              delayedSSEClient(80*time.Millisecond, sseOK),
		TTFBKeepaliveInterval: 10 * time.Millisecond,
		SoftCooldown:          time.Minute,
	})

	rec := postChat(t, handler, `{"model":"glm-5.2","messages":[],"stream":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	body := rec.Body.String()
	if !strings.Contains(body, ": keepalive\n\n") {
		t.Fatalf("慢首帧应补注释帧: %q", body)
	}

	if !strings.Contains(body, `"content":"你好"`) {
		t.Errorf("正文丢失: %q", body)
	}

	if n := strings.Count(body, "data: [DONE]"); n != 1 {
		t.Errorf("[DONE] count=%d want 1: %q", n, body)
	}

	assertCompleteSSEFrames(t, body)
}

// TestChatStreamTTFBHeartbeatFastFirstFrame 快首帧：零注释帧（不改变正常流字节）。
func TestChatStreamTTFBHeartbeatFastFirstFrame(t *testing.T) {
	t.Parallel()

	handler := NewHandler(&Config{
		Pool:                  testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream:              delayedSSEClient(0, sseOK),
		TTFBKeepaliveInterval: 10 * time.Millisecond,
		SoftCooldown:          time.Minute,
	})

	rec := postChat(t, handler, `{"model":"glm-5.2","messages":[],"stream":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if body := rec.Body.String(); strings.Contains(body, ": keepalive") {
		t.Errorf("快首帧不应写注释帧: %q", body)
	}
}

// TestChatStreamTTFBHeartbeatDisabled 间隔为 0：慢首帧同样零注释帧（逐字节保持旧行为）。
func TestChatStreamTTFBHeartbeatDisabled(t *testing.T) {
	t.Parallel()

	handler := NewHandler(&Config{
		Pool:                  testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream:              delayedSSEClient(60*time.Millisecond, sseOK),
		TTFBKeepaliveInterval: 0,
		SoftCooldown:          time.Minute,
	})

	rec := postChat(t, handler, `{"model":"glm-5.2","messages":[],"stream":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if body := rec.Body.String(); strings.Contains(body, ": keepalive") {
		t.Errorf("间隔为 0 时不应写注释帧: %q", body)
	}
}

// TestChatTerminalErrorAfterCommittedHeaders 首帧等待期已写过注释帧（响应头已提交）
// 后轮转全部失败：降级为 200 + error 帧 + [DONE]，不往已提交的 SSE 流里塞 JSON。
func TestChatTerminalErrorAfterCommittedHeaders(t *testing.T) {
	t.Parallel()

	const errFrame = "data: {\"error\":{\"code\":\"500\",\"message\":\"upstream exploded\"}}\n\n"

	handler := NewHandler(&Config{
		Pool:                  testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream:              delayedSSEClient(60*time.Millisecond, errFrame),
		TTFBKeepaliveInterval: 10 * time.Millisecond,
		MaxRotate:             1,
		SoftCooldown:          time.Minute,
	})

	rec := postChat(t, handler, `{"model":"glm-5.2","messages":[],"stream":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("响应头已提交后状态码只能是 200, got %d body=%s", rec.Code, rec.Body)
	}

	body := rec.Body.String()
	if !strings.Contains(body, `"code":"no_healthy_account"`) {
		t.Errorf("缺 error 帧: %q", body)
	}

	if !strings.Contains(body, `"type":"api_error"`) {
		t.Errorf("error 帧形状不符: %q", body)
	}

	if n := strings.Count(body, "data: [DONE]"); n != 1 {
		t.Errorf("[DONE] count=%d want 1: %q", n, body)
	}

	assertCompleteSSEFrames(t, body)
}

// TestChatTerminalErrorKeepsJSONBeforeCommit 未提交响应头（首帧立刻失败、心跳未触发）
// 时仍走原来的 JSON 错误码。
func TestChatTerminalErrorKeepsJSONBeforeCommit(t *testing.T) {
	t.Parallel()

	const errFrame = "data: {\"error\":{\"code\":\"500\",\"message\":\"upstream exploded\"}}\n\n"

	handler := NewHandler(&Config{
		Pool:                  testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream:              delayedSSEClient(0, errFrame),
		TTFBKeepaliveInterval: 10 * time.Millisecond,
		MaxRotate:             1,
		SoftCooldown:          time.Minute,
	})

	rec := postChat(t, handler, `{"model":"glm-5.2","messages":[],"stream":true}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("未提交响应头应保持 JSON 错误码, got %d body=%s", rec.Code, rec.Body)
	}

	body := rec.Body.String()
	if !strings.HasPrefix(body, `{"error":`) {
		t.Errorf("未提交时应是 JSON 错误体: %q", body)
	}

	if strings.Contains(body, "data: ") {
		t.Errorf("未提交时不应写 SSE 帧: %q", body)
	}
}
