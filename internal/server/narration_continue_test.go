// narration_continue_test.go 钉住预告句与工具轮的单次续写（issue #495）：尾部是
// 冒号/省略号/未闭合代码块时续写一次；工具轮只采纳续写里的 tool_calls，续写失败
// 回落原样下发。
package server

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/jsonx"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

const sseNarrationStop = "data: {\"id\":\"c1\",\"choices\":[{\"index\":0," +
	"\"delta\":{\"content\":\"我先查一下文件：\"}}]}\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: [DONE]\n\n"

const sseTailText = "data: {\"id\":\"c2\",\"choices\":[{\"index\":0," +
	"\"delta\":{\"content\":\"续写正文\"}}]}\n\n" +
	"data: {\"id\":\"c2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: [DONE]\n\n"

// sseTailToolCall 续写段先吐一段正文（工具轮必须丢弃）再给工具调用。
const sseTailToolCall = "data: {\"id\":\"c2\",\"choices\":[{\"index\":0," +
	"\"delta\":{\"content\":\"不该出现的续写正文\"}}]}\n\n" +
	"data: {\"id\":\"c2\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0," +
	"\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"exec_command\"," +
	"\"arguments\":\"{\\\"cmd\\\":\\\"ls\\\"}\"}}]}}]}\n\n" +
	"data: {\"id\":\"c2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
	"data: [DONE]\n\n"

// TestClassifyTail 尾部形态判据（issue #495 / #570）：标点型悬停、无标点承诺句、
// 结构未闭合分别归位；完整句、等用户回话、超长标点型悬停不救。
func TestClassifyTail(t *testing.T) {
	t.Parallel()

	longPrefix := strings.Repeat("我把三处改动逐行过了一遍，其中两点需要单独说明。", 20) // ≈600B

	cases := []struct {
		name      string
		text      string
		toolRound bool
		want      upstream.TailKind
	}{
		{"full-width-colon", "文件结构如下：", false, upstream.TailPunctuated},
		{"ascii-colon", "The plan is:", false, upstream.TailPunctuated},
		{"ellipsis", "然后它就……", false, upstream.TailPunctuated},
		{"ascii-ellipsis", "and then...", false, upstream.TailPunctuated},
		{"open-fence", "```go\nfunc main() {", false, upstream.TailStructural},
		{"closed-fence", "```go\nfunc main() {}\n```", false, upstream.TailNone},
		{"complete-sentence", "配置项在 config.json 里。", false, upstream.TailNone},
		{"empty", "   ", false, upstream.TailNone},
		{"promise-short", "我将开始修改 internal/verify/check.go", false, upstream.TailPromise},
		{"promise-long", longPrefix + "我将开始修改 internal/verify/check.go", false, upstream.TailPromise},
		// 长度闸门只约束标点型悬停：超长的「停在冒号上」多半是正常排版。
		{"punctuated-too-long", longPrefix + "改动清单如下：", false, upstream.TailNone},
		// 交回控制权是故意的收尾，不能替用户拍板。
		{"yields-to-user", "我已经过了一遍，请确认后我再发送：", false, upstream.TailNone},
		// 中段的「是否需要」是技术叙述，只看末句窗口。
		{"mid-text-yield-marker", longPrefix + "为是否需要更换提供量化支撑。\n\n我将开始修改 check.go", false, upstream.TailPromise},
		// 工具轮句号收尾：意图词判没写完，收尾词优先判答完。
		{"tool-round-intent", "前面三处引用都已经逐个核对过，参数名与返回类型都对得上。我继续检查其余链接。", true, upstream.TailPromise},
		{"tool-round-closing", "前面三处引用都已经逐个核对过，参数名与返回类型都对得上。任务已全部完成，我来总结一下。", true, upstream.TailNone},
		{"tool-round-short", "已改完。", true, upstream.TailPromise},
		// 非工具轮不按最后一句的意图词救：那是普通叙述。
		{"plain-intent-sentence", "前面三处引用都已经逐个核对过，参数名与返回类型都对得上。我继续检查其余链接。", false, upstream.TailNone},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			if got := classifyTail(testCase.text, testCase.toolRound); got != testCase.want {
				t.Errorf("classifyTail(%q, toolRound=%v)=%v want %v", testCase.text, testCase.toolRound, got, testCase.want)
			}
		})
	}
}

// TestChatNarrationTailContinued 流式：预告句收尾时续写一次，第二段正文拼进同一流。
func TestChatNarrationTailContinued(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	fake := newFakeUpstream(t, func(string) (int, string, bool) {
		if calls.Add(1) == 1 {
			return http.StatusOK, sseNarrationStop, true
		}

		return http.StatusOK, sseTailText, true
	})
	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(&Config{Pool: state, Upstream: fake})

	rec := postChat(t, h, `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}

	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls=%d want 2（预告句续写一次）", got)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "我先查一下文件：") || !strings.Contains(body, "续写正文") {
		t.Errorf("两段正文都应下发: %s", body)
	}
}

// TestChatNarrationContinuationFailureKeepsFirst 续写失败：首段原样下发，不丢内容。
func TestChatNarrationContinuationFailureKeepsFirst(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	fake := newFakeUpstream(t, func(string) (int, string, bool) {
		if calls.Add(1) == 1 {
			return http.StatusOK, sseNarrationStop, true
		}

		return http.StatusInternalServerError, `{"code":10000,"msg":"boom"}`, false
	})
	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(&Config{Pool: state, Upstream: fake})

	rec := postChat(t, h, `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}

	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls=%d want 2（只续写一次）", got)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "我先查一下文件：") {
		t.Errorf("首段正文不应丢失: %s", body)
	}

	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Errorf("续写失败应保留首段 stop 终态: %s", body)
	}

	st := statusOf(t, state, "u1")
	if st.ConsecutiveFails != 0 || st.BreakerFails != 0 {
		t.Errorf("续写失败不应罚号: fails=%d breaker=%d", st.ConsecutiveFails, st.BreakerFails)
	}
}

// TestChatToolRoundAdoptsOnlyToolCalls 流式工具轮：声明了 tools、预告句收尾零调用，
// 续写一次且只采纳 tool_calls，续写正文丢弃。
func TestChatToolRoundAdoptsOnlyToolCalls(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	fake := newFakeUpstream(t, func(string) (int, string, bool) {
		if calls.Add(1) == 1 {
			return http.StatusOK, sseNarrationStop, true
		}

		return http.StatusOK, sseTailToolCall, true
	})
	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(&Config{Pool: state, Upstream: fake})

	body := `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}],` +
		declaredToolsBody + `}`
	rec := postChat(t, h, body)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}

	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls=%d want 2（工具轮续写一次）", got)
	}

	got := rec.Body.String()
	if !strings.Contains(got, `"exec_command"`) {
		t.Errorf("tool_calls 未下发: %s", got)
	}

	if strings.Contains(got, "不该出现的续写正文") {
		t.Errorf("工具轮续写正文应丢弃: %s", got)
	}

	if !strings.Contains(got, `"finish_reason":"tool_calls"`) {
		t.Errorf("终态应为 tool_calls: %s", got)
	}

	st := statusOf(t, state, "u1")
	if st.ConsecutiveFails != 0 || st.BreakerFails != 0 {
		t.Errorf("工具轮续写不应罚号: fails=%d breaker=%d", st.ConsecutiveFails, st.BreakerFails)
	}
}

// TestChatNonStreamNarrationContinued 非流式预告句：两段正文拼进同一响应。
func TestChatNonStreamNarrationContinued(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	fake := newFakeUpstream(t, func(string) (int, string, bool) {
		if calls.Add(1) == 1 {
			return http.StatusOK, sseNarrationStop, true
		}

		return http.StatusOK, sseTailText, true
	})
	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(&Config{Pool: state, Upstream: fake})

	rec := postChat(t, h, `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}

	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls=%d want 2（预告句续写一次）", got)
	}

	var got map[string]any
	if err := jsonx.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	content, _ := firstChoice(got)["message"].(map[string]any)["content"].(string)
	if content != "我先查一下文件：续写正文" {
		t.Errorf("content=%q want 两段拼接", content)
	}
}

// TestChatNonStreamToolRoundAdoptsOnlyToolCalls 非流式工具轮：保留预告句正文，
// 只并入续写的 tool_calls，续写正文丢弃。
func TestChatNonStreamToolRoundAdoptsOnlyToolCalls(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	fake := newFakeUpstream(t, func(string) (int, string, bool) {
		if calls.Add(1) == 1 {
			return http.StatusOK, sseNarrationStop, true
		}

		return http.StatusOK, sseTailToolCall, true
	})
	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(&Config{Pool: state, Upstream: fake})

	body := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],` +
		declaredToolsBody + `}`
	rec := postChat(t, h, body)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}

	var got map[string]any
	if err := jsonx.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	choice := firstChoice(got)
	message, _ := choice["message"].(map[string]any)

	if content, _ := message["content"].(string); content != "我先查一下文件：" {
		t.Errorf("content=%q want 只保留首段预告句", content)
	}

	if calls, _ := message["tool_calls"].([]any); len(calls) == 0 {
		t.Errorf("tool_calls 未并入: %v", message)
	}

	if finish, _ := choice["finish_reason"].(string); finish != "tool_calls" {
		t.Errorf("finish_reason=%q want tool_calls", finish)
	}
}
