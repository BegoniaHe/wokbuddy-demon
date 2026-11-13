// Package logrotate 提供「按天分文件 + 单文件大小上限轮转 + 保留期/总容量裁剪」的
// 追加写入器，供请求归档（internal/reqlog）与运行日志落盘（internal/runlog）共用。
//
// 文件命名：<prefix>-YYYY-MM-DD<ext>；同一天超过单文件上限时依次写成
// <prefix>-YYYY-MM-DD.1<ext>、.2……，重启复用当天序号最大的文件继续追加。
// 保留期与总容量按文件 mtime 从旧到新裁剪，绝不删除当前正在写的文件。
package logrotate

import (
	"bufio"
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultFileMaxBytes 单文件缺省上限（16 MiB）。
	DefaultFileMaxBytes int64 = 16 << 20
	// DefaultMaxBytes 目录总容量缺省上限（100 MiB）。
	DefaultMaxBytes int64 = 100 << 20
	// DefaultRetentionDays 缺省保留天数。
	DefaultRetentionDays = 7
	// defaultBufferSize 写缓冲缺省大小。
	defaultBufferSize = 64 << 10
	// dayLayout 文件名中的日期格式（与 locals 无关，切日按写入时刻的本地日期）。
	dayLayout = "2006-01-02"
)

// Config 轮转参数。Dir 为空时 New 返回 nil，调用方按「未启用」处理。
type Config struct {
	Dir string
	// Prefix 文件名前缀（如 "requests" / "server"），不含分隔符。
	Prefix string
	// Ext 扩展名，含前导点（如 ".jsonl" / ".log"）。
	Ext string
	// FileMaxBytes 单文件上限；<=0 回落 DefaultFileMaxBytes。
	FileMaxBytes int64
	// MaxBytes 目录总容量上限；<=0 回落 DefaultMaxBytes。
	MaxBytes int64
	// RetentionDays 保留天数；<=0 回落 DefaultRetentionDays。
	RetentionDays int
	// BufferSize 写缓冲字节；<=0 回落 64 KiB。
	BufferSize int
}

// withDefaults 补齐零值参数（Config 由调用方按自己的语义归一化后传入）。
func (c *Config) withDefaults() {
	if c.FileMaxBytes <= 0 {
		c.FileMaxBytes = DefaultFileMaxBytes
	}

	if c.MaxBytes <= 0 {
		c.MaxBytes = DefaultMaxBytes
	}

	if c.RetentionDays <= 0 {
		c.RetentionDays = DefaultRetentionDays
	}

	if c.BufferSize <= 0 {
		c.BufferSize = defaultBufferSize
	}
}

// File 一个轮转文件：日期 + 当天序号（0 = 基准文件名）。
type File struct {
	Path    string
	Name    string
	Day     time.Time
	Index   int
	Size    int64
	ModTime time.Time
}

// Writer 并发安全的轮转追加写入器。零值不可用，用 New 构造。
type Writer struct {
	cfg Config

	mu   sync.Mutex
	file *os.File
	buf  *bufio.Writer
	path string
	day  string
	size int64
}

// New 构造写入器：创建目录并补齐缺省参数，文件在首次写入时惰性打开。
// Dir 为空返回 (nil, nil)——调用方用 nil 表示未启用。
func New(cfg *Config) (*Writer, error) {
	if cfg == nil {
		return nil, nil //nolint:nilnil // nil 写入器表示未启用，调用方直接跳过
	}

	cfg.withDefaults()

	if strings.TrimSpace(cfg.Dir) == "" {
		return nil, nil //nolint:nilnil // Dir 为空同样表示未启用
	}

	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("logrotate: 创建目录 %s: %w", cfg.Dir, err)
	}

	return &Writer{cfg: *cfg}, nil
}

// Write 实现 io.Writer：按当前时刻切分文件。
func (w *Writer) Write(data []byte) (int, error) {
	return w.WriteAt(time.Now(), data)
}

// WriteAt 按给定时刻追加一条完整记录（不拆分记录跨文件）。
func (w *Writer) WriteAt(now time.Time, data []byte) (int, error) {
	if w == nil {
		return len(data), nil
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	if err := w.ensureFileLocked(now, len(data)); err != nil {
		return 0, err
	}

	n, err := w.buf.Write(data)
	w.size += int64(n)

	if err != nil {
		return n, fmt.Errorf("logrotate: 写入 %s: %w", w.path, err)
	}

	return n, nil
}

// Flush 把缓冲刷到磁盘。
func (w *Writer) Flush() error {
	if w == nil {
		return nil
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	if w.buf == nil {
		return nil
	}

	if err := w.buf.Flush(); err != nil {
		return fmt.Errorf("logrotate: 刷盘: %w", err)
	}

	return nil
}

// Close 刷盘并关闭当前文件；可重复调用。
func (w *Writer) Close() error {
	if w == nil {
		return nil
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	var flushErr error

	if w.buf != nil {
		if err := w.buf.Flush(); err != nil {
			flushErr = fmt.Errorf("logrotate: 刷盘: %w", err)
		}
	}

	w.closeFileLocked()

	return flushErr
}

// Dir 返回写入目录；nil 写入器返回空串。
func (w *Writer) Dir() string {
	if w == nil {
		return ""
	}

	return w.cfg.Dir
}

// Path 当前写入文件路径（未打开时为空）。
func (w *Writer) Path() string {
	if w == nil {
		return ""
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	return w.path
}

// Files 返回目录下全部轮转文件，按 (日期, 序号) 升序。
func (w *Writer) Files() ([]File, error) {
	if w == nil {
		return nil, nil
	}

	entries, err := os.ReadDir(w.cfg.Dir)
	if err != nil {
		return nil, fmt.Errorf("logrotate: 读取目录 %s: %w", w.cfg.Dir, err)
	}

	files := make([]File, 0, len(entries))

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		day, index, ok := parseName(w.cfg.Prefix, w.cfg.Ext, entry.Name())
		if !ok {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		files = append(files, File{
			Path:    filepath.Join(w.cfg.Dir, entry.Name()),
			Name:    entry.Name(),
			Day:     day,
			Index:   index,
			Size:    info.Size(),
			ModTime: info.ModTime(),
		})
	}

	slices.SortFunc(files, func(left, right File) int {
		if diff := left.Day.Compare(right.Day); diff != 0 {
			return diff
		}

		return cmp.Compare(left.Index, right.Index)
	})

	return files, nil
}

// Stats 返回文件数与总字节数。
func (w *Writer) Stats() (int, int64, error) {
	files, err := w.Files()
	if err != nil {
		return 0, 0, err
	}

	var total int64

	for _, file := range files {
		total += file.Size
	}

	return len(files), total, nil
}

// Prune 按保留期与总容量裁剪旧文件（跳过当前写入文件）。
func (w *Writer) Prune(now time.Time) {
	if w == nil {
		return
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	w.pruneLocked(now)
}

// ensureFileLocked 保证当前文件存在且能容纳记录，必要时切日或轮转。
func (w *Writer) ensureFileLocked(now time.Time, recordLen int) error {
	day := now.Format(dayLayout)
	if w.file == nil || w.day != day {
		if err := w.openFileLocked(now, false); err != nil {
			return err
		}
	}

	if w.size > 0 && w.size+int64(recordLen) > w.cfg.FileMaxBytes {
		if err := w.openFileLocked(now, true); err != nil {
			return err
		}
	}

	return nil
}

// openFileLocked 打开当天文件：rotate=false 复用当天序号最大的文件，rotate=true
// 分配下一个序号。每次切换文件后顺带裁剪，避免长期无跨天写入时保留期不生效。
func (w *Writer) openFileLocked(now time.Time, rotate bool) error {
	w.closeFileLocked()

	day := now.Format(dayLayout)

	path := filepath.Join(w.cfg.Dir, w.cfg.Prefix+"-"+day+w.cfg.Ext)
	if rotate {
		path = w.nextPathLocked(day)
	} else if latest := w.latestNameLocked(day); latest != "" {
		path = filepath.Join(w.cfg.Dir, latest)
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("logrotate: 打开 %s: %w", path, err)
	}

	info, err := file.Stat()
	if err != nil {
		_ = file.Close()

		return fmt.Errorf("logrotate: 读取 %s 大小: %w", path, err)
	}

	w.file = file
	w.buf = bufio.NewWriterSize(file, w.cfg.BufferSize)
	w.path = path
	w.day = day
	w.size = info.Size()

	w.pruneLocked(now)

	return nil
}

// closeFileLocked 刷盘并关闭当前文件（调用方已持锁）。
func (w *Writer) closeFileLocked() {
	if w.buf != nil {
		_ = w.buf.Flush()
	}

	if w.file != nil {
		_ = w.file.Close()
	}

	w.file = nil
	w.buf = nil
	w.path = ""
	w.day = ""
	w.size = 0
}

// latestNameLocked 返回当天序号最大的文件名；无匹配返回空串。
func (w *Writer) latestNameLocked(day string) string {
	entries, err := os.ReadDir(w.cfg.Dir)
	if err != nil {
		return ""
	}

	best := ""
	bestIndex := -1

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		parsedDay, index, ok := parseName(w.cfg.Prefix, w.cfg.Ext, entry.Name())
		if !ok || parsedDay.Format(dayLayout) != day {
			continue
		}

		if index > bestIndex {
			bestIndex = index
			best = entry.Name()
		}
	}

	return best
}

// nextPathLocked 返回当天第一个不存在的序号路径。
func (w *Writer) nextPathLocked(day string) string {
	base := filepath.Join(w.cfg.Dir, w.cfg.Prefix+"-"+day)

	path := base + w.cfg.Ext
	for index := 1; fileExists(path); index++ {
		path = base + "." + strconv.Itoa(index) + w.cfg.Ext
	}

	return path
}

// pruneLocked 按 mtime 从旧到新删除过期文件与超额文件（调用方已持锁）。
func (w *Writer) pruneLocked(now time.Time) {
	files, err := w.Files()
	if err != nil {
		// 裁剪是旁路维护：目录读不到不影响写入主路径。
		return
	}

	var total int64

	for _, file := range files {
		total += file.Size
	}

	slices.SortFunc(files, func(left, right File) int {
		if left.ModTime.Equal(right.ModTime) {
			return cmp.Compare(left.Name, right.Name)
		}

		return left.ModTime.Compare(right.ModTime)
	})

	cutoff := now.AddDate(0, 0, -w.cfg.RetentionDays)

	for _, file := range files {
		if file.Path == w.path || (!file.ModTime.Before(cutoff) && total <= w.cfg.MaxBytes) {
			continue
		}

		if err := os.Remove(file.Path); err == nil {
			total -= file.Size
		}
	}
}

// parseName 解析 <prefix>-YYYY-MM-DD[.N]<ext>；不匹配返回 ok=false。
func parseName(prefix, ext, name string) (time.Time, int, bool) {
	fullPrefix := prefix + "-"
	if !strings.HasPrefix(name, fullPrefix) || !strings.HasSuffix(name, ext) {
		return time.Time{}, 0, false
	}

	rest := strings.TrimSuffix(strings.TrimPrefix(name, fullPrefix), ext)

	dayText, indexText, hasIndex := strings.Cut(rest, ".")

	day, err := time.ParseInLocation(dayLayout, dayText, time.Local)
	if err != nil {
		return time.Time{}, 0, false
	}

	if !hasIndex {
		return day, 0, true
	}

	index, err := strconv.Atoi(indexText)
	if err != nil || index <= 0 {
		return time.Time{}, 0, false
	}

	return day, index, true
}

// fileExists 报告普通文件是否存在。
func fileExists(path string) bool {
	info, err := os.Stat(path)

	return err == nil && !info.IsDir()
}
