package pool

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// TestPersistFailureKeepsDirty 落盘失败后必须保持 dirty，下一轮 flusher/Flush
// 才会重试；否则磁盘恢复后运行态仍只留在内存，重启即丢。
func TestPersistFailureKeepsDirty(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New("")
	p.stateFp = filepath.Join(blocker, "state.json") // 父路径是文件 → 写盘必失败
	p.Add(&auth.Auth{UID: "u1"})
	p.dirty.Store(true)
	p.Flush()
	if !p.dirty.Load() {
		t.Fatal("落盘失败后 dirty 应保持 true")
	}

	// 故障解除后，下一次 Flush 应成功写盘并清脏。
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(blocker, 0o755); err != nil {
		t.Fatal(err)
	}
	p.Flush()
	if p.dirty.Load() {
		t.Fatal("落盘成功后 dirty 应清零")
	}
	if _, err := os.Stat(p.stateFp); err != nil {
		t.Fatalf("重试后应生成 state.json: %v", err)
	}
}
