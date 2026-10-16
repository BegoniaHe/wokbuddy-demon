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
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/atomicwrite"
	"github.com/BegoniaHe/wokbuddy-demon/internal/httpauth"
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

	started  atomic.Bool
	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

// New 创建存储并从 path 加载（path 为空 = 纯内存，不落盘，测试用）。
func New(path string) *Store {
	s := &Store{
		path: path,
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	if path != "" {
		if err := s.load(); err != nil {
			log.Printf("[apikey] 读取 %s 失败（从零开始）: %v", path, err)
		}
	}
	return s
}

// load 读取落盘文件并重建校验快照。
func (s *Store) load() error {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		return err
	}
	s.mu.Lock()
	s.records = f.Keys
	s.rebuildKeysLocked()
	s.mu.Unlock()
	if n := len(f.Keys); n > 0 {
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
		return err
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return atomicwrite.Rename(tmp, path)
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
