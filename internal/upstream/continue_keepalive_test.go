// continue_keepalive_test.go 换段等待期的 SSE 注释心跳（#578）。
//
// 续写 / 救援 / 盲重试每一段都是全新的上游 POST（十万级 prompt 的 prefill 常
// 3~15s，慢时 30s+）。网关缺省的入站 keepalive 是关闭的（upstream.keepalive_interval
// = "0"），同步等换段会让客户端在这段窗口里零字节，idle 超时直接断流。
package upstream

import (
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// continueTestFirst 首段：正文 + length 截断 + [DONE]。
const continueTestFirst = `data: {"id":"a","model":"m","choices":[{"index":0,"delta":{"content":"hello "}}]}` + "\n\n" +
	`data: {"id":"a","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}` + "\n\n" +
	"data: [DONE]\n\n"

// errUpstream500 换段失败用例的固定错误（err113：测试里也不定义动态 error）。
var errUpstream500 = errors.New("upstream 500")

// continueTestSecond 续写段：正文 + stop + [DONE]。
const continueTestSecond = `data: {"id":"b","model":"m","choices":[{"index":0,"delta":{"content":"world"}}]}` + "\n\n" +
	`data: {"id":"b","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
	"data: [DONE]\n\n"

// TestTextContinueReaderKeepaliveDuringSwitch 换段等待期按间隔补注释帧：客户端看到的
// 最大静默不超过心跳间隔（用例留 4 倍容差给调度/CI 抖动），且终态契约不变。
func TestTextContinueReaderKeepaliveDuringSwitch(t *testing.T) {
	t.Parallel()

	const (
		interval = 40 * time.Millisecond
		wait     = 400 * time.Millisecond
	)

	r := NewTextContinueReader(closeString{strings.NewReader(continueTestFirst)}, "", false,
		func(string, TailKind) (io.ReadCloser, error) {
			time.Sleep(wait)

			return closeString{strings.NewReader(continueTestSecond)}, nil
		}).ContinueKeepalive(interval)
	defer r.Close()

	got, keepalives, maxGap := readWithGaps(t, r)

	// 400ms / 40ms ≈ 10 帧；留出首帧与退出时的边界余量。
	if keepalives < 2 {
		t.Fatalf("keepalive 帧数=%d want >= 2（换段等待期静默）", keepalives)
	}

	if maxGap > 4*interval {
		t.Errorf("最大静默=%v want <= %v（心跳间隔的 4 倍容差）", maxGap, 4*interval)
	}

	if !strings.Contains(got, "hello ") || !strings.Contains(got, "world") {
		t.Fatalf("正文缺失: %q", got)
	}

	if strings.Contains(got, `"finish_reason":"length"`) {
		t.Errorf("首段 length 终态未扣下: %q", got)
	}

	if n := strings.Count(got, "data: [DONE]"); n != 1 {
		t.Errorf("[DONE] 数量=%d want 1: %q", n, got)
	}
}

// TestTextContinueReaderKeepaliveSwitchFailureFinishes 换段失败（上游 500）仍按首段
// 终态收尾：补发扣下的 length 帧与唯一一条 [DONE]，Read 不挂住。
func TestTextContinueReaderKeepaliveSwitchFailureFinishes(t *testing.T) {
	t.Parallel()

	r := NewTextContinueReader(closeString{strings.NewReader(continueTestFirst)}, "", false,
		func(string, TailKind) (io.ReadCloser, error) {
			time.Sleep(120 * time.Millisecond)

			return nil, errUpstream500
		}).ContinueKeepalive(20 * time.Millisecond)
	defer r.Close()

	done := make(chan string, 1)

	go func() {
		out, err := io.ReadAll(r)
		if err != nil {
			t.Errorf("read: %v", err)
		}

		done <- string(out)
	}()

	select {
	case out := <-done:
		if !strings.Contains(out, `"finish_reason":"length"`) {
			t.Errorf("首段终态未补发: %q", out)
		}

		if n := strings.Count(out, "data: [DONE]"); n != 1 {
			t.Errorf("[DONE] 数量=%d want 1: %q", n, out)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("换段失败后 Read 挂住")
	}
}

// readWithGaps 读完 r，返回全部输出、注释心跳帧数与相邻可读块之间的最大间隔。
func readWithGaps(t *testing.T, r io.Reader) (string, int, time.Duration) {
	t.Helper()

	buf := make([]byte, 1024)

	var (
		out        strings.Builder
		keepalives int
		maxGap     time.Duration
		last       = time.Now()
	)

	for {
		n, err := r.Read(buf)

		now := time.Now()
		if n > 0 {
			if gap := now.Sub(last); gap > maxGap {
				maxGap = gap
			}

			last = now

			out.Write(buf[:n])

			keepalives += strings.Count(string(buf[:n]), sseKeepaliveLine)
		}

		if err != nil {
			if !errors.Is(err, io.EOF) {
				t.Fatalf("read: %v", err)
			}

			return out.String(), keepalives, maxGap
		}
	}
}

// closeRecorder 记录迟到的换段响应体是否被回收。
type closeRecorder struct{ closed atomic.Bool }

func (c *closeRecorder) Read([]byte) (int, error) { return 0, io.EOF }

func (c *closeRecorder) Close() error {
	c.closed.Store(true)

	return nil
}

// TestTextContinueReaderKeepaliveCloseReapsLateBody Close 发生在换段在途时：Read 立即
// 退出，迟到的响应体由发起方回收（客户端断开不泄漏 fd）。
func TestTextContinueReaderKeepaliveCloseReapsLateBody(t *testing.T) {
	t.Parallel()

	late := &closeRecorder{}
	opened := make(chan struct{})
	release := make(chan struct{})

	r := NewTextContinueReader(closeString{strings.NewReader(continueTestFirst)}, "", false,
		func(string, TailKind) (io.ReadCloser, error) {
			close(opened)
			<-release

			return late, nil
		}).ContinueKeepalive(20 * time.Millisecond)
	defer r.Close()

	readDone := make(chan error, 1)

	go func() {
		_, err := io.ReadAll(r)
		readDone <- err
	}()

	<-opened

	_ = r.Close()

	select {
	case <-readDone:
	case <-time.After(time.Second):
		t.Fatal("Close 后 Read 未退出")
	}

	close(release)

	deadline := time.Now().Add(2 * time.Second)
	for !late.closed.Load() {
		if time.Now().After(deadline) {
			t.Fatal("迟到的换段响应体未被回收")
		}

		time.Sleep(5 * time.Millisecond)
	}
}

// TestTextContinueReaderSwitchPanicIsolated 换段协程里 panic 必须隔离：换段跑在独立
// goroutine 上，没有 net/http 的按请求 recover，未隔离就是进程级终止。隔离后按换段
// 失败收尾（补发首段终态与唯一一条 [DONE]），不向上抛。
func TestTextContinueReaderSwitchPanicIsolated(t *testing.T) {
	t.Parallel()

	r := NewTextContinueReader(closeString{strings.NewReader(continueTestFirst)}, "", false,
		func(string, TailKind) (io.ReadCloser, error) {
			panic("boom")
		}).ContinueKeepalive(20 * time.Millisecond)
	defer r.Close()

	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if !strings.Contains(string(out), `"finish_reason":"length"`) {
		t.Errorf("首段终态未补发: %q", out)
	}

	if n := strings.Count(string(out), "data: [DONE]"); n != 1 {
		t.Errorf("[DONE] 数量=%d want 1: %q", n, out)
	}
}
