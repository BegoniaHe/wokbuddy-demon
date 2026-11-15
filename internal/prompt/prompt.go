// Package prompt 提供网关自有系统提示词：内置默认 + 文件覆盖 + 降级中性提示词。
//
// 背景：客户端（Claude Code/Codex 等 CLI）在 system prompt 注入固定模板句，
// 上游内容审核按逐字精确匹配误杀合法流量（issue #36/PR39 的 11-128）。
// 方案：网关在出站前用自有系统提示词替换客户端 system/developer 消息，
// 从源头消灭 system 来源的指纹误报（用户/assistant 消息里的指纹串仍由
// internal/upstream/sanitize.go 清洗，两层叠加、互不替代）。
package prompt

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"
)

//go:embed defaultprompt.md
var defaultPrompt string

// Degraded 降级提示词：误报处理用，刻意极简中性。
//
// 触发场景：passthrough 模式下请求被上游内容策略拦截（HTTP 400 + 审核文案），
// 判定为指纹误报后换最小中性提示词重试一次。非对抗框架——只用于绕开
// system 来源的误报，不改变用户指令的合法性语义。
const Degraded = "You are a helpful assistant. Respond in the user's language, " +
	"follow the user's instructions, and be direct and concise."

// Load 按 mode 与 file 加载系统提示词文本。
//   - file 非空 → 读文件（不存在/读失败返回 error，调用方 fail fast）；
//   - file 空 → 返回内置 defaultPrompt。
//
// mode 在此仅做透传记录（实际 custom/passthrough 路由由调用方决定），
// Load 只负责"拿到一段提示词文本"，不关心路由语义。
func Load(mode, file string) (string, error) {
	if file == "" {
		return defaultPrompt, nil
	}

	raw, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("prompt file %s: %w", file, err)
	}
	// 空文件必须报错：custom/append 模式下 systemPrompt 为空时 Rewrite/Append 直接
	// 原样返回 body，等于「配了提示词文件、实际一行都没生效」，又把指纹误杀放回来，
	// 而配置层只看得到「读取成功」。
	if strings.TrimSpace(string(raw)) == "" {
		return "", fmt.Errorf("prompt file %s（删除该配置项以使用内置默认提示词）: %w", file, errEmptyPromptFile)
	}

	return string(raw), nil
}

// Rewrite 解析 OpenAI 请求体并替换系统提示词：
//   - 删除 messages 中所有 role 为 system/developer 的消息；
//   - 在 messages 头部插入一条 {"role":"system","content":systemPrompt}；
//   - 其余字段与 user/assistant/tool 消息逐字不动。
//
// 解析失败 → 原样返回（绝不失败）：Rewrite 是出站改写的关键路径，
// 任何解析错误都不应阻塞请求转发，让上游按其原始语义处理。
func Rewrite(body []byte, systemPrompt string) []byte {
	obj, ok := decodeBody(body, systemPrompt)
	if !ok {
		return body
	}

	msgs, ok := promptMessages(obj)
	if !ok {
		// messages 键存在但不是数组（字符串/对象/null）：改写只能靠覆盖，等于丢掉
		// 客户端内容，故原样返回，交给上游按其原始语义处理。
		return body
	}

	if msgs == nil {
		// 键不存在 → 插入单条 system 后原样保留其余字段。
		obj["messages"] = []any{systemMsg(systemPrompt)}
		return marshalOr(obj, body)
	}
	// 过滤掉所有 system/developer 消息，保留 user/assistant/tool 及其他角色。
	kept := make([]any, 0, len(msgs)+1)
	for _, m := range msgs {
		if isSystemMsg(m) {
			continue
		}

		kept = append(kept, m)
	}
	// 头部插入单条 system 消息（prepend 避免整体重排语义）。
	obj["messages"] = append([]any{systemMsg(systemPrompt)}, kept...)

	return marshalOr(obj, body)
}

// promptMessages 取 body 的 messages 字段：
//   - 键不存在 → (nil, true)，调用方插入单条 system；
//   - 键存在且是数组 → (msgs, true)；
//   - 键存在但类型不符（字符串/对象/null）→ (nil, false)，调用方放弃改写。
func promptMessages(obj map[string]any) ([]any, bool) {
	raw, exists := obj["messages"]
	if !exists {
		return nil, true
	}

	msgs, ok := raw.([]any)

	return msgs, ok
}

// Append 解析 OpenAI 请求体并在"开头连续 system/developer 块"之后插入一条
// 网关自有 system 提示词（issue #129 append 模式）：
//   - 开头连续块 = 从 messages[0] 起向后 role 为 system/developer（精确字符串
//     匹配，与 Rewrite 删除口径一致）的消息；遇第一条非 system/developer
//     消息（含非 map 消息、无 role 消息）即停；
//   - 插入点 = 连续块末尾之后（块长 0 时即 messages 最前）；
//   - 所有既有消息（含开头块、中途 system、user/assistant/tool）逐字不动
//     ——客户端项目规范/工具约定与网关提示词并用。
//
// 守卫与 Rewrite 逐条一致：空 body / 空 systemPrompt / 坏 JSON → 原样返回
// （绝不失败）；无 messages 字段 → messages=[网关 system]，其余字段原样。
//
// 边界判定须显式同时匹配 system 与 developer：归一（developer→system）在
// 下游 prepareBody 的 normalizeRoles，Append 执行时开头块里的 developer
// 还是 developer。网关消息角色用 system 而非 developer——上游 role 白名单
// 不含 developer，插 developer 等于制造一次必然归一与多余的 11-128 风险窗口。
func Append(body []byte, systemPrompt string) []byte {
	obj, ok := decodeBody(body, systemPrompt)
	if !ok {
		return body
	}

	msgs, ok := promptMessages(obj)
	if !ok {
		// 同 Rewrite：键存在而类型不符时放弃改写，不覆盖客户端内容。
		return body
	}

	if msgs == nil {
		// 键不存在 → 插入单条 system 后原样保留其余字段。
		obj["messages"] = []any{systemMsg(systemPrompt)}
		return marshalOr(obj, body)
	}
	// 扫描开头连续 system/developer 块，遇第一条非 system/developer 即停。
	insertAt := 0

	for _, m := range msgs {
		if !isSystemMsg(m) {
			break
		}

		insertAt++
	}
	// 已有消息逐字不动：只在插入点拼接，不重排、不改写任何元素。
	rewritten := make([]any, 0, len(msgs)+1)
	rewritten = append(rewritten, msgs[:insertAt]...)
	rewritten = append(rewritten, systemMsg(systemPrompt))
	rewritten = append(rewritten, msgs[insertAt:]...)
	obj["messages"] = rewritten

	return marshalOr(obj, body)
}

// decodeBody 解析 OpenAI 请求体，供 Rewrite/Append 共用：
// 空 body 或空 systemPrompt 直接判否（调用方原样返回，绝不失败）；
// JSON 解析失败或顶层为 null 同样判否（nil map 不可写入，否则后续赋值 panic）。
//
// UseNumber：改写只是动 messages，不能顺手改掉客户端的数值字面量。默认解码会把
// 一切数字变 float64 —— seed 9007199254740993 变成 ...992、1.0 变成 1、1e21 变成
// 1e+21（与 split.go / internal/server/logging.go 的 rewriteModel 同口径）。
func decodeBody(body []byte, systemPrompt string) (map[string]any, bool) {
	if len(body) == 0 || systemPrompt == "" {
		return nil, false
	}

	obj, err := decodeJSONObject(body)
	if err != nil || obj == nil {
		return nil, false
	}

	return obj, true
}

// decodeJSONObject 以 UseNumber 语义解析顶层 JSON 对象（数值按字面量保留）。
func decodeJSONObject(body []byte) (map[string]any, error) {
	// 非法 UTF-8 一律不改写：json.Unmarshal 会把坏字节替换成 U+FFFD，回写后 body
	// 与客户端发出的内容不再逐字节相同，而不改写时它是原样转发的。改写只该动
	// messages 的形状，不该顺手改写内容编码。
	if !utf8.Valid(body) {
		return nil, errInvalidUTF8Body
	}

	var obj map[string]any

	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()

	if err := dec.Decode(&obj); err != nil {
		return nil, fmt.Errorf("decode json object: %w", err)
	}

	// 尾随内容必须拒绝：{}{} 或 {}garbage 会被改写成只剩第一个对象再发出去，
	// 「改写绝不失败」就变成了「悄悄丢数据」。与 clientprofile 的 ensureEOF 同口径。
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errTrailingJSON
	}

	return obj, nil
}

// errTrailingJSON 请求体在第一个 JSON 对象之后还有内容：不做改写，原样转发。
var errTrailingJSON = errors.New("request body has trailing content after JSON object")

// errInvalidUTF8Body 请求体含非法 UTF-8：调用方原样转发（见 decodeBody）。
var errInvalidUTF8Body = errors.New("request body is not valid UTF-8")

// errEmptyPromptFile 提示词文件内容为空：custom/append 模式会静默退化成 passthrough。
var errEmptyPromptFile = errors.New("prompt file is empty")

// systemMsg 构造一条网关自有 system 消息（角色固定 system，理由见 Append 注释）。
func systemMsg(content string) map[string]any {
	return map[string]any{"role": "system", "content": content}
}

// isSystemMsg 判定消息是否 role 为 system/developer（精确字符串匹配）。
// 非 map 消息、无 role 消息均判否（与 Rewrite 删除口径、Append 停止口径一致）。
func isSystemMsg(m any) bool {
	mm, ok := m.(map[string]any)
	if !ok {
		return false
	}

	role, _ := mm["role"].(string)

	return role == "system" || role == "developer"
}

// marshalOr 序列化 obj，失败时返回 fallback（原 body），保证改写路径绝不失败。
func marshalOr(obj map[string]any, fallback []byte) []byte {
	out, err := json.Marshal(obj)
	if err != nil {
		return fallback
	}

	return out
}

// EnsureSystemFirst 保证出站 messages 的第一条是 system。
//
// 上游对部分账号灰度启用请求形态校验：messages[0] 不是 system 即 400 11-128
// "first message is not system prompt"。官方客户端每次请求都带开头 system；
// passthrough 模式会把「只带 user」的请求原样转发，属于官方不会发出的 wire 形态。
//
// 只补角色形态，不动任何既有消息：首条已是 system 原样返回；首条 developer
// 就地改成 system（内容逐字保留，下游 normalizeRoles 同口径）；首条是
// user/assistant/tool/非对象时在最前插入 fallback（空则用 Degraded）。
// body 不可解析 / 无 messages / messages 为 [] 时原样返回，不替上游编造对话。
func EnsureSystemFirst(body []byte, fallback string) []byte {
	if len(body) == 0 {
		return body
	}

	// UseNumber：只改首条消息的 role，其余字段（含 seed 等大整数）必须逐字保留。
	obj, err := decodeJSONObject(body)
	if err != nil {
		return body
	}

	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return body
	}

	if first, ok := msgs[0].(map[string]any); ok {
		role, _ := first["role"].(string)
		if role == "system" {
			return body
		}

		if role == "developer" {
			first["role"] = "system"
			return marshalOr(obj, body)
		}
	}

	if fallback == "" {
		fallback = Degraded
	}

	rewritten := make([]any, 0, len(msgs)+1)
	rewritten = append(rewritten, systemMsg(fallback))
	rewritten = append(rewritten, msgs...)
	obj["messages"] = rewritten

	return marshalOr(obj, body)
}
