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
)

const (
	// auditFileMaxBytes 单文件上限；超过后轮转为 audit.jsonl.1 ... .N。
	auditFileMaxBytes = 8 << 20
	// auditBackups 保留的历史文件数（不含当前文件）。总上限约 40 MiB。
	auditBackups = 4
	// auditMaxLineBytes 单条记录上限，防止异常字段撑大文件。
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
func (a *auditLog) Append(e auditEntry) {
	if a == nil {
		return
	}
	if e.Time == "" {
		e.Time = a.now().UTC().Format(time.RFC3339)
	}
	line, err := json.Marshal(e)
	if err != nil {
		return
	}
	if len(line) > auditMaxLineBytes {
		// 字段都由本包截断/生成，正常不会触发；宁可丢一条也不写半截 JSON。
		log.Printf("panel: 审计记录超长已丢弃（%d 字节）", len(line))
		return
	}
	line = append(line, '\n')

	a.mu.Lock()
	defer a.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(a.path), 0o755); err != nil {
		log.Printf("panel: 审计目录创建失败: %v", err)
		return
	}
	if a.size > 0 && a.size+int64(len(line)) > a.maxBytes {
		a.rotateLocked()
	}
	f, err := os.OpenFile(a.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		log.Printf("panel: 审计文件打开失败: %v", err)
		return
	}
	defer f.Close()
	n, err := f.Write(line)
	a.size += int64(n)
	if err != nil {
		log.Printf("panel: 审计写入失败: %v", err)
	}
}

// rotateLocked 当前文件 → .1，旧的依次后移，最老的丢弃。
func (a *auditLog) rotateLocked() {
	_ = os.Remove(a.path + "." + strconv.Itoa(a.backups))
	for i := a.backups - 1; i >= 1; i-- {
		_ = os.Rename(a.path+"."+strconv.Itoa(i), a.path+"."+strconv.Itoa(i+1))
	}
	if err := os.Rename(a.path, a.path+".1"); err != nil && !os.IsNotExist(err) {
		log.Printf("panel: 审计轮转失败: %v", err)
	}
	a.size = 0
}

// Recent 返回最多 limit 条记录，按时间倒序（最新在前）。
func (a *auditLog) Recent(limit int) []auditEntry {
	if a == nil || limit <= 0 {
		return []auditEntry{}
	}
	if limit > 1000 {
		limit = 1000
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	paths := []string{a.path}
	for i := 1; i <= a.backups; i++ {
		paths = append(paths, a.path+"."+strconv.Itoa(i))
	}
	out := make([]auditEntry, 0, limit)
	for _, path := range paths {
		entries := readAuditFile(path)
		for i := len(entries) - 1; i >= 0 && len(out) < limit; i-- {
			out = append(out, entries[i])
		}
		if len(out) >= limit {
			break
		}
	}
	return out
}

// readAuditFile 读取单个文件；坏行跳过，不影响其余记录。
func readAuditFile(path string) []auditEntry {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []auditEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), auditMaxLineBytes)
	for sc.Scan() {
		var e auditEntry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
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
	if st, err := os.Stat(a.path); err == nil {
		a.size = st.Size()
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
	if w.status == 0 {
		w.status = code
	}
	w.w.WriteHeader(code)
}

func (w *auditResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.w.Write(p)
}

func (w *auditResponseWriter) Unwrap() http.ResponseWriter { return w.w }

// auditList 返回最近的审计记录（最新在前）。审计未启用时返回空列表。
func (p *Panel) auditList(w http.ResponseWriter, r *http.Request) {
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
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
