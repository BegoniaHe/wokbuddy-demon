// recovery.go 网关路由 panic 兜底（issue #146）。
//
// net/http 只保证 panic 不拖垮进程，但会静默 abort 连接：客户端只看到 EOF，
// reqlog 归档缺失，SSE 在途计数也可能漏减。这里在最外层 recover，未写响应头时
// 按入站协议返回统一错误体；已写头（流式已开始）时只记录，避免 superfluous
// WriteHeader 与半截错误体污染。
package server

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"runtime/debug"

	"github.com/BegoniaHe/wokbuddy-demon/internal/logfmt"
)

// maxPanicStack 日志里保留的 stack 上限。panic 栈通常 < 16KiB，64KiB 足以覆盖
// 深调用链，同时防止异常递归把单行日志撑到不可读。
const maxPanicStack = 64 << 10

// withRecovery 包裹单个 handler。必须放在鉴权/限流中间件最外层：否则内层
// 中间件的 defer（如命名密钥 release）不会执行。
func (*Handler) withRecovery(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		recoveryWriter2 := &recoveryWriter{ResponseWriter: w}

		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			// 客户端已断开：net/http 用 ErrAbortHandler 表示「主动中止」，不是 bug，
			// 原样抛回让标准库按既有语义处理。
			if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(rec)
			}

			stack := logfmt.Truncate(string(debug.Stack()), maxPanicStack)

			log.Printf("ERR: [server] panic recovered method=%q path=%q type=%T\n%s",
				logfmt.Truncate(r.Method, 16), logfmt.Truncate(r.URL.Path, 256), rec, stack)

			if recoveryWriter2.wroteHeader {
				return
			}

			if isAnthropicMessagesPath(r.URL.Path) {
				writeAnthropicError(
					recoveryWriter2, http.StatusInternalServerError, "api_error", "internal gateway error", "",
				)

				return
			}

			writeOpenAIError(recoveryWriter2, http.StatusInternalServerError, "internal_error", "internal gateway error")
		}()

		next(recoveryWriter2, r)
	}
}

// recoveryWriter 记录响应头是否已写出，并透传 Flusher/Hijacker/Pusher/ReaderFrom，
// 保证 SSE、WebSocket 升级与 sendfile 路径不被兜底中间件降级。
type recoveryWriter struct {
	http.ResponseWriter

	wroteHeader bool
}

func (w *recoveryWriter) WriteHeader(code int) {
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *recoveryWriter) Write(p []byte) (int, error) {
	w.wroteHeader = true

	return w.ResponseWriter.Write(p)
}

func (w *recoveryWriter) Flush() {
	// Flush 会让 net/http 立即发送响应头；未标记时随后 panic 会被误判成
	// 「尚未写头」，向已开始的响应追加错误体。
	w.wroteHeader = true

	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *recoveryWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := w.ResponseWriter.(http.Hijacker); ok {
		conn, bufrw, err := hj.Hijack()
		if err == nil {
			// 连接已交给调用方，HTTP 响应阶段结束；之后再 panic 不得向被劫持的
			// 连接追加错误体（net/http 会拒绝写，但 recover 仍会误走写错误分支）。
			w.wroteHeader = true
		}

		return conn, bufrw, err
	}

	return nil, nil, http.ErrNotSupported
}

func (w *recoveryWriter) Push(target string, opts *http.PushOptions) error {
	if p, ok := w.ResponseWriter.(http.Pusher); ok {
		return p.Push(target, opts)
	}

	return http.ErrNotSupported
}

func (w *recoveryWriter) ReadFrom(r io.Reader) (int64, error) {
	// ReadFrom 直接写底层 ResponseWriter，不经过本类型的 Write。必须在这里同步
	// 标记响应头已写出，否则 sendfile/io.Copy 路径之后再 panic 会被 recover 误判
	// 成「尚未写头」，向已开始发送的响应追加第二个错误体。
	w.wroteHeader = true

	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}

	written, err := io.Copy(w.ResponseWriter, r)
	if err != nil {
		return written, fmt.Errorf("copy response: %w", err)
	}

	return written, nil
}

func (w *recoveryWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
