package pool

import (
	"strconv"
	"testing"
	"time"
)

// TestRestoreCapsModelLedgers 恢复路径也必须压到每账号台账上限：state.json 可以
// 人工编辑，也可能是加限之前写下的旧文件。不设限时「启动即内存膨胀 + 每次选号
// 全表扫描」，且要等下一次 pick 才自愈。
func TestRestoreCapsModelLedgers(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

	restored := stateAccount{
		ModelCooldowns: map[string]stateModelCooldown{},
		ModelCosts:     map[string]stateModelCost{},
	}

	for i := range maxModelKeysPerAccount + 40 {
		model := "m" + strconv.Itoa(i)
		restored.ModelCooldowns[model] = stateModelCooldown{Until: now.Add(time.Hour)}
		restored.ModelCosts[model] = stateModelCost{CostPer1k: 1, LastSeen: now}
	}

	entry2 := buildEntryFromState("u1", &restored, now)
	if got := len(entry2.modelCooldowns); got > maxModelKeysPerAccount {
		t.Fatalf("恢复的 modelCooldowns=%d 超过上限 %d", got, maxModelKeysPerAccount)
	}

	if got := len(entry2.modelCost); got > maxModelKeysPerAccount {
		t.Fatalf("恢复的 modelCost=%d 超过上限 %d", got, maxModelKeysPerAccount)
	}
}

// TestApplySchedulingCursorsBounded 恢复游标同样受上限约束（state.json 可人工编辑）。
func TestApplySchedulingCursorsBounded(t *testing.T) {
	t.Parallel()

	pool := newTestPool(t, "")

	cursors := map[string]int{}
	for i := range maxSchedulingCursors + 40 {
		cursors["cn:m"+strconv.Itoa(i)] = i
	}

	pool.mu.Lock()
	pool.applySchedulingCursorsLocked(cursors)
	got := len(pool.schedulingCursors)
	pool.mu.Unlock()

	if got == 0 {
		t.Fatal("游标恢复不应为空")
	}

	if got > maxSchedulingCursors {
		t.Fatalf("恢复的游标条目数 %d 超过上限 %d", got, maxSchedulingCursors)
	}
}

// TestCloseJoinsFlusher Close 必须等落盘协程退出：否则「Close 返回后不会再有协程
// 写 state.json」这个前提不成立（调用方随即删临时目录/persistState 又写一次）。
func TestCloseJoinsFlusher(t *testing.T) {
	t.Parallel()

	pool := New(t.TempDir() + "/state.json")
	if pool.flusherDone == nil {
		t.Fatal("stateFp 非空时应有 flusher")
	}

	pool.Close()

	select {
	case <-pool.flusherDone:
	default:
		t.Fatal("Close 返回后落盘协程仍未退出")
	}
	// 幂等：二次 Close 不重复 close、不 panic、也不阻塞在已关闭的 channel 上。
	pool.Close()
}
