// audit.go 管理面写操作审计：追加 JSONL、按大小轮转、提供最近记录查询。
//
// 为什么单独做：面板的写操作（改配置、删账号、吊销密钥、导入导出）此前只写
// 进程日志与 500 条环形缓冲——重启即丢，且没有来源 IP / 结果状态，出事无法
// 追溯。审计只记元数据（方法、路径、状态、来源），**不记请求体与凭证**。
package panel

import (
	"bufio"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/httpauth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/logfmt"
)

const (
	// auditFileMaxBytes 单文件上限；超过后轮转为 audit.jsonl.1 ... .N。
	auditFileMaxBytes = 8 << 20
	// auditBackups 保留的历史文件数（不含当前文件）。总上限约 40 MiB。
	auditBackups = 4
	// auditMaxLineBytes 单条记录上限（含换行），防止异常字段撑大文件；
	// 与 readAuditFile 的 scanner 缓冲上限同值，保证能读回自己写入的每一条。
	auditMaxLineBytes = 64 << 10
)

// auditEntry 一条管理面审计记录。字段刻意保持扁平、可 grep。
type auditEntry struct {
	Time       string `json:"time"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	Pattern    string `json:"pattern,omitempty"`
	Status     int    `json:"status"`
	OK         bool   `json:"ok"`
	IP         string `json:"ip,omitempty"`
	UserAgent  string `json:"user_agent,omitempty"`
	AuthFailed bool   `json:"auth_failed,omitempty"`
	// Panic 标记 handler 发生 panic（未写出响应头时 Status 记为 500；已写出
	// 的响应仍按实际状态记录）。审计不能因为 panic 跳过：部分写操作可能已
	// 生效，缺记录会留下无法追溯的变更。
	Panic bool `json:"panic,omitempty"`
}

// auditLog 并发安全的追加写入器。零值不可用；用 newAuditLog 构造。
// dir 为空时返回 nil，所有方法 nil 安全（测试或未配置目录时直接跳过）。
type auditLog struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	backups  int
	size     int64
	now      func() time.Time
}

func newAuditLog(dir string) *auditLog {
	if strings.TrimSpace(dir) == "" {
		return nil
	}

	return &auditLog{
		path:     filepath.Join(dir, "audit.jsonl"),
		maxBytes: auditFileMaxBytes,
		backups:  auditBackups,
		now:      time.Now,
	}
}

// Append 写一条审计记录。失败只记进程日志，绝不影响请求本身。
func (a *auditLog) Append(entry *auditEntry) {
	if a == nil || entry == nil {
		return
	}

	line, ok := a.encode(entry)
	if !ok {
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	a.appendLocked(line)
}

// encode 生成一行 JSON（含换行）。超长记录直接丢弃，不写半截 JSON。
func (a *auditLog) encode(entry *auditEntry) ([]byte, bool) {
	if entry.Time == "" {
		entry.Time = a.now().UTC().Format(time.RFC3339)
	}

	line, err := json.Marshal(entry)
	if err != nil {
		return nil, false
	}

	// 换行也算进上限：否则恰好写满上限的记录会超出 scanner 缓冲，
	// 读取时该行报错并中断，文件后半段全部不可见。
	if len(line)+1 > auditMaxLineBytes {
		// 字段都由本包截断/生成，正常不会触发；宁可丢一条也不写半截 JSON。
		log.Printf("panel: 审计记录超长已丢弃（%d 字节）", len(line))

		return nil, false
	}

	return append(line, '\n'), true
}

// appendLocked 追加一行并按需轮转（调用方已持有锁）。
func (a *auditLog) appendLocked(line []byte) {
	if mkdirErr := os.MkdirAll(filepath.Dir(a.path), 0o755); mkdirErr != nil {
		log.Printf("panel: 审计目录创建失败: %v", mkdirErr)

		return
	}

	if a.size > 0 && a.size+int64(len(line)) > a.maxBytes {
		a.rotateLocked()
	}

	file, err := os.OpenFile(a.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		log.Printf("panel: 审计文件打开失败: %v", err)

		return
	}
	defer file.Close()

	written, err := file.Write(line)
	a.size += int64(written)

	if err != nil {
		log.Printf("panel: 审计写入失败: %v", err)

		return
	}

	// 审计的保留价值在于崩溃后可追溯：不 fsync 时掉电会丢掉最近的写操作记录，
	// 恰恰是最需要的那几条。
	if err := file.Sync(); err != nil {
		log.Printf("panel: 审计 fsync 失败: %v", err)
	}
}

// rotateLocked 当前文件 → .1，旧的依次后移，最老的丢弃。
func (a *auditLog) rotateLocked() {
	// 删除/后移失败都留痕：最老备份删不掉（权限、占位）会让后续 rename 覆盖失败，
	// 结果是最旧的归档一直占着位、新记录写满后轮转静默停摆。
	if err := os.Remove(a.path + "." + strconv.Itoa(a.backups)); err != nil && !os.IsNotExist(err) {
		log.Printf("panel: 审计备份删除失败: %v", err)
	}

	for i := a.backups - 1; i >= 1; i-- {
		from, to := a.path+"."+strconv.Itoa(i), a.path+"."+strconv.Itoa(i+1)
		if err := os.Rename(from, to); err != nil && !os.IsNotExist(err) {
			log.Printf("panel: 审计备份后移失败（.%d → .%d）: %v", i, i+1, err)
		}
	}

	if err := os.Rename(a.path, a.path+".1"); err != nil && !os.IsNotExist(err) {
		log.Printf("panel: 审计轮转失败: %v", err)
	}

	a.size = 0
}

// Recent 返回最多 limit 条记录，按时间倒序（最新在前）。
//
// 锁只用于取路径快照：读盘（最多 backups+1 个文件、单个上限 8 MiB）放在锁外。
// 文件只追加、轮转只用 os.Rename，读到的是某个一致的「已落盘前缀」，最多比内存
// 计数少几条，不影响审计语义；而把读盘放在锁内会让每次面板查询都阻塞所有写审计。
func (a *auditLog) Recent(limit int) []auditEntry {
	if a == nil || limit <= 0 {
		return []auditEntry{}
	}

	if limit > 1000 {
		limit = 1000
	}

	a.mu.Lock()
	paths := make([]string, 0, a.backups+1)
	paths = append(paths, a.path)

	for i := 1; i <= a.backups; i++ {
		paths = append(paths, a.path+"."+strconv.Itoa(i))
	}
	a.mu.Unlock()

	out := make([]auditEntry, 0, limit)

	for _, path := range paths {
		remaining := limit - len(out)
		if remaining <= 0 {
			break
		}

		// 只读尾部 remaining 条：/panel/api/audit 是 scopeRead 且无缓存，全量解析
		// 未裁剪的 5 个文件（合计约 40 MiB）对 readonly 轮询是纯浪费。
		entries := readAuditTail(path, remaining)
		for i := len(entries) - 1; i >= 0 && len(out) < limit; i-- {
			out = append(out, entries[i])
		}
	}

	return out
}

// readAuditTail 读取单个文件并只保留最后 want 条（环形缓冲），坏行跳过。
func readAuditTail(path string, want int) []auditEntry {
	if want <= 0 {
		return nil
	}

	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()

	ring := make([]auditEntry, want)
	count := 0

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64<<10), auditMaxLineBytes)

	for scanner.Scan() {
		var entry auditEntry
		if json.Unmarshal(scanner.Bytes(), &entry) != nil {
			continue
		}

		ring[count%want] = entry
		count++
	}

	n := min(count, want)
	out := make([]auditEntry, 0, n)

	for i := count - n; i < count; i++ {
		out = append(out, ring[i%want])
	}

	return out
}

// initAuditSize 启动时读取当前文件大小，避免第一次写入就误判轮转。
func (a *auditLog) initAuditSize() {
	if a == nil {
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if stat, err := os.Stat(a.path); err == nil {
		a.size = stat.Size()
	}
}

// auditEntryFor 由响应状态构造一条审计记录。panicked=true 且尚未写出响应头时
// 状态按 500 记；已写出 2xx 但随后 panic 的请求也不计为成功。
func (p *Panel) auditEntryFor(auditedWriter *auditResponseWriter, r *http.Request, panicked bool) *auditEntry {
	status := auditedWriter.status
	switch {
	case panicked && status == 0:
		status = http.StatusInternalServerError
	case status == 0:
		status = http.StatusOK
	}

	return &auditEntry{
		Method:     r.Method,
		Path:       r.URL.Path,
		Pattern:    r.Pattern,
		Status:     status,
		OK:         status < 400 && !panicked,
		IP:         httpauth.ClientIP(r, p.cfg.TrustedProxies),
		UserAgent:  logfmt.Truncate(r.UserAgent(), 200),
		AuthFailed: status == http.StatusUnauthorized || status == http.StatusForbidden,
		Panic:      panicked,
	}
}

// auditResponseWriter 记录响应状态码，供审计中间件使用。只包非流式管理 API，
// 但仍实现 Unwrap 以兼容 ResponseController。
type auditResponseWriter struct {
	w      http.ResponseWriter
	status int
}

func (w *auditResponseWriter) Header() http.Header { return w.w.Header() }

func (w *auditResponseWriter) WriteHeader(code int) {
	// 只转发第一次：重复调用会被 net/http 记为 "superfluous response.WriteHeader
	// call"，且第二次的码与客户端实际收到的语义不一致（审计里记的是第一次）。
	if w.status != 0 {
		return
	}

	w.status = code
	w.w.WriteHeader(code)
}

func (w *auditResponseWriter) Write(payload []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}

	return w.w.Write(payload)
}

func (w *auditResponseWriter) Unwrap() http.ResponseWriter { return w.w }

// auditList 返回最近的审计记录（最新在前）。审计未启用时返回空列表。
func (p *Panel) auditList(w http.ResponseWriter, r *http.Request) {
	limit := 200

	if value := r.URL.Query().Get("limit"); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil && parsed > 0 {
			limit = parsed
		}
	}

	if limit > 1000 {
		limit = 1000
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"entries": p.audit.Recent(limit),
		"limit":   limit,
	})
}
