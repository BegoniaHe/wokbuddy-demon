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
	"sync"
	"time"

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

const (
	// flushLines / flushInterval 批量刷盘的阈值。
	//
	// log 包在 outMu 持锁期间调用 Write：逐行 Flush 等于让进程内所有 log.Printf
	// 串行排在同一次磁盘 I/O 上（调度器、请求路径互相阻塞）。改成按行数/时间批量
	// 刷。代价是崩溃时最多丢一个批次（原注释承诺「最多丢当前行」）。
	flushLines = 64
	// maxLineBytes 单行落盘上限。RedactForLog 的 limit=0 表示不截断，而上游错误
	// body / 整段 HTML 会经 log.Printf 进来：单行无界会让当前文件远超 FileMaxBytes
	// 且因是「正在写的文件」永远不被 pruneLocked 删除。先脱敏再截断（顺序不可换）。
	maxLineBytes = 8 << 10
)

// flushInterval 刷盘兜底间隔（测试可临时调小）。
var flushInterval = time.Second

// Sink 运行日志落盘出口：实现 io.Writer，按天分文件、按大小轮转、写入前脱敏。
type Sink struct {
	mu     sync.Mutex
	writer *logrotate.Writer
	// pending 自上次刷盘以来写入的行数；lastFlush 上次刷盘时刻。
	pending   int
	lastFlush time.Time
	closed    bool
	// stop/done 定时刷盘 goroutine 的生命周期。
	stop chan struct{}
	done chan struct{}
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

	sink := &Sink{writer: writer, stop: make(chan struct{}), done: make(chan struct{})}
	go sink.flushLoop()

	return sink, nil
}

// flushLoop 定时刷盘兜底。
//
// 批量策略的时间条件原本只在「下一次 Write」时检查：低流量部署（调度器每 5 分钟
// 一行）下最后一条会无限期留在 bufio 里，面板导出读的是磁盘文件，最新几条永远
// 缺席；进程被 kill -9 则整批丢失。
func (s *Sink) flushLoop() {
	defer close(s.done)

	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			s.mu.Lock()

			needFlush := !s.closed && s.pending > 0
			if needFlush {
				s.pending, s.lastFlush = 0, time.Now()
			}

			s.mu.Unlock()

			// Flush 放在 Sink.mu 之外：它是磁盘 I/O，持锁会让所有 log.Printf 排在
			// 一次 fsync 后面——正是批量刷盘要避免的串行化。logrotate.Writer 自带
			// 锁，写与刷的先后仍由它保证。
			if needFlush {
				_ = s.writer.Flush()
			}
		}
	}
}

// Enabled 报告落盘是否启用。
func (s *Sink) Enabled() bool { return s != nil && s.writer != nil }

// Write 实现 io.Writer：整行脱敏后落盘，按行数/时间批量刷盘。
func (s *Sink) Write(data []byte) (int, error) {
	if !s.Enabled() {
		return len(data), nil
	}

	s.mu.Lock()

	if s.closed {
		s.mu.Unlock()

		return len(data), nil
	}

	// 逐行处理：log.Printf 可以被调用方传入含换行的文本（panic 栈），原先整段
	// 走 RedactForLog 会把 \n 压成空格再截到 8 KiB，堆栈在落盘日志与面板导出里
	// 完全不可读。行数与总字节仍由 logrotate 兜底。
	lines := 0

	for line := range strings.SplitSeq(strings.TrimRight(string(data), "\r\n"), "\n") {
		clean := upstream.RedactForLog(line, maxLineBytes)

		if _, err := s.writer.Write([]byte(clean + "\n")); err != nil {
			s.mu.Unlock()

			return 0, fmt.Errorf("runlog: 写入运行日志: %w", err)
		}

		lines++
	}

	// 按**行数**计待刷：一次 log.Printf 传入的 panic 栈是几十行，按调用次数计时
	// flushLines=64 的批量语义会失效，全压到 1 秒 ticker 上。
	s.pending += lines
	now := time.Now()
	needFlush := s.pending >= flushLines || now.Sub(s.lastFlush) >= flushInterval

	if needFlush {
		s.pending, s.lastFlush = 0, now
	}

	s.mu.Unlock()

	if !needFlush {
		return len(data), nil
	}

	// Flush 放在 Sink.mu 之外：磁盘 I/O 持锁会把所有并发 log.Printf 串行在它后面。
	if err := s.writer.Flush(); err != nil {
		return 0, fmt.Errorf("runlog: 刷新运行日志: %w", err)
	}

	return len(data), nil
}

// Close 刷盘并关闭当前文件；可重复调用。关闭后 Write 直接丢弃。
func (s *Sink) Close() error {
	if !s.Enabled() {
		return nil
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()

		return nil
	}

	s.closed = true
	s.pending = 0
	s.mu.Unlock()

	// 先停定时刷盘再关文件：否则 goroutine 可能在 Close 之后仍对已关闭的 writer
	// 调 Flush。
	close(s.stop)
	<-s.done

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
