package pool

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// TestPersistFailureKeepsDirty 落盘失败后必须保持 dirty，下一轮 flusher/Flush
// 才会重试；否则磁盘恢复后运行态仍只留在内存，重启即丢。
func TestPersistFailureKeepsDirty(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	blocker := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	pool := New("")
	pool.stateFp = filepath.Join(blocker, "state.json") // 父路径是文件 → 写盘必失败
	pool.Add(&auth.Auth{UID: "u1"})
	pool.dirty.Store(true)
	pool.Flush()

	if !pool.dirty.Load() {
		t.Fatal("落盘失败后 dirty 应保持 true")
	}

	// 故障解除后，下一次 Flush 应成功写盘并清脏。
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}

	if err := os.Mkdir(blocker, 0o755); err != nil {
		t.Fatal(err)
	}

	pool.Flush()

	if pool.dirty.Load() {
		t.Fatal("落盘成功后 dirty 应清零")
	}

	if _, err := os.Stat(pool.stateFp); err != nil {
		t.Fatalf("重试后应生成 state.json: %v", err)
	}
}

// TestConcurrentPersistKeepsNewestSnapshot（E3）：并发 Flush 时快照序号必须
// 保证后生成的状态覆盖先生成的，旧快照不得回写；最终落盘是最后一次写入。
func TestConcurrentPersistKeepsNewestSnapshot(t *testing.T) {
	t.Parallel()

	statePath := filepath.Join(t.TempDir(), "state.json")
	testPool := newTestPool(t, statePath)
	testPool.Add(&auth.Auth{UID: "u1"})

	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			testPool.SetCredits("u1", int64(i+1), 0)
			testPool.Flush()
		})
	}

	wg.Wait()

	testPool.SetCredits("u1", 999, 0)
	testPool.Flush()

	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(raw), `"credits": 999`) {
		t.Fatalf("newest snapshot not persisted: %s", raw)
	}
}
