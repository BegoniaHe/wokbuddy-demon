package upstream

import (
	"net/http/httptest"
	"strings"
	"testing"
)

type sseTestEvent struct {
	name string
	id   string
	data []string
}

// parseSSETestEvents 按 WHATWG EventSource 规则解析输出：只有空行才 dispatch 事件，
// event:/id: 字段属于当前事件。用于断言帧边界，而不是断言字段书写顺序。
func parseSSETestEvents(t *testing.T, raw string) []sseTestEvent {
	t.Helper()

	var (
		events []sseTestEvent
		cur    sseTestEvent
	)

	dispatch := func() {
		if cur.name == "" && cur.id == "" && len(cur.data) == 0 {
			return
		}

		events = append(events, cur)
		cur = sseTestEvent{}
	}

	for line := range strings.SplitSeq(raw, "\n") {
		if line == "" {
			dispatch()

			continue
		}

		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")

		switch field {
		case "event":
			cur.name = value
		case "id":
			cur.id = value
		case "data":
			cur.data = append(cur.data, value)
		}
	}

	dispatch()

	return events
}

// TestNonDataLinesStayInSameEvent 非 data 行（`event:` / `id:` / 心跳注释）不得提前
// flush 已缓冲的 data：flush 会插入空行，把事件字段甩到下一个事件（issue #421）。
func TestNonDataLinesStayInSameEvent(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	sw := newStreamWriter(rec, nil)

	for _, line := range []string{"data: hello\n", "event: custom\n", "id: 42\n", "\n"} {
		if _, _, err := sw.handleStreamLine(line); err != nil {
			t.Fatalf("handleStreamLine(%q): %v", line, err)
		}
	}

	events := parseSSETestEvents(t, rec.Body.String())
	if len(events) != 1 {
		t.Fatalf("输出应只有一个事件，got %d: %q", len(events), rec.Body.String())
	}

	ev := events[0]
	if ev.name != "custom" || ev.id != "42" || strings.Join(ev.data, "\n") != "hello" {
		t.Fatalf("事件字段被拆分: %+v (raw %q)", ev, rec.Body.String())
	}
}

// TestCommentDoesNotSplitBufferedData 心跳注释同样不得在 data 与空行之间制造事件边界。
func TestCommentDoesNotSplitBufferedData(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	sw := newStreamWriter(rec, nil)

	for _, line := range []string{"data: hello\n", ": keepalive\n", "\n"} {
		if _, _, err := sw.handleStreamLine(line); err != nil {
			t.Fatalf("handleStreamLine(%q): %v", line, err)
		}
	}

	events := parseSSETestEvents(t, rec.Body.String())
	if len(events) != 1 {
		t.Fatalf("输出应只有一个事件，got %d: %q", len(events), rec.Body.String())
	}

	if got := strings.Join(events[0].data, "\n"); got != "hello" {
		t.Fatalf("data 与注释被拆成两个事件: %q (raw %q)", got, rec.Body.String())
	}
}
