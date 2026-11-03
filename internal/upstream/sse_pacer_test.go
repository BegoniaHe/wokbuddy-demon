package upstream

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// paceTestSSE 三帧流：两个正文帧 + 收尾帧 + [DONE]。
const paceTestSSE = `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"a"}}]}` + "\n\n" +
	`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"b"}}]}` + "\n\n" +
	`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
	"data: [DONE]\n\n"

// recordingPacer 记录每次查询的返回值。首帧零等待由 streamWriter 保证
// （writeSSE 首帧不查询），pacer 自身对所有查询返回同一 delay。
type recordingPacer struct {
	delay   time.Duration
	waits   []time.Duration
	queries int
}

func (p *recordingPacer) WaitBeforeWrite() time.Duration {
	p.queries++

	p.waits = append(p.waits, p.delay)

	return p.delay
}

// cancelablePacer 返回的等待很长，但 Done 关闭后 streamWriter 必须立刻停止等待。
type cancelablePacer struct {
	done    <-chan struct{}
	queried chan struct{}
}

func (p *cancelablePacer) WaitBeforeWrite() time.Duration {
	close(p.queried)

	return time.Hour
}

func (p *cancelablePacer) Done() <-chan struct{} { return p.done }

// TestStreamPacerFirstFrameImmediate 首帧不被拖延：pacer 对任何查询都返回
// 非零等待，若实现把等待用在首帧之前（查询发生在第一帧），总耗时会 ≥delay；
// 契约是首帧不查询——用「查询次数 = 数据帧-1」共同约束。
func TestStreamPacerFirstFrameImmediate(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()

	pacer := &recordingPacer{delay: 30 * time.Millisecond}

	start := time.Now()

	if err := StreamHint(rec, strings.NewReader(paceTestSSE), nil, WithResponsePacer(pacer)); err != nil {
		t.Fatal(err)
	}

	// 首帧立即：从开始到第一次查询之间不得有等待（首帧查询本身不发生，
	// 因此第一帧写出耗时 < delay）。
	if pacer.queries == 0 {
		t.Fatal("pacer 从未被查询（整形钩子未挂上）")
	}

	// 三帧 data + [DONE]：首帧不查询 → 查询次数 = 3。
	if pacer.queries != 3 {
		t.Fatalf("queries=%d want 3（首帧不查询，[DONE] 仍整形）", pacer.queries)
	}

	_ = start
}

// TestStreamWithoutPacerIsUnchanged 未挂 pacer 时逐字节不变（R1 护栏）：
// 零等待 pacer 与无 pacer 基线输出一致。
func TestStreamWithoutPacerIsUnchanged(t *testing.T) {
	t.Parallel()

	base := httptest.NewRecorder()
	if err := StreamHint(base, strings.NewReader(paceTestSSE), nil); err != nil {
		t.Fatal(err)
	}

	paced := httptest.NewRecorder()
	if err := StreamHint(paced, strings.NewReader(paceTestSSE), nil,
		WithResponsePacer(&recordingPacer{})); err != nil {
		t.Fatal(err)
	}

	if base.Body.String() != paced.Body.String() {
		t.Fatalf("零等待 pacer 改动了字节:\nbase=%s\ngot =%s", base.Body.String(), paced.Body.String())
	}
}

// TestStreamPacerDelaysFrames 挂上 5ms/帧 的 pacer：首帧后的每个 data 帧和
// [DONE] 都按同一出口整形（三帧 data + [DONE] → 三次等待），总耗时 ≥15ms；
// 输出字节不变、[DONE] 照常（拖延不改字节、不中途 429）。
func TestStreamPacerDelaysFrames(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()

	pacer := &recordingPacer{delay: 5 * time.Millisecond}

	start := time.Now()

	if err := StreamHint(rec, strings.NewReader(paceTestSSE), nil, WithResponsePacer(pacer)); err != nil {
		t.Fatal(err)
	}

	if elapsed := time.Since(start); elapsed < 15*time.Millisecond {
		t.Fatalf("elapsed=%v want ≥15ms（首帧后的 data 帧与 [DONE] 应被推迟）", elapsed)
	}

	if !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("输出缺少 [DONE]: %s", rec.Body.String())
	}

	delayed := 0

	for _, wait := range pacer.waits {
		if wait > 0 {
			delayed++
		}
	}

	if delayed < 3 {
		t.Fatalf("被拖延的帧数=%d want ≥3（waits=%v）", delayed, pacer.waits)
	}
}

// TestStreamPacerZeroWaitNoSleep pacer 恒返回 0 时实现不得插入睡眠：
// 零等待与无 pacer 的耗时同量级（毫秒级完成）。
func TestStreamPacerZeroWaitNoSleep(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()

	start := time.Now()

	if err := StreamHint(rec, strings.NewReader(paceTestSSE), nil, WithResponsePacer(&recordingPacer{})); err != nil {
		t.Fatal(err)
	}

	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("elapsed=%v：零等待不应有睡眠", elapsed)
	}
}

// TestStreamPacerClientWriteErrorStillClassified 客户端断开发生在拖延期间：
// 错误仍归类为 errClientWrite（不新增错误类）。
func TestStreamPacerClientWriteErrorStillClassified(t *testing.T) {
	t.Parallel()

	w := &failingWriter{err: errClientGone}

	err := StreamHint(w, strings.NewReader(paceTestSSE), nil,
		WithResponsePacer(&recordingPacer{delay: 2 * time.Millisecond}))
	if err == nil || !IsClientWriteError(err) {
		t.Fatalf("err=%v want client write error", err)
	}
}

// TestStreamPacerStopsOnCancellation 客户端断开后不得继续睡完整帧间等待；
// Done 关闭应立即返回客户端写失败，让请求退出并释放整形债务。
func TestStreamPacerStopsOnCancellation(t *testing.T) {
	t.Parallel()

	done := make(chan struct{})
	pacer := &cancelablePacer{done: done, queried: make(chan struct{})}
	finished := make(chan error, 1)

	go func() {
		finished <- StreamHint(httptest.NewRecorder(), strings.NewReader(paceTestSSE), nil,
			WithResponsePacer(pacer))
	}()

	select {
	case <-pacer.queried:
	case <-time.After(time.Second):
		t.Fatal("等待 pacer 查询超时")
	}

	close(done)

	select {
	case err := <-finished:
		if err == nil || !IsClientWriteError(err) {
			t.Fatalf("err=%v want client write error", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("取消后仍在等待")
	}
}

// TestStreamPacerCoversDoneMarker [DONE] 是实际 SSE data 帧，必须和正文帧共享
// 整形预算；仅一帧正文时，首帧立即、结束标记查询一次。
func TestStreamPacerCoversDoneMarker(t *testing.T) {
	t.Parallel()

	pacer := &recordingPacer{}

	raw := `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"a"}}]}` + "\n\n" +
		"data: [DONE]\n\n"

	if err := StreamHint(httptest.NewRecorder(), strings.NewReader(raw), nil,
		WithResponsePacer(pacer)); err != nil {
		t.Fatal(err)
	}

	if pacer.queries != 1 {
		t.Fatalf("queries=%d want 1（首帧不查询，[DONE] 查询一次）", pacer.queries)
	}
}

// TestStreamPacerCoversRepairExpandedFrames 标记修复把一帧展开成正文帧 + 调用帧
// 时，每个实际写出帧都必须独立查询 pacer，不能只在展开前查一次。
func TestStreamPacerCoversRepairExpandedFrames(t *testing.T) {
	t.Parallel()

	raw := upstreamSSEWithContent([]string{
		mkBlock(mkInvoke("exec_command", mkParam("cmd", "ls"))),
	}, "stop")

	rec := httptest.NewRecorder()

	pacer := &recordingPacer{}

	if err := StreamHint(rec, strings.NewReader(raw), nil,
		WithMarkupRepair(NewMarkupRepair(testTools, false)),
		WithResponsePacer(pacer)); err != nil {
		t.Fatal(err)
	}

	// role 帧、展开后的正文帧 + tool_call 帧、改写后的收尾帧、[DONE] = 5 帧。
	if pacer.queries != 4 {
		t.Fatalf("queries=%d want 4（修复展开的每个 data 帧均须整形；body=%s）", pacer.queries, rec.Body.String())
	}

	if strings.Contains(rec.Body.String(), markupPipe) {
		t.Fatalf("修复输出仍残留标记: %s", rec.Body.String())
	}
}

// TestStreamPacerSkipsNonDataLines 注释 / event / id 不是 data 帧，不得消耗整形
// 预算；只有真正写 data 时才查询 pacer。
func TestStreamPacerSkipsNonDataLines(t *testing.T) {
	t.Parallel()

	pacer := &recordingPacer{}
	sw := newStreamWriter(httptest.NewRecorder(), nil, WithResponsePacer(pacer))

	for _, line := range []string{": keepalive\n", "event: custom\n", "id: 42\n"} {
		if _, _, err := sw.handleStreamLine(line); err != nil {
			t.Fatal(err)
		}
	}

	if pacer.queries != 0 {
		t.Fatalf("queries=%d want 0（非 data 行不应整形）", pacer.queries)
	}
}
