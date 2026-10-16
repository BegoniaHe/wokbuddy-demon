package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 成功/失败尝试计数、total 的 pt+ct 兜底口径、按域/账号聚合。
func TestAddAndTotals(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now, "cn", "uid1", "glm-5.2", Delta{PromptTokens: 100, HasPromptTokens: true, CompletionTokens: 50, HasCompletion: true, Credit: 1.5, HasCredit: true, ModelRate: "0.05", LatencyMs: 200, HasLatency: true}, true)
	// 失败尝试：无 usage → 只计请求数与失败数，token 不加。
	r.Add(now, "global", "uid1", "claude-4.6", Delta{}, false)
	// 上游没给 total 时用 pt+ct 兜底，保证总量口径连续。
	r.Add(now, "cn", "uid1", "glm-5.2", Delta{PromptTokens: 10, HasPromptTokens: true, CompletionTokens: 5, HasCompletion: true}, true)

	s := r.Snapshot(24, nil)
	wantEq(t, "requests", s.Totals.Requests, int64(3))
	wantEq(t, "errors", s.Totals.Errors, int64(1))
	wantEq(t, "prompt_tokens", s.Totals.PromptTokens, int64(110))
	wantEq(t, "completion_tokens", s.Totals.CompletionTok, int64(55))
	wantEq(t, "total_tokens", s.Totals.TotalTokens, int64(165))
	wantEq(t, "credits", s.Totals.Credits, 1.5)
	wantEq(t, "credit_samples", s.Totals.CreditSamples, int64(1))
	wantEq(t, "credit_tokens", s.Totals.CreditTokens, int64(150))
	wantEq(t, "credits_per_1m_tokens", s.Totals.CreditsPer1MTokens, 10000.0)
	wantEq(t, "avg_latency_ms", s.Totals.AvgLatencyMs, 200.0)
	wantEq(t, "by_realm 项数", len(s.ByRealm), 2)
	if s.ByAccount[0].Realm == "" {
		t.Fatal("by_account 行缺 realm 标注")
	}
	wantEq(t, "credit_by_account 项数", len(s.CreditByAccount), 1)
	wantEq(t, "credit_by_account key", s.CreditByAccount[0].Key, "uid1")
	wantEq(t, "credit_by_account samples", s.CreditByAccount[0].CreditSamples, int64(1))
	wantEq(t, "credit_by_account ratio", s.CreditByAccount[0].CreditsPer1MTokens, 10000.0)
	wantEq(t, "credit_by_model 项数", len(s.CreditByModel), 1)
	wantEq(t, "credit_by_model key", s.CreditByModel[0].Key, "glm-5.2")
	wantEq(t, "credit_by_model rate", s.CreditByModel[0].Rate, "0.05")
	wantEq(t, "credit_by_model ratio", s.CreditByModel[0].CreditsPer1MTokens, 10000.0)
}

// Rollup 把超出 hourlyKeep 的小时桶折叠为日桶，且幂等：重复折叠不重复计数。
// 窗口口径：24h 窗口不含 100 天前的日桶；hours=0（全部历史）才含日点。
func TestRollupIdempotent(t *testing.T) {
	r := New("")
	old := time.Now().AddDate(0, 0, -100) // 100 天前，超出 90 天小时保留
	r.Add(old, "cn", "u", "m", Delta{PromptTokens: 7, HasPromptTokens: true}, true)
	r.Add(old, "cn", "u", "m", Delta{PromptTokens: 7, HasPromptTokens: true}, true)
	r.Add(time.Now(), "cn", "u", "m", Delta{PromptTokens: 1, HasPromptTokens: true}, true)

	r.Rollup(time.Now())
	after := r.Snapshot(24, nil)
	wantEq(t, "24h requests", after.Totals.Requests, int64(1))
	wantEq(t, "24h prompt_tokens", after.Totals.PromptTokens, int64(1))
	wantEq(t, "24h series 项数", len(after.Series), 1)
	wantEq(t, "24h series scope", after.Series[0].Scope, "hour")

	all := r.Snapshot(0, nil)
	wantEq(t, "全部历史 requests", all.Totals.Requests, int64(3))
	wantEq(t, "全部历史 prompt_tokens", all.Totals.PromptTokens, int64(15))
	wantEq(t, "全部历史 series 项数", len(all.Series), 2)
	wantEq(t, "全部历史 series[0] scope", all.Series[0].Scope, "day")
	wantEq(t, "全部历史 series[1] scope", all.Series[1].Scope, "hour")

	r.Rollup(time.Now())
	again := r.Snapshot(0, nil)
	wantEq(t, "二次折叠 requests", again.Totals.Requests, int64(3))
	wantEq(t, "二次折叠 prompt_tokens", again.Totals.PromptTokens, int64(15))
}

// 落盘→新实例恢复，数据不丢；落盘结构带版本号。
func TestFlushLoadRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	r1 := New(path)
	r1.Add(time.Now(), "cn", "u1", "glm-5.2", Delta{PromptTokens: 42, HasPromptTokens: true, TotalTokens: 42, HasTotal: true}, true)
	r1.Save()

	r2 := New(path)
	s := r2.Snapshot(24, nil)
	if s.Totals.Requests != 1 || s.Totals.TotalTokens != 42 {
		t.Fatalf("恢复后 totals = %d/%d, want 1/42", s.Totals.Requests, s.Totals.TotalTokens)
	}
	raw, _ := os.ReadFile(path)
	var f file
	if err := json.Unmarshal(raw, &f); err != nil || f.Version != fileVersion || len(f.Buckets) != 1 {
		t.Fatalf("落盘文件异常: err=%v buckets=%d", err, len(f.Buckets))
	}
}

// 版本 1 文件没有积分字段：按零值恢复，旧 Token 数据保持可见且不产生伪比例。
func TestLoadLegacyWithoutCredit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	legacy := `{"version":1,"saved":"2026-09-28T00:00:00+08:00","buckets":[{"s":"h:2026-09-28T10","r":"cn","u":"u1","m":"glm-5.2","q":1,"p":42,"t":42}]}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	r := New(path)
	s := r.Snapshot(0, nil)
	if s.Totals.TotalTokens != 42 || s.Totals.CreditSamples != 0 || s.Totals.CreditsPer1MTokens != 0 {
		t.Fatalf("legacy totals = %+v, want token-only history", s.Totals)
	}
	if len(s.CreditByAccount) != 0 || len(s.CreditByModel) != 0 {
		t.Fatalf("legacy credit dimensions = %+v / %+v, want none", s.CreditByAccount, s.CreditByModel)
	}
}

// 小时桶折叠为日桶时必须保留积分、样本数和匹配 Token，比例不能因 Rollup 漂移。
func TestCreditSurvivesRollup(t *testing.T) {
	r := New("")
	old := time.Now().AddDate(0, 0, -100)
	r.Add(old, "cn", "u", "m", Delta{PromptTokens: 100, HasPromptTokens: true, TotalTokens: 100, HasTotal: true, Credit: 1.25, HasCredit: true, ModelRate: "0.5"}, true)
	r.Add(old.Add(2*time.Hour), "cn", "u", "m", Delta{PromptTokens: 300, HasPromptTokens: true, TotalTokens: 300, HasTotal: true, Credit: 3.75, HasCredit: true, ModelRate: "0.5"}, true)
	r.Rollup(time.Now())
	s := r.Snapshot(0, nil)
	if s.Totals.Credits != 5 || s.Totals.CreditSamples != 2 || s.Totals.CreditTokens != 400 || s.Totals.CreditsPer1MTokens != 12500 {
		t.Fatalf("rolled credit totals = %+v, want credits=5 tokens=400 ratio=12500", s.Totals)
	}
	if len(s.CreditByModel) != 1 || s.CreditByModel[0].Rate != "0.5" || s.CreditByModel[0].CreditsPer1MTokens != 12500 {
		t.Fatalf("rolled credit_by_model = %+v, want one rate-preserving row", s.CreditByModel)
	}
}

// 模型维度按“裸模型名 + 生效倍率”合并；同倍率跨账号/时间合并，不同倍率拆行。
func TestCreditDimensionsRateGrouping(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now, "cn", "u1", "cn:glm-5.2", Delta{TotalTokens: 100, HasTotal: true, Credit: 1, HasCredit: true, ModelRate: "0.5"}, true)
	r.Add(now.Add(time.Hour), "cn", "u2", "glm-5.2", Delta{TotalTokens: 200, HasTotal: true, Credit: 2, HasCredit: true, ModelRate: "0.5"}, true)
	r.Add(now.Add(2*time.Hour), "cn", "u2", "glm-5.2", Delta{TotalTokens: 300, HasTotal: true, Credit: 6, HasCredit: true, ModelRate: "0.8"}, true)

	s := r.Snapshot(24, nil)
	if len(s.CreditByAccount) != 2 || len(s.CreditByModel) != 2 {
		t.Fatalf("dimensions accounts=%+v models=%+v, want 2 accounts and 2 model-rate rows", s.CreditByAccount, s.CreditByModel)
	}
	if s.CreditByModel[0].Key != "glm-5.2" || s.CreditByModel[0].Rate != "0.8" ||
		s.CreditByModel[0].Credits != 6 || s.CreditByModel[0].CreditTokens != 300 {
		t.Fatalf("first model row = %+v, want rate=0.8 credits=6 tokens=300", s.CreditByModel[0])
	}
	if s.CreditByModel[1].Rate != "0.5" || s.CreditByModel[1].Credits != 3 || s.CreditByModel[1].CreditTokens != 300 {
		t.Fatalf("merged model row = %+v, want rate=0.5 credits=3 tokens=300", s.CreditByModel[1])
	}
}

// 旧桶缺倍率时由当前目录倍率回填，并与新桶同倍率记录合并；目录缺失时保留未知行。
func TestCreditLegacyRateFallback(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now, "cn", "u1", "glm-5.2", Delta{TotalTokens: 100, HasTotal: true, Credit: 1, HasCredit: true}, true)
	r.Add(now.Add(time.Hour), "cn", "u1", "glm-5.2", Delta{TotalTokens: 200, HasTotal: true, Credit: 2, HasCredit: true, ModelRate: "0.79"}, true)

	s := r.SnapshotWithRates(24, nil, func(realm, model string) string {
		if realm == "cn" && model == "glm-5.2" {
			return "0.79"
		}
		return ""
	})
	if len(s.CreditByModel) != 1 || s.CreditByModel[0].Rate != "0.79" ||
		s.CreditByModel[0].Credits != 3 || s.CreditByModel[0].CreditTokens != 300 {
		t.Fatalf("fallback model rows = %+v, want legacy merged into rate=0.79", s.CreditByModel)
	}
}

// Snapshot 全口径窗口过滤：窗口外的数据不进**任何**聚合（卡片/表格/时序），
// 切窗口数字随之变化；hours=0 全部历史。Buckets 为窗口内命中的桶数。
func TestSnapshotWindowFilter(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now.Add(-48*time.Hour), "cn", "u", "m", Delta{PromptTokens: 5, HasPromptTokens: true}, true) // 窗口(24h)外
	r.Add(now, "cn", "u", "m", Delta{PromptTokens: 3, HasPromptTokens: true}, true)                    // 窗口内
	s := r.Snapshot(24, nil)
	wantEq(t, "24h 窗口 requests", s.Totals.Requests, int64(1))
	wantEq(t, "24h 窗口 prompt_tokens", s.Totals.PromptTokens, int64(3))
	wantEq(t, "series 项数", len(s.Series), 1)
	wantEq(t, "series scope", s.Series[0].Scope, "hour")
	wantEq(t, "series prompt_tokens", s.Series[0].PromptTokens, int64(3))
	wantEq(t, "buckets", s.Buckets, 1)

	all := r.Snapshot(0, nil)
	wantEq(t, "全部历史 requests", all.Totals.Requests, int64(2))
	wantEq(t, "全部历史 prompt_tokens", all.Totals.PromptTokens, int64(8))
	// since 是全库数据起点，不受窗口影响。
	if all.Since == "" || s.Since != all.Since {
		t.Fatalf("since 应为全库起点且不随窗口变化: all=%q windowed=%q", all.Since, s.Since)
	}
}

// 显式区间（「今天」/「自定义」）与滚动窗口走同一套全口径过滤；区间是闭区间
// （桶起点落在 [From, To] 内即命中），且 From/To 会回显给面板确认口径。
func TestSnapshotExplicitWindow(t *testing.T) {
	r := New("")
	base := time.Now().Truncate(time.Hour).Add(-5 * time.Hour)
	for i := range 6 {
		r.Add(base.Add(time.Duration(i)*time.Hour), "cn", "u", "m",
			Delta{PromptTokens: 10, HasPromptTokens: true}, true)
	}
	// 只取中间两小时（base+2h、base+3h）。
	s := r.SnapshotWindow(Window{
		From: base.Add(2 * time.Hour),
		To:   base.Add(3 * time.Hour),
	}, nil, nil)
	wantEq(t, "显式区间 requests", s.Totals.Requests, int64(2))
	wantEq(t, "显式区间 prompt_tokens", s.Totals.PromptTokens, int64(20))
	wantEq(t, "显式区间 series 项数", len(s.Series), 2)
	wantEq(t, "显式区间 buckets", s.Buckets, 2)
	if s.WindowFrom == "" || s.WindowTo == "" {
		t.Fatalf("显式区间应回显 window_from/window_to: %+v", s)
	}
	if _, err := time.Parse(time.RFC3339, s.WindowFrom); err != nil {
		t.Fatalf("window_from 不是 RFC3339: %q", s.WindowFrom)
	}

	// 只有 From（「今天」的形态）：从该点起到最新，全量命中。
	only := r.SnapshotWindow(Window{From: base.Add(4 * time.Hour)}, nil, nil)
	wantEq(t, "仅 From 的 requests", only.Totals.Requests, int64(2))
	if only.WindowFrom == "" || only.WindowTo != "" {
		t.Fatalf("仅 From 时 window_to 应为空: %+v", only)
	}

	// 空窗口（From/To 全零且 Hours<=0）= 全部历史，与 Snapshot(0) 等价。
	all := r.SnapshotWindow(Window{}, nil, nil)
	wantEq(t, "全零窗口 requests", all.Totals.Requests, int64(6))
	if all.WindowFrom != "" || all.WindowTo != "" {
		t.Fatalf("全部历史不应回显区间: %+v", all)
	}
}

// 滚动窗口的上限仍是 60 天，且与显式区间互不干扰（From/To 优先）。
func TestWindowBounds(t *testing.T) {
	// From/To 优先于 Hours。
	from := time.Now().Add(-2 * time.Hour)
	gotFrom, gotTo := Window{Hours: 720, From: from}.bounds()
	if !gotFrom.Equal(from) || !gotTo.IsZero() {
		t.Fatalf("From 应优先于 Hours: from=%v to=%v", gotFrom, gotTo)
	}
	// 只有 Hours：起点 = 当前整点往回 Hours-1 小时。
	f, to := Window{Hours: 24}.bounds()
	want := time.Now().Truncate(time.Hour).Add(-23 * time.Hour)
	if !f.Equal(want) || !to.IsZero() {
		t.Fatalf("24h bounds = %v/%v, want %v/零值", f, to, want)
	}
	// Hours<=0 且无 From/To = 全部历史。
	if f, to := (Window{}).bounds(); !f.IsZero() || !to.IsZero() {
		t.Fatalf("空窗口 bounds = %v/%v, want 零值/零值", f, to)
	}
	// 上限 60 天。
	f60, _ := Window{Hours: 100000}.bounds()
	want60 := time.Now().Truncate(time.Hour).Add(-(24*60 - 1) * time.Hour)
	if !f60.Equal(want60) {
		t.Fatalf("超限 Hours 未被夹到 60 天: %v want %v", f60, want60)
	}
}

// 脏 scope（解析失败）不进任何口径，也不会让整次快照失败。
func TestBucketTimeRejectsGarbage(t *testing.T) {
	if _, ok := bucketTime("h:not-a-time"); ok {
		t.Fatal("脏小时 scope 应判定失败")
	}
	if _, ok := bucketTime("d:2026-13-45"); ok {
		t.Fatal("脏日 scope 应判定失败")
	}
	if ts, ok := bucketTime("h:2026-09-30T13"); !ok || ts.Hour() != 13 {
		t.Fatalf("合法小时 scope 解析失败: %v %v", ts, ok)
	}
	if ts, ok := bucketTime("d:2026-09-30"); !ok || ts.Day() != 30 {
		t.Fatalf("合法日 scope 解析失败: %v %v", ts, ok)
	}
}

// Stop 触发最终落盘（Start 后未到防抖间隔也要落）。
func TestLifecycleFlush(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	r := New(path)
	r.Start()
	r.Add(time.Now(), "cn", "u", "m", Delta{PromptTokens: 9, HasPromptTokens: true}, true)
	r.Stop()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Stop 后应有落盘文件: %v", err)
	}
}

// TestCacheHitRateAccumulation（issue #92）：桶累计 hit/miss，credit_by_model
// 与 totals 输出命中率；无缓存观测的桶不参与（rate 零值省略）。
func TestCacheHitRateAccumulation(t *testing.T) {
	r := New(t.TempDir() + "/usage.json")
	base := time.Date(2026, 10, 1, 10, 0, 0, 0, time.Local)
	r.Add(base, "cn", "u1", "glm-5.3", Delta{
		PromptTokens: 1000, HasPromptTokens: true,
		Credit: 1, HasCredit: true, TotalTokens: 1005, HasTotal: true,
		HasCacheTokens: true, CacheHitTokens: 900, CacheMissTokens: 100,
	}, true)
	r.Add(base.Add(time.Minute), "cn", "u1", "glm-5.3", Delta{
		PromptTokens: 1000, HasPromptTokens: true,
		Credit: 2, HasCredit: true, TotalTokens: 1004, HasTotal: true,
		HasCacheTokens: true, CacheHitTokens: 100, CacheMissTokens: 900,
	}, true)
	snap := r.Snapshot(0, nil)
	if got := snap.CreditByModel[0].CacheHitRate; got != 50 {
		t.Fatalf("cache_hit_rate=%v want 50", got)
	}
	if snap.Totals.CacheHitRate != 50 {
		t.Fatalf("totals cache_hit_rate=%v want 50", snap.Totals.CacheHitRate)
	}
	if snap.CreditByModel[0].CacheHitTokens != 1000 || snap.CreditByModel[0].CacheMissTokens != 1000 {
		t.Fatalf("hit/miss 累计错误: %+v", snap.CreditByModel[0])
	}

	// 无缓存观测的桶：rate 零值。
	r2 := New(t.TempDir() + "/usage2.json")
	r2.Add(base, "cn", "u1", "m", Delta{PromptTokens: 10, HasPromptTokens: true, Credit: 1, HasCredit: true, TotalTokens: 12, HasTotal: true}, true)
	if got := r2.Snapshot(0, nil).CreditByModel[0].CacheHitRate; got != 0 {
		t.Fatalf("无缓存观测应 rate=0, got %v", got)
	}
}

// TestCacheTokensPersistRoundtrip：ch/cm 落盘 v4 并无损恢复。
func TestCacheTokensPersistRoundtrip(t *testing.T) {
	dir := t.TempDir()
	r := New(dir + "/usage.json")
	base := time.Date(2026, 10, 1, 10, 0, 0, 0, time.Local)
	r.Add(base, "cn", "u1", "m", Delta{
		Credit: 1, HasCredit: true, TotalTokens: 100, HasTotal: true,
		HasCacheTokens: true, CacheHitTokens: 75, CacheMissTokens: 25,
	}, true)
	r.Save()
	r2 := New(dir + "/usage.json")
	snap := r2.Snapshot(0, nil)
	if snap.CreditByModel[0].CacheHitRate != 75 {
		t.Fatalf("恢复后 cache_hit_rate=%v want 75", snap.CreditByModel[0].CacheHitRate)
	}
}

// AggregateByModel 按裸模型名聚合全历史（跨 realm/uid/时间片），按 key 升序。
func TestAggregateByModel(t *testing.T) {
	r := New("")
	now := time.Now()
	// 同一模型两个账号/两个域/两个时间片，应合并为一行。
	r.Add(now, "cn", "uid1", "glm-5.2", Delta{PromptTokens: 100, HasPromptTokens: true, CompletionTokens: 50, HasCompletion: true}, true)
	r.Add(now.Add(-2*time.Hour), "global", "uid2", "glm-5.2", Delta{PromptTokens: 10, HasPromptTokens: true}, false)
	r.Add(now, "cn", "uid1", "claude-4.6", Delta{TotalTokens: 7, HasTotal: true}, true)

	rows := r.AggregateByModel()
	wantEq(t, "项数", len(rows), 2)
	// 升序：claude-4.6 < glm-5.2。
	wantEq(t, "rows[0].key", rows[0].Key, "claude-4.6")
	wantEq(t, "claude requests", rows[0].Requests, int64(1))
	wantEq(t, "glm requests", rows[1].Requests, int64(2))
	wantEq(t, "glm errors", rows[1].Errors, int64(1))
	wantEq(t, "glm prompt_tokens", rows[1].PromptTokens, int64(110))
	wantEq(t, "glm completion_tokens", rows[1].CompletionTok, int64(50))
	wantEq(t, "glm total_tokens(兜底 pt+ct)", rows[1].TotalTokens, int64(160))

	// 空记录器返回空切片，非 nil。
	empty := New("").AggregateByModel()
	if empty == nil {
		t.Fatal("空记录器应返回非 nil 空切片")
	}
	wantEq(t, "空切片长度", len(empty), 0)
}

// TestTTFTAggregation TTFT 采样：仅 HasTTFT 的样本参与均值，非流式不污染样本（issue #16）。
func TestTTFTAggregation(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now, "cn", "uid1", "glm-5.2", Delta{TTFTMs: 300, HasTTFT: true, LatencyMs: 2000, HasLatency: true}, true)
	r.Add(now, "cn", "uid1", "glm-5.2", Delta{TTFTMs: 500, HasTTFT: true, LatencyMs: 2500, HasLatency: true}, true)
	// 非流式：无 TTFT 观测，不参与均值。
	r.Add(now, "cn", "uid1", "glm-5.2", Delta{LatencyMs: 100, HasLatency: true}, true)

	s := r.Snapshot(24, nil)
	wantEq(t, "avg_ttft_ms", s.Totals.AvgTTFTMs, 400.0)
	wantEq(t, "avg_latency_ms", s.Totals.AvgLatencyMs, (2000.0+2500.0+100.0)/3)
}

// TestBackfillOverridesLocalDay 上游回填覆盖同 (天, realm, uid, model) 的本地桶，
// 不与本地实时记录重复计数（issue #31）。
func TestBackfillOverridesLocalDay(t *testing.T) {
	r := New("")
	now := time.Now()
	day := now.Format("2006-01-02")
	r.Add(now, "cn", "uid1", "glm-5.2", Delta{TotalTokens: 10, HasTotal: true, Credit: 1, HasCredit: true}, true)

	n := r.Backfill([]BackfillRow{{Day: day, Realm: "cn", UID: "uid1", Model: "glm-5.2", Requests: 5, Credits: 9, Tokens: 50}})
	if n != 1 {
		t.Fatalf("backfill buckets=%d want 1", n)
	}
	s := r.Snapshot(0, nil)
	wantEq(t, "requests(覆盖后)", s.Totals.Requests, int64(5))
	wantEq(t, "credits(覆盖后)", s.Totals.Credits, 9.0)
	wantEq(t, "total_tokens(覆盖后)", s.Totals.TotalTokens, int64(50))
}

// TestBackfillThenRealtimeNoDoubleCount 回填后同日的实时记录与 Rollup 折叠都不与
// 回填值重复计数（issue #31 复审）：回填删除当日本地桶，故实时记录落在新小时桶，
// 与 Synced 日桶相加即正确总量；小时桶折叠进 Synced 日桶同样不叠加。
func TestBackfillThenRealtimeNoDoubleCount(t *testing.T) {
	r := New("")
	old := time.Now().Add(-100 * 24 * time.Hour) // 早于 hourlyKeep，可被 Rollup 折叠
	day := old.Format("2006-01-02")
	r.Backfill([]BackfillRow{{Day: day, Realm: "cn", UID: "u1", Model: "m", Requests: 5, Credits: 9, Tokens: 50}})
	r.Add(old, "cn", "u1", "m", Delta{TotalTokens: 20, HasTotal: true, Credit: 2, HasCredit: true}, true)
	r.Rollup(time.Now())
	s := r.Snapshot(0, nil)
	wantEq(t, "requests(回填+实时)", s.Totals.Requests, int64(6))
	wantEq(t, "credits(回填+实时)", s.Totals.Credits, 11.0)
	wantEq(t, "total_tokens(回填+实时)", s.Totals.TotalTokens, int64(70))
}

// wantEq 断言 got == want，失败时用 label 指明字段。统一入口避免逐项 if 抬高
// 测试函数复杂度，断言条件与原逐项检查等价。
func wantEq[T comparable](t *testing.T, label string, got, want T) {
	t.Helper()
	if got != want {
		t.Fatalf("%s = %v, want %v", label, got, want)
	}
}
