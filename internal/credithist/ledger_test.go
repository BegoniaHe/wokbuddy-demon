package credithist

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// writeFile 测试辅助：写文件（避免测试里到处 if err）。
func writeFile(path, body string) error {
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return fmt.Errorf("write test file: %w", err)
	}

	return nil
}

// TestLedgerFirstObservationBaseline 首次观测只建基线，不产生「凭空 +N」流水。
func TestLedgerFirstObservationBaseline(t *testing.T) {
	t.Parallel()

	ledger := Open("", 100)
	ledger.Observe("u1", 1000, "")

	if got := ledger.Recent(0, ""); len(got) != 0 {
		t.Fatalf("首次观测不应产生流水，got %v", got)
	}

	ledger.Observe("u1", 900, "checkin")

	got := ledger.Recent(0, "")
	if len(got) != 1 || got[0].Delta != -100 || got[0].Remain != 900 || got[0].Note != "checkin" {
		t.Fatalf("entries=%v want 1 条 delta=-100 remain=900", got)
	}
}

// TestLedgerFiltersAbnormalDelta 超上限的跳变丢弃（只更新基线）。
func TestLedgerFiltersAbnormalDelta(t *testing.T) {
	t.Parallel()

	ledger := Open("", 100)
	ledger.Observe("u1", 10, "")
	ledger.Observe("u1", 10+int64(sanityDelta)+1, "")

	if got := ledger.Recent(0, ""); len(got) != 0 {
		t.Fatalf("异常跳变不应记流水，got %v", got)
	}

	// 基线已更新到异常值之后：下一次正常变动仍可记录。
	ledger.Observe("u1", 10+int64(sanityDelta)+2, "")

	if got := ledger.Recent(0, ""); len(got) != 1 || got[0].Delta != 1 {
		t.Fatalf("entries=%v want 1 条 delta=1", got)
	}
}

// TestLedgerTrimAndFilter 滚动裁剪 + uid 过滤 + limit 钳制。
func TestLedgerTrimAndFilter(t *testing.T) {
	t.Parallel()

	ledger := Open("", 3)
	ledger.Observe("u1", 0, "")

	for i := 1; i <= 5; i++ {
		ledger.Observe("u1", int64(10*i), "")
	}

	ledger.Observe("u2", 100, "")

	got := ledger.Recent(0, "u1")
	if len(got) != 3 {
		t.Fatalf("u1 entries=%d want 3（上限裁剪）", len(got))
	}

	// 升序返回（时间递增）。
	for i := 1; i < len(got); i++ {
		if got[i-1].Time.After(got[i].Time) {
			t.Errorf("流水应为时间升序：%v", got)
		}
	}

	if all := ledger.Recent(1000, ""); len(all) != 3 {
		t.Errorf("总条数=%d want 3", len(all))
	}

	// u2 首次观测只建基线 → 无流水。
	if u2 := ledger.Recent(0, "u2"); len(u2) != 0 {
		t.Errorf("u2 entries=%v want 0", u2)
	}
}

// TestLedgerPersistAndReload 落盘后可恢复（含基线，重启后首次观测不误记）。
func TestLedgerPersistAndReload(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "credit-history.json")

	ledger := Open(path, 100)
	ledger.Observe("u1", 500, "")
	ledger.Observe("u1", 400, "task")
	ledger.Flush()

	reloaded := Open(path, 100)
	if got := reloaded.Recent(0, ""); len(got) != 1 || got[0].Delta != -100 {
		t.Fatalf("重载 entries=%v want 1 条 delta=-100", got)
	}

	// 基线已恢复：同一余额不产生新流水。
	reloaded.Observe("u1", 400, "")

	if got := reloaded.Recent(0, ""); len(got) != 1 {
		t.Errorf("基线未恢复（同一余额重复入账）：%v", got)
	}
}

// TestLedgerCorruptedFileStartsEmpty 文件损坏时按空账本继续（不 panic、不报错）。
func TestLedgerCorruptedFileStartsEmpty(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "credit-history.json")

	ledger := Open(path, 100)
	ledger.Observe("u1", 1, "")
	ledger.Flush()

	// 覆盖成坏 JSON。
	if err := writeFile(path, "{not json"); err != nil {
		t.Fatalf("write: %v", err)
	}

	if got := Open(path, 100).Recent(0, ""); len(got) != 0 {
		t.Errorf("损坏文件应按空账本，got %v", got)
	}
}

// TestLedgerFlushFailureKeepsDirty 写盘失败后必须保留脏标记：脏标记在写盘前就被清掉，
// 不置回的话这批流水会永久留在内存里（下一次 Flush 因「不脏」直接跳过）。
func TestLedgerFlushFailureKeepsDirty(t *testing.T) {
	t.Parallel()

	// 父路径是普通文件 → MkdirAll 必然失败，模拟写盘失败。
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := writeFile(blocker, "x"); err != nil {
		t.Fatalf("write blocker: %v", err)
	}

	ledger := Open(filepath.Join(blocker, "credit-history.json"), 100)
	ledger.Observe("u1", 500, "")
	ledger.Observe("u1", 400, "task")

	ledger.Flush()

	ledger.mu.Lock()
	dirty := ledger.dirty
	ledger.mu.Unlock()

	if !dirty {
		t.Fatal("写盘失败后必须保持脏标记，否则这批流水永远不会再被写出")
	}
}

// TestLedgerConcurrentFlushStaysValid 多账号并发 Observe+Flush（与真实余额刷新
// 并发模型一致）不得把共用的 tmp 文件写坏：落盘必须串行化，最终文件始终是合法 JSON。
func TestLedgerConcurrentFlushStaysValid(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "credit-history.json")
	ledger := Open(path, 10000)

	var wg sync.WaitGroup

	for i := range 8 {
		wg.Go(func() {
			uid := fmt.Sprintf("u%d", i)
			ledger.Observe(uid, 100, "")

			for j := range 50 {
				ledger.Observe(uid, int64(100+j+1), "")
				ledger.Flush()
			}
		})
	}

	wg.Wait()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}

	var doc fileDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("并发 Flush 写坏了账本文件: %v", err)
	}

	if len(doc.Entries) == 0 {
		t.Fatal("并发 Flush 后账本不应为空")
	}
}
