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
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/safefile"
)

// DefaultMaxEntries 单文件默认保留的流水条数（滚动裁剪）。
const DefaultMaxEntries = 5000

// sanityDelta 单次变动绝对值上限：超过视为脏数据/单位错误，不记流水（只更新基线）。
// 取 1e7：正常积分规模在万级，百万级跳变几乎必然是解析错误。
const sanityDelta = 1e7

// maxBalance 余额绝对值的合理上界，超过即视为脏数据/单位错误。
//
// 它同时是减法溢出的护栏：remain - prev 在两个 int64 量级极大、符号相反时会绕回
// 小数值（MaxInt64 - MinInt64 得 -1），于是下面 |delta| ≤ sanityDelta 的判定反而
// 通过，账本里多出一条金额荒谬的流水。取 2^53（float64 能精确表示的最大整数），
// 远超任何真实积分规模。
const maxBalance int64 = 1 << 53

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
	// seq 内存账本的单调变更序号：每次追加流水 +1。落盘期间若 seq 变化，说明
	// 快照之后又来了新流水（可能同时触发滚动裁剪，len 不变），不能清脏。
	seq uint64
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

	// safefile.Read 在正文不可解析时回落 .bak：坏文件按空账本继续、下一次 Flush
	// 又把它彻底覆盖，账本就永久丢了。首次运行/文件缺失仍是「从空账本开始」。
	raw, err := safefile.Read(l.path, json.Valid)
	if err != nil {
		// 最严重的「正文与 .bak 都坏」原先无声返回：账本随后从空开始、第一次
		// Flush 就覆盖正文，用户既丢流水也无从知道原因。
		if !errors.Is(err, fs.ErrNotExist) {
			log.Printf("WARN: [credithist] 账本不可用（按空账本继续）: %v", err)
		}

		return
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

	// 载入即裁剪：旧配置（更小的上限）留下的超大文件会整份驻留到第一次 Observe。
	l.trimLocked()
}

// Observe 记录一次余额观测：与上次比有变化才记流水。
// 首次观测只建基线（避免凭空一条 +N）；|delta| 超过 sanityDelta 时丢弃并重建基线。
func (l *Ledger) Observe(uid string, remain int64, note string) {
	if uid == "" {
		return
	}

	// 超出合理量级的余额是脏数据：不记流水，也不更新基线（等下一次合理值来比）。
	if remain > maxBalance || remain < -maxBalance {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	prev, seen := l.last[uid]
	l.last[uid] = remain

	// 首次观测只建基线；上一次基线本身是脏数据（历史遗留/旧版本写入）时同理：
	// 差值无意义，且与极端值相减会溢出。
	if !seen || prev > maxBalance || prev < -maxBalance {
		return
	}

	delta := remain - prev
	if delta == 0 {
		return
	}

	if delta > sanityDelta || delta < -sanityDelta {
		// 脏数据：不记流水（否则一条异常值会污染整本账），只更新基线。
		return
	}

	l.entries = append(l.entries, Entry{Time: time.Now(), UID: uid, Delta: delta, Remain: remain, Note: note})
	l.seq++
	l.trimLocked()
	l.dirty = true
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

	// 锁内只克隆快照，序列化放锁外：json.Marshal 最多 5000 条，持锁做 CPU 工作会
	// 让并发 Observe（余额刷新）排在整个序列化后面。必须深拷贝切片头——滚动裁剪
	// 会重切底层数组，只拷切片头会让锁外读到被复用的元素。
	entries := slices.Clone(l.entries)
	flushedSeq := l.seq
	l.mu.Unlock()

	raw, err := json.Marshal(fileDoc{Version: 1, Entries: entries})
	if err != nil {
		log.Printf("WARN: [credithist] 序列化失败: %v", err)

		return
	}

	// 走 safefile：唯一临时名 + 文件 fsync + rename + 目录 fsync + 失败清理。
	// 固定 ".tmp" 名在 server 与 cmd/credit 同时运行时会被互相覆盖，且无 fsync
	// 时掉电可能发布零长账本。
	//
	// 账本目录此前只由池状态落盘顺带建出：首次余额观测早于首次状态落盘时，
	// CreateTemp 因目录不存在而失败，这批流水只剩一条 WARN。
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		log.Printf("WARN: [credithist] 创建目录失败: %v", err)

		return
	}
	// WriteKeep 保留上一份有效账本为 .bak（与 load 的回退口径配对）。
	if err := safefile.WriteKeep(l.path, raw, 0o600, json.Valid); err != nil {
		log.Printf("WARN: [credithist] 落盘失败: %v", err)

		return
	}

	l.markFlushed(flushedSeq)
}

// markFlushed 写盘成功后清脏：只有快照之后没有新流水（seq 未变）才清。
// 写盘失败路径不会调用它，因此脏标记天然保留，下一轮 Flush 会重试。
func (l *Ledger) markFlushed(flushedSeq uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.seq == flushedSeq {
		l.dirty = false
	}
}
