package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
}

// JSON 写出非流式成功响应（OpenAI JSON）。
func (r *httpResponder) JSON(status int, body any) {
	writeJSON(r.w, status, body)
}

// Error 写出 OpenAI 错误信封（hint 为空时省略 gateway_hint 字段）。
func (r *httpResponder) Error(status int, code, msg, hint string) {
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
	opts := make([]upstream.StreamOption, 0, 2)
	if onErrFrame != nil {
		opts = append(opts, upstream.WithErrorFrameObserver(onErrFrame))
	}

	opts = append(opts, upstream.WithMarkupRepair(repair))
	if r.keepalive > 0 {
		opts = append(opts, upstream.WithKeepaliveInterval(r.keepalive))
	}

	if err := upstream.StreamHint(r.w, rc, hint, opts...); err != nil {
		return fmt.Errorf("stream hint: %w", err)
	}

	return nil
}
