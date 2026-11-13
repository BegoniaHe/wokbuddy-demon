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

// TestMarkFlushedKeepsDirtyWhenSeqAdvanced 落盘期间（快照之后）有新增流水：
// 即使滚动裁剪让条数不变，也不能在写盘成功后清脏——否则新流水不会落盘。
func TestMarkFlushedKeepsDirtyWhenSeqAdvanced(t *testing.T) {
	t.Parallel()

	ledger := Open("", 1) // 上限 1：每次新增都会裁剪，len 恒为 1
	ledger.Observe("u1", 100, "")
	ledger.Observe("u1", 90, "") // 第一条流水

	ledger.mu.Lock()
	flushedSeq := ledger.seq
	ledger.mu.Unlock()

	ledger.Observe("u1", 80, "") // 快照之后新增（len 仍为 1）

	ledger.markFlushed(flushedSeq)

	ledger.mu.Lock()
	dirty := ledger.dirty
	ledger.mu.Unlock()

	if !dirty {
		t.Fatal("快照后新增的流水未落盘，必须保持脏标记")
	}
}

// TestMarkFlushedClearsWhenSeqUnchanged 快照后无新增：写盘成功后清脏。
func TestMarkFlushedClearsWhenSeqUnchanged(t *testing.T) {
	t.Parallel()

	ledger := Open("", 10)
	ledger.Observe("u1", 100, "")
	ledger.Observe("u1", 90, "")

	ledger.mu.Lock()
	flushedSeq := ledger.seq
	ledger.mu.Unlock()

	ledger.markFlushed(flushedSeq)

	ledger.mu.Lock()
	dirty := ledger.dirty
	ledger.mu.Unlock()

	if dirty {
		t.Fatal("快照后无新流水，写盘成功后应清脏")
	}
}

// 主文件损坏时必须回落到 .bak：否则「按空账本继续 → 下次 Flush 覆盖坏文件」会
// 永久丢掉整本账。
func TestLedgerFallsBackToBackup(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "credits.json")

	good := `{"version":1,"entries":[{"time":"2026-10-18T12:00:00Z","uid":"u1","delta":-100,"remain":900}]}`
	if err := writeFile(path+".bak", good); err != nil {
		t.Fatalf(".bak: %v", err)
	}

	if err := writeFile(path, `{"version":1,"entr`); err != nil {
		t.Fatalf("主文件: %v", err)
	}

	ledger := Open(path, 100)

	got := ledger.Recent(0, "")
	if len(got) != 1 || got[0].UID != "u1" || got[0].Remain != 900 {
		t.Fatalf("未回落到 .bak：entries=%v", got)
	}
}

// 载入即裁剪：旧配置（更小上限）留下的超大文件不该整份驻留到第一次 Observe。
func TestLedgerTrimsOnLoad(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "credits.json")

	entries := make([]Entry, 0, 50)
	for i := range 50 {
		entries = append(entries, Entry{UID: "u1", Delta: int64(i), Remain: int64(i)})
	}

	raw, err := json.Marshal(fileDoc{Version: 1, Entries: entries})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	ledger := Open(path, 10)

	if got := ledger.Recent(0, ""); len(got) != 10 {
		t.Fatalf("载入后条数=%d want 10（裁剪到上限）", len(got))
	}
}

// TestLedgerRejectsOverflowingBalance 极端余额不得靠减法溢出绕过 |delta| 上限。
//
// remain - prev 在两个 int64 量级极大、符号相反时会绕回小数值（MaxInt64 - MinInt64
// 得 -1），于是「异常跳变丢弃」的判定反而通过，账本里多出一条金额荒谬的流水。
// 余额来自上游 JSON，必须先把超出合理量级的观测挡在基线之外。
func TestLedgerRejectsOverflowingBalance(t *testing.T) {
	t.Parallel()

	const (
		maxInt64 = 1<<63 - 1
		minInt64 = -1 << 63
	)

	ledger := Open("", 100)
	// 两个极端观测都属脏数据：既不记流水，也不成为基线。
	ledger.Observe("u1", minInt64, "")
	ledger.Observe("u1", maxInt64, "")

	if got := ledger.Recent(0, ""); len(got) != 0 {
		t.Fatalf("极端余额不应产生流水，got %v", got)
	}

	// 脏数据未污染基线：随后的一次合理观测仍建基线，再变动照常记录。
	ledger.Observe("u1", 100, "")
	ledger.Observe("u1", 150, "")

	got := ledger.Recent(0, "")
	if len(got) != 1 || got[0].Delta != 50 || got[0].Remain != 150 {
		t.Fatalf("entries=%v want 1 条 delta=50 remain=150（基线被脏数据污染）", got)
	}
}
