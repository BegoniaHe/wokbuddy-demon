// Package session 会话粘性路由：同一会话（conversationId / metadata 键）尽量绑定同一账号。
//
// 设计参考 antigravityProxyGo internal/session（fast-path RLock / 双段分配 / TTL / 持久化），
// 但改为纯内存 + redisstore 异步镜像：
//   - 命中走 RLock 快查（绝大多数请求已绑定）；
//   - 未命中/失效走写锁 re-check 后分配，避免同 key 并发重复分配（TOCTOU 防护）；
//   - 分配优先"空闲账号"（未绑定任何会话的可用号）哈希，其次全池哈希（双段策略）；
//   - LastActive 滚动续期，TTL 过期由后台 GC 或快路径惰性过期清理；
//   - 每次绑定变更 fire-and-forget 镜像到 redisstore（防重启丢粘性）。
package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/redisstore"
)

// entry 单条会话绑定。
type entry struct {
	uid        string
	lastActive time.Time
}

// avoidance 软逃逸后的回纳抑制（issue #116 P0-1）：窗口内不再绑定回原账号；
// 窗口结束后仍需 LowLoad 回落低水位才允许复用。独立于当前绑定保存，重新分配
// 或 Unbind 后仍然有效，不会因 entry 被覆盖/删除而丢失。
//
// 同一会话按账号保存多条窗口：换绑后的新账号再次降级时，旧账号未过期的窗口
// 必须保留，否则下一轮分配可能立刻回到旧账号。
type avoidance struct {
	uid   string
	until time.Time
}

// Config 路由依赖；Available 返回"可用账号"（healthy 且未占满在途）的有序 uid 列表，
// 由 pool.AvailableUIDs 提供。Store 可为 redisstore.Noop（纯内存）。
type Config struct {
	TTL        time.Duration
	GCInterval time.Duration
	Store      redisstore.Store
	Available  func() []string
	// AvailableForModel 按请求模型返回"在该模型上可用"的账号（healthy 且未占满在途，
	// 且未被该模型限流/限额）。model 为 "[realm:]model" 形式，由实现方剥前缀并按
	// realm 过滤；nil 时回落 Available（无模型维度，行为与引入前一致）。
	//
	// 为什么粘性需要模型维度：绑定只记 uid，而同一个会话可能换模型。账号被 6004
	// 模型级限额后对**其他模型**仍可用（issue #31 豁免），此时若只按账号级可用性
	// 校验，会话会被钉在这个号上反复失败——正是"限额后换不动号"的观感来源。
	AvailableForModel func(model string) []string
	// AvailableOne 单账号可用性判定（model 语法同上）。快路径命中时只查绑定账号，
	// 避免为一次检查构建全池列表与集合；nil 时回落 AvailableForModel/Available
	// （行为不变，代价是全池扫描）。
	AvailableOne func(model, uid string) bool
	// LowLoad 报告账号是否回落到可重新接收粘性绑定的低水位（降级窗口结束后使用）。
	// model 为 "[realm:]model"；nil 时视为始终可回纳。
	LowLoad func(model, uid string) bool
}

// Router 会话粘性路由器。
type Router struct {
	mu      sync.RWMutex
	entries map[string]entry
	avoid   map[string][]avoidance
	cfg     Config
	stop    chan struct{}
}

// New 构建路由器。若 cfg.Store 为 nil 则用 Noop（纯内存）；cfg.Available 为 nil 视为空池。
// TTL/GCInterval 非正取默认（30m / 5m）——main 从 config 解析后传入，这里兜底。
func New(cfg Config) *Router {
	if cfg.Store == nil {
		cfg.Store = redisstore.Noop{}
	}

	if cfg.TTL <= 0 {
		cfg.TTL = 30 * time.Minute
	}

	if cfg.GCInterval <= 0 {
		cfg.GCInterval = 5 * time.Minute
	}

	return &Router{entries: map[string]entry{}, avoid: map[string][]avoidance{}, cfg: cfg}
}

// StartGC 启动后台 GC goroutine（幂等）。进程退出时调 StopGC。
//
// stop channel 必须在启 goroutine 前捕获到**局部变量**：goroutine 在 select 里
// 每轮重新求值 r.stop 是无锁读，而 StopGC 持写锁把它置 nil——既是数据竞争
// （-race 可复现），又会在读到 nil 后让该 case 永久阻塞（nil channel 永不就绪），
// 于是关停彻底失效：goroutine 再也不会退出，ticker 无限触发 gcOnce（goroutine
// 泄漏 + 关停后仍持续 GC）。捕获局部变量后，close(stop) 与 select 观测的是同一个
// channel，StopGC 一定能让 goroutine 退出。
func (r *Router) StartGC() {
	r.mu.Lock()
	if r.stop != nil {
		r.mu.Unlock()
		return
	}

	stop := make(chan struct{})
	r.stop = stop
	r.mu.Unlock()

	go func() {
		t := time.NewTicker(r.cfg.GCInterval)
		defer t.Stop()

		for {
			select {
			case <-stop:
				return
			case <-t.C:
				r.gcOnce(time.Now())
			}
		}
	}()
}

// StopGC 停止后台 GC（幂等）。
func (r *Router) StopGC() {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.stop != nil {
		close(r.stop)
		r.stop = nil
	}
}

// LoadFromStore 启动时从 redisstore 恢复绑定（内存覆盖本地，读操作仅此处发生）。
// 已有本地绑定被保留——Redis 仅为恢复备份，本地一旦建立即为权威。
func (r *Router) LoadFromStore() {
	binds := r.cfg.Store.LoadBinds()
	if len(binds) == 0 {
		return
	}

	now := time.Now()

	r.mu.Lock()
	loaded := 0

	for key, uid := range binds {
		if _, exists := r.entries[key]; exists {
			continue
		}

		r.entries[key] = entry{uid: uid, lastActive: now}
		loaded++
	}
	r.mu.Unlock()

	if loaded > 0 {
		log.Printf("[session] 从 Redis 恢复 %d 条粘性会话绑定", loaded)
	}
}

// Resolve 返回会话 key 应绑定的账号 uid，ok=false 表示当前无可用账号。
// 无模型维度（等价于 ResolveForModel(key, "")），保留给不关心模型的调用方。
func (r *Router) Resolve(key string) (string, bool) {
	return r.ResolveForModel(key, "")
}

// ResolveExisting 只复用**既有**绑定：命中有效且该模型可用的绑定返回 uid，否则 false。
//
// 与 ResolveForModel 的差别是没有慢路径哈希分配——有序选号策略（round_robin /
// sequential_exhaust，issue #205）下，哈希分配会绕过账号顺序，让粘性变成「随处落号」。
// 调用方在未命中时交回选号策略决定。
func (r *Router) ResolveExisting(key, model string) (string, bool) {
	now := time.Now()

	r.mu.RLock()
	entry2, found := r.entries[key]
	a, hasAvoid := r.avoid[key]
	avoid := r.avoidActive(a, hasAvoid, entry2, model, now)
	r.mu.RUnlock()

	if !found || expired(entry2, now, r.cfg.TTL) || !r.availableOne(model, entry2.uid) || avoid {
		return "", false
	}

	r.touch(key, entry2.uid, model, now)

	return entry2.uid, true
}

// ResolveForModel 返回会话 key 在该模型上应绑定的账号 uid。
// 命中且账号在该模型可用 → 滚动 lastActive 并直接返回；否则（绑定号已冷却/占满/
// 被该模型限流）走重新分配。
//
// model 沿用网关的 "[realm:]model" 语法：AvailableForModel 实现方剥前缀后按 realm
// 过滤，避免 global 会话只传裸名时被当成 cn、绑定到错误域的账号。
//
// 为什么必须带模型：绑定只记 uid，同一个会话可能换模型；账号被 6004 模型级限额后
// 对其他模型仍可用（见 pool.healthyForModel 的 softRateModel 豁免）。若只按账号级
// 可用性校验，会话会被钉在一个"对当前模型不可用"的号上反复失败。
func (r *Router) ResolveForModel(key, model string) (string, bool) {
	now := time.Now()

	// ── Fast path: RLock 快查 ──────────────────────────────
	r.mu.RLock()
	entry2, found := r.entries[key]
	a, hasAvoid := r.avoid[key]
	// avoidActive 必须在锁内判定：Degrade/touch/Bind 会原地改写同一底层切片，
	// 只复制 slice header 出锁再遍历仍会与写方并发。
	avoid := r.avoidActive(a, hasAvoid, entry2, model, now)
	r.mu.RUnlock()

	// 命中只判定绑定账号本身，不构建全池可用集合（每请求 O(N log N) + map 分配）。
	if found && !expired(entry2, now, r.cfg.TTL) && r.availableOne(model, entry2.uid) && !avoid {
		r.touch(key, entry2.uid, model, now)
		return entry2.uid, true
	}
	// 未命中/失效，或绑定号已冷却/占满 → 落入慢路径重分配。

	available := r.availableSet(model)

	// ── Slow path: 写锁 re-check 后分配 ────────────────────
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.resolveSlowLocked(key, model, now, available)
}

// availableOne 查询单账号可用性；未注入闭包时回退全池集合（兼容旧配置/测试）。
func (r *Router) availableOne(model, uid string) bool {
	if r.cfg.AvailableOne != nil {
		return r.cfg.AvailableOne(model, uid)
	}

	return r.availableSet(model)[uid]
}

// resolveSlowLocked 在写锁内完成 re-check 与分配（见 ResolveForModel 头注）。
func (r *Router) resolveSlowLocked(key, model string, now time.Time, available map[string]bool) (string, bool) {
	// re-check：并发同 key 可能已被其他 goroutine 分配好。
	if entry2, found := r.entries[key]; found && !expired(entry2, now, r.cfg.TTL) {
		a, hasAvoid := r.avoid[key]

		avoid := r.avoidActive(a, hasAvoid, entry2, model, now)
		if available[entry2.uid] && !avoid {
			r.entries[key] = entry{uid: entry2.uid, lastActive: now}
			return entry2.uid, true
		}

		if !avoid {
			delete(r.entries, key) // 失效：清掉再分配
		}
		// 降级抑制中：保留原条目，让 pickUID 读取 avoid 记录后避开原账号。
	}

	uids := r.availableSlice(model)
	if len(uids) == 0 {
		return "", false
	}

	uid := r.pickUID(key, uids, model, now)
	prev, existed := r.entries[key]

	r.entries[key] = entry{uid: uid, lastActive: now}
	if existed && prev.uid != uid {
		r.cfg.Store.DelBind(key)
	}

	r.cfg.Store.SetBind(key, uid, r.cfg.TTL)

	return uid, true
}

// pickUID 双段分配：优先"空闲账号"（未被任何会话绑定的可用号），其次全池。
func (r *Router) pickUID(key string, uids []string, model string, now time.Time) string {
	bound := map[string]bool{}
	for _, v := range r.entries {
		bound[v.uid] = true
	}

	avoided := r.avoidedUIDs(key, model, now)

	var idle []string

	for _, item := range uids {
		if avoided[item] {
			continue
		}

		if !bound[item] {
			idle = append(idle, item)
		}
	}

	pool := idle
	if len(pool) == 0 {
		pool = uids
	}

	if len(avoided) > 0 {
		if filtered := withoutUIDs(pool, avoided); len(filtered) > 0 {
			pool = filtered
		}
	}

	return pool[hashIndex(key, len(pool))]
}

// avoidedUIDs 返回当前降级抑制中的账号集合。窗口内始终避开；窗口结束后仅在
// LowLoad 未回落低水位时继续避开。
func (r *Router) avoidedUIDs(key, model string, now time.Time) map[string]bool {
	var out map[string]bool

	for _, item := range r.avoid[key] {
		if item.uid == "" {
			continue
		}

		if now.Before(item.until) || (r.cfg.LowLoad != nil && !r.cfg.LowLoad(model, item.uid)) {
			if out == nil {
				out = map[string]bool{}
			}

			out[item.uid] = true
		}
	}

	return out
}

// withoutUIDs 返回去掉指定账号的副本；结果为空时保持原列表（单账号池不饿死）。
func withoutUIDs(uids []string, avoided map[string]bool) []string {
	out := make([]string, 0, len(uids))
	for _, u := range uids {
		if !avoided[u] {
			out = append(out, u)
		}
	}

	return out
}

// touch 滚动 lastActive 并异步镜像（只在快路径命中时写最后一次）。
// 写锁内 re-check：快路径读取与 touch 之间可能发生 Degrade（写入 avoid）或
// Unbind（删除条目）；直接覆盖会把降级窗口清掉/让已解绑的会话复活。
//
// 清理已过期窗口时必须区分账号：当前绑定号被 touch 说明它已被重新选中，
// 其窗口可清；其他账号的窗口即使过了时间窗，只要 LowLoad 仍判定未回落低水位，
// 就仍是有效抑制，不能因为本会话在别的账号上成功就一并清掉。
func (r *Router) touch(key, uid, model string, now time.Time) {
	r.mu.Lock()

	entry2, ok := r.entries[key]
	if !ok || entry2.uid != uid {
		r.mu.Unlock()
		return
	}
	// 降级窗口内不得把刚被 Degrade 删除的镜像写回；已过期且不再抑制的窗口顺手清掉。
	suppressed := r.pruneAvoidLocked(key, uid, model, now)

	entry2.lastActive = now
	r.entries[key] = entry2
	r.mu.Unlock()

	if suppressed {
		return
	}

	r.cfg.Store.SetBind(key, uid, r.cfg.TTL)
}

// pruneAvoidLocked 清理该会话已失效的抑制窗口，返回当前绑定是否仍被抑制。
// 当前绑定号（uid）一旦被 touch 即视为回纳证据；其他账号过窗后仍需 lowLoad
// 放行才可清理，否则 LowLoad 门控会因本会话在别处成功而丢失。
func (r *Router) pruneAvoidLocked(key, uid, model string, now time.Time) bool {
	list, ok := r.avoid[key]
	if !ok {
		return false
	}

	suppressed := false
	kept := list[:0]

	for _, item := range list {
		expired := !now.Before(item.until)
		if expired && (item.uid == uid || r.lowLoad(model, item.uid)) {
			continue
		}

		if item.uid == uid {
			suppressed = true
		}

		kept = append(kept, item)
	}

	if len(kept) == 0 {
		delete(r.avoid, key)
	} else {
		r.avoid[key] = kept
	}

	return suppressed
}

// lowLoad 报告账号是否已回落低水位；未配置判定时视为始终可回纳。
func (r *Router) lowLoad(model, uid string) bool {
	return r.cfg.LowLoad == nil || r.cfg.LowLoad(model, uid)
}

// Bind 显式把会话 key 绑定到 uid（幂等覆盖旧值），并异步镜像到 redisstore。
// 供"粘性跟随最终成功号"用：请求成功返回前，把会话重绑到实际成功的账号，让多轮对话下一跳稳定
// 收敛到"对该会话持续成功的号"（对齐 antigravity 语义）。空 key 直接返回（无会话则不绑）。
func (r *Router) Bind(key, uid string) {
	if key == "" || uid == "" {
		return
	}

	now := time.Now()

	r.mu.Lock()
	r.entries[key] = entry{uid: uid, lastActive: now}
	// 绑定成功即回纳证据：只清除该账号的抑制；换绑到其他账号时保留其余窗口
	// （含已过期但 LowLoad 仍未回落的），防止旧账号立刻又被重新绑定。
	if list, ok := r.avoid[key]; ok {
		kept := list[:0]
		for _, a := range list {
			if a.uid == uid {
				continue
			}

			kept = append(kept, a)
		}

		if len(kept) == 0 {
			delete(r.avoid, key)
		} else {
			r.avoid[key] = kept
		}
	}
	r.mu.Unlock()
	r.cfg.Store.SetBind(key, uid, r.cfg.TTL)
}

// avoidActive 报告会话当前绑定是否仍处于降级抑制：窗口内禁止复用原账号；
// 窗口结束后仍要求 LowLoad 低水位。绑定已换到其他账号时不再抑制。
// 调用方须持有 r.mu（读锁或写锁）并在锁内调用：Degrade/Bind/touch 会原地改写
// 切片元素，锁外遍历同一底层数组会与写方数据竞争。
func (r *Router) avoidActive(list []avoidance, found bool, entry2 entry, model string, now time.Time) bool {
	if !found || entry2.uid == "" {
		return false
	}

	var until time.Time
	for _, a := range list {
		if a.uid == entry2.uid && a.until.After(until) {
			until = a.until
		}
	}

	if until.IsZero() {
		return false
	}

	if now.Before(until) {
		return true
	}

	return r.cfg.LowLoad != nil && !r.cfg.LowLoad(model, entry2.uid)
}

// Degrade 软逃逸后把账号置为降级：窗口内不绑定回原账号，并删除 Redis 镜像。
// 抑制独立于当前绑定保存，重新分配/解绑后仍然有效，直到窗口结束且低水位回纳。
// 同一会话可同时抑制多个账号；重复降级同一账号取更晚的窗口结束时间。
func (r *Router) Degrade(key, uid string, duration time.Duration) {
	if key == "" || uid == "" {
		return
	}

	now := time.Now()

	r.mu.Lock()

	entry2, ok := r.entries[key]
	if !ok || entry2.uid != uid {
		r.mu.Unlock()
		return
	}

	until := now.Add(duration)
	list := r.avoid[key]
	merged := false

	for i := range list {
		if list[i].uid != uid {
			continue
		}

		if until.After(list[i].until) {
			list[i].until = until
		}

		merged = true

		break
	}

	if !merged {
		list = append(list, avoidance{uid: uid, until: until})
	}

	r.avoid[key] = list
	entry2.lastActive = now
	r.entries[key] = entry2
	r.mu.Unlock()
	r.cfg.Store.DelBind(key)
}

// Unbind 解除会话绑定（请求失败时调用，让该会话下次重新分配）。返回是否存在。
func (r *Router) Unbind(key string) bool {
	r.mu.Lock()

	_, found := r.entries[key]
	if found {
		delete(r.entries, key)
	}
	r.mu.Unlock()

	if found {
		r.cfg.Store.DelBind(key)
	}

	return found
}

// Count 返回当前绑定数（供 /status 观测）。
func (r *Router) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return len(r.entries)
}

// gcOnce 清理 TTL 过期的绑定，并镜像删除。
func (r *Router) gcOnce(now time.Time) int {
	r.mu.Lock()

	var expiredKeys []string

	for key, e := range r.entries {
		if now.Sub(e.lastActive) > r.cfg.TTL {
			expiredKeys = append(expiredKeys, key)
		}
	}

	for _, key := range expiredKeys {
		delete(r.entries, key)
		delete(r.avoid, key)
	}
	// 已解绑且窗口过期的抑制记录不再有用（绑定中的过期记录保留给 LowLoad 门控，
	// 由 touch 在回纳时清除）。
	for key, list := range r.avoid {
		if _, bound := r.entries[key]; bound {
			continue
		}

		kept := list[:0]
		for _, a := range list {
			if now.Before(a.until) {
				kept = append(kept, a)
			}
		}

		if len(kept) == 0 {
			delete(r.avoid, key)
		} else {
			r.avoid[key] = kept
		}
	}
	r.mu.Unlock()

	for _, key := range expiredKeys {
		r.cfg.Store.DelBind(key)
	}

	return len(expiredKeys)
}

// availableSet 把 AvailableForModel(model) 的有序列表转集合（快路径命中校验用）。
func (r *Router) availableSet(model string) map[string]bool {
	uids := r.availableSlice(model)

	set := make(map[string]bool, len(uids))
	for _, u := range uids {
		set[u] = true
	}

	return set
}

// availableSlice 安全调用 AvailableForModel；未注入时回落 Available（nil 视空池）。
func (r *Router) availableSlice(model string) []string {
	if r.cfg.AvailableForModel != nil {
		return r.cfg.AvailableForModel(model)
	}

	if r.cfg.Available == nil {
		return nil
	}

	return r.cfg.Available()
}

func expired(e entry, now time.Time, ttl time.Duration) bool {
	return now.Sub(e.lastActive) > ttl
}

// hashIndex FNV-1a 哈希取模（antigravity 双段分配的稳定散列）。
func hashIndex(key string, n int) int {
	var num uint32 = 2166136261
	for i := range len(key) {
		num ^= uint32(key[i])
		num *= 16777619
	}

	return int(num % uint32(n)) //nolint:gosec // n 为账号池规模，恒远小于 2^32
}

// ExtractKey 从请求体提取会话键；按下列顺序依次尝试，找不到时回退到
// 内容派生的稳定键（见 deriveKey），仍为空则返回空串（绝不失败）。
//  1. metadata.conversation_id
//  2. metadata.conversationId
//  3. conversation_id
//  4. conversationId
//  5. prompt_cache_key（OpenAI 前缀缓存键，见下）
//  6. 派生键：system 提示词 + 首条用户消息的哈希（客户端不发会话 id 时的回退，
//     带 user_id 的请求不派生——见下方契约说明）
//
// 前 5 项均为 conversation 维度（对话级）。metadata.user_id 不再作为粘性键
// （P1-anti-monopoly 剔除）：user 维度粒度过粗——一个 user 的全部并行对话
// 会钉同一账号（粘性范围远大于上游 prompt cache 的对话级边界），且曾抢占顶层
// conversation_id 的优先级。剔除后发 user_id 的客户端回落加权轮换（与无标识
// 客户端同路径），旧 user_id 绑定靠 TTL 自然过期，键消失不产生脏绑定。
//
// issue #35：客户端实际发 camelCase 的 conversationId，此前只识别 snake_case，
// 导致粘性路由不命中、同对话轮转不同账号、上游上下文缓存 miss。现两种命名均识别，
// snake_case 优先级高于 camelCase（同值不同名命中同一对话时返回相同值，天然不混用）。
//
// 第 5 项 prompt_cache_key：pi-ai 驱动的客户端（dsh 等）把会话 ID 放在这个 OpenAI
// 前缀缓存字段里（而非 conversation_id），网关在 upstream 侧本就认它（见
// InjectPromptCacheKey 优先级 1：客户端自带则原值保留）。纳入识别后，这类客户端
// 无需改配置即可命中粘性。置于显式会话键之后、派生键之前，绝不抢占 conversation
// 维度的优先级。
//
// 派生键的 user_id 抑制（P1-anti-monopoly 契约在 fallback 路径的延伸）：body 携带
// metadata.user_id 或顶层 user_id 时**不派生**。ExtractKey 有意剔除 user_id 作粘性键
// （user 维度粒度过粗——一个 user 的全部并行对话会被钉到同一账号），若派生路径不设
// 此闸，只发 user_id 的请求会借内容哈希重新获得粘性，使该契约在 fallback 路径失效
// （上游 #169 同款回归修正）。这类客户端回落加权轮换。
func ExtractKey(body []byte) string {
	if len(body) == 0 {
		return ""
	}

	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return ""
	}

	if meta, ok := obj["metadata"].(map[string]any); ok {
		if v := strOrEmpty(meta["conversation_id"]); v != "" {
			return v
		}

		if v := strOrEmpty(meta["conversationId"]); v != "" {
			return v
		}
	}

	if v := strOrEmpty(obj["conversation_id"]); v != "" {
		return v
	}

	if v := strOrEmpty(obj["conversationId"]); v != "" {
		return v
	}
	// 5. prompt_cache_key：OpenAI 系的会话级前缀缓存键，语义就是"同一会话复用同一
	//    前缀"，与粘性诉求同源。放显式会话键之后、派生键之前，不抢占优先级。
	if v := strOrEmpty(obj["prompt_cache_key"]); v != "" {
		return v
	}
	// 派生回退闸：带 user 维度标识的请求不派生（契约见上）。
	if hasUserIDKey(obj) {
		return ""
	}

	return deriveKey(obj)
}

// hasUserIDKey 报告已解析 body 是否携带 user 维度标识（metadata.user_id 或顶层
// user_id，非空字符串才算）。只用于派生回退闸（ExtractKey）；解析失败按无处理。
func hasUserIDKey(obj map[string]any) bool {
	if meta, ok := obj["metadata"].(map[string]any); ok {
		if strOrEmpty(meta["user_id"]) != "" {
			return true
		}
	}

	return strOrEmpty(obj["user_id"]) != ""
}

// derivedKeyPrefix 派生键前缀，与显式会话 id 的命名空间隔离：
// 即便客户端恰好传了形如 "d-<hex>" 的显式 id 也不至于与派生键混淆（显式 id 优先返回）。
const derivedKeyPrefix = "d-"

// deriveKey 从消息内容派生稳定会话键：SHA-256(system 文本 + 首条 user 文本) 前 16 字节。
//
// 为什么用「system + 首条 user」而不是全部消息：
//   - 多轮对话里历史消息每轮追加，全量哈希会每轮变化 → 粘性完全失效；
//   - system 与首条 user 在一次对话中恒定，足以区分不同对话；
//   - 同一会话多轮请求 → 同一键 → 稳定粘住同一账号（上游 prompt 缓存命中）。
//
// 取不到用户文本（纯图片等）时返回空串：不粘性，退回普通轮换（安全降级）。
func deriveKey(obj map[string]any) string {
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return ""
	}

	systemText, firstUserText := scanDeriveTexts(msgs)
	if firstUserText == "" {
		return "" // 无用户消息：无从归属会话
	}

	sum := sha256.Sum256([]byte(systemText + "\x00" + firstUserText))

	return derivedKeyPrefix + hex.EncodeToString(sum[:16])
}

// scanDeriveTexts 遍历消息，取首条 system/developer 文本与首条 user 内容签名；
// 两者都拿到即提前停止（见 deriveKey）。首条 user 用内容签名（ids.go
// contentSignature 的会话级变体 userContentSignature）而非纯文本提取：纯图片轮
// （无 text part）在 messageText 下恒空串 → 派生键失效（首图会话的粘性盲区）；
// 签名口径下图片 part 以 [type] 占位入键。
func scanDeriveTexts(msgs []any) (systemText, firstUserText string) {
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}

		text := messageText(msg["content"])
		switch strOrEmpty(msg["role"]) {
		case "system", "developer":
			if systemText == "" {
				systemText = text
			}
		case "user":
			if firstUserText == "" {
				firstUserText = userContentSignature(msg["content"])
			}
		}

		if firstUserText != "" && systemText != "" {
			break // 都已拿到：停止遍历长历史
		}
	}

	return systemText, firstUserText
}

// userContentSignature 把已解析的 content（any）转回 RawMessage 后取**会话级稳定**
// 签名，供 deriveKey（粘性键）专用。与 TurnKey 的 contentSignature（全文摘）不同：
// 文本 part 照常拼接；非文本 part 只入 "[type]" 占位**不入内容摘要**——同一逻辑
// 图片的 URL 逐轮变化（签名 URL/重编码 base64）是真实场景，内容摘要入键会让
// 派生键随轮漂移、粘性名存实亡（fork 既有契约：image url changes must not
// break derived key stability）。占位仍能区分"有无图片/图片数量"，纯图片首条
// 消息（无 text part）由此可派生非空键（首图会话粘性盲区修复，对齐上游 G1
// 的目标语义而保留 fork 的稳定性口径）。marshal 失败按无内容处理（不伪造）。
func userContentSignature(content any) string {
	raw, err := json.Marshal(content)
	if err != nil {
		return ""
	}

	str2 := strings.TrimSpace(string(raw))
	if str2 == "" || str2 == "null" {
		return ""
	}

	if str2[0] == '"' {
		var str string
		if json.Unmarshal(raw, &str) != nil {
			return ""
		}

		return str
	}

	if str2[0] != '[' {
		return ""
	}
	// 非文本 part 只入 "[type]" 占位，不入内容摘要（保证派生键随图片 URL 变化
	// 仍稳定——fork 既有契约）。
	return partsSignature(raw, func(typeName string, _ json.RawMessage) string {
		return "\n[" + typeName + "]\n"
	})
}

// messageText 提取消息 content 的文本表示。
// 兼容：字符串 / [{type:"text",text:"..."}] 数组（OpenAI 多模态）；其他类型取空。
func messageText(content any) string {
	switch item := content.(type) {
	case string:
		return item
	case []any:
		var builder strings.Builder

		for _, part := range item {
			if p, ok := part.(map[string]any); ok {
				builder.WriteString(strOrEmpty(p["text"]))
			}
		}

		return builder.String()
	}

	return ""
}

// strOrEmpty 把 JSON 字符串字段安全转 string（非字符串类型返回空）。
func strOrEmpty(v any) string {
	s, _ := v.(string)
	return s
}
