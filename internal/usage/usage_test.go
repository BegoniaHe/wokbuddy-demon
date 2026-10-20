package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// 成功/失败尝试计数、total 的 pt+ct 兜底口径、按域/账号聚合。
func TestAddAndTotals(t *testing.T) {
	t.Parallel()

	r := New("")
	now := time.Now()
	r.Add(now, "cn", "uid1", "glm-5.2", &Delta{PromptTokens: 100, HasPromptTokens: true, CompletionTokens: 50, HasCompletion: true, Credit: 1.5, HasCredit: true, ModelRate: "0.05", LatencyMs: 200, HasLatency: true}, true)
	// 失败尝试：无 usage → 只计请求数与失败数，token 不加。
	r.Add(now, "global", "uid1", "claude-4.6", &Delta{}, false)
	// 上游没给 total 时用 pt+ct 兜底，保证总量口径连续。
	r.Add(now, "cn", "uid1", "glm-5.2", &Delta{PromptTokens: 10, HasPromptTokens: true, CompletionTokens: 5, HasCompletion: true}, true)

	snapshot := r.Snapshot(24, nil)
	wantEq(t, "requests", snapshot.Totals.Requests, int64(3))
	wantEq(t, "errors", snapshot.Totals.Errors, int64(1))
	wantEq(t, "prompt_tokens", snapshot.Totals.PromptTokens, int64(110))
	wantEq(t, "completion_tokens", snapshot.Totals.CompletionTok, int64(55))
	wantEq(t, "total_tokens", snapshot.Totals.TotalTokens, int64(165))
	wantEq(t, "credits", snapshot.Totals.Credits, 1.5)
	wantEq(t, "credit_samples", snapshot.Totals.CreditSamples, int64(1))
	wantEq(t, "credit_tokens", snapshot.Totals.CreditTokens, int64(150))
	wantEq(t, "credits_per_1m_tokens", snapshot.Totals.CreditsPer1MTokens, 10000.0)
	wantEq(t, "avg_latency_ms", snapshot.Totals.AvgLatencyMs, 200.0)
	wantEq(t, "by_realm 项数", len(snapshot.ByRealm), 2)

	if snapshot.ByAccount[0].Realm == "" {
		t.Fatal("by_account 行缺 realm 标注")
	}

	wantEq(t, "credit_by_account 项数", len(snapshot.CreditByAccount), 1)
	wantEq(t, "credit_by_account key", snapshot.CreditByAccount[0].Key, "uid1")
	wantEq(t, "credit_by_account samples", snapshot.CreditByAccount[0].CreditSamples, int64(1))
	wantEq(t, "credit_by_account ratio", snapshot.CreditByAccount[0].CreditsPer1MTokens, 10000.0)
	wantEq(t, "credit_by_model 项数", len(snapshot.CreditByModel), 1)
	wantEq(t, "credit_by_model key", snapshot.CreditByModel[0].Key, "glm-5.2")
	wantEq(t, "credit_by_model rate", snapshot.CreditByModel[0].Rate, "0.05")
	wantEq(t, "credit_by_model ratio", snapshot.CreditByModel[0].CreditsPer1MTokens, 10000.0)
}

// Rollup 把超出 hourlyKeep 的小时桶折叠为日桶，且幂等：重复折叠不重复计数。
// 窗口口径：24h 窗口不含 100 天前的日桶；hours=0（全部历史）才含日点。
func TestRollupIdempotent(t *testing.T) {
	t.Parallel()

	r := New("")
	old := time.Now().AddDate(0, 0, -100) // 100 天前，超出 90 天小时保留
	r.Add(old, "cn", "u", "m", &Delta{PromptTokens: 7, HasPromptTokens: true}, true)
	r.Add(old, "cn", "u", "m", &Delta{PromptTokens: 7, HasPromptTokens: true}, true)
	r.Add(time.Now(), "cn", "u", "m", &Delta{PromptTokens: 1, HasPromptTokens: true}, true)

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
	t.Parallel()
	path := filepath.Join(t.TempDir(), "usage.json")
	r1 := New(path)
	r1.Add(time.Now(), "cn", "u1", "glm-5.2", &Delta{PromptTokens: 42, HasPromptTokens: true, TotalTokens: 42, HasTotal: true}, true)
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
	t.Parallel()
	path := filepath.Join(t.TempDir(), "usage.json")

	legacy := `{"version":1,"saved":"2026-09-28T00:00:00+08:00","buckets":[{"s":"h:2026-09-28T10","r":"cn","u":"u1","m":"glm-5.2","q":1,"p":42,"t":42}]}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	r := New(path)

	snapshot := r.Snapshot(0, nil)
	if snapshot.Totals.TotalTokens != 42 || snapshot.Totals.CreditSamples != 0 || snapshot.Totals.CreditsPer1MTokens != 0 {
		t.Fatalf("legacy totals = %+v, want token-only history", snapshot.Totals)
	}

	if len(snapshot.CreditByAccount) != 0 || len(snapshot.CreditByModel) != 0 {
		t.Fatalf("legacy credit dimensions = %+v / %+v, want none", snapshot.CreditByAccount, snapshot.CreditByModel)
	}
}

// 小时桶折叠为日桶时必须保留积分、样本数和匹配 Token，比例不能因 Rollup 漂移。
func TestCreditSurvivesRollup(t *testing.T) {
	t.Parallel()

	r := New("")
	old := time.Now().AddDate(0, 0, -100)
	r.Add(old, "cn", "u", "m", &Delta{PromptTokens: 100, HasPromptTokens: true, TotalTokens: 100, HasTotal: true, Credit: 1.25, HasCredit: true, ModelRate: "0.5"}, true)
	r.Add(old.Add(2*time.Hour), "cn", "u", "m", &Delta{PromptTokens: 300, HasPromptTokens: true, TotalTokens: 300, HasTotal: true, Credit: 3.75, HasCredit: true, ModelRate: "0.5"}, true)
	r.Rollup(time.Now())

	snapshot := r.Snapshot(0, nil)
	if snapshot.Totals.Credits != 5 || snapshot.Totals.CreditSamples != 2 || snapshot.Totals.CreditTokens != 400 || snapshot.Totals.CreditsPer1MTokens != 12500 {
		t.Fatalf("rolled credit totals = %+v, want credits=5 tokens=400 ratio=12500", snapshot.Totals)
	}

	if len(snapshot.CreditByModel) != 1 || snapshot.CreditByModel[0].Rate != "0.5" || snapshot.CreditByModel[0].CreditsPer1MTokens != 12500 {
		t.Fatalf("rolled credit_by_model = %+v, want one rate-preserving row", snapshot.CreditByModel)
	}
}

// 模型维度按“裸模型名 + 生效倍率”合并；同倍率跨账号/时间合并，不同倍率拆行。
func TestCreditDimensionsRateGrouping(t *testing.T) {
	t.Parallel()

	r := New("")
	now := time.Now()
	r.Add(now, "cn", "u1", "cn:glm-5.2", &Delta{TotalTokens: 100, HasTotal: true, Credit: 1, HasCredit: true, ModelRate: "0.5"}, true)
	r.Add(now.Add(time.Hour), "cn", "u2", "glm-5.2", &Delta{TotalTokens: 200, HasTotal: true, Credit: 2, HasCredit: true, ModelRate: "0.5"}, true)
	r.Add(now.Add(2*time.Hour), "cn", "u2", "glm-5.2", &Delta{TotalTokens: 300, HasTotal: true, Credit: 6, HasCredit: true, ModelRate: "0.8"}, true)

	snapshot := r.Snapshot(24, nil)
	if len(snapshot.CreditByAccount) != 2 || len(snapshot.CreditByModel) != 2 {
		t.Fatalf("dimensions accounts=%+v models=%+v, want 2 accounts and 2 model-rate rows", snapshot.CreditByAccount, snapshot.CreditByModel)
	}

	if snapshot.CreditByModel[0].Key != "glm-5.2" || snapshot.CreditByModel[0].Rate != "0.8" ||
		snapshot.CreditByModel[0].Credits != 6 || snapshot.CreditByModel[0].CreditTokens != 300 {
		t.Fatalf("first model row = %+v, want rate=0.8 credits=6 tokens=300", snapshot.CreditByModel[0])
	}

	if snapshot.CreditByModel[1].Rate != "0.5" || snapshot.CreditByModel[1].Credits != 3 || snapshot.CreditByModel[1].CreditTokens != 300 {
		t.Fatalf("merged model row = %+v, want rate=0.5 credits=3 tokens=300", snapshot.CreditByModel[1])
	}
}

// 旧桶缺倍率时由当前目录倍率回填，并与新桶同倍率记录合并；目录缺失时保留未知行。
func TestCreditLegacyRateFallback(t *testing.T) {
	t.Parallel()

	r := New("")
	now := time.Now()
	r.Add(now, "cn", "u1", "glm-5.2", &Delta{TotalTokens: 100, HasTotal: true, Credit: 1, HasCredit: true}, true)
	r.Add(now.Add(time.Hour), "cn", "u1", "glm-5.2", &Delta{TotalTokens: 200, HasTotal: true, Credit: 2, HasCredit: true, ModelRate: "0.79"}, true)

	snapshot := r.SnapshotWithRates(24, nil, func(realm, model string) string {
		if realm == "cn" && model == "glm-5.2" {
			return "0.79"
		}

		return ""
	})
	if len(snapshot.CreditByModel) != 1 || snapshot.CreditByModel[0].Rate != "0.79" ||
		snapshot.CreditByModel[0].Credits != 3 || snapshot.CreditByModel[0].CreditTokens != 300 {
		t.Fatalf("fallback model rows = %+v, want legacy merged into rate=0.79", snapshot.CreditByModel)
	}
}

// Snapshot 全口径窗口过滤：窗口外的数据不进**任何**聚合（卡片/表格/时序），
// 切窗口数字随之变化；hours=0 全部历史。Buckets 为窗口内命中的桶数。
func TestSnapshotWindowFilter(t *testing.T) {
	t.Parallel()

	r := New("")
	now := time.Now()
	r.Add(now.Add(-48*time.Hour), "cn", "u", "m", &Delta{PromptTokens: 5, HasPromptTokens: true}, true) // 窗口(24h)外
	r.Add(now, "cn", "u", "m", &Delta{PromptTokens: 3, HasPromptTokens: true}, true)                    // 窗口内
	snapshot := r.Snapshot(24, nil)
	wantEq(t, "24h 窗口 requests", snapshot.Totals.Requests, int64(1))
	wantEq(t, "24h 窗口 prompt_tokens", snapshot.Totals.PromptTokens, int64(3))
	wantEq(t, "series 项数", len(snapshot.Series), 1)
	wantEq(t, "series scope", snapshot.Series[0].Scope, "hour")
	wantEq(t, "series prompt_tokens", snapshot.Series[0].PromptTokens, int64(3))
	wantEq(t, "buckets", snapshot.Buckets, 1)

	all := r.Snapshot(0, nil)
	wantEq(t, "全部历史 requests", all.Totals.Requests, int64(2))
	wantEq(t, "全部历史 prompt_tokens", all.Totals.PromptTokens, int64(8))
	// since 是全库数据起点，不受窗口影响。
	if all.Since == "" || snapshot.Since != all.Since {
		t.Fatalf("since 应为全库起点且不随窗口变化: all=%q windowed=%q", all.Since, snapshot.Since)
	}
}

// wallHourStarts 返回 now 所在本地整点往回 count 个互不相同的墙钟小时桶起点
// （升序）。桶 scope 按本地墙钟整点归档，用绝对 Add 取相邻小时会在夏令时切换日
// 失真：回拨日的重复小时生成重复 scope（6 条记录只剩 5 个桶），拨快日不存在的
// 整点被归一成同一时刻。这里按墙钟小时回退并按 Unix 时刻去重，保证每个返回
// 时刻都是一个独立桶的起点。
func wallHourStarts(now time.Time, count int) []time.Time {
	base := time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), 0, 0, 0, now.Location())

	out := make([]time.Time, 0, count)
	seen := make(map[int64]struct{}, count)

	for k := 0; len(out) < count; k++ {
		t := time.Date(base.Year(), base.Month(), base.Day(), base.Hour()-k, 0, 0, 0, base.Location())
		if _, dup := seen[t.Unix()]; dup {
			continue
		}

		seen[t.Unix()] = struct{}{}
		out = append(out, t)
	}

	slices.Reverse(out)

	return out
}

// 显式区间（「今天」/「自定义」）与滚动窗口走同一套全口径过滤；区间是闭区间
// （桶起点落在 [From, To] 内即命中），且 From/To 会回显给面板确认口径。
func TestSnapshotExplicitWindow(t *testing.T) {
	t.Parallel()

	r := New("")

	hours := wallHourStarts(time.Now(), 6)
	for _, at := range hours {
		r.Add(at, "cn", "u", "m", &Delta{PromptTokens: 10, HasPromptTokens: true}, true)
	}
	// 只取中间两小时（hours[2]、hours[3]）。
	snapshot := r.SnapshotWindow(Window{
		From:  hours[2],
		To:    hours[3],
		Hours: 0,
	}, nil, nil)
	wantEq(t, "显式区间 requests", snapshot.Totals.Requests, int64(2))
	wantEq(t, "显式区间 prompt_tokens", snapshot.Totals.PromptTokens, int64(20))
	wantEq(t, "显式区间 series 项数", len(snapshot.Series), 2)
	wantEq(t, "显式区间 buckets", snapshot.Buckets, 2)

	if snapshot.WindowFrom == "" || snapshot.WindowTo == "" {
		t.Fatalf("显式区间应回显 window_from/window_to: %+v", snapshot)
	}

	if _, err := time.Parse(time.RFC3339, snapshot.WindowFrom); err != nil {
		t.Fatalf("window_from 不是 RFC3339: %q", snapshot.WindowFrom)
	}

	// 只有 From（「今天」的形态）：从该点起到最新，全量命中。
	only := r.SnapshotWindow(Window{From: hours[4], Hours: 0, To: time.Time{}}, nil, nil)
	wantEq(t, "仅 From 的 requests", only.Totals.Requests, int64(2))

	if only.WindowFrom == "" || only.WindowTo != "" {
		t.Fatalf("仅 From 时 window_to 应为空: %+v", only)
	}

	// 空窗口（From/To 全零且 Hours<=0）= 全部历史，与 Snapshot(0) 等价。
	all := r.SnapshotWindow(Window{Hours: 0, From: time.Time{}, To: time.Time{}}, nil, nil)
	wantEq(t, "全零窗口 requests", all.Totals.Requests, int64(6))

	if all.WindowFrom != "" || all.WindowTo != "" {
		t.Fatalf("全部历史不应回显区间: %+v", all)
	}
}

// 滚动窗口的上限仍是 60 天，且与显式区间互不干扰（From/To 优先）。
func TestWindowBounds(t *testing.T) {
	t.Parallel()
	// From/To 优先于 Hours。
	from := time.Now().Add(-2 * time.Hour)

	gotFrom, gotTo := Window{Hours: 720, From: from, To: time.Time{}}.bounds()
	if !gotFrom.Equal(from) || !gotTo.IsZero() {
		t.Fatalf("From 应优先于 Hours: from=%v to=%v", gotFrom, gotTo)
	}
	// 只有 Hours：起点 = 当前整点桶往回 Hours-1 个桶。用 UTC 固定时刻注入，
	// 避免宿主机时区（尤其 Lord Howe 这类 30 分钟 DST）影响期望值。
	now := time.Date(2026, 10, 20, 14, 37, 0, 0, time.UTC)

	f, to := Window{Hours: 24, From: time.Time{}, To: time.Time{}}.boundsAt(now)

	want := time.Date(2026, 10, 19, 15, 0, 0, 0, time.UTC)
	if !f.Equal(want) || !to.IsZero() {
		t.Fatalf("24h bounds = %v/%v, want %v/零值", f, to, want)
	}
	// Hours<=0 且无 From/To = 全部历史。
	if f, to := (Window{Hours: 0, From: time.Time{}, To: time.Time{}}).bounds(); !f.IsZero() || !to.IsZero() {
		t.Fatalf("空窗口 bounds = %v/%v, want 零值/零值", f, to)
	}
	// 上限 60 天。
	f60, _ := Window{Hours: 100000, From: time.Time{}, To: time.Time{}}.boundsAt(now)

	want60 := time.Date(2026, 8, 21, 15, 0, 0, 0, time.UTC)
	if !f60.Equal(want60) {
		t.Fatalf("超限 Hours 未被夹到 60 天: %v want %v", f60, want60)
	}
}

// 夏令时回拨的重复小时里 time.Date 可能选到更晚的那次 occurrence，让滚动窗口
// 起点落到 now 之后：hours=1 还能命中桶，hours>=2 会少算上一小时。
func TestLocalHourStartAcrossFallBack(t *testing.T) {
	t.Parallel()

	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skipf("tzdata 不可用，跳过 DST 用例: %v", err)
	}

	// 2026-10-25 03:00 CEST 回拨到 02:00 CET：00:17 UTC 是第一次 02:17 CEST。
	now := time.Date(2026, 10, 25, 0, 17, 0, 0, time.UTC).In(loc)

	got := localHourStart(now)
	if got.After(now) {
		t.Fatalf("hour start=%v 晚于 now=%v（重复小时选了更晚的 occurrence）", got, now)
	}

	if want := now.Add(-17 * time.Minute); !got.Equal(want) {
		t.Fatalf("hour start=%v want %v", got, want)
	}

	from, _ := (Window{Hours: 2, From: time.Time{}, To: time.Time{}}).boundsAt(now)
	if want := now.Add(-time.Hour - 17*time.Minute); !from.Equal(want) {
		t.Fatalf("hours=2 from=%v want %v", from, want)
	}
}

// 回拨日的第二次 02:xx（CET）同样要按墙钟桶回退：此刻 02 点桶的起点是第二次
// occurrence，绝对回退一小时仍停在 02 点，hours>=2 会漏掉 01 点桶。
func TestWindowBoundsFallBackSecondOccurrence(t *testing.T) {
	t.Parallel()

	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skipf("tzdata 不可用，跳过 DST 用例: %v", err)
	}

	// 2026-10-25 03:00 CEST 回拨到 02:00 CET：01:17 UTC 是第二次 02:17 CET。
	now := time.Date(2026, 10, 25, 1, 17, 0, 0, time.UTC).In(loc)

	cases := []struct {
		hours int
		want  time.Time // UTC 时刻，对应墙钟 01:00 / 00:00 CEST
	}{
		{2, time.Date(2026, 10, 24, 23, 0, 0, 0, time.UTC)},
		{3, time.Date(2026, 10, 24, 22, 0, 0, 0, time.UTC)},
	}
	for _, item := range cases {
		from, _ := (Window{Hours: item.hours, From: time.Time{}, To: time.Time{}}).boundsAt(now)
		if !from.Equal(item.want) {
			t.Errorf("hours=%d from=%v want %v", item.hours, from.UTC(), item.want)
		}
	}
}

// 2 小时回拨的时区（Antarctica/Troll：03:00+02 → 01:00+00）里重复小时的
// time.Date 会选更晚的 occurrence；回退上一小时若重新按墙钟字段取整，会落到 t
// 之后，hours>=3 反而漏掉更早的桶。必须按 prev 的实际偏移去掉分秒。
func TestWindowBoundsTwoHourFallBack(t *testing.T) {
	t.Parallel()

	loc, err := time.LoadLocation("Antarctica/Troll")
	if err != nil {
		t.Skipf("tzdata 不可用，跳过 DST 用例: %v", err)
	}

	// 00:17 UTC 是第一次 02:17+02（第二次在 02:17+00）：窗口应覆盖墙钟
	// 00/01/02 三个桶，起点是 00:00+02 = 前一日 22:00 UTC。
	now := time.Date(2026, 10, 25, 0, 17, 0, 0, time.UTC).In(loc)

	from, _ := (Window{Hours: 3, From: time.Time{}, To: time.Time{}}).boundsAt(now)
	if want := time.Date(2026, 10, 24, 22, 0, 0, 0, time.UTC); !from.Equal(want) {
		t.Fatalf("hours=3 from=%v want %v", from.UTC(), want)
	}

	// 02:17 UTC 是第二次 02:17+00（回拨已完成）：上一墙钟小时 01 在 time.Date
	// 里选到更晚的 01:00+00，若据此按墙钟再取整会得到 00:00 UTC（墙钟 02 的
	// 第一次 occurrence），漏掉 00 点桶。起点必须仍是 00:00+02 = 22:00 UTC。
	now = time.Date(2026, 10, 25, 2, 17, 0, 0, time.UTC).In(loc)

	from, _ = (Window{Hours: 3, From: time.Time{}, To: time.Time{}}).boundsAt(now)
	if want := time.Date(2026, 10, 24, 22, 0, 0, 0, time.UTC); !from.Equal(want) {
		t.Fatalf("hours=3（第二次 02 点）from=%v want %v", from.UTC(), want)
	}
}

// 30 分钟拨快的时区（Australia/Lord_Howe：01:30+10:30 → 02:30+11）里
// time.Date(02:00) 会归一化成 02:30；若把它当成上一桶起点，hours=2 只含当前
// 小时桶，01 点桶被整段排除。
func TestWindowBoundsPartialSpringForward(t *testing.T) {
	t.Parallel()

	loc, err := time.LoadLocation("Australia/Lord_Howe")
	if err != nil {
		t.Skipf("tzdata 不可用，跳过 DST 用例: %v", err)
	}

	now := time.Date(2026, 10, 4, 3, 10, 0, 0, loc)

	from, _ := (Window{Hours: 2, From: time.Time{}, To: time.Time{}}).boundsAt(now)
	if want := time.Date(2026, 10, 4, 1, 0, 0, 0, loc); !from.Equal(want) {
		t.Fatalf("hours=2 from=%v want %v（02:00 不存在，上一桶是 01:00）", from, want)
	}
}

// 非整小时偏移时区（如 +05:30）的滚动窗口必须按本地墙钟整点对齐：
// Truncate(time.Hour) 会得到 :30，当前小时的桶被整段排除，短窗口恒为空。
func TestWindowBoundsLocalHourOffset(t *testing.T) {
	t.Parallel()

	loc := time.FixedZone("UTC+5:30", 5*60*60+30*60)
	now := time.Date(2026, 10, 20, 14, 47, 0, 0, loc)

	from, _ := (Window{Hours: 1, From: time.Time{}, To: time.Time{}}).boundsAt(now)

	want := time.Date(2026, 10, 20, 14, 0, 0, 0, loc)
	if !from.Equal(want) {
		t.Fatalf("from=%v want %v", from, want)
	}
	// 不能再用 inWindow 断言「当前小时桶在窗口内」：桶 scope 由 bucketTime 按
	// time.Local 解析，而这里注入的是 FixedZone，宿主机时区不同会得到相反结论
	// （CI 的 UTC 通过、Asia/Shanghai 失败）。生产路径 now.Location() 恒等于
	// time.Local，bounds 对齐本地整点即已覆盖该语义。
	if from.Minute() != 0 || from.Second() != 0 {
		t.Fatalf("窗口起点未对齐本地整点: %v", from)
	}
}

// 脏 scope（解析失败）不进任何口径，也不会让整次快照失败。
func TestBucketTimeRejectsGarbage(t *testing.T) {
	t.Parallel()

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
	t.Parallel()
	path := filepath.Join(t.TempDir(), "usage.json")
	r := New(path)
	r.Start()
	r.Add(time.Now(), "cn", "u", "m", &Delta{PromptTokens: 9, HasPromptTokens: true}, true)
	r.Stop()

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Stop 后应有落盘文件: %v", err)
	}
}

// TestCacheHitRateAccumulation（issue #92）：桶累计 hit/miss，credit_by_model
// 与 totals 输出命中率；无缓存观测的桶不参与（rate 零值省略）。
func TestCacheHitRateAccumulation(t *testing.T) {
	t.Parallel()
	r := New(t.TempDir() + "/usage.json")
	base := time.Date(2026, 10, 1, 10, 0, 0, 0, time.Local)
	r.Add(base, "cn", "u1", "glm-5.3", &Delta{
		PromptTokens: 1000, HasPromptTokens: true,
		Credit: 1, HasCredit: true, TotalTokens: 1005, HasTotal: true,
		HasCacheTokens: true, CacheHitTokens: 900, CacheMissTokens: 100,
	}, true)
	r.Add(base.Add(time.Minute), "cn", "u1", "glm-5.3", &Delta{
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
	r2.Add(base, "cn", "u1", "m", &Delta{PromptTokens: 10, HasPromptTokens: true, Credit: 1, HasCredit: true, TotalTokens: 12, HasTotal: true}, true)

	if got := r2.Snapshot(0, nil).CreditByModel[0].CacheHitRate; got != 0 {
		t.Fatalf("无缓存观测应 rate=0, got %v", got)
	}
}

// TestCacheTokensPersistRoundtrip：ch/cm 落盘 v4 并无损恢复。
func TestCacheTokensPersistRoundtrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	r := New(dir + "/usage.json")
	base := time.Date(2026, 10, 1, 10, 0, 0, 0, time.Local)
	r.Add(base, "cn", "u1", "m", &Delta{
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
	t.Parallel()

	r := New("")
	now := time.Now()
	// 同一模型两个账号/两个域/两个时间片，应合并为一行。
	r.Add(now, "cn", "uid1", "glm-5.2", &Delta{PromptTokens: 100, HasPromptTokens: true, CompletionTokens: 50, HasCompletion: true}, true)
	r.Add(now.Add(-2*time.Hour), "global", "uid2", "glm-5.2", &Delta{PromptTokens: 10, HasPromptTokens: true}, false)
	r.Add(now, "cn", "uid1", "claude-4.6", &Delta{TotalTokens: 7, HasTotal: true}, true)

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
	t.Parallel()

	r := New("")
	now := time.Now()
	r.Add(now, "cn", "uid1", "glm-5.2", &Delta{TTFTMs: 300, HasTTFT: true, LatencyMs: 2000, HasLatency: true}, true)
	r.Add(now, "cn", "uid1", "glm-5.2", &Delta{TTFTMs: 500, HasTTFT: true, LatencyMs: 2500, HasLatency: true}, true)
	// 非流式：无 TTFT 观测，不参与均值。
	r.Add(now, "cn", "uid1", "glm-5.2", &Delta{LatencyMs: 100, HasLatency: true}, true)

	s := r.Snapshot(24, nil)
	wantEq(t, "avg_ttft_ms", s.Totals.AvgTTFTMs, 400.0)
	wantEq(t, "avg_latency_ms", s.Totals.AvgLatencyMs, (2000.0+2500.0+100.0)/3)
}

// TestBackfillOverridesLocalDay 上游回填覆盖同 (天, realm, uid, model) 的请求数/积分，
// 不与本地实时记录重复计数；token 等上游不提供的维度保留本地观测（issue #31）。
func TestBackfillOverridesLocalDay(t *testing.T) {
	t.Parallel()

	r := New("")
	now := time.Now()
	day := now.Format("2006-01-02")
	r.Add(now, "cn", "uid1", "glm-5.2", &Delta{TotalTokens: 10, HasTotal: true, Credit: 1, HasCredit: true}, true)

	n := r.Backfill([]BackfillRow{{Day: day, Realm: "cn", UID: "uid1", Model: "glm-5.2", Requests: 5, Credits: 9}})
	if n != 1 {
		t.Fatalf("backfill buckets=%d want 1", n)
	}

	s := r.Snapshot(0, nil)
	wantEq(t, "requests(覆盖后)", s.Totals.Requests, int64(5))
	wantEq(t, "credits(覆盖后)", s.Totals.Credits, 9.0)
	// token 无法从上游逐笔接口恢复：覆盖请求数/积分后仍保留本地值（10），不被清零。
	wantEq(t, "total_tokens(保留本地)", s.Totals.TotalTokens, int64(10))
}

// TestBackfillThenRealtimeNoDoubleCount 回填后同日的实时记录与 Rollup 折叠都不与
// 回填值重复计数（issue #31 复审）：回填删除当日本地桶，故实时记录落在新小时桶，
// 与 Synced 日桶相加即正确总量；小时桶折叠进 Synced 日桶同样不叠加。
func TestBackfillThenRealtimeNoDoubleCount(t *testing.T) {
	t.Parallel()

	r := New("")
	old := time.Now().Add(-100 * 24 * time.Hour) // 早于 hourlyKeep，可被 Rollup 折叠
	day := old.Format("2006-01-02")
	r.Backfill([]BackfillRow{{Day: day, Realm: "cn", UID: "u1", Model: "m", Requests: 5, Credits: 9}})
	r.Add(old, "cn", "u1", "m", &Delta{TotalTokens: 20, HasTotal: true, Credit: 2, HasCredit: true}, true)
	r.Rollup(time.Now())
	s := r.Snapshot(0, nil)
	wantEq(t, "requests(回填+实时)", s.Totals.Requests, int64(6))
	wantEq(t, "credits(回填+实时)", s.Totals.Credits, 11.0)
	wantEq(t, "total_tokens(回填+实时)", s.Totals.TotalTokens, int64(20))
}

// wantEq 断言 got == want，失败时用 label 指明字段。统一入口避免逐项 if 抬高
// 测试函数复杂度，断言条件与原逐项检查等价。
func wantEq[T comparable](t *testing.T, label string, got, want T) {
	t.Helper()

	if got != want {
		t.Fatalf("%s = %v, want %v", label, got, want)
	}
}

func TestTokensByAccountModel(t *testing.T) {
	t.Parallel()

	r := New("")
	now := time.Now()
	r.Add(now, "cn", "u1", "glm-5.2", &Delta{TotalTokens: 10, HasTotal: true}, true)
	r.Add(now, "cn", "u2", "glm-5.2", &Delta{TotalTokens: 20, HasTotal: true}, true)

	got := r.TokensByAccountModel(now.Add(-time.Hour))
	if got["u1"]["glm-5.2"] != 10 || got["u2"]["glm-5.2"] != 20 {
		t.Fatalf("tokens=%v", got)
	}

	if since := r.TokensSince("cn", "u1", "glm-5.2", now.Add(-time.Hour)); since != 10 {
		t.Fatalf("since=%d want 10", since)
	}

	if all := r.TokensSince("cn", "u1", "glm-5.2", now.Add(time.Hour)); all != 0 {
		t.Fatalf("future since=%d want 0", all)
	}
}

func TestDailyTotals(t *testing.T) {
	t.Parallel()

	r := New("")
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.Local)
	r.Add(now, "cn", "u1", "glm-5.2", &Delta{TotalTokens: 100, HasTotal: true, Credit: 1.5, HasCredit: true}, true)
	r.Add(now, "cn", "u1", "hy4", &Delta{TotalTokens: 50, HasTotal: true}, true)
	r.Add(now, "global", "u2", "gpt-5", &Delta{TotalTokens: 70, HasTotal: true}, true)
	// 昨天的桶不计入今天。
	r.Add(now.AddDate(0, 0, -1), "cn", "u1", "glm-5.2", &Delta{TotalTokens: 999, HasTotal: true}, true)

	got := r.DailyTotals(now)
	if got["u1"].Tokens != 150 || got["u2"].Tokens != 70 {
		t.Fatalf("daily totals=%+v", got)
	}

	if got["u1"].Credits != 1.5 {
		t.Errorf("credits=%v want 1.5", got["u1"].Credits)
	}

	if got["u1"].Models["glm-5.2"] != 100 || got["u1"].Models["hy4"] != 50 {
		t.Errorf("model totals=%v", got["u1"].Models)
	}
	// 缓存命中：同一时刻直接复用。
	if cached := r.DailyTotalsCached(now); cached["u1"].Tokens != 150 {
		t.Errorf("cached totals=%+v", cached)
	}
}

// 落盘失败后必须保持 dirty，下一轮 tick 才会重试。
func TestFlushFailureKeepsDirty(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	r := New("")
	r.Add(time.Now(), "cn", "u1", "m", &Delta{PromptTokens: 1, HasPromptTokens: true}, true)
	r.path = dir // rename 目标为目录 → 原子替换失败

	r.flush(false)

	r.mu.Lock()
	dirty := r.dirty
	r.mu.Unlock()

	if !dirty {
		t.Fatal("flush 失败后 dirty=false，后续 tick 不会重试")
	}
}

// TestStopWithoutStartDoesNotBlock 未启动后台循环时 Stop 只做一次落盘，不等待 done。
func TestStopWithoutStartDoesNotBlock(t *testing.T) {
	t.Parallel()

	r := New("")
	done := make(chan struct{})

	go func() {
		r.Stop()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("未 Start 时 Stop 不应阻塞")
	}
}

// TestStopWithoutStartFlushes 未启动后台循环时 Stop 也应落盘一次。
func TestStopWithoutStartFlushes(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "usage.json")
	r := New(path)
	r.Add(time.Now(), "cn", "u1", "m", &Delta{PromptTokens: 1, HasPromptTokens: true}, true)

	r.Stop()

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Stop 未落盘: %v", err)
	}
}

// TestStartIdempotent 重复 Start 不得启动两个循环（否则 Stop 会重复 close(done)）。
func TestStartIdempotent(t *testing.T) {
	t.Parallel()

	r := New("")
	r.Start()
	r.Start()
	r.Stop()
}
