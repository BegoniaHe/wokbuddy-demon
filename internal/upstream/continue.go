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

	finishLength  bool
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
	if c.finishLength && !c.continued && c.tryContinue() {
		return
	}

	c.out.WriteString(line)
}

// handleFrame 处理 JSON 帧：错误帧原样重编码，正常帧按是否需要改写决定透传方式。
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

	if c.finishLength && !c.continued {
		c.holdLengthFrame(line, frame)

		return
	}

	// 非续写段不需要改写 id/model/usage：整行原样透传，省掉每帧一次 marshal
	// （续写段才走 writeFrameJSON 的重编码）。
	if !c.continuation {
		c.out.WriteString(line)

		return
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
//
//nolint:gocognit // 正文/拒答/工具调用判定按优先级线性展开。
func frameHasSubstantive(frame map[string]any) bool {
	choices, _ := frame["choices"].([]any)

	for _, raw := range choices {
		choice, _ := raw.(map[string]any)
		if choice == nil {
			continue
		}

		for _, key := range []string{"delta", "message"} {
			node, _ := choice[key].(map[string]any)
			if node == nil {
				continue
			}

			if text, _ := node["content"].(string); text != "" {
				return true
			}

			if text, _ := node["refusal"].(string); text != "" {
				return true
			}

			if tools, _ := node["tool_calls"].([]any); len(tools) > 0 {
				return true
			}
		}
	}

	return false
}

// frameHasUsage 报告帧是否携带非空 usage 对象。
func frameHasUsage(frame map[string]any) bool {
	usage, ok := frame["usage"].(map[string]any)

	return ok && len(usage) > 0
}

// frameFinishReasonIsLength 报告帧是否带 finish_reason=length。
func frameFinishReasonIsLength(frame map[string]any) bool {
	choices, _ := frame["choices"].([]any)

	for _, raw := range choices {
		choice, _ := raw.(map[string]any)
		if choice == nil {
			continue
		}

		if fr, _ := choice["finish_reason"].(string); fr == "length" {
			return true
		}
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
func (c *TextContinueReader) inspectFrame(frame map[string]any) {
	if !c.continuation {
		// 首段 usage 在续写成功后仍是独立一次上游调用，不能随 length 帧一起丢弃；
		// 续写段带 usage 时把它合并进去（末帧口径，与 statsReader 一致）。
		if usage, ok := frame["usage"].(map[string]any); ok && len(usage) > 0 {
			c.firstUsage = usage
		}
	}

	walkChoiceDeltas(frame, func(fr string) {
		if fr == "length" {
			c.finishLength = true
		}
	}, c.inspectDelta)
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

// rewriteContinuationFrame 把续写帧的 id/model 对齐首段，并把首段 usage 合并进
// 续写段 usage：续写是第二次上游调用，只报本段会让流式记账漏记首段
// （issue #247，与非流式 mergeUsageTotals 同口径）。
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
	}
}

// handleEOF 流结束：有暂存 length 时尝试续写；否则收尾。
func (c *TextContinueReader) handleEOF() {
	if c.finishLength && !c.continued {
		if c.tryContinue() {
			return
		}

		if len(c.held) > 0 {
			c.out.Write(c.held)
			c.held = nil
		}
	}

	c.done = true
}

// tryContinue 发起一次续写。成功则切换 reader；失败则补发暂存的 length 帧。
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
	c.finishLength = false
	c.held = nil

	return true
}

// flushHeld 把暂存的 length 终态帧补发出去（续写失败时保留真实终态）。
func (c *TextContinueReader) flushHeld() {
	if len(c.held) == 0 {
		return
	}

	c.out.Write(c.held)
	c.held = nil
}

// mergeUsageNumbers 递归把 src 的数值字段累加进 dst，缺失的非数值键直接补齐。
// 只处理 usage 的数值树（prompt_tokens_details 等嵌套对象）。
func mergeUsageNumbers(dst, src map[string]any) {
	for key, value := range src {
		switch typed := value.(type) {
		case map[string]any:
			if target, ok := dst[key].(map[string]any); ok {
				mergeUsageNumbers(target, typed)

				continue
			}
		case float64:
			if target, ok := dst[key].(float64); ok {
				dst[key] = target + typed

				continue
			}
		}

		if _, exists := dst[key]; !exists {
			dst[key] = value
		}
	}
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
