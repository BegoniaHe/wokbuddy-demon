// continue.go 截断续写：上游以 finish_reason=length 结束且已产生正文时，自动用
// 同账号同模型发起一次续写，把后续正文拼接到同一 SSE 流里；正文停在预告句 /
// 承诺句上（issue #495 / #570）时也走同一条续写路径，续写指令按尾部形态选择。
//
// 约束（issue #247 / #570）：
//   - 只续写正文；工具调用（尤其是参数未闭合）不盲重试；
//   - 续写请求由调用方经 ChatStreamContext → prepareBody 发起（本文件只负责判定与拼接）；
//   - 次数上限：预告句 / 承诺句 2 次，结构未闭合与 length 截断 1 次；
//   - 续写失败时把首段的终态原样交给客户端；
//   - 续写帧复用首段的 id/model，客户端看到的是同一条流。
package upstream

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/BegoniaHe/wokbuddy-demon/internal/jsonx"
)

// TailKind 续写触发时正文尾部的形态（由调用方探针判定，见 server 侧 classifyTail）。
// 形态决定续写的上限与追加的续写指令；TailNone 表示不是尾部触发的续写（length 截断）。
type TailKind int

const (
	// TailNone 非尾部触发（length 截断 / 长文 stop）。
	TailNone TailKind = iota
	// TailPunctuated 标点型悬停：停在冒号/逗号/顿号/破折号/省略号/左括号上（排版习惯）。
	TailPunctuated
	// TailPromise 无标点悬停：承诺句 / 工具轮意图句被掐断，与正文长度无关。
	TailPromise
	// TailStructural 结构未闭合：未闭合代码块或截断的调用标记。
	TailStructural
)

// String 形态的日志标签。
func (k TailKind) String() string {
	switch k {
	case TailPunctuated:
		return "punctuated"
	case TailPromise:
		return "promise"
	case TailStructural:
		return "structural"
	case TailNone:
		return "none"
	}

	return "none"
}

// ContinueOpener 发起一次续写请求；partial 是已输出的正文，kind 是触发形态
// （决定续写指令文案，见 continuationInstructionFor）。
type ContinueOpener func(partial string, kind TailKind) (io.ReadCloser, error)

// TextContinueReader 在检测到 finish_reason=length + 正文后切换到续写流。
type TextContinueReader struct {
	src        io.ReadCloser
	reader     *bufio.Reader
	open       ContinueOpener
	text       strings.Builder
	hasTool    bool
	out        bytes.Buffer
	held       []byte
	firstID    string
	firstMdl   string
	firstUsage map[string]any

	finishLength bool
	// stopTriggers 把 finish_reason=stop 也当作可续写终态（长文续接 / 预告句，见
	// ContinueOnStop 与 ContinueOnStopTail）。
	stopTriggers bool
	// stopMinChars stop 触发续写所需的已输出字符数（长文口径的短答门槛）。
	stopMinChars int
	// stopTail 尾部探针：非 nil 时 stop 是否可续写由它判定（预告句 / 工具轮，#495）。
	stopTail func(string) TailKind
	// stopKind 本次 stop 触发时的尾部形态（由探针给出）。
	stopKind TailKind
	// stopByTail 本次 stop 是尾部探针授权的：跳过 stopMinChars 的字符门槛。
	stopByTail bool
	// toolsOnly 工具轮续写：只采纳续写段的 tool_calls，丢弃正文。
	toolsOnly bool
	// continuationToolsOnly 本次实际生效的「只采纳调用」（length 触发的仍续正文）。
	continuationToolsOnly bool
	// continuationHasTool 续写段是否已带回工具调用。
	continuationHasTool bool
	// noMoreContinue 续写段已被判定为「已完成」声明：不再发起救援（issue #589）。
	noMoreContinue bool
	// segmentStart 本次续写段开始时的正文长度（字节），用于取本段新增的正文。
	segmentStart int
	// pending 续写段里尚未确定归属的正文帧（已按续写口径重编码，顺序即写出顺序）。
	// 模型按旧习惯只回「已完成」时这些帧不能下发，判定为真实正文才按序放行。
	pending []string
	// settled 本段已确定是真实正文（或已按声明收尾）：后续帧直接写出。
	settled bool
	// onContinue 真正发起续写时的诊断回调（收到已输出正文的尾部片段与触发形态）。
	onContinue func(tail string, kind TailKind)
	// finishStop 本次终态是 stop（且已开启 stopTriggers）：与 finishLength 同处置。
	finishStop bool
	// continueTries 本请求已发起的续写次数（上限见 tryContinue）。
	continueTries int
	continued     bool
	continuation  bool
	// streamGen 每次切换底层流（续写）+1：Read 用它判断「本行处理期间流已换」，
	// 而不是比较 reader 指针——切换实现为同一条 bufio Reset，指针不变。
	streamGen uint64
	done      bool
	released  bool
	err       error
}

// continueReaderPool 续写层 64KB 行缓冲的复用池。
//
// 归还点不能放在 Close：keepalive 路径会在读 goroutine 仍阻塞在上游 Read 时先
// Close 底层流解阻塞（见 streamWithKeepalive），此刻把缓冲放回池就会与仍在读的
// goroutine 撞车。调用方必须在 Stream 返回后调用 Release——那时 Stream 已保证
// 读 goroutine 退出。
var continueReaderPool = sync.Pool{
	New: func() any { return bufio.NewReaderSize(nil, 64*1024) },
}

// promptUsageKeys 是续写合并时取末段、不累加的键。续写请求已带上前文，
// prompt/cache 再加会按段数重复计量（issue #432）。
var promptUsageKeys = map[string]struct{}{
	"prompt_tokens":            {},
	"input_tokens":             {},
	"prompt_tokens_details":    {},
	"input_tokens_details":     {},
	"cached_tokens":            {},
	"cache_read_input_tokens":  {},
	"prompt_cache_hit_tokens":  {},
	"prompt_cache_miss_tokens": {},
}

// NewTextContinueReader 包装上游流。initialText 是 ProbeStream 已消费的正文。
//
// 行缓冲从 continueReaderPool 取（64KB，每流一次）；调用方在 Stream 返回后必须
// 调 Release 归还，见该方法的说明。
func NewTextContinueReader(
	src io.ReadCloser, initialText string, hasTool bool, open ContinueOpener,
) *TextContinueReader {
	reader, _ := continueReaderPool.Get().(*bufio.Reader)
	if reader == nil {
		reader = bufio.NewReaderSize(nil, 64*1024)
	}

	reader.Reset(src)

	r := &TextContinueReader{
		src: src, reader: reader, open: open, hasTool: hasTool,
	}
	r.text.WriteString(initialText)

	return r
}

// ContinueOnStop 让 finish_reason=stop 也触发一次续写（issue #494 长文续接）。
// 只在调用方已判定「客户端要的是长文」时开启；minChars 是响应侧门槛——已写出成篇
// 内容却停在 stop 才续写，短答 stop 仍按原样下发。
func (c *TextContinueReader) ContinueOnStop(minChars int) *TextContinueReader {
	if c != nil {
		c.stopTriggers = true
		c.stopMinChars = minChars
	}

	return c
}

// ContinueOnStopTail 让「正文停在预告句 / 承诺句」的 stop 触发续写（issue #495 / #570）。
// tail 探针作用于已输出正文并返回尾部形态；toolsOnly 为真表示本轮声明了工具——只采纳
// 续写段里的 tool_calls，丢弃续写正文（工具轮「说一句就停」的补救）。onTrigger 在真正
// 发起续写时收到已输出正文的尾部片段与形态，供调用方落诊断日志（可能为 nil）。
func (c *TextContinueReader) ContinueOnStopTail(
	tail func(string) TailKind, toolsOnly bool, onTrigger func(tail string, kind TailKind),
) *TextContinueReader {
	if c != nil {
		c.stopTriggers = true
		c.stopTail = tail
		c.toolsOnly = toolsOnly
		c.onContinue = onTrigger
	}

	return c
}

// Continued 报告本流是否发生过续写（诊断日志用）。
func (c *TextContinueReader) Continued() bool {
	return c != nil && c.continued
}

// ContinuationToolCall 报告续写段是否带回工具调用（工具轮诊断用）。
func (c *TextContinueReader) ContinuationToolCall() bool {
	return c != nil && c.continuationHasTool
}

// Release 归还行缓冲（幂等）。只能在整个流确实读完之后调用：调用方在 Stream
// 返回后调——那之后本 reader 不会再被读。
func (c *TextContinueReader) Release() {
	if c == nil || c.released {
		return
	}

	c.released = true
	c.done = true
	c.reader.Reset(nil)
	continueReaderPool.Put(c.reader)
}

// Read 实现 io.Reader：优先返回内部缓冲，行级解析并改写终态。
//
//nolint:wrapcheck // io.Reader 实现直接返回底层错误。
func (c *TextContinueReader) Read(dst []byte) (int, error) {
	for c.out.Len() == 0 {
		if c.done {
			if c.err != nil {
				return 0, c.err
			}

			return 0, io.EOF
		}

		genBefore := c.streamGen
		line, err := ReadSSELine(c.reader)

		// 单行超限（issue #113）：与 stream / Aggregate 同口径，按上游失败上抛。
		// 此前这里用 ReadString，超长无换行行会被无上限拼接，是本路径唯一的
		// 无界内存点；超限后也不尝试续写（续写段同样在此处读取）。
		if errors.Is(err, ErrSSELineTooLong) {
			c.err = err
			c.done = true

			continue
		}

		c.handleLine(line)

		if err != nil && !c.afterReadError(genBefore, err) {
			continue
		}
	}

	return c.out.Read(dst)
}

// afterReadError 处理读错误并返回是否继续读取同一循环。
//
// handleLine 内已成功切到续写流时（streamGen 变化），旧流的 EOF/读错误不得终止
// 新的读取循环：否则续写段会被直接丢弃（无尾随换行的 [DONE] 尤其常见），故返回
// false 让调用方继续读新流。其余情况按 EOF / 真错误收尾，补发暂存帧与续写也由
// handleEOF 负责。
func (c *TextContinueReader) afterReadError(genBefore uint64, err error) bool {
	if c.streamGen != genBefore {
		return false
	}

	c.handleEOF()

	if !errors.Is(err, io.EOF) {
		c.err = err
	}

	return true
}

// Close 关闭当前上游流。
func (c *TextContinueReader) Close() error {
	if c.src == nil {
		return nil
	}

	return c.src.Close()
}

// handleLine 处理一行：非 data 行原样透传，data 行按 [DONE] / JSON 帧分派。
func (c *TextContinueReader) handleLine(line string) {
	payload, isData := sseDataPayload(strings.TrimRight(line, "\r\n"))
	if !isData {
		c.out.WriteString(line)

		return
	}

	if sseDone(payload) {
		c.handleDone(line)

		return
	}

	var frame map[string]any
	if jsonx.Unmarshal([]byte(payload), &frame) != nil || frame == nil {
		// 非 JSON 帧与顶层 null（nil map 没有可写字段，续写重写 id/model 会 panic）
		// 一律原样透传。
		c.out.WriteString(line)

		return
	}

	c.handleFrame(line, frame)
}

// handleDone 处理 [DONE]：首段因 length 截断且尚未续写时，先换流续写并吞掉本行；
// 否则原行透传。
func (c *TextContinueReader) handleDone(line string) {
	c.settleSegment()

	if c.continueTriggered() && c.tryContinue() {
		return
	}

	// 工具轮续写没带回任何调用时，首段终态必须补在 [DONE] 之前（#495）。
	if c.continued && c.continuationToolsOnly && !c.continuationHasTool {
		c.flushHeld()
	}

	c.out.WriteString(line)
}

// handleFrame 处理 JSON 帧：错误帧原样重编码，正常帧按是否需要改写决定透传方式。
func (c *TextContinueReader) handleFrame(line string, frame map[string]any) {
	c.captureMeta(frame)

	if frameIsError(frame) {
		c.writeFrameJSON(frame)

		return
	}

	c.inspectFrame(frame)

	if c.continueTriggered() {
		c.holdLengthFrame(line, frame)

		return
	}

	// 非续写段不需要改写 id/model/usage：整行原样透传，省掉每帧一次 marshal
	// （续写段才走 writeFrameJSON 的重编码）。
	if !c.continuation {
		c.out.WriteString(line)

		return
	}

	// 工具轮续写只采纳调用（issue #495）：正文/思考帧丢弃；终态帧只在已采纳调用后
	// 才写出，否则由首段 stop（held）保底。用量仍要记账，剥掉终态后写出。
	if c.continuationToolsOnly {
		switch {
		case frameHasToolCalls(frame):
			c.continuationHasTool = true
		case !c.continuationHasTool:
			if frameHasUsage(frame) {
				c.writeFrameJSON(stripFinishReason(frame))
			}

			return
		case !frameHasFinish(frame):
			return
		}
	}

	if c.holdUntilSettled(frame) {
		return
	}

	c.writeFrameJSON(frame)
}

// holdLengthFrame 在续写前暂存终态：续写成功则丢弃，失败则补发。
// usage 帧立即写出并剥掉 finish_reason——hold 会被后续帧覆盖或在续写成功时
// 丢弃（用量/积分漏记），直接写出终态则会让客户端提前结束响应。
//
// 与 usage / 正文同帧的终态必须按原值暂存（length 与 stop 都算）：上游实测会把
// usage 与终态放同一帧，漏存 stop 后若续写段又没带回工具调用，整条响应就再也
// 没有 finish_reason（工具轮兜底依赖这里补发，见 flushHeld）。
func (c *TextContinueReader) holdLengthFrame(line string, frame map[string]any) {
	if frameHasUsage(frame) {
		c.writeFrameJSON(stripFinishReason(frame))

		if frameHasFinish(frame) {
			c.held = append(c.held, finishOnlyFrameLine(frame)...)
		}

		return
	}

	if frameHasSubstantive(frame) {
		// 终态帧同时带正文（少见）：正文先按普通内容写出，再暂存 finish-only 帧。
		c.writeFrameJSON(stripFinishReason(frame))

		if frameHasFinish(frame) {
			c.held = append(c.held, finishOnlyFrameLine(frame)...)
		}

		return
	}

	c.held = append(c.held, line...)
}

// frameHasSubstantive 报告帧是否携带正文/拒答/工具调用。
func frameHasSubstantive(frame map[string]any) bool {
	return frameChoiceNodeHas(frame, func(node map[string]any) bool {
		if text, _ := node["content"].(string); text != "" {
			return true
		}

		if text, _ := node["refusal"].(string); text != "" {
			return true
		}

		return nodeHasToolCalls(node)
	})
}

// frameHasToolCalls 报告帧是否携带工具调用（delta 或 message）。
func frameHasToolCalls(frame map[string]any) bool {
	return frameChoiceNodeHas(frame, nodeHasToolCalls)
}

// nodeHasToolCalls 报告 delta/message 节点是否带工具调用。
func nodeHasToolCalls(node map[string]any) bool {
	tools, _ := node["tool_calls"].([]any)

	return len(tools) > 0
}

// frameChoiceNodeHas 报告任一 choice 的 delta/message 是否满足 ok。
func frameChoiceNodeHas(frame map[string]any, ok func(node map[string]any) bool) bool {
	found := false

	forEachChoice(frame, func(choice map[string]any) {
		for _, key := range []string{"delta", "message"} {
			node, _ := choice[key].(map[string]any)
			if node == nil {
				continue
			}

			if ok(node) {
				found = true
			}
		}
	})

	return found
}

// frameHasFinish 报告帧是否带非空 finish_reason。
func frameHasFinish(frame map[string]any) bool {
	return frameHasFinishReason(frame, "")
}

// frameHasUsage 报告帧是否携带非空 usage 对象。
func frameHasUsage(frame map[string]any) bool {
	usage, ok := frame["usage"].(map[string]any)

	return ok && len(usage) > 0
}

// frameHasFinishReason 报告帧是否带终态；want 为空表示任意非空终态。
func frameHasFinishReason(frame map[string]any, want string) bool {
	choices, _ := frame["choices"].([]any)

	for _, raw := range choices {
		choice, _ := raw.(map[string]any)
		if choice == nil {
			continue
		}

		fr, _ := choice["finish_reason"].(string)
		if fr == "" || (want != "" && fr != want) {
			continue
		}

		return true
	}

	return false
}

// stripFinishReason 复制帧并把所有 finish_reason 置空（保留内容帧）。
func stripFinishReason(frame map[string]any) map[string]any {
	out, _ := deepCopyMap(frame)
	choices, _ := out["choices"].([]any)

	for _, raw := range choices {
		choice, _ := raw.(map[string]any)
		if choice != nil {
			choice["finish_reason"] = nil
		}
	}

	return out
}

// finishOnlyFrameLine 构造只保留终态（delta 置空）的 data 行：finish_reason 沿用
// 原帧的值——length 仍是 length，stop 仍是 stop。
func finishOnlyFrameLine(frame map[string]any) []byte {
	out, _ := deepCopyMap(frame)
	// 终态帧的 usage 已由 holdLengthFrame 单独写出（并已按续写口径合并）；暂存副本
	// 不能再带 usage，否则补发时会用首段原值覆盖续写段的合并值（多段用量回退）。
	delete(out, "usage")
	choices, _ := out["choices"].([]any)

	for _, raw := range choices {
		choice, _ := raw.(map[string]any)
		if choice == nil {
			continue
		}

		choice["delta"] = map[string]any{}
		delete(choice, "message")
	}

	raw, err := jsonx.Marshal(out)
	if err != nil {
		return nil
	}

	return []byte("data: " + string(raw) + "\n\n")
}

// deepCopyMap 经 JSON 往返复制（帧结构小，续写只在截断时触发）。
func deepCopyMap(src map[string]any) (map[string]any, error) {
	raw, err := jsonx.Marshal(src)
	if err != nil {
		return src, fmt.Errorf("deepCopyMap marshal: %w", err)
	}

	var out map[string]any
	if err := jsonx.Unmarshal(raw, &out); err != nil {
		return src, fmt.Errorf("deepCopyMap unmarshal: %w", err)
	}

	return out, nil
}

// captureMeta 记录首段 id/model，供续写段复用。
func (c *TextContinueReader) captureMeta(frame map[string]any) {
	if c.firstID == "" {
		if id, ok := frame["id"].(string); ok {
			c.firstID = id
		}
	}

	if c.firstMdl == "" {
		if model, ok := frame["model"].(string); ok {
			c.firstMdl = model
		}
	}
}

// inspectFrame 累计正文/工具调用并读取 finish_reason。
//
//nolint:cyclop,gocyclo // stop 可续写判定按「尾部探针 → 长文门槛」两段线性展开。
func (c *TextContinueReader) inspectFrame(frame map[string]any) {
	if !c.continuation {
		// 首段 usage 在续写成功后仍是独立一次上游调用，不能随 length 帧一起丢弃；
		// 续写段带 usage 时把它合并进去（末帧口径，与 statsReader 一致）。
		if usage, ok := frame["usage"].(map[string]any); ok && len(usage) > 0 {
			c.firstUsage = usage
		}
	}

	sawStop := false

	walkChoiceDeltas(frame, func(fr string) {
		switch fr {
		case "length":
			c.finishLength = true
		case "stop":
			sawStop = true
		}
	}, c.inspectDelta)

	if !sawStop || !c.stopTriggers {
		return
	}

	// 给了尾部探针就按它判（预告句 / 承诺句 / 工具轮，#495 / #570）；否则沿用长文
	// 口径——stop 一律可续写，短答由 tryContinue 的字符门槛挡掉（#494）。
	// 续写段再次停在预告句上同样走这里：救援次数由 tryContinue 的上限控制。
	if c.stopTail == nil {
		c.finishStop = true

		return
	}

	if kind := c.stopTail(c.text.String()); kind != TailNone {
		c.finishStop = true
		c.stopByTail = true
		c.stopKind = kind

		return
	}

	// 两个策略共存：尾部探针不命中时，非工具轮仍按长文门槛续写（stopMinChars=0
	// 表示未开启长文续接，不在此放行）。
	if !c.toolsOnly && c.stopMinChars > 0 &&
		utf8.RuneCountInString(c.text.String()) >= c.stopMinChars {
		c.finishStop = true
	}
}

// forEachChoice 遍历帧内非空 choices。
func forEachChoice(frame map[string]any, visit func(choice map[string]any)) {
	choices, _ := frame["choices"].([]any)

	for _, raw := range choices {
		choice, _ := raw.(map[string]any)
		if choice == nil {
			continue
		}

		visit(choice)
	}
}

// walkChoiceDeltas 遍历 choices，回调 finish_reason 与 delta/message。
func walkChoiceDeltas(frame map[string]any, handleFinish func(string), inspect func(map[string]any)) {
	forEachChoice(frame, func(choice map[string]any) {
		if fr, ok := choice["finish_reason"].(string); ok {
			handleFinish(fr)
		}

		if delta, ok := choice["delta"].(map[string]any); ok {
			inspect(delta)
		}

		if msg, ok := choice["message"].(map[string]any); ok {
			inspect(msg)
		}
	})
}

// inspectDelta 累计正文与工具调用。
func (c *TextContinueReader) inspectDelta(delta map[string]any) {
	if text, ok := delta["content"].(string); ok {
		c.appendText(text)
	}

	if refusal, ok := delta["refusal"].(string); ok {
		c.appendText(refusal)
	}

	if tools, ok := delta["tool_calls"].([]any); ok && len(tools) > 0 {
		c.hasTool = true
	}
}

// appendText 累计正文并施加上限（与聚合路径的 maxSSEAggregateBytes 同口径）。
//
// 续写会把整段正文回灌给第二次上游调用：无界累积等于同一份内容在内存里翻倍。
// 超限即停止累积并置 hasTool（语义为「不再续写」），只保留已经写出的帧——放弃
// 续写是最小影响的降级，静默截断回灌内容反而会改坏回答。
func (c *TextContinueReader) appendText(text string) {
	if c.text.Len()+len(text) > maxSSEAggregateBytes {
		c.hasTool = true

		return
	}

	c.text.WriteString(text)
}

// writeFrameJSON 写回数据帧；续写段复用首段 id/model。
func (c *TextContinueReader) writeFrameJSON(frame map[string]any) {
	c.out.WriteString(c.frameLine(frame))
}

// frameLine 把帧按续写口径重编码成一行 data；编码失败返回空串（不写出）。
func (c *TextContinueReader) frameLine(frame map[string]any) string {
	c.rewriteContinuationFrame(frame)

	raw, err := jsonx.Marshal(frame)
	if err != nil {
		return ""
	}

	return "data: " + string(raw) + "\n\n"
}

// frameIsError 报告帧是否是错误帧（顶层 error 字段，或业务错误码）。
func frameIsError(frame map[string]any) bool {
	if _, hasErr := frame["error"]; hasErr {
		return true
	}

	_, _, isErr := frameBusinessError(frame)

	return isErr
}

// holdUntilSettled 处理续写段「先暂存、再放行」的一步（issue #589）：正文帧暂存并返回
// 真（本帧不要写出），非正文帧（终态 / usage）先收尾暂存帧以保持写出顺序并返回假。
func (c *TextContinueReader) holdUntilSettled(frame map[string]any) bool {
	if !c.continuation || c.settled {
		return false
	}

	if frameTextOnly(frame) {
		c.holdDeclarationFrame(frame)

		return true
	}

	// 元数据帧（role-only 首帧、只带思考的帧、空 delta）不代表本段正文结束：放行且
	// 不收尾。真机续写段常以 `delta.role` 开帧，收尾会把「先暂存再判定」整个关掉，
	// 「已完成」照样写进客户端正文（#589）。
	if !frameEndsSegment(frame) {
		return false
	}

	c.settleSegment()

	return false
}

// frameEndsSegment 报告帧是否代表本段正文已经结束（终态 / 工具调用 / 用量）：
// 只有这类帧才需要先把暂存的正文定性放行，再写出自己。
func frameEndsSegment(frame map[string]any) bool {
	return frameHasToolCalls(frame) || frameHasFinish(frame) || frameHasUsage(frame)
}

// frameTextOnly 报告帧只携带正文（不带工具调用）：只有这种帧可以暂存后丢弃。
func frameTextOnly(frame map[string]any) bool {
	return frameHasText(frame) && !frameHasToolCalls(frame)
}

// frameHasText 报告帧是否携带正文（delta.content / message.content）。
func frameHasText(frame map[string]any) bool {
	return frameChoiceNodeHas(frame, func(node map[string]any) bool {
		text, _ := node["content"].(string)

		return text != ""
	})
}

// rewriteContinuationFrame 把续写帧的 id/model 对齐首段，并把已累计的 usage 并进
// 续写帧：prompt/cache 留末段，completion/credit 累加，total 重算。续写是第二次
// 上游调用，只报本段会漏记前面各段完成量（issue #247 / #432，与非流式同口径）。
func (c *TextContinueReader) rewriteContinuationFrame(frame map[string]any) {
	if !c.continuation {
		return
	}

	if c.firstID != "" {
		frame["id"] = c.firstID
	}

	if c.firstMdl != "" {
		frame["model"] = c.firstMdl
	}

	if usage, ok := frame["usage"].(map[string]any); ok && len(usage) > 0 {
		if c.firstUsage != nil {
			mergeUsageNumbers(usage, c.firstUsage)
			recomputeMergedUsageTotal(usage)
		}
		// 多段续写（issue #570 起预告句最多救两次）把累计值留作下一段基线：只留住
		// 首段会让第三段漏记中间段用量，末帧反而比上一帧少。
		c.firstUsage = usage
	}
}

// continueTriggered 报告本次终态是否已判定为可续写（length，或开启后的 stop）。
func (c *TextContinueReader) continueTriggered() bool {
	return c.finishLength || c.finishStop
}

// handleEOF 流结束：有暂存终态时尝试续写；否则收尾。
func (c *TextContinueReader) handleEOF() {
	c.settleSegment()

	if c.continueTriggered() {
		if c.tryContinue() {
			return
		}

		if len(c.held) > 0 {
			c.out.Write(c.held)
			c.held = nil
		}
	}

	// 工具轮续写没带回调用：流尾补发首段 stop（[DONE] 路径已在 handleDone 处理）。
	if c.continued && c.continuationToolsOnly && !c.continuationHasTool {
		c.flushHeld()
	}

	c.done = true
}

// 每个请求的续写次数上限。
const (
	// narrationMaxRescues 预告句 / 承诺句最多救几次。2 而不是 1：长正文的模型常连写
	// 两段预告（「我先说明结论，然后我将开始修改…」，再停一次），只给一次机会时第二
	// 段又原样回到客户端。上限仍在，避免「反复预告」把请求拖成无限续写。
	narrationMaxRescues = 2
	// singleShotRescues 结构未闭合与 length 截断只补一次：残缺标记留在已输出前缀里，
	// 追加正文补不完它，再救只是白花一次上游调用。
	singleShotRescues = 1
)

// continueAllowed 报告本次是否还允许发起续写：上限按形态分开——预告句 / 承诺句给
// 两次；结构未闭合（残缺标记补不完）与 length 截断仍只补一次。次数在真正发起前
// 累加，失败也算用过，避免重复打上游。
func (c *TextContinueReader) continueAllowed() bool {
	if c.hasTool || c.open == nil || c.text.Len() == 0 || c.noMoreContinue {
		return false
	}

	// 只因 stop 触发时（非 length 截断）还要求已输出成篇内容：短答是正常收尾。
	// 尾部探针授权的 stop（预告句 / 承诺句 / 工具轮）不受该门槛约束。
	if c.finishStop && !c.finishLength && !c.stopByTail &&
		utf8.RuneCountInString(c.text.String()) < c.stopMinChars {
		return false
	}

	return c.continueTries < c.continueLimit()
}

// continueLimit 本次续写的次数上限：预告句 / 承诺句两次；结构未闭合与 length 截断一次。
func (c *TextContinueReader) continueLimit() int {
	if c.stopByTail && (c.stopKind == TailPunctuated || c.stopKind == TailPromise) {
		return narrationMaxRescues
	}

	return singleShotRescues
}

// tryContinue 发起一次续写。成功则切换 reader；失败则补发暂存的终态帧。
func (c *TextContinueReader) tryContinue() bool {
	if !c.continueAllowed() {
		c.flushHeld()

		return false
	}

	kind := TailNone
	if c.stopByTail {
		kind = c.stopKind
	}

	c.continueTries++

	next, err := c.open(c.text.String(), kind)
	if err != nil || next == nil {
		c.flushHeld()

		return false
	}

	_ = c.src.Close()
	c.src = next
	// 复用同一条 64KB 行缓冲：老流的残留（被截断的尾巴）本就该丢弃，
	// Reset 与新建 reader 的语义一致，省掉续写路径上的一次 64KB 分配。
	// streamGen 必须同时 +1，Read 靠它识别流已切换。
	c.reader.Reset(next)
	c.streamGen++
	c.continued = true
	c.continuation = true
	c.segmentStart = c.text.Len()
	c.pending = nil
	c.settled = false
	// 只有尾部探针授权的 stop 才走「只采纳调用」；length 截断照旧续正文。
	c.continuationToolsOnly = c.toolsOnly && c.stopByTail

	if c.onContinue != nil {
		c.onContinue(tailSnippet(c.text.String(), continuationTailLogChars), kind)
	}

	c.finishLength = false
	c.finishStop = false
	c.stopByTail = false
	c.stopKind = TailNone

	// 工具轮保留首段终态作为「续写没带回调用」时的兜底，其余情况丢弃。
	if !c.continuationToolsOnly {
		c.held = nil
	}

	return true
}

// settleSegment 收尾本次续写段的暂存帧（issue #589）：本段只是「已完成」这类声明时
// 丢弃——这三个字不进客户端正文，也不再发起救援；真实正文按序放行。
func (c *TextContinueReader) settleSegment() {
	if c.settled {
		return
	}

	c.settled = true

	if len(c.pending) == 0 {
		return
	}

	if completionDeclaration(c.segmentText()) {
		c.noMoreContinue = true
		c.pending = nil

		return
	}

	for _, line := range c.pending {
		c.out.WriteString(line)
	}

	c.pending = nil
}

// holdDeclarationFrame 暂存续写段的正文帧：在能确定本段不是「已完成」声明之前先不下发，
// 一旦正文超出了这类声明的形态就立即放行（流式不因此变慢）。
func (c *TextContinueReader) holdDeclarationFrame(frame map[string]any) {
	if line := c.frameLine(frame); line != "" {
		c.pending = append(c.pending, line)
	}

	if !completionPrefixPossible(c.segmentText()) {
		c.settleSegment()
	}
}

// segmentText 取本次续写段新增的正文。
func (c *TextContinueReader) segmentText() string {
	text := c.text.String()
	if c.segmentStart >= len(text) {
		return ""
	}

	return text[c.segmentStart:]
}

// completionDeclMaxRunes 「已完成」类声明的最长字符数：超过它的正文肯定是新内容。
const completionDeclMaxRunes = 16

// completionPhrases 模型按旧习惯回复「已完成」时的终态措辞：这类内容不能拼进客户端
// 正文，也不该再为它发起一次救援（issue #589）。
var completionPhrases = []string{"已完成", "已经完成", "已改完", "已经改完", "全部完成", "任务已完成"}

// completionDeclaration 报告续写段正文是否只是声明「已完成」：按尾部后缀判断，不依赖
// 末句切分（续写段与上一条之间可能没有分隔符）。
func completionDeclaration(segment string) bool {
	trimmed := strings.TrimSpace(segment)
	if trimmed == "" || utf8.RuneCountInString(trimmed) > completionDeclMaxRunes {
		return false
	}

	for _, phrase := range completionPhrases {
		if strings.HasSuffix(trimmed, phrase) {
			return true
		}
	}

	return false
}

// completionPrefixPossible 报告续写段正文是否仍可能是一个「已完成」声明：只有这种前缀
// 形态才需要继续暂存，其余正文一出现就放行。
func completionPrefixPossible(segment string) bool {
	trimmed := strings.TrimSpace(segment)
	if trimmed == "" {
		return true
	}

	if utf8.RuneCountInString(trimmed) > completionDeclMaxRunes {
		return false
	}

	for _, phrase := range completionPhrases {
		if strings.HasPrefix(phrase, trimmed) || strings.HasSuffix(trimmed, phrase) {
			return true
		}
	}

	return false
}

// continuationTailLogChars 续写触发日志里尾部片段的字符上限。
const continuationTailLogChars = 80

// tailSnippet 取正文末尾至多 n 个字符（避免整段正文进日志）。
func tailSnippet(text string, n int) string {
	runes := []rune(text)
	if len(runes) <= n {
		return string(runes)
	}

	return string(runes[len(runes)-n:])
}

// flushHeld 把暂存的 length 终态帧补发出去（续写失败时保留真实终态）。
func (c *TextContinueReader) flushHeld() {
	if len(c.held) == 0 {
		return
	}

	c.out.Write(c.held)
	c.held = nil
}

// mergeUsageNumbers 把 src（首段）并进 dst（末段）：prompt 侧只留末段（src 不补），
// total_tokens 留给重算或保留末段原值，其余数值叶子相加，dst 缺失的键补上。
func mergeUsageNumbers(dst, src map[string]any) {
	for key, value := range src {
		applyMergedUsageLeaf(dst, key, value)
	}
}

// applyMergedUsageLeaf 把首段的一个叶子并进末段。prompt 侧与 total_tokens 跳过；
// 嵌套对象递归；两边都是 float64 则相加；末段没有的键补上。
func applyMergedUsageLeaf(dst map[string]any, key string, value any) {
	if key == "total_tokens" {
		return
	}

	if _, promptSide := promptUsageKeys[key]; promptSide {
		return
	}

	if typed, ok := value.(map[string]any); ok {
		if target, ok := dst[key].(map[string]any); ok {
			mergeUsageNumbers(target, typed)

			return
		}
	}
	// 数值叶子按数值相加：帧解码保留原始字面量后取值是 jsontext.Value，只认 float64
	// 会让多段 completion 被末段覆盖（少算）。
	if sum, ok := UsageSumLeaves(dst[key], value); ok {
		dst[key] = sum

		return
	}

	if _, exists := dst[key]; !exists {
		dst[key] = value
	}
}

// recomputeMergedUsageTotal 在末段带 prompt_tokens 时按「末段 prompt + 累计
// completion」重写 total_tokens。没有该字段则保留末段原 total。
func recomputeMergedUsageTotal(usageMap map[string]any) {
	promptTokens, ok := UsageNumber(usageMap, "prompt_tokens")
	if !ok {
		return
	}

	completion, _ := UsageNumber(usageMap, "completion_tokens")

	total := promptTokens + completion
	// 溢出/负值不可信：保留末段原 total，别把 +Inf 写进请求体。
	if math.IsInf(total, 0) || math.IsNaN(total) || total < 0 {
		return
	}

	usageMap["total_tokens"] = total
}

// BuildContinuationBody 在原始请求体后追加 assistant 部分正文与续写指令。
// 返回的新 body 仍交给 ChatStreamContext（经 prepareBody 脱敏/思考/缓存键管线）。
// continuationInstruction 截断续写时追加的合成 user 指令。历史裁剪按「user 消息
// 算一轮」计数，这条合成消息不算客户端的一轮（见 history.go 的 trimHistory），
// 否则 prompt.history_keep_turns=1 时会把它顶成唯一一轮、连被续写的 partial
// 正文一起裁掉。
const continuationInstruction = "Continue the previous response from exactly where it stopped. " +
	"Do not repeat any text already produced. Do not add commentary."

// completionExit 两条救援指令共用的完成出口（issue #589）：判据误触发时模型只需要零
// 输出；让它回一句「已完成」会把这三个字当正文拼进客户端输出，甚至被当成续写内容转发。
const completionExit = "如果这部分其实已经写完，就不要输出任何内容：不要回复「已完成」，" +
	"不要解释，也不要争辩。\n\n"

// narrationNudge 尾部停在标点型悬停（冒号/逗号/未闭合代码块）时的续写指令。
// 必须点明「你停在预告上」：否则模型会以为任务已完成而重复一遍结论。
const narrationNudge = "你上一条回复停在一句预告（例如以冒号结尾的「……：」）上就结束了，" +
	"剩余内容还没写。请紧接着把剩下的部分写完：需要查看文件、执行命令或调用工具就直接调用，" +
	"不要重复已经写过的内容，不要重新开头，不要道歉或解释。\n\n" + completionExit

// narrationPromiseNudge 尾部是「我将…/我会…」这类承诺句时的续写指令。
// 不能复用 narrationNudge：那条说「停在冒号上」，承诺句既没有冒号也不是预告排版，
// 模型会反过来争辩「上一条并未中断」，争论也会一并写进客户端正文。
const narrationPromiseNudge = "你上一条回复最后停在「我将…」「我会…」这类承诺句上就结束了，" +
	"承诺的动作没有执行。请直接执行它：需要查看文件、执行命令或调用工具就直接调用，" +
	"不要解释为什么停下，不要重复已经写过的内容，不要重新开头。\n\n" + completionExit

// continuationInstructionFor 按触发形态选续写指令：承诺句用专门的文案，其余沿用
// 通用「从断点接着写」。
func continuationInstructionFor(kind TailKind) string {
	switch kind {
	case TailPunctuated:
		return narrationNudge
	case TailPromise:
		return narrationPromiseNudge
	case TailNone, TailStructural:
	}

	return continuationInstruction
}

// BuildContinuationBody 在原始请求体后追加 assistant 部分正文与按形态选定的续写指令。
func BuildContinuationBody(src []byte, partial string, kind TailKind) []byte {
	obj, err := decodeObjectRawNumbers(src)
	if err != nil || obj == nil {
		return src
	}

	msgs, _ := obj["messages"].([]any)
	msgs = append(msgs,
		map[string]any{"role": "assistant", "content": partial},
		map[string]any{"role": "user", "content": continuationInstructionFor(kind)},
	)
	obj["messages"] = msgs

	out, err := jsonx.Marshal(obj)
	if err != nil {
		return src
	}

	return out
}

// DisableThinking 关闭请求体里的思考（thinking.type=disabled 并删 effort），
// 用于「预算被思考吃光」的同额度重发。非 deepseek / 无 thinking 字段时也返回
// 合法 body（无 thinking 时仅删 effort，不凭空加 thinking 以免触发上游校验）。
func DisableThinking(body []byte) []byte {
	obj, err := decodeObjectRawNumbers(body)
	if err != nil || obj == nil {
		return body
	}

	delete(obj, "reasoning_effort")
	delete(obj, "reasoningEffort")

	if _, has := obj["thinking"]; has {
		obj["thinking"] = map[string]any{"type": "disabled"}
	}

	out, err := jsonx.Marshal(obj)
	if err != nil {
		return body
	}

	return out
}
