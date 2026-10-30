package upstream

import (
	"net/http/httptest"
	"testing"
)

// TestHandleStreamLineFlushesDataBeforeNonDataLine 非 data 行（`event:` / `id:` /
// 心跳注释）必须先落帧已缓冲的 data，否则同一事件内的数据与事件字段顺序颠倒
// （issue #421）。期望输出：先完整 data 帧（含结尾空行），再是 `event:` 行。
func TestHandleStreamLineFlushesDataBeforeNonDataLine(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	sw := newStreamWriter(rec, nil)

	for _, line := range []string{"data: hello\n", "event: x\n", "\n"} {
		if _, _, err := sw.handleStreamLine(line); err != nil {
			t.Fatalf("handleStreamLine(%q): %v", line, err)
		}
	}

	const want = "data: hello\n\nevent: x\n"
	if got := rec.Body.String(); got != want {
		t.Fatalf("输出顺序错误：got %q want %q", got, want)
	}
}
