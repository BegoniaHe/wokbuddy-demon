// Package runlog 把运行时日志按天落盘（可选启用），供面板按日期区间导出。
//
// 落盘前统一走 upstream.RedactForLog：日志行可能带上游错误原文、URL 与 token，
// 磁盘上的运行日志与导出的 ZIP 都必须是脱敏后的内容。未启用时 New 返回 nil，
// 所有方法 nil 安全——MultiWriter 直接跳过，行为与「只写 stdout/stderr + 面板
// 环形缓冲」完全一致。
package runlog

import (
	"fmt"
	"strings"

	"github.com/BegoniaHe/wokbuddy-demon/internal/logrotate"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

const (
	// Prefix 运行日志文件名前缀：<dir>/server-YYYY-MM-DD.log。
	Prefix = "server"
	// Ext 运行日志扩展名。
	Ext = ".log"
	// FileMaxBytes 单文件上限（16 MiB），超过后按天序号轮转。
	FileMaxBytes int64 = logrotate.DefaultFileMaxBytes
)

// Config 运行日志落盘参数。Enabled=false 时不创建任何文件。
type Config struct {
	Dir           string
	Enabled       bool
	RetentionDays int
	MaxBytes      int64
}

// Sink 运行日志落盘出口：实现 io.Writer，按天分文件、按大小轮转、写入前脱敏。
type Sink struct {
	writer *logrotate.Writer
}

// New 构造落盘出口。未启用或 Dir 为空返回 (nil, nil)。
func New(cfg Config) (*Sink, error) {
	if !cfg.Enabled || strings.TrimSpace(cfg.Dir) == "" {
		return nil, nil //nolint:nilnil // nil 出口表示未启用，调用方直接跳过
	}

	writer, err := logrotate.New(&logrotate.Config{
		Dir:           cfg.Dir,
		Prefix:        Prefix,
		Ext:           Ext,
		FileMaxBytes:  FileMaxBytes,
		MaxBytes:      cfg.MaxBytes,
		RetentionDays: cfg.RetentionDays,
	})
	if err != nil {
		return nil, fmt.Errorf("runlog: %w", err)
	}

	if writer == nil {
		return nil, nil //nolint:nilnil // logrotate 未启用时同样返回 nil 出口
	}

	return &Sink{writer: writer}, nil
}

// Enabled 报告落盘是否启用。
func (s *Sink) Enabled() bool { return s != nil && s.writer != nil }

// Write 实现 io.Writer：整行脱敏后落盘并立即刷盘（进程崩溃最多丢当前行）。
func (s *Sink) Write(data []byte) (int, error) {
	if !s.Enabled() {
		return len(data), nil
	}

	line := strings.TrimRight(string(data), "\r\n")
	clean := upstream.RedactForLog(line, 0)

	if _, err := s.writer.Write([]byte(clean + "\n")); err != nil {
		return 0, fmt.Errorf("runlog: 写入运行日志: %w", err)
	}

	if err := s.writer.Flush(); err != nil {
		return 0, fmt.Errorf("runlog: 刷新运行日志: %w", err)
	}

	return len(data), nil
}

// Close 刷盘并关闭当前文件；可重复调用。
func (s *Sink) Close() error {
	if !s.Enabled() {
		return nil
	}

	return s.writer.Close() //nolint:wrapcheck // 包装方法原样透传（logrotate 已附上下文）。
}

// Dir 运行日志目录；未启用返回空串。
func (s *Sink) Dir() string {
	if !s.Enabled() {
		return ""
	}

	return s.writer.Dir()
}

// Files 返回目录下的运行日志文件（按日期、序号升序）。
func (s *Sink) Files() ([]logrotate.File, error) {
	if !s.Enabled() {
		return nil, nil
	}

	return s.writer.Files() //nolint:wrapcheck // 包装方法原样透传（logrotate 已附上下文）。
}
