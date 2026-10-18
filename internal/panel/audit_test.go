package panel

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAuditAppendAndRecent(t *testing.T) {
	dir := t.TempDir()
	a := newAuditLog(dir)
	a.Append(auditEntry{Method: "POST", Path: "/panel/api/a", Status: 200, OK: true})
	a.Append(auditEntry{Method: "POST", Path: "/panel/api/b", Status: 500})

	got := a.Recent(10)
	if len(got) != 2 {
		t.Fatalf("entries=%d want 2", len(got))
	}
	if got[0].Path != "/panel/api/b" || got[1].Path != "/panel/api/a" {
		t.Fatalf("order=%+v want newest first", got)
	}
	if got[1].Time == "" {
		t.Fatal("time should be filled")
	}
	if only := a.Recent(1); len(only) != 1 || only[0].Path != "/panel/api/b" {
		t.Fatalf("limit not applied: %+v", only)
	}
}

func TestAuditRotation(t *testing.T) {
	dir := t.TempDir()
	a := newAuditLog(dir)
	a.maxBytes = 120
	a.backups = 2
	a.now = func() time.Time { return time.Unix(0, 0) }
	for range 20 {
		a.Append(auditEntry{Method: "POST", Path: "/panel/api/some/long/path/for/rotation"})
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
	dir := t.TempDir()
	p := New(Config{Version: "test", AuditDir: dir})
	// POST 写操作：Handler 未注入 SaveConfig → 501，但仍应留痕。
	req := httptest.NewRequest(http.MethodPost, "/panel/api/config", strings.NewReader("SECRET_BODY"))
	req.Header.Set("User-Agent", "audit-test")
	req.RemoteAddr = "198.51.100.9:1234"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	entries := p.audit.Recent(10)
	if len(entries) != 1 {
		t.Fatalf("entries=%d want 1", len(entries))
	}
	e := entries[0]
	if e.Method != http.MethodPost || e.Path != "/panel/api/config" || e.Status != http.StatusNotImplemented {
		t.Fatalf("entry=%+v", e)
	}
	if e.IP != "198.51.100.9" || e.UserAgent != "audit-test" {
		t.Fatalf("source not recorded: %+v", e)
	}

	// GET 不写审计。
	p.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/panel/api/audit", nil))

	raw, err := os.ReadFile(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "SECRET_BODY") {
		t.Fatal("audit must not record request body")
	}
	if got := len(p.audit.Recent(10)); got != 1 {
		t.Fatalf("GET should not be audited, entries=%d", got)
	}
}
