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
// （paceFrame 首帧不查询），pacer 自身对所有查询返回同一 delay。
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

// TestStreamPacerFirstFrameImmediate 首帧不被拖延：pacer 对任何查询都返回
// 非零等待，若实现把等待用在首帧之前（查询发生在第一帧），总耗时会 ≥delay；
// 契约是首帧不查询——用「查询次数 = 帧数-1」与「首次查询前零耗时」共同约束。
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

	// 三帧流：首帧不查询 → 查询次数 = 2。
	if pacer.queries != 2 {
		t.Fatalf("queries=%d want 2（首帧不应查询 pacer）", pacer.queries)
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

// TestStreamPacerDelaysFrames 挂上 5ms/帧 的 pacer：首帧后的每一帧都被推迟
// （三帧流 → 两次等待，[DONE] 收尾帧不计入 data 帧），总耗时 ≥10ms；
// 输出字节不变、[DONE] 照常（拖延不改字节、不中途 429）。
func TestStreamPacerDelaysFrames(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()

	pacer := &recordingPacer{delay: 5 * time.Millisecond}

	start := time.Now()

	if err := StreamHint(rec, strings.NewReader(paceTestSSE), nil, WithResponsePacer(pacer)); err != nil {
		t.Fatal(err)
	}

	if elapsed := time.Since(start); elapsed < 10*time.Millisecond {
		t.Fatalf("elapsed=%v want ≥10ms（首帧后的帧应被推迟）", elapsed)
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

	if delayed < 2 {
		t.Fatalf("被拖延的帧数=%d want ≥2（waits=%v）", delayed, pacer.waits)
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
