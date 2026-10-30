package upstream

import (
	"errors"
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

// TestProbeStreamLineTooLong 预览阶段单行超上限（issue #113）：总量上限是读完
// 一行之后才判的，挡不住单行把内存撑到任意大小。此时标记 SizeLimited 且不回放
// 半行，由 handler 按 upstream_parse 收尾。
func TestProbeStreamLineTooLong(t *testing.T) {
	t.Parallel()

	huge := "data: " + strings.Repeat("x", MaxSSELineBytes)

	probe := ProbeStream(strings.NewReader(huge + "\n\n"))

	if !probe.SizeLimited {
		t.Fatal("SizeLimited=false，超长单行未被识别")
	}

	if probe.Kind != ErrNone {
		t.Errorf("kind=%v want ErrNone（分类交给 handler）", probe.Kind)
	}

	if len(probe.buffered) > probeMaxBytes {
		t.Errorf("buffered=%d bytes want <= %d（不保留超长行）", len(probe.buffered), probeMaxBytes)
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

// TestTextContinueReaderLineTooLong 单行超限（issue #113）：续写层此前用
// ReadString 无上限拼接，超长无换行行能把内存撑到任意大小；现在与 stream /
// Aggregate 同口径，超限即按上游失败上抛，不留残缺行。
func TestTextContinueReaderLineTooLong(t *testing.T) {
	t.Parallel()

	huge := strings.Repeat("x", MaxSSELineBytes+1024)

	r := NewTextContinueReader(closeString{strings.NewReader(huge)}, "", false, nil)
	defer r.Close()

	buf := make([]byte, 64*1024)

	var err error

	for range 16 {
		_, err = r.Read(buf)
		if err != nil {
			break
		}
	}

	if !errors.Is(err, ErrSSELineTooLong) {
		t.Fatalf("err=%v want ErrSSELineTooLong", err)
	}

	if errors.Is(err, io.EOF) {
		t.Fatal("超长行不得按正常 EOF 收尾")
	}
}

// TestTextContinueReaderReleaseStopsReads Release 之后不得还能读到数据：行缓冲
// 已还给全局池，任何残留读取都会读到别的流的内容（幂等 + nil 安全也不得 panic）。
func TestTextContinueReaderReleaseStopsReads(t *testing.T) {
	t.Parallel()

	r := NewTextContinueReader(closeString{strings.NewReader("data: hi\n\n")}, "", false, nil)
	r.Release()

	if _, err := r.Read(make([]byte, 16)); !errors.Is(err, io.EOF) {
		t.Fatalf("err=%v want io.EOF（Release 后不得再读）", err)
	}

	r.Release() // 幂等

	var nilReader *TextContinueReader

	nilReader.Release() // nil 安全
}

// TestTextContinueReaderBufferPooled 行缓冲必须跨流复用：不池化时每流一次 64KB
// 分配，是流式请求里最大的一笔垃圾（实测占 B/op 约 2/3）。
//
//nolint:paralleltest // testing.AllocsPerRun 不能在并行测试里调用（会直接 panic）。
func TestTextContinueReaderBufferPooled(t *testing.T) {
	// 池在不同 P 上是私有的，AllocsPerRun 固定在同一 goroutine 内跑。
	// src 建在闭包外：每次新建 strings.Reader 自带一次分配，会盖住要测的那笔。
	src := closeString{strings.NewReader("")}
	NewTextContinueReader(src, "", false, nil).Release()

	allocs := testing.AllocsPerRun(64, func() {
		r := NewTextContinueReader(src, "", false, nil)
		r.Release()
	})

	// 复用命中时只剩 reader 结构体本身；未复用时还会多出 bufio.Reader + 64KB 缓冲。
	if allocs > 2 {
		t.Fatalf("allocs/op=%v want <=2（64KB 行缓冲未被复用）", allocs)
	}
}

// TestStreamProbeReleaseStopsReads Release 之后不得还能读到上游剩余流：行缓冲
// 已还给全局池，残留读取会读到别的请求的内容（幂等 + nil 安全也不得 panic）。
func TestStreamProbeReleaseStopsReads(t *testing.T) {
	t.Parallel()

	probe := ProbeStream(strings.NewReader(
		"data: {\"choices\":[{\"delta\":{\"content\":\"你\"}}]}\n\ndata: tail\n\n"))
	if probe.Text != "你" {
		t.Fatalf("Text=%q want 你", probe.Text)
	}

	// Release 前：已预览字节 + 上游剩余流都能读到。
	rest, err := io.ReadAll(probe.Reader())
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	if !strings.Contains(string(rest), "tail") {
		t.Fatalf("rest=%q want 含上游剩余流", rest)
	}

	probe.Release()

	// Release 后：只剩已预览字节，剩余流不再可达。
	rest, err = io.ReadAll(probe.Reader())
	if err != nil {
		t.Fatalf("ReadAll after Release: %v", err)
	}

	if strings.Contains(string(rest), "tail") {
		t.Fatalf("rest=%q Release 后不得再读上游", rest)
	}

	probe.Release() // 幂等

	var nilProbe *StreamProbe

	nilProbe.Release() // nil 安全
}

// TestStreamProbeReaderPooled 预览行缓冲必须跨请求复用：不池化时每请求一次 4KB
// 分配（流式 B/op 里除 JSON 解码外最大的一笔）。
//
//nolint:paralleltest // testing.AllocsPerRun 不能在并行测试里调用（会直接 panic）。
func TestStreamProbeReaderPooled(t *testing.T) {
	// 池在不同 P 上是私有的，AllocsPerRun 固定在同一 goroutine 内跑。
	// src 建在闭包外：每次新建 strings.Reader 自带一次分配，会盖住要测的那笔。
	src := strings.NewReader("")
	ProbeStream(src).Release()

	allocs := testing.AllocsPerRun(64, func() {
		ProbeStream(src).Release()
	})

	// 复用命中时只剩 StreamProbe 结构体本身；未复用时还会多出 bufio.Reader + 4KB 缓冲。
	if allocs > 1 {
		t.Fatalf("allocs/op=%v want <=1（4KB 预览行缓冲未被复用）", allocs)
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

// TestTextContinueReaderPreservesLengthWithSeparateUsage 末帧 length 之后还有独立
// usage 帧时，续写失败必须同时保留 length 与 usage（issue #247）。
func TestTextContinueReaderPreservesLengthWithSeparateUsage(t *testing.T) {
	t.Parallel()

	first := `data: {"id":"a","model":"m","choices":[{"index":0,"delta":{"content":"hello "}}]}` + "\n\n" +
		`data: {"id":"a","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}` + "\n\n" +
		`data: {"id":"a","model":"m","usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}` + "\n\n" +
		"data: [DONE]\n\n"

	r := NewTextContinueReader(closeString{strings.NewReader(first)}, "", false, func(string) (io.ReadCloser, error) {
		return nil, io.ErrUnexpectedEOF
	})

	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(out), `"finish_reason":"length"`) {
		t.Fatalf("length frame lost after usage frame: %s", out)
	}

	if !strings.Contains(string(out), `"total_tokens":15`) {
		t.Fatalf("usage frame lost after length: %s", out)
	}
}

// TestTextContinueReaderMergesFirstUsage 续写成功时末帧 usage 必须包含首段用量，
// 不能只保留续写段（issue #247，与非流式 mergeUsageTotals 同口径）。
func TestTextContinueReaderMergesFirstUsage(t *testing.T) {
	t.Parallel()

	first := `data: {"id":"a","model":"m","choices":[{"index":0,"delta":{"content":"hello "}}]}` + "\n\n" +
		`data: {"id":"a","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12,"credit":1,"prompt_tokens_details":{"cached_tokens":3}}}` + "\n\n" +
		"data: [DONE]\n\n"
	second := `data: {"id":"b","model":"m","choices":[{"index":0,"delta":{"content":"world"},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":4,"total_tokens":24,"credit":2,"prompt_tokens_details":{"cached_tokens":5}}}` + "\n\n" +
		"data: [DONE]\n\n"

	r := NewTextContinueReader(closeString{strings.NewReader(first)}, "", false, func(string) (io.ReadCloser, error) {
		return closeString{strings.NewReader(second)}, nil
	})

	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		`"prompt_tokens":30`, `"completion_tokens":6`, `"total_tokens":36`, `"credit":3`, `"cached_tokens":8`,
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("merged usage missing %s: %s", want, out)
		}
	}

	if strings.Contains(string(out), `"finish_reason":"length"`) {
		t.Fatalf("length finish must be suppressed after successful continuation: %s", out)
	}
}

// TestTextContinueReaderNullFrame 续写段的顶层 null 帧必须原样透传：nil map 没有
// 可写字段，续写重写 id/model 时写 nil map 会 panic。
func TestTextContinueReaderNullFrame(t *testing.T) {
	t.Parallel()

	first := `data: {"id":"a","model":"m","choices":[{"index":0,"delta":{"content":"hello "}}]}` + "\n\n" +
		`data: {"id":"a","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	second := "data: null\n\n" +
		`data: {"id":"b","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"

	r := NewTextContinueReader(closeString{strings.NewReader(first)}, "", false, func(string) (io.ReadCloser, error) {
		return closeString{strings.NewReader(second)}, nil
	})

	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(out), "data: null") {
		t.Fatalf("null frame must be passed through: %s", out)
	}
}
