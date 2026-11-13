// logrotate_test.go 钉住共用轮转写入器的契约：按天分文件、按大小上限轮转且不拆
// 记录、重启复用当天最大序号、按保留期/总容量裁剪旧文件。
package logrotate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestWriter 构造指向临时目录的写入器。
func newTestWriter(t *testing.T, cfg Config) *Writer {
	t.Helper()

	cfg.Dir = t.TempDir()
	cfg.Prefix = "server"
	cfg.Ext = ".log"

	writer, err := New(&cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Cleanup(func() { _ = writer.Close() })

	return writer
}

// mustWrite 写一条记录，失败即中止用例。
func mustWrite(t *testing.T, writer *Writer, now time.Time, line string) {
	t.Helper()

	if _, err := writer.WriteAt(now, []byte(line)); err != nil {
		t.Fatalf("WriteAt(%q): %v", line, err)
	}
}

// mustClose 关闭写入器，失败即中止用例。
func mustClose(t *testing.T, writer *Writer) {
	t.Helper()

	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestWriteRotatesBySize 单文件超限后按序号轮转，记录不被拆开。
func TestWriteRotatesBySize(t *testing.T) {
	t.Parallel()

	writer := newTestWriter(t, Config{FileMaxBytes: 6, MaxBytes: 1 << 20, RetentionDays: 7})
	now := time.Date(2026, 11, 13, 10, 0, 0, 0, time.Local)

	for _, line := range []string{"aaaa\n", "bbbb\n", "cccc\n"} {
		mustWrite(t, writer, now, line)
	}

	mustClose(t, writer)

	files, err := writer.Files()
	if err != nil {
		t.Fatal(err)
	}

	if len(files) != 3 {
		t.Fatalf("files=%+v want 3（FileMaxBytes=6，每条 5 字节记录各占一个文件）", files)
	}

	for index, file := range files {
		assertSingleRecordFile(t, file, index)
	}
}

// assertSingleRecordFile 断言文件序号正确且恰好是一条完整记录。
func assertSingleRecordFile(t *testing.T, file File, index int) {
	t.Helper()

	if file.Index != index {
		t.Errorf("files[%d].Index=%d want %d", index, file.Index, index)
	}

	raw, err := os.ReadFile(file.Path)
	if err != nil {
		t.Fatal(err)
	}

	if len(raw) != 5 || raw[4] != '\n' {
		t.Errorf("文件 %s 内容=%q，记录被拆开", file.Name, raw)
	}
}

// TestWriteSwitchesByDay 写入时刻跨天时切换到新文件。
func TestWriteSwitchesByDay(t *testing.T) {
	t.Parallel()

	writer := newTestWriter(t, Config{})
	day1 := time.Date(2026, 11, 12, 23, 59, 0, 0, time.Local)
	day2 := day1.Add(2 * time.Minute)

	mustWrite(t, writer, day1, "first\n")
	mustWrite(t, writer, day2, "second\n")
	mustClose(t, writer)

	files, err := writer.Files()
	if err != nil {
		t.Fatal(err)
	}

	if len(files) != 2 || files[0].Day.Format("2006-01-02") != "2026-11-12" ||
		files[1].Day.Format("2006-01-02") != "2026-11-13" {
		t.Fatalf("files=%+v want 两天各一个文件", files)
	}
}

// TestResumeLatestFileForDay 重启后复用当天序号最大的文件继续追加。
func TestResumeLatestFileForDay(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	now := time.Date(2026, 11, 13, 10, 0, 0, 0, time.Local)
	cfg := Config{Dir: dir, Prefix: "server", Ext: ".log"}

	first, err := New(&cfg)
	if err != nil {
		t.Fatal(err)
	}

	mustWrite(t, first, now, "one\n")
	mustClose(t, first)

	second, err := New(&cfg)
	if err != nil {
		t.Fatal(err)
	}

	mustWrite(t, second, now, "two\n")
	mustClose(t, second)

	files, err := second.Files()
	if err != nil {
		t.Fatal(err)
	}

	if len(files) != 1 {
		t.Fatalf("files=%+v want 1（同一天复用同一文件）", files)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "server-2026-11-13.log"))
	if err != nil {
		t.Fatal(err)
	}

	if string(raw) != "one\ntwo\n" {
		t.Fatalf("content=%q want one+two", raw)
	}
}

// TestPruneHonorsSizeAndRetention 超额与过期文件从最旧的开始删，当前文件不删。
func TestPruneHonorsSizeAndRetention(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	names := []string{
		"server-2026-09-20.log",
		"server-2026-09-21.log",
		"server-2026-09-22.log",
		"server-2026-09-23.log",
	}
	for i, name := range names {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(strings.Repeat("x", 40)), 0o600); err != nil {
			t.Fatal(err)
		}

		old := time.Now().AddDate(0, 0, -10+i)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}

	writer, err := New(&Config{
		Dir: dir, Prefix: "server", Ext: ".log", MaxBytes: 50, RetentionDays: 30,
	})
	if err != nil {
		t.Fatal(err)
	}

	writer.Prune(time.Now())

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 1 || entries[0].Name() != "server-2026-09-23.log" {
		t.Fatalf("remaining=%+v want newest only", entries)
	}
}

// TestPruneDropsFilesOlderThanRetention 只按保留期删除，当前写入文件保留。
func TestPruneDropsFilesOlderThanRetention(t *testing.T) {
	t.Parallel()

	writer := newTestWriter(t, Config{RetentionDays: 2, MaxBytes: 1 << 20})
	now := time.Now()

	mustWrite(t, writer, now, "current\n")

	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}

	stale := filepath.Join(writer.cfg.Dir, "server-2000-01-01.log")
	if err := os.WriteFile(stale, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	old := now.AddDate(0, 0, -10)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	writer.Prune(now)

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("过期文件应被删除: %v", err)
	}

	if _, err := os.Stat(writer.Path()); err != nil {
		t.Errorf("当前写入文件不应被删除: %v", err)
	}
}

// TestFilesIgnoresForeignNames 目录里的其它文件不参与轮转与裁剪。
func TestFilesIgnoresForeignNames(t *testing.T) {
	t.Parallel()

	writer := newTestWriter(t, Config{})
	dir := writer.cfg.Dir

	for _, name := range []string{"other-2026-11-13.log", "server-not-a-day.log", "server-2026-11-13.jsonl"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	files, err := writer.Files()
	if err != nil {
		t.Fatal(err)
	}

	if len(files) != 0 {
		t.Fatalf("files=%+v want 0（不匹配 prefix/ext/日期）", files)
	}
}

// TestNilWriterIsNoop 未启用时 nil 写入器全部方法可安全调用。
func TestNilWriterIsNoop(t *testing.T) {
	t.Parallel()

	var writer *Writer

	if n, err := writer.Write([]byte("x")); n != 1 || err != nil {
		t.Fatalf("Write=%d,%v", n, err)
	}

	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}

	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	if writer.Path() != "" {
		t.Errorf("Path=%q want empty", writer.Path())
	}

	writer.Prune(time.Now())
}

// TestNewEmptyDirDisabled Dir 为空返回 nil（调用方按未启用处理）。
func TestNewEmptyDirDisabled(t *testing.T) {
	t.Parallel()

	writer, err := New(&Config{Prefix: "server", Ext: ".log"})
	if err != nil || writer != nil {
		t.Fatalf("New=%v,%v want nil,nil", writer, err)
	}
}
