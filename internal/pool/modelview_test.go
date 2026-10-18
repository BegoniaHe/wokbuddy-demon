package pool

import (
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// ---------------------------------------------------------------------------
// ModelLockView：把「哪些模型不能用、锁了几个号、还要锁多久」聚合成一张全清单
// （ModelBlocked 是单模型按请求回答，本视图是全清单，口径必须与选号一致）
// ---------------------------------------------------------------------------

// lockViewPool 造一个含两个 cn 账号的池，便于手搓模型级冷却。
func lockViewPool(t *testing.T) *Pool {
	t.Helper()
	withNoPickGap(t)

	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})

	return p
}

// setModelCooldown 直接写模型级冷却（与 modelblocked_test.go 同法）。
func setModelCooldown(pool *Pool, uid, model string, until time.Time, reason string, auditOnly bool) {
	pool.mu.Lock()

	e := pool.byUID[uid]
	if e.modelCooldowns == nil {
		e.modelCooldowns = map[string]modelCooldown{}
	}

	e.modelCooldowns[model] = modelCooldown{Until: until, Reason: reason, AuditOnly: auditOnly}
	pool.mu.Unlock()
}

// oneRow 断言恰好一行并返回它。
func oneRow(t *testing.T, rows []ModelLockRow) ModelLockRow {
	t.Helper()

	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %+v", rows)
	}

	return rows[0]
}

// assertUnlockAround 断言解锁时刻距今约 want（±10 分钟）。
func assertUnlockAround(t *testing.T, got time.Time, want time.Duration) {
	t.Helper()

	if d := time.Until(got); d < want-10*time.Minute || d > want+10*time.Minute {
		t.Errorf("unlock 应在约 %v 后，得到 %v", want, d)
	}
}

// rowsByRealm 把行按域建索引。
func rowsByRealm(rows []ModelLockRow) map[string]ModelLockRow {
	byRealm := map[string]ModelLockRow{}
	for _, r := range rows {
		byRealm[r.Realm] = r
	}

	return byRealm
}

// assertRealmLockRow 断言某域的行：total/locked/reason 均匹配。
func assertRealmLockRow(t *testing.T, byRealm map[string]ModelLockRow, realm string, total, locked int, reason string) {
	t.Helper()

	r, ok := byRealm[realm]
	if !ok || r.Total != total || r.Locked != locked || r.Reason != reason {
		t.Errorf("%s 行 = %+v want total %d locked %d reason %q", realm, r, total, locked, reason)
	}
}

// TestModelLockViewEmpty 没有未过期模型级冷却 → 返回 nil（面板按空态渲染）。
func TestModelLockViewEmpty(t *testing.T) {
	t.Parallel()

	p := lockViewPool(t)
	if rows := p.ModelLockView(); rows != nil {
		t.Fatalf("无冷却时应返回 nil，得到 %+v", rows)
	}
}

// TestModelLockViewPartial 部分号被锁 → partial，且带出可选/总数、最早解锁与原因。
func TestModelLockViewPartial(t *testing.T) {
	t.Parallel()
	p := lockViewPool(t)
	setModelCooldown(p, "u1", "glm-5.3", time.Now().Add(time.Hour), "6004 model rate limit", false)

	rows := p.ModelLockView()

	r := oneRow(t, rows)
	if r.Model != "glm-5.3" || r.Realm != "cn" {
		t.Errorf("model/realm = %q/%q want glm-5.3/cn", r.Model, r.Realm)
	}

	if r.Total != 2 || r.Locked != 1 || r.Servable != 1 {
		t.Errorf("total/locked/servable = %d/%d/%d want 2/1/1", r.Total, r.Locked, r.Servable)
	}

	if r.State != "partial" {
		t.Errorf("state = %q want partial", r.State)
	}

	if r.Reason != "6004 model rate limit" {
		t.Errorf("reason = %q want 6004 model rate limit", r.Reason)
	}

	assertUnlockAround(t, r.UnlockAt, time.Hour)

	if !r.FullyUnlockAt.Equal(r.UnlockAt) {
		t.Errorf("只有一个号被锁时 fully_unlock_at 应等于 unlock_at")
	}
}

// TestModelLockViewLocked 全部参与选号的号都被该模型挡住 → locked（与 ModelBlocked 同义），
// 最早解锁取最小 Until、全池解锁取最大 Until，原因取最早解锁账号的。
func TestModelLockViewLocked(t *testing.T) {
	t.Parallel()
	p := lockViewPool(t)
	base := time.Now()
	setModelCooldown(p, "u1", "glm-5.3", base.Add(2*time.Hour), "reason-late", false)
	setModelCooldown(p, "u2", "glm-5.3", base.Add(time.Hour), "reason-early", false)

	rows := p.ModelLockView()

	r := oneRow(t, rows)
	if r.State != "locked" {
		t.Errorf("state = %q want locked", r.State)
	}

	if r.Servable != 0 || r.Locked != 2 || r.Total != 2 {
		t.Errorf("servable/locked/total = %d/%d/%d want 0/2/2", r.Servable, r.Locked, r.Total)
	}

	if r.Reason != "reason-early" {
		t.Errorf("reason 应取最早解锁账号的，得到 %q", r.Reason)
	}

	assertUnlockAround(t, r.UnlockAt, time.Hour)
	assertUnlockAround(t, r.FullyUnlockAt, 2*time.Hour)
}

// TestModelLockViewStarved 此刻没号能服务、但不是模型冷却造成的（账号级冷却）→ starved，
// 与 locked 区分开：这种情况换模型没用，等一下就好。
func TestModelLockViewStarved(t *testing.T) {
	t.Parallel()
	p := lockViewPool(t)
	setModelCooldown(p, "u1", "glm-5.3", time.Now().Add(time.Hour), "6004 model rate limit", false)
	p.Cooldown("u2", CoolHard, time.Hour, "14018 credits exhausted")

	rows := p.ModelLockView()
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %+v", rows)
	}

	r := rows[0]
	if r.Servable != 0 || r.Locked != 1 || r.Total != 2 {
		t.Errorf("servable/locked/total = %d/%d/%d want 0/1/2", r.Servable, r.Locked, r.Total)
	}

	if r.State != "starved" {
		t.Errorf("state = %q want starved（模型没全锁，只是此刻没号）", r.State)
	}
}

// TestModelLockViewSkipsAuditOnly AuditOnly 台账只审计不拦路由 → 既不产生行，
// 也不计入 locked（口径与 modelCooled 一致，否则会把其实能调的模型报成不可用）。
func TestModelLockViewSkipsAuditOnly(t *testing.T) {
	t.Parallel()
	p := lockViewPool(t)
	setModelCooldown(p, "u1", "glm-5.3", time.Now().Add(time.Hour), "audit", true)

	if rows := p.ModelLockView(); rows != nil {
		t.Fatalf("AuditOnly 条目不应产生行：%+v", rows)
	}
}

// TestModelLockViewSkipsExpired 已过期的冷却不算锁。
func TestModelLockViewSkipsExpired(t *testing.T) {
	t.Parallel()
	p := lockViewPool(t)
	setModelCooldown(p, "u1", "glm-5.3", time.Now().Add(-time.Minute), "6004", false)

	if rows := p.ModelLockView(); rows != nil {
		t.Fatalf("已过期冷却不应产生行：%+v", rows)
	}
}

// TestModelLockViewSkipsDisabledAndPaused 禁用号/暂停号的不可用与模型无关：
// 既不计入分母 Total，也不该被算成「还能服务的证明」。
func TestModelLockViewSkipsDisabledAndPaused(t *testing.T) {
	t.Parallel()
	pool := lockViewPool(t)
	setModelCooldown(pool, "u1", "glm-5.3", time.Now().Add(time.Hour), "6004", false)
	pool.mu.Lock()
	pool.byUID["u2"].disabled = true
	pool.mu.Unlock()

	rows := pool.ModelLockView()
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %+v", rows)
	}

	if rows[0].Total != 1 {
		t.Errorf("total = %d want 1（禁用号不计入分母）", rows[0].Total)
	}

	if rows[0].State != "locked" {
		t.Errorf("state = %q want locked（唯一参与选号的号被锁）", rows[0].State)
	}

	pool.mu.Lock()
	pool.byUID["u2"].disabled = false
	pool.byUID["u2"].paused = true
	pool.mu.Unlock()

	rows = pool.ModelLockView()
	if len(rows) != 1 || rows[0].Total != 1 {
		t.Errorf("暂停号同样不计入分母：%+v", rows)
	}
}

// TestModelLockViewRealmIsolation 同名模型在 cn/global 各自独立计数与原因。
func TestModelLockViewRealmIsolation(t *testing.T) {
	t.Parallel()
	p := realmPool(t)
	setModelCooldown(p, "cn1", "glm-5.3", time.Now().Add(time.Hour), "6004 cn", false)
	setModelCooldown(p, "g1", "glm-5.3", time.Now().Add(time.Hour), "6004 global", false)

	rows := p.ModelLockView()
	if len(rows) != 2 {
		t.Fatalf("同名模型在两个域应各出一行，得到 %+v", rows)
	}

	byRealm := rowsByRealm(rows)
	assertRealmLockRow(t, byRealm, "cn", 2, 1, "6004 cn")
	assertRealmLockRow(t, byRealm, "global", 2, 1, "6004 global")
}

// TestModelLockViewSortsLockedFirst 整池不可用排在部分限流前面，方便一眼看到最该处理的。
func TestModelLockViewSortsLockedFirst(t *testing.T) {
	t.Parallel()
	pool := lockViewPool(t)
	base := time.Now()
	setModelCooldown(pool, "u1", "glm-5.3", base.Add(time.Hour), "6004", false)
	setModelCooldown(pool, "u2", "glm-5.3", base.Add(time.Hour), "6004", false)
	setModelCooldown(pool, "u1", "kimi-k2", base.Add(time.Hour), "6004", false)

	rows := pool.ModelLockView()
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %+v", rows)
	}

	if rows[0].State != "locked" || rows[1].State != "partial" {
		t.Errorf("整池不可用应排在部分限流前：%+v", rows)
	}

	if rows[0].Model != "glm-5.3" {
		t.Errorf("首行应为 glm-5.3，得到 %q", rows[0].Model)
	}
}

// TestModelLockViewAfterClear 走真实写入/清除通道（而非只手搓 map）：
// BlockModelBackoff 后应出 partial 行，BlockModelClear 后应回到 nil。
func TestModelLockViewAfterClear(t *testing.T) {
	t.Parallel()
	pool := lockViewPool(t)
	pool.BlockModelBackoff("u1", "glm-5.3", "11102 model [glm-5.3] service info not found")

	rows := pool.ModelLockView()
	if len(rows) != 1 || rows[0].State != "partial" {
		t.Fatalf("写入负缓存后应有一行 partial：%+v", rows)
	}

	pool.BlockModelClear("u1", "glm-5.3")

	if rows := pool.ModelLockView(); rows != nil {
		t.Fatalf("清除后应回到 nil：%+v", rows)
	}
}

// TestModelLockViewScope 锁池行范围诊断（issue #29）：全锁账号共享同一非零上游
// 重置时刻 → platform；否则 account。
func TestModelLockViewScope(t *testing.T) {
	t.Parallel()

	reset := time.Now().Add(time.Hour)
	p := lockViewPool(t)
	p.CooldownSoftForModel("u1", time.Minute, reset, "glm-5.3", "6004 rate limited")
	p.CooldownSoftForModel("u2", time.Minute, reset, "glm-5.3", "6004 rate limited")

	if row := oneRow(t, p.ModelLockView()); row.Scope != ModelBlockScopePlatform {
		t.Errorf("scope=%q want platform", row.Scope)
	}

	// 不同重置时刻 → 账号级。
	p2 := lockViewPool(t)
	p2.CooldownSoftForModel("u1", time.Minute, reset, "glm-5.3", "6004 rate limited")
	p2.CooldownSoftForModel("u2", time.Minute, reset.Add(30*time.Minute), "glm-5.3", "6004 rate limited")

	if row := oneRow(t, p2.ModelLockView()); row.Scope != ModelBlockScopeAccount {
		t.Errorf("scope=%q want account", row.Scope)
	}
}
