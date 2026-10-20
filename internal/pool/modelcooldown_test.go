package pool

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// ---------------------------------------------------------------------------
// 6004 模型级 limit 独立冷却（issue：多模型独立计时）
// ---------------------------------------------------------------------------

// TestModelCooldownsIndependent 核心：模型 A 触发 6004（重置 2h 后），模型 B 再触发
// 6004（重置 1h 后）→
//  1. A 的冷却独立保留：1h 后 A 仍在限额中、B 已恢复；
//  2. until（全账号级）不被任何模型的 6004 覆盖。
func TestModelCooldownsIndependent(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})

	base := time.Now()
	resetA := base.Add(2 * time.Hour)
	resetB := base.Add(1 * time.Hour)

	pool.mu.Lock()
	entry2 := pool.byUID["u1"]
	entry2.modelCooldowns = map[string]modelCooldown{
		"glm-5.3": {Until: resetA, ResetAt: resetA},
		"hy3-x":   {Until: resetB, ResetAt: resetB},
	}
	pool.mu.Unlock()

	now := base.Add(90 * time.Minute)
	if entry2.healthyForModel(now, "glm-5.3") {
		t.Fatalf("90m 后 A(glm-5.3, reset 2h) 仍应限额中，但 healthyForModel 放行了")
	}

	if !entry2.healthyForModel(now, "hy3-x") {
		t.Fatalf("90m 后 B(hy3-x, reset 1h) 应已恢复，但 healthyForModel 仍拦截")
	}

	if !entry2.until.IsZero() {
		t.Errorf("until=%v 应为零值（6004 模型级冷却不写 until）", entry2.until)
	}
}

// TestModelCooldownsBDoesNotOverwriteA 模型 B 触发 6004 后，A 的冷却截止不被覆盖：
// 这是本 issue 的核心——旧实现用单 until 字段，B 会覆盖 A。
func TestModelCooldownsBDoesNotOverwriteA(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	resetA := time.Now().Add(2 * time.Hour)
	// 模拟真实路径两次 6004：A(2h) 然后 B(1h)。
	pool.CooldownSoftForModel("u1", 600*time.Second, resetA, "glm-5.3", "6004 model rate limit")
	pool.CooldownSoftForModel("u1", 600*time.Second, time.Now().Add(1*time.Hour), "hy3-x", "6004 model rate limit")

	pool.mu.RLock()
	e := pool.byUID["u1"]
	mcA, okA := e.modelCooldowns["glm-5.3"]
	until := e.until

	pool.mu.RUnlock()

	if !okA {
		t.Fatalf("A(glm-5.3) 的模型冷却条目丢失（被 B 覆盖？）")
	}

	if d := mcA.Until.Sub(resetA); d < -time.Second || d > time.Second {
		t.Errorf("A until=%v want ~%v（B 的冷却不得覆盖 A 的截止）", mcA.Until, resetA)
	}

	if !until.IsZero() {
		t.Errorf("until=%v 应为零值（6004 从不写账号级 until）", until)
	}
}

// TestCooldownSoftForModelDoesNotClobberUntil 带解析时间的 6004 不写 until
// （否则全账号级冷却被模型重置时间污染），只写 modelCooldowns[model]。
func TestCooldownSoftForModelDoesNotClobberUntil(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	reset := time.Now().Add(30 * time.Minute)
	pool.CooldownSoftForModel("u1", 600*time.Second, reset, "glm-5.3", "6004 model rate limit")
	pool.mu.RLock()
	e := pool.byUID["u1"]
	until := e.until
	modelCooldown2, ok := e.modelCooldowns["glm-5.3"]

	pool.mu.RUnlock()

	if !until.IsZero() {
		t.Errorf("until=%v 应零值（6004 不写 until）", until)
	}

	if !ok || modelCooldown2.Until.IsZero() {
		t.Errorf("modelCooldowns[glm-5.3]=%+v ok=%v，应已记录模型冷却", modelCooldown2, ok)
	}
}

// TestCooldownSoftForModelCapsUntilKeepsResetAt 6004 写 modelCooldowns：
// until 截断到 soft_rate_max，reset_at 保留上游原始墙钟（issue #36 台账语义迁移）。
func TestCooldownSoftForModelCapsUntilKeepsResetAt(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	pool.SetSoftRateMax(10 * time.Minute)

	reset := time.Now().Add(2 * time.Hour) // 远超封顶 → until 截断到 10m，reset_at 保留 2h
	pool.CooldownSoftForModel("u1", 600*time.Second, reset, "glm-5.3", "6004 model rate limit")
	pool.mu.RLock()
	modelCooldown2, ok := pool.byUID["u1"].modelCooldowns["glm-5.3"]
	pool.mu.RUnlock()

	if !ok {
		t.Fatal("modelCooldowns 缺少 glm-5.3")
	}

	if rem := time.Until(modelCooldown2.Until); rem <= 0 || rem > 10*time.Minute+time.Second {
		t.Errorf("Until 应在 (0,10m] 区间，实际剩余 %v", rem)
	}

	if d := modelCooldown2.ResetAt.Sub(reset); d < -time.Second || d > time.Second {
		t.Errorf("ResetAt=%v want ~2h 后=%v", modelCooldown2.ResetAt, reset)
	}
}

// TestHealthyForModelAfterModelSpecific6004 6004 只锁该模型：
// 账号对触发模型不可选、对其他模型仍可选（issue #31 豁免保持）；账号级 healthy 仍真。
func TestHealthyForModelAfterModelSpecific6004(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	reset := time.Now().Add(30 * time.Minute)
	pool.CooldownSoftForModel("u1", 600*time.Second, reset, "glm-5.3", "6004 model rate limit")
	pool.mu.RLock()
	entry2 := pool.byUID["u1"]
	pool.mu.RUnlock()

	now := time.Now()
	if entry2.healthyForModel(now, "glm-5.3") {
		t.Fatal("触发模型 glm-5.3 应不可选")
	}

	if !entry2.healthyForModel(now, "hy3-x") {
		t.Fatal("其他模型 hy3-x 应可选（模型豁免）")
	}

	if !entry2.healthy(now) {
		t.Fatal("账号级 healthy 应仍 true（6004 只锁模型，不锁账号）")
	}
}

// TestModelCooldownsTwoLimitsBothBlock 同一账号两个模型同时 6004：这两个模型都不可选
// （无账号级冷却），其他模型仍可选。
func TestModelCooldownsTwoLimitsBothBlock(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	resetA := time.Now().Add(2 * time.Hour)
	resetB := time.Now().Add(2 * time.Hour)

	pool.mu.Lock()
	entry2 := pool.byUID["u1"]
	entry2.modelCooldowns = map[string]modelCooldown{
		"glm-5.3": {Until: resetA, ResetAt: resetA},
		"hy3-x":   {Until: resetB, ResetAt: resetB},
	}
	pool.mu.Unlock()

	now := time.Now()
	if entry2.healthyForModel(now, "glm-5.3") {
		t.Fatal("glm-5.3 应被自身冷却拦截")
	}

	if entry2.healthyForModel(now, "hy3-x") {
		t.Fatal("hy3-x 应被自身冷却拦截")
	}

	if !entry2.healthyForModel(now, "other") {
		t.Fatal("other 模型应可选（多模型限流不应让账号级不可选）")
	}
}

// TestModelCooldownsPreservedByNoteSuccess 成功（NoteSuccess）不得清除模型级 6004 冷却：
// 若清除，B 模型成功会抹掉 A 模型的独立冷却——正是本 issue 要修的核心缺陷。
func TestModelCooldownsPreservedByNoteSuccess(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	resetA := time.Now().Add(2 * time.Hour)
	pool.CooldownSoftForModel("u1", 600*time.Second, resetA, "glm-5.3", "6004 model rate limit")
	pool.NoteSuccess("u1") // 其他模型成功
	pool.mu.RLock()
	modelCooldown2, ok := pool.byUID["u1"].modelCooldowns["glm-5.3"]
	pool.mu.RUnlock()

	if !ok {
		t.Fatalf("NoteSuccess 后 A(glm-5.3) 独立冷却被清除——模型独立性被破坏")
	}

	if d := modelCooldown2.Until.Sub(resetA); d < -time.Second || d > time.Second {
		t.Errorf("A until=%v want ~%v（不得被 NoteSuccess 干扰）", modelCooldown2.Until, resetA)
	}
}

// TestModelCooldownsSurviveRevive 签到/余额刷新解冻（reviveCoolingLocked）不得清
// 模型级 6004 冷却——限流的恢复证据是上游重置墙钟到期，不是余额恢复；余额刷新
// 周期任务每 5 分钟经 ReenableIfCredits 到达这里，若在此清台账，撞限号会被误判
// 健康、重新选中再撞 429，全池冷却保护形同虚设（两号池实测复现：expiring==0 的
// 号每 5 分钟被抹一次台账，expiring>0 的号走 SetCreditsDetailed 幸免，行为不对称）。
func TestModelCooldownsSurviveRevive(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	pool.CooldownSoftForModel("u1", 600*time.Second, time.Now().Add(time.Hour), "glm-5.3", "6004")
	pool.ReenableIfCredits("u1", 500, 0)
	pool.mu.RLock()
	modelCooldown2, ok := pool.byUID["u1"].modelCooldowns["glm-5.3"]
	pool.mu.RUnlock()

	if !ok {
		t.Fatal("revive 后 modelCooldowns[glm-5.3] 应保留——余额恢复不构成限流解除证据")
	}

	if rem := time.Until(modelCooldown2.Until); rem < 55*time.Minute || rem > time.Hour+time.Minute {
		t.Errorf("6004 until 应保持 ~1h 不变, got remaining=%v", rem)
	}
}

// TestModelCooldownsLazyCleanup 已过期的模型冷却在 pick（写锁路径）时被惰性清理。
func TestModelCooldownsLazyCleanup(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	pool.mu.Lock()
	entry2 := pool.byUID["u1"]
	entry2.modelCooldowns = map[string]modelCooldown{
		"old": {Until: time.Now().Add(-time.Minute), ResetAt: time.Now().Add(-time.Minute)},
	}
	pool.mu.Unlock()

	got := pool.PickExcludingForModel(nil, "fresh")
	if got == nil || got.UID != "u1" {
		t.Fatalf("过期模型冷却不应拦截 u1, got %+v", got)
	}

	pool.mu.RLock()

	_, still := entry2.modelCooldowns["old"]

	pool.mu.RUnlock()

	if still {
		t.Error("过期模型冷却条目应在 pick 时被清理")
	}
}

// TestModelCooldownsExpiredAllowsSameModel 过期后同模型请求也放行（read 路径无清理也可选）。
func TestModelCooldownsExpiredAllowsSameModel(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	pool.mu.Lock()
	entry2 := pool.byUID["u1"]
	entry2.modelCooldowns = map[string]modelCooldown{
		"glm-5.3": {Until: time.Now().Add(-time.Nanosecond), ResetAt: time.Now().Add(-time.Nanosecond)},
	}
	pool.mu.Unlock()

	if !entry2.healthyForModel(time.Now(), "glm-5.3") {
		t.Fatal("过期后同模型请求应放行")
	}
}

// TestRateLimitedModelsMultiModel 6004 多模型同时限流 → /status 台账全部展示。
func TestRateLimitedModelsMultiModel(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	resetA := time.Now().Add(2 * time.Hour)
	resetB := time.Now().Add(1 * time.Hour)

	pool.mu.Lock()
	e := pool.byUID["u1"]
	e.modelCooldowns = map[string]modelCooldown{
		"glm-5.3": {Until: resetA, ResetAt: resetA, Reason: "6004 model rate limit"},
		"hy3-x":   {Until: resetB, ResetAt: resetB, Reason: "6004 model rate limit"},
	}
	pool.mu.Unlock()

	status, ok := pool.Status("u1")
	if !ok {
		t.Fatal("status missing")
	}

	if len(status.RateLimitedModels) != 2 {
		t.Fatalf("rate_limited_models=%+v want 2 行", status.RateLimitedModels)
	}

	wantModels := map[string]bool{"glm-5.3": true, "hy3-x": true}
	for _, row := range status.RateLimitedModels {
		if !wantModels[row.Model] {
			t.Errorf("unexpected row model=%q", row.Model)
		}

		if row.Kind != "rate_limit" {
			t.Errorf("%s kind=%q want rate_limit", row.Model, row.Kind)
		}

		delete(wantModels, row.Model)
	}

	if len(wantModels) != 0 {
		t.Errorf("缺行: %v", wantModels)
	}
}

// 无重置时间的 6004 只写 AuditOnly 台账：账号页可见，但不得改变模型路由。
func TestModelRateLimitAuditDoesNotAffectRouting(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	pool.CooldownSoftRate("u1", time.Minute, time.Time{}, "429 rate limit")
	pool.RecordModelRateLimitAudit("u1", "glm-5.3", "6004 model rate limit (reset unknown)")

	pool.mu.Lock()
	entry2 := pool.byUID["u1"]

	modelCooldown2, ok := entry2.modelCooldowns["glm-5.3"]
	if ok && modelCooldown2.AuditOnly {
		modelCooldown2.Until = time.Now().Add(time.Hour)
		entry2.modelCooldowns["glm-5.3"] = modelCooldown2
	}
	pool.mu.Unlock()

	if !ok || !modelCooldown2.AuditOnly {
		t.Fatalf("audit entry missing: %+v ok=%v", modelCooldown2, ok)
	}

	if entry2.modelCooled(time.Now(), "glm-5.3") {
		t.Fatal("AuditOnly 台账不得参与 modelCooled")
	}

	if entry2.modelExempt() {
		t.Fatal("仅 AuditOnly 台账不得让账号进入模型豁免形态")
	}

	st, _ := pool.Status("u1")
	if len(st.RateLimitedModels) != 1 || st.RateLimitedModels[0].Kind != "rate_limit" {
		t.Fatalf("audit status = %+v, want one rate_limit row", st.RateLimitedModels)
	}
}

// 11136 与 6004 共用台账但必须输出不同 kind。
func TestRateLimitedModelKindModelUnavailable(t *testing.T) {
	t.Parallel()

	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.BlockModelBackoff("u1", "missing-model", "11136 model not permitted")

	st, _ := p.Status("u1")
	if len(st.RateLimitedModels) != 1 || st.RateLimitedModels[0].Kind != "model_unavailable" {
		t.Fatalf("rows=%+v want model_unavailable", st.RateLimitedModels)
	}
}

// 11141（模型下线）与 11136 同属「模型不可用」，台账不能显示成 rate_limit：
// 面板会按限流口径给出「上游重置：时间未知」，把下线误读成可等待恢复。
func TestRateLimitedModelKindModelRetired(t *testing.T) {
	t.Parallel()

	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.BlockModelBackoff("u1", "retired-model", "11141 model retired")

	st, _ := p.Status("u1")
	if len(st.RateLimitedModels) != 1 || st.RateLimitedModels[0].Kind != "model_unavailable" {
		t.Fatalf("rows=%+v want model_unavailable", st.RateLimitedModels)
	}
}

// AuditOnly 标记必须跨重启保留，否则无重置时间的 6004 展示项会失忆并参与路由。
func TestModelRateLimitAuditPersists(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	filePath := dir + "/state.json"
	p := newTestPool(t, filePath)
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownSoftRate("u1", time.Minute, time.Time{}, "429 rate limit")
	p.RecordModelRateLimitAudit("u1", "glm-5.3", "6004 model rate limit (reset unknown)")
	p.Flush()

	testCase := newTestPool(t, filePath)
	testCase.Add(&auth.Auth{UID: "u1"})
	testCase.mu.RLock()
	mc, ok := testCase.byUID["u1"].modelCooldowns["glm-5.3"]
	testCase.mu.RUnlock()

	if !ok || !mc.AuditOnly {
		t.Fatalf("audit entry not restored: %+v ok=%v", mc, ok)
	}

	if testCase.byUID["u1"].modelExempt() {
		t.Fatal("restored AuditOnly entry must not create model exemption")
	}
}

// TestRateLimitedModelsMultiModelStableOutput 多模型台账行按模型名排序（稳定输出）。
func TestRateLimitedModelsMultiModelStableOutput(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	resetA := time.Now().Add(2 * time.Hour)
	resetB := time.Now().Add(1 * time.Hour)

	pool.mu.Lock()
	e := pool.byUID["u1"]
	e.modelCooldowns = map[string]modelCooldown{
		"hy3-x":   {Until: resetB, ResetAt: resetB, Reason: "r"},
		"glm-5.3": {Until: resetA, ResetAt: resetA, Reason: "r"},
	}
	pool.mu.Unlock()
	st, _ := pool.Status("u1")
	got := []string{st.RateLimitedModels[0].Model, st.RateLimitedModels[1].Model}

	want := []string{"glm-5.3", "hy3-x"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rows=%v want sorted %v", got, want)
	}
}

// TestRateLimitedModelsEachModelHasOwnUntil 台账行 Until = 该模型独立冷却截止，
// Status.Until（账号级）不受 6004 影响（无账号级冷却时为零值）。
func TestRateLimitedModelsEachModelHasOwnUntil(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	resetA := time.Now().Add(2 * time.Hour)
	resetB := time.Now().Add(1 * time.Hour)

	pool.mu.Lock()
	e := pool.byUID["u1"]
	e.modelCooldowns = map[string]modelCooldown{
		"glm-5.3": {Until: resetA, ResetAt: resetA, Reason: "r"},
		"hy3-x":   {Until: resetB, ResetAt: resetB, Reason: "r"},
	}
	pool.mu.Unlock()

	status, ok := pool.Status("u1")
	if !ok {
		t.Fatal("status missing")
	}

	if !status.Until.IsZero() {
		t.Fatalf("Status.Until 应零值（无账号级冷却），got %v", status.Until)
	}

	for _, row := range status.RateLimitedModels {
		want := resetA
		if row.Model == "hy3-x" {
			want = resetB
		}

		if d := row.Until.Sub(want); d < -time.Second || d > time.Second {
			t.Errorf("%s row.Until=%v want ~%v", row.Model, row.Until, want)
		}
	}
}

// TestModelCooldownsPickSkipsLimitedModel 被模型 X 6004 的账号，请求 X 时选到别的号，
// 请求其他模型时可选到该号（模型豁免进入 normal 选号）。
func TestModelCooldownsPickSkipsLimitedModel(t *testing.T) {
	t.Parallel()
	withNoPickGap(t)

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	pool.Add(&auth.Auth{UID: "u2"})
	pool.SetCredits("u1", 1000, 0)
	pool.SetCredits("u2", 1, 0)
	pool.SetRandomSource(func(n int64) int64 { return 0 })
	pool.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "6004")

	if got := pool.PickExcludingForModel(nil, "glm-5.3"); got == nil || got.UID != "u2" {
		t.Fatalf("glm-5.3 请求应跳过 u1, got %+v", got)
	}

	if got := pool.PickExcludingForModel(nil, "hy3-x"); got == nil || got.UID != "u1" {
		t.Fatalf("hy3-x 请求应豁免 u1, got %+v", got)
	}
}

// TestServableNowModelCooldownStillServable 单模型 6004 限流不破坏探活（池还可服务），
// 全账号冷却（until）则不可服务。
func TestServableNowModelCooldownStillServable(t *testing.T) {
	t.Parallel()

	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "6004")

	if !p.ServableNow() {
		t.Fatal("6004 模型冷却不锁账号，ServableNow 应 true")
	}
}

// ---------------------------------------------------------------------------
// healthyForModel 优先级（全账号级先判，模型级 6004 后判）
// ---------------------------------------------------------------------------

// TestHealthyForModelAccountCooledBeatsModelNotCooled 全账号冷却（until）优先于模型
// 独立冷却：账号级 until 未到期的账号，即使该模型没有 6004 独立冷却也不可选
// （旧实现先查 modelCooled 再查 healthy，逻辑上等价的短路位置不同；锁死新语义：
// 全账号冷却优先）。
func TestHealthyForModelAccountCooledBeatsModelNotCooled(t *testing.T) {
	t.Parallel()

	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolSoft, time.Hour, "429 rate limit") // 全账号级 until 冷却，无 modelCooldowns
	p.mu.RLock()
	entry2 := p.byUID["u1"]
	p.mu.RUnlock()

	if entry2.healthyForModel(time.Now(), "glm-5.3") {
		t.Fatal("全账号 until 冷却中且模型无独立冷却：该模型也不可选（全账号冷却优先）")
	}

	if entry2.healthyForModel(time.Now(), "") {
		t.Fatal("空模型名同样不可选（等价 healthy 短路到全账号冷却）")
	}
}

// TestHealthyForModelAccountCooledWithModelCooldownStillBlocked 全账号冷却 + 该模型
// 也有 6004 独立冷却 → 不可选（无论哪条拦截都一致，优先级短路不误放行）。
func TestHealthyForModelAccountCooledWithModelCooldownStillBlocked(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	pool.Cooldown("u1", CoolSoft, time.Hour, "429 rate limit")
	pool.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "6004")
	pool.mu.RLock()
	e := pool.byUID["u1"]
	pool.mu.RUnlock()

	if e.healthyForModel(time.Now(), "glm-5.3") {
		t.Fatal("全账号冷却 + 模型独立冷却双拦截，仍应不可选")
	}
}

// TestHealthyForModelDisabledBeatsModelNotCooled disabled 是全账号级的最强冷却：
// 即使模型没有独立冷却也永不可选（disabled > 模型级）。
func TestHealthyForModelDisabledBeatsModelNotCooled(t *testing.T) {
	t.Parallel()

	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "session dead")
	p.mu.RLock()
	e := p.byUID["u1"]
	p.mu.RUnlock()

	if e.healthyForModel(time.Now(), "glm-5.3") {
		t.Fatal("disabled 账号即使模型无独立冷却也不可选")
	}
}

// TestHealthyForModelBreakerBeatsModelNotCooled 熔断（breakerUntil）是全账号级的
// 冷却：熔断期内即使模型没有独立冷却也不可选。
func TestHealthyForModelBreakerBeatsModelNotCooled(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	pool.SetBreaker(1, time.Hour, time.Hour)
	pool.NoteError("u1") // 触发熔断
	pool.mu.RLock()
	entry2 := pool.byUID["u1"]
	bt := entry2.breakerUntil

	pool.mu.RUnlock()

	if bt.IsZero() {
		t.Fatal("precondition: breaker should be open")
	}

	if entry2.healthyForModel(time.Now(), "glm-5.3") {
		t.Fatal("熔断期内即使模型无独立冷却也不可选")
	}
}

// TestHealthyForModelHealthyAccountNoModelCooldown 全账号健康 + 无任何模型独立冷却 →
// 所有模型都可选（基线）。
func TestHealthyForModelHealthyAccountNoModelCooldown(t *testing.T) {
	t.Parallel()

	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.mu.RLock()
	e := p.byUID["u1"]
	p.mu.RUnlock()

	for _, m := range []string{"glm-5.3", "hy3-x", ""} {
		if !e.healthyForModel(time.Now(), m) {
			t.Errorf("健康账号对模型 %q 应可选", m)
		}
	}
}

// TestHealthyForModelAccountCooledAllowsNothingAfterUntil 优先级锁死的另一端：
// 全账号 until 冷却到期后，模型无独立冷却的请求恢复正常（模型级判定接管）。
func TestHealthyForModelAccountCooledAllowsNothingAfterUntil(t *testing.T) {
	t.Parallel()

	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolSoft, time.Millisecond, "429")
	p.mu.RLock()
	entry2 := p.byUID["u1"]
	p.mu.RUnlock()
	// until 尚未到期：不可选（优先级：全账号级先判）。
	if entry2.healthyForModel(time.Now(), "glm-5.3") {
		t.Fatal("until 冷却内不可选")
	}
	// 推进 now 越过 until：模型无独立冷却 → 恢复可选（无需真实 sleep）。
	if !entry2.healthyForModel(time.Now().Add(20*time.Millisecond), "glm-5.3") {
		t.Fatal("until 过期后应恢复可选")
	}
}

// TestHealthyForModelPriorityViaPick 端到端：全账号冷却中的账号即使对某模型无独立冷却
// 也不被选出；健康 + 仅该模型 6004 的账号走模型豁免（同一账号对不同模型口径分离）。
func TestHealthyForModelPriorityViaPick(t *testing.T) {
	t.Parallel()
	withNoPickGap(t)

	pool := New("")
	pool.Add(&auth.Auth{UID: "cooled"})
	pool.Add(&auth.Auth{UID: "exempt"})
	pool.SetCredits("cooled", 100, 0)
	pool.SetCredits("exempt", 50, 0)
	pool.SetRandomSource(func(n int64) int64 { return 0 }) // r=0 → 最高分 cooled
	pool.Cooldown("cooled", CoolSoft, time.Hour, "429")    // 全账号级冷却，无模型级记录
	pool.CooldownSoftForModel("exempt", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "6004")

	// 请求 other：cooled 被全账号冷却拦截（即使无模型独立冷却），exempt 模型豁免
	// （6004 只锁 glm-5.3）→ 唯一候选 exempt。
	if got := pool.PickExcludingForModel(nil, "other"); got == nil || got.UID != "exempt" {
		t.Fatalf("other 模型请求应豁免 exempt（全账号冷却的 cooled 仍拦截），got %+v", got)
	}
	// 请求 glm-5.3：cooled 全账号冷却拦截；exempt 自身 6004 拦截 → 无健康候选 →
	// 全冷却兜底只认账号级冷却（exempt 无 until/breakerUntil,expiry 零值被排除），
	// 选 cooled（软冷却参与兜底）。
	if got := pool.PickExcludingForModel(nil, "glm-5.3"); got == nil || got.UID != "cooled" {
		t.Fatalf("glm-5.3 请求：exempt 被自身 6004 拦截，兜底应选全账号冷却的 cooled，got %+v", got)
	}
}

// TestModelCooldownsNotPersisted modelCooldowns 运行态、不持久化（重启清零）。
func TestModelCooldownsPersist(t *testing.T) {
	t.Parallel()
	// 6004 重置墙钟可长达数小时，跨重启是常态：model_cooldowns 持久化，
	// 恢复后 healthyForModel 不失忆（吸收上游 2f4c77b）。
	dir := t.TempDir()
	filePath := dir + "/state.json"
	p := newTestPool(t, filePath)
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "6004")
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
	p2.mu.RLock()
	n := len(p2.byUID["u1"].modelCooldowns)
	p2.mu.RUnlock()

	if n != 1 {
		t.Errorf("重载后 modelCooldowns=%d want 1（持久化恢复）", n)
	}
}

// TestModelCooldownsPersistCompatOldState 旧 state.json 无 modelCooldowns 字段正常加载
// （缺字段零值，模型冷却清零退化为账号级，向后兼容）。
func TestModelCooldownsPersistCompatOldState(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	filePath := dir + "/state.json"

	old := `{"accounts":{"u1":{"credits":100,"until":"2099-01-01T00:00:00Z","cool_kind":1}}}`
	if err := os.WriteFile(filePath, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}

	p := newTestPool(t, filePath)
	p.Add(&auth.Auth{UID: "u1"})
	p.mu.RLock()
	n := len(p.byUID["u1"].modelCooldowns)
	until := p.byUID["u1"].until
	p.mu.RUnlock()

	if n != 0 {
		t.Errorf("旧文件加载后 modelCooldowns=%d want 0", n)
	}

	if until.IsZero() {
		t.Error("旧文件 until 应照常加载（账号级冷却兼容）")
	}
}
