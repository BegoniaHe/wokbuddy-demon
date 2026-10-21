// cache_stats_test.go 钉住缓存命中率观测链路（issue #92）：SSE 末帧的
// prompt_cache_hit/miss_tokens 采集，以及「曾命中却整段未命中」的 WARN 信号判据。
package server

import (
	"strings"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

func feedStatsLines(s *chatStatsReader, lines ...string) {
	for _, l := range lines {
		s.parseSSELine(l)
	}
}

// 流式末帧带 hit/miss：CacheTokens 原样返回；只带 hit 时 miss 按 prompt-hit 推导；
// 上游完全没回缓存维度时 ok=false（不参与统计）。
func TestStatsReaderCacheTokens(t *testing.T) {
	t.Parallel()

	s := newChatStatsReaderSince(strings.NewReader(""), time.Now())
	feedStatsLines(s,
		`data: {"usage":{"prompt_tokens":1000,"completion_tokens":5,"total_tokens":1005,"credit":0.1,"prompt_cache_hit_tokens":900,"prompt_cache_miss_tokens":100}}`,
	)

	if hit, miss, ok := s.CacheTokens(); !ok || hit != 900 || miss != 100 {
		t.Fatalf("hit=%d miss=%d ok=%v, want 900/100/true", hit, miss, ok)
	}

	s2 := newChatStatsReaderSince(strings.NewReader(""), time.Now())
	feedStatsLines(s2,
		`data: {"usage":{"prompt_tokens":8500,"prompt_cache_hit_tokens":8400}}`,
	)

	if hit, miss, ok := s2.CacheTokens(); !ok || hit != 8400 || miss != 100 {
		t.Fatalf("derived miss: hit=%d miss=%d ok=%v, want 8400/100/true", hit, miss, ok)
	}

	s3 := newChatStatsReaderSince(strings.NewReader(""), time.Now())
	feedStatsLines(s3,
		`data: {"usage":{"prompt_tokens":100,"completion_tokens":5}}`,
	)

	if _, _, ok := s3.CacheTokens(); ok {
		t.Fatal("无缓存字段应 ok=false")
	}
}

// WARN 信号状态机：先命中、再大前缀整段未命中 → 触发一次；冷却内重复未命中
// 不再触发；从未命中的模型（首请求天然全 miss）与小前缀不触发。
func TestCacheMissSignal(t *testing.T) {
	t.Parallel()

	old := cacheMissWarnEvery
	cacheMissWarnEvery = time.Hour // 拉长冷却，验证「冷却内不重复」

	t.Cleanup(func() {
		cacheMissWarnEvery = old
		cacheMissWarn = &cacheMissSignal{everHit: map[string]bool{}, last: map[string]time.Time{}}
	})

	cacheMissWarn = &cacheMissSignal{everHit: map[string]bool{}, last: map[string]time.Time{}}

	cacheMissWarn.noteCacheTokens("glm-5.3", 8500, 8000, 500) // 命中 → 标记可命中

	if cacheMissWarn.last["glm-5.3"] != (time.Time{}) {
		t.Fatal("命中不应触发 WARN")
	}

	cacheMissWarn.noteCacheTokens("glm-5.3", 8500, 0, 8500) // 整段未命中 → 触发

	if cacheMissWarn.last["glm-5.3"].IsZero() {
		t.Fatal("曾命中的模型整段未命中应触发 WARN 记录")
	}

	first := cacheMissWarn.last["glm-5.3"]
	cacheMissWarn.noteCacheTokens("glm-5.3", 8500, 0, 8500) // 冷却内 → 不重复

	if !cacheMissWarn.last["glm-5.3"].Equal(first) {
		t.Fatal("冷却期内不应重复触发")
	}

	// 从未命中的模型（新模型首请求天然全 miss）不触发。
	cacheMissWarn.noteCacheTokens("new-model", 5000, 0, 5000)

	if !cacheMissWarn.last["new-model"].IsZero() {
		t.Fatal("从未命中的模型不应触发")
	}
	// 小前缀不触发。
	cacheMissWarn.noteCacheTokens("glm-5.3", 500, 0, 500)
	// miss 缺失（上游只回 hit=0、无 miss、无 prompt）不触发。
	cacheMissWarn.noteCacheTokens("glm-5.3", 0, 0, 0)
}

// TestCacheTokensSymmetricHitDerivation 缓存命中的**对称反推**（上游同步窗口 issue #186）：
// 上游「接近全命中」时可能只回显式 miss、把 hit 置 0 或留空。单向推导会把这种形态
// 判成缓存完全失效，刷出假阳性 WARN 并污染面板缓存指标。
func TestCacheTokensSymmetricHitDerivation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		line string
		hit  int64
		miss int64
		ok   bool
	}{
		{
			name: "hit=0+miss=3+prompt=50000 反推 hit",
			line: `data: {"usage":{"prompt_tokens":50000,"prompt_cache_hit_tokens":0,"prompt_cache_miss_tokens":3}}`,
			hit:  49997, miss: 3, ok: true,
		},
		{
			name: "hit 缺失 + miss 显式",
			line: `data: {"usage":{"prompt_tokens":50000,"prompt_cache_miss_tokens":3}}`,
			hit:  49997, miss: 3, ok: true,
		},
		{
			name: "prompt<=miss 不自造命中",
			line: `data: {"usage":{"prompt_tokens":3,"prompt_cache_miss_tokens":3}}`,
			hit:  0, miss: 3, ok: true,
		},
		{
			name: "真整段未命中保持 hit=0",
			line: `data: {"usage":{"prompt_tokens":50000,"prompt_cache_hit_tokens":0,"prompt_cache_miss_tokens":50000}}`,
			hit:  0, miss: 50000, ok: true,
		},
		{
			name: "两侧都缺 → 不可得",
			line: `data: {"usage":{"prompt_tokens":50000}}`,
			hit:  0, miss: 0, ok: false,
		},
		{
			// 反推只接受「正的显式 miss」：两侧都显式 0 时没有任何命中证据，
			// 反推会凭空造出 100% 命中（命中率统计被污染成反向假阳性）。
			name: "两侧显式 0 不反推命中",
			line: `data: {"usage":{"prompt_tokens":50000,"prompt_cache_hit_tokens":0,"prompt_cache_miss_tokens":0}}`,
			hit:  0, miss: 0, ok: true,
		},
		{
			name: "hit 缺失且无 miss（只有 prompt）不反推命中",
			line: `data: {"usage":{"prompt_tokens":50000,"prompt_cache_hit_tokens":0}}`,
			hit:  0, miss: 50000, ok: true,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			s := newChatStatsReaderSince(strings.NewReader(""), time.Now())
			feedStatsLines(s, testCase.line)

			hit, miss, ok := s.CacheTokens()
			if ok != testCase.ok || hit != testCase.hit || miss != testCase.miss {
				t.Fatalf("hit=%d miss=%d ok=%v, want %d/%d/%v", hit, miss, ok, testCase.hit, testCase.miss, testCase.ok)
			}
		})
	}
}

// TestCacheTokensFromUsageMap 非流式路径与流式同口径：聚合响应 usage 走同一套
// 对称推导（修复前只取 hit 侧，miss 显式存在时被丢弃）。
func TestCacheTokensFromUsageMap(t *testing.T) {
	t.Parallel()

	usage := map[string]any{"prompt_tokens": float64(50000), "prompt_cache_miss_tokens": float64(3)}
	hit, hasHit := upstream.UsageCacheHitTokens(usage)
	miss, hasMiss := upstream.UsageCacheMissTokens(usage)
	prompt, hasPrompt := upstream.UsagePromptTokens(usage)

	hitInt, missInt, ok := cacheTokensFromUsage(cacheTokenTriple{
		prompt:    int64(prompt),
		hit:       int64(hit),
		miss:      int64(miss),
		hasPrompt: hasPrompt,
		hasHit:    hasHit,
		hasMiss:   hasMiss,
	})
	if !ok || hitInt != 49997 || missInt != 3 {
		t.Fatalf("map 反推 hit=%d miss=%d ok=%v, want 49997/3/true", hitInt, missInt, ok)
	}
}

// TestCacheMissWarnSkipsExplicitZero 两侧显式 0（hit=0/miss=0）没有未命中证据：
// 即使该模型此前命中过也不能触发「整段重算」WARN（对称反推的配套防假阳性）。
func TestCacheMissWarnSkipsExplicitZero(t *testing.T) {
	t.Parallel()

	const model = "explicit-zero-cache-model"

	cacheMissWarn.noteCacheTokens(model, 5000, 100, 0) // 先建立「可命中」基线
	cacheMissWarn.noteCacheTokens(model, 5000, 0, 0)   // 两侧显式 0：不应告警

	cacheMissWarn.mu.Lock()
	_, warned := cacheMissWarn.last[model]
	cacheMissWarn.mu.Unlock()

	if warned {
		t.Fatal("两侧显式 0 不应触发缓存未命中 WARN")
	}
}
