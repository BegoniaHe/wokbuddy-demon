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
	"os"
	pathpkg "path"
	"path/filepath"
	"sort"
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

var errSetRealmFailed = errors.New("SetRealm failed")

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
	TPM           int      `json:"tpm,omitempty"`            // 每分钟 token 数（按已完成请求累计）
	MaxConcurrent int      `json:"max_concurrent,omitempty"` // 最大并发请求数
	Models        []string `json:"models,omitempty"`         // 模型白名单（glob，可带 cn:/global: 前缀）
}

// limitRuntime 单把密钥的进程内限流状态（固定 1 分钟窗口）。
type limitRuntime struct {
	windowStart time.Time
	rpm         int
	tpm         int64
	inflight    int
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

	limMu  sync.Mutex
	limits map[string]*limitRuntime

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
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	if path != "" {
		if err := store.load(); err != nil {
			log.Printf("[apikey] 读取 %s 失败（从零开始）: %v", path, err)
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
				s.flush()
			}
		}
	}()
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
// 落盘失败时回滚内存变更并返回错误。
func (s *Store) Create(name string) (Record, string, error) {
	raw, rec, err := newRecord(name)
	if err != nil {
		return Record{}, "", err
	}

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

	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })

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
		s.limMu.Lock()
		rt := s.runtimeLocked(id, now)
		rt.tpm += total
		s.limMu.Unlock()

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

// flush 防抖落盘：持锁写盘，与 Create/Revoke/Delete 的落盘同序。若先快照再释放
// 锁写盘，旧快照会覆盖期间并发变更（内存有、磁盘无），且两个写共用 path+".tmp"
// 会相互截断导致文件损坏。落盘失败时保持 dirty，下次 tick 重试。
func (s *Store) flush() {
	if s == nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.path == "" || !s.dirty {
		return
	}

	if err := s.persistLocked(); err != nil {
		log.Printf("[apikey] 落盘失败: %v", err)
	}
}

// persistLocked 在持锁状态下原子写盘当前记录；成功清 dirty，失败由调用方决定回滚。
func (s *Store) persistLocked() error {
	if s.path == "" {
		s.dirty = false
		return nil
	}

	if err := writeRecords(s.path, s.records); err != nil {
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

		h, err := hex.DecodeString(s.records[i].Hash)
		if err != nil || len(h) != 32 {
			continue
		}

		keys = append(keys, httpauth.Key{ID: s.records[i].ID, Hash: h})
	}

	s.keys = keys
}

// Acquire 按命名密钥限制尝试进入一次请求。返回 release（必须 defer 调用）、
// 建议 Retry-After 与是否放行。未配置限制时直接放行（no-op release）。
// model 白名单不在此检查（请求体尚未解析），见 ModelAllowed。
func (s *Store) Acquire(id string) (release func(), retryAfter time.Duration, ok bool) {
	limits, found := s.limitsOf(id)
	if !found || !limits.enabled() {
		return func() {}, 0, true
	}

	now := time.Now()

	s.limMu.Lock()
	defer s.limMu.Unlock()

	limitRuntime2 := s.runtimeLocked(id, now)
	if retry, ok := allowLocked(limitRuntime2, limits); !ok {
		return nil, retry, false
	}

	limitRuntime2.rpm++
	limitRuntime2.inflight++

	return func() {
		s.limMu.Lock()
		if limitRuntime2.inflight > 0 {
			limitRuntime2.inflight--
		}
		s.limMu.Unlock()
	}, 0, true
}

// enabled 报告是否配置了任一限流维度。
func (l Limits) enabled() bool { return l.RPM > 0 || l.TPM > 0 || l.MaxConcurrent > 0 }

// allowLocked 检查并发/RPM/TPM，返回建议等待时长。
func allowLocked(limitRuntime2 *limitRuntime, limits Limits) (time.Duration, bool) {
	if limits.MaxConcurrent > 0 && limitRuntime2.inflight >= limits.MaxConcurrent {
		return time.Second, false
	}

	retry := time.Until(limitRuntime2.windowStart.Add(time.Minute))
	if limits.RPM > 0 && limitRuntime2.rpm >= limits.RPM {
		return retry, false
	}

	if limits.TPM > 0 && limitRuntime2.tpm >= int64(limits.TPM) {
		return retry, false
	}

	return 0, true
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

	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.records {
		if s.records[i].ID != id {
			continue
		}

		old := s.records[i].Realm

		s.records[i].Realm = realm
		if err := s.persistLocked(); err != nil {
			s.records[i].Realm = old
			return false, err
		}

		return true, nil
	}

	return false, nil
}

// UpdateLimits 替换一把密钥的限制（面板编辑用）。非法值在调用方校验。
func (s *Store) UpdateLimits(id string, limits Limits) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.records {
		if s.records[i].ID != id {
			continue
		}

		old := s.records[i].Limits

		s.records[i].Limits = limits
		if err := s.persistLocked(); err != nil {
			s.records[i].Limits = old
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

// runtimeLocked 取/初始化密钥运行时；窗口过期时重置计数。
func (s *Store) runtimeLocked(id string, now time.Time) *limitRuntime {
	limitRuntime2 := s.limits[id]
	if limitRuntime2 == nil {
		limitRuntime2 = &limitRuntime{}
		s.limits[id] = limitRuntime2
	}

	if limitRuntime2.windowStart.IsZero() || now.Sub(limitRuntime2.windowStart) >= time.Minute {
		limitRuntime2.windowStart = now
		limitRuntime2.rpm = 0
		limitRuntime2.tpm = 0
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
