// Package apikey 命名 API 密钥的持久化存储与基础用量计数。
//
// 定位：在保持 config.api_key 作默认（管理）密钥的前提下，允许面板签发多把
// 「命名密钥」分发给不同客户端，并支持逐把撤销与「按密钥看用量」。
//
// 设计约束：
//   - 只存 SHA-256 哈希与展示用前缀，明文仅在创建时返回一次，绝不落盘/回显；
//   - 鉴权比较复用 internal/httpauth 的常量时间原语（HashKey/Authenticate），
//     本包不自行实现比较；
//   - 落盘 data/api_keys.json（由 state_file 派生），原子替换 + 防抖刷新；
//   - 计数（请求数/token/last_used_at）为累计值，不建时间序列，也不涉及计费。
package apikey

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/netip"
	"os"
	pathpkg "path"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/httpauth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/safefile"
)

const (
	// fileVersion 落盘格式版本；旧版本缺字段按零值加载。
	fileVersion = 1
	// keyPrefix 命名密钥的可见前缀（与默认 api_key 的 "sk-" 区分）。
	keyPrefix = "sk-wb-"
	// prefixLen 列表展示保留的明文前缀长度（含 keyPrefix）。
	prefixLen = 12
	// rawBytes 随机密钥字节数（base64url 后约 32 字符，熵足够）。
	rawBytes = 24
	// flushInterval 计数 / last_used_at 的防抖落盘间隔。
	flushInterval = 30 * time.Second
)

var (
	errSetRealmFailed = errors.New("SetRealm failed")
	// ErrInvalidLimits 表示 Limits 校验失败（调用方应映射为 400，而不是落盘/500）。
	ErrInvalidLimits = errors.New("invalid limits")
	// ErrUnsupportedVersion 表示落盘文件的格式版本高于本程序支持的版本。
	ErrUnsupportedVersion = errors.New("unsupported keys file version")
)

// Record 一把命名密钥的持久化记录。Hash 为 SHA-256 十六进制串，绝不回显。
type Record struct {
	ID               string     `json:"id"`
	Name             string     `json:"name"`
	Prefix           string     `json:"prefix"`
	Hash             string     `json:"hash,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	LastUsedAt       *time.Time `json:"last_used_at,omitempty"`
	Revoked          bool       `json:"revoked,omitempty"`
	Requests         int64      `json:"requests"`
	PromptTokens     int64      `json:"prompt_tokens"`
	CompletionTokens int64      `json:"completion_tokens"`
	TotalTokens      int64      `json:"total_tokens"`
	Rejected         int64      `json:"rejected,omitempty"`
	Limits           Limits     `json:"limits,omitzero"`
	// Realm 出口绑定（issue #94）：空 = 不绑定（跟随模型前缀/全局），
	// cn / global = 固定该域。只影响 /v1/* 路由，不改变鉴权与管理面可见性。
	Realm string `json:"realm,omitempty"`
}

// Limits 一把命名密钥的可选限制；零值 = 不限。进程内计数，单实例语义。
type Limits struct {
	RPM           int      `json:"rpm,omitempty"`            // 每分钟请求数
	TPM           int      `json:"tpm,omitempty"`            // 每分钟 completion token 数（Decode/输出；不含 prompt）
	MaxConcurrent int      `json:"max_concurrent,omitempty"` // 最大并发请求数
	Models        []string `json:"models,omitempty"`         // 模型白名单（glob，可带 cn:/global: 前缀）
	// ExpiresAt 到期时刻（UTC）；nil = 永不过期。now >= ExpiresAt 即拒绝。
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// QuotaTokens 生命周期总 token 配额；0 = 不限。按 Record.TotalTokens 判定。
	QuotaTokens int64 `json:"quota_tokens,omitempty"`
	// IPAllowlist 来源 IP 白名单（裸 IP 或 CIDR）；空 = 不限。保存时归一化。
	IPAllowlist []string `json:"ip_allowlist,omitempty"`
}

// limitRuntime 单把密钥的进程内在途计数（只跟踪并发；RPM/TPM 改由 pace.go 的
// GCRA 账本整形，issue #448）。
type limitRuntime struct {
	inflight int
}

// file 落盘结构。
type file struct {
	Version int      `json:"version"`
	Keys    []Record `json:"keys"`
}

// Store 并发安全的命名密钥存储。
type Store struct {
	mu      sync.Mutex
	path    string
	records []Record
	// keys 校验快照（仅未撤销），随 records 变更重建；Authenticate 直接消费。
	keys  []httpauth.Key
	dirty bool
	// seq 记录变更序号（每次 records 变化递增），flush 据此判断快照是否仍然最新。
	seq uint64
	// writeMu 串行化写盘；flushedSeq 是已落盘序号，防旧快照覆盖新状态。
	writeMu    sync.Mutex
	flushedSeq uint64

	limMu  sync.Mutex
	limits map[string]*limitRuntime
	// pace 响应整形账本（issue #448）：RPM/TPM 的 GCRA 虚拟调度。自带锁
	// （paceLedger.mu），与 limMu 无包含关系：并发准入（Acquire 持 limMu）与
	// 整形拨账（Pace*/NoteEmitted 持 pace.mu）互不阻塞。密钥增删不触碰它——
	// Delete/Revoke 后下一次 Pace* 会因 stateOf 未命中而空操作，
	// 残留条目由 DropPacing 显式清理（客户端断开路径）。
	pace *paceLedger

	// loadErr 首次加载失败（除 fs.ErrNotExist 外的任何原因）。一旦置位就拒绝写盘：
	// 内存里的记录集合是空/不全的，整份写出会把磁盘上的真实密钥表覆盖成「只剩
	// 新建的那几把」——所有命名密钥客户端静默 401，面板显示 0 把，唯一线索是启动
	// 日志。读失败要么是暂时性 I/O 错误、要么是文件损坏，两种都不该用空集合替代。
	loadErr error

	started  atomic.Bool
	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

// New 创建存储并从 path 加载（path 为空 = 纯内存，不落盘，测试用）。
func New(path string) *Store {
	store := &Store{
		path:   path,
		limits: map[string]*limitRuntime{},
		pace:   newPaceLedger(),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	if path != "" {
		if err := store.load(); err != nil {
			// 记录并留痕但不致命：启动仍可用默认 api_key，命名密钥面转为只读
			// （见 persistLocked 的 loadErr 守卫）。
			store.loadErr = err
			log.Printf("WARN: [apikey] 读取 %s 失败，命名密钥转为只读（不落盘、不覆盖原文件）: %v", path, err)
		}
	}

	return store
}

// load 读取落盘文件并重建校验快照。
func (s *Store) load() error {
	raw, err := safefile.Read(s.path, nil)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}

		return fmt.Errorf("load: %w", err)
	}

	var file2 file
	if err := json.Unmarshal(raw, &file2); err != nil {
		return fmt.Errorf("load: %w", err)
	}

	// 更高版本的文件可能换了哈希算法/字段语义：按当前二进制解释会把「不认识」当成
	// 「没配置」（哈希解不出来 → 密钥静默失效），故拒绝加载并让上层转只读。
	if file2.Version > fileVersion {
		return fmt.Errorf("load: 文件格式版本 %d 高于本程序支持的 %d（请升级后再启动）: %w",
			file2.Version, fileVersion, ErrUnsupportedVersion)
	}

	// 落盘记录要重跑一遍 Limits 归一化：Create/UpdateLimits 的全部约束（RPM/TPM
	// 上界、UTC 过期时间、IP 白名单与模型模式归一）此前只在写入路径执行，重启后
	// 手改或旧版本写出的值会原样进入内存——例如 rpm 超过 maxPacingRate 时
	// time.Minute/rpm 归零，限速静默消失。
	for i := range file2.Keys {
		normalized, err := NormalizeLimits(file2.Keys[i].Limits)
		if err != nil {
			log.Printf("WARN: [apikey] 密钥 %s 的 limits 非法（保持原值，限速/配额可能不生效）: %v",
				file2.Keys[i].ID, err)

			continue
		}

		file2.Keys[i].Limits = normalized
	}

	s.mu.Lock()
	s.records = file2.Keys
	s.rebuildKeysLocked()
	s.mu.Unlock()

	if n := len(file2.Keys); n > 0 {
		log.Printf("[apikey] 已恢复 %d 把命名密钥（%s）", n, s.path)
	}

	return nil
}

// Start 启动后台防抖落盘；Stop 前一直运行。
func (s *Store) Start() {
	if s == nil || !s.started.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer close(s.done)

		t := time.NewTicker(flushInterval)
		defer t.Stop()

		for {
			select {
			case <-s.stop:
				s.flush()
				return
			case <-t.C:
				s.flushGuarded()
			}
		}
	}()
}

// flushGuarded 定时落盘，panic 就地隔离。
//
// 循环里跑的是 JSON 编码与原子写盘；一次 panic 会终结整个协程（进而整进程），
// 命名密钥的计数与撤销状态就此停止落盘，只能靠重启发现。
func (s *Store) flushGuarded() {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("ERR: [apikey] 落盘循环 panic（已隔离）: %v\n%s", rec, debug.Stack())
		}
	}()

	s.flush()
}

// Stop 停止后台循环并做最后一次落盘（未 Start 时只做一次落盘）。
func (s *Store) Stop() {
	if s == nil {
		return
	}

	s.stopOnce.Do(func() { close(s.stop) })

	if s.started.Load() {
		<-s.done
		return
	}

	s.flush()
}

// Create 生成并持久化一把新命名密钥，返回记录（不含哈希）与仅此一次的明文。
// 可选 Limits 在写入前统一校验/归一化；非法值返回 ErrInvalidLimits，绝不落盘。
// 落盘失败时回滚内存变更并返回错误。
func (s *Store) Create(name string, limits ...Limits) (Record, string, error) {
	var normalized Limits

	if len(limits) > 0 {
		var err error
		if normalized, err = NormalizeLimits(limits[0]); err != nil {
			return Record{}, "", err
		}
	}

	raw, rec, err := newRecord(name)
	if err != nil {
		return Record{}, "", err
	}

	rec.Limits = normalized

	s.mu.Lock()
	defer s.mu.Unlock()

	s.records = append(s.records, rec)
	if err := s.persistLocked(); err != nil {
		s.records = s.records[:len(s.records)-1]
		return Record{}, "", err
	}

	s.rebuildKeysLocked()

	out := rec
	out.Hash = ""

	return out, raw, nil
}

// List 返回全部记录（按创建时间倒序），哈希字段已清空。
func (s *Store) List() []Record {
	if s == nil {
		return nil
	}

	s.mu.Lock()
	out := make([]Record, len(s.records))
	copy(out, s.records)
	s.mu.Unlock()

	for i := range out {
		out[i].Hash = ""
	}

	slices.SortStableFunc(out, func(a, b Record) int { return b.CreatedAt.Compare(a.CreatedAt) })

	return out
}

// Revoke 软撤销一把密钥（保留记录与计数）。已撤销或不存在返回 (false, nil)。
func (s *Store) Revoke(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.records {
		if s.records[i].ID != id {
			continue
		}

		if s.records[i].Revoked {
			return false, nil
		}

		s.records[i].Revoked = true
		if err := s.persistLocked(); err != nil {
			s.records[i].Revoked = false
			return false, err
		}

		s.rebuildKeysLocked()

		return true, nil
	}

	return false, nil
}

// Delete 物理删除一把密钥。落盘失败时回滚内存变更。
func (s *Store) Delete(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.records {
		if s.records[i].ID != id {
			continue
		}

		removed := s.records[i]

		s.records = append(s.records[:i], s.records[i+1:]...)
		if err := s.persistLocked(); err != nil {
			s.records = append(s.records, Record{})
			copy(s.records[i+1:], s.records[i:])
			s.records[i] = removed

			return false, err
		}

		s.rebuildKeysLocked()
		// 运行时账本随记录一起回收：limits 只在 runtimeLocked 里增，原先全包无
		// 任何 delete，删掉的密钥其并发计数/窗口状态会永久留在 map 里。
		s.limMu.Lock()
		delete(s.limits, id)
		s.limMu.Unlock()

		return true, nil
	}

	return false, nil
}

// Keys 返回当前未撤销密钥的校验快照（nil 安全；调用方只读）。
func (s *Store) Keys() []httpauth.Key {
	if s == nil {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.keys
}

// RecordUsage 累计一把密钥的一次成功鉴权请求（含 token）。last_used_at 同步刷新。
//
// 只写持久计数（Record 字段）：TPM 整形账本由 NoteEmitted 在流中增量推进
// （issue #448），不在这里补——请求结束才记账会把「已见 token」算晚，
// 快模型在记账前已把多轮并发槽周转完。
func (s *Store) RecordUsage(id string, prompt, completion, total int64) {
	if s == nil || id == "" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.records {
		if s.records[i].ID != id {
			continue
		}

		r := &s.records[i]
		r.Requests++
		r.PromptTokens += prompt
		r.CompletionTokens += completion
		r.TotalTokens += total
		now := time.Now()
		r.LastUsedAt = &now
		s.dirty = true
		// seq 必须一起递增：flush 靠「写盘期间 seq 有没有变」决定能不能清 dirty，
		// 计数变动若不动 seq，flush 会在写盘后把这次计数当成已落盘而丢掉。
		s.seq++

		return
	}
}

// Flush 立即落盘（关闭前或面板触发）。
func (s *Store) Flush() {
	if s == nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.persistLocked(); err != nil {
		log.Printf("[apikey] 落盘失败: %v", err)
	}
}

// flush 防抖落盘：锁内只取记录快照，序列化与写盘（含 fsync）在锁外进行。
//
// 原先持 s.mu 全程写盘，而鉴权路径（Keys/Authenticate）也要拿同一把锁——一次
// fsync 抖动就会让全部在线请求排在锁上。快照落盘的代价是要防「旧快照覆盖新状态」：
//   - writeMu 串行化写入，避免两次 rename 乱序；
//   - flushedSeq 记录已落盘序号，snapshot 序号不大于它时直接丢弃，保证磁盘状态
//     单调前进（提交期并发 Delete 写出的新状态不会被旧快照盖回去）；
//   - dirty 只在「写盘期间没有新变更」时清零，否则下一 tick 重试。
//
//nolint:revive // flush/Flush 为公开与内部变体，命名有意成对。
func (s *Store) flush() {
	if s == nil {
		return
	}

	s.mu.Lock()
	if s.path == "" || !s.dirty {
		s.mu.Unlock()
		return
	}

	snapshot := append([]Record(nil), s.records...)
	seq := s.seq
	s.mu.Unlock()

	if err := s.persistSnapshot(snapshot, seq); err != nil {
		log.Printf("[apikey] 落盘失败: %v", err)

		return
	}

	s.mu.Lock()
	if s.seq == seq {
		s.dirty = false
	}
	s.mu.Unlock()
}

// persistSnapshot 写入一份记录快照（锁外调用，writeMu 串行化）。
// 快照序号不大于已落盘序号时直接丢弃：说明期间已有更新的状态落盘。
func (s *Store) persistSnapshot(records []Record, seq uint64) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	if seq <= s.flushedSeq {
		return nil
	}

	if err := writeRecords(s.path, records); err != nil {
		return err
	}

	s.flushedSeq = seq

	return nil
}

// persistLocked 在持锁状态下原子写盘当前记录；成功清 dirty，失败由调用方决定回滚。
// persistLocked 在持锁状态下把当前记录落盘；失败由调用方回滚内存变更。
//
// 这里仍在 s.mu 内完成写入（含 fsync）：Create/Revoke/Delete/updateRecord 需要
// 「写盘结果 → 回滚」的同步语义。它们是面板单次操作，不在请求热路径上；热路径的
// 定期计数落盘走 flush，锁外写盘。
//
// 序号递增 + writeMu 串行化：与并发 flush 的旧快照竞争时，快照序号比较保证磁盘
// 状态单调前进，本次（较新）写入不会被旧快照覆盖，反之亦然。
func (s *Store) persistLocked() error {
	if s.path == "" {
		s.dirty = false
		return nil
	}

	if s.loadErr != nil {
		return fmt.Errorf("apikey: 状态文件加载失败，拒绝覆盖（修复或删除 %s 后重启）: %w", s.path, s.loadErr)
	}

	s.seq++
	seq := s.seq
	records := append([]Record(nil), s.records...)

	if err := s.persistSnapshot(records, seq); err != nil {
		return err
	}

	s.dirty = false

	return nil
}

// writeRecords 原子写盘记录集合（tmp + rename，0600）。
func writeRecords(path string, records []Record) error {
	raw, err := json.MarshalIndent(file{Version: fileVersion, Keys: records}, "", "  ")
	if err != nil {
		return fmt.Errorf("writeRecords: %w", err)
	}

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("writeRecords: %w", err)
		}
	}

	if err := safefile.WriteKeep(path, raw, 0o600, nil); err != nil {
		return fmt.Errorf("writeRecords: %w", err)
	}

	return nil
}

// rebuildKeysLocked 由 records 重建未撤销密钥的校验快照。
func (s *Store) rebuildKeysLocked() {
	keys := make([]httpauth.Key, 0, len(s.records))
	for i := range s.records {
		if s.records[i].Revoked {
			continue
		}

		hash, err := hex.DecodeString(s.records[i].Hash)
		if err != nil || len(hash) != 32 {
			// 静默跳过会让该密钥的客户端恒 401、面板却仍显示生效：留一条可定位的
			// WARN（只打 ID 与长度，不打哈希本身）。
			log.Printf("WARN: [apikey] 密钥 %s 的 hash 非法（长度 %d，期望 64 hex），该密钥不参与鉴权",
				s.records[i].ID, len(s.records[i].Hash))

			continue
		}

		keys = append(keys, httpauth.Key{ID: s.records[i].ID, Hash: hash})
	}

	s.keys = keys
}

// RejectReason 命名密钥准入拒绝原因。调用方按枚举决定状态码，不解析字符串。
type RejectReason int

const (
	RejectNone RejectReason = iota
	// RejectExpired 密钥已过期（401 invalid_api_key）。
	RejectExpired
	// RejectIPNotAllowed 来源 IP 不在白名单（403 ip_not_allowed）。
	RejectIPNotAllowed
	// RejectQuotaExhausted 生命周期 token 配额耗尽（429 insufficient_quota）。
	RejectQuotaExhausted
	// RejectRateLimited RPM/TPM 超限（429 + Retry-After）。
	RejectRateLimited
	// RejectConcurrency 并发超限（429 + Retry-After）。
	RejectConcurrency
)

// Acquire 按命名密钥限制尝试进入一次请求。返回 release（放行时必须 defer 调用）、
// 建议 Retry-After 与拒绝原因。检查顺序固定：过期 → IP 白名单 → 总量配额 → 并发。
//
// RPM/TPM **不再是准入拒绝**（issue #448）：超速请求照常放行，由 PaceRequest /
// PaceBoth 在写出侧按 GCRA 账本整形（首帧立即、结束标记择晚）。固定窗口拒绝
// 只会把客户端的重试与窗口清零后的突发堆到上游账号上，适得其反。
//
// 总量配额是准入时 best-effort：max_concurrent 条在途请求仍可能各消耗 token，
// 因此允许有限超额，不做 token 预留。model 白名单不在此检查（请求体尚未解析），
// 见 ModelAllowed。
func (s *Store) Acquire(id, ip string) (release func(), retryAfter time.Duration, reason RejectReason) {
	limits, totalTokens, found := s.stateOf(id)
	if !found {
		return func() {}, 0, RejectNone
	}

	now := time.Now()

	if reason := checkKeyAdmission(limits, totalTokens, ip, now); reason != RejectNone {
		return nil, 0, reason
	}

	if limits.MaxConcurrent <= 0 {
		return func() {}, 0, RejectNone
	}

	s.limMu.Lock()
	defer s.limMu.Unlock()

	limitRuntime2 := s.runtimeLocked(id)
	if limitRuntime2.inflight >= limits.MaxConcurrent {
		return nil, time.Second, RejectConcurrency
	}

	limitRuntime2.inflight++

	return func() {
		s.limMu.Lock()
		if limitRuntime2.inflight > 0 {
			limitRuntime2.inflight--
		}
		s.limMu.Unlock()
	}, 0, RejectNone
}

// checkKeyAdmission 检查不依赖进程内窗口的准入维度：过期 → IP 白名单 → 总量配额。
//
//nolint:gocritic // 值语义的 Limits，与 Acquire/NormalizeLimits 保持一致。
func checkKeyAdmission(limits Limits, totalTokens int64, ip string, now time.Time) RejectReason {
	if limits.ExpiresAt != nil && !now.Before(*limits.ExpiresAt) {
		return RejectExpired
	}

	if len(limits.IPAllowlist) > 0 && !ipAllowed(limits.IPAllowlist, ip) {
		return RejectIPNotAllowed
	}

	if limits.QuotaTokens > 0 && totalTokens >= limits.QuotaTokens {
		return RejectQuotaExhausted
	}

	return RejectNone
}

// ModelAllowed 报告模型是否命中密钥白名单（未配置 = 全部允许）。
// 模式支持 glob 与可选 realm 前缀；未带 realm 的模式匹配任意域。
func (s *Store) ModelAllowed(id, model string) bool {
	limits, found := s.limitsOf(id)
	if !found || len(limits.Models) == 0 {
		return true
	}

	modelRealm, modelBare := splitRealmName(model)
	if modelRealm == "" {
		modelRealm = "cn" // 裸模型名默认按 cn 路由（与 server.resolveModel 同口径）
	}

	for _, pattern := range limits.Models {
		wantRealm, wantBare := splitRealmName(pattern)
		if wantRealm != "" && wantRealm != modelRealm {
			continue
		}

		if match, err := pathpkg.Match(wantBare, modelBare); err == nil && match {
			return true
		}
	}

	return false
}

// Realm 返回一把密钥的出口绑定（空 = 不绑定；不存在/已撤销返回空）。
func (s *Store) Realm(id string) string {
	if s == nil || id == "" {
		return ""
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.records {
		if s.records[i].ID == id && !s.records[i].Revoked {
			return s.records[i].Realm
		}
	}

	return ""
}

// SetRealm 更新一把密钥的出口绑定（issue #94）。realm 只接受 ""/cn/global，
// 变更即时生效（下一个请求读取）；落盘失败回滚。
func (s *Store) SetRealm(id, realm string) (bool, error) {
	switch realm {
	case "", "cn", "global":
	default:
		return false, fmt.Errorf("realm 必须是空、cn 或 global，当前 %q: %w", realm, errSetRealmFailed)
	}

	return s.updateRecord(id, func(rec *Record) func() {
		old := rec.Realm
		rec.Realm = realm

		return func() { rec.Realm = old }
	})
}

// UpdateLimits 替换一把密钥的限制（面板编辑用）。统一在包内校验/归一化；
// 非法值返回 ErrInvalidLimits，绝不落盘。
//
//nolint:gocritic // 值语义的 Limits 便于调用方以字面量表达「零 = 不限」。
func (s *Store) UpdateLimits(id string, limits Limits) (bool, error) {
	normalized, err := NormalizeLimits(limits)
	if err != nil {
		return false, err
	}

	return s.updateRecord(id, func(rec *Record) func() {
		old := rec.Limits
		rec.Limits = normalized

		return func() { rec.Limits = old }
	})
}

// updateRecord 在锁内修改指定记录并落盘；落盘失败调用 undo 回滚。
func (s *Store) updateRecord(id string, apply func(*Record) func()) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.records {
		if s.records[i].ID != id {
			continue
		}

		undo := apply(&s.records[i])
		if err := s.persistLocked(); err != nil {
			undo()

			return false, err
		}

		return true, nil
	}

	return false, nil
}

// RecordRejected 累计一次被限制拒绝的请求（面板展示）。
func (s *Store) RecordRejected(id string) {
	if s == nil || id == "" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.records {
		if s.records[i].ID == id {
			s.records[i].Rejected++
			s.dirty = true
			s.seq++

			return
		}
	}
}

// limitsOf 读取记录限制（返回 false 表示密钥不存在）。
func (s *Store) limitsOf(id string) (Limits, bool) {
	if s == nil {
		return Limits{RPM: 0, TPM: 0, MaxConcurrent: 0, Models: nil}, false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.records {
		if s.records[i].ID == id && !s.records[i].Revoked {
			return s.records[i].Limits, true
		}
	}

	return Limits{RPM: 0, TPM: 0, MaxConcurrent: 0, Models: nil}, false
}

// stateOf 读取记录限制与已累计 token（返回 false 表示不存在/已撤销）。
func (s *Store) stateOf(id string) (Limits, int64, bool) {
	if s == nil {
		return Limits{}, 0, false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.records {
		if s.records[i].ID == id && !s.records[i].Revoked {
			return s.records[i].Limits, s.records[i].TotalTokens, true
		}
	}

	return Limits{}, 0, false
}

// maxPacingRate RPM/TPM 的允许上界（间隔 ≥1µs）。
const maxPacingRate = 60_000_000

// NormalizeLimits 校验并归一化一把密钥的限制；零值保持「不限」：
//   - expires_at 转 UTC；
//   - quota_tokens 负数拒绝；
//   - ip_allowlist 解析裸 IP / CIDR，IPv4-mapped 归一，CIDR 取 Masked，去重；
//   - models 去空白、去重并校验 glob。
//
//nolint:gocritic // 同上：值语义避免调用方关心生命周期。
func NormalizeLimits(limits Limits) (Limits, error) {
	if limits.RPM < 0 || limits.TPM < 0 || limits.MaxConcurrent < 0 {
		return Limits{}, fmt.Errorf("rpm/tpm/max_concurrent 不能为负: %w", ErrInvalidLimits)
	}
	// 上界：GCRA 的间隔是 time.Minute / rpm，取值过大时整除得 0，TAT 永不前进，
	// 限速静默失效（用户以为配了限速，实际完全不生效）。1e6/分钟 已是任何真实
	// 客户端都到不了的水位，超过按非法拒绝而不是悄悄关掉整形。
	if limits.RPM > maxPacingRate || limits.TPM > maxPacingRate {
		return Limits{}, fmt.Errorf("rpm/tpm 不能超过 %d（间隔会归零、限速失效）: %w",
			maxPacingRate, ErrInvalidLimits)
	}

	if limits.QuotaTokens < 0 {
		return Limits{}, fmt.Errorf("quota_tokens 不能为负（0 = 不限）: %w", ErrInvalidLimits)
	}

	if limits.ExpiresAt != nil {
		utc := limits.ExpiresAt.UTC()
		limits.ExpiresAt = &utc
	}

	ips, err := normalizeIPAllowlist(limits.IPAllowlist)
	if err != nil {
		return Limits{}, err
	}

	limits.IPAllowlist = ips

	models, err := normalizeModelPatterns(limits.Models)
	if err != nil {
		return Limits{}, err
	}

	limits.Models = models

	return limits, nil
}

// normalizeIPAllowlist 归一化 IP 白名单：接受裸 IP 或 CIDR（IPv4/IPv6）。
func normalizeIPAllowlist(raw []string) ([]string, error) {
	out := make([]string, 0, len(raw))
	seen := make(map[string]bool, len(raw))

	for _, item := range raw {
		value := strings.TrimSpace(item)
		if value == "" {
			continue
		}

		canonical, err := normalizeIPPattern(value)
		if err != nil {
			return nil, err
		}

		if seen[canonical] {
			continue
		}

		seen[canonical] = true
		out = append(out, canonical)
	}

	if len(out) == 0 {
		return nil, nil
	}

	return out, nil
}

// normalizeIPPattern 归一化单个 IP / CIDR 模式。
func normalizeIPPattern(value string) (string, error) {
	if strings.Contains(value, "/") {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			return "", fmt.Errorf("ip_allowlist[%q]: 非法 CIDR: %w: %w", value, err, ErrInvalidLimits)
		}

		prefix, err = normalizePrefix(prefix, value)
		if err != nil {
			return "", err
		}

		return prefix.String(), nil
	}

	addr, err := netip.ParseAddr(value)
	if err != nil {
		return "", fmt.Errorf("ip_allowlist[%q]: 非法 IP: %w: %w", value, err, ErrInvalidLimits)
	}

	return addr.Unmap().String(), nil
}

// normalizePrefix 把 CIDR 归一为 Masked 形式；IPv4-mapped 前缀转成 IPv4。
func normalizePrefix(prefix netip.Prefix, raw string) (netip.Prefix, error) {
	prefix = prefix.Masked()
	if !prefix.Addr().Is4In6() {
		return prefix, nil
	}

	const mappedPrefixBits = 96
	if prefix.Bits() < mappedPrefixBits {
		return netip.Prefix{}, fmt.Errorf(
			"ip_allowlist[%q]: IPv4-mapped CIDR 前缀过宽: %w", raw, ErrInvalidLimits,
		)
	}

	return netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()-mappedPrefixBits), nil
}

// ipAllowed 报告来源 IP 是否命中白名单（白名单已归一化）。
func ipAllowed(patterns []string, rawIP string) bool {
	addr, err := netip.ParseAddr(strings.TrimSpace(rawIP))
	if err != nil {
		return false
	}

	addr = addr.Unmap()

	for _, pattern := range patterns {
		if strings.Contains(pattern, "/") {
			prefix, err := netip.ParsePrefix(pattern)
			if err != nil {
				continue
			}

			if prefix.Contains(addr) {
				return true
			}

			continue
		}

		want, err := netip.ParseAddr(pattern)
		if err != nil {
			continue
		}

		if want.Unmap() == addr {
			return true
		}
	}

	return false
}

// normalizeModelPatterns 去空白/去重并校验 glob 语法；空列表返回 nil。
func normalizeModelPatterns(raw []string) ([]string, error) {
	seen := make(map[string]bool, len(raw))

	out := make([]string, 0, len(raw))
	for _, item := range raw {
		pat := strings.TrimSpace(item)
		if pat == "" {
			continue
		}

		bare := pat
		if i := strings.IndexByte(pat, ':'); i >= 0 && (pat[:i] == "cn" || pat[:i] == "global") {
			bare = pat[i+1:]
		}

		if bare == "" {
			return nil, fmt.Errorf("模型 glob %q 缺少模型名: %w", item, ErrInvalidLimits)
		}

		if _, err := pathpkg.Match(bare, "x"); err != nil {
			return nil, fmt.Errorf("非法模型 glob %q: %w: %w", item, err, ErrInvalidLimits)
		}

		if seen[pat] {
			continue
		}

		seen[pat] = true
		out = append(out, pat)
	}

	if len(out) == 0 {
		return nil, nil
	}

	return out, nil
}

// runtimeLocked 取/初始化密钥的并发运行时（持 limMu 调用）。
func (s *Store) runtimeLocked(id string) *limitRuntime {
	limitRuntime2 := s.limits[id]
	if limitRuntime2 == nil {
		limitRuntime2 = &limitRuntime{}
		s.limits[id] = limitRuntime2
	}

	return limitRuntime2
}

// splitRealmName 拆出可选 realm 前缀（仅识别 cn/global），无前缀时 realm 为空。
func splitRealmName(name string) (realm, bare string) {
	prefix, rest, ok := strings.Cut(name, ":")
	if !ok || (prefix != "cn" && prefix != "global") {
		return "", name
	}

	return prefix, rest
}

// newRecord 生成一把随机命名密钥及其记录（含哈希与展示前缀）。
func newRecord(name string) (string, Record, error) {
	buf := make([]byte, rawBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", Record{}, fmt.Errorf("gen key: %w", err)
	}

	idb := make([]byte, 8)
	if _, err := rand.Read(idb); err != nil {
		return "", Record{}, fmt.Errorf("gen key id: %w", err)
	}

	raw := keyPrefix + base64.RawURLEncoding.EncodeToString(buf)

	name = strings.TrimSpace(name)
	if name == "" {
		name = "API key"
	}

	prefix := raw
	if len(prefix) > prefixLen {
		prefix = prefix[:prefixLen]
	}

	rec := Record{
		ID:        hex.EncodeToString(idb),
		Name:      name,
		Prefix:    prefix,
		Hash:      hex.EncodeToString(httpauth.HashKey(raw)),
		CreatedAt: time.Now(),
	}

	return raw, rec, nil
}
