// runlog_test.go 钉住运行日志落盘的契约：写入前脱敏、按天命名、未启用时零副作用。
package runlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// TestSinkFlushesOnTick 无后续写入时也要按期落盘。
//
// 批量策略的时间条件原本只在「下一次 Write」时检查：低流量部署（调度器每 5 分钟
// 一行）最后一条会无限期留在 bufio 里，面板导出读磁盘文件、最新几条永远缺席。
func TestSinkFlushesOnTick(t *testing.T) { //nolint:paralleltest // 改包级 flushInterval，须与其他用例串行
	old := flushInterval

	flushInterval = 20 * time.Millisecond

	t.Cleanup(func() { flushInterval = old })

	sink := testSink(t)

	if _, err := sink.Write([]byte("tick-flush-line\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// 只等 ticker，不再写第二行（Write 的常规批量路径不会触发刷盘）。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		files, err := sink.Files()
		if err != nil {
			t.Fatalf("Files: %v", err)
		}

		for _, file := range files {
			raw, rerr := os.ReadFile(filepath.Join(sink.Dir(), file.Name))
			if rerr == nil && strings.Contains(string(raw), "tick-flush-line") {
				return
			}
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("定时刷盘未把日志写入磁盘")
}

// TestSinkCapsSingleLine 单行长度有上限：上游错误 body 不能把当前文件撑成无界。
func TestSinkCapsSingleLine(t *testing.T) {
	t.Parallel()

	sink := testSink(t)

	line := strings.Repeat("a", maxLineBytes*2) + "\n"
	if _, err := sink.Write([]byte(line)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if err := sink.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	files, err := sink.Files()
	if err != nil || len(files) == 0 {
		t.Fatalf("Files: %v (%d)", err, len(files))
	}

	raw, err := os.ReadFile(filepath.Join(sink.Dir(), files[len(files)-1].Name))
	if err != nil {
		t.Fatal(err)
	}

	if len(raw) > maxLineBytes+1 {
		t.Fatalf("落盘长度=%d 超过单行上限 %d", len(raw), maxLineBytes)
	}
}

// TestWriteKeepsMultiLineRecords 多行记录必须逐行落盘：log.Printf 传入 panic 栈时，
// 整段走 RedactForLog 会把换行压成空格再截到 8 KiB，堆栈在日志与面板导出里不可读。
func TestWriteKeepsMultiLineRecords(t *testing.T) {
	t.Parallel()

	sink := testSink(t)
	defer sink.Close()

	record := "ERR: 落盘循环 panic（已隔离）: boom\n" +
		"goroutine 1 [running]:\n" +
		"main.main()\n" +
		"\t/app/main.go:42 +0x1d\n"
	if _, err := sink.Write([]byte(record)); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := sink.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	files, err := sink.Files()
	if err != nil {
		t.Fatalf("files: %v", err)
	}

	if len(files) == 0 {
		t.Fatal("没有落盘文件")
	}

	raw, err := os.ReadFile(files[0].Path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if got := strings.Count(string(raw), "\n"); got < 4 {
		t.Fatalf("行数=%d want >=4（多行记录被压成一行）: %q", got, raw)
	}
}
