// logs_export_test.go 钉住运行日志 ZIP 导出（issue #487）：区间筛选、内容与落盘
// 一致且已脱敏、未启用与非法区间明确报错。
package panel

import (
	"archive/zip"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/runlog"
)

// exportTestSink 构造启用态运行日志出口与面板。
func exportTestSink(t *testing.T) (*Panel, *runlog.Sink) {
	t.Helper()

	sink, err := runlog.New(runlog.Config{
		Dir:           t.TempDir(),
		Enabled:       true,
		RetentionDays: 7,
		MaxBytes:      1 << 20,
	})
	if err != nil {
		t.Fatalf("runlog.New: %v", err)
	}

	t.Cleanup(func() { _ = sink.Close() })

	panel := New(&Config{Version: "test", APIKey: "k", RuntimeLog: sink})

	return panel, sink
}

// getExport 带面板凭据请求导出接口。
func getExport(pn *Panel, query string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/panel/api/logs/export"+query, nil)
	req.Header.Set("Authorization", "Bearer k")

	responseRecorder := httptest.NewRecorder()
	pn.ServeHTTP(responseRecorder, req)

	return responseRecorder
}

// zipEntries 解开 ZIP，返回 文件名 → 内容。
func zipEntries(t *testing.T, raw []byte) map[string]string {
	t.Helper()

	reader, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("ZIP 解析失败: %v (body 前 200 字节: %q)", err, truncateForTest(raw))
	}

	out := map[string]string{}

	for _, file := range reader.File {
		src, err := file.Open()
		if err != nil {
			t.Fatalf("打开 ZIP 条目 %s: %v", file.Name, err)
		}

		content, err := io.ReadAll(src)
		src.Close()

		if err != nil {
			t.Fatalf("读取 ZIP 条目 %s: %v", file.Name, err)
		}

		out[file.Name] = string(content)
	}

	return out
}

// truncateForTest 截断响应体用于失败提示。
func truncateForTest(raw []byte) string {
	if len(raw) > 200 {
		return string(raw[:200])
	}

	return string(raw)
}

// TestLogsExportContainsRedactedDay 导出当天：只含当天文件，token 已打码。
func TestLogsExportContainsRedactedDay(t *testing.T) {
	t.Parallel()

	panel, sink := exportTestSink(t)

	if _, err := sink.Write([]byte(`ERR: body={"access_token":"sk-secret-value"}` + "\n")); err != nil {
		t.Fatal(err)
	}

	// 区间外的旧文件：必须不被导出。
	stale := filepath.Join(sink.Dir(), "server-2026-09-01.log")
	if err := os.WriteFile(stale, []byte("old line\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	rec := getExport(panel, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if got := rec.Header().Get("Content-Type"); got != "application/zip" {
		t.Errorf("Content-Type=%q want application/zip", got)
	}

	entries := zipEntries(t, rec.Body.Bytes())
	today := "server-" + time.Now().Format("2006-01-02") + ".log"

	if len(entries) != 1 {
		t.Fatalf("entries=%v want 仅当天文件", entries)
	}

	content, ok := entries[today]
	if !ok {
		t.Fatalf("entries=%v 缺当天文件 %s", entries, today)
	}

	if strings.Contains(content, "sk-secret-value") {
		t.Errorf("导出内容不得含凭证: %q", content)
	}

	if !strings.Contains(content, "[REDACTED]") {
		t.Errorf("导出内容应为落盘时的脱敏文本: %q", content)
	}
}

// TestLogsExportRangeOnly 显式区间只导出区间内的文件。
func TestLogsExportRangeOnly(t *testing.T) {
	t.Parallel()

	panel, sink := exportTestSink(t)

	old := filepath.Join(sink.Dir(), "server-2026-09-01.log")
	if err := os.WriteFile(old, []byte("sep first\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	older := filepath.Join(sink.Dir(), "server-2026-08-01.log")
	if err := os.WriteFile(older, []byte("aug\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	rec := getExport(panel, "?from=2026-09-01&to=2026-09-01")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	entries := zipEntries(t, rec.Body.Bytes())
	if len(entries) != 1 {
		t.Fatalf("entries=%v want 1", entries)
	}

	if content := entries["server-2026-09-01.log"]; content != "sep first\n" {
		t.Errorf("content=%q want 原始落盘文本", content)
	}
}

// TestLogsExportDisabled 未启用落盘时返回 501。
func TestLogsExportDisabled(t *testing.T) {
	t.Parallel()

	panel := New(&Config{Version: "test", APIKey: "k"})

	if rec := getExport(panel, ""); rec.Code != http.StatusNotImplemented {
		t.Fatalf("code=%d want 501", rec.Code)
	}
}

// TestLogsExportRejectsBadRange 反向区间与超长区间明确报错。
func TestLogsExportRejectsBadRange(t *testing.T) {
	t.Parallel()

	panel, _ := exportTestSink(t)

	if rec := getExport(panel, "?from=2026-09-10&to=2026-09-01"); rec.Code != http.StatusBadRequest {
		t.Errorf("反向区间 code=%d want 400", rec.Code)
	}

	if rec := getExport(panel, "?from=2026-01-01&to=2026-03-01"); rec.Code != http.StatusBadRequest {
		t.Errorf("超长区间 code=%d want 400", rec.Code)
	}
}

// TestLogsExportEmptyRange 区间内无文件返回 404，而不是空 ZIP。
func TestLogsExportEmptyRange(t *testing.T) {
	t.Parallel()

	panel, _ := exportTestSink(t)

	if rec := getExport(panel, "?from=2026-09-01&to=2026-09-02"); rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d body=%s want 404", rec.Code, rec.Body)
	}
}
