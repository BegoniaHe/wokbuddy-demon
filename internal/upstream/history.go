// history.go 出站历史裁剪（issue #99）：按「轮」丢弃过老的多轮历史。
//
// 只做切片，不改写任何单条消息的内容；让网关侧统一收敛，任何客户端（DSH / Codex /
// Claude Code / 自研脚本）都受益，不需要客户端支持，也不触碰存储层。
package upstream

import (
	"log"
	"slices"
)

// trimHistory 保留开头连续 system/developer 消息 + 倒数第 keep 条 user 起的尾部。
// keep <= 0 或消息数不足时原样返回（dropped=0）。
//
// 「一轮」= 一条 user 消息（含多模态 content 的 user 消息）；tool / assistant 不参与计数。
// 裁剪起点之前的消息：仅当全是 system/developer 时才保留在头部，其余整体丢弃——保证
// 「开头连续」语义，不出现「system → 被删段 → 保留 system」的荒谬结果。
func trimHistory(msgs []any, keep int) (out []any, dropped int) {
	if keep <= 0 || len(msgs) == 0 {
		return msgs, 0
	}
	// 从尾部往前数第 keep 条 user 的下标即裁剪起点。
	start := -1
	seen := 0

	for i, m := range slices.Backward(msgs) {
		if messageRole(m) != "user" {
			continue
		}

		seen++
		if seen == keep {
			start = i

			break
		}
	}

	if start < 0 {
		// 本请求 user 消息数 <= keep：原样不动。
		return msgs, 0
	}
	// 头部只保留开头连续 system/developer；遇到第一条非 system/developer 即停。
	head := 0
	for head < start && isSystemRole(messageRole(msgs[head])) {
		head++
	}

	if head >= start {
		// 起点前全是提示词，没有可丢弃的内容。
		return msgs, 0
	}

	out = make([]any, 0, head+len(msgs)-start)
	out = append(out, msgs[:head]...)
	out = append(out, msgs[start:]...)

	return out, start - head
}

// trimHistoryInObj 对出站 body 应用按轮裁剪：切片 + 孤儿 tool 配对清理 + 一行 INFO。
// 必须在 tool 配对三步之后调用——先裁会让 repackToolResultBlocks 把已删的配对搬回来。
func trimHistoryInObj(obj map[string]any, keep int) {
	if keep <= 0 {
		return
	}

	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}

	trimmed, dropped := trimHistory(msgs, keep)
	if dropped <= 0 {
		return
	}
	// 安全网：裁剪可能打断跨边界的 tool 配对（极端形态：客户端把 tool 结果放在
	// user 之后），残留孤儿会让上游对之后每条消息都返 11148。
	trimmed, _ = cleanupOrphanToolCalls(trimmed)
	obj["messages"] = trimmed
	// 只记条数与保留轮数：绝不把消息正文写进日志。
	if modelName, isStr := obj["model"].(string); isStr && modelName != "" {
		log.Printf("INFO: [upstream] history trimmed: dropped=%d kept_turns=%d model=%s",
			dropped, keep, modelName)

		return
	}

	log.Printf("INFO: [upstream] history trimmed: dropped=%d kept_turns=%d", dropped, keep)
}

// messageRole 取 msg["role"] 字符串；非对象/缺失返回空串。
func messageRole(msg any) string {
	m, ok := msg.(map[string]any)
	if !ok {
		return ""
	}

	role, _ := m["role"].(string)

	return role
}

// isSystemRole 提示词类角色：system 与 developer（OpenAI 新口径）一律视同。
func isSystemRole(role string) bool {
	return role == "system" || role == "developer"
}
