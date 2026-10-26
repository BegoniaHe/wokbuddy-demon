package apikey

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newLimited(t *testing.T, lim Limits) (*Store, string) {
	t.Helper()

	store := New("")

	rec, _, err := store.Create("k")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if ok, err := store.UpdateLimits(rec.ID, lim); err != nil || !ok {
		t.Fatalf("update limits: %v %v", ok, err)
	}

	return store, rec.ID
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	return raw
}

func TestAcquireRPM(t *testing.T) {
	t.Parallel()

	store, id := newLimited(t, Limits{RPM: 2})
	for i := range 2 {
		rel, _, reason := store.Acquire(id, "")
		if reason != RejectNone {
			t.Fatalf("request %d 应放行: %v", i, reason)
		}

		rel()
	}

	rel, retry, reason := store.Acquire(id, "")
	if reason != RejectRateLimited || rel != nil {
		t.Fatalf("超过 RPM 应拒绝: reason=%v rel=%v", reason, rel != nil)
	}

	if retry <= 0 || retry > time.Minute {
		t.Fatalf("retryAfter=%v", retry)
	}
}

func TestAcquireConcurrency(t *testing.T) {
	t.Parallel()
	store, id := newLimited(t, Limits{MaxConcurrent: 1})

	rel, _, reason := store.Acquire(id, "")
	if reason != RejectNone {
		t.Fatalf("首个请求应放行: %v", reason)
	}

	if _, _, reason := store.Acquire(id, ""); reason != RejectConcurrency {
		t.Fatalf("并发超限应拒绝: %v", reason)
	}

	rel()

	if rel2, _, reason := store.Acquire(id, ""); reason != RejectNone {
		t.Fatalf("释放后应放行: %v", reason)
	} else {
		rel2()
	}
}

func TestAcquireTPM(t *testing.T) {
	t.Parallel()
	store, id := newLimited(t, Limits{TPM: 100})
	store.RecordUsage(id, 0, 60, 60)

	if _, _, reason := store.Acquire(id, ""); reason != RejectNone {
		t.Fatalf("未达 TPM 应放行: %v", reason)
	}

	store.RecordUsage(id, 0, 50, 50)

	if _, _, reason := store.Acquire(id, ""); reason != RejectRateLimited {
		t.Fatalf("超过 TPM 应拒绝: %v", reason)
	}
}

func TestModelAllowed(t *testing.T) {
	t.Parallel()
	store, id := newLimited(t, Limits{Models: []string{"cn:glm-*", "gpt-5*"}})

	cases := []struct {
		model string
		want  bool
	}{
		{"cn:glm-5.2", true},
		{"glm-5.2", true}, // 无前缀按 cn
		{"global:glm-5.2", false},
		{"gpt-5-codex", true},
		{"global:gpt-5-codex", true}, // 无 realm 模式匹配任意域
		{"claude-x", false},
	}
	for _, testCase := range cases {
		if got := store.ModelAllowed(id, testCase.model); got != testCase.want {
			t.Errorf("ModelAllowed(%q)=%v want %v", testCase.model, got, testCase.want)
		}
	}
}

func TestUnlimitedKeyAllowsEverything(t *testing.T) {
	t.Parallel()

	store := New("")

	rec, _, _ := store.Create("k")
	for range 100 {
		rel, _, reason := store.Acquire(rec.ID, "")
		if reason != RejectNone {
			t.Fatalf("未配置限制应始终放行: %v", reason)
		}

		rel()
	}

	if !store.ModelAllowed(rec.ID, "any:model") {
		t.Fatal("未配置白名单应允许所有模型")
	}
}

// TestAcquireExpiry now >= expires_at 拒绝；未到期放行。只配置 expires_at 时
// 也必须进入新维度检查（不能走旧 enabled 快速路径）。
func TestAcquireExpiry(t *testing.T) {
	t.Parallel()

	past := time.Now().Add(-time.Minute)
	store, id := newLimited(t, Limits{ExpiresAt: &past})

	if _, _, reason := store.Acquire(id, ""); reason != RejectExpired {
		t.Fatalf("过期应拒绝: %v", reason)
	}

	future := time.Now().Add(time.Hour)

	store2, id2 := newLimited(t, Limits{ExpiresAt: &future})

	if _, _, reason := store2.Acquire(id2, ""); reason != RejectNone {
		t.Fatalf("未到期应放行: %v", reason)
	}
}

// TestAcquireQuota 配额 0 = 不限；total >= quota 即耗尽。
func TestAcquireQuota(t *testing.T) {
	t.Parallel()

	unlimited, uid := newLimited(t, Limits{QuotaTokens: 0})
	unlimited.RecordUsage(uid, 0, 1_000_000, 1_000_000)

	if _, _, reason := unlimited.Acquire(uid, ""); reason != RejectNone {
		t.Fatalf("配额 0 应不限: %v", reason)
	}

	store, id := newLimited(t, Limits{QuotaTokens: 100})
	store.RecordUsage(id, 0, 99, 99)

	if _, _, reason := store.Acquire(id, ""); reason != RejectNone {
		t.Fatalf("未达配额应放行: %v", reason)
	}

	store.RecordUsage(id, 0, 1, 1) // total = 100，恰好等于配额

	if _, _, reason := store.Acquire(id, ""); reason != RejectQuotaExhausted {
		t.Fatalf("恰好等于配额应耗尽: %v", reason)
	}

	store.RecordUsage(id, 0, 10, 10) // total = 110，超额

	if _, _, reason := store.Acquire(id, ""); reason != RejectQuotaExhausted {
		t.Fatalf("超额应拒绝: %v", reason)
	}
}

// TestAcquireIPAllowlist IPv4 / IPv6 / CIDR / 未命中 / IPv4-mapped 归一。
func TestAcquireIPAllowlist(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		allow   []string
		ip      string
		allowed bool
	}{
		{"IPv4 命中", []string{"192.0.2.10"}, "192.0.2.10", true},
		{"IPv4 未命中", []string{"192.0.2.10"}, "192.0.2.11", false},
		{"IPv6 命中", []string{"2001:db8::1"}, "2001:db8::1", true},
		{"IPv6 未命中", []string{"2001:db8::1"}, "2001:db8::2", false},
		{"IPv4 CIDR 命中", []string{"198.51.100.0/24"}, "198.51.100.77", true},
		{"IPv4 CIDR 未命中", []string{"198.51.100.0/24"}, "198.51.101.1", false},
		{"IPv6 CIDR 命中", []string{"2001:db8:1::/48"}, "2001:db8:1::5", true},
		{"IPv4-mapped 白名单命中 IPv4", []string{"::ffff:192.0.2.10"}, "192.0.2.10", true},
		{"IPv4-mapped 来源命中 IPv4 白名单", []string{"192.0.2.10"}, "::ffff:192.0.2.10", true},
		{"非法来源拒绝", []string{"192.0.2.10"}, "not-an-ip", false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			store, id := newLimited(t, Limits{IPAllowlist: testCase.allow})

			_, _, reason := store.Acquire(id, testCase.ip)
			if testCase.allowed && reason != RejectNone {
				t.Fatalf("应放行: %v", reason)
			}

			if !testCase.allowed && reason != RejectIPNotAllowed {
				t.Fatalf("应拒绝: %v", reason)
			}
		})
	}
}

func TestNormalizeLimitsIP(t *testing.T) {
	t.Parallel()

	got, err := NormalizeLimits(Limits{IPAllowlist: []string{
		" 192.0.2.1 ", "192.0.2.1", "::ffff:192.0.2.2",
		"2001:db8:1::5/48", "198.51.100.1/24",
	}})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}

	want := []string{"192.0.2.1", "192.0.2.2", "2001:db8:1::/48", "198.51.100.0/24"}
	if len(got.IPAllowlist) != len(want) {
		t.Fatalf("ips=%v want %v", got.IPAllowlist, want)
	}

	for i := range want {
		if got.IPAllowlist[i] != want[i] {
			t.Fatalf("ips=%v want %v", got.IPAllowlist, want)
		}
	}
}

func TestNormalizeLimitsExpiresUTC(t *testing.T) {
	t.Parallel()

	loc := time.FixedZone("UTC+8", 8*3600)
	when := time.Date(2026, 1, 2, 3, 4, 5, 0, loc)

	got, err := NormalizeLimits(Limits{ExpiresAt: &when})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}

	if got.ExpiresAt == nil || got.ExpiresAt.Location() != time.UTC {
		t.Fatalf("expires_at 未转 UTC: %v", got.ExpiresAt)
	}

	if !got.ExpiresAt.Equal(when) {
		t.Fatalf("时间值改变: %v != %v", got.ExpiresAt, when)
	}
}

// TestInvalidLimitsNeverPersist 非法配额/IP/模型 glob 拒绝保存，磁盘保持原值。
func TestInvalidLimitsNeverPersist(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "keys.json")
	store := New(path)

	rec, _, err := store.Create("k", Limits{RPM: 1})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	before := readFile(t, path)

	cases := []Limits{
		{QuotaTokens: -1},
		{RPM: -1},
		{IPAllowlist: []string{"not-an-ip"}},
		{IPAllowlist: []string{"10.0.0.0/99"}},
		{Models: []string{"cn:"}},
		{Models: []string{"["}},
	}
	for _, lim := range cases {
		ok, err := store.UpdateLimits(rec.ID, lim)
		if ok || !errors.Is(err, ErrInvalidLimits) {
			t.Fatalf("limits=%+v ok=%v err=%v，应返回 ErrInvalidLimits", lim, ok, err)
		}
	}

	after := readFile(t, path)
	if string(before) != string(after) {
		t.Fatalf("非法限制写入了磁盘:\nbefore=%s\nafter=%s", before, after)
	}

	if _, _, err := store.Create("bad", Limits{QuotaTokens: -1}); !errors.Is(err, ErrInvalidLimits) {
		t.Fatalf("Create 非法限制应拒绝: %v", err)
	}
}

// TestLimitsPersistAcrossRestart 限制落盘后重启仍生效。
func TestLimitsPersistAcrossRestart(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "keys.json")
	future := time.Now().Add(time.Hour)
	store := New(path)

	rec, raw, err := store.Create("k", Limits{
		QuotaTokens: 10,
		IPAllowlist: []string{"192.0.2.1"},
		ExpiresAt:   &future,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	store.Flush()

	reloaded := New(path)
	if _, _, reason := reloaded.Acquire(rec.ID, "192.0.2.9"); reason != RejectIPNotAllowed {
		t.Fatalf("重启后 IP 白名单未生效: %v", reason)
	}

	if _, _, reason := reloaded.Acquire(rec.ID, "192.0.2.1"); reason != RejectNone {
		t.Fatalf("重启后合法 IP 应放行: %v", reason)
	}

	if got := reloaded.List(); len(got) != 1 || got[0].Limits.QuotaTokens != 10 || len(got[0].Limits.IPAllowlist) != 1 {
		t.Fatalf("重启后限制未恢复: %+v", got)
	}

	reloaded.RecordUsage(rec.ID, 0, 10, 10)

	if _, _, reason := reloaded.Acquire(rec.ID, "192.0.2.1"); reason != RejectQuotaExhausted {
		t.Fatalf("重启后配额未生效: %v", reason)
	}

	// 明文仍能鉴权（限制不改变哈希）。
	if got := reloaded.Keys(); len(got) != 1 || got[0].ID != rec.ID {
		t.Fatalf("reloaded keys=%+v raw=%q", got, raw)
	}
}
