// rescue.go 响应形态判定入口（issue #494 / #495 / #570）。
//
// 上游「说到一半就停」有几种可分辨的形态：只有思维链没有正文、客户端要了长文但模型
// 自己收尾、正文停在预告句 / 承诺句、声明了工具却零调用。判定集中在这里，处置（关思考
// 重发 / 续写）仍复用既有收尾路径，不新增第二套写出逻辑。
package server

import (
	"strings"
	"unicode/utf8"

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
	// shapeNarrationTail 正文停在预告句 / 承诺句（标点型或无标点悬停）。
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

// 尾部形态判定的门槛（issue #570），流式与非流式共用。
const (
	// punctuationHoverMaxBytes 标点型悬停的长度闸门（只对这类形态生效）：预告句实测
	// 40~200B，400B 已覆盖真实样本；再长的「停在冒号上」多半是正常排版（说明 + 列表）。
	// 无标点悬停是句子被掐断，与正文长度无关——长会话里恰是最常见形态，不能按长度挡。
	punctuationHoverMaxBytes = 400
	// toolRoundFinalAnswerBytes 工具轮「够长」门槛：短句 + 句号收尾一律当作没写完
	// （模型该在这一步调工具），够长才按最后一句的收尾词 / 意图词分流。
	toolRoundFinalAnswerBytes = 120
	// userYieldTailBytes 判「等用户回话」时最多扫末尾这么长的窗口。
	userYieldTailBytes = 200
)

// tailHangPunct 标点型悬停的收尾符（后面必然还有东西）。
const tailHangPunct = "：:，,、—–…（(【["

// sentenceEnders 句末标点与收尾符：出现即视为「这句说完了」。
const sentenceEnders = "。！？!?\"'”’）)》】]}`*~&"

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
	if finishReason != "stop" || tools != 0 {
		return shapeNormal
	}

	if classifyTail(content, f.declaredToolCount > 0) != upstream.TailNone {
		// 声明了工具却只说不做（issue #495）：工具轮触发，续写只采纳工具调用。
		if f.declaredToolCount > 0 {
			return shapeToolDeclaredNoCall
		}

		return shapeNarrationTail
	}

	if f.longFormRequested() && len([]rune(content)) >= longFormContinuationMinChars {
		return shapeLongFormTruncated
	}

	return shapeNormal
}

// nonStreamContinuePlan 报告非流式聚合响应是否值得续写，以及是否只采纳工具调用、
// 尾部形态是什么（issue #494 / #495 / #570）：length 截断与长文 stop 续正文；预告句 /
// 承诺句续正文；声明了工具却零调用的工具轮只采纳续写里的 tool_calls。
func (f *chatFlow) nonStreamContinuePlan(
	resp, choice map[string]any,
) (wanted, toolsOnly bool, kind upstream.TailKind) {
	if finishReason, _ := choice["finish_reason"].(string); finishReason == "length" {
		return true, false, upstream.TailNone
	}

	shape := f.aggregateShape(resp)
	switch shape {
	case shapeLongFormTruncated:
		return true, false, upstream.TailNone
	case shapeNarrationTail, shapeToolDeclaredNoCall:
		message, _ := choice["message"].(map[string]any)
		content, _ := message["content"].(string)

		return true, shape == shapeToolDeclaredNoCall, classifyTail(content, f.declaredToolCount > 0)
	case shapeNormal, shapeReasoningOnly:
	}

	return false, false, upstream.TailNone
}

// classifyTail 判定正文尾部形态，决定是否续写、续写上限与指令（流式与非流式共用）：
//
//   - 结构未闭合（未闭合代码块）→ TailStructural：长度不限，但只补一次；
//   - 等用户回话（问句结尾 / 末句「请确认」类措辞）→ 不救 —— 这是故意的收尾，
//     替模型继续执行等于替用户拍板；
//   - 标点型悬停（冒号 / 逗号 / 顿号 / 破折号 / 省略号 / 左括号）→ TailPunctuated，
//     受长度闸门约束（那是排版习惯，正文越长越可能是正常排版）；
//   - 无标点悬停（末句是「我将…」「我会…」承诺句）→ TailPromise，与长度无关；
//   - 工具轮句号收尾 → 看最后一句：收尾词优先（仍算答完），否则意图词（没写完）。
func classifyTail(text string, toolRound bool) upstream.TailKind {
	trimmed := strings.TrimRight(text, " \t\r\n")
	if trimmed == "" {
		return upstream.TailNone
	}

	if structurallyOpen(trimmed) {
		return upstream.TailStructural
	}

	if textYieldsToUser(trimmed) {
		return upstream.TailNone
	}

	if punctuatedTail(trimmed) {
		if len(trimmed) > punctuationHoverMaxBytes {
			return upstream.TailNone
		}

		return upstream.TailPunctuated
	}

	if sentenceEnded(trimmed) {
		if toolRound {
			return toolRoundTail(trimmed)
		}

		return upstream.TailNone
	}

	if promiseSentence(trimmed) {
		return upstream.TailPromise
	}

	return upstream.TailNone
}

// structurallyOpen 报告尾部是否「结构上没写完」：围栏数量为奇数说明还缺收尾 ```。
func structurallyOpen(text string) bool {
	return strings.Count(strings.TrimRight(text, " \t\r\n"), "```")%2 == 1
}

// punctuatedTail 报告尾部是否停在标点型悬停的收尾符上。
func punctuatedTail(text string) bool {
	if strings.HasSuffix(text, "...") {
		return true
	}

	runes := []rune(text)

	return strings.ContainsRune(tailHangPunct, runes[len(runes)-1])
}

// sentenceEnded 报告尾部是否以句末标点 / 收尾符结束（这句已经说完）。
func sentenceEnded(text string) bool {
	trimmed := strings.TrimRight(text, " \t\r\n")
	if trimmed == "" {
		return false
	}

	runes := []rune(trimmed)

	return strings.ContainsRune(sentenceEnders, runes[len(runes)-1])
}

// promiseSentence 报告末句是否是「我将…/我会…」这类承诺句：模型说要动手却停在文字上。
func promiseSentence(text string) bool {
	last := lastSentence(text)
	if last == "" {
		return false
	}

	low := strings.ToLower(last)

	for _, marker := range promiseMarkers {
		if strings.Contains(low, marker) {
			return true
		}
	}

	return false
}

// promiseMarkers 末句出现这些词说明模型在预告下一步动作（承诺句形态）。
// 不收「下面」「接着」这类泛词：「下面这些文件都已改好。」是典型收尾。
var promiseMarkers = []string{
	"我将", "我会", "我来", "我继续", "接下来", "下一步", "现在开始", "现在修复", "再检查",
	"let me", "next, i'll", "next i'll", "next i will", "now i'll", "i'll now", "i will",
}

// toolRoundTail 工具轮句号收尾的分流：够长之后看最后一句——收尾词优先（仍算答完），
// 其次意图词（判为没写完）；短句一律按没写完处理（工具轮本该在这一步动手）。
func toolRoundTail(text string) upstream.TailKind {
	if len(text) < toolRoundFinalAnswerBytes {
		return upstream.TailPromise
	}

	last := lastSentence(text)

	for _, marker := range toolRoundClosingMarkers {
		if strings.Contains(last, marker) {
			return upstream.TailNone
		}
	}

	low := strings.ToLower(last)

	for _, marker := range toolRoundIntentMarkers {
		if strings.Contains(low, marker) {
			return upstream.TailPromise
		}
	}

	return upstream.TailNone
}

// toolRoundIntentMarkers 最后一句里出现这些词，说明模型在说「下一步要动手」而不是收尾。
// 取自线上漏救样本（「我继续检查其他被错误替换的内容，例如 UnityManual.html 链接。」）。
var toolRoundIntentMarkers = []string{
	"我继续", "接下来", "下一步", "我来", "现在开始", "现在修复", "再检查",
	"next, i'll", "next i'll", "next i will", "let me", "now i'll", "i'll now",
}

// toolRoundClosingMarkers 收尾词：最后一句出现就一律当「已答完」，压过意图词
// （「任务已全部完成，我来总结一下。」的意图词是「我来」，但整句是收尾）。
var toolRoundClosingMarkers = []string{
	"总结", "已完成", "全部完成", "已经完成", "完毕", "收尾", "到这里",
}

// lastSentence 取最后一句（去掉尾部句末标点）。
// 只按句末标点切、不按逗号切：「检查完了，我继续修下一个」整句都算最后一句，
// 切成半句反而会把意图词切走。ASCII 句点后面必须是空白或行尾才算分隔点，
// 否则文件名（internal/verify/check.go）会被切成「go」。
func lastSentence(text string) string {
	runes := []rune(strings.TrimRight(text, " \t\r\n"))
	end := len(runes)

	for end > 0 && strings.ContainsRune("。！？!?.", runes[end-1]) {
		end--
	}

	start := 0

	for i := end - 1; i >= 0; i-- {
		if sentenceBoundary(runes, i) {
			start = i + 1

			break
		}
	}

	return strings.TrimSpace(string(runes[start:end]))
}

// sentenceBoundary 报告该位置是否是句末分隔点（只用于切最后一句）。
func sentenceBoundary(runes []rune, i int) bool {
	switch runes[i] {
	case '。', '！', '？', '!', '?', '\n':
		return true
	case '.':
		return i+1 >= len(runes) || runes[i+1] == ' ' || runes[i+1] == '\n'
	}

	return false
}

// userYieldMarkers 末句出现这些措辞说明模型把控制权交回用户（等拍板 / 等补充信息）。
//
// 救援的语义是「模型没说完，替它补上」，但有一类收尾是故意的：模型在等用户确认
// （「邮件已经写好，请确认后我再发送」）。这时补一句「继续执行下一步」会拿到工具调用
// 并被采纳，等于替用户按下了发送键——宁可漏救，也不能替用户拍板。
var userYieldMarkers = []string{
	"请确认", "确认后", "等你确认", "待你确认", "等你回复", "是否需要", "需不需要",
	"要不要我", "要不要", "你希望", "请告知", "让我知道", "请你确认", "你确认后",
	"确认无误", "可以吗", "行吗", "好吗",
}

// userYieldMarkersEN 英文形态（模型偶发用英文收尾）。
var userYieldMarkersEN = []string{
	"please confirm", "let me know", "should i ", "do you want", "would you like",
	"awaiting your", "waiting for your", "confirm before", "shall i ",
}

// textYieldsToUser 报告正文是否在「等用户回话」：以问号收尾，或末句含请确认类措辞。
//
// 只判末句窗口：整段 Contains 会被正文中段的技术叙述误伤（真实样本里「是否需要
// 更换钢丝绳」距尾部三千余字），长正文几乎必然命中，等于长会话永远不救。
func textYieldsToUser(text string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return false
	}

	if strings.HasSuffix(trimmed, "？") || strings.HasSuffix(trimmed, "?") {
		return true
	}

	tail := userYieldTail(trimmed)

	for _, marker := range userYieldMarkers {
		if strings.Contains(tail, marker) {
			return true
		}
	}

	low := strings.ToLower(tail)

	for _, marker := range userYieldMarkersEN {
		if strings.Contains(low, marker) {
			return true
		}
	}

	return false
}

// userYieldTail 取「最后一次句读之后」的窗口：交回控制权总是最后一句里的事。
func userYieldTail(text string) string {
	trimmed := strings.TrimRight(text, "。！？!?\n ")

	if i := strings.LastIndexAny(trimmed, "。！？!?\n"); i >= 0 {
		_, size := utf8.DecodeRuneInString(trimmed[i:])
		trimmed = trimmed[i+size:]
	}

	return tailChars(strings.TrimSpace(trimmed), userYieldTailBytes)
}

// tailChars 取文本末尾至多 n 个字符。
func tailChars(text string, n int) string {
	runes := []rune(text)
	if len(runes) <= n {
		return text
	}

	return string(runes[len(runes)-n:])
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
