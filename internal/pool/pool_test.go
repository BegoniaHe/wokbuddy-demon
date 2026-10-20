package pool

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// withNoPickGap 保留调用点语义：测试统一经 TestMain 关闭窗口（gap=0），
// 需要窗口语义的用例在各自的 Pool 上注入 pickGap。
func withNoPickGap(t *testing.T) {
	t.Helper()
}

func TestPickHighestCredits(t *testing.T) {
	t.Parallel()
	withNoPickGap(t)
	// 三因子加权（credits 比例×10 + 闲置 + 成功率）：积分悬殊时高积分账号应被多数选中，
	// 但不再像纯 credits 加权那样接近 99%（闲置补偿 + 成功率中性 1.5 拉平了基线）。
	pool := New("")
	a1 := &auth.Auth{UID: "u1"}
	a2 := &auth.Auth{UID: "u2"}
	a3 := &auth.Auth{UID: "u3"}

	pool.Add(a1)
	pool.Add(a2)
	pool.Add(a3)
	pool.SetCredits("u1", 100, 0)
	pool.SetCredits("u2", 50000, 0)
	pool.SetCredits("u3", 300, 0)

	counts := map[string]int{}
	for range 3000 {
		counts[pool.Pick().UID]++
	}

	if counts["u2"] <= counts["u1"] || counts["u2"] <= counts["u3"] {
		t.Errorf("u2 (highest credits) should be picked most: %v", counts)
	}
}

func TestPickSkipsCooling(t *testing.T) {
	t.Parallel()

	pool := New("")
	a1 := &auth.Auth{UID: "u1"}
	a2 := &auth.Auth{UID: "u2"}

	pool.Add(a1)
	pool.Add(a2)
	pool.SetCredits("u1", 100, 0)
	pool.SetCredits("u2", 50, 0)
	pool.Cooldown("u1", CoolHard, time.Hour, "test")

	got := pool.Pick()
	if got == nil || got.UID != "u2" {
		t.Fatalf("pick=%+v want u2", got)
	}
}

func TestPickExpiredCooldownReturnsToHealthy(t *testing.T) {
	t.Parallel()

	pool := New("")
	// 注入时钟：推进虚拟时间让冷却到期，无需真实 sleep（issue #20）。
	now := time.Now()

	pool.SetClock(func() time.Time { return now })

	a1 := &auth.Auth{UID: "u1"}
	pool.Add(a1)
	pool.SetCredits("u1", 100, 0)
	pool.Cooldown("u1", CoolSoft, time.Millisecond, "429")
	now = now.Add(5 * time.Millisecond) // 推进注入时钟越过冷却截止

	got := pool.Pick()
	if got == nil || got.UID != "u1" {
		t.Fatalf("pick=%+v want u1 after cooldown expiry", got)
	}
}

func TestPickNilWhenAllDisabled(t *testing.T) {
	t.Parallel()
	// 全禁用 → 兜底不参与（禁用账号永不参与兜底）→ 返回 nil。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "session dead")

	if got := p.Pick(); got != nil {
		t.Fatalf("want nil (all disabled), got %+v", got)
	}
}

func TestPickExcluding(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	pool.Add(&auth.Auth{UID: "u2"})
	pool.SetCredits("u1", 100, 0)
	pool.SetCredits("u2", 50, 0)

	tried := map[string]bool{"u1": true}

	got := pool.PickExcluding(tried)
	if got == nil || got.UID != "u2" {
		t.Fatalf("pick=%+v want u2", got)
	}

	tried["u2"] = true
	if got := pool.PickExcluding(tried); got != nil {
		t.Fatalf("want nil, got %+v", got)
	}
}

func TestPickExcludingStaysWithinHealthy(t *testing.T) {
	t.Parallel()
	withNoPickGap(t)
	// 加权随机不能选出冷却/禁用账号。
	pool := New("")
	pool.Add(&auth.Auth{UID: "u-cold"})
	pool.Add(&auth.Auth{UID: "u-hot"})
	pool.SetCredits("u-cold", 9999, 0)
	pool.SetCredits("u-hot", 1, 0)
	pool.Cooldown("u-cold", CoolHard, time.Hour, "x")

	for i := range 20 {
		got := pool.PickExcluding(nil)
		if got == nil || got.UID != "u-hot" {
			t.Fatalf("iter %d: picked %+v, want only healthy u-hot", i, got)
		}
	}
}

func TestPickWeightedSkewTowardHighCredits(t *testing.T) {
	t.Parallel()
	withNoPickGap(t)
	// Top5 三因子加权：单账号 credits 占比足够高时，多数挑中它。
	pool := New("")
	for _, u := range []string{"w1", "w2", "w3", "w4", "w5", "w6"} {
		pool.Add(&auth.Auth{UID: u})
		pool.SetCredits(u, 1, 0)
	}

	pool.SetCredits("w1", 1000, 0)

	counts := map[string]int{}
	for range 5000 {
		counts[pool.Pick().UID]++
	}

	mx, mxUID := 0, ""
	for uid, n := range counts {
		if n > mx {
			mx, mxUID = n, uid
		}
	}

	if mxUID != "w1" {
		t.Errorf("w1 (highest credits) should be picked most: %v", counts)
	}
}

func TestPickWeightedUniformWhenAllZero(t *testing.T) {
	t.Parallel()
	withNoPickGap(t)
	// credits 全为 0 → 退化为均匀随机，不能只挑固定一个。
	pool := New("")
	for _, u := range []string{"z1", "z2", "z3"} {
		pool.Add(&auth.Auth{UID: u})
	}

	seen := map[string]bool{}
	for range 30 {
		seen[pool.Pick().UID] = true
	}

	if len(seen) != 3 {
		t.Errorf("uniform fallback should hit all, seen=%v", seen)
	}
}

func TestPickWeightedTopFiveOnly(t *testing.T) {
	t.Parallel()
	withNoPickGap(t)
	// 第 6 高 credits 的账号在 Top5 之外，权重抽签永远轮不到它。
	pool := New("")
	for _, u := range []string{"a1", "a2", "a3", "a4", "a5", "a6"} {
		pool.Add(&auth.Auth{UID: u})
	}

	pool.SetCredits("a1", 1000, 0)
	pool.SetCredits("a2", 1000, 0)
	pool.SetCredits("a3", 1000, 0)
	pool.SetCredits("a4", 1000, 0)
	pool.SetCredits("a5", 1000, 0)
	pool.SetCredits("a6", 5, 0) // Top5 之外

	for i := range 2000 {
		if got := pool.Pick(); got == nil || got.UID == "a6" {
			t.Fatalf("iter %d: picked %+v, a6 must stay outside top-5", i, got)
		}
	}
}

func TestPickTopFiveByIdleNotCredits(t *testing.T) {
	t.Parallel()
	withNoPickGap(t)
	// C1 回归：闲置补偿同样影响短名单。a1..a5 credits=100 但刚被用过（闲置 0），
	// a6 credits=90 但从未使用（闲置满分）。纯 credits 排序时 a6 进不了 top5；
	// 三因子权重下 a6 权重最高，首轮必被选中。仅断言首轮。
	pool := New("")
	now := time.Now()

	for _, u := range []string{"a1", "a2", "a3", "a4", "a5"} {
		pool.Add(&auth.Auth{UID: u})
		pool.SetCredits(u, 100, 0)
	}

	pool.Add(&auth.Auth{UID: "a6"})
	pool.SetCredits("a6", 90, 0)
	pool.SetRandomSource(func(n int64) int64 { return 0 })
	// a1..a5 全部"刚被用过"，闲置补偿归零；a6 从未使用 → 闲置满分。
	pool.mu.Lock()
	for _, u := range []string{"a1", "a2", "a3", "a4", "a5"} {
		pool.byUID[u].lastUsed = now
	}
	pool.mu.Unlock()

	if got := pool.Pick(); got == nil || got.UID != "a6" {
		t.Fatalf("pick=%v, want a6 (idle low-credit must enter top5 by weight)", got)
	}
}

func TestPickDeterministicViaSetRandomSource(t *testing.T) {
	t.Parallel()
	withNoPickGap(t)

	pool := New("")
	pool.SetRandomSource(func(n int64) int64 { return 0 })
	pool.Add(&auth.Auth{UID: "u1"})
	pool.Add(&auth.Auth{UID: "u2"})
	pool.SetCredits("u1", 100, 0)
	pool.SetCredits("u2", 50, 0)
	// r=0 ∈ [0,50) → 命中 u1。注入源应使选号完全确定。
	for i := range 50 {
		if got := pool.Pick(); got == nil || got.UID != "u1" {
			t.Fatalf("iter %d: pick=%+v want u1 (deterministic)", i, got)
		}
	}
}

func TestPickAntiThunderingHerd(t *testing.T) {
	t.Parallel()
	// 100 goroutine 同时 Pick：防并发撞号窗口内同一账号不应被重复选中。
	// credits 相同 → 无注入源时加权随机应天然打散；为保证稳定，全部置 0 走均匀随机。
	//
	// pickGap 置 0（源码注释标注的测试开关）：Windows 时钟粒度粗，整轮并发
	// Pick 可落在同一时钟刻度内——所有 lastUsed 时间戳相等，eligible 恒空走 LRU
	// 兜底，而 LRU 对相等时间戳按 UID 稳定 tie-break，结果 100 次全命中 c00。
	// 关闭窗口后本用例回归其真实断言口径：加权随机自身的打散性（跨平台稳定）。
	pool := New("")

	pool.pickGap = 0
	for i := range 10 {
		pool.Add(&auth.Auth{UID: fmt.Sprintf("c%02d", i)})
	}
	// 关键：验证并发中任意瞬间不会全选同一账号。
	const num = 100

	var wg sync.WaitGroup

	picked := make([]string, num)
	for i := range num {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			if a := pool.Pick(); a != nil {
				picked[idx] = a.UID
			}
		}(i)
	}

	wg.Wait()

	counts := map[string]int{}

	for _, uid := range picked {
		if uid != "" {
			counts[uid]++
		}
	}
	// 选号必须覆盖多个账号，且最热门的账号不超过一半。
	if len(counts) < 2 {
		t.Fatalf("anti-thundering-herd failed: all %d picks hit %d account(s) %v", num, len(counts), counts)
	}

	for uid, n := range counts {
		if n > num/2 {
			t.Errorf("account %s picked %d/%d (>50%%): thundering herd", uid, n, num)
		}
	}
}

func TestPickLRUFallbackWhenTopAllRecentlyUsed(t *testing.T) {
	t.Parallel()
	// top5 全部刚被选中 → LRU 兜底应挑最近最少使用的那个（= 最早 lastUsed）。
	pool := New("")

	pool.pickGap = time.Hour // 超大窗口：任何 lastUsed 都在窗口内
	for i := range 5 {
		pool.Add(&auth.Auth{UID: fmt.Sprintf("a%d", i)})
	}
	// 直接构造 lastUsed：不经过 Pick（避免 Pick 改写 lastUsed）。
	order := []string{"a4", "a3", "a2", "a1", "a0"}

	pool.mu.Lock()
	for i, uid := range order {
		pool.byUID[uid].lastUsed = time.Now().Add(-time.Duration(len(order)-i) * time.Second) // a4 最旧
	}
	pool.mu.Unlock()

	got := pool.Pick()
	if got == nil {
		t.Fatal("pick returned nil")
	}

	if got.UID != "a4" {
		t.Errorf("LRU fallback picked %s want a4 (oldest lastUsed)", got.UID)
	}
}

func TestCooldownPersists(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	filePath := filepath.Join(dir, "state.json")
	p := newTestPool(t, filePath)
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolHard, time.Hour, "余额不足")
	p.Flush() // 状态变更走 dirty 标志，落盘由 Flush / 后台 goroutine 负责

	p2 := newTestPool(t, filePath)
	p2.Add(&auth.Auth{UID: "u1"})

	st, ok := p2.Status("u1")
	if !ok || !st.Cooling || st.Reason != "余额不足" {
		t.Fatalf("cooldown lost after reload: %+v ok=%v", st, ok)
	}
}

func TestDisablePersists(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	filePath := filepath.Join(dir, "state.json")
	p := newTestPool(t, filePath)
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "12153 session dead")
	p.Flush()

	testCase := newTestPool(t, filePath)
	testCase.Add(&auth.Auth{UID: "u1"})

	if testCase.Pick() != nil {
		t.Fatal("disabled account picked after reload")
	}

	st, _ := testCase.Status("u1")
	if !st.Disabled || st.Reason != "12153 session dead" {
		t.Errorf("status=%+v", st)
	}
}

func TestReenableIfCredits(t *testing.T) {
	t.Parallel()

	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolHard, time.Hour, "余额不足")
	p.ReenableIfCredits("u1", 500, 0)

	got := p.Pick()
	if got == nil || got.UID != "u1" {
		t.Fatalf("should reenable, pick=%+v", got)
	}
}

func TestReenableZeroCreditsKeepsCooling(t *testing.T) {
	t.Parallel()

	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolHard, time.Hour, "余额不足")
	p.ReenableIfCredits("u1", 0, 0)

	st, _ := p.Status("u1")
	if !st.Cooling {
		t.Fatal("zero credits should stay cooling")
	}
}

func TestReenableDoesNotTouchDisabled(t *testing.T) {
	t.Parallel()

	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "session dead")
	p.ReenableIfCredits("u1", 500, 0)

	if p.Pick() != nil {
		t.Fatal("disabled must not auto-reenable")
	}
}

func TestNoteErrorAccumulatesErrTotal(t *testing.T) {
	t.Parallel()
	// NoteError 语义变更：不再有独立的 err 冷却（CoolErr 已并入熔断器），
	// 只累计 errTotal（不清零，供成功率权重）并喂熔断器 fails。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteError("u1")
	p.NoteError("u1")

	status, _ := p.Status("u1")
	if status.ErrTotal != 2 {
		t.Errorf("err_total=%d want 2", status.ErrTotal)
	}

	if status.Cooling {
		t.Errorf("NoteError alone must not set cooling (no CoolErr): %+v", status)
	}

	if status.LastErrTime.IsZero() {
		t.Error("last_err not set")
	}
}

func TestNoteSuccessResetsBreakerNotErrTotal(t *testing.T) {
	t.Parallel()
	// NoteSuccess 清 fails/熔断（运行态），但不清 errTotal（累计值）。
	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	pool.SetBreaker(2, time.Hour, 2*time.Hour)
	pool.NoteError("u1")
	pool.NoteError("u1") // 触发熔断

	if pool.internalHealthy("u1") {
		t.Fatal("breaker should be open (unhealthy) after 2 failures")
	}

	pool.NoteSuccess("u1")

	status, _ := pool.Status("u1")
	if status.ErrTotal != 2 {
		t.Errorf("err_total=%d want 2 (cumulative, not cleared by success)", status.ErrTotal)
	}

	if status.Cooling {
		t.Errorf("success should clear breaker: %+v", status)
	}
}

func TestNoteSuccessIncrementsAndRecords(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})

	before := time.Now()

	pool.NoteSuccess("u1")
	pool.NoteSuccess("u1")

	status, _ := pool.Status("u1")
	if status.SuccessCount != 2 {
		t.Errorf("success_count=%d want 2", status.SuccessCount)
	}

	if status.LastSuccessTime.Before(before) {
		t.Errorf("last_success=%v before call", status.LastSuccessTime)
	}

	if !status.LastErrTime.IsZero() {
		t.Errorf("last_err should be zero for fresh success: %v", status.LastErrTime)
	}
}

func TestReenableClearsCoolingNotBreaker(t *testing.T) {
	t.Parallel()
	// C5：签到解冻只清冷却（until/coolKind/reason）+ 更新 credits，不清熔断
	// （fails/retryCount/breakerUntil）。签到成功只证明余额与 billing 通道恢复，
	// 不证明 chat 通道健康——熔断仍按 breakerUntil 退避到期或 NoteSuccess 恢复。
	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	pool.CooldownUntilTomorrow4AM("u1", "余额不足") // 硬冷却（喂 fails，但此时阈值默认 3，不熔断）
	pool.SetBreaker(1, time.Hour, time.Hour)
	pool.NoteError("u1") // 触发熔断（fails→阈值1→fails=0, retryCount=1, breakerUntil 非零）
	pool.ReenableIfCredits("u1", 500, 0)

	status, _ := pool.Status("u1")
	if status.Reason != "" || status.Credits != 500 {
		t.Errorf("signin should clear reason + set credits=500: %+v", status)
	}

	if status.Until != (time.Time{}) {
		t.Errorf("signin should clear hard-cooling until: %+v", status.Until)
	}

	if status.BreakerUntil.IsZero() {
		t.Fatal("signin must NOT clear breakerUntil (chat health unresolved)")
	}
	// 熔断仍在 → 账号仍不可选（直至 breakerUntil 到期）。
	if pool.internalHealthy("u1") {
		t.Fatal("account should stay unhealthy while breaker active after signin")
	}
}

func TestReenableKeepsBreaker(t *testing.T) {
	t.Parallel()
	// C5 回归锁定新语义：仅熔断（无冷却）的账号，签到解冻不得清熔断。
	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	pool.SetBreaker(1, time.Hour, time.Hour)
	pool.NoteError("u1") // 触发熔断

	if bt, _ := pool.breakerUntil("u1"); bt.IsZero() {
		t.Fatal("precondition: breaker should be open")
	}

	pool.ReenableIfCredits("u1", 500, 0)

	if bt, _ := pool.breakerUntil("u1"); bt.IsZero() {
		t.Fatal("signin must not clear breakerUntil")
	}

	if pool.internalHealthy("u1") {
		t.Fatal("account should stay unhealthy while breaker active after signin")
	}
}

func TestCoolKindPersistsAcrossReload(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	filePath := filepath.Join(dir, "state.json")
	p := newTestPool(t, filePath)
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolHard, time.Hour, "余额不足")
	p.Flush()

	// 旧文件缺新字段时零值 → 冷却应仍工作（向后兼容）。
	p2 := newTestPool(t, filePath)
	p2.Add(&auth.Auth{UID: "u1"})

	status, ok := p2.Status("u1")
	if !ok || !status.Cooling {
		t.Fatalf("cooldown state lost after reload: %+v ok=%v", status, ok)
	}

	if status.CoolKind != "hard_credit" {
		t.Errorf("cool_kind after reload=%q want hard_credit", status.CoolKind)
	}
}

func TestStateRoundTripExtendedFields(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	filePath := filepath.Join(dir, "state.json")
	pool := newTestPool(t, filePath)
	pool.Add(&auth.Auth{UID: "u1"})
	pool.Cooldown("u1", CoolHard, time.Hour, "余额不足")
	pool.NoteSuccess("u1") // successCount=1，last_success 非零
	pool.NoteSuccess("u1") // successCount=2
	pool.NoteError("u1")   // errTotal=1（累计），last_err 非零
	pool.Flush()

	raw, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatal(err)
	}

	assertStateJSONFields(t, raw)

	// 重载后字段保留
	p2 := newTestPool(t, filePath)
	p2.Add(&auth.Auth{UID: "u1"})

	status, ok := p2.Status("u1")
	if !ok {
		t.Fatal("no status")
	}

	if status.SuccessCount != 2 || status.CoolKind != "hard_credit" {
		t.Errorf("reloaded portrait=%+v", status)
	}

	if status.ErrTotal != 1 {
		t.Errorf("reloaded err_total=%d want 1", status.ErrTotal)
	}

	if status.LastSuccessTime.IsZero() || status.LastErrTime.IsZero() {
		t.Error("last_success/last_err lost after reload")
	}
}

// assertStateJSONFields 校验 state.json 含必需字段、且不再写旧 err_count。
func assertStateJSONFields(t *testing.T, raw []byte) {
	t.Helper()
	// JSON tag 全小写下划线；err_total 落盘，err_count 不再落盘。
	for _, want := range []string{`"cool_kind"`, `"success_count"`, `"err_total"`, `"last_success"`, `"last_err"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("state.json missing %s:\n%s", want, raw)
		}
	}

	if strings.Contains(string(raw), `"err_count"`) {
		t.Errorf("state.json should not write legacy err_count:\n%s", raw)
	}
}

func TestLoadLegacyErrCountMigratesToErrTotal(t *testing.T) {
	t.Parallel()
	// 迁移测试：旧 state.json 只含 err_count（连续错误）→ 加载后 err_total 正确。
	dir := t.TempDir()
	filePath := filepath.Join(dir, "state.json")

	legacy := `{"accounts":{"u1":{"credits":100,"err_count":7}}}`
	if err := os.WriteFile(filePath, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	p := newTestPool(t, filePath)
	p.Add(&auth.Auth{UID: "u1"})

	status, ok := p.Status("u1")
	if !ok {
		t.Fatal("legacy account should load")
	}

	if status.ErrTotal != 7 {
		t.Errorf("err_total=%d want 7 (migrated from legacy err_count)", status.ErrTotal)
	}
	// 新字段优先：二者并存时取较大者。
	both := `{"accounts":{"u1":{"credits":100,"err_count":3,"err_total":9}}}`
	if err := os.WriteFile(filePath, []byte(both), 0o600); err != nil {
		t.Fatal(err)
	}

	p2 := newTestPool(t, filePath)
	p2.Add(&auth.Auth{UID: "u1"})

	if st2, _ := p2.Status("u1"); st2.ErrTotal != 9 {
		t.Errorf("err_total=%d want 9 (new field wins over legacy)", st2.ErrTotal)
	}
}

func TestStatusCoolKindDefaultsWhenNotCooling(t *testing.T) {
	t.Parallel()

	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	st, _ := p.Status("u1")
	if st.CoolKind != "" || st.CoolRemaining != 0 {
		t.Errorf("non-cooling portrait=%+v", st)
	}
}

func TestNextDay4AMBoundaries(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		now  string // RFC3339 (UTC 表示)
		want string // 下一个 04:00（同一时区，UTC 表示）
	}{
		{"普通日", "2026-08-28T17:00:00+08:00", "2026-08-29T04:00:00+08:00"},
		// 凌晨 00:00~04:00 触发硬冷却：当天 04:00 尚未到，冷却应落在当天（而非次日），
		// 否则多冷约一天（原 bug）。
		{"凌晨02:30", "2026-08-28T02:30:00+08:00", "2026-08-28T04:00:00+08:00"},
		{"凌晨00:00", "2026-08-28T00:00:00+08:00", "2026-08-28T04:00:00+08:00"},
		{"凌晨03:59:59", "2026-08-28T03:59:59+08:00", "2026-08-28T04:00:00+08:00"},
		{"正好4点", "2026-08-28T04:00:00+08:00", "2026-08-29T04:00:00+08:00"},
		{"4点刚过", "2026-08-28T04:00:01+08:00", "2026-08-29T04:00:00+08:00"},
		{"月末(31天月)", "2026-01-31T12:00:00+08:00", "2026-02-01T04:00:00+08:00"},
		{"月末(28天月)", "2026-02-28T12:00:00+08:00", "2026-03-01T04:00:00+08:00"},
		{"闰年月末", "2028-02-29T12:00:00+08:00", "2028-03-01T04:00:00+08:00"},
		{"年末", "2026-12-31T23:59:59+08:00", "2027-01-01T04:00:00+08:00"},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			t.Parallel()

			now, err := time.Parse(time.RFC3339, item.now)
			if err != nil {
				t.Fatal(err)
			}

			want, err := time.Parse(time.RFC3339, item.want)
			if err != nil {
				t.Fatal(err)
			}

			if got := nextDay4AM(now); !got.Equal(want) {
				t.Errorf("nextDay4AM(%v)=%v want %v", item.now, got, want)
			}
		})
	}
}

func TestCooldownUntilTomorrow4AM(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})

	before := time.Now()

	pool.CooldownUntilTomorrow4AM("u1", "余额不足")

	after := time.Now()

	status, ok := pool.Status("u1")
	if !ok {
		t.Fatal("no status")
	}

	if !status.Cooling {
		t.Fatalf("should be cooling: %+v", status)
	}

	if status.Reason != "余额不足" {
		t.Errorf("reason=%q", status.Reason)
	}
	// 冷却截止必须是"此刻之后的最近一个 04:00"：晚于 now、距今不超过 24h
	//（凌晨 00:00~04:00 触发时落在当天 04:00，其余时段落在次日 04:00，跨度恒 < 24h）。
	if status.Until.Before(after) {
		t.Errorf("until %v is in the past (call span %v..%v)", status.Until, before, after)
	}

	if status.Until.Hour() != 4 {
		t.Errorf("until hour=%d want 4", status.Until.Hour())
	}

	if d := status.Until.Sub(after); d > 24*time.Hour {
		t.Errorf("until %v is more than 24h out: %v", status.Until, d)
	}
	// 全冷却时余额耗尽（hard）号不参与兜底 → 返回 nil（等签到恢复）。
	if got := pool.Pick(); got != nil {
		t.Fatalf("all-hard-cooling should return nil (hard excluded from fallback), got %+v", got)
	}
}

func TestCooldownUntilTomorrow4AMPersists(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	filePath := filepath.Join(dir, "state.json")
	p := newTestPool(t, filePath)
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownUntilTomorrow4AM("u1", "余额不足")
	p.Flush()

	p2 := newTestPool(t, filePath)
	p2.Add(&auth.Auth{UID: "u1"})

	st, ok := p2.Status("u1")
	if !ok || st.Until.Hour() != 4 || st.Reason != "余额不足" {
		t.Errorf("status after reload=%+v ok=%v", st, ok)
	}
}

// ---------------------------------------------------------------------------
// 软冷却指数退避（softStreak）
// ---------------------------------------------------------------------------

// wantCoolSec 断言账号当前冷却剩余秒数 ≈ want（±tol 秒，容忍测试内的 tick 漂移）。
func wantCoolSec(t *testing.T, p *Pool, uid string, want, tol int64) {
	t.Helper()

	status, ok := p.Status(uid)
	if !ok {
		t.Fatalf("status(%s) missing", uid)
	}

	if !status.Cooling || status.CoolKind != "soft_rate" {
		t.Fatalf("%s should be in soft_rate cooling: %+v", uid, status)
	}
	// 软冷却写入口叠加 ±10% 抖动（issue #116 P0-3），断言按抖动上界放宽。
	if jitterTol := want/10 + 2; jitterTol > tol {
		tol = jitterTol
	}

	if got := status.CoolRemaining; got < want-tol || got > want+tol {
		t.Errorf("cool_remaining_sec=%d want ~%d (±%d)", got, want, tol)
	}
}

// expireCooldown 测试助手：把账号冷却截止回拨到过去，模拟冷却已到期
// （CooldownSoftRate 只在"不在有效软冷却中"时推进 streak——跨冷却期堆加）。
func expireCooldown(p *Pool, uid string) {
	p.mu.Lock()
	if e, ok := p.byUID[uid]; ok {
		e.until = time.Now().Add(-time.Second)
	}
	p.mu.Unlock()
}

func TestCooldownSoftExponentialBackoff(t *testing.T) {
	t.Parallel()
	// 同一账号**跨冷却期**连续软限流 → 时长按 2 倍指数增长（吸收上游：冷却中的
	// 兜底探测不再堆加，堆加只发生在到期后的新一轮限流）。
	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	pool.SetSoftRateMax(time.Hour) // 封顶 1h：本用例三步（600/1200/2400）都不触及

	for i, want := range []int64{600, 1200, 2400} {
		pool.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "429 rate limit")
		wantCoolSec(t, pool, "u1", want, 3)

		if st, _ := pool.Status("u1"); st.SoftStreak != i+1 {
			t.Errorf("after call %d: soft_streak=%d want %d", i+1, st.SoftStreak, i+1)
		}

		expireCooldown(pool, "u1") // 模拟冷却到期后再撞新一轮限流
	}
}

func TestCooldownSoftCappedBySoftRateMax(t *testing.T) {
	t.Parallel()
	// 注入封顶：streak 3 的 400s 被压到 250s（跨冷却期堆加）。
	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	pool.SetSoftRateMax(250 * time.Second)

	pool.CooldownSoftRate("u1", 100*time.Second, time.Time{}, "x")
	wantCoolSec(t, pool, "u1", 100, 3)
	expireCooldown(pool, "u1")
	pool.CooldownSoftRate("u1", 100*time.Second, time.Time{}, "x")
	wantCoolSec(t, pool, "u1", 200, 3)
	expireCooldown(pool, "u1")
	pool.CooldownSoftRate("u1", 100*time.Second, time.Time{}, "x")
	wantCoolSec(t, pool, "u1", 250, 3)
}

func TestCooldownSoftDefaultCapWhenUnset(t *testing.T) {
	t.Parallel()
	// 未注入 softRateMax → 按 2h 封顶（避免裸用池时退避无上限）。
	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})

	for i, want := range []int64{600, 1200, 2400, 4800, 7200} {
		pool.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
		wantCoolSec(t, pool, "u1", want, 3)

		if st, _ := pool.Status("u1"); st.SoftStreak != i+1 {
			t.Errorf("soft_streak=%d want %d", st.SoftStreak, i+1)
		}

		expireCooldown(pool, "u1")
	}
}

func TestSetSoftRateMaxIgnoresNonPositive(t *testing.T) {
	t.Parallel()
	// 非正值保留原值（风格同 SetBreaker）：0 不应把封顶清零导致无上限。
	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	pool.SetSoftRateMax(0)
	pool.SetSoftRateMax(-time.Second)

	for i := range 6 {
		pool.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")

		if i < 5 {
			expireCooldown(pool, "u1") // 最后一轮不回拨：断言时须仍在冷却中
		}
	}

	wantCoolSec(t, pool, "u1", 7200, 3) // 仍是 2h 封顶（第 6 步 19200s → 7200s）
}

func TestCooldownSoftStreakResetBySuccess(t *testing.T) {
	t.Parallel()
	// 成功即证明账号恢复 → streak 归零，下次软冷却回到基数。
	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	pool.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	expireCooldown(pool, "u1")
	pool.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	wantCoolSec(t, pool, "u1", 1200, 3)

	pool.NoteSuccess("u1")

	if st, _ := pool.Status("u1"); st.SoftStreak != 0 {
		t.Fatalf("success should reset soft_streak, got %d", st.SoftStreak)
	}

	expireCooldown(pool, "u1") // 新一轮限流（上一轮冷却已过/已被成功重置）
	pool.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	wantCoolSec(t, pool, "u1", 600, 3)
}

func TestCooldownSoftKeptByReenable(t *testing.T) {
	t.Parallel()
	// 签到/余额刷新解冻（reviveCoolingLocked）只解冻余额耗尽冷却（CoolHard）；
	// 软限流冷却（CoolSoft）与 softStreak 保留——限流恢复证据是重置墙钟/退避到期，
	// 不是余额恢复（余额刷新每 5 分钟一次，若在此清冷却域，限流保护实际寿命被压到
	// 一个刷新周期内）。熔断域（fails/retryCount/breakerUntil）不动，与既有 C5 语义一致。
	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	pool.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	expireCooldown(pool, "u1") // 跨冷却期第二次限流，streak 累计到 2（冷却中重复触发不堆叠，#152）
	pool.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	failsBefore := pool.breakerFails("u1")

	pool.ReenableIfCredits("u1", 500, 0)

	status, _ := pool.Status("u1")
	if !status.Cooling {
		t.Errorf("reenable 不得解除软限流冷却: %+v", status)
	}

	if status.SoftStreak != 2 {
		t.Errorf("reenable 不得清零软限流退避计数 soft_streak, got %d want 2", status.SoftStreak)
	}

	if failsAfter := pool.breakerFails("u1"); failsAfter != failsBefore {
		t.Errorf("reenable must not touch breaker: fails %d → %d", failsBefore, failsAfter)
	}

	// 退避延续：第 3 次触发从既有 streak=2 继续 → 600s<<2 = 2400s（而非归零后的 600s）。
	expireCooldown(pool, "u1")
	pool.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	wantCoolSec(t, pool, "u1", 2400, 3)
}

func TestCooldownHardDoesNotAdvanceSoftStreak(t *testing.T) {
	t.Parallel()
	// 硬冷却（余额耗尽）时长由签到时点决定，不参与软退避指数。
	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	pool.CooldownUntilTomorrow4AM("u1", "余额不足")

	if st, _ := pool.Status("u1"); st.SoftStreak != 0 {
		t.Fatalf("hard cooldown must not touch soft_streak, got %d", st.SoftStreak)
	}

	pool.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	wantCoolSec(t, pool, "u1", 600, 3)
}

func TestSoftStreakPersistsAcrossReload(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	filePath := filepath.Join(dir, "state.json")
	p := newTestPool(t, filePath)
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	expireCooldown(p, "u1")
	p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	p.Flush()

	raw, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(raw), `"soft_streak"`) {
		t.Fatalf("state.json missing soft_streak: %s", raw)
	}

	testCase := newTestPool(t, filePath)
	testCase.Add(&auth.Auth{UID: "u1"})

	if st, _ := testCase.Status("u1"); st.SoftStreak != 2 {
		t.Fatalf("soft_streak after reload=%d want 2", st.SoftStreak)
	}
	// 退避从持久化的 streak 继续：第 3 次 → 2400s。
	expireCooldown(testCase, "u1")
	testCase.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	wantCoolSec(t, testCase, "u1", 2400, 3)
}

func TestSoftStreakMissingInLegacyStateFile(t *testing.T) {
	t.Parallel()
	// 旧 state.json 无 soft_streak → 零值兼容，退避从基数重新开始。
	dir := t.TempDir()

	fp := filepath.Join(dir, "state.json")
	if err := os.WriteFile(fp, []byte(`{"accounts":{"u1":{"credits":100}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	pool := newTestPool(t, fp)
	pool.Add(&auth.Auth{UID: "u1"})

	if st, _ := pool.Status("u1"); st.SoftStreak != 0 {
		t.Fatalf("legacy file should load soft_streak=0, got %d", st.SoftStreak)
	}

	pool.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	wantCoolSec(t, pool, "u1", 600, 3)
}

// ---------------------------------------------------------------------------
// issue #31：429 6004 模型级限流 → 按上游重置时间收窄冷却 + 模型级豁免选号
// ---------------------------------------------------------------------------

func TestCooldownSoftForModelParsedUntil(t *testing.T) {
	t.Parallel()
	// 6004 msg 带「将在 … 重置」→ 该模型的独立冷却截止精确等于解析时间（wall-clock 判断）。
	// 用未来 5 分钟的时间戳：解析后 Until ≈ now+5m，远短于固定 600s 基数的指数退避，
	// 证明"上游明说重置时间"优先于"600s 起指数退避"。
	// 新语义：只写 modelCooldowns（不写账号级 until）→ 账号不 cooling、台账单行。
	reset := time.Now().Add(5 * time.Minute)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownSoftForModel("u1", 600*time.Second, reset, "glm-5.3", "429 rate limit")

	status, ok := p.Status("u1")
	if !ok {
		t.Fatal("account missing")
	}

	if status.Cooling {
		t.Fatalf("6004-with-reset should NOT set account-level cooling: %+v", status)
	}

	if len(status.RateLimitedModels) != 1 || status.RateLimitedModels[0].Model != "glm-5.3" {
		t.Fatalf("want single model ledger row for glm-5.3: %+v", status.RateLimitedModels)
	}

	until := status.RateLimitedModels[0].Until
	if d := until.Sub(reset); d < -time.Second || d > time.Second {
		t.Errorf("model until=%v want ~reset=%v (diff %v)", until, reset, d)
	}
}

func TestCooldownSoftForModelCappedBySoftRateMax(t *testing.T) {
	t.Parallel()
	// 解析时间超出 soft_rate_max → 截断到 soft_rate_max（不无限期拉黑）。
	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	pool.SetSoftRateMax(10 * time.Minute)

	reset := time.Now().Add(2 * time.Hour) // 远超过封顶 10m
	before := time.Now()

	pool.CooldownSoftForModel("u1", 600*time.Second, reset, "glm-5.3", "429 rate limit")

	status, _ := pool.Status("u1")
	if len(status.RateLimitedModels) != 1 {
		t.Fatalf("want model ledger row: %+v", status.RateLimitedModels)
	}

	if status.RateLimitedModels[0].Until.Sub(before) > 10*time.Minute+time.Second {
		t.Errorf("model until=%v want capped at soft_rate_max=10m", status.RateLimitedModels[0].Until)
	}
}

func TestCooldownSoftForModelNoResetFallbackBackoff(t *testing.T) {
	t.Parallel()
	// 无解析时间（resetAt 零值）→ 有界退避（600s 起）。冷却中的兜底探测再撞 429
	// **不推进 streak、不延长**（吸收上游：旧「每次探测都翻倍」正是全池被推到
	// 2h 封顶的元凶）。
	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	pool.SetSoftRateMax(time.Hour)
	pool.CooldownSoftForModel("u1", 600*time.Second, time.Time{}, "", "429 rate limit")
	wantCoolSec(t, pool, "u1", 600, 3)
	pool.CooldownSoftForModel("u1", 600*time.Second, time.Time{}, "", "429 rate limit")
	wantCoolSec(t, pool, "u1", 600, 3) // 仍在冷却中：不堆加
}

// TestPickExcludingForModelSkipsSoftCoolingSameModel 冷却中账号（6004 带解析时间，
// 已记录模型）+ 同 model 请求 → 仍不可选（现状语义保持）。
func TestPickExcludingForModelSkipsSoftCoolingSameModel(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	pool.Add(&auth.Auth{UID: "u2"})
	pool.SetCredits("u1", 1000, 0)
	pool.SetCredits("u2", 1, 0)
	pool.SetRandomSource(func(n int64) int64 { return 0 }) // r=0 → 最高分 u1
	pool.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "429 rate limit")

	got := pool.PickExcludingForModel(nil, "glm-5.3")
	if got == nil || got.UID != "u2" {
		t.Fatalf("same-model request must skip cooling u1, got %+v", got)
	}
}

// TestPickExcludingForModelAllowsDifferentModel 6004 冷却中的账号 + 不同 model
// → 视为可用，可选到该号（真·单模型限流，切模型立即可用）。
func TestPickExcludingForModelAllowsDifferentModel(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	pool.SetCredits("u1", 1000, 0)
	pool.Add(&auth.Auth{UID: "u2"})
	pool.SetCredits("u2", 1, 0)
	pool.SetRandomSource(func(n int64) int64 { return 0 }) // r=0 → 最高分 u1
	pool.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "429 rate limit")

	got := pool.PickExcludingForModel(nil, "hy3-x")
	if got == nil || got.UID != "u1" {
		t.Fatalf("different-model request should bypass u1 soft cooling, got %+v", got)
	}
}

// TestCooldownSoftWithoutModelRecordsNone 非 6004 的普通软冷却（resetAt 零值，
// 不记录 softRateModel）→ 不因模型切换而豁免（现状语义）。
func TestCooldownSoftWithoutModelRecordsNone(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	pool.Add(&auth.Auth{UID: "u2"})
	pool.SetCredits("u1", 1000, 0)
	pool.SetCredits("u2", 1, 0)
	pool.SetRandomSource(func(n int64) int64 { return 0 })
	pool.CooldownSoftForModel("u1", time.Minute, time.Time{}, "", "429 rate limit")
	// 冷却中 + 不同 model 请求仍跳过 u1（无 softRateModel，不豁免）。
	got := pool.PickExcludingForModel(nil, "hy3-x")
	if got == nil || got.UID != "u2" {
		t.Fatalf("no model recorded → must not bypass, got %+v", got)
	}
}

// TestPickExcludingForModelBreakerStillBlocks 模型豁免只豁免软冷却维度，
// 熔断（breakerUntil）仍拦截：6004 冷却 + 熔断中的账号，切模型也不可选。
func TestPickExcludingForModelBreakerStillBlocks(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	pool.Add(&auth.Auth{UID: "u2"})
	pool.SetCredits("u1", 1000, 0)
	pool.SetCredits("u2", 1, 0)
	pool.SetRandomSource(func(n int64) int64 { return 0 })
	pool.SetBreaker(1, time.Hour, time.Hour)
	pool.NoteError("u1") // u1 熔断
	pool.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "429 rate limit")

	got := pool.PickExcludingForModel(nil, "hy3-x")
	if got == nil || got.UID != "u2" {
		t.Fatalf("breaker must still block, got %+v", got)
	}
}

// TestSoftRateModelClearedByPlainCooldown 回归：6004 模型冷却后，若账号又经历一次
// **非模型级**软冷却（plain Cooldown），softRateModel 必须被清空——否则上次 6004 的
// 模型豁免会泄漏到本次账号级限流上，导致"换模型请求"错误绕过本次冷却。
func TestSoftRateModelClearedByPlainCooldown(t *testing.T) {
	t.Parallel()
	withNoPickGap(t)

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	pool.Add(&auth.Auth{UID: "u2"})
	pool.SetCredits("u1", 1000, 0)
	pool.SetCredits("u2", 1, 0)
	pool.SetRandomSource(func(n int64) int64 { return 0 })

	// 1) 6004 带解析时间 → 记录模型 glm-5.3。
	pool.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "6004")

	if got := pool.PickExcludingForModel(nil, "hy3-x"); got == nil || got.UID != "u1" {
		t.Fatalf("precondition: different-model should bypass, got %+v", got)
	}
	// 2) 账号恢复后经历普通账号级软冷却（无模型语义）。
	pool.NoteSuccess("u1") // 还原 fresh 状态（Cooldown 会重设 until）
	pool.Cooldown("u1", CoolSoft, time.Minute, "429 rate limit")
	// 3) 换模型请求不得再豁免（softRateModel 已清空）。
	got := pool.PickExcludingForModel(nil, "hy3-x")
	if got == nil || got.UID != "u2" {
		t.Fatalf("plain cooldown must clear softRateModel (no bypass), got %+v", got)
	}
}

// TestSoftRateModelNotPersistedToState 模型级独立冷却（modelCooldowns）是运行态：
// 落盘不引入该字段，重启清零（退化为仅账号级 until 冷却的现状）。
// 新语义：6004 带重置时间只写 modelCooldowns、不写 until → 重载后账号不冷却、
// 台账为空。
func TestSoftRateModelPersistsToState(t *testing.T) {
	t.Parallel()
	// 6004 重置墙钟可长达数小时，跨重启是常态：model_cooldowns 持久化，
	// 恢复后 healthyForModel 不失忆（吸收上游 2f4c77b）。
	dir := t.TempDir()
	filePath := filepath.Join(dir, "state.json")
	p := newTestPool(t, filePath)
	p.Add(&auth.Auth{UID: "u1"})
	// resetAt 必须显著晚于 now：传 time.Now() 会走"重置时间已过"分支把冷却压到 1ms。
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(10*time.Minute), "glm-5.3", "429 rate limit")
	p.Flush()

	raw, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(raw), "model_cooldowns") {
		t.Errorf("state.json 应持久化 model_cooldowns: %s", raw)
	}

	p2 := newTestPool(t, filePath)
	p2.Add(&auth.Auth{UID: "u1"})

	status, ok := p2.Status("u1")
	if !ok {
		t.Fatalf("account missing after reload")
	}

	if status.Cooling {
		t.Fatalf("6004-with-reset 不写账号级 until，重载后不应 cooling: %+v", status)
	}

	if len(status.RateLimitedModels) != 1 || status.RateLimitedModels[0].Model != "glm-5.3" {
		t.Errorf("modelCooldowns should survive reload, got %+v", status.RateLimitedModels)
	}
}

func TestList(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1", Nickname: "nick1"})
	pool.Add(&auth.Auth{UID: "u2"})
	pool.SetCredits("u1", 42, 0)
	pool.Cooldown("u2", CoolSoft, time.Minute, "429")

	list := pool.List()
	if len(list) != 2 {
		t.Fatalf("list=%d", len(list))
	}

	s1 := statusByUID(list, "u1")
	status := statusByUID(list, "u2")

	if s1.Credits != 42 || s1.Nickname != "nick1" || s1.Disabled || s1.Cooling {
		t.Errorf("s1=%+v", s1)
	}

	if !status.Cooling || status.Reason != "429" {
		t.Errorf("s2=%+v", status)
	}
}

// statusByUID 从状态列表里取指定 uid 的项；不存在返回零值。
func statusByUID(list []Status, uid string) Status {
	for _, s := range list {
		if s.UID == uid {
			return s
		}
	}

	return Status{}
}

func TestRemoveMissingFromDir(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	pool.Add(&auth.Auth{UID: "u2"})
	pool.SyncToDir([]*auth.Auth{{UID: "u2"}})

	if pool.Pick() == nil || pool.Pick().UID != "u2" {
		t.Fatal("u1 should be removed")
	}

	if _, ok := pool.Status("u1"); ok {
		t.Fatal("u1 should not exist")
	}
}

func TestFlushPersistsCredits(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	filePath := filepath.Join(dir, "state.json")
	p := newTestPool(t, filePath)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 42, 0)
	p.Flush()

	p2 := newTestPool(t, filePath)
	p2.Add(&auth.Auth{UID: "u1"})

	st, ok := p2.Status("u1")
	if !ok || st.Credits != 42 {
		t.Fatalf("flush not persisted: %+v ok=%v", st, ok)
	}
}

func TestAutoFlush(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "state.json")
	p := newTestPool(t, filePath)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 77, 0)

	deadline := time.Now().Add(2 * time.Second)

	for {
		if _, err := os.Stat(filePath); err == nil {
			break
		}

		if time.Now().After(deadline) {
			t.Fatal("state.json not written by background flusher")
		}

		time.Sleep(10 * time.Millisecond)
	}

	p2 := newTestPool(t, filePath)
	p2.Add(&auth.Auth{UID: "u1"})

	st, ok := p2.Status("u1")
	if !ok || st.Credits != 77 {
		t.Fatalf("auto flush not persisted: %+v ok=%v", st, ok)
	}
}

func TestFlushIdempotentWhenClean(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := newTestPool(t, fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Flush() // 无 dirty，不应写盘

	if _, err := os.Stat(fp); !os.IsNotExist(err) {
		t.Fatalf("flush on clean pool should not write: %v", err)
	}
}

func TestSaveFailureRecordedAndRecovers(t *testing.T) {
	t.Parallel()
	// stateFp 的父路径是一个普通文件（非目录）→ MkdirAll/WriteFile 必失败，
	// root 也不可绕过，可靠地触发落盘失败路径。
	dir := t.TempDir()

	block := filepath.Join(dir, "block")
	if err := os.WriteFile(block, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	p := newTestPool(t, filepath.Join(block, "state.json"))
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 42, 0)
	p.Flush()

	if p.persistFails == 0 {
		t.Fatal("persist failure should be recorded (visible), got 0")
	}

	// 换回可写目录 → 成功后 persistFails 归零（恢复日志由零值门槛触发）。
	good := filepath.Join(t.TempDir(), "state.json")
	testCase := newTestPool(t, good)
	testCase.Add(&auth.Auth{UID: "u1"})
	testCase.SetCredits("u1", 42, 0)
	testCase.Flush()

	if testCase.persistFails != 0 {
		t.Fatalf("successful save should reset persistFails, got %d", testCase.persistFails)
	}

	if raw, err := os.ReadFile(good); err != nil || !strings.Contains(string(raw), `"credits": 42`) {
		t.Fatalf("state.json not written on success: %v %s", err, raw)
	}
}

// ---------------------------------------------------------------------------
// T2 熔断器 + 全冷却兜底 + 指数退避
// ---------------------------------------------------------------------------

// breakerUntil 曝露内部运行态供测试断言（包内私有 helper）。
func (p *Pool) breakerUntil(uid string) (time.Time, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	e, ok := p.byUID[uid]
	if !ok {
		return time.Time{}, false
	}

	return e.breakerUntil, true
}

// breakerFails 曝露 entry.fails 供测试断言（包内私有 helper）。
func (p *Pool) breakerFails(uid string) int {
	p.mu.RLock()
	defer p.mu.RUnlock()

	return p.byUID[uid].fails
}

// internalHealthy 曝露 entry.healthy 供测试断言（包内私有 helper）。
func (p *Pool) internalHealthy(uid string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()

	e, ok := p.byUID[uid]
	if !ok {
		return false
	}

	return e.healthy(time.Now())
}

func TestBreakerTripsAtThreshold(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	pool.SetBreaker(3, time.Hour, 6*time.Hour)

	for i := range 2 {
		pool.NoteError("u1") // NoteError 只驱动熔断（不再有单独 err 冷却）

		if bt, ok := pool.breakerUntil("u1"); ok && !bt.IsZero() {
			t.Fatalf("breaker tripped too early at %d: %v", i+1, bt)
		}
	}

	pool.NoteError("u1")

	bt, ok := pool.breakerUntil("u1")
	if !ok || bt.IsZero() {
		t.Fatalf("breaker should trip at threshold: until=%v ok=%v", bt, ok)
	}
}

func TestBreakerSuccessClears(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	pool.SetBreaker(3, time.Hour, 6*time.Hour)
	pool.NoteError("u1")
	pool.NoteError("u1")
	pool.NoteError("u1") // 触发熔断

	if bt, _ := pool.breakerUntil("u1"); bt.IsZero() {
		t.Fatal("breaker should be open")
	}

	pool.NoteSuccess("u1")

	if bt, _ := pool.breakerUntil("u1"); !bt.IsZero() {
		t.Fatalf("success should clear breaker, until=%v", bt)
	}

	if !pool.internalHealthy("u1") {
		t.Fatal("account should be healthy after success clears breaker")
	}
}

func TestBreakerExponentialBackoffCapped(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	pool.SetBreaker(3, time.Minute, 4*time.Minute) // threshold=3：连续 3 次失败熔断一次
	// 连续 9 次失败（无成功）→ 熔断 3 次，retryCount 1→2→3，退避 1m→2m→4m(封顶)。
	for range 3 {
		for range 3 {
			pool.NoteError("u1") // 连续失败只驱动熔断
		}
	}

	bt, ok := pool.breakerUntil("u1")
	if !ok || bt.IsZero() {
		t.Fatal("breaker should be open")
	}

	d := time.Until(bt)
	// 第 3 次熔断：d = min(1m * 2^2, 4m) = 4m
	if d < 4*time.Minute-time.Second || d > 4*time.Minute+time.Second {
		t.Errorf("backoff should cap at max=4m, got %v", d)
	}

	// 对比第 1 次熔断（新账号重新来）：退避应更短。
	testCase := New("")
	testCase.Add(&auth.Auth{UID: "u1"})
	testCase.SetBreaker(3, time.Minute, 4*time.Minute)

	for range 3 {
		testCase.NoteError("u1")
	}

	bt1, _ := testCase.breakerUntil("u1")
	if d1 := time.Until(bt1); d1 > time.Minute+time.Second {
		t.Errorf("first trip should be ~1m, got %v", d1)
	}
}

func TestFallbackPicksEarliestExpiry(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "late"})
	pool.Add(&auth.Auth{UID: "early"})
	// 两个都软冷却；early 更早到期 → 兜底选 early。
	pool.Cooldown("late", CoolSoft, 2*time.Hour, "x")
	pool.Cooldown("early", CoolSoft, time.Hour, "x")

	got := pool.Pick()
	if got == nil || got.UID != "early" {
		t.Fatalf("fallback should pick earliest expiry (early), got %+v", got)
	}
}

func TestFallbackSkipsDisabled(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "cooled"})
	pool.Add(&auth.Auth{UID: "dead"})
	pool.Cooldown("cooled", CoolSoft, time.Hour, "x")
	pool.Disable("dead", "session dead") // 禁用不参与兜底

	got := pool.Pick()
	if got == nil || got.UID != "cooled" {
		t.Fatalf("fallback should skip disabled, got %+v", got)
	}
}

func TestFallbackSkipsHardCooldown(t *testing.T) {
	t.Parallel()
	// D3：余额耗尽（CoolHard）号不参与兜底——调了必 402，浪费轮换并产生噪音日志。
	p := New("")
	p.Add(&auth.Auth{UID: "hard"})
	p.Cooldown("hard", CoolHard, time.Hour, "余额不足")

	if got := p.Pick(); got != nil {
		t.Fatalf("hard-cooled account must not be fallback-picked, got %+v", got)
	}
}

func TestFallbackAllHardReturnsNil(t *testing.T) {
	t.Parallel()
	// 全 hard 冷却 → 无软冷却/熔断号可兜底 → 返回 nil。
	pool := New("")
	pool.Add(&auth.Auth{UID: "h1"})
	pool.Add(&auth.Auth{UID: "h2"})
	pool.Cooldown("h1", CoolHard, time.Hour, "x")
	pool.Cooldown("h2", CoolHard, 2*time.Hour, "x")

	if got := pool.Pick(); got != nil {
		t.Fatalf("all-hard should return nil, got %+v", got)
	}
}

func TestFallbackSoftAndBreakerParticipate(t *testing.T) {
	t.Parallel()
	// D3：soft 与 breaker 冷却号允许参与兜底，取最早到期者。
	pool := New("")
	pool.Add(&auth.Auth{UID: "soft"})
	pool.Add(&auth.Auth{UID: "brk"})
	pool.Cooldown("soft", CoolSoft, 10*time.Minute, "429") // soft: until=10m, fails=1
	pool.SetBreaker(2, 5*time.Minute, 5*time.Minute)       // 阈值 2：soft 的 1 次失败不熔断
	pool.NoteError("brk")                                  // brk: fails=1
	pool.NoteError("brk")                                  // brk: 熔断，breakerUntil=5m

	got := pool.Pick()
	if got == nil {
		t.Fatal("fallback should pick breaker (earliest) account")
	}

	if got.UID != "brk" {
		t.Fatalf("fallback should pick earliest expiry brk (5m < soft 10m), got %+v", got)
	}
}

func TestFallbackNilWhenAllDisabled(t *testing.T) {
	t.Parallel()

	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "session dead")

	if got := p.Pick(); got != nil {
		t.Fatalf("want nil when all disabled, got %+v", got)
	}
}

// ---------------------------------------------------------------------------
// T3 三因子加权选取
// ---------------------------------------------------------------------------

// idleWeightOf 曝露 weightOf 的单因子拆解不便，改用完整权重断言（包内私有 helper）。
func (p *Pool) entryWeight(uid string) float64 {
	p.mu.RLock()
	defer p.mu.RUnlock()

	entry2 := p.byUID[uid]

	var maxCredits int64
	for _, x := range p.byUID {
		if x.credits > maxCredits {
			maxCredits = x.credits
		}
	}

	return p.weightOf(entry2, maxCredits, "", time.Now())
}

func TestWeightHighCreditsDominates(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "hi"})
	pool.Add(&auth.Auth{UID: "lo"})
	pool.SetCredits("hi", 1000, 0)
	pool.SetCredits("lo", 10, 0)

	wHi, wLo := pool.entryWeight("hi"), pool.entryWeight("lo")
	if wHi <= wLo {
		t.Errorf("high credits should weigh more: hi=%v lo=%v", wHi, wLo)
	}
}

func TestWeightIdleCompensation(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "used"})
	pool.Add(&auth.Auth{UID: "idle"})
	pool.SetCredits("used", 100, 0)
	pool.SetCredits("idle", 100, 0)
	// used 1 小时前被选中过、idle 从未使用 → idle 权重更高（闲置补偿）。
	pool.mu.Lock()
	pool.byUID["used"].lastUsed = time.Now().Add(-1 * time.Hour)
	pool.mu.Unlock()

	wUsed, wIdle := pool.entryWeight("used"), pool.entryWeight("idle")
	if wIdle <= wUsed {
		t.Errorf("idle should weigh more: used=%v idle=%v", wUsed, wIdle)
	}
}

func TestWeightAllZeroCreditsStillWeighted(t *testing.T) {
	t.Parallel()
	// credits 全 0：权重完全由 idle+successRate 决定，不退化均匀随机（仍可选出更高分者）。
	pool := New("")
	pool.Add(&auth.Auth{UID: "idle"})
	pool.Add(&auth.Auth{UID: "bursty"})
	// idle 从未使用、bursty 半分钟前刚用过 → idle 权重更高。
	pool.mu.Lock()
	pool.byUID["bursty"].lastUsed = time.Now().Add(-30 * time.Second)
	pool.mu.Unlock()

	wIdle, wBursty := pool.entryWeight("idle"), pool.entryWeight("bursty")
	if wIdle <= wBursty {
		t.Errorf("idle should outweigh recently-used when credits all zero: idle=%v bursty=%v", wIdle, wBursty)
	}
}

func TestWeightTopFiveSelectionChanges(t *testing.T) {
	t.Parallel()
	withNoPickGap(t)
	// credits 相差不大时，闲置补偿可让"低分但久置"的账号权重反超"高分但刚用"的账号，
	// 即使 credits 排序里 b 在前（Top5 内权重排序可与 credits 排序不同）。
	pool := New("")
	for _, u := range []string{"a", "b"} {
		pool.Add(&auth.Auth{UID: u})
	}

	pool.SetCredits("a", 90, 0) // a credits 略低，但久置
	pool.SetCredits("b", 100, 0)
	pool.mu.Lock()
	pool.byUID["b"].lastUsed = time.Now()
	pool.byUID["a"].lastUsed = time.Now().Add(-48 * time.Hour)
	pool.mu.Unlock()

	if wA, wB := pool.entryWeight("a"), pool.entryWeight("b"); wA <= wB {
		t.Errorf("idle a should outweigh busy higher-credit b: a=%v b=%v", wA, wB)
	}
}

// ---------------------------------------------------------------------------
// T4 在途租约（单账号并发上限）
// ---------------------------------------------------------------------------

func TestAcquireReleaseLifecycle(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	pool.SetMaxInFlight(2)

	if !pool.Acquire("u1") {
		t.Fatal("first acquire should succeed")
	}

	if !pool.Acquire("u1") {
		t.Fatal("second acquire should succeed")
	}

	if pool.Acquire("u1") {
		t.Fatal("third acquire should fail (limit 2)")
	}

	pool.Release("u1")

	if !pool.Acquire("u1") {
		t.Fatal("acquire after release should succeed")
	}
}

func TestAcquireUnlimited(t *testing.T) {
	t.Parallel()

	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	// max=0 不限：连续 acquire 永不拒绝。
	for i := range 100 {
		if !p.Acquire("u1") {
			t.Fatalf("unlimited acquire %d failed", i)
		}
	}
}

func TestAcquireUnknownUID(t *testing.T) {
	t.Parallel()

	p := New("")
	if p.Acquire("nope") {
		t.Fatal("acquire unknown uid should fail")
	}

	p.Release("nope") // 不 panic
}

func TestMaxInFlightTotal(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	pool.Add(&auth.Auth{UID: "u2"})
	pool.SetMaxInFlightTotal(1)

	if !pool.Acquire("u1") {
		t.Fatal("first acquire should succeed")
	}

	if pool.InFlightTotal() != 1 {
		t.Fatalf("in-flight total=%d want 1", pool.InFlightTotal())
	}

	if pool.Acquire("u2") {
		t.Fatal("second acquire should fail on global limit")
	}

	if !pool.GlobalInFlightFull() {
		t.Error("GlobalInFlightFull should be true at limit")
	}

	pool.Release("u1")

	if !pool.Acquire("u2") {
		t.Fatal("acquire after global release should succeed")
	}

	pool.Release("u2")
	pool.Release("u2") // 重复释放幂等

	if pool.InFlightTotal() != 0 {
		t.Fatalf("in-flight total=%d want 0", pool.InFlightTotal())
	}
}

func TestRemoveReleasesGlobalInFlight(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	pool.SetMaxInFlightTotal(1)

	if !pool.Acquire("u1") {
		t.Fatal("acquire failed")
	}

	if pool.InFlightTotal() != 1 {
		t.Fatalf("in-flight total=%d want 1", pool.InFlightTotal())
	}

	pool.Remove("u1")

	if pool.InFlightTotal() != 0 {
		t.Fatalf("after remove in-flight total=%d want 0", pool.InFlightTotal())
	}

	pool.Release("u1")

	if pool.InFlightTotal() != 0 {
		t.Fatalf("release after remove total=%d want 0", pool.InFlightTotal())
	}

	if !pool.acquireTotal(1) {
		t.Fatal("global slot should be reusable after remove")
	}

	pool.releaseTotal()
}

func TestSyncToDirReleasesGlobalInFlight(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	pool.Add(&auth.Auth{UID: "u2"})

	if !pool.Acquire("u1") {
		t.Fatal("acquire failed")
	}

	pool.SyncToDir([]*auth.Auth{{UID: "u2"}})

	if pool.InFlightTotal() != 0 {
		t.Fatalf("after sync in-flight total=%d want 0", pool.InFlightTotal())
	}
}

func TestMaxInFlightTotalUnlimited(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})

	for i := range 50 {
		if !pool.Acquire("u1") {
			t.Fatalf("unlimited global acquire %d failed", i)
		}
	}

	if pool.InFlightTotal() != 50 {
		t.Fatalf("in-flight total=%d want 50", pool.InFlightTotal())
	}

	for range 50 {
		pool.Release("u1")
	}

	if pool.InFlightTotal() != 0 {
		t.Fatalf("in-flight total=%d want 0", pool.InFlightTotal())
	}
}

func TestPickSkipsInFlightFull(t *testing.T) {
	t.Parallel()
	withNoPickGap(t)

	pool := New("")
	pool.Add(&auth.Auth{UID: "full"})
	pool.Add(&auth.Auth{UID: "free"})
	pool.SetCredits("full", 1000, 0)
	pool.SetCredits("free", 1, 0)
	pool.SetMaxInFlight(1)
	// full 占满唯一名额 → Pick 应跳过它，选 free（即使 credits 更低）。
	pool.Acquire("full")

	got := pool.Pick()
	if got == nil || got.UID != "free" {
		t.Fatalf("pick should skip in-flight-full account, got %+v", got)
	}

	pool.Release("full")
	// 释放后可重新被选中（确定性随机源 r=0 → 选 credits 最高的 full）。
	pool.SetRandomSource(func(n int64) int64 { return 0 })

	if got := pool.Pick(); got == nil || got.UID != "full" {
		t.Fatalf("after release full should be pickable, got %+v", got)
	}

	pool.Release("full")
}

func TestInFlightCountNotExceedLimit(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	pool.SetMaxInFlight(2)

	// 并发 50 次 acquire：CAS 保证任一时刻在途数不超上限；每次成功后立即 release。
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			if pool.Acquire("u1") {
				// 峰值检查：acquire 成功后立即读计数，应 ≤ 2。
				pool.mu.RLock()

				if n := pool.byUID["u1"].inFlight.Load(); n > 2 {
					t.Errorf("in-flight exceeded limit: %d", n)
				}

				pool.mu.RUnlock()
				pool.Release("u1")
			}
		})
	}

	wg.Wait()

	// 全部释放后计数必须为 0。
	pool.mu.RLock()
	n := pool.byUID["u1"].inFlight.Load()
	pool.mu.RUnlock()

	if n != 0 {
		t.Fatalf("in-flight should be 0 after all releases, got %d", n)
	}
}

// ---------------------------------------------------------------------------
// T6 向后兼容 + 运行态 Status 扩展
// ---------------------------------------------------------------------------

func TestLoadLegacyStateFile(t *testing.T) {
	t.Parallel()
	// 旧 state.json 只含 credits/until/disabled 等老字段，缺熔断/在途/成功率新字段。
	dir := t.TempDir()
	filePath := filepath.Join(dir, "state.json")

	legacy := `{"accounts":{"legacy":{"credits":123,"until":"2027-01-01T04:00:00+08:00","cool_kind":1,"reason":"余额不足"}}}`
	if err := os.WriteFile(filePath, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	p := newTestPool(t, filePath)
	p.Add(&auth.Auth{UID: "legacy"})

	status, ok := p.Status("legacy")
	if !ok {
		t.Fatal("legacy account should load")
	}

	if status.Credits != 123 || !status.Cooling || status.Reason != "余额不足" {
		t.Errorf("legacy state misloaded: %+v", status)
	}
	// 运行态新字段默认零值。
	if status.InFlight != 0 || status.BreakerFails != 0 || !status.BreakerUntil.IsZero() {
		t.Errorf("runtime fields should be zero for legacy load: %+v", status)
	}
}

// ---------------------------------------------------------------------------
// T7 D5: Redis 状态快照镜像 + 择新恢复
// ---------------------------------------------------------------------------

// memStore 内存假 Store：记录 SaveState（模拟 Redis 快照）并可按需返回 LoadState。
type memStore struct {
	mu       sync.Mutex
	saved    []byte
	loadData []byte
	loadOK   bool
}

func (m *memStore) SaveState(data []byte) {
	m.mu.Lock()

	m.saved = append([]byte(nil), data...)
	m.mu.Unlock()
}

func (m *memStore) LoadState() ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.loadOK {
		return nil, false
	}

	return append([]byte(nil), m.loadData...), true
}

func TestSaveMirrorsSnapshot(t *testing.T) {
	t.Parallel()
	// Flush 落盘时同步 fire-and-forget SaveState（带 saved_at）。
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := newTestPool(t, fp)
	memStore2 := &memStore{}
	p.SetStore(memStore2)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 42, 0)
	p.Flush()
	memStore2.mu.Lock()
	raw := string(memStore2.saved)
	memStore2.mu.Unlock()

	if !strings.Contains(raw, `"saved_at"`) {
		t.Fatalf("snapshot should carry saved_at: %s", raw)
	}

	if !strings.Contains(raw, `"credits":42`) {
		t.Fatalf("snapshot should carry account state: %s", raw)
	}
}

func TestRestoreUsesRedisWhenNewer(t *testing.T) {
	t.Parallel()
	// Redis 快照比本地 state.json 新 → 采用 Redis。
	dir := t.TempDir()
	filePath := filepath.Join(dir, "state.json")
	// 本地较旧
	if err := os.WriteFile(filePath, []byte(`{"accounts":{"u1":{"credits":1}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// 把本地 mtime 设到过去
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filePath, old, old); err != nil {
		t.Fatal(err)
	}

	ms := &memStore{loadOK: true}
	snap := snapshot{Accounts: map[string]stateAccount{"u1": {Credits: 999}}, SavedAt: time.Now()}
	ms.loadData, _ = json.Marshal(snap)
	p := newTestPool(t, filePath)
	p.SetStore(ms)
	p.RestoreFromSnapshot()

	st, ok := p.Status("u1")
	if !ok || st.Credits != 999 {
		t.Fatalf("should restore from Redis snapshot: %+v ok=%v", st, ok)
	}
}

func TestRestoreUsesLocalWhenNewer(t *testing.T) {
	t.Parallel()
	// 本地 state.json 比 Redis 快照新 → 本地优先。
	dir := t.TempDir()

	filePath := filepath.Join(dir, "state.json")
	if err := os.WriteFile(filePath, []byte(`{"accounts":{"u1":{"credits":77}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	ms := &memStore{loadOK: true}
	snap := snapshot{Accounts: map[string]stateAccount{"u1": {Credits: 999}}, SavedAt: time.Now().Add(-time.Hour)}
	ms.loadData, _ = json.Marshal(snap)
	p := newTestPool(t, filePath)
	p.SetStore(ms)
	p.RestoreFromSnapshot()

	st, ok := p.Status("u1")
	if !ok || st.Credits != 77 {
		t.Fatalf("should keep local (newer): %+v ok=%v", st, ok)
	}
}

func TestRestoreNoRedisUsesLocal(t *testing.T) {
	t.Parallel()
	// 无 Redis 快照 → 本地优先。
	dir := t.TempDir()

	filePath := filepath.Join(dir, "state.json")
	if err := os.WriteFile(filePath, []byte(`{"accounts":{"u1":{"credits":55}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	ms := &memStore{loadOK: false}
	p := newTestPool(t, filePath)
	p.SetStore(ms)
	p.RestoreFromSnapshot()

	st, ok := p.Status("u1")
	if !ok || st.Credits != 55 {
		t.Fatalf("no redis → use local: %+v ok=%v", st, ok)
	}
}

func TestStatusExposesRuntimeFields(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	pool.SetMaxInFlight(2)
	pool.Acquire("u1") // in_flight=1

	status, _ := pool.Status("u1")
	if status.InFlight != 1 {
		t.Errorf("in_flight=%d want 1", status.InFlight)
	}

	pool.SetBreaker(2, time.Hour, 2*time.Hour)
	pool.NoteError("u1") // breaker_fails=1

	status, _ = pool.Status("u1")
	if status.BreakerFails != 1 {
		t.Errorf("breaker_fails=%d want 1", status.BreakerFails)
	}

	pool.Release("u1")
}

func TestRecordTokenUsage(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})

	before := time.Now()

	pool.RecordTokenUsage("u1", &TokenUsageDelta{
		Model:               "glm-5.2",
		HasPromptTokens:     true,
		PromptTokens:        5,
		HasCompletionTokens: true,
		CompletionTokens:    7,
		HasTotalTokens:      true,
		TotalTokens:         12,
		HasLatencyMs:        true,
		LatencyMs:           1250,
		HasTokensPerSecond:  true,
		TokensPerSecond:     9.6,
	})
	pool.RecordTokenUsage("u1", &TokenUsageDelta{Model: "glm-5.2", HasLatencyMs: true, LatencyMs: 300})

	status, _ := pool.Status("u1")
	if status.TokenUsage.RequestCount != 2 {
		t.Errorf("request_count=%d want 2", status.TokenUsage.RequestCount)
	}

	if status.TokenUsage.UsageCount != 1 {
		t.Errorf("usage_count=%d want 1", status.TokenUsage.UsageCount)
	}

	if status.TokenUsage.PromptTokens != 5 || status.TokenUsage.CompletionTokens != 7 || status.TokenUsage.TotalTokens != 12 {
		t.Errorf("token usage=%+v", status.TokenUsage)
	}

	if status.TokenUsage.LastLatencyMs != 300 || status.TokenUsage.LastTokensPerSecond != nil {
		t.Errorf("latest performance should replace speed with unknown: %+v", status.TokenUsage)
	}

	if status.TokenUsage.LastModel != "glm-5.2" || status.TokenUsage.LastUsedAt.Before(before) {
		t.Errorf("last usage=%+v", status.TokenUsage)
	}
}

func TestTokenUsagePersistsAcrossReload(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	filePath := filepath.Join(dir, "state.json")
	pool := newTestPool(t, filePath)
	pool.Add(&auth.Auth{UID: "u1"})
	pool.RecordTokenUsage("u1", &TokenUsageDelta{
		Model:               "deepseek-v4",
		HasPromptTokens:     true,
		PromptTokens:        11,
		HasCompletionTokens: true,
		CompletionTokens:    13,
		HasTotalTokens:      true,
		TotalTokens:         24,
		HasLatencyMs:        true,
		LatencyMs:           2300,
		HasTokensPerSecond:  true,
		TokensPerSecond:     5.65,
	})
	pool.Flush()

	p2 := newTestPool(t, filePath)
	p2.Add(&auth.Auth{UID: "u1"})

	status, ok := p2.Status("u1")
	if !ok {
		t.Fatal("account missing after reload")
	}

	if status.TokenUsage.RequestCount != 1 || status.TokenUsage.TotalTokens != 24 || status.TokenUsage.LastModel != "deepseek-v4" {
		t.Errorf("token usage lost after reload: %+v", status.TokenUsage)
	}

	if status.TokenUsage.LastLatencyMs != 2300 || status.TokenUsage.LastTokensPerSecond == nil || *status.TokenUsage.LastTokensPerSecond != 5.65 {
		t.Errorf("latest performance lost after reload: %+v", status.TokenUsage)
	}

	raw, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatal(err)
	}

	assertTokenUsagePersisted(t, raw)
}

// assertTokenUsagePersisted 校验 state.json 落了 token_usage 且不含凭证字段。
func assertTokenUsagePersisted(t *testing.T, raw []byte) {
	t.Helper()

	if !strings.Contains(string(raw), `"token_usage"`) {
		t.Fatalf("state.json missing token_usage: %s", raw)
	}

	if strings.Contains(string(raw), "AccessToken") || strings.Contains(string(raw), "RefreshToken") {
		t.Fatalf("state.json contains credential field: %s", raw)
	}
}

func TestPickPrefersExpiringByVirtualWeight(t *testing.T) {
	t.Parallel()
	withNoPickGap(t)

	pool := New("")
	pool.Add(&auth.Auth{UID: "later"})
	pool.Add(&auth.Auth{UID: "soon"})
	pool.Add(&auth.Auth{UID: "none"})

	now := time.Now()
	pool.SetCreditsDetailed("later", 100, 100, 100, now.Add(48*time.Hour), 100)
	pool.SetCreditsDetailed("soon", 100, 100, 100, now.Add(2*time.Hour), 100)
	pool.SetCreditsDetailed("none", 100, 100, 0, time.Time{}, 0)

	// 3:1 虚拟实例是软偏好而非硬优先。用确定性随机源统计长期分布：两个快过期
	// 账号的合计份额应显著高于普通账号，同时普通账号仍保留少量流量。
	rng := rand.New(rand.NewPCG(1, 2))

	pool.SetRandomSource(func(n int64) int64 { return rng.Int64N(n) })

	counts := map[string]int{}

	for range 2000 {
		got := pool.Pick()
		if got == nil {
			t.Fatal("pick returned nil")
		}

		counts[got.UID]++
	}

	expiring := counts["soon"] + counts["later"]
	if expiring < 1500 || counts["none"] == 0 {
		t.Fatalf("virtual weight distribution=%v, want expiring majority and regular non-zero", counts)
	}
}

func TestPickExpiringTieUsesExistingWeight(t *testing.T) {
	t.Parallel()
	withNoPickGap(t)

	pool := New("")
	pool.Add(&auth.Auth{UID: "small"})
	pool.Add(&auth.Auth{UID: "large"})
	pool.SetRandomSource(func(n int64) int64 { return 0 })

	at := time.Now().Add(time.Hour)
	pool.SetCreditsDetailed("small", 10, 10, 10, at, 10)
	pool.SetCreditsDetailed("large", 50, 50, 50, at, 50)

	got := pool.Pick()
	if got == nil || got.UID != "large" {
		t.Fatalf("pick=%v want large", got)
	}
}

func TestPreferExpiringDisabledRestoresWeight(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "a"})
	pool.Add(&auth.Auth{UID: "b"})

	now := time.Now()
	pool.SetCreditsDetailed("a", 100, 100, 50, now.Add(time.Hour), 50)
	pool.SetCreditsDetailed("b", 100, 100, 0, time.Time{}, 0)

	pool.mu.Lock()
	we := pool.routingWeightOf(pool.byUID["a"], 100, "", now)
	wn := pool.routingWeightOf(pool.byUID["b"], 100, "", now)
	pool.mu.Unlock()

	if we != wn*expiringVirtualSlots {
		t.Fatalf("enabled expiring weight=%v want %v", we, wn*expiringVirtualSlots)
	}

	pool.SetPreferExpiring(false)

	pool.mu.Lock()
	wa := pool.routingWeightOf(pool.byUID["a"], 100, "", now)
	wb := pool.routingWeightOf(pool.byUID["b"], 100, "", now)
	pool.mu.Unlock()

	if wa != wb {
		t.Fatalf("disabled expiring weights differ: %v/%v", wa, wb)
	}
}

func TestCreditExpirySnapshotConsumptionAndClear(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})

	now := time.Now()
	pool.SetCreditsDetailed("u1", 100, 100, 50, now.Add(time.Hour), 40)

	status, _ := pool.Status("u1")
	assertExpirySnapshot(t, status, "initial", 50, 40, false)

	pool.NoteModelCost("u1", "m", 10, 1000)
	status, _ = pool.Status("u1")
	assertCredits(t, status, "after consume", 90)
	assertExpirySnapshot(t, status, "after consume", 40, 30, false)

	pool.NoteModelCost("u1", "m", 40, 1000)
	status, _ = pool.Status("u1")
	assertCredits(t, status, "after exhaustion", 50)
	assertExpirySnapshot(t, status, "after exhaustion", 0, 0, true)

	pool.SetCreditsDetailed("u1", 50, 50, 10, now.Add(time.Hour), 10)
	pool.SetCreditsDetailed("u1", 50, 50, 0, time.Time{}, 0)
	status, _ = pool.Status("u1")
	assertExpirySnapshot(t, status, "zero refresh", 0, 0, true)
}

// assertCredits 断言账号剩余积分。
func assertCredits(t *testing.T, st Status, stage string, want int64) {
	t.Helper()

	if st.Credits != want {
		t.Fatalf("%s credits=%d want %d: %+v", stage, st.Credits, want, st)
	}
}

// assertExpirySnapshot 断言账号到期快照三元组：expiring/earliestRemaining 与
// earliestExpiry 是否零值。
func assertExpirySnapshot(t *testing.T, st Status, stage string, expiring, earliest int64, expiryZero bool) {
	t.Helper()

	if st.CreditsExpiring != expiring || st.CreditsEarliestRemaining != earliest || st.CreditsEarliestExpiry.IsZero() != expiryZero {
		t.Fatalf("%s snapshot=%+v want expiring=%d earliest=%d expiryZero=%v", stage, st, expiring, earliest, expiryZero)
	}
}

// ---------- 暂停选号（paused）----------
//
// paused 与 disabled 正交：两者都退出选号候选，但 paused 是「临时让位」——
// 不清冷却域、不写 reason，且**保号任务照常参与**（见 scheduler 侧测试）。

// TestPauseExitsRouting 暂停号退出选号（Pick 含全冷却兜底都不参与）。
func TestPauseExitsRouting(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})

	if !pool.Pause("u1") {
		t.Fatal("Pause 对存在的账号应返回 true")
	}

	if got := pool.Pick(); got != nil {
		t.Fatalf("暂停号不应被选中, got %+v", got)
	}

	status, _ := pool.Status("u1")
	if !status.Paused {
		t.Errorf("Status.Paused 应为 true: %+v", status)
	}

	if status.Disabled {
		t.Errorf("暂停不是禁用：Disabled 必须为 false: %+v", status)
	}
}

// TestPauseExcludesFromFallback 全冷却兜底同样不捞回暂停号（与 disabled 同口径）。
func TestPauseExcludesFromFallback(t *testing.T) {
	t.Parallel()

	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Pause("u1")
	p.Cooldown("u1", CoolSoft, time.Hour, "429 rate limited") // 迫使命中兜底路径

	if got := p.Pick(); got != nil {
		t.Fatalf("want nil（暂停号不入兜底）, got %+v", got)
	}
}

// TestResumeRestoresRouting 解除暂停后立刻回到池子（无需重登或解冻）。
func TestResumeRestoresRouting(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	pool.Pause("u1")

	if !pool.Resume("u1") {
		t.Fatal("Resume 对存在的账号应返回 true")
	}

	if st, _ := pool.Status("u1"); st.Paused {
		t.Errorf("Resume 后 Paused 应为 false: %+v", st)
	}

	if got := pool.Pick(); got == nil || got.UID != "u1" {
		t.Errorf("Resume 后应可选, got %+v", got)
	}
}

// TestPauseDoesNotTouchCoolingDomain 暂停**不**清冷却域——与 disable 的关键区别：
// disable 是终态故清冷却，pause 是临时态故保留观测（恢复后仍有效）。
func TestPauseDoesNotTouchCoolingDomain(t *testing.T) {
	t.Parallel()

	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolSoft, time.Hour, "429 rate limited")
	p.Pause("u1")

	st, _ := p.Status("u1")
	if !st.Cooling || st.Reason != "429 rate limited" {
		t.Errorf("Pause 不应清冷却/原因: %+v", st)
	}
}

// TestDisableClearsPaused 禁用是比暂停更强的终态：二者不叠加。
func TestDisableClearsPaused(t *testing.T) {
	t.Parallel()

	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Pause("u1")
	p.Disable("u1", "12153 session dead")

	status, _ := p.Status("u1")
	if status.Paused {
		t.Errorf("disable 后 paused 应被清（不叠加）: %+v", status)
	}

	if !status.Disabled {
		t.Errorf("应为 disabled: %+v", status)
	}
}

// TestReviveClearsPaused 运维「解冻」是全清：paused 一并解除。
func TestReviveClearsPaused(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	pool.Pause("u1")

	if !pool.Revive("u1") {
		t.Fatal("Revive 应返回 true")
	}

	if st, _ := pool.Status("u1"); st.Paused {
		t.Errorf("Revive 应清 paused: %+v", st)
	}

	if got := pool.Pick(); got == nil || got.UID != "u1" {
		t.Errorf("Revive 后应可选, got %+v", got)
	}
}

// TestPausePersists 暂停状态落盘，跨重启不丢（轮换用法要求）。
func TestPausePersists(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	filePath := filepath.Join(dir, "state.json")
	p := newTestPool(t, filePath)
	p.Add(&auth.Auth{UID: "u1"})
	p.Pause("u1")
	p.Flush()

	testCase := newTestPool(t, filePath)
	testCase.Add(&auth.Auth{UID: "u1"})

	if st, _ := testCase.Status("u1"); !st.Paused {
		t.Errorf("重载后 Paused 应保持: %+v", st)
	}

	if got := testCase.Pick(); got != nil {
		t.Fatalf("重载后暂停号仍不应可选, got %+v", got)
	}
}

// TestPauseUnknownUIDReturnsFalse 不存在的 uid 返回 false（供面板区分 404）。
func TestPauseUnknownUIDReturnsFalse(t *testing.T) {
	t.Parallel()

	p := New("")
	if p.Pause("nope") {
		t.Error("Pause 未知 uid 应返回 false")
	}

	if p.Resume("nope") {
		t.Error("Resume 未知 uid 应返回 false")
	}
}

// TestResumeIdempotent 对未暂停账号 Resume 是空操作（幂等，不改变状态）。
func TestResumeIdempotent(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})

	if !pool.Resume("u1") {
		t.Fatal("Resume 幂等应返回 true")
	}

	if st, _ := pool.Status("u1"); st.Paused {
		t.Errorf("Paused 应仍为 false: %+v", st)
	}
}

func TestModelRateLimitHook(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})

	var gotUID, gotModel, gotRealm string

	pool.SetModelRateLimitHook(func(uid, model, realm string, _ time.Time) {
		gotUID, gotModel, gotRealm = uid, model, realm
	})
	pool.CooldownSoftForModel("u1", time.Minute, time.Now().Add(time.Hour), "glm-5.2", "6004 limit")

	if gotUID != "u1" || gotModel != "glm-5.2" || gotRealm != "cn" {
		t.Fatalf("hook got uid=%q model=%q realm=%q", gotUID, gotModel, gotRealm)
	}
}

func TestSetProxySlotPersists(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.json")
	pool := newTestPool(t, path)
	pool.Add(&auth.Auth{UID: "u1"})

	if !pool.SetProxySlot("u1", "residential") {
		t.Fatal("set proxy slot failed")
	}

	if got := pool.ProxySlotOf("u1"); got != "residential" {
		t.Fatalf("slot=%q want residential", got)
	}

	pool.Flush()

	testCase := newTestPool(t, path)

	e := testCase.byUID["u1"]
	if e == nil || e.proxySlot != "residential" {
		t.Fatalf("reloaded entry=%+v want proxy_slot residential", e)
	}

	if !testCase.SetProxySlot("u1", "") || testCase.ProxySlotOf("u1") != "" {
		t.Fatal("unbind failed")
	}
}

func TestDailyLimitTokens(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	pool.Add(&auth.Auth{UID: "u2"})
	pool.SetDailyLimits(DailyLimits{Tokens: 100}, DailyLimits{}, DailyLimits{})
	pool.SetDailyUsageProvider(func() map[string]DailyUsage {
		return map[string]DailyUsage{"u1": {Tokens: 100}}
	})

	if got := pool.Pick(); got == nil || got.UID != "u2" {
		t.Fatalf("pick=%+v want u2 (u1 at daily token limit)", got)
	}
}

func TestDailyLimitCreditsOnlyPaid(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	pool.SetDailyLimits(DailyLimits{Credits: 10}, DailyLimits{}, DailyLimits{})
	pool.SetDailyUsageProvider(func() map[string]DailyUsage {
		return map[string]DailyUsage{"u1": {Credits: 10}}
	})
	pool.SetModelRateOf(func(_, model string) string {
		if model == "paid" {
			return "1"
		}

		return "0"
	})

	if got := pool.PickExcludingForRealm(nil, "paid", "cn"); got != nil {
		t.Fatalf("paid pick=%+v want nil (daily credit limit)", got)
	}

	if got := pool.PickExcludingForRealm(nil, "free", "cn"); got == nil {
		t.Fatal("free model should still be served after daily credit limit")
	}
}

func TestDailyLimitPerModel(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	pool.SetDailyLimits(DailyLimits{ModelTokens: 100}, DailyLimits{}, DailyLimits{})
	pool.SetDailyUsageProvider(func() map[string]DailyUsage {
		return map[string]DailyUsage{"u1": {Models: map[string]int64{"hot": 100}}}
	})

	if got := pool.PickExcludingForRealm(nil, "hot", "cn"); got != nil {
		t.Fatalf("hot pick=%+v want nil", got)
	}

	if got := pool.PickExcludingForRealm(nil, "other", "cn"); got == nil {
		t.Fatal("other model should still be served")
	}
}

// TestAllDailyBlockedModelCooldownPastMidnight 纯模型级冷却跨过本地 0 点不能算日限：
// 换模型仍可能可用，必须由 ModelBlocked 回模型级 400/429，而不是 daily_limit_reached。
func TestAllDailyBlockedModelCooldownPastMidnight(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	pool.SetSoftRateMax(48 * time.Hour)
	pool.SetDailyUsageProvider(func() map[string]DailyUsage { return map[string]DailyUsage{} })

	now := time.Now()
	midnight := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, now.Location())
	pool.CooldownSoftForModel("u1", time.Minute, midnight.Add(time.Hour), "m", "6004 model rate limit")

	if pool.AllDailyBlocked("m", "cn") {
		t.Fatal("模型级冷却跨 0 点不应被当作日限")
	}
}

// TestAllDailyBlockedMixedModelCooldownBeforeMidnight 模型级冷却会在 0 点前恢复时，
// 即使其他账号已触日限，也不能报「日限到次日」：前者冷却到期即可服务，上层应回
// 更早的模型级恢复时间，而不是把少数账号的日限放大成整池等待。
func TestAllDailyBlockedMixedModelCooldownBeforeMidnight(t *testing.T) {
	t.Parallel()

	pool := New("")
	now := time.Date(2026, 10, 20, 18, 0, 0, 0, time.Local)

	pool.SetClock(func() time.Time { return now })
	pool.Add(&auth.Auth{UID: "u1"})
	pool.Add(&auth.Auth{UID: "u2"})
	pool.SetDailyLimits(DailyLimits{Tokens: 100}, DailyLimits{}, DailyLimits{})
	pool.SetDailyUsageProvider(func() map[string]DailyUsage {
		return map[string]DailyUsage{"u2": {Tokens: 100}}
	})
	// u1 模型级冷却 20:00 恢复（早于次日 0 点）；u2 已触日限。
	pool.CooldownSoftForModel("u1", time.Minute, now.Add(2*time.Hour), "m", "6004 model rate limit")

	if pool.AllDailyBlocked("m", "cn") {
		t.Fatal("模型级冷却早于 0 点恢复时不应报日限")
	}
}

// TestAllDailyBlockedAccountCoolingPastMidnight 对照：账号级冷却跨 0 点仍按既有
// 口径归入「今天没号可用」，避免本次修复扩大到账号级冷却。
func TestAllDailyBlockedAccountCoolingPastMidnight(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	pool.SetDailyUsageProvider(func() map[string]DailyUsage { return map[string]DailyUsage{} })
	pool.Cooldown("u1", CoolSoft, 30*time.Hour, "429 rate limit")

	if !pool.AllDailyBlocked("m", "cn") {
		t.Fatal("账号级冷却跨 0 点应保持既有日限口径")
	}
}
