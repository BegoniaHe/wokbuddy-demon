// browsercomplete_test.go 钉住独立控制标记 `::browser-complete{...}` 的处置（issue #629）：
// 整段只有一条标记时不下发到客户端；带工具上下文时补一次续写，纯聊天只吞标记；
// 标记混在正文里、或流断在 `}` 之前时原样透传。
package upstream

import (
	"io"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/jsonx"
)

// browserMarker 一条完整的独立控制标记。
const browserMarker = `::browser-complete{title="Task"}`

// sseToolCall 续写段：带回一个工具调用 + tool_calls 终态。
const sseToolCall = "data: {\"id\":\"b\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[" +
	"{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"browser_eval\",\"arguments\":\"{}\"}}]}}]}\n\n"

// sseStopDone 首段终态：stop + [DONE]。
const sseStopDone = `data: {"id":"a","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` +
	"\n\n" + "data: [DONE]\n\n"

// sseContent 把若干正文片段编码成 SSE 正文帧（片段顺序即 delta 顺序）。
func sseContent(parts ...string) string {
	var frames strings.Builder

	for _, part := range parts {
		frames.WriteString(`data: {"id":"a","model":"m","choices":[{"index":0,"delta":{"content":`)
		frames.WriteString(strconv.Quote(part))
		frames.WriteString("}}]}\n\n")
	}

	return frames.String()
}

// readAllT 读完整条流并返回原文。
func readAllT(t *testing.T, r io.Reader) string {
	t.Helper()

	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	return string(out)
}

// streamedContent 依次取出所有帧里的 delta 正文并拼接（跨帧拆分的标记才能还原）。
func streamedContent(t *testing.T, stream string) string {
	t.Helper()

	var content strings.Builder

	for raw := range strings.SplitSeq(stream, "\n") {
		payload, isData := sseDataPayload(strings.TrimRight(raw, "\r\n"))
		if !isData || sseDone(payload) {
			continue
		}

		var frame struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}

		if jsonx.Unmarshal([]byte(payload), &frame) != nil {
			continue
		}

		for _, choice := range frame.Choices {
			content.WriteString(choice.Delta.Content)
		}
	}

	return content.String()
}

// TestBrowserCompleteOnly 判定口径：只有「整条标记 + 前后空白」才算独立标记。
func TestBrowserCompleteOnly(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		text string
		want bool
	}{
		{"独立标记", browserMarker, true},
		{"前后空白", "  " + browserMarker + "\n", true},
		{"带参数", `::browser-complete{title="A" url="https://x"}`, true},
		{"标记前有正文", "说明：" + browserMarker, false},
		{"标记后有正文", browserMarker + " 然后继续说明", false},
		{"参数体未闭合", `::browser-complete{title="Task"`, false},
		{"普通正文", "任务已经做完了。", false},
		{"空正文", "", false},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			if got := BrowserCompleteOnly(testCase.text); got != testCase.want {
				t.Errorf("BrowserCompleteOnly(%q)=%v want %v", testCase.text, got, testCase.want)
			}
		})
	}
}

// TestTextContinueReaderBrowserCompleteSwallowed 纯聊天轮：标记（跨 delta 拆分）不下发，
// 也不额外发起一次上游调用。
func TestTextContinueReaderBrowserCompleteSwallowed(t *testing.T) {
	t.Parallel()

	r := NewTextContinueReader(
		closeString{strings.NewReader(sseContent("::browser-", `complete{title="Task"}`) + sseStopDone)},
		"", false,
		func(string, TailKind) (io.ReadCloser, error) {
			t.Error("无工具上下文时不应发起续写")

			return nil, errUpstream500
		}).ContinueOnBrowserComplete(false)
	defer r.Close()

	out := readAllT(t, r)
	if got := streamedContent(t, out); got != "" {
		t.Errorf("独立控制标记被透传给客户端: %q", got)
	}

	if n := strings.Count(out, "[DONE]"); n != 1 {
		t.Errorf("[DONE] 数量=%d want 1: %q", n, out)
	}

	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Errorf("终态帧缺失（只该吞标记，不该吞收尾）: %q", out)
	}
}

// TestTextContinueReaderBrowserCompleteToolRoundContinues 工具上下文：吞掉标记后补一次
// 续写，续写形态是 TailBrowserComplete，续写正文拼进同一条流。
func TestTextContinueReaderBrowserCompleteToolRoundContinues(t *testing.T) {
	t.Parallel()

	var (
		gotPartial string
		gotKind    TailKind
	)

	r := NewTextContinueReader(
		closeString{strings.NewReader(sseContent("::browser-", `complete{title="Task"}`) + sseStopDone)},
		"", false,
		func(partial string, kind TailKind) (io.ReadCloser, error) {
			gotPartial, gotKind = partial, kind

			return closeString{strings.NewReader(sseContent("继续干活") + sseStopDone)}, nil
		}).ContinueOnBrowserComplete(true)
	defer r.Close()

	out := readAllT(t, r)
	forwarded := streamedContent(t, out)

	if gotKind != TailBrowserComplete {
		t.Errorf("续写形态=%v want browser-complete", gotKind)
	}

	if !strings.Contains(gotPartial, "::browser-complete") {
		t.Errorf("续写请求应带上模型刚发过的标记，got %q", gotPartial)
	}

	if strings.Contains(forwarded, "::browser-complete") {
		t.Errorf("控制标记被透传给客户端: %q", forwarded)
	}

	if !strings.Contains(forwarded, "继续干活") {
		t.Errorf("续写正文未拼进同一流: %q", forwarded)
	}
}

// TestTextContinueReaderBrowserCompleteTruncatedFlushed 流断在 `}` 之前：暂存内容原样回吐，
// 不能把正常正文吞掉，也不触发续写。
func TestTextContinueReaderBrowserCompleteTruncatedFlushed(t *testing.T) {
	t.Parallel()

	r := NewTextContinueReader(
		closeString{strings.NewReader(sseContent(`::browser-complete{title="Task"`))},
		"", false,
		func(string, TailKind) (io.ReadCloser, error) {
			t.Error("截断的标记不应触发续写")

			return nil, errUpstream500
		}).ContinueOnBrowserComplete(true)
	defer r.Close()

	out := readAllT(t, r)
	if got := streamedContent(t, out); !strings.Contains(got, `::browser-complete{title="Task"`) {
		t.Errorf("截断标记未原样回吐: %q", got)
	}
}

// TestTextContinueReaderBrowserCompleteWithTextPassesThrough 标记混在正文里时整段原样透传。
func TestTextContinueReaderBrowserCompleteWithTextPassesThrough(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		parts []string
	}{
		{"标记前有正文", []string{"说明：", browserMarker}},
		{"标记后有正文", []string{browserMarker, " 然后继续说明"}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			r := NewTextContinueReader(
				closeString{strings.NewReader(sseContent(testCase.parts...) + sseStopDone)}, "", false,
				func(string, TailKind) (io.ReadCloser, error) {
					t.Error("标记混在正文里时不应发起续写")

					return nil, errUpstream500
				}).ContinueOnBrowserComplete(true)
			defer r.Close()

			out := readAllT(t, r)
			if got := streamedContent(t, out); !strings.Contains(got, "::browser-complete") {
				t.Errorf("正文里的标记被误吞: %q", got)
			}
		})
	}
}

// TestBuildContinuationBodyBrowserComplete 续写请求的最后一条 user 消息必须点明
// 「这是浏览器完成控制标记」。
func TestBuildContinuationBodyBrowserComplete(t *testing.T) {
	t.Parallel()

	body := BuildContinuationBody([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`),
		browserMarker, TailBrowserComplete)

	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}

	if err := jsonx.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	last := req.Messages[len(req.Messages)-1]
	if last.Role != "user" {
		t.Errorf("末条消息角色=%q want user", last.Role)
	}

	if !strings.Contains(last.Content, "::browser-complete") || !strings.Contains(last.Content, "控制标记") {
		t.Errorf("续写指令未点明控制标记: %q", last.Content)
	}

	if !strings.Contains(last.Content, completionExit) {
		t.Errorf("续写指令应复用 completionExit 作为完成出口: %q", last.Content)
	}
}

// TestLastMessageIsToolResult 工具上下文的第二个判据：最后一条消息是不是工具结果。
func TestLastMessageIsToolResult(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
		want bool
	}{
		{"末条是工具结果", `{"messages":[{"role":"user"},{"role":"tool","content":"ok"}]}`, true},
		{"末条是用户消息", `{"messages":[{"role":"tool"},{"role":"user"}]}`, false},
		{"无消息", `{"messages":[]}`, false},
		{"坏 body", `{`, false},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			if got := LastMessageIsToolResult([]byte(testCase.body)); got != testCase.want {
				t.Errorf("LastMessageIsToolResult(%s)=%v want %v", testCase.body, got, testCase.want)
			}
		})
	}
}

// TestTextContinueReaderBrowserCompleteSingleRescue 同一段只救一次：即使尾部探针也在
// （工具轮 narration 口径），控制标记段只按 TailBrowserComplete 续写一次，不被
// narration / toolonly 再续一遍（issue #629 验收项）。
func TestTextContinueReaderBrowserCompleteSingleRescue(t *testing.T) {
	t.Parallel()

	var (
		opens atomic.Int32
		kinds []TailKind
	)

	r := NewTextContinueReader(
		closeString{strings.NewReader(sseContent(browserMarker) + sseStopDone)}, "", false,
		func(_ string, kind TailKind) (io.ReadCloser, error) {
			opens.Add(1)

			kinds = append(kinds, kind)

			// 续写段带回工具调用：工具轮只采纳调用，且不该再触发第二次续写。
			return closeString{strings.NewReader(sseToolCall + sseStopDone)}, nil
		}).
		ContinueOnStopTail(func(string) TailKind { return TailPromise }, true, nil).
		ContinueOnBrowserComplete(true)
	defer r.Close()

	_ = readAllT(t, r)

	if got := opens.Load(); got != 1 {
		t.Errorf("续写次数=%d want 1（同一段只救一次）", got)
	}

	if len(kinds) != 1 || kinds[0] != TailBrowserComplete {
		t.Errorf("续写形态=%v want [browser-complete]", kinds)
	}
}
