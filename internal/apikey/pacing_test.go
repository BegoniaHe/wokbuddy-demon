package apikey

import (
	"slices"
	"testing"
	"time"
)

// TestPaceRequestIntervalRPM rpm=6（间隔 10s）：连发两发的第二发必须被拨到
// ≥10s 之后（GCRA 虚拟调度）；空闲后的第一发零等待（issue #448）。
// 本用例在实现前红：旧实现是固定窗口计数，不返回任何等待。
func TestPaceRequestIntervalRPM(t *testing.T) {
	t.Parallel()

	store, id := newLimited(t, Limits{RPM: 6})

	base := time.Now()

	// 第一发：TAT 从零拨到 base+10s。
	wait := store.PaceRequest(id, base)
	if wait != 0 {
		t.Fatalf("首发（空闲）等待=%v want 0", wait)
	}

	// 第二发立即到达：TAT 已在 base+10s，等待到该点。
	wait = store.PaceRequest(id, base)
	if want := 10 * time.Second; wait < want-time.Second || wait > want+time.Second {
		t.Fatalf("连发第二发等待=%v want ≈%v", wait, want)
	}

	// 空闲 30s 后的第一发：TAT 已过去，零等待。
	wait = store.PaceRequest(id, base.Add(30*time.Second))
	if wait != 0 {
		t.Fatalf("空闲后等待=%v want 0", wait)
	}
}

// TestPaceTokenRateTPM tpm=600（每秒 10 completion token）：已见 300
// completion token 时，下一发结束时刻应被拨到约 30s 后；且 max_tokens 不参与。
func TestPaceTokenRateTPM(t *testing.T) {
	t.Parallel()

	store, id := newLimited(t, Limits{TPM: 600})

	base := time.Now()

	if wait := store.PaceTokens(id, base, 0); wait != 0 {
		t.Fatalf("零 token 等待=%v want 0", wait)
	}

	// 请求已完成并上报 300 completion token：token TAT 推进 30s。
	store.NoteEmitted(id, base, 300)

	// 零 token 查询只读时间线：此时应报 ≈30s 等待。
	wait := store.PaceWait(id, base)
	if want := 30 * time.Second; wait < want-time.Second || wait > want+time.Second {
		t.Fatalf("300 token 后 PaceWait=%v want ≈%v", wait, want)
	}

	// 再放行 300 token：时间线推进到 ≈60s，等待相应增长（说明 TAT 在累加，
	// 不是每发重置——漏桶语义而非固定窗口）。
	if got := store.PaceTokens(id, base, 300); got < 59*time.Second || got > 61*time.Second {
		t.Fatalf("再放行 300 token 等待=%v want ≈60s", got)
	}
}

// TestPaceTakesLaterOfBothSchedules 两项都配时取更晚者：请求间隔把结束推到
// 40s，token 只推到 5s → 结果 40s。
func TestPaceTakesLaterOfBothSchedules(t *testing.T) {
	t.Parallel()

	store, id := newLimited(t, Limits{RPM: 6, TPM: 600}) // 10s/req, 10 tok/s

	base := time.Now()

	store.PaceRequest(id, base)     // 请求 TAT = base+10s
	store.PaceRequest(id, base)     // 再发 → base+20s
	store.NoteEmitted(id, base, 50) // token TAT = base+5s

	if wait := store.PaceBoth(id, base, 0); wait < 19*time.Second || wait > 21*time.Second {
		t.Fatalf("RPM+TPM 等待=%v want ≈20s（取更晚）", wait)
	}
}

// TestPaceRequestTATCap 请求时间线封顶：到顶后返回封顶值，调用方据此把剩余内容
// 一次放完，且 TAT 不越过 now+PaceMaxDelay，避免后续请求被永久推到未来。
func TestPaceRequestTATCap(t *testing.T) {
	t.Parallel()

	store, id := newLimited(t, Limits{RPM: 1}) // 60s/req

	base := time.Now()

	store.PaceRequest(id, base)
	store.PaceRequest(id, base)

	wait := store.PaceRequest(id, base)
	if want := PaceMaxDelay; wait != want {
		t.Fatalf("超上限等待=%v want 封顶 %v", wait, want)
	}

	// 再连发也不能把账本推到 now+PaceMaxDelay 之后。
	for range 10 {
		if got := store.PaceRequest(id, base); got != PaceMaxDelay {
			t.Fatalf("连续推进后等待=%v want 恒为 %v", got, PaceMaxDelay)
		}
	}
}

// TestPaceTokenTATCap token 时间线同样封顶：一次超量 completion 只能把 TAT
// 推到 now+PaceMaxDelay，连续推进也不能继续把后续请求永久推远。
func TestPaceTokenTATCap(t *testing.T) {
	t.Parallel()

	store, id := newLimited(t, Limits{TPM: 600}) // 10 token/s

	base := time.Now()
	store.NoteEmitted(id, base, 3000) // 名义债务 300s

	if got := store.PaceWait(id, base); got != PaceMaxDelay {
		t.Fatalf("超量 token 后等待=%v want 封顶 %v", got, PaceMaxDelay)
	}

	for range 10 {
		store.NoteEmitted(id, base, 3000)

		if got := store.PaceWait(id, base); got != PaceMaxDelay {
			t.Fatalf("连续入账后等待=%v want 恒为 %v", got, PaceMaxDelay)
		}
	}
}

// TestPaceNoLimits 三项都缺 = 不透传任何延迟（R1）。
func TestPaceNoLimits(t *testing.T) {
	t.Parallel()

	store := New("")

	rec, _, err := store.Create("k")
	if err != nil {
		t.Fatal(err)
	}

	if wait := store.PaceBoth(rec.ID, time.Now(), 12345); wait != 0 {
		t.Fatalf("无限制 PaceBoth=%v want 0", wait)
	}
}

// TestPaceConcurrentTATAdvancesOnce 并发两发不得读到同一时刻后一起放行：
// TAT 必须在锁内先拨到未来。两发各拿一个不重叠的等待区间。
func TestPaceConcurrentTATAdvancesOnce(t *testing.T) {
	t.Parallel()

	store, id := newLimited(t, Limits{RPM: 60}) // 1s/req

	base := time.Now()

	const n = 8

	waits := make([]time.Duration, n)
	done := make(chan struct{}, n)

	for i := range n {
		go func(i int) {
			defer func() { done <- struct{}{} }()

			waits[i] = store.PaceRequest(id, base)
		}(i)
	}

	for range n {
		<-done
	}

	// 第 i 发（0 起）的等待应为 i 秒（TAT 每次 +1s）。goroutine 完成顺序不定，
	// 故对等待值集合排序后逐一比对，而不是按 i 下标比对。
	slices.Sort(waits)

	for i, wait := range waits {
		want := time.Duration(i) * time.Second
		if wait < want-200*time.Millisecond || wait > want+200*time.Millisecond {
			t.Fatalf("第 %d 小等待=%v want ≈%v（TAT 未锁内推进）", i, wait, want)
		}
	}
}

// TestPaceDropDebt 客户端断开后退还未发出的延迟：之后的下发回到零等待基线，
// 时间线不被挂起的债务占着（issue #448 验收 7）。
func TestPaceDropDebt(t *testing.T) {
	t.Parallel()

	store, id := newLimited(t, Limits{RPM: 6}) // 10s/req

	base := time.Now()

	store.PaceRequest(id, base) // TAT = base+10s

	store.DropPacing(id)

	if wait := store.PaceRequest(id, base); wait != 0 {
		t.Fatalf("DropPacing 后等待=%v want 0", wait)
	}
}

// TestNoteEmittedCanReconcileEstimate 流式中途先按正文估算入账，末帧拿到精确
// completion_tokens 后必须能把本请求的差额补上或退回，否则跨请求时间线会永久
// 保留估算误差。
func TestNoteEmittedCanReconcileEstimate(t *testing.T) {
	t.Parallel()

	store, id := newLimited(t, Limits{TPM: 600}) // 10 token/s

	base := time.Now()
	store.NoteEmitted(id, base, 300)  // 估算 300 → TAT +30s
	store.NoteEmitted(id, base, -100) // 精确只有 200 → 退回 10s

	if wait := store.PaceWait(id, base); wait < 19*time.Second || wait > 21*time.Second {
		t.Fatalf("负向对账后等待=%v want ≈20s", wait)
	}

	store.NoteEmitted(id, base, 200) // 精确 400 → 再补 20s

	if wait := store.PaceWait(id, base); wait < 39*time.Second || wait > 41*time.Second {
		t.Fatalf("正向对账后等待=%v want ≈40s", wait)
	}

	// 退回量超过当前 TAT 时不能留下负时间线；下一次入账仍从 now 起算。
	store.DropPacing(id)
	store.NoteEmitted(id, base, 10)
	store.NoteEmitted(id, base, -100)

	if wait := store.PaceWait(id, base); wait != 0 {
		t.Fatalf("过度退回后等待=%v want 0", wait)
	}
}
