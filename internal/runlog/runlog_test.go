// runlog_test.go 钉住运行日志落盘的契约：写入前脱敏、按天命名、未启用时零副作用。
package runlog

import (
	"os"
	"strings"
	"testing"
)

// testSink 构造指向临时目录的启用态出口。
func testSink(t *testing.T) *Sink {
	t.Helper()

	sink, err := New(Config{
		Dir:           t.TempDir(),
		Enabled:       true,
		RetentionDays: 7,
		MaxBytes:      1 << 20,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Cleanup(func() { _ = sink.Close() })

	if !sink.Enabled() {
		t.Fatal("sink 应处于启用态")
	}

	return sink
}

// TestSinkRedactsTokensBeforeDisk 含 token 的日志行落盘后必须已打码，键名保留可排障。
func TestSinkRedactsTokensBeforeDisk(t *testing.T) {
	t.Parallel()

	sink := testSink(t)

	line := `2026/11/13 ERR: [upstream] body={"access_token":"sk-secret-value","msg":"boom"}` + "\n"
	if n, err := sink.Write([]byte(line)); n != len(line) || err != nil {
		t.Fatalf("Write=%d,%v want %d,nil", n, err, len(line))
	}

	if err := sink.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	files, err := sink.Files()
	if err != nil {
		t.Fatalf("Files: %v", err)
	}

	if len(files) != 1 {
		t.Fatalf("files=%+v want 1", files)
	}

	raw, err := os.ReadFile(files[0].Path)
	if err != nil {
		t.Fatal(err)
	}

	assertRedactedLine(t, string(raw))
}

// assertRedactedLine 断言落盘行已打码、键名与换行保留。
func assertRedactedLine(t *testing.T, content string) {
	t.Helper()

	if strings.Contains(content, "sk-secret-value") {
		t.Errorf("落盘内容不得含凭证: %q", content)
	}

	if !strings.Contains(content, "access_token") {
		t.Errorf("键名应保留: %q", content)
	}

	if !strings.Contains(content, "[REDACTED]") {
		t.Errorf("应写入打码标记: %q", content)
	}

	if !strings.HasSuffix(content, "\n") {
		t.Errorf("落盘应保留行尾换行: %q", content)
	}
}

// TestSinkWritesDailyFile 文件名按天命名，前缀固定为 server-<day>.log。
func TestSinkWritesDailyFile(t *testing.T) {
	t.Parallel()

	sink := testSink(t)

	if _, err := sink.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}

	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}

	files, err := sink.Files()
	if err != nil {
		t.Fatal(err)
	}

	if len(files) != 1 || !strings.HasPrefix(files[0].Name, Prefix+"-") ||
		!strings.HasSuffix(files[0].Name, Ext) {
		t.Fatalf("files=%+v want 1 个 %s-<day>%s", files, Prefix, Ext)
	}

	if sink.Dir() == "" {
		t.Error("Dir 不应为空")
	}
}

// TestSinkDisabledIsNoop 未启用时返回 nil，Write 直接丢弃且不创建文件。
func TestSinkDisabledIsNoop(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	sink, err := New(Config{Dir: dir, Enabled: false})
	if err != nil || sink != nil {
		t.Fatalf("New=%v,%v want nil,nil", sink, err)
	}

	assertDisabledSink(t, sink, dir)
}

// assertDisabledSink 断言未启用出口全链路 no-op 且不创建文件。
func assertDisabledSink(t *testing.T, sink *Sink, dir string) {
	t.Helper()

	if sink.Enabled() {
		t.Error("nil sink 不应报告启用")
	}

	if n, err := sink.Write([]byte("dropped")); n != len("dropped") || err != nil {
		t.Fatalf("Write=%d,%v want 全量丢弃且无错", n, err)
	}

	if err := sink.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	files, err := sink.Files()
	if err != nil || files != nil {
		t.Fatalf("Files=%v,%v want nil,nil", files, err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("未启用不得创建文件: %v (err=%v)", entries, err)
	}
}
