package session

import (
	"fmt"
	"maps"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/redisstore"
)

// extractKey 只取 ExtractSession 的粘性会话键，保持既有用例表达式简洁。
func extractKey(body []byte) string {
	key, _ := ExtractSession(body)

	return key
}

// countingStore 记录镜像调用次数的假 Store（不联网）。
type countingStore struct {
	redisstore.Noop

	mu       sync.Mutex
	setBinds int
	delBinds int
	binds    map[string]string
}

func newCountingStore() *countingStore {
	return &countingStore{binds: map[string]string{}}
}

func (c *countingStore) SetBind(key, uid string, ttl time.Duration) {
	c.mu.Lock()
	c.setBinds++
	c.binds[key] = uid
	c.mu.Unlock()
}

func (c *countingStore) DelBind(key string) {
	c.mu.Lock()
	c.delBinds++
	delete(c.binds, key)
	c.mu.Unlock()
}

func (c *countingStore) LoadBinds() map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := map[string]string{}
	maps.Copy(out, c.binds)

	return out
}

func routerWith(store redisstore.Store, avail []string, ttl time.Duration) *Router {
	return New(Config{
		TTL:       ttl,
		Store:     store,
		Available: func() []string { return avail },
	})
}

func TestWeightedVirtualCandidatesBiasNewSessions(t *testing.T) {
	t.Parallel()

	st := newCountingStore()
	// 一个普通账号 1 个虚拟实例，一个快过期账号 3 个虚拟实例。
	r := routerWith(st, []string{"regular", "expiring", "expiring", "expiring"}, time.Minute)

	// 先让每个真实账号都已有绑定，确保后续新会话进入全池哈希阶段，而不是
	// 被“未绑定账号优先”规则逐个填满。这样可以直接验证虚拟实例权重。
	r.Bind("warm-regular", "regular")
	r.Bind("warm-expiring", "expiring")

	counts := map[string]int{}

	const n = 4000
	for i := range n {
		key := fmt.Sprintf("session-%d", i)

		uid, ok := r.Resolve(key)
		if !ok {
			t.Fatal("Resolve returned !ok")
		}

		counts[uid]++
	}

	share := float64(counts["expiring"]) / float64(n)
	if share < 0.70 || share > 0.80 {
		t.Fatalf("weighted expiring share=%.3f counts=%v, want 70%%-80%%", share, counts)
	}
}

func TestSameKeySameAccount(t *testing.T) {
	t.Parallel()

	r := routerWith(newCountingStore(), []string{"a1", "a2"}, time.Minute)
	u1, ok1 := r.Resolve("c1")

	u2, ok2 := r.Resolve("c1")
	if !ok1 || !ok2 || u1 != u2 {
		t.Fatalf("same key should map to same account: %s vs %s", u1, u2)
	}

	if r.Count() != 1 {
		t.Errorf("count=%d want 1", r.Count())
	}
}

func TestTTLExpiryReassigns(t *testing.T) {
	t.Parallel()

	st := newCountingStore()
	r := routerWith(st, []string{"a1", "a2"}, 10*time.Millisecond)
	str2, _ := r.Resolve("c1")

	time.Sleep(20 * time.Millisecond)

	str, ok := r.Resolve("c1")
	if !ok {
		t.Fatal("resolve after expiry should still succeed")
	}
	// 过期后可重新分配（可能巧合同号，但至少返回有效账号）。
	_ = str2
	_ = str

	if r.Count() != 1 {
		t.Errorf("count=%d want 1 (reassigned, not duplicated)", r.Count())
	}
}

func TestBoundAccountCooldownReassigns(t *testing.T) {
	t.Parallel()

	r := routerWith(newCountingStore(), []string{"a1"}, time.Minute)

	str, _ := r.Resolve("c1")
	if str != "a1" {
		t.Fatalf("initial bind=%s want a1", str)
	}
	// a1 冷却 → 可用列表只剩 a2 → 重新分配必须换到 a2。
	r.cfg.Available = func() []string { return []string{"a2"} }

	str2, ok := r.Resolve("c1")
	if !ok {
		t.Fatal("resolve should succeed with fallback account")
	}

	if str2 == str {
		t.Fatalf("bound account %s cooled but still assigned", str)
	}

	if str2 != "a2" {
		t.Fatalf("reassigned to %s want a2", str2)
	}
}

func TestNoSessionKeyPassthrough(t *testing.T) {
	t.Parallel()
	// ExtractKey 找不到任何会话键 → 空串（调用方据空串走普通 Pick；router 不会被调用）。
	got := extractKey([]byte(`{"model":"x","messages":[]}`))
	if got != "" {
		t.Errorf("ExtractKey should return empty, got %q", got)
	}
}

func TestExtractKeyPriority(t *testing.T) {
	t.Parallel()

	cases := []struct {
		body string
		want string
	}{
		{`{"metadata":{"conversation_id":"mc","user_id":"mu"},"conversation_id":"top"}`, "mc"}, // metadata.conversation_id 优先
		{`{"conversation_id":"top"}`, "top"},                                                   // 顶层 conversation_id
		{`{"metadata":{"user_id":"mu"}}`, ""},                                                  // user_id 已剔除粘性键（粒度过粗），回落轮换
		{`{"metadata":{"conversation_id":123}}`, ""},                                           // 非字符串 → 空
		{`not-json`, ""}, // 非法 JSON → 空
		// issue #35：客户端实际发 camelCase conversationId，ExtractKey 必须识别。
		{`{"conversationId":"abc"}`, "abc"},                                            // 顶层 camelCase
		{`{"metadata":{"conversationId":"abc"}}`, "abc"},                               // metadata.camelCase
		{`{"metadata":{"conversation_id":"snake","conversationId":"camel"}}`, "snake"}, // snake 优先于 camel
		{`{"conversation_id":"snake","conversationId":"camel"}`, "snake"},              // 顶层 snake 优先于 camel
		{`{"conversationId":123}`, ""},                                                 // 数字 conversationId → 空
		{`{"metadata":{"conversationId":456}}`, ""},                                    // metadata 数字 conversationId → 空
		{`{"metadata":{"conversationId":"abc","user_id":"mu"}}`, "abc"},                // camel conversationId 优先于 user_id
		// 顶层字段解析不再依赖 metadata 必须是对象：非对象 / null / 类型不符时
		// 仍继续看顶层 conversation_id（旧 map 断言失败后的行为）。
		{`{"metadata":"x","conversation_id":"top"}`, "top"},
		{`{"metadata":null,"conversation_id":"top"}`, "top"},
		{`{"metadata":{"conversation_id":123},"conversation_id":"top"}`, "top"},
		{`{"metadata":{"user_id":123},"conversation_id":"top"}`, "top"},
	}
	for _, c := range cases {
		if got := extractKey([]byte(c.body)); got != c.want {
			t.Errorf("extractKey(%s)=%q want %q", c.body, got, c.want)
		}
	}
}

func TestConcurrentSameKeyAssignsOnce(t *testing.T) {
	t.Parallel()

	avail := []string{"a1", "a2", "a3", "a4", "a5"}
	r := routerWith(newCountingStore(), avail, time.Minute)

	const N = 100

	uids := make([]string, N)

	var wg sync.WaitGroup
	for i := range N {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			u, ok := r.Resolve("same-key")
			if ok {
				uids[idx] = u
			}
		}(i)
	}

	wg.Wait()

	// 所有 goroutine 必须拿到同一个账号（写锁 re-check 防重复分配）。
	first := ""

	for _, str := range uids {
		if str == "" {
			t.Fatal("some goroutine failed to resolve")
		}

		if first == "" {
			first = str
		}

		if str != first {
			t.Fatalf("concurrent resolve assigned different accounts: %s vs %s", first, str)
		}
	}

	if r.Count() != 1 {
		t.Errorf("count=%d want 1 (single binding)", r.Count())
	}
}

// boundUID 直接读绑定 uid（不触发 Resolve 的重分配），供 Bind 系列测试断言用（包内私有 helper）。
func (r *Router) boundUID(key string) (string, bool) {
	r.mu.RLock()
	e, ok := r.entries[key]
	r.mu.RUnlock()

	return e.uid, ok
}

func TestBindOverridesAndMirrors(t *testing.T) {
	t.Parallel()
	// Bind 幂等覆盖旧值，并异步镜像 SetBind。
	countingStore2 := newCountingStore()
	r := routerWith(countingStore2, []string{"a1", "a2"}, time.Minute)
	r.Bind("c1", "a1")

	if u, ok := r.boundUID("c1"); !ok || u != "a1" {
		t.Fatalf("bind c1->a1 then bound=%s ok=%v", u, ok)
	}
	// 覆盖到 a2
	r.Bind("c1", "a2")

	if u, _ := r.boundUID("c1"); u != "a2" {
		t.Fatalf("bind override should map c1->a2, got %s", u)
	}

	if r.Count() != 1 {
		t.Errorf("bind override must not duplicate entries, count=%d", r.Count())
	}

	countingStore2.mu.Lock()
	n := countingStore2.setBinds
	binds := map[string]string{}
	maps.Copy(binds, countingStore2.binds)
	countingStore2.mu.Unlock()

	if n != 2 {
		t.Errorf("SetBind mirror count=%d want 2", n)
	}

	if binds["c1"] != "a2" {
		t.Errorf("mirrored bind should be a2, got %s", binds["c1"])
	}
}

func TestBindIgnoresEmptyKey(t *testing.T) {
	t.Parallel()

	countingStore2 := newCountingStore()
	r := routerWith(countingStore2, []string{"a1"}, time.Minute)
	r.Bind("", "a1")
	r.Bind("c1", "")

	if r.Count() != 0 {
		t.Errorf("Bind with empty key/uid must be no-op, count=%d", r.Count())
	}

	countingStore2.mu.Lock()
	n := countingStore2.setBinds
	countingStore2.mu.Unlock()

	if n != 0 {
		t.Errorf("empty-key Bind must not mirror, setBinds=%d", n)
	}
}

func TestBindThenUnbindLifecycle(t *testing.T) {
	t.Parallel()

	countingStore2 := newCountingStore()
	r := routerWith(countingStore2, []string{"a1"}, time.Minute)
	r.Bind("c1", "a1")

	if !r.Unbind("c1") {
		t.Fatal("Unbind should report found")
	}

	if r.Count() != 0 {
		t.Errorf("count after unbind=%d want 0", r.Count())
	}

	countingStore2.mu.Lock()
	del := countingStore2.delBinds
	countingStore2.mu.Unlock()

	if del != 1 {
		t.Errorf("DelBind mirror count=%d want 1", del)
	}
}

func TestRedisMirrorSetBindCount(t *testing.T) {
	t.Parallel()

	countingStore2 := newCountingStore()
	r := routerWith(countingStore2, []string{"a1", "a2"}, time.Minute)
	r.Resolve("c1")
	r.Resolve("c1") // 快路径 touch → 又镜像一次

	if countingStore2.setBinds < 1 {
		t.Errorf("SetBind mirror count=%d want >=1", countingStore2.setBinds)
	}

	r.Unbind("c1")

	if countingStore2.delBinds != 1 {
		t.Errorf("DelBind mirror count=%d want 1", countingStore2.delBinds)
	}
}

func TestGCCleansExpired(t *testing.T) {
	t.Parallel()

	st := newCountingStore()
	r := routerWith(st, []string{"a1"}, 10*time.Millisecond)
	r.Resolve("c1")
	r.Resolve("c2")
	time.Sleep(20 * time.Millisecond)

	removed := r.gcOnce(time.Now())
	if removed != 2 {
		t.Errorf("gc removed=%d want 2", removed)
	}

	if r.Count() != 0 {
		t.Errorf("count after gc=%d want 0", r.Count())
	}
}

func TestLoadFromStoreRestores(t *testing.T) {
	t.Parallel()

	st := newCountingStore()
	st.binds["c1"] = "a1"
	st.binds["c2"] = "a2"
	r := routerWith(st, []string{"a1", "a2"}, time.Minute)
	r.LoadFromStore()

	if r.Count() != 2 {
		t.Fatalf("restored count=%d want 2", r.Count())
	}

	u, ok := r.Resolve("c1")
	if !ok || u != "a1" {
		t.Errorf("restored c1 -> %s want a1", u)
	}
}

// TestExtractKeyDerivedFromContent 客户端不发会话 id 时回退到内容派生键：
// 同一对话多轮（历史追加）→ 键稳定不变；不同对话 → 键不同。
func TestExtractKeyDerivedFromContent(t *testing.T) {
	t.Parallel()
	// 第一轮
	turn1 := `{"model":"glm-5.3","messages":[{"role":"system","content":"你是助手"},{"role":"user","content":"帮我写个排序算法"}]}`
	// 第二轮：历史追加了 assistant 与新的 user（system 与首条 user 不变）
	turn2 := `{"model":"glm-5.3","messages":[{"role":"system","content":"你是助手"},{"role":"user","content":"帮我写个排序算法"},{"role":"assistant","content":"好的"},{"role":"user","content":"换成快排"}]}`

	str, key2 := extractKey([]byte(turn1)), extractKey([]byte(turn2))
	if str == "" {
		t.Fatal("derived key should not be empty when messages present")
	}

	if str != key2 {
		t.Errorf("derived key must be stable across turns: turn1=%q turn2=%q", str, key2)
	}

	if !strings.HasPrefix(str, "d-") {
		t.Errorf("derived key should carry prefix d-: %q", str)
	}
	// 不同对话（首条 user 不同）→ 不同键
	other := `{"model":"glm-5.3","messages":[{"role":"system","content":"你是助手"},{"role":"user","content":"翻译这段话"}]}`
	if extractKey([]byte(other)) == str {
		t.Error("different first user message must yield a different derived key")
	}
	// 非对象消息条目（旧整树解析里本就被 scanDeriveTexts 跳过）不影响派生结果。
	withJunk := `{"model":"glm-5.3","messages":["junk",{"role":"system","content":"你是助手"},{"role":"user","content":"帮我写个排序算法"}]}`
	if got := extractKey([]byte(withJunk)); got != str {
		t.Errorf("junk message entries must not change derived key: got %q want %q", got, str)
	}
	// 显式 id 优先于派生键
	withID := `{"conversation_id":"my-session","messages":[{"role":"user","content":"帮我写个排序算法"}]}`
	if got := extractKey([]byte(withID)); got != "my-session" {
		t.Errorf("explicit id must win over derived key, got %q", got)
	}
	// 无 messages / 纯无文本内容 → 空（退回普通轮换，不误粘）
	if got := extractKey([]byte(`{"model":"x"}`)); got != "" {
		t.Errorf("no messages should yield empty key, got %q", got)
	}

	if got := extractKey([]byte(`{"messages":[{"role":"user","content":[]}]}`)); got != "" {
		t.Errorf("text-less content should yield empty key, got %q", got)
	}
}

// TestExtractKeyMultimodalContent 多模态 content 数组取文本部分派生。
func TestExtractKeyMultimodalContent(t *testing.T) {
	t.Parallel()

	body := `{"messages":[{"role":"user","content":[{"type":"text","text":"看图说话"},{"type":"image_url","image_url":{"url":"http://x/y.png"}}]}]}`

	k := extractKey([]byte(body))
	if k == "" || !strings.HasPrefix(k, "d-") {
		t.Fatalf("multimodal text should derive a key, got %q", k)
	}
	// 同一文本（图片不同）→ 同键（图片不参与派生，避免签名 URL 变化破坏粘性）
	body2 := `{"messages":[{"role":"user","content":[{"type":"text","text":"看图说话"},{"type":"image_url","image_url":{"url":"http://x/z.png"}}]}]}`
	if extractKey([]byte(body2)) != k {
		t.Error("image url changes must not break derived key stability")
	}
	// 纯图片首条 user（无 text part）也应派生非空键（首图会话粘性盲区修复；
	// 图片只入类型占位，同图重发/换 URL 均同键）。
	imgOnly := `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"http://x/y.png"}}]}]}`

	str := extractKey([]byte(imgOnly))
	if str == "" || !strings.HasPrefix(str, "d-") {
		t.Fatalf("pure-image first user should derive a key, got %q", str)
	}

	imgOnly2 := `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"http://x/z.png"}}]}]}`
	if extractKey([]byte(imgOnly2)) != str {
		t.Error("pure-image url changes must keep same derived key")
	}
}

// TestResolveFastPathUsesAvailableOne（E1）：快路径命中时只查单账号可用性，
// 不得构建全池可用集合。
func TestResolveFastPathUsesAvailableOne(t *testing.T) {
	t.Parallel()

	var oneCalls, allCalls int

	r := New(Config{
		TTL: time.Hour,
		Available: func() []string {
			allCalls++

			return []string{"u1"}
		},
		AvailableOne: func(model, uid string) bool {
			oneCalls++

			return uid == "u1"
		},
	})
	r.Bind("session-a", "u1")

	uid, ok := r.ResolveForModel("session-a", "cn:glm-5.2")
	if !ok || uid != "u1" {
		t.Fatalf("resolve = (%q,%v), want (u1,true)", uid, ok)
	}

	if oneCalls != 1 || allCalls != 0 {
		t.Fatalf("fast path calls: AvailableOne=%d Available=%d, want 1/0", oneCalls, allCalls)
	}
}

// blockingSetStore 让 SetBind 在写入前阻塞，把「镜像写进行中」变成一个可观测状态。
type blockingSetStore struct {
	*countingStore

	entered chan struct{}
	release chan struct{}
	once    sync.Once
	block   atomic.Bool
}

func (s *blockingSetStore) SetBind(key, uid string, ttl time.Duration) {
	// 只在进入并发段后阻塞：前面的 Bind 需要正常返回。
	if s.block.Load() {
		s.once.Do(func() {
			close(s.entered)
		})
		<-s.release
	}

	s.countingStore.SetBind(key, uid, ttl)
}

// TestUnbindCannotBeOverwrittenByStaleMirror（#417 补充）：镜像写与内存绑定必须在
// 同一把锁内提交。否则 Unbind 可以插进「解锁 → SetBind」之间，让已解绑的绑定在
// Redis 里复活，重启后又恢复成粘性映射。
func TestUnbindCannotBeOverwrittenByStaleMirror(t *testing.T) {
	t.Parallel()

	countingStore2 := newCountingStore()
	store := &blockingSetStore{
		countingStore: countingStore2,
		entered:       make(chan struct{}),
		release:       make(chan struct{}),
	}
	r := routerWith(store, []string{"a1"}, time.Minute)
	r.Bind("c1", "a1")

	var wg sync.WaitGroup

	store.block.Store(true)
	wg.Go(func() { r.ResolveExisting("c1", "") })

	<-store.entered // 镜像写已开始（touch 的 SetBind 进行中）

	unbound := make(chan struct{})

	wg.Go(func() {
		r.Unbind("c1")
		close(unbound)
	})

	select {
	case <-unbound:
		close(store.release)
		wg.Wait()
		t.Fatal("镜像写未完成时 Unbind 就返回了：解绑可能被陈旧镜像写回")
	case <-time.After(20 * time.Millisecond):
	}

	close(store.release)
	wg.Wait()

	if _, inMemory := r.boundUID("c1"); inMemory {
		t.Fatal("Unbind 后内存不应还有绑定")
	}

	countingStore2.mu.Lock()
	_, inStore := countingStore2.binds["c1"]
	countingStore2.mu.Unlock()

	if inStore {
		t.Fatal("Unbind 后 Store 不得残留绑定（陈旧镜像把解绑写回）")
	}
}
