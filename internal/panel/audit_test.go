package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAuditAppendAndRecent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	audit := newAuditLog(dir)
	audit.Append(&auditEntry{Method: "POST", Path: "/panel/api/a", Status: 200, OK: true})
	audit.Append(&auditEntry{Method: "POST", Path: "/panel/api/b", Status: 500})

	got := audit.Recent(10)
	if len(got) != 2 {
		t.Fatalf("entries=%d want 2", len(got))
	}

	if got[0].Path != "/panel/api/b" || got[1].Path != "/panel/api/a" {
		t.Fatalf("order=%+v want newest first", got)
	}

	if got[1].Time == "" {
		t.Fatal("time should be filled")
	}

	if only := audit.Recent(1); len(only) != 1 || only[0].Path != "/panel/api/b" {
		t.Fatalf("limit not applied: %+v", only)
	}
}

// 恰好写满上限的记录必须能重新读回：scanner 缓冲上限要覆盖行尾换行。
func TestAuditMaxLineIsReadable(t *testing.T) {
	t.Parallel()

	base, err := json.Marshal(&auditEntry{Method: "POST", Path: "/panel/api/x", Time: time.Unix(0, 0).UTC().Format(time.RFC3339)})
	if err != nil {
		t.Fatal(err)
	}

	path := "/panel/api/" + strings.Repeat("a", auditMaxLineBytes-len(base))

	line, err := json.Marshal(&auditEntry{Method: "POST", Path: path, Time: time.Unix(0, 0).UTC().Format(time.RFC3339)})
	if err != nil {
		t.Fatal(err)
	}

	if len(line)+1 != auditMaxLineBytes {
		t.Fatalf("line=%d want %d", len(line)+1, auditMaxLineBytes)
	}

	dir := t.TempDir()
	audit := newAuditLog(dir)
	audit.Append(&auditEntry{Method: "POST", Path: path})

	if got := audit.Recent(1); len(got) != 1 || got[0].Path != path {
		t.Fatalf("max-sized line not readable: %d entries", len(got))
	}
}

func TestAuditRotation(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	audit := newAuditLog(dir)
	audit.maxBytes = 120
	audit.backups = 2

	audit.now = func() time.Time { return time.Unix(0, 0) }
	for range 20 {
		audit.Append(&auditEntry{Method: "POST", Path: "/panel/api/some/long/path/for/rotation"})
	}

	if _, err := os.Stat(filepath.Join(dir, "audit.jsonl.1")); err != nil {
		t.Fatalf("missing audit.jsonl.1: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "audit.jsonl.2")); err != nil {
		t.Fatalf("missing audit.jsonl.2: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "audit.jsonl.3")); !os.IsNotExist(err) {
		t.Fatalf("audit.jsonl.3 should not exist, err=%v", err)
	}
}

func TestAuditMiddlewareRecordsMutationsOnly(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	panel := New(&Config{Version: "test", AuditDir: dir})
	// POST 写操作：Handler 未注入 SaveConfig → 501，但仍应留痕。
	req := httptest.NewRequest(http.MethodPost, "/panel/api/config", strings.NewReader("SECRET_BODY"))
	req.Header.Set("User-Agent", "audit-test")
	req.RemoteAddr = "198.51.100.9:1234"
	rec := httptest.NewRecorder()
	panel.ServeHTTP(rec, req)

	entries := panel.audit.Recent(10)
	if len(entries) != 1 {
		t.Fatalf("entries=%d want 1", len(entries))
	}

	entry := entries[0]
	if entry.Method != http.MethodPost || entry.Path != "/panel/api/config" || entry.Status != http.StatusNotImplemented {
		t.Fatalf("entry=%+v", entry)
	}

	if entry.IP != "198.51.100.9" || entry.UserAgent != "audit-test" {
		t.Fatalf("source not recorded: %+v", entry)
	}

	// GET 不写审计。
	panel.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/panel/api/audit", nil))

	raw, err := os.ReadFile(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(raw), "SECRET_BODY") {
		t.Fatal("audit must not record request body")
	}

	if got := len(panel.audit.Recent(10)); got != 1 {
		t.Fatalf("GET should not be audited, entries=%d", got)
	}
}

// panic 的管理面写操作也必须留审计：handler 可能在 panic 前已改了部分状态，
// 否则这类变更完全不可追溯（且响应是 500）。
func TestAuditRecordsPanickedMutation(t *testing.T) {
	t.Parallel()

	panel := New(&Config{Version: "test", AuditDir: t.TempDir()})
	panel.mux.HandleFunc("POST /panel/api/panic", func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})

	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("panic should propagate to net/http")
			}
		}()

		panel.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/panel/api/panic", nil))
	}()

	entries := panel.audit.Recent(1)
	if len(entries) != 1 {
		t.Fatalf("entries=%d want 1", len(entries))
	}

	if !entries[0].Panic || entries[0].Status != http.StatusInternalServerError || entries[0].OK {
		t.Fatalf("entry=%+v want panic/500/not-ok", entries[0])
	}
}
