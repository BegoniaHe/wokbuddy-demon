package server

import (
	"io"
	"net/http"

	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// 本文件定义业务核心与传输层之间的「端口」（port）：入站方向是传输无关的
// chatInput（结构化请求对象），出站方向是 chatResponder（结构化结果出口）。
//
// 目标（Issue #12）：让 chatFlow 及其下游阶段不再直接读写 http.Request /
// http.ResponseWriter，从而为后续用 protobuf 自动生成端点预留可替换的最外层
// 适配器。所有 net/http 读写收敛在 adapter.go 一层薄适配器内。
//
// 端口刻意保持最小面：只承载核心真正需要的信息与出口动作，不引入 HTTP 概念
// （方法、URL、Header map、ResponseWriter 等）。

// chatInput 一次 chat 请求的传输无关输入。字段由适配器在入站边界一次性提取，
// 核心只消费已结构化的值。
type chatInput struct {
	// body 原始请求体（出站透传用；核心在其上做 model/prompt 改写）。
	body []byte
	// stream 是否流式（由适配器从 body peek 出，避免核心重复解码）。
	stream bool
	// model 原始 model 名（可能带 "cn:"/"global:" realm 前缀，含前缀）。
	model string
	// clientIP 已提取的客户端 IP（供透传给上游；空 = 未透传）。
	clientIP string
	// convReqID 入站 X-Conversation-Request-Id（缺省空串，不伪造）。
	convReqID string
	// traceID 入站 X-Trace-Id（缺省空串）。
	traceID string
	// inboundSession 入站官方会话元数据（缺省零值，不伪造）。
	inboundSession inboundSessionMeta
	// trace 请求观测状态（请求日志出口回填统计）；nil = 未启用请求日志。
	trace *requestTrace
	// keyUsage 命名密钥用量累加器（由 withClientAuth 经 context 注入）；
	// nil = 默认密钥命中或未启用命名密钥，出口不记账。
	keyUsage *keyUsage
	// keyRealm 命名密钥出口绑定（issue #94）："" = 不绑定，cn/global = 固定。
	keyRealm string
}

// chatResponder 核心的结构化结果出口。实现方负责把结果编码为具体传输形态
// （HTTP/SSE，或未来的 protobuf 端点）；核心只决定「说什么」，不决定「怎么编码」。
type chatResponder interface {
	// JSON 写出非流式成功响应（实现方负责 OpenAI JSON 编码与 Content-Type）。
	JSON(status int, body any)
	// Error 写出 OpenAI 错误信封（code/msg/hint 由核心按业务语义给出；hint 为空
	// 时实现方省略该字段）。
	Error(status int, code, msg, hint string)
	// Header 设置响应头（如 Retry-After）；必须在写出响应体之前调用。
	Header(name, value string)
	// Stream 把上游流按 OpenAI SSE 透传给客户端，返回透传错误（如空流、
	// 客户端写失败）。hint 为上游 error 帧的 gateway_hint 生成器；repair 为
	// 原生工具调用标记修复器；onErrFrame 为上游 error 帧旁路观察者（可 nil）。
	Stream(rc io.Reader, hint func(string) string, repair *upstream.MarkupRepair, onErrFrame func(string)) error
}

// inboundSessionMeta 入站官方会话元数据（来自客户端 X-* 头）。
// 网关只透传客户端显式给出的值，不自行补默认。
type inboundSessionMeta struct {
	conversationID       string
	rootRequestID        string
	parentConversationID string
	agentIntent          string
	agentPurpose         string
	agentType            string
	compaction           string
	contextCompacted     string
}

// inboundSessionMetaFrom 从入站请求头提取官方会话元数据。
func inboundSessionMetaFrom(r *http.Request) inboundSessionMeta {
	return inboundSessionMeta{
		conversationID:       r.Header.Get("X-Conversation-Id"),
		rootRequestID:        r.Header.Get("X-Root-Request-Id"),
		parentConversationID: r.Header.Get("X-Parent-Conversation-Id"),
		agentIntent:          r.Header.Get("X-Agent-Intent"),
		agentPurpose:         r.Header.Get("X-Agent-Purpose"),
		agentType:            r.Header.Get("X-Agent-Type"),
		compaction:           r.Header.Get("X-Compaction"),
		contextCompacted:     r.Header.Get("X-Context-Compacted"),
	}
}
