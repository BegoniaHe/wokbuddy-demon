// browsercomplete.go 下游 agent 客户端的独立控制标记处理（issue #629）。
//
// `::browser-complete{...}` 是 assistant 正文里的一条控制标记，不是最终答案：把它当
// 正文透给客户端，浏览器回合会被判成已完成、提前收尾，用户看到「任务结束」而工作区
// 没有任何变化。官方 WorkBuddy 包（cn 5.11.3 / global 5.11.4）全树没有这个字面量，
// 语义来自下游 agent 客户端（Codex 侧浏览器工具）。
package upstream

import (
	"strings"

	"github.com/BegoniaHe/wokbuddy-demon/internal/jsonx"
)

// browserCompleteMarkerPrefix 独立控制标记的开头，后面跟 `{键="值"}` 参数体。
const browserCompleteMarkerPrefix = "::browser-complete{"

// BrowserCompleteOnly 报告正文是否就是一条独立控制标记（前后只允许空白）：只有这种
// 形态才吞掉。标记前面或后面还有正文时整段原样透传——标记混在回答里就是回答的一部分。
func BrowserCompleteOnly(text string) bool {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, browserCompleteMarkerPrefix) {
		return false
	}

	// 参数体没闭合（流被截断）不算标记：残缺的控制标记必须原样回吐，不能把正文吞掉。
	return strings.HasSuffix(trimmed, "}")
}

// BrowserCompletePossible 报告正文是否仍可能变成（或就是）一条独立控制标记：
// 一旦出现标记之外的正文就返回 false，暂存内容立刻放行。
func BrowserCompletePossible(text string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return true
	}

	// 标记还没打完（`::browse` 这类前缀）。
	if strings.HasPrefix(browserCompleteMarkerPrefix, trimmed) {
		return true
	}

	if !strings.HasPrefix(trimmed, browserCompleteMarkerPrefix) {
		return false
	}

	// 参数体闭合之后又跟了正文 → 已经不是单条标记。
	rest := trimmed[len(browserCompleteMarkerPrefix):]
	if i := strings.LastIndexByte(rest, '}'); i >= 0 {
		return i == len(rest)-1
	}

	return true
}

// LastMessageIsToolResult 报告请求体最后一条消息是否是工具结果（role=tool）。
// 与「本请求声明了 tools」一起构成浏览器完成标记的续写授权条件（issue #629）：
// 没有工具上下文的纯聊天请求只吞标记，不额外打一次上游。
func LastMessageIsToolResult(body []byte) bool {
	var req struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}

	if jsonx.Unmarshal(body, &req) != nil || len(req.Messages) == 0 {
		return false
	}

	return req.Messages[len(req.Messages)-1].Role == "tool"
}
