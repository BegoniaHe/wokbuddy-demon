package safefile

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteKeepAndReadBackup(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.json")
	if err := WriteKeep(path, []byte(`{"v":1}`), 0o600, nil); err != nil {
		t.Fatal(err)
	}

	if err := WriteKeep(path, []byte(`{"v":2}`), 0o600, nil); err != nil {
		t.Fatal(err)
	}

	got, err := Read(path, nil)
	if err != nil || string(got) != `{"v":2}` {
		t.Fatalf("read=%s err=%v", got, err)
	}

	bak, err := os.ReadFile(path + ".bak")
	if err != nil || string(bak) != `{"v":1}` {
		t.Fatalf("bak=%s err=%v", bak, err)
	}

	// 正文损坏 → 回退 .bak。
	if writeErr := os.WriteFile(path, []byte(`{"v":`), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}

	got, err = Read(path, nil)
	if err != nil || string(got) != `{"v":1}` {
		t.Fatalf("fallback=%s err=%v", got, err)
	}
}

func TestWriteKeepDoesNotPoisonBackup(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"v":1}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := WriteKeep(path, []byte(`{"v":2}`), 0o600, nil); err != nil {
		t.Fatal(err)
	}

	// 正文坏了，但 .bak 仍是有效的 v1；再次 WriteKeep 不得用坏内容覆盖 .bak。
	if err := os.WriteFile(path, []byte("bad"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := WriteKeep(path, []byte(`{"v":3}`), 0o600, nil); err != nil {
		t.Fatal(err)
	}

	bak, err := os.ReadFile(path + ".bak")
	if err != nil || string(bak) != `{"v":1}` {
		t.Fatalf("bak=%s err=%v want v1", bak, err)
	}
}

// TestWriteKeepUsesCallerValidator 正文是合法 JSON 但通不过调用方语义校验时，
// 不得用它覆盖 .bak（auth 的有效性判定比 json.Valid 更严）。
func TestWriteKeepUsesCallerValidator(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(path+".bak", []byte(`{"v":1}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(`{"other":1}`), 0o600); err != nil {
		t.Fatal(err)
	}

	valid := func(b []byte) bool { return bytes.Contains(b, []byte(`"v"`)) }
	if err := WriteKeep(path, []byte(`{"v":2}`), 0o600, valid); err != nil {
		t.Fatal(err)
	}

	bak, err := os.ReadFile(path + ".bak")
	if err != nil || string(bak) != `{"v":1}` {
		t.Fatalf("bak=%s err=%v want v1", bak, err)
	}

	got, err := Read(path, valid)
	if err != nil || string(got) != `{"v":2}` {
		t.Fatalf("main=%s err=%v want v2", got, err)
	}
}

func TestReadMissing(t *testing.T) {
	t.Parallel()

	_, err := Read(filepath.Join(t.TempDir(), "missing.json"), nil)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err=%v want fs.ErrNotExist", err)
	}
}

func TestReadBothCorrupt(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("bad"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path+".bak", []byte("also bad"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Read(path, nil); err == nil {
		t.Fatal("expected error when both files are corrupt")
	}
}

// 软链部署（auths/x.json → 仓库外凭证目录）必须写到链接目标上：os.Rename 替换的
// 是链接自身，第一次写回就让链接消失、目标文件保持过期，且无任何提示。
func TestWriteFollowsSymlinkTarget(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "real.json")
	link := filepath.Join(dir, "link.json")

	if err := os.WriteFile(target, []byte(`{"old":true}`), 0o600); err != nil {
		t.Fatalf("seed target: %v", err)
	}

	if err := os.Symlink(target, link); err != nil {
		t.Skipf("当前平台不支持符号链接: %v", err)
	}

	if err := Write(link, []byte(`{"new":true}`), 0o600); err != nil {
		t.Fatalf("Write: %v", err)
	}

	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("Lstat link: %v", err)
	}

	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("符号链接被替换成了普通文件")
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}

	if string(got) != `{"new":true}` {
		t.Fatalf("链接目标未被更新: %s", got)
	}
}

// 进程被 kill -9 后遗留的临时文件（内含完整凭证）必须被下次写入前的清扫移除。
func TestWriteSweepsStaleTemps(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	stale := filepath.Join(dir, ".state.json.123456")

	if err := os.WriteFile(stale, []byte(`{"stale":true}`), 0o600); err != nil {
		t.Fatalf("seed stale: %v", err)
	}

	// 把 mtime 推到阈值之外（-2h）。
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	if err := Write(path, []byte(`{"ok":true}`), 0o600); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		t.Fatalf("过期临时文件未被清理: err=%v", err)
	}
}

// TestReadWithFallbackReportsBackup 回退到 .bak 时必须报告来源：调用方据此留痕，
// 否则「读到上一版旧凭据」与「文件正常」在 (data, nil) 下无法区分。
func TestReadWithFallbackReportsBackup(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path+".bak", []byte(`{"ok":true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	data, restored, err := ReadWithFallback(path, nil)
	if err != nil {
		t.Fatalf("err=%v", err)
	}

	if !restored {
		t.Fatal("读到 .bak 时必须报告 restored=true")
	}

	if string(data) != `{"ok":true}` {
		t.Fatalf("data=%s", data)
	}

	if werr := os.WriteFile(path, []byte(`{"ok":false}`), 0o600); werr != nil {
		t.Fatal(werr)
	}

	_, restored, err = ReadWithFallback(path, nil)
	if err != nil || restored {
		t.Fatalf("正文可用时不得报告回退: err=%v restored=%v", err, restored)
	}
}
