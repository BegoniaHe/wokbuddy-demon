// rescue.go 响应形态判定入口（issue #494 / #495）。
//
// 上游「说到一半就停」有几种可分辨的形态：只有思维链没有正文、客户端要了长文但模型
// 自己收尾、正文停在预告句、声明了工具却零调用。判定集中在这里，处置（关思考重发 /
// 续写）仍复用既有收尾路径，不新增第二套写出逻辑。
package server

import (
	"strings"

	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// responseShape 上游响应的异常形态。
type responseShape int

const (
	// shapeNormal 正常响应，按原样收尾。
	shapeNormal responseShape = iota
	// shapeReasoningOnly 只有思维链，没有正文与工具调用。
	shapeReasoningOnly
	// shapeLongFormTruncated 客户端要了长文（≥32k 输出上限），模型却以 stop 提前收尾。
	shapeLongFormTruncated
	// shapeNarrationTail 正文停在预告句（结尾为冒号/省略号/未闭合代码块）。
	shapeNarrationTail
	// shapeToolDeclaredNoCall 声明了工具、预告句收尾却零工具调用（工具轮「说一句就停」）。
	shapeToolDeclaredNoCall
)

// longFormContinuationMinTokens 长文续接的请求侧门槛：客户端自己把输出上限要到
// 32k 以上，说明它期望长输出，「模型自己停」不再等同于「用户只要这么短」。
//
// 必须在**客户端原始 body** 上判：出站前 prepareBody 会按模型目录补默认 max_tokens，
// 拿注入后的值会把所有请求都判成长文。
const longFormContinuationMinTokens = 32000

// longFormContinuationMinChars 长文续接的响应侧门槛：已经写出成篇内容却停在 stop
// 才按「说了一半」处理。短答 stop 是正常收尾——尤其 /v1/responses 适配器在客户端
// 不传 max_output_tokens 时会按模型目录补 32k，只看请求上限会把每一句完整回答都
// 续写一遍。
const longFormContinuationMinChars = 2000

// longFormRequested 报告本次请求是否达到长文续接门槛。
func (f *chatFlow) longFormRequested() bool {
	return maxOutputTokensFromBody(f.body) >= longFormContinuationMinTokens
}

// streamShape 首帧否决窗口的形态判定：只思考没正文单列一档，其余交给既有分支。
func streamShape(probe *upstream.StreamProbe) responseShape {
	if probe != nil && probe.Empty && probe.SawReasoning {
		return shapeReasoningOnly
	}

	return shapeNormal
}

// aggregateShape 非流式聚合响应的形态判定。
//
//nolint:cyclop,gocyclo // 形态按优先级线性判定，拆散会打乱「先零输出、后预告句、再长文」的次序。
func (f *chatFlow) aggregateShape(resp map[string]any) responseShape {
	choice := firstChoice(resp)
	if choice == nil {
		return shapeNormal
	}

	message, _ := choice["message"].(map[string]any)
	if message == nil {
		return shapeNormal
	}

	content, _ := message["content"].(string)

	tools := responseToolCallCount(resp)

	if reasoningOnly(content, tools, message) {
		return shapeReasoningOnly
	}

	finishReason, _ := choice["finish_reason"].(string)
	if finishReason == "stop" && tools == 0 && narrationTail(content) {
		// 声明了工具却只说不做（issue #495）：工具轮触发，续写只采纳工具调用。
		if f.declaredToolCount > 0 {
			return shapeToolDeclaredNoCall
		}

		return shapeNarrationTail
	}

	if finishReason == "stop" && tools == 0 && f.longFormRequested() &&
		len([]rune(content)) >= longFormContinuationMinChars {
		return shapeLongFormTruncated
	}

	return shapeNormal
}

// nonStreamContinuePlan 报告非流式聚合响应是否值得续写一次，以及是否只采纳工具
// 调用（issue #494 / #495）：length 截断与长文 stop 续正文；预告句续正文；声明了
// 工具却零调用的工具轮只采纳续写里的 tool_calls。
func (f *chatFlow) nonStreamContinuePlan(resp, choice map[string]any) (wanted, toolsOnly bool) {
	if finishReason, _ := choice["finish_reason"].(string); finishReason == "length" {
		return true, false
	}

	switch f.aggregateShape(resp) {
	case shapeLongFormTruncated, shapeNarrationTail:
		return true, false
	case shapeToolDeclaredNoCall:
		return true, true
	case shapeNormal, shapeReasoningOnly:
	}

	return false, false
}

// narrationTail 报告正文是否停在预告句：结尾是冒号、省略号，或代码块未闭合。
// 只用于「模型自己 stop」的收尾判定；length 截断由既有口径处理。
//
// 门槛取得窄：完整回答极少以冒号/省略号收尾，而这三类形态正是「后面还有内容」
// 的信号。宁可漏（交回原样下发）也不误续——续写会多打一次上游。
func narrationTail(text string) bool {
	trimmed := strings.TrimRight(text, " \t\r\n")
	if trimmed == "" {
		return false
	}

	// 未闭合代码块：围栏数量为奇数说明还缺收尾 ```。
	if strings.Count(trimmed, "```")%2 == 1 {
		return true
	}

	for _, suffix := range []string{":", "：", "...", "…"} {
		if strings.HasSuffix(trimmed, suffix) {
			return true
		}
	}

	return false
}

// reasoningOnly 报告聚合消息是否「只有思维链」：无正文、无工具调用，但有思维链。
func reasoningOnly(content string, toolCalls int, message map[string]any) bool {
	if content != "" || toolCalls > 0 {
		return false
	}

	for _, key := range []string{"reasoning_content", "reasoning"} {
		if reasoning, _ := message[key].(string); reasoning != "" {
			return true
		}
	}

	return false
}
