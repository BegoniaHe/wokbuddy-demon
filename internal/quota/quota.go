// Package quota 记录「账号 × 模型」撞线事件并估算窗口预算（issue #101）。
//
// 只读分析：事件来自 429/6004 冷却处理点，预算按同 realm×模型的撞线样本反推；
// 不参与请求路径、不改变冷却语义。文件不含 prompt/响应正文。
package quota

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/atomicwrite"
)

// Event 一次撞线样本。
type Event struct {
	UID     string    `json:"uid"`
	Realm   string    `json:"realm"`
	Model   string    `json:"model"`
	ResetAt time.Time `json:"reset_at"`
	Used    int64     `json:"used"` // 窗口内成功 token 用量（自 resetAt-24h 起）
	At      time.Time `json:"at"`
}

// Budget 同 realm×模型的预算估算。
type Budget struct {
	Used    float64 `json:"used"`
	Samples int     `json:"samples"`
}

// Store 事件集合（内存 + JSONL 持久化）。
type Store struct {
	mu     sync.Mutex
	path   string
	events map[string]Event // key: uid|model|resetAt
}

const (
	eventRetention = 7 * 24 * time.Hour
	maxEvents      = 2048
)

// New 从 path 加载（空 path = 纯内存）。
func New(path string) *Store {
	s := &Store{path: path, events: map[string]Event{}}
	if path != "" {
		s.load()
	}

	return s
}

func (s *Store) load() {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return
	}

	for _, line := range splitLines(raw) {
		var e Event
		if json.Unmarshal(line, &e) != nil || e.UID == "" || e.Model == "" {
			continue
		}

		s.events[eventKey(&e)] = e
	}

	// 加载后立即按保留期/条数上限裁剪：文件里的旧事件不应在重启后短暂参与
	// 预算聚合，等下一次 Add 才被清掉。
	s.pruneLocked(time.Now())
}

// Add 记录一条撞线事件；同「账号+模型+重置时刻」重复事件被忽略。
func (s *Store) Add(e *Event) bool {
	if s == nil || e.UID == "" || e.Model == "" {
		return false
	}

	event := *e
	if event.At.IsZero() {
		event.At = time.Now()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	key := eventKey(&event)
	if _, exists := s.events[key]; exists {
		return false
	}

	s.events[key] = event
	s.pruneLocked(event.At)
	s.persistLocked()

	return true
}

// Events 返回事件快照。
func (s *Store) Events() []Event {
	if s == nil {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]Event, 0, len(s.events))
	for _, e := range s.events {
		out = append(out, e)
	}

	return out
}

// Budgets 按 realm×模型聚合：先按账号平均，再跨账号平均；样本数一并返回。
func (s *Store) Budgets() map[string]Budget {
	events := s.Events()
	perAccount := map[string]map[string]float64{} // modelKey -> uid -> sum
	counts := map[string]map[string]int{}

	for _, item := range events {
		key := item.Realm + "|" + item.Model
		if perAccount[key] == nil {
			perAccount[key] = map[string]float64{}
			counts[key] = map[string]int{}
		}

		perAccount[key][item.UID] += float64(item.Used)
		counts[key][item.UID]++
	}

	out := make(map[string]Budget, len(perAccount))
	for key, accounts := range perAccount {
		var sum float64

		samples := 0

		for uid, total := range accounts {
			sum += total / float64(counts[key][uid])
			samples += counts[key][uid]
		}

		out[key] = Budget{Used: sum / float64(len(accounts)), Samples: samples}
	}

	return out
}

// pruneLocked 删除超过保留期的旧事件，并在总数超限时丢弃最早的事件。
// 必须持有 s.mu。
func (s *Store) pruneLocked(now time.Time) {
	cutoff := now.Add(-eventRetention)
	for key, e := range s.events {
		if e.At.Before(cutoff) {
			delete(s.events, key)
		}
	}

	if len(s.events) <= maxEvents {
		return
	}

	type entry struct {
		key string
		at  time.Time
	}

	all := make([]entry, 0, len(s.events))
	for key, e := range s.events {
		all = append(all, entry{key, e.At})
	}

	sort.Slice(all, func(i, j int) bool { return all[i].at.Before(all[j].at) })

	for _, item := range all[:len(all)-maxEvents] {
		delete(s.events, item.key)
	}
}

func (s *Store) persistLocked() {
	if s.path == "" {
		return
	}

	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return
	}

	raw := make([]byte, 0, len(s.events)*128)
	for _, e := range s.events {
		line, err := json.Marshal(e)
		if err != nil {
			continue
		}

		raw = append(raw, line...)
		raw = append(raw, '\n')
	}

	tmp := s.path + ".tmp"
	if os.WriteFile(tmp, raw, 0o600) == nil {
		_ = atomicwrite.Rename(tmp, s.path)
	}
}

func eventKey(e *Event) string {
	return e.UID + "|" + e.Model + "|" + e.ResetAt.UTC().Format(time.RFC3339)
}

func splitLines(raw []byte) [][]byte {
	var out [][]byte

	start := 0

	for i, b := range raw {
		if b == '\n' {
			if i > start {
				out = append(out, raw[start:i])
			}

			start = i + 1
		}
	}

	if start < len(raw) {
		out = append(out, raw[start:])
	}

	return out
}
