package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/reqlog"
)

// TestShouldWarnStopWithoutToolCalls 覆盖 issue #44 的观测判据：只有「声明了 tools
// + finish_reason=stop + 零 tool_calls + 本轮成功」四条同时成立才报。
func TestShouldWarnStopWithoutToolCalls(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		declared     int
		calls        int
		finishReason string
		outcome      string
		want         bool
	}{
		{
			name:         "declared tools, stop, zero calls, success",
			declared:     1,
			calls:        0,
			finishReason: "stop",
			outcome:      reqlog.OutcomeSuccess,
			want:         true,
		},
		{
			// 核心反例：纯对话请求的 stop 完全正常，绝不能报。
			name:         "no tools declared, stop",
			declared:     0,
			calls:        0,
			finishReason: "stop",
			outcome:      reqlog.OutcomeSuccess,
			want:         false,
		},
		{
			name:         "declared tools, stop, some calls",
			declared:     2,
			calls:        1,
			finishReason: "stop",
			outcome:      reqlog.OutcomeSuccess,
			want:         false,
		},
		{
			name:         "declared tools, finish_reason=length",
			declared:     1,
			calls:        0,
			finishReason: "length",
			outcome:      reqlog.OutcomeSuccess,
			want:         false,
		},
		{
			name:         "declared tools, finish_reason=tool_calls",
			declared:     1,
			calls:        0,
			finishReason: "tool_calls",
			outcome:      reqlog.OutcomeSuccess,
			want:         false,
		},
		{
			name:         "declared tools, empty finish_reason",
			declared:     1,
			calls:        0,
			finishReason: "",
			outcome:      reqlog.OutcomeSuccess,
			want:         false,
		},
		{
			// 只在整体成功时报，避免与空流/截断/error frame 的既有 WARN 重复。
			name:         "declared tools, stop, zero calls, interrupted",
			declared:     1,
			calls:        0,
			finishReason: "stop",
			outcome:      reqlog.OutcomeInterrupted,
			want:         false,
		},
		{
			name:         "declared tools, stop, zero calls, stream error",
			declared:     1,
			calls:        0,
			finishReason: "stop",
			outcome:      reqlog.OutcomeStreamError,
			want:         false,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got := shouldWarnStopWithoutToolCalls(testCase.declared, testCase.calls, testCase.finishReason, testCase.outcome)
			if got != testCase.want {
				t.Fatalf("shouldWarnStopWithoutToolCalls(%d, %d, %q, %q) = %v, want %v",
					testCase.declared, testCase.calls, testCase.finishReason, testCase.outcome, got, testCase.want)
			}
		})
	}
}

// TestNonStreamFinishReason 校验从聚合响应取 finish_reason 的容错口径。
func TestNonStreamFinishReason(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		resp map[string]any
		want string
	}{
		{
			name: "stop",
			resp: map[string]any{"choices": []any{
				map[string]any{"finish_reason": "stop"},
			}},
			want: "stop",
		},
		{
			name: "missing choices",
			resp: map[string]any{},
			want: "",
		},
		{
			name: "missing finish_reason",
			resp: map[string]any{"choices": []any{map[string]any{}}},
			want: "",
		},
		{
			name: "non-string finish_reason",
			resp: map[string]any{"choices": []any{
				map[string]any{"finish_reason": 123},
			}},
			want: "",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			if got := nonStreamFinishReason(testCase.resp); got != testCase.want {
				t.Fatalf("nonStreamFinishReason() = %q, want %q", got, testCase.want)
			}
		})
	}
}

// stopNoToolCallsSSE 造一段「正文正常 + finish_reason=stop + 零 tool_calls」的流。
func stopNoToolCallsSSE() string {
	chunk := func(delta map[string]any, finish any) string {
		b, _ := json.Marshal(map[string]any{
			"id": "resp-1", "object": "chat.completion.chunk", "created": 1, "model": "glm-5.2",
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
		})

		return "data: " + string(b) + "\n\n"
	}

	return chunk(map[string]any{"role": "assistant"}, nil) +
		chunk(map[string]any{"content": "我确认一下。"}, nil) +
		chunk(map[string]any{}, "stop") +
		"data: [DONE]\n\n"
}

// warnNeedle 是 issue #44 那条 WARN 的稳定片段。
const warnNeedle = "finish_reason=stop without tool_calls"

// TestStopWithoutToolCallsWarn 端到端钉住三条验收：声明 tools + stop + 零调用分别
// 在流式与非流式路径各留一条 WARN；未声明 tools 的纯对话 stop 一条都不留。
//
//nolint:paralleltest // 用例改写进程级日志出口，必须串行执行
func TestStopWithoutToolCallsWarn(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantWarn bool
	}{
		{
			name:     "stream declared tools stop zero calls",
			body:     `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}],` + declaredToolsBody + `}`,
			wantWarn: true,
		},
		{
			name:     "non-stream declared tools stop zero calls",
			body:     `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],` + declaredToolsBody + `}`,
			wantWarn: true,
		},
		{
			// 核心反例：纯对话的 stop 完全正常。
			name:     "stream plain chat stop",
			body:     `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			wantWarn: false,
		},
		{
			name:     "non-stream plain chat stop",
			body:     `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`,
			wantWarn: false,
		},
	}

	//nolint:paralleltest // 同上：日志捕获期间不能并行
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			// 网关无论客户端是否 stream，都向上游发起流式请求并以 SSE 聚合
			// （非流式由 Aggregate 拼装），所以假上游一律回 SSE。
			up := newFakeUpstream(t, func(string) (int, string, bool) {
				return http.StatusOK, stopNoToolCallsSSE(), true
			})
			handler := NewHandler(&Config{
				Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
				Upstream: up,
			})

			logs := captureLog(t)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(
				http.MethodPost, "/v1/chat/completions", strings.NewReader(testCase.body),
			))

			if rec.Code != http.StatusOK {
				t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
			}

			got := strings.Contains(logs.String(), warnNeedle)
			if got != testCase.wantWarn {
				t.Fatalf("WARN 出现=%v want %v; logs=%s", got, testCase.wantWarn, logs.String())
			}

			if testCase.wantWarn {
				for _, want := range []string{"acct=u1", "model=glm-5.2", "despite 1 tool(s) declared"} {
					if !strings.Contains(logs.String(), want) {
						t.Fatalf("WARN 缺少 %q; logs=%s", want, logs.String())
					}
				}
			}
		})
	}
}
