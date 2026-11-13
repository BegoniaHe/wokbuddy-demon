// continue.go 截断续写：上游以 finish_reason=length 结束且已产生正文时，自动用
// 同账号同模型发起一次续写，把后续正文拼接到同一 SSE 流里。
//
// 约束（issue #247）：
//   - 只续写正文；工具调用（尤其是参数未闭合）不盲重试；
//   - 续写请求由调用方经 ChatStreamContext → prepareBody 发起（本文件只负责判定与拼接）；
//   - 次数上限 1 次；续写失败时把首段的 length 原样交给客户端；
//   - 续写帧复用首段的 id/model，客户端看到的是同一条流。
package upstream

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"unicode/utf8"
)

// ContinueOpener 发起一次续写请求；partial 是已输出的正文。
type ContinueOpener func(partial string) (io.ReadCloser, error)

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
	stopTail func(string) bool
	// stopByTail 本次 stop 是尾部探针授权的：跳过 stopMinChars 的字符门槛。
	stopByTail bool
	// toolsOnly 工具轮续写：只采纳续写段的 tool_calls，丢弃正文。
	toolsOnly bool
	// continuationToolsOnly 本次实际生效的「只采纳调用」（length 触发的仍续正文）。
	continuationToolsOnly bool
	// continuationHasTool 续写段是否已带回工具调用。
	continuationHasTool bool
	// onContinue 真正发起续写时的诊断回调（收到已输出正文的尾部片段）。
	onContinue func(tail string)
	// finishStop 本次终态是 stop（且已开启 stopTriggers）：与 finishLength 同处置。
	finishStop    bool
	continueTried bool
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

// ContinueOnStopTail 让「正文停在预告句」的 stop 触发一次续写（issue #495）。
// tail 判据作用于已输出正文；toolsOnly 为真表示本轮声明了工具——只采纳续写段里
// 的 tool_calls，丢弃续写正文（工具轮「说一句就停」的补救）。onTrigger 在真正
// 发起续写时收到已输出正文的尾部片段，供调用方落诊断日志（可能为 nil）。
func (c *TextContinueReader) ContinueOnStopTail(
	tail func(string) bool, toolsOnly bool, onTrigger func(tail string),
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
	if json.Unmarshal([]byte(payload), &frame) != nil || frame == nil {
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
	if c.continueTriggered() && !c.continued && c.tryContinue() {
		return
	}

	// 工具轮续写没带回任何调用时，首段终态必须补在 [DONE] 之前（#495）。
	if c.continued && c.continuationToolsOnly && !c.continuationHasTool {
		c.flushHeld()
	}

	c.out.WriteString(line)
}

// handleFrame 处理 JSON 帧：错误帧原样重编码，正常帧按是否需要改写决定透传方式。
//
//nolint:cyclop,gocyclo // 帧分派按优先级线性展开（错误帧 → 终态暂存 → 工具轮过滤 → 透传）。
func (c *TextContinueReader) handleFrame(line string, frame map[string]any) {
	c.captureMeta(frame)

	if _, hasErr := frame["error"]; hasErr {
		c.writeFrameJSON(frame)

		return
	}

	if _, _, isErr := frameBusinessError(frame); isErr {
		c.writeFrameJSON(frame)

		return
	}

	c.inspectFrame(frame)

	if c.continueTriggered() && !c.continued {
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

	c.writeFrameJSON(frame)
}

// holdLengthFrame 在续写前暂存 length 终态：续写成功则丢弃，失败则补发。
// usage 帧立即写出并剥掉 finish_reason——hold 会被后续帧覆盖或在续写成功时
// 丢弃（用量/积分漏记），直接写出 length 则会让客户端提前结束响应。
func (c *TextContinueReader) holdLengthFrame(line string, frame map[string]any) {
	if frameHasUsage(frame) {
		c.writeFrameJSON(stripFinishReason(frame))

		if frameFinishReasonIsLength(frame) {
			c.held = append(c.held, finishOnlyFrameLine(frame)...)
		}

		return
	}

	if frameHasSubstantive(frame) {
		// 终态帧同时带正文（少见）：正文先按普通内容写出，再暂存 finish-only 帧。
		c.writeFrameJSON(stripFinishReason(frame))
		c.held = append(c.held, finishOnlyFrameLine(frame)...)

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

// frameFinishReasonIsLength 报告帧是否带 finish_reason=length。
func frameFinishReasonIsLength(frame map[string]any) bool {
	return frameHasFinishReason(frame, "length")
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

// finishOnlyFrameLine 构造仅含 finish_reason=length 的 data 行。
func finishOnlyFrameLine(frame map[string]any) []byte {
	out, _ := deepCopyMap(frame)
	choices, _ := out["choices"].([]any)

	for _, raw := range choices {
		choice, _ := raw.(map[string]any)
		if choice == nil {
			continue
		}

		choice["delta"] = map[string]any{}
		delete(choice, "message")
		choice["finish_reason"] = "length"
	}

	raw, err := json.Marshal(out)
	if err != nil {
		return nil
	}

	return []byte("data: " + string(raw) + "\n\n")
}

// deepCopyMap 经 JSON 往返复制（帧结构小，续写只在截断时触发）。
func deepCopyMap(src map[string]any) (map[string]any, error) {
	raw, err := json.Marshal(src)
	if err != nil {
		return src, fmt.Errorf("deepCopyMap marshal: %w", err)
	}

	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
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

	if !sawStop || !c.stopTriggers || c.continued {
		return
	}

	// 给了尾部探针就按它判（预告句 / 工具轮，#495）；否则沿用长文口径——stop 一律
	// 可续写，短答由 tryContinue 的字符门槛挡掉（#494）。
	if c.stopTail == nil {
		c.finishStop = true

		return
	}

	if c.stopTail(c.text.String()) {
		c.finishStop = true
		c.stopByTail = true

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
		c.text.WriteString(text)
	}

	if refusal, ok := delta["refusal"].(string); ok {
		c.text.WriteString(refusal)
	}

	if tools, ok := delta["tool_calls"].([]any); ok && len(tools) > 0 {
		c.hasTool = true
	}
}

// writeFrameJSON 写回数据帧；续写段复用首段 id/model。
func (c *TextContinueReader) writeFrameJSON(frame map[string]any) {
	c.rewriteContinuationFrame(frame)

	raw, err := json.Marshal(frame)
	if err != nil {
		return
	}

	c.out.WriteString("data: " + string(raw) + "\n\n")
}

// rewriteContinuationFrame 把续写帧的 id/model 对齐首段，并把首段 usage 并进
// 续写帧：prompt/cache 留末段，completion/credit 累加，total 重算。续写是第二次
// 上游调用，只报本段会漏记首段完成量（issue #247 / #432，与非流式同口径）。
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

	if usage, ok := frame["usage"].(map[string]any); ok && c.firstUsage != nil {
		mergeUsageNumbers(usage, c.firstUsage)
		recomputeMergedUsageTotal(usage)
	}
}

// continueTriggered 报告本次终态是否已判定为可续写（length，或开启后的 stop）。
func (c *TextContinueReader) continueTriggered() bool {
	return c.finishLength || c.finishStop
}

// handleEOF 流结束：有暂存终态时尝试续写；否则收尾。
func (c *TextContinueReader) handleEOF() {
	if c.continueTriggered() && !c.continued {
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

// tryContinue 发起一次续写。成功则切换 reader；失败则补发暂存的 length 帧。
//
//nolint:cyclop,gocyclo // 续写前置条件线性校验（只做一次 → 无工具 → 正文非空 → 门槛）。
func (c *TextContinueReader) tryContinue() bool {
	// 只尝试一次：失败后 held 里的 length 终态已经补发，若在 EOF 处再试会把
	// 续写帧写到已经发出的 [DONE] 之后，并重复打上游。工具调用不盲重试：参数
	// 分片可能未闭合，续写会拼出半截参数。
	if c.continueTried {
		return false
	}

	c.continueTried = true

	if c.hasTool || c.open == nil || c.text.Len() == 0 {
		c.flushHeld()

		return false
	}

	// 只因 stop 触发时（非 length 截断）还要求已输出成篇内容：短答是正常收尾。
	// 尾部探针授权的 stop（预告句 / 工具轮）不受该门槛约束。
	if c.finishStop && !c.finishLength && !c.stopByTail &&
		utf8.RuneCountInString(c.text.String()) < c.stopMinChars {
		c.flushHeld()

		return false
	}

	next, err := c.open(c.text.String())
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
	// 只有尾部探针授权的 stop 才走「只采纳调用」；length 截断照旧续正文。
	c.continuationToolsOnly = c.toolsOnly && c.stopByTail

	if c.onContinue != nil {
		c.onContinue(tailSnippet(c.text.String(), continuationTailLogChars))
	}

	c.finishLength = false
	c.finishStop = false
	c.stopByTail = false

	// 工具轮保留首段终态作为「续写没带回调用」时的兜底，其余情况丢弃。
	if !c.continuationToolsOnly {
		c.held = nil
	}

	return true
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

	switch typed := value.(type) {
	case map[string]any:
		if target, ok := dst[key].(map[string]any); ok {
			mergeUsageNumbers(target, typed)

			return
		}
	case float64:
		if target, ok := dst[key].(float64); ok {
			dst[key] = target + typed

			return
		}
	}

	if _, exists := dst[key]; !exists {
		dst[key] = value
	}
}

// recomputeMergedUsageTotal 在末段带 prompt_tokens 时按「末段 prompt + 累计
// completion」重写 total_tokens。没有该字段则保留末段原 total。
func recomputeMergedUsageTotal(usageMap map[string]any) {
	promptTokens, ok := usageMap["prompt_tokens"].(float64)
	if !ok {
		return
	}

	completion, _ := usageMap["completion_tokens"].(float64)
	usageMap["total_tokens"] = promptTokens + completion
}

// BuildContinuationBody 在原始请求体后追加 assistant 部分正文与续写指令。
// 返回的新 body 仍交给 ChatStreamContext（经 prepareBody 脱敏/思考/缓存键管线）。
// continuationInstruction 截断续写时追加的合成 user 指令。历史裁剪按「user 消息
// 算一轮」计数，这条合成消息不算客户端的一轮（见 history.go 的 trimHistory），
// 否则 prompt.history_keep_turns=1 时会把它顶成唯一一轮、连被续写的 partial
// 正文一起裁掉。
const continuationInstruction = "Continue the previous response from exactly where it stopped. " +
	"Do not repeat any text already produced. Do not add commentary."

func BuildContinuationBody(src []byte, partial string) []byte {
	var obj map[string]any
	if json.Unmarshal(src, &obj) != nil || obj == nil {
		return src
	}

	msgs, _ := obj["messages"].([]any)
	msgs = append(msgs,
		map[string]any{"role": "assistant", "content": partial},
		map[string]any{"role": "user", "content": continuationInstruction},
	)
	obj["messages"] = msgs

	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}

	return out
}

// DisableThinking 关闭请求体里的思考（thinking.type=disabled 并删 effort），
// 用于「预算被思考吃光」的同额度重发。非 deepseek / 无 thinking 字段时也返回
// 合法 body（无 thinking 时仅删 effort，不凭空加 thinking 以免触发上游校验）。
func DisableThinking(body []byte) []byte {
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil || obj == nil {
		return body
	}

	delete(obj, "reasoning_effort")
	delete(obj, "reasoningEffort")

	if _, has := obj["thinking"]; has {
		obj["thinking"] = map[string]any{"type": "disabled"}
	}

	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}

	return out
}
