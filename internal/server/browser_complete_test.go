// browser_complete_test.go 钉住独立控制标记 `::browser-complete{...}` 的服务侧处置
// （issue #629）：一行都不下发；带工具上下文时补一次续写，纯聊天只吞标记；
// 续写请求的最后一条 user 消息点明这是控制标记。
package server

import (
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/jsonx"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// sseBrowserMarkerOnly 首段：控制标记拆在两个 delta 里 + stop + [DONE]。
const sseBrowserMarkerOnly = "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"::browser-\"}}]}\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"complete{title=\\\"Task\\\"}\"}}]}\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: [DONE]\n\n"

// sseBrowserToolCall 续写段：带回一次工具调用 + tool_calls 终态 + [DONE]。
const sseBrowserToolCall = "data: {\"id\":\"c2\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[" +
	"{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"browser_eval\",\"arguments\":\"{}\"}}]}}]}\n\n" +
	"data: {\"id\":\"c2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
	"data: [DONE]\n\n"

// bodyRecorder 记录出站请求体（续写请求在独立 goroutine 里发起，需要加锁）。
type bodyRecorder struct {
	mu     sync.Mutex
	bodies [][]byte
}

func (r *bodyRecorder) add(raw []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.bodies = append(r.bodies, raw)
}

func (r *bodyRecorder) all() [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()

	return slices.Clone(r.bodies)
}

// newRecordingUpstream 记录每次出站请求体，并按调用序号返回 responses 里的响应
// （越界时复用最后一个）。
func newRecordingUpstream(t *testing.T, rec *bodyRecorder, responses ...string) *upstream.Client {
	t.Helper()

	var calls atomic.Int32

	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			rec.add(raw)

			idx := int(calls.Add(1)) - 1
			if idx >= len(responses) {
				idx = len(responses) - 1
			}

			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(responses[idx])),
			}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
}

// browserMarkerTestPool 单账号池。
func browserMarkerTestPool(t *testing.T) *pool.Pool {
	t.Helper()

	return testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
}

// TestChatStreamBrowserCompleteSwallowed 流式纯聊天轮：标记不下发，也不发起续写。
func TestChatStreamBrowserCompleteSwallowed(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	fake := newFakeUpstream(t, func(string) (int, string, bool) {
		calls.Add(1)

		return http.StatusOK, sseBrowserMarkerOnly, true
	})
	state := browserMarkerTestPool(t)
	h := NewHandler(&Config{Pool: state, Upstream: fake})

	rec := postChat(t, h, `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	if strings.Contains(body, "::browser-") || strings.Contains(body, "complete{title") {
		t.Errorf("独立控制标记被透传给客户端: %q", body)
	}

	if got := calls.Load(); got != 1 {
		t.Errorf("upstream calls=%d want 1（无工具上下文不续写）", got)
	}

	// 吞掉标记后客户端零正文，但上游产出了实质输出：不能按空流罚号。
	if st := statusOf(t, state, "u1"); st.ConsecutiveFails != 0 || st.BreakerFails != 0 {
		t.Errorf("吞掉控制标记不应罚号: fails=%d breaker=%d", st.ConsecutiveFails, st.BreakerFails)
	}
}

// TestChatStreamBrowserCompleteToolRoundContinued 流式工具轮：标记不下发，补一次续写，
// 续写请求的最后一条 user 消息点明这是浏览器完成控制标记。
func TestChatStreamBrowserCompleteToolRoundContinued(t *testing.T) {
	t.Parallel()

	rec := &bodyRecorder{}
	fake := newRecordingUpstream(t, rec, sseBrowserMarkerOnly, sseBrowserToolCall)
	handler := NewHandler(&Config{Pool: browserMarkerTestPool(t), Upstream: fake})

	body := `{"model":"glm-5.2","stream":true,"tools":[{"type":"function",` +
		`"function":{"name":"browser_eval","parameters":{"type":"object"}}}],` +
		`"messages":[{"role":"user","content":"打开页面"}]}`

	response := postChat(t, handler, body)
	if response.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", response.Code, response.Body.String())
	}

	out := response.Body.String()
	if strings.Contains(out, "::browser-") {
		t.Errorf("控制标记被透传给客户端: %q", out)
	}

	if !strings.Contains(out, "call_1") {
		t.Errorf("续写带回的工具调用未下发（工具轮只采纳调用）: %q", out)
	}

	bodies := rec.all()
	if len(bodies) != 2 {
		t.Fatalf("出站请求数=%d want 2（首段 + 一次续写）", len(bodies))
	}

	// 续写请求：末条 user 消息必须点明控制标记，assistant 部分保留模型刚发过的标记。
	continuing := string(bodies[1])
	if !strings.Contains(continuing, "browser-complete") || !strings.Contains(continuing, "控制标记") {
		t.Errorf("续写指令未点明浏览器完成控制标记: %s", continuing)
	}
}

// TestChatNonStreamBrowserCompleteSwallowed 非流式纯聊天轮：正文被吞成空串，不续写。
func TestChatNonStreamBrowserCompleteSwallowed(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	fake := newFakeUpstream(t, func(string) (int, string, bool) {
		calls.Add(1)

		return http.StatusOK, sseBrowserMarkerOnly, true
	})
	h := NewHandler(&Config{Pool: browserMarkerTestPool(t), Upstream: fake})

	rec := postChat(t, h, `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}

	if got := messageContent(t, rec.Body.Bytes()); got != "" {
		t.Errorf("独立控制标记被透传给客户端: %q", got)
	}

	if got := calls.Load(); got != 1 {
		t.Errorf("upstream calls=%d want 1（无工具上下文不续写）", got)
	}
}

// TestChatNonStreamBrowserCompleteContinued 非流式工具上下文（末条是工具结果）：续写一次，
// 标记不进最终正文。
func TestChatNonStreamBrowserCompleteContinued(t *testing.T) {
	t.Parallel()

	rec := &bodyRecorder{}
	fake := newRecordingUpstream(t, rec, sseBrowserMarkerOnly, sseOK)
	h := NewHandler(&Config{Pool: browserMarkerTestPool(t), Upstream: fake})

	body := `{"model":"glm-5.2","messages":[{"role":"user","content":"打开页面"},` +
		`{"role":"tool","tool_call_id":"t1","content":"ok"}]}`

	response := postChat(t, h, body)
	if response.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", response.Code, response.Body.String())
	}

	if got := messageContent(t, response.Body.Bytes()); got != "你好" {
		t.Errorf("非流式正文=%q want 续写结果（标记不得留在正文里）", got)
	}

	bodies := rec.all()
	if len(bodies) != 2 {
		t.Fatalf("出站请求数=%d want 2（首段 + 一次续写）", len(bodies))
	}

	if continuing := string(bodies[1]); !strings.Contains(continuing, "控制标记") {
		t.Errorf("续写指令未点明浏览器完成控制标记: %s", continuing)
	}
}

// messageContent 取非流式响应 choices[0].message.content。
func messageContent(t *testing.T, raw []byte) string {
	t.Helper()

	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}

	if err := jsonx.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(resp.Choices) == 0 {
		t.Fatalf("no choices: %s", raw)
	}

	return resp.Choices[0].Message.Content
}
