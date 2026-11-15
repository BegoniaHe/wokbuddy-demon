package quota

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBudgetsAndDedup(t *testing.T) {
	t.Parallel()

	store := New("")

	reset := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	if !store.Add(&Event{UID: "u1", Realm: "cn", Model: "m", ResetAt: reset, Used: 100, At: time.Time{}}) {
		t.Fatal("first add should succeed")
	}

	if store.Add(&Event{UID: "u1", Realm: "cn", Model: "m", ResetAt: reset, Used: 999, At: time.Time{}}) {
		t.Fatal("duplicate should be ignored")
	}

	store.Add(&Event{UID: "u2", Realm: "cn", Model: "m", ResetAt: reset, Used: 300, At: time.Time{}})
	budgets := store.Budgets()

	b := budgets["cn|m"]
	if b.Samples != 2 || b.Used != 200 {
		t.Fatalf("budget=%+v want used 200 samples 2", b)
	}
}

// TestLoadPrunesExpiredEvents 加载后必须按保留期裁剪：重启时文件里的旧事件
// 不应短暂参与预算聚合，等下一次 Add 才被清掉。
//
// 事件直接写进文件而不是走 Add：Add 落盘前已按自身时间裁剪，文件里不会留下
// 过期事件，用例就测不到 load 的裁剪分支（删掉 load 里的 pruneLocked 也照样通过）。
func TestLoadPrunesExpiredEvents(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "quota.jsonl")
	now := time.Now()
	old := now.Add(-eventRetention - 24*time.Hour)

	var raw []byte

	for _, event := range []Event{
		{UID: "u1", Realm: "cn", Model: "m", ResetAt: old, Used: 100, At: old},
		{UID: "u2", Realm: "cn", Model: "m", ResetAt: now, Used: 300, At: now},
	} {
		line, err := json.Marshal(event)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}

		raw = append(raw, line...)
		raw = append(raw, '\n')
	}

	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write quota file: %v", err)
	}

	reloaded := New(path)
	if got := len(reloaded.Events()); got != 1 {
		t.Fatalf("events=%d want 1（过期事件应在加载时被裁剪）", got)
	}

	if b := reloaded.Budgets()["cn|m"]; b.Samples != 1 || b.Used != 300 {
		t.Fatalf("budget=%+v want used 300 samples 1", b)
	}
}

// TestAddPrunesWithWallClock 保留期裁剪必须用真实当前时间：事件自带 At 偏未来
// 会一次性清空全部历史样本。
func TestAddPrunesWithWallClock(t *testing.T) {
	t.Parallel()

	store := New("")
	reset := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

	store.Add(&Event{UID: "u1", Realm: "cn", Model: "m", ResetAt: reset, Used: 10, At: time.Now()})
	// 上游时钟漂移：At 落在很久以后。
	store.Add(&Event{
		UID: "u2", Realm: "cn", Model: "m", ResetAt: reset, Used: 20,
		At: time.Now().Add(400 * 24 * time.Hour),
	})

	budgets := store.Budgets()
	if b := budgets["cn|m"]; b.Samples != 2 {
		t.Fatalf("samples=%d want 2（At 偏未来不该裁掉历史样本）", b.Samples)
	}
}

// TestAddCreatesMissingDir 撞线样本可能早于 pool 首次落盘出现：data/ 目录尚不
// 存在时 Add 必须自己建目录，否则 safefile 的 os.CreateTemp 必然失败，样本被
// 静默丢弃（只在失败首条留一行 WARN）。
func TestAddCreatesMissingDir(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "data", "quota_events.jsonl")
	store := New(path)

	if !store.Add(&Event{
		UID: "u1", Realm: "cn", Model: "m",
		ResetAt: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC), Used: 1, At: time.Now(),
	}) {
		t.Fatal("add should succeed")
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("样本未落盘: %v", err)
	}
}
