// round16_regression_test.go 第 16 轮审查修复点的回归测试：链路头、轻头族、
// global 目录作用域与容错、models.dev 空文档、model.json 保鲜期、缓存 token 口径。
package upstream

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// TestTraceHeadersPad16HexTraceparent 入站透传 16 hex trace id 时，traceparent 的
// trace-id 必须补足 32 hex（W3C 要求），否则上游丢弃该头、链路断在半路。
func TestTraceHeadersPad16HexTraceparent(t *testing.T) {
	t.Parallel()

	req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/v2/chat/completions", http.NoBody)
	injectTraceHeaders(req, "0123456789abcdef", strings.Repeat("ab", 16))

	// X-Trace-Id / B3 保持原值（B3 允许 16 hex，且要与客户端入站值同值）。
	if got := req.Header.Get("X-Trace-Id"); got != "0123456789abcdef" {
		t.Errorf("X-Trace-Id=%q want 原值", got)
	}

	if got := req.Header.Get("X-B3-Traceid"); got != "0123456789abcdef" {
		t.Errorf("X-B3-Traceid=%q want 原值", got)
	}

	traceparent := req.Header.Get("Traceparent")
	// 00-<32hex>-<16hex>-01
	pattern := regexp.MustCompile(`^00-[0-9a-f]{32}-[0-9a-f]{16}-01$`)
	if !pattern.MatchString(traceparent) {
		t.Fatalf("Traceparent=%q want 32 hex trace-id", traceparent)
	}

	if !strings.HasPrefix(traceparent, "00-00000000000000000123456789abcdef-") {
		t.Errorf("Traceparent=%q want 16 hex 左补零（B3 的 64 位等价形式）", traceparent)
	}
}

// TestBaggageValueRejectsUnsafeID 含分隔符的会话 id 会打断 W3C baggage 结构，
// 此时整头不发，而不是发一条解析即错的 baggage。
func TestBaggageValueRejectsUnsafeID(t *testing.T) {
	t.Parallel()

	if got := baggageValue("a,b", "0123456789abcdef0123456789abcdef"); got != "" {
		t.Fatalf("baggageValue=%q want 空串（逗号会打断成员）", got)
	}

	valid := "0123456789ab-cdef-0123456789abcdef"
	if got := baggageValue(valid, valid); got == "" {
		t.Fatal("合规 UUID 形态本应生成 baggage")
	}

	req, _ := http.NewRequest(http.MethodPost, "https://example.invalid", http.NoBody)
	req.Header.Set("X-Conversation-Id", "a;b")
	req.Header.Set("X-Conversation-Request-Id", valid)
	injectChatBaggage(req)

	if got := req.Header.Get("Baggage"); got != "" {
		t.Fatalf("Baggage=%q want 不发（id 不合规）", got)
	}
}

// TestLogoutHeadersCarryUserAgentAndNoEmptyBearer logout 属 plugin 轻头族：UA 不能
// 缺失（否则落回 Go-http-client/1.1），且空 access token 不写 "Bearer "。
func TestLogoutHeadersCarryUserAgentAndNoEmptyBearer(t *testing.T) {
	t.Parallel()

	// 生产装配里 ClientVersion/profile 恒存在（启动期校验），此处显式给出以覆盖
	// userAgent 的三段式分支。
	client := New()
	client.ClientVersion = "5.9.4"
	client.CliVersion = "5.9.4"

	acct := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", Domain: "www.workbuddy.ai"}

	req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/v2/plugin/auth/logout", http.NoBody)
	client.LogoutHeaders(req, acct)

	if got := req.Header.Get("User-Agent"); got == "" {
		t.Error("logout UA 为空：请求会带 Go-http-client/1.1 默认 UA")
	}

	// logout 只认 refreshToken 三件套，Authorization 必须被删掉。
	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization=%q want 空（logout 走 X-No-Authorization）", got)
	}

	empty := &auth.Auth{UID: "u2", RefreshToken: "rt", Domain: "www.workbuddy.ai"}

	req2, _ := http.NewRequest(http.MethodPost, "https://example.invalid/v2/billing/meter/summary", http.NoBody)
	client.BillingHeaders(req2, empty)

	if got, ok := req2.Header["Authorization"]; ok {
		t.Errorf("Authorization=%v want 该头整条不出现（空凭证）", got)
	}
}

// TestParseGlobalModelEntriesToleratesBadEntry 单条坏条目不能拖垮整份 global 目录：
// 原先整数组一次解码失败就降级宽松解析，丢掉 tags/模态，菜单整体变空。
func TestParseGlobalModelEntriesToleratesBadEntry(t *testing.T) {
	t.Parallel()

	arr := json.RawMessage(`[
		{"id":"good-model","maxInputTokens":128000,"tags":["badge:paid"]},
		{"id":"bad-model","maxInputTokens":"not-a-number"},
		{"id":"another-good","maxOutputTokens":4096}
	]`)

	names, infos, _, _, ok := parseGlobalModelEntries(arr)
	if !ok {
		t.Fatal("含一条坏条目不应让整份目录解析失败")
	}

	want := []string{"good-model", "bad-model", "another-good"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("names=%v want %v（坏条目按宽松解析保住模型名）", names, want)
	}

	if len(infos) != len(want) {
		t.Fatalf("infos=%d want %d", len(infos), len(want))
	}

	// 好条目必须保留 typed 解析的字段（tags 是 ChatVisible 判定的依据）。
	if len(infos[0].Tags) == 0 {
		t.Error("good-model 丢失 tags：整份目录被降级成宽松解析了")
	}
}

// TestGlobalModelsCacheScopedByHost 目录缓存按实际出站主机分桶：同 realm 的两套
// 国际主机不能互用目录（否则 B 域会选到本地不存在的模型）。
func TestGlobalModelsCacheScopedByHost(t *testing.T) {
	t.Parallel()

	client := New()
	client.GlobalEnabled = true

	official := &auth.Auth{UID: "g1", AccessToken: "at", Domain: "www.codebuddy.ai"}
	legacy := &auth.Auth{UID: "g2", AccessToken: "at", Domain: "www.workbuddy.ai"}

	for _, account := range []*auth.Auth{official, legacy} {
		if _, err := auth.BackfillRealmFor(account, "global"); err != nil {
			t.Fatalf("backfill realm: %v", err)
		}
	}

	officialScope := client.modelScope(official)
	legacyScope := client.modelScope(legacy)

	if officialScope == legacyScope {
		t.Fatalf("两套国际主机作用域相同（%s）：目录会互用", officialScope)
	}

	client.storeGlobalModels(officialScope, []string{"official-only"}, nil)

	if names, _, hit := client.globalModelsCacheHit(officialScope); !hit || len(names) != 1 {
		t.Fatalf("同主机应命中缓存，got hit=%v names=%v", hit, names)
	}

	if _, _, hit := client.globalModelsCacheHit(legacyScope); hit {
		t.Fatal("另一套主机的目录不应被当成自己的缓存命中")
	}
}

// TestUsageCacheMissTokensIgnoresExplicitZero 上游把 miss 置 0 表示「未统计」，
// 采信 0 会让命中率恒为 100%（hit 侧早已只取正数）。
func TestUsageCacheMissTokensIgnoresExplicitZero(t *testing.T) {
	t.Parallel()

	usage := map[string]any{
		"prompt_tokens":            float64(1000),
		"prompt_cache_hit_tokens":  float64(400),
		"prompt_cache_miss_tokens": float64(0),
	}

	miss, ok := UsageCacheMissTokens(usage)
	if !ok || miss != 600 {
		t.Fatalf("miss=%v ok=%v want 600/true（显式 0 应回落到推导）", miss, ok)
	}

	// 无 prompt 可推导且 miss 显式为 0：返回未知，而不是 0。
	if _, ok := UsageCacheMissTokens(map[string]any{"prompt_cache_miss_tokens": float64(0)}); ok {
		t.Fatal("仅显式 0 时应报未知")
	}
}

// TestModelCatalogStaleModelsDevEntry modelsdev 条目过期即视为未命中，让第 4 级
// 重新拉取；seed/manual 条目不过期。
func TestModelCatalogStaleModelsDevEntry(t *testing.T) {
	t.Parallel()

	old := time.Now().Add(-modelsDevEntryMaxAge - 24*time.Hour).UTC().Format(time.RFC3339)

	tests := []struct {
		name  string
		entry ModelCapEntry
		want  bool
	}{
		{
			name: "过期 modelsdev",
			entry: ModelCapEntry{
				ContextLength: 128000, MaxOutputTokens: 0, FetchedAt: old, Source: "modelsdev",
			},
			want: false,
		},
		{
			name: "新 modelsdev",
			entry: ModelCapEntry{
				ContextLength: 128000, MaxOutputTokens: 0, Source: "modelsdev",
				FetchedAt: time.Now().UTC().Format(time.RFC3339),
			},
			want: true,
		},
		{
			name: "无时间戳 modelsdev",
			entry: ModelCapEntry{
				ContextLength: 128000, MaxOutputTokens: 0, FetchedAt: "", Source: "modelsdev",
			},
			want: false,
		},
		{
			name: "seed 永不过期",
			entry: ModelCapEntry{
				ContextLength: 128000, MaxOutputTokens: 0, FetchedAt: old, Source: "seed",
			},
			want: true,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			catalog := &modelCatalog{}
			catalog.mu.Lock()
			catalog.loaded = true
			catalog.entries = map[string]ModelCapEntry{"m": testCase.entry}
			catalog.mu.Unlock()

			if _, ok := catalog.get("m"); ok != testCase.want {
				t.Fatalf("get ok=%v want %v", ok, testCase.want)
			}
		})
	}
}

// TestModelsDevEmptyDocNotLocked 「200 但空索引」不能写成 doc：否则后续永不重拉
// 且无日志，所有未知模型静默退到 1M。
func TestModelsDevEmptyDocNotLocked(t *testing.T) {
	t.Parallel()

	fetcher := &modelsDevFetcher{}
	if fetcher.publishDoc(map[string]modelsDevEntry{}, nil) {
		t.Fatal("空索引不应发布：ensureDocAsync 会永久短路")
	}

	if fetcher.doc != nil {
		t.Fatal("空索引被写进了 doc")
	}

	doc := map[string]modelsDevEntry{"m": {Context: 128000, Output: 4096}}
	if !fetcher.publishDoc(doc, nil) {
		t.Fatal("非空索引应发布成功")
	}

	if len(fetcher.doc) != 1 {
		t.Fatalf("doc=%v want 1 条", fetcher.doc)
	}
}

// TestGlobalEffortSnapshotMergesScopesGlobalEffortSnapshot 在只有 realm 信息时取
// 各主机桶的并集（展示用），先出现的键权威。
func TestGlobalEffortSnapshotMergesScopes(t *testing.T) {
	t.Parallel()

	client := New()
	client.storeEfforts("global|a.example", map[string][]string{"m1": {"high"}}, map[string]string{"m1": "high"})
	client.storeEfforts("global|b.example", map[string][]string{"m1": {"low"}, "m2": {"low"}}, nil)

	efforts, defaults := client.GlobalEffortSnapshot()
	if got := efforts["m1"]; len(got) != 1 || got[0] != "high" {
		t.Fatalf("m1=%v want 权威桶 [high]", got)
	}

	if got := efforts["m2"]; len(got) != 1 || got[0] != "low" {
		t.Fatalf("m2=%v want 兄弟桶补齐 [low]", got)
	}

	if defaults["m1"] != "high" {
		t.Fatalf("defaults[m1]=%q want high", defaults["m1"])
	}
}

// TestChatBodySnapshotsFallsBackToSiblingScope 本主机尚未探测时回落到任一 global
// 桶（宁可降级也比完全透传安全），CN 作用域不跨桶回落。
func TestChatBodySnapshotsFallsBackToSiblingScope(t *testing.T) {
	t.Parallel()

	client := New()
	client.storeEfforts("global|a.example", map[string][]string{"m1": {"high"}}, nil)

	efforts, _, _ := client.chatBodySnapshots("global|b.example")
	if len(efforts["m1"]) == 0 {
		t.Fatal("global 兄弟桶应回落，否则 global 请求完全不降级")
	}

	if cnEfforts, _, _ := client.chatBodySnapshots("cn"); len(cnEfforts) != 0 {
		t.Fatalf("cn 不应回落到 global 桶: %v", cnEfforts)
	}

	// 精确命中优先于兄弟桶。
	client.storeEfforts("global|b.example", map[string][]string{"m1": {"low"}}, nil)

	efforts, _, _ = client.chatBodySnapshots("global|b.example")
	if got := efforts["m1"]; len(got) != 1 || got[0] != "low" {
		t.Fatalf("m1=%v want 本主机桶 [low]", got)
	}
}

// TestGlobalRegisterStatusRejected 非 2xx 不能被当成业务成功（403/401 的 body 常是
// HTML 或 {}，解出的 code 为 0）。
func TestGlobalRegisterStatusRejected(t *testing.T) {
	t.Parallel()

	client := testClient(func(*http.Request) (*http.Response, error) {
		return jsonResp(http.StatusForbidden, `{}`), nil
	})
	client.GlobalEnabled = true

	acct := &auth.Auth{UID: "g1", AccessToken: "at", Domain: "www.workbuddy.ai"}
	if _, err := auth.BackfillRealmFor(acct, "global"); err != nil {
		t.Fatalf("backfill realm: %v", err)
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"https://example.invalid/console/login/account", http.NoBody)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	if _, _, _, err := client.globalRegisterJSON(acct, req); err == nil {
		t.Fatal("HTTP 403 应报错，不能被当成 code=0 的成功")
	}
}

// TestUsageNumbersAcceptJSONNumber 聚合帧与出站改写都走 UseNumber：数值叶子是
// json.Number，只认 float64 会让缓存命中观测、total_tokens 合成、max_tokens
// 别名迁移全部静默失效。
func TestUsageNumbersAcceptJSONNumber(t *testing.T) {
	t.Parallel()

	if got, ok := positiveUsageNumber(json.Number("123")); !ok || got != 123 {
		t.Fatalf("positiveUsageNumber(json.Number)=%v,%v want 123,true", got, ok)
	}

	if got, ok := num64(json.Number("7")); !ok || got != 7 {
		t.Fatalf("num64(json.Number)=%v,%v want 7,true", got, ok)
	}

	// 整值浮点字面量（Python 默认序列化、科学计数法）按整值接受。
	for _, literal := range []string{"4096.0", "1e5"} {
		if _, ok := positiveInt64(json.Number(literal)); !ok {
			t.Errorf("positiveInt64(json.Number(%q)) 被判非法", literal)
		}
	}
}

// TestEnsureUsageTotalWithJSONNumber 上游省略 total_tokens 时要能合成：帧解码走
// UseNumber 后 num64 必须认 json.Number。
func TestEnsureUsageTotalWithJSONNumber(t *testing.T) {
	t.Parallel()

	usage := map[string]any{
		"prompt_tokens": json.Number("3"), "completion_tokens": json.Number("2"),
	}

	out := ensureUsageTotal(usage)
	if got, ok := jsonInt(out["total_tokens"]); !ok || got != 5 {
		t.Fatalf("total_tokens=%v want 5", out["total_tokens"])
	}
}
