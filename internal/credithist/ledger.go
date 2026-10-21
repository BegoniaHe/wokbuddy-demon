// Package credithist 本地积分变动账本：每次余额变化记一条（时间/账号/变动/变动后余额）。
//
// 与既有能力的区别：
//   - `/panel/api/packages` 回答「有哪些积分包发放」（发放视角）；
//   - 本账本回答「余额何时变成多少」（余额变动视角）。
//
// 设计取舍（KISS）：
//   - 本地单文件、与 state_file 同侧，不做跨节点共享（单实例部署模型，见 IGNOREME）；
//   - 首次观测只建基线、不记一条「凭空 +N」；异常变动（超上限的跳变）丢弃并记基线；
//   - 条数上限滚动裁剪，落盘「先写 tmp 再 rename」；写盘失败不得影响余额查询主路径。
package credithist

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/atomicwrite"
)

// DefaultMaxEntries 单文件默认保留的流水条数（滚动裁剪）。
const DefaultMaxEntries = 5000

// sanityDelta 单次变动绝对值上限：超过视为脏数据/单位错误，不记流水（只更新基线）。
// 取 1e7：正常积分规模在万级，百万级跳变几乎必然是解析错误。
const sanityDelta = 1e7

// Entry 一条余额变动流水。
type Entry struct {
	Time   time.Time `json:"time"`
	UID    string    `json:"uid"`
	Delta  int64     `json:"delta"`
	Remain int64     `json:"remain"`
	Note   string    `json:"note,omitempty"`
}

// Ledger 账本（并发安全）。
type Ledger struct {
	// flushMu 串行化落盘：多个账号的余额查询会并发触发 Observe+Flush，共用同一个
	// 「path.tmp」；不串行化时两个写者会在同一 inode 上交错写/rename，产生非法 JSON。
	flushMu sync.Mutex
	mu      sync.Mutex
	path    string
	max     int
	entries []Entry
	// last 每个账号最近一次观测到的余额（基线判定；重启后由磁盘 entries 恢复）。
	last map[string]int64
	// dirty 是否有未落盘的变更。
	dirty bool
}

// fileDoc 账本文件形态（带 schema 版本，便于将来迁移）。
type fileDoc struct {
	Version int     `json:"version"`
	Entries []Entry `json:"entries"`
}

// Open 打开（或创建）账本；path 为空时为纯内存账本（测试/未配置路径）。
func Open(path string, maxEntries int) *Ledger {
	ledger := &Ledger{path: path, max: maxEntries, last: map[string]int64{}}
	if ledger.max <= 0 {
		ledger.max = DefaultMaxEntries
	}

	ledger.load()

	return ledger
}

// load 读回已有流水并恢复各账号基线（只保留有 UID 的条目）。
func (l *Ledger) load() {
	if l.path == "" {
		return
	}

	raw, err := os.ReadFile(l.path)
	if err != nil {
		return // 首次运行/文件缺失：从空账本开始，不是错误。
	}

	var doc fileDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		log.Printf("WARN: [credithist] 账本损坏，按空账本继续: %v", err)
		return
	}

	for _, entry := range doc.Entries {
		if entry.UID == "" {
			continue
		}

		l.entries = append(l.entries, entry)
		l.last[entry.UID] = entry.Remain
	}
}

// Observe 记录一次余额观测：与上次比有变化才记流水。
// 首次观测只建基线（避免凭空一条 +N）；|delta| 超过 sanityDelta 时丢弃并重建基线。
func (l *Ledger) Observe(uid string, remain int64, note string) {
	if uid == "" {
		return
	}

	l.mu.Lock()

	prev, seen := l.last[uid]
	l.last[uid] = remain

	if !seen {
		l.mu.Unlock()
		return
	}

	delta := remain - prev
	if delta == 0 {
		l.mu.Unlock()
		return
	}

	if delta > sanityDelta || delta < -sanityDelta {
		// 脏数据：不记流水（否则一条异常值会污染整本账），只更新基线。
		l.mu.Unlock()

		return
	}

	l.entries = append(l.entries, Entry{Time: time.Now(), UID: uid, Delta: delta, Remain: remain, Note: note})
	l.trimLocked()
	l.dirty = true
	l.mu.Unlock()
}

// trimLocked 滚动裁剪到上限（保留最近 max 条；调用方已持锁）。
func (l *Ledger) trimLocked() {
	if len(l.entries) <= l.max {
		return
	}

	l.entries = l.entries[len(l.entries)-l.max:]
}

// Recent 返回最近 limit 条流水（按时间升序返回，便于前端直接倒序渲染），
// uid 非空时只返回该账号；limit<=0 用默认 200，上限 1000。
func (l *Ledger) Recent(limit int, uid string) []Entry {
	if limit <= 0 {
		limit = 200
	}

	if limit > 1000 {
		limit = 1000
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	out := make([]Entry, 0, limit)

	for i := len(l.entries) - 1; i >= 0 && len(out) < limit; i-- {
		entry := l.entries[i]
		if uid != "" && entry.UID != uid {
			continue
		}

		out = append(out, entry)
	}

	// 反转成时间升序。
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}

	return out
}

// Flush 把当前账本原子写盘（先写 tmp 再 rename）。写失败只记日志——
// 账本是观测附属物，不能影响余额查询主路径。
func (l *Ledger) Flush() {
	// 先串行化整个「快照 + 写 tmp + rename」：Flush 会在 p.mu 之外做磁盘 I/O，
	// 并发 Flush 若共用同一个 tmp 路径会互相截断/交错写。flushMu 只在落盘路径使用，
	// 不与 l.mu 形成反向加锁（其它方法只取 l.mu）。
	l.flushMu.Lock()
	defer l.flushMu.Unlock()

	l.mu.Lock()

	if !l.dirty || l.path == "" {
		l.mu.Unlock()
		return
	}

	raw, err := json.Marshal(fileDoc{Version: 1, Entries: l.entries})
	// 记下本次写出的条数：清脏标记后，写盘期间新进来的流水必须重新置脏，
	// 否则它们只留在内存里，下一次 Flush 会因「不脏」直接跳过。
	written := len(l.entries)
	l.dirty = false
	l.mu.Unlock()

	if err != nil {
		log.Printf("WARN: [credithist] 序列化失败: %v", err)
		l.markDirty()

		return
	}

	tmp := l.path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		log.Printf("WARN: [credithist] 建目录失败: %v", err)
		l.markDirty()

		return
	}

	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("WARN: [credithist] 写临时文件失败: %v", err)
		l.markDirty()

		return
	}

	if err := atomicwrite.Rename(tmp, l.path); err != nil {
		log.Printf("WARN: [credithist] 原子替换失败: %v", err)
		l.markDirty()

		return
	}

	l.dirtyIfGrown(written)
}

// markDirty 重新置脏（写盘失败时调用）：脏标记已提前清掉，不置回就会永久丢写。
func (l *Ledger) markDirty() {
	l.mu.Lock()
	l.dirty = true
	l.mu.Unlock()
}

// dirtyIfGrown 写盘成功后补一次脏标记：写盘期间若又追加了流水（条数变化），
// 这批新条目还没落盘，必须让下一次 Flush 再写一遍。
func (l *Ledger) dirtyIfGrown(written int) {
	l.mu.Lock()

	if len(l.entries) != written {
		l.dirty = true
	}

	l.mu.Unlock()
}
