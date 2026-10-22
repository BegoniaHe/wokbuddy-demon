// ids.go 会话头族 ID 的解析与生成（issue #35 后台聚合）。
//
// 官方 CodeBuddy CLI 出站头族（X-Conversation-ID / X-Conversation-Request-ID /
// X-Request-ID / X-B3-*），后台按 X-Conversation-Request-ID（对话轮）聚合请求；
// 本文件提供 conversationId 提取、消息级 32 hex messageID、以及"同一会话键
// 稳定复用"的 conversationRequestID 惰性缓存，供 handler 轮转循环外生成、循环内
// 复用（换号/重试/降级全部同 ID → 后台不再碎片化）。
package session

import (
	"bytes"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"time"
)

// NewMessageID 生成随机 32 位 hex（无 UUID 版本位），仅用于 B3 span 等不要求
// UUID 语义的短标识；消息/请求级 ID 用 NewUUIDv7Hex。crypto/rand 失败（理论上
// 不可能）时回落 math/rand/v2 双 uint64 拼 32 hex——恒 32 hex、恒合法。
func NewMessageID() string {
	b := make([]byte, 16)
	if _, err := cryptorand.Read(b); err == nil {
		return hex.EncodeToString(b)
	}
	// 熵源故障的极端兜底：仍保证 32 hex（fallbackID 不 panic、不空串）。
	return fmt.Sprintf("%016x%016x", rand.Uint64()|1, rand.Uint64())
}

// NewUUIDv7Hex 生成 32 位小写 hex 的 UUIDv7（去连字符），对齐官方
// generateConversationRequestId / X-Conversation-Request-ID 校验正则：
// ^[0-9a-f]{12}7[0-9a-f]{3}[89ab][0-9a-f]{15}$。
// 前 48 位为 Unix 毫秒时间戳，剩余位来自 crypto/rand；熵源失败时回落
// math/rand/v2，版本/变体位仍在返回前强制写入。
func NewUUIDv7Hex() string {
	var buf [16]byte
	if _, err := cryptorand.Read(buf[:]); err != nil {
		binary.BigEndian.PutUint64(buf[0:8], rand.Uint64()|1)
		binary.BigEndian.PutUint64(buf[8:16], rand.Uint64())
	}

	var timestamp [8]byte
	binary.BigEndian.PutUint64(timestamp[:], uint64(time.Now().UnixMilli()))
	// Unix 毫秒只占 8 字节的后 6 字节（高 2 字节恒 0），UUIDv7 取低 48 位时间戳。
	copy(buf[:6], timestamp[2:])

	return uuidV7Hex(buf[:])
}

// NewUUIDv4 生成标准带连字符 UUIDv4（8-4-4-4-12，version=4、variant=10xx），
// 用于会话级兜底 ID（官方 crypto.randomUUID() 同款形态）。
func NewUUIDv4() string {
	var buf [16]byte
	if _, err := cryptorand.Read(buf[:]); err != nil {
		binary.BigEndian.PutUint64(buf[0:8], rand.Uint64()|1)
		binary.BigEndian.PutUint64(buf[8:16], rand.Uint64())
	}

	buf[6] = buf[6]&0x0f | 0x40
	buf[8] = buf[8]&0x3f | 0x80

	h := hex.EncodeToString(buf[:])

	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// uuidV7Hex 将 16 字节的版本/变体位改成 UUIDv7 后编码为 32 hex。
// 调用方保证 b 至少 16 字节（可直接传 sha256 派生结果）。
func uuidV7Hex(b []byte) string {
	b[6] = b[6]&0x0f | 0x70
	b[8] = b[8]&0x3f | 0x80

	return hex.EncodeToString(b[:16])
}

// deriveSalt 进程启动随机盐：对所有稳定聚合 ID 的纯派生统一加盐，使派生值无法
// 按外部可控的键内容（会话键/轮级键）被预计算；重启换新（重启时旧对话轮/会话已
// 结束，不构成断档）。会话级（RequestIDForKey）与轮级（TurnRequestID）共用同一盐。
var deriveSalt = NewMessageID()

// RequestIDForKey 返回会话键的稳定 conversationRequestID：sha256(盐|键) 前 16 字节
// 的 hex，纯派生（无缓存、无 TTL、内存不随键数增长——此前 sync.Map 惰性缓存随
// 会话键数量无上限增长，纯派生天然有界）。
//   - 同 key：进程内恒派生同值（一次 user send/同会话多轮聚合）；
//   - 异 key：各自独立，互不相同；
//   - 空 key：每次生成新值（无会话则无"会话内稳定"语义——调用方应在请求级
//     捕获复用，handler 在轮转循环外取一次即天然共享）。
//
// 返回值恒为 UUIDv7 形态的 32 hex，可直接用作 B3 TraceId（16/32 hex 合法）。
func RequestIDForKey(key string) string {
	if key == "" {
		return NewUUIDv7Hex()
	}

	sum := sha256.Sum256([]byte(deriveSalt + "|" + key))

	return uuidV7Hex(sum[:])
}

// turnMessage TurnKey 只读 role 与 content：content 用 bodyRaw 直接引用请求体
// 子切片，避免为每条消息复制一份 content（长上下文下可达几十 KB）。
type turnMessage struct {
	Role    string  `json:"role"`
	Content bodyRaw `json:"content"`
}

// TurnKey 派生「对话轮级」聚合键：body 里**最后一条** role=="user" 消息的
// 「序号 + 文本」。
//
// 为什么需要它：无会话键的客户端（OpenAI 兼容协议——dsh / Codex / Cherry Studio
// 等的请求体里既无 conversationId 也无 metadata 键）会让 ExtractSession 的会话键恒为空，
// 会话头族的聚合主键便只能逐请求新生成，agent 多轮在上游用量明细里仍是一条请求
// 一条记录。本函数给这类客户端一个**不依赖客户端配合**的轮级键：一次用户发送内的
// 所有上游调用（tool call 多轮 / 换号重试 / 降级重发）body 里最后一条 user 消息
// 恒定 → 同键；用户发下一条消息 → 换键。
//
// 为什么不取第一条 user 消息：首条在整个会话内不变，会把一次会话的所有轮并进
// 同一个聚合键（跨对话轮混并）。取最后一条才对齐官方 X-Conversation-Request-ID
// 的「对话轮」语义。序号一并入键：两次不同轮里内容相同的提问（"继续"）不会被并成
// 一轮。
//
// 无 body / 无 messages / 无 user 消息 / 该消息无文本 → ""（调用方回落请求级随机
// ID，不伪造聚合键）。
func TurnKey(body []byte) string {
	if len(body) == 0 {
		return ""
	}

	var obj struct {
		Messages []turnMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return ""
	}

	for i, msg := range slices.Backward(obj.Messages) {
		if msg.Role != "user" {
			continue
		}

		sig := contentSignature(msg.Content)
		if sig == "" {
			// 最后一条 user 消息没有可签名内容（空/null/空 parts）→ 本轮不建立聚合键。
			// 不继续往前找：整轮内该消息位置恒定，往前找反而会让键随 step 漂移。
			return ""
		}

		return fmt.Sprintf("u%d:%s", i, sig)
	}

	return ""
}

// contentSignature 取消息 content 的确定性签名（G1 修复——纯图片轮不再碎片化）：
//   - string 形态：返回文本，与 contentText 结果**完全一致**——纯文本路径键值
//     不变，存量会话的轮键/粘性键零漂移（向后兼容契约）；
//   - 数组形态（多模态 parts）：文本 part（type 为 "" 或 "text"，与 contentText
//     的拼接口径一致）按原文无缝拼接；非文本 part 追加 "[type:摘要]"——摘要取该
//     part 原始字节的 sha256 前 8 hex。data: base64 内联图可能超长，原文入键会
//     放大派生哈希压力（见 TurnKey 头注），只入短摘要；type + 原文摘要天然区分
//     不同内容/数量的非文本 part 集合，无需额外计数占位。
//
// 无可签名内容（空 / null / 空数组 / 纯文本 part 全为空串）返回 ""（不伪造——
// 调用方回落原有的空键语义）。TurnKey 与 firstUserText（粘性兜底）共用本函数，
// 两条链路的图片盲区一并修复。
func contentSignature(raw []byte) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || (len(trimmed) == 4 && trimmed[0] == 'n') { // 空 / null
		return ""
	}
	// 字符串形态：纯文本路径签名 == contentText 结果（键值零漂移）。
	if trimmed[0] == '"' {
		var str string
		if err := json.Unmarshal(trimmed, &str); err != nil {
			return ""
		}

		return str
	}

	if trimmed[0] != '[' {
		return ""
	}
	// 非文本 part：type + 原文 sha256 前 8 hex（超长 data: URL 只入短摘要）。
	return partsSignature(json.RawMessage(trimmed), func(typeName string, rawPart json.RawMessage) string {
		sum := sha256.Sum256(rawPart)
		return "\n[" + typeName + ":" + hex.EncodeToString(sum[:4]) + "]\n"
	})
}

// partsSignature 对多模态 parts 数组做确定性签名：文本 part（type 为 "" 或 "text"）
// 按原文无缝拼接（与 contentText 完全同口径——全文本 part 的数组签名 == contentText
// 结果，存量键零漂移）；非文本 part 的渲染交由 renderNonText（各调用方口径不同，
// 见 contentSignature / userContentSignatureRaw）。存在非文本 part 时对整体结果
// TrimSpace。json 解析失败返回 ""（不伪造）。
func partsSignature(raw json.RawMessage, renderNonText func(typeName string, rawPart json.RawMessage) string) string {
	var parts []json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}

	var builder strings.Builder

	hasNonText := false

	for _, item2 := range parts {
		var item struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(item2, &item); err != nil {
			return ""
		}

		if item.Type == "" || item.Type == "text" {
			builder.WriteString(item.Text)
			continue
		}

		hasNonText = true

		builder.WriteString(renderNonText(item.Type, item2))
	}

	out := builder.String()
	if !hasNonText {
		return out
	}

	return strings.TrimSpace(out)
}

// TurnRequestID 返回轮级键对应的聚合 ID：sha256(盐|键) 前 16 字节强制为
// UUIDv7 版本/变体位后的 32 hex（可直接作 B3 TraceId）。
//
// 纯派生，无缓存、无 TTL、不随进程内请求数增长内存 —— 这点与会话级的
// RequestIDForKey 相反：会话键数量有限（与粘性会话同源）可以常驻缓存，而轮级键
// 每个对话轮新增一条，缓存必须有界，派生式天然有界。
// 空键返回新随机值（无轮可聚合时保持原有的「每请求独立」行为）。
func TurnRequestID(turnKey string) string {
	if turnKey == "" {
		return NewUUIDv7Hex()
	}

	sum := sha256.Sum256([]byte(deriveSalt + "|" + turnKey))

	return uuidV7Hex(sum[:])
}
