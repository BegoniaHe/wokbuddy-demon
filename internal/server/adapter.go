package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// 本文件是唯一的入站 HTTP 适配层（Issue #12）：把 *http.Request 解码为
// chatInput、把 http.ResponseWriter 包装为 chatResponder。核心（handler.go）
// 不再直接接触 net/http 的请求/响应类型。
//
// 替换点：后续用 protobuf 自动生成端点时，只需另写一份等价的 newXxxInput /
// xxxResponder 适配器，核心逻辑（chatFlow 及其下游）无需改动。

// newChatInput 读取并解码入站请求，返回传输无关输入与输出端口。返回 false
// 表示请求体读取失败、已写出错误响应（核心不启动）。
func newChatInput(
	w http.ResponseWriter, r *http.Request, keepalive time.Duration, maxBodyBytes int64,
) (chatInput, chatResponder, bool) {
	// 解析前先套上网关侧上限（server.max_body_bytes，0 = 不限制）：Content-Length
	// 与 chunked 都由 MaxBytesReader 约束，超限不进入选号与上游请求。
	limitRequestBody(w, r, maxBodyBytes)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		if isBodyTooLarge(err) {
			writeOpenAIErrorHint(w, http.StatusRequestEntityTooLarge, "payload_too_large",
				bodyTooLargeMessage(maxBodyBytes), "缩小请求体或调大 server.max_body_bytes")

			return chatInput{}, nil, false
		}

		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())

		return chatInput{}, nil, false
	}
	// 只 peek stream/model 两个路由必需字段；其余字段原样透传，不做协议解码。
	var peek struct {
		Stream bool   `json:"stream"`
		Model  string `json:"model"`
	}

	_ = json.Unmarshal(body, &peek)

	chatInput2 := newChatInputFromRequest(r, body, peek.Stream, peek.Model)

	return chatInput2, &httpResponder{w: w, keepalive: keepalive}, true
}

// newChatInputFromRequest 从 HTTP 请求提取传输无关的聊天输入。
func newChatInputFromRequest(r *http.Request, body []byte, stream bool, model string) chatInput {
	return chatInput{
		body:           body,
		stream:         stream,
		model:          model,
		convReqID:      r.Header.Get("X-Conversation-Request-Id"),
		traceID:        r.Header.Get("X-Trace-Id"),
		inboundSession: inboundSessionMetaFrom(r),
		trace:          requestTraceFrom(r),
		keyUsage:       keyUsageFrom(r.Context()),
		keyRealm:       keyRealmFrom(r.Context()),
	}
}

// limitRequestBody 用 http.MaxBytesReader 包住请求体；limit<=0 表示不限制。
func limitRequestBody(w http.ResponseWriter, r *http.Request, limit int64) {
	if limit > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, limit)
	}
}

// isBodyTooLarge 判断读取错误是否来自 MaxBytesReader 的上限。
func isBodyTooLarge(err error) bool {
	var mbe *http.MaxBytesError
	return errors.As(err, &mbe)
}

// bodyTooLargeMessage 生成不泄露内部细节的超限提示。
func bodyTooLargeMessage(limit int64) string {
	return fmt.Sprintf("request body exceeds server.max_body_bytes (%d bytes)", limit)
}

// httpResponder 把核心的结构化结果编码为 OpenAI 兼容 HTTP 响应。它是 chat 路径
// 上唯一写 http.ResponseWriter 的类型。
type httpResponder struct {
	w         http.ResponseWriter
	keepalive time.Duration
	// pacer 响应整形器（issue #448；nil = 关闭）。由核心在写出前挂上，只影响
	// 流式帧的写出节奏，不改任何字节。
	pacer upstream.ResponsePacer
	// committed 响应头是否已提交（issue #500）：首帧等待期的注释帧一写出即已提交，
	// 之后状态码与 JSON 错误都改不了，只能在这条 200 SSE 流里收尾。
	committed bool
}

// setStreamPacer 挂响应整形器（issue #448；见 streamPacerSetter）。
func (r *httpResponder) setStreamPacer(p upstream.ResponsePacer) { r.pacer = p }

// JSON 写出非流式成功响应（OpenAI JSON）。
func (r *httpResponder) JSON(status int, body any) {
	writeJSON(r.w, status, body)
}

// Error 写出 OpenAI 错误信封（hint 为空时省略 gateway_hint 字段）。
// 响应头已提交（首帧等待期写过注释帧，issue #500）时无法再改状态码，降级为
// 这条 200 SSE 流里的 error 帧 + [DONE]——客户端拿到的仍是可解析的错误，
// 而不是「200 + 半截流里塞 JSON」。
func (r *httpResponder) Error(status int, code, msg, hint string) {
	if r.committed {
		r.writeErrorFrame(code, msg, hint)
		return
	}

	writeOpenAIErrorHint(r.w, status, code, msg, hint)
}

// Header 设置响应头（必须在写响应体前调用）。
func (r *httpResponder) Header(name, value string) {
	r.w.Header().Set(name, value)
}

// Stream 把上游 SSE 透传给客户端。SSE 头与帧格式由 upstream.StreamHint 负责
// （本仓出站协议层，未改动），适配器只负责挂接选项与写入目标。
func (r *httpResponder) Stream(
	rc io.Reader, hint func(string) string, repair *upstream.MarkupRepair, onErrFrame func(string),
) error {
	r.committed = true

	opts := streamHintOptions(r.keepalive, r.pacer, repair, onErrFrame)
	if err := upstream.StreamHint(r.w, rc, hint, opts...); err != nil {
		return fmt.Errorf("stream hint: %w", err)
	}

	return nil
}

// setSSEHeaders 补齐 SSE 响应头（issue #500）：首帧等待期的注释帧由本类型自己写，
// 必须与 upstream.StreamHint 的头逐项对齐，否则内容类型为空、反代会缓冲。
func (r *httpResponder) setSSEHeaders() {
	h := r.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
}

// writeHeartbeat 写一帧 SSE 注释并 flush（issue #500）。返回错误表示客户端已走：
// 心跳协程据此退出，后续真实帧会拿到同一个写错误。
func (r *httpResponder) writeHeartbeat() error {
	r.setSSEHeaders()

	// 先置位：即使写只发出半帧，响应头也已提交，之后的错误只能走 SSE 帧收尾。
	r.committed = true

	if _, err := io.WriteString(r.w, ": keepalive\n\n"); err != nil {
		return fmt.Errorf("heartbeat write: %w", err)
	}

	r.flush()

	return nil
}

// writeErrorFrame 在已提交的 200 SSE 流里补一帧 error + [DONE]（issue #500）。
func (r *httpResponder) writeErrorFrame(code, msg, hint string) {
	r.setSSEHeaders()

	raw, err := json.Marshal(openAIErrorEnvelope(code, msg, hint))
	if err != nil {
		return // map 只含 string，编码不会失败；真失败也不能改写已提交的响应头
	}

	for _, part := range []string{"data: ", string(raw), "\n\n", "data: [DONE]\n\n"} {
		if _, werr := io.WriteString(r.w, part); werr != nil {
			return
		}
	}

	r.flush()
}

// flush 尽力刷新（非 Flusher 的测试替身忽略）。
func (r *httpResponder) flush() {
	if fl, ok := r.w.(http.Flusher); ok {
		fl.Flush()
	}
}

// heartbeatWriter 可选实现的响应编码器：首帧等待期的 SSE 注释帧出口（issue #500）。
// 不是 chatResponder 的一部分——测试替身与其他协议出口不需要心跳。
type heartbeatWriter interface {
	writeHeartbeat() error
}

// startFirstByteHeartbeat 在等上游首帧期间按 interval 写 SSE 注释帧（issue #500）。
// 首帧否决窗口为了能静默换号而一个字节都不写，客户端从建连起零字节干等到 TTFB；
// 大上下文把这个空窗拉到秒级，客户端会按空闲超时断开。
//
// 返回的 stop 必须调用：它关闭 ticker 并等待在途写完成，之后才开始透传真实帧——
// 注释帧与真实帧因此不会写交错（串行化靠 join，不需要额外的写锁）。
// interval<=0 或出口不支持注释帧时返回 no-op（0 = 逐字节保持旧行为）。
func startFirstByteHeartbeat(out chatResponder, interval time.Duration) func() {
	writer, ok := out.(heartbeatWriter)
	if !ok || interval <= 0 {
		return func() {}
	}

	done := make(chan struct{})
	stopped := make(chan struct{})

	go func() {
		defer close(stopped)

		// 写注释帧走的是 response writer：封装层若 panic，这里是独立协程，
		// 无人 recover 就是进程级终止（连带所有账号的 /v1/*）。隔离后按
		// 「客户端已走」处理，真实帧会拿到写错误。
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("ERR: [server] first-byte heartbeat panic（已隔离）: %v", rec)
			}
		}()

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if writer.writeHeartbeat() != nil {
					return // 客户端已走：不再写注释，真实帧会拿到写错误
				}
			}
		}
	}()

	return func() {
		close(done)
		<-stopped
	}
}

// streamHintOptions 组装 StreamHint 的旁路选项（error 帧观察 → 标记修复 → 保活
// → 整形）。四路响应编码器共用同一口径，避免各自维护一份选项拼装顺序。
func streamHintOptions(
	keepalive time.Duration, pacer upstream.ResponsePacer,
	repair *upstream.MarkupRepair, onErrFrame func(string),
) []upstream.StreamOption {
	opts := make([]upstream.StreamOption, 0, 4)
	if onErrFrame != nil {
		opts = append(opts, upstream.WithErrorFrameObserver(onErrFrame))
	}

	opts = append(opts, upstream.WithMarkupRepair(repair))
	if keepalive > 0 {
		opts = append(opts, upstream.WithKeepaliveInterval(keepalive))
	}

	if pacer != nil {
		opts = append(opts, upstream.WithResponsePacer(pacer))
	}

	return opts
}
