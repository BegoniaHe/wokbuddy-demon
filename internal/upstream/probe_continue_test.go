package upstream

import (
	"io"
	"strings"
	"testing"
)

type closeString struct{ io.Reader }

func (closeString) Close() error { return nil }

// TestProbeStreamBusinessFrame 顶层 {code,msg} 被识别为可否决失败（issue #244）。
func TestProbeStreamBusinessFrame(t *testing.T) {
	t.Parallel()

	probe := ProbeStream(strings.NewReader("data: {\"code\":11133,\"msg\":\"messages is required\"}\n\n"))
	if probe.Kind != ErrBadParams {
		t.Fatalf("kind=%v want bad_params", probe.Kind)
	}

	if probe.Frame == "" {
		t.Fatal("frame should be captured")
	}
}

// TestProbeStreamZeroOutputLength 零输出 length 进入重发分支。
func TestProbeStreamZeroOutputLength(t *testing.T) {
	t.Parallel()

	frame := `data: {"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}` + "\n\n"
	probe := ProbeStream(strings.NewReader(frame))

	if !probe.ZeroOutputLength {
		t.Fatalf("ZeroOutputLength=false probe=%+v", probe)
	}
}

// TestProbeStreamContentFilterPassesThrough content_filter 是合法终态，不能当
// 账号故障轮转；预览应立即放行，把终态帧交给客户端。
func TestProbeStreamContentFilterPassesThrough(t *testing.T) {
	t.Parallel()

	frame := `data: {"choices":[{"index":0,"delta":{},"finish_reason":"content_filter"}]}` + "\n\n" +
		"data: [DONE]\n\n"

	probe := ProbeStream(strings.NewReader(frame))
	if probe.Kind != ErrNone || probe.ZeroOutputLength || probe.Empty || !probe.passThrough {
		t.Fatalf("probe=%+v want pass-through content_filter", probe)
	}

	out, err := io.ReadAll(probe.Reader())
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(out), `"finish_reason":"content_filter"`) {
		t.Fatalf("content_filter frame lost: %s", out)
	}
}

// TestTextContinueReaderSplicesLength 首段 length 后自动接续写段，客户端看到连续正文。
func TestTextContinueReaderSplicesLength(t *testing.T) {
	t.Parallel()

	first := `data: {"id":"a","model":"m","choices":[{"index":0,"delta":{"content":"hello "}}]}` + "\n\n" +
		`data: {"id":"a","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	second := `data: {"id":"b","model":"m","choices":[{"index":0,"delta":{"content":"world"}}]}` + "\n\n" +
		`data: {"id":"b","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"

	var partial string

	r := NewTextContinueReader(closeString{strings.NewReader(first)}, "", false, func(p string) (io.ReadCloser, error) {
		partial = p
		return closeString{strings.NewReader(second)}, nil
	})

	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(out), "hello ") || !strings.Contains(string(out), "world") {
		t.Fatalf("out=%s", out)
	}

	if strings.Contains(string(out), `"finish_reason":"length"`) {
		t.Fatalf("length finish must be suppressed after successful continuation: %s", out)
	}

	if strings.Contains(string(out), `"id":"b"`) {
		t.Fatalf("continuation frames must reuse first id: %s", out)
	}

	if partial != "hello " {
		t.Fatalf("partial=%q", partial)
	}
}

// TestTextContinueReaderToolNotRetried 工具调用截断不盲重试（issue #247）。
func TestTextContinueReaderToolNotRetried(t *testing.T) {
	t.Parallel()

	first := `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"apply_patch","arguments":"{\"in"}}]},"finish_reason":"length"}]}` + "\n\n" +
		"data: [DONE]\n\n"

	called := false

	r := NewTextContinueReader(closeString{strings.NewReader(first)}, "", false, func(string) (io.ReadCloser, error) {
		called = true

		return closeString{strings.NewReader("")}, nil
	})

	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}

	if called {
		t.Fatal("tool call truncation must not trigger continuation")
	}

	if !strings.Contains(string(out), `"finish_reason":"length"`) {
		t.Fatalf("length must be forwarded: %s", out)
	}
}

// TestTextContinueReaderContinueFailureAttemptedOnce 续写 opener 失败后不得在 EOF
// 处重试：重试会把续写帧写到已经发出的 [DONE] 之后（issue #247 的「次数上限 1 次」）。
func TestTextContinueReaderContinueFailureAttemptedOnce(t *testing.T) {
	t.Parallel()

	first := `data: {"id":"a","model":"m","choices":[{"index":0,"delta":{"content":"hello "}}]}` + "\n\n" +
		`data: {"id":"a","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}` + "\n\n" +
		"data: [DONE]\n\n"

	calls := 0

	r := NewTextContinueReader(closeString{strings.NewReader(first)}, "", false, func(string) (io.ReadCloser, error) {
		calls++

		return nil, io.ErrUnexpectedEOF
	})

	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}

	if calls != 1 {
		t.Fatalf("opener calls=%d want 1（续写失败不得重试）", calls)
	}

	if got := strings.Count(string(out), `"finish_reason":"length"`); got != 1 {
		t.Fatalf("length frame count=%d want 1: %s", got, out)
	}

	if got := strings.Count(string(out), "data: [DONE]"); got != 1 {
		t.Fatalf("[DONE] count=%d want 1: %s", got, out)
	}
}

// TestTextContinueReaderContinuesWhenDoneHasNoNewline 首段以无尾随换行的 [DONE]
// 结束时，handleLine 已切到续写流；旧 reader 的 EOF 不得把 done 置位吞掉续写段。
func TestTextContinueReaderContinuesWhenDoneHasNoNewline(t *testing.T) {
	t.Parallel()

	first := `data: {"id":"a","model":"m","choices":[{"index":0,"delta":{"content":"hello "}}]}` + "\n\n" +
		`data: {"id":"a","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}` + "\n\n" +
		"data: [DONE]"
	second := `data: {"id":"b","model":"m","choices":[{"index":0,"delta":{"content":"world"},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"

	calls := 0

	r := NewTextContinueReader(closeString{strings.NewReader(first)}, "", false, func(string) (io.ReadCloser, error) {
		calls++

		return closeString{strings.NewReader(second)}, nil
	})

	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}

	if calls != 1 {
		t.Fatalf("opener calls=%d want 1", calls)
	}

	if !strings.Contains(string(out), "world") {
		t.Fatalf("continuation lost after newline-less [DONE]: %s", out)
	}

	if strings.Contains(string(out), `"finish_reason":"length"`) {
		t.Fatalf("length finish must be suppressed: %s", out)
	}
}
