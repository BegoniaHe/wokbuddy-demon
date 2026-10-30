package server

import (
	"net/http/httptest"
	"testing"
)

// handleSSELine 是 responses / gemini 两面共用的单行分派，覆盖四类输入：
// 注释行原样转发、无空格 data 行、[DONE] 收尾、非 data 行忽略。
func TestHandleSSELine(t *testing.T) {
	t.Parallel()

	var (
		finished int
		chunks   []string
	)

	rec := httptest.NewRecorder()
	finish := func() error { finished++; return nil }
	chunk := func(payload string) error { chunks = append(chunks, payload); return nil }

	mustHandle := func(t *testing.T, line string) {
		t.Helper()

		if err := handleSSELine(rec, rec, line, finish, chunk); err != nil {
			t.Fatalf("handleSSELine(%q): %v", line, err)
		}
	}

	mustHandle(t, ": keepalive")

	if got, want := rec.Body.String(), ": keepalive\n\n"; got != want {
		t.Errorf("comment passthrough=%q want %q", got, want)
	}

	mustHandle(t, `data:{"a":1}`)

	if len(chunks) != 1 || chunks[0] != `{"a":1}` {
		t.Errorf("chunks=%q want [{\"a\":1}]", chunks)
	}

	mustHandle(t, "data: [DONE]")

	if finished != 1 {
		t.Errorf("finished=%d want 1", finished)
	}

	mustHandle(t, "event: ping")

	if len(chunks) != 1 || finished != 1 {
		t.Errorf("non-data line must be ignored: chunks=%d finished=%d", len(chunks), finished)
	}
}
