package pool

import (
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// ---------------------------------------------------------------------------
// ModelBlocked：区分「号都在但都对这个模型关闭」与「池子真没号」（issue #102a）
// ---------------------------------------------------------------------------

// TestModelBlockedAllAccountsCooled 全部账号都对该模型冷却 → Blocked，
// 且带出账号数、最早解封时间与上游原因。
func TestModelBlockedAllAccountsCooled(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	pool.Add(&auth.Auth{UID: "u2"})

	base := time.Now()
	// 两个号对同一模型都有冷却，解封时间不同 → 应取最早的。
	pool.mu.Lock()
	pool.byUID["u1"].modelCooldowns = map[string]modelCooldown{
		"glm-5.3": {Until: base.Add(2 * time.Hour), Reason: "11136 model not permitted"},
	}
	pool.byUID["u2"].modelCooldowns = map[string]modelCooldown{
		"glm-5.3": {Until: base.Add(1 * time.Hour), Reason: "11136 model not permitted"},
	}
	pool.mu.Unlock()

	modelBlockStatus := pool.ModelBlocked("glm-5.3")
	if !modelBlockStatus.Blocked {
		t.Fatalf("全部账号冷却时 Blocked 应为 true，得到 %+v", modelBlockStatus)
	}

	if modelBlockStatus.Count != 2 {
		t.Errorf("Count=%d want 2", modelBlockStatus.Count)
	}

	if modelBlockStatus.Reason == "" {
		t.Errorf("Reason 应带出上游原因，得到空")
	}
	// 最早解封应接近 1h 后（u2），不是 2h（u1）。
	if d := time.Until(modelBlockStatus.Until); d > 70*time.Minute || d < 50*time.Minute {
		t.Errorf("Until=%v 应取最早解封（约 1h 后），得到 %v", modelBlockStatus.Until, d)
	}
}

// TestModelBlockedOneAccountStillServes 只要还有账号能服务该模型，就不算模型级阻塞
// ——否则会把「在途占满/积分保底」导致的选号失败误报成模型问题。
func TestModelBlockedOneAccountStillServes(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	pool.Add(&auth.Auth{UID: "u2"})
	pool.mu.Lock()
	pool.byUID["u1"].modelCooldowns = map[string]modelCooldown{
		"glm-5.3": {Until: time.Now().Add(time.Hour), Reason: "11136 ..."},
	}
	pool.mu.Unlock()

	if st := pool.ModelBlocked("glm-5.3"); st.Blocked {
		t.Fatalf("u2 仍可服务该模型，不应判定为模型级阻塞：%+v", st)
	}
	// 但 u2 对该模型无冷却这一点必须是真的（防止测试自身构造错误）。
	pool.mu.RLock()
	cooled := pool.byUID["u2"].modelCooled(time.Now(), "glm-5.3")
	pool.mu.RUnlock()

	if cooled {
		t.Fatalf("测试构造有误：u2 不该对该模型冷却")
	}
}

// TestModelBlockedDisabledDoesNotCount 禁用号不参与判定：它不可用与模型无关，
// 既不构成"还能服务"的证明，也不该被算进被挡账号数。
func TestModelBlockedDisabledDoesNotCount(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	pool.Add(&auth.Auth{UID: "u2"})
	pool.mu.Lock()
	pool.byUID["u1"].modelCooldowns = map[string]modelCooldown{
		"glm-5.3": {Until: time.Now().Add(time.Hour), Reason: "11136 ..."},
	}
	pool.byUID["u2"].disabled = true // 唯一还能服务的号被禁用了
	pool.mu.Unlock()

	modelBlockStatus := pool.ModelBlocked("glm-5.3")
	if !modelBlockStatus.Blocked {
		t.Fatalf("除禁用号外全部冷却 → 应判定为模型级阻塞，得到 %+v", modelBlockStatus)
	}

	if modelBlockStatus.Count != 1 {
		t.Errorf("Count=%d want 1（禁用号不计入）", modelBlockStatus.Count)
	}
}

// TestModelBlockedAuditOnlyNotBlocking AuditOnly 条目只审计不拦截，不能算阻塞
// ——口径必须与选号的 modelCooled 一致，否则会把"其实能调"的模型报成不可用。
func TestModelBlockedAuditOnlyNotBlocking(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	pool.mu.Lock()
	pool.byUID["u1"].modelCooldowns = map[string]modelCooldown{
		"glm-5.3": {Until: time.Now().Add(time.Hour), Reason: "audit", AuditOnly: true},
	}
	pool.mu.Unlock()

	if st := pool.ModelBlocked("glm-5.3"); st.Blocked {
		t.Fatalf("AuditOnly 条目不得判定为阻塞：%+v", st)
	}
}

// TestModelBlockedExpiredNotBlocking 已过期的冷却不算阻塞。
func TestModelBlockedExpiredNotBlocking(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	pool.mu.Lock()
	pool.byUID["u1"].modelCooldowns = map[string]modelCooldown{
		"glm-5.3": {Until: time.Now().Add(-time.Minute), Reason: "11136 ..."},
	}
	pool.mu.Unlock()

	if st := pool.ModelBlocked("glm-5.3"); st.Blocked {
		t.Fatalf("已过期冷却不得判定为阻塞：%+v", st)
	}
}

// TestModelBlockedEmptyInputs 空模型名与空池都返回"未阻塞"：前者无从判断，
// 后者属于"真的没号"，绝不能冒充模型问题。
func TestModelBlockedEmptyInputs(t *testing.T) {
	t.Parallel()

	pool := New("")
	// 空池
	if st := pool.ModelBlocked("glm-5.3"); st.Blocked {
		t.Fatalf("空池不应判定为模型阻塞：%+v", st)
	}
	// 空模型名
	pool.Add(&auth.Auth{UID: "u1"})

	if st := pool.ModelBlocked(""); st.Blocked {
		t.Fatalf("空模型名不应判定为模型阻塞：%+v", st)
	}
}

// TestModelBlockedThenClear 回归：BlockModelBackoff 写入后应判定为阻塞，
// BlockModelClear 清除后应恢复——覆盖真实的写入/清除通道，而不是只手搓 map。
func TestModelBlockedThenClear(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})

	pool.BlockModelBackoff("u1", "glm-5.3", "11136 model not permitted")

	st := pool.ModelBlocked("glm-5.3")
	if !st.Blocked || st.Count != 1 {
		t.Fatalf("写入负缓存后应阻塞：%+v", st)
	}

	if st.Reason == "" {
		t.Errorf("应带出 reason")
	}

	pool.BlockModelClear("u1", "glm-5.3")

	if st := pool.ModelBlocked("glm-5.3"); st.Blocked {
		t.Fatalf("清除后不应再阻塞：%+v", st)
	}
}

// TestModelAccessBlockedThenClear 11141 的模型避让条目也应在成功后清除，
// 与 11136 同通道但独立 reason；6004 限流台账不得被顺手清掉。
func TestModelAccessBlockedThenClear(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})

	pool.BlockModelBackoff("u1", "glm-5.3", "11141 model retired")

	if st := pool.ModelBlocked("glm-5.3"); !st.Blocked {
		t.Fatalf("写入 11141 避让后应阻塞：%+v", st)
	}

	pool.BlockModelClear("u1", "glm-5.3")

	if st := pool.ModelBlocked("glm-5.3"); st.Blocked {
		t.Fatalf("清除后不应再阻塞：%+v", st)
	}
}

// TestModelRateLimitNotCleared 6004 限流台账不受 BlockModelClear 影响。
func TestModelRateLimitNotCleared(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})

	pool.CooldownSoftForModel("u1", time.Minute, time.Now().Add(time.Hour), "glm-5.3", "6004 model rate limit")
	pool.BlockModelClear("u1", "glm-5.3")

	if st := pool.ModelBlocked("glm-5.3"); !st.Blocked {
		t.Fatalf("6004 模型限流不应被成功清除：%+v", st)
	}
}

// TestModelBlockedPausedDoesNotMaskBlock 暂停号不构成「能服务」的证明：
// 除暂停号外全部模型冷却时仍应判定阻塞（合并 #113 时补的口径——
// 否则一个暂停的健康号会掩盖「其余号全被模型级冷却挡住」）。
func TestModelBlockedPausedDoesNotMaskBlock(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1"})
	pool.Add(&auth.Auth{UID: "u2"})
	pool.mu.Lock()
	pool.byUID["u1"].modelCooldowns = map[string]modelCooldown{
		"glm-5.3": {Until: time.Now().Add(time.Hour), Reason: "11136 ..."},
	}
	pool.byUID["u2"].paused = true // 唯一「健康」的号在暂停让位
	pool.mu.Unlock()

	modelBlockStatus := pool.ModelBlocked("glm-5.3")
	if !modelBlockStatus.Blocked {
		t.Fatalf("暂停号不得掩盖模型级阻塞：%+v", modelBlockStatus)
	}

	if modelBlockStatus.Count != 1 {
		t.Errorf("Count=%d want 1（暂停号不计入）", modelBlockStatus.Count)
	}
}

// TestModelBlockedScope 阻塞范围诊断（issue #29）：全池被挡账号共享同一非零
// ResetAt → platform；不同 ResetAt / 单账号 / 无 ResetAt → account。
func TestModelBlockedScope(t *testing.T) {
	t.Parallel()

	reset := time.Now().Add(time.Hour)

	cases := []struct {
		name  string
		setup func(p *Pool)
		want  string
	}{
		{
			name: "platform-same-reset",
			setup: func(pool4 *Pool) {
				pool4.mu.Lock()
				for _, uid := range []string{"u1", "u2"} {
					pool4.byUID[uid].modelCooldowns = map[string]modelCooldown{
						"m": {Until: reset, ResetAt: reset, Reason: "6004 limit"},
					}
				}
				pool4.mu.Unlock()
			},
			want: ModelBlockScopePlatform,
		},
		{
			name: "account-different-reset",
			setup: func(pool *Pool) {
				pool.mu.Lock()
				pool.byUID["u1"].modelCooldowns = map[string]modelCooldown{
					"m": {Until: reset, ResetAt: reset, Reason: "6004 limit"},
				}
				pool.byUID["u2"].modelCooldowns = map[string]modelCooldown{
					"m": {Until: reset.Add(time.Minute), ResetAt: reset.Add(time.Minute), Reason: "6004 limit"},
				}
				pool.mu.Unlock()
			},
			want: ModelBlockScopeAccount,
		},
		{
			name: "account-no-reset",
			setup: func(pool2 *Pool) {
				pool2.mu.Lock()
				for _, uid := range []string{"u1", "u2"} {
					pool2.byUID[uid].modelCooldowns = map[string]modelCooldown{
						"m": {Until: reset, Reason: "11136 model not permitted"},
					}
				}
				pool2.mu.Unlock()
			},
			want: ModelBlockScopeAccount,
		},
		{
			name: "account-single",
			setup: func(pool3 *Pool) {
				pool3.mu.Lock()
				pool3.byUID["u1"].modelCooldowns = map[string]modelCooldown{
					"m": {Until: reset, ResetAt: reset, Reason: "6004 limit"},
				}
				pool3.byUID["u2"].paused = true // 单账号被挡（另一号暂停不计入）
				pool3.mu.Unlock()
			},
			want: ModelBlockScopeAccount,
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			t.Parallel()

			p := New("")
			p.Add(&auth.Auth{UID: "u1"})
			p.Add(&auth.Auth{UID: "u2"})
			item.setup(p)

			modelBlockStatus := p.ModelBlocked("m")
			if !modelBlockStatus.Blocked {
				t.Fatalf("want blocked, got %+v", modelBlockStatus)
			}

			if modelBlockStatus.Scope != item.want {
				t.Errorf("scope=%q want %q", modelBlockStatus.Scope, item.want)
			}

			if item.want == ModelBlockScopePlatform && !modelBlockStatus.SharedResetAt.Equal(reset) {
				t.Errorf("shared reset=%v want %v", modelBlockStatus.SharedResetAt, reset)
			}
		})
	}
}
