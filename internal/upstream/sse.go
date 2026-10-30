// sse.go 处理上游 SSE 流：聚合成单个 OpenAI 响应，或透传给客户端。
package upstream

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// errEmptyStream 上游返回 200 但没有有效 SSE 数据帧（空流/只有注释/[DONE]）。
//
// errTruncatedStream 上游未发 data: [DONE] 就以 EOF 收尾（连接被切 / 代理中断 /
// 上游进程崩溃）。流式路径仍向客户端补一个 [DONE] 兜底收尾，但请求结果必须是
// 失败——截断的 content 不是完整回答（issue #105）。
//
// 两者都是上游缺陷，不是「客户端已走」。用哨兵错误替代裸 fmt.Errorf：StreamHint
// 的调用方（handler 流式路径）需要区分「上游空流/截断」与「客户端写失败」——
// 前者是上游失败，应记失败观测并不再计账号成功；写失败是客户端断开，账号侧
// 照常记成功。Aggregate 与 StreamHint 共用同一哨兵（errors.Is 判定）。
// 读错误（io.EOF 之外的 Read 错误）原样上抛，调用方按上游失败处置
// （errClientWrite 只包装写目标失败，见 writeAll）。
var (
	errEmptyStream     = errors.New("upstream stream contained no valid data events")
	errTruncatedStream = errors.New("upstream stream ended without [DONE]")
	errClientWrite     = errors.New("client write failed")
	errErrorFrame      = errors.New("upstream stream returned an error frame")
)

// FrameError 上游以 `data: {"error":...}` 帧报错（HTTP 200 + error 帧）。
// 非流式聚合必须识别并上抛，否则会被当成空 content 的成功响应。
type FrameError struct {
	Payload string
}

func (e *FrameError) Error() string { return errErrorFrame.Error() + ": " + e.Payload }
func (*FrameError) Unwrap() error   { return errErrorFrame }

// ErrorFramePayload 返回 error 帧原始 JSON；非该类型返回空串。
func ErrorFramePayload(err error) string {
	if frame, ok := errors.AsType[*FrameError](err); ok {
		return frame.Payload
	}

	return ""
}

// IsEmptyStreamError 报告错误是否为「上游空流」（无有效 SSE 帧）——供 handler
// 在流式路径把空流记为失败观测（HTTP 头已发出只能 200，但日志/状态应收敛到
// upstream_parse 同语义），与客户端断连类错误区分。
func IsEmptyStreamError(err error) bool { return errors.Is(err, errEmptyStream) }

// EmptyStreamError 返回空流哨兵（供 handler 在首帧预览阶段复用同一末端映射）。
func EmptyStreamError() error { return errEmptyStream }

// IsTruncatedStreamError 报告错误是否为「上游流被截断」（EOF 收尾但从未收到
// data: [DONE]）。截断按上游失败处置：账号不记成功、不绑粘性（issue #105）。
func IsTruncatedStreamError(err error) bool { return errors.Is(err, errTruncatedStream) }

// IsClientWriteError 报告错误是否为「写客户端失败」（下游断连 / 写超时）。
// 与上游读错误区分：客户端已走不是账号的错，handler 仍按 Interrupted 记、
// 账号侧照常记成功（issue #105）。
func IsClientWriteError(err error) bool { return errors.Is(err, errClientWrite) }

// Aggregate 读取完整 SSE 流，聚合 delta.content 为单个 OpenAI 响应。
// 分片/半行由 readSSELine 处理（单行上限 4 MiB，issue #113）；遇到 "data: [DONE]" 结束。
// EOF 收尾但未见过 [DONE] → errTruncatedStream：截断的 content 不是完整回答，
// 不返回残缺成功响应，由 handler 映射为 502 upstream_parse（issue #105）。
// 聚合总量超 32 MiB → ErrSSEAggregateTooLong（issue #113），同样不返回残缺成功。
// tool_calls 以流式 delta 到达（按 index 合并：首片带 id/type/name，后续只带 arguments 片段）。
// sseReaderPool 复用 64KB 的 SSE 行读取缓冲：每个流式请求都会创建一层
// bufio.Reader，池化后避免每请求一次 64KB 分配。归还前 Reset(nil)，不持有
// 上游 body 引用。
var sseReaderPool = sync.Pool{
	New: func() any { return bufio.NewReaderSize(nil, 64*1024) },
}

func getSSEReader(r io.Reader) *bufio.Reader {
	reader, _ := sseReaderPool.Get().(*bufio.Reader)
	if reader == nil {
		reader = bufio.NewReaderSize(nil, 64*1024)
	}

	reader.Reset(r)

	return reader
}

func putSSEReader(reader *bufio.Reader) {
	if reader == nil {
		return
	}

	reader.Reset(nil)
	sseReaderPool.Put(reader)
}

func Aggregate(r io.Reader) (map[string]any, error) {
	streamAggregator2 := newStreamAggregator()

	reader := getSSEReader(r)
	defer putSSEReader(reader)

	for {
		line, err := ReadSSELine(reader)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}

		line = strings.TrimRight(line, "\r\n")

		done, herr := streamAggregator2.handleLine(line)
		if herr != nil {
			return nil, herr // 聚合总量超限：立即失败，不返回残缺成功（issue #113）
		}

		if done {
			break // 上游显式发过 data: [DONE]（正常收尾）
		}

		if errors.Is(err, io.EOF) {
			break
		}
	}

	return streamAggregator2.result()
}

// streamAggregator Aggregate 的聚合状态机：把逐帧解析拆到方法上，降低单函数复杂度。
type streamAggregator struct {
	id, model     string
	created       float64
	content       strings.Builder
	reasoning     strings.Builder
	refusal       strings.Builder
	role          string
	finishReason  string
	usage         map[string]any
	gotAnyContent bool
	validEvents   int
	sawDone       bool // 上游显式发过 data: [DONE]（正常收尾）
	sawFinish     bool // 收到过非空 finish_reason（EOF 无 [DONE] 时视为完整收尾）
	toolCalls     map[int]map[string]any
	// functionCall 旧式（OpenAI functions API）function_call 的累计形态：name
	// 覆盖、arguments 拼接；与 tool_calls 互斥。
	functionCall map[string]any
	// tools 解析 tool_call 归属 index（兼容上游省略 index 的形态）；聚合与逐帧
	// 透传路径共用同一套规则。
	tools *toolIndex
	// guard 聚合总量上限（issue #113）：content/reasoning/tool_call arguments
	// 三条写入路径都经它。超限即失败，不返回残缺成功响应。
	guard aggregateGuard
}

// newStreamAggregator 初始化聚合状态（role/finish_reason 有默认值）。
func newStreamAggregator() *streamAggregator {
	return &streamAggregator{
		role:         "assistant",
		finishReason: "stop",
		toolCalls:    map[int]map[string]any{},
		tools:        newToolIndex(),
	}
}

// sseDataPayload 取 SSE "data:" 行的载荷。SSE 规范允许 "data:" 后不带空格，
// 上游两种形态都实测出现过（见 handleStreamLine 的 "data:" 分支），各读取路径
// 统一经这里归一，避免有的路径认无空格形态、有的整段漏掉。
func sseDataPayload(line string) (string, bool) {
	payload, ok := strings.CutPrefix(line, "data:")
	if !ok {
		return "", false
	}

	return strings.TrimPrefix(payload, " "), true
}

// sseDone 判断 data 载荷是否为流终止标记。上游偶发尾随空白（"[DONE] "、"\r" 残留），
// 严格等值会让聚合路径继续读到 EOF 并判成截断流，故按去空白后比较。
func sseDone(payload string) bool {
	return strings.TrimSpace(payload) == "[DONE]"
}

// handleLine 处理一行：data: 帧解析聚合，[DONE] 返回 true（调用方停止读取）。
// 聚合总量超限时返回 ErrSSEAggregateTooLong（issue #113）。
func (a *streamAggregator) handleLine(line string) (bool, error) {
	payload, ok := sseDataPayload(line)
	if !ok {
		return false, nil
	}

	if sseDone(payload) {
		// 上游显式结束：停止读取，DONE 之后的任何数据一律忽略。
		a.sawDone = true
		return true, nil
	}

	var chunk map[string]any
	if json.Unmarshal([]byte(payload), &chunk) == nil && chunk != nil {
		// 有效事件计数：仅 JSON 对象数据帧计入（解析失败、顶层 null 沿用静默 continue）。
		// null 是合法 JSON 但解码成 nil map，没有可聚合字段，计入会掩盖空流。
		a.validEvents++
		if err := a.handleChunk(chunk); err != nil {
			return false, err
		}
	}

	return false, nil
}

// handleChunk 并入一个数据帧的顶层字段与 choices。聚合超限返回错误（issue #113）。
func (a *streamAggregator) handleChunk(chunk map[string]any) error {
	if errObj, ok := chunk["error"]; ok && errObj != nil {
		payload, err := json.Marshal(chunk)
		if err != nil {
			return fmt.Errorf("marshal error frame: %w", err)
		}

		return &FrameError{Payload: string(payload)}
	}
	// 顶层 {code,msg} 业务错误帧：与 error 帧同口径上抛，不能聚合成功（issue #244）。
	if _, _, isErr := frameBusinessError(chunk); isErr {
		payload, err := json.Marshal(chunk)
		if err != nil {
			return fmt.Errorf("marshal business error frame: %w", err)
		}

		return &FrameError{Payload: string(payload)}
	}

	a.mergeChunkMeta(chunk)

	ch, ok := chunk["choices"].([]any)
	if !ok {
		return nil
	}

	for _, ci := range ch {
		c, _ := ci.(map[string]any)
		if c == nil {
			continue
		}

		if err := a.handleChoice(c); err != nil {
			return err
		}
	}

	return nil
}

// mergeChunkMeta 并入数据帧的顶层标量字段（id/model/created 只取首个非空，usage 覆盖）。
func (a *streamAggregator) mergeChunkMeta(chunk map[string]any) {
	if v, ok := chunk["id"].(string); ok && a.id == "" {
		a.id = v
	}

	if v, ok := chunk["model"].(string); ok && a.model == "" {
		a.model = v
	}

	if v, ok := chunk["created"].(float64); ok && a.created == 0 {
		a.created = v
	}

	if u, ok := chunk["usage"].(map[string]any); ok {
		a.usage = u
	}
}

// handleChoice 并入单个 choice：finish_reason + delta（或整条 message）。
// 聚合超限返回错误（issue #113）。
func (a *streamAggregator) handleChoice(table3 map[string]any) error {
	if fr, ok := table3["finish_reason"].(string); ok && fr != "" {
		a.finishReason = fr
		a.sawFinish = true
	}

	if delta, ok := table3["delta"].(map[string]any); ok {
		if err := a.handleDelta(delta); err != nil {
			return err
		}
	}
	// 有的上游把完整消息放在 message 里（非 delta）：整条并入（role/content/
	// reasoning_content/tool_calls），与 delta 分支同构。delta 已取过正文
	// （gotAnyContent）则跳过（避免与 delta 路径重复拼接——PR #134 的 latch 语义）。
	if msg, ok := table3["message"].(map[string]any); ok && !a.gotAnyContent {
		return a.mergeMessageFields(msg)
	}

	return nil
}

// deltaReasoning 读取推理文本：优先 reasoning_content，兼容官方 CLI 的
// delta.reasoning 别名。两者同时存在时以 reasoning_content 为准。
func deltaReasoning(node map[string]any) (string, bool) {
	if rc, ok := node["reasoning_content"].(string); ok {
		return rc, true
	}

	rc, ok := node["reasoning"].(string)

	return rc, ok
}

// handleDelta 并入一个流式 delta（role/content/reasoning_content/tool_calls）。
// 聚合超限返回错误（issue #113）。
func (a *streamAggregator) handleDelta(delta map[string]any) error {
	return a.mergeFields(delta)
}

// mergeMessageFields 把非 delta 的完整 message 内容并入聚合（message 是整条下发，
// 非流式拼接，content 只取一次）。role/reasoning_content/tool_calls 与 delta 分支
// 同构透出；content 同样置 gotAnyContent，与 delta 路径的 latch 语义一致
// （一帧整条 message 之后，后续 delta 帧不重复追加）。
func (a *streamAggregator) mergeMessageFields(msg map[string]any) error {
	return a.mergeFields(msg)
}

// mergeFields 是 delta 与 message 两路共用的字段归并逻辑：role/content/
// reasoning_content/refusal/tool_calls/function_call。聚合超限返回错误。
func (a *streamAggregator) mergeFields(fields map[string]any) error {
	if r2, ok := fields["role"].(string); ok && r2 != "" {
		a.role = r2
	}

	if txt, ok := fields["content"].(string); ok {
		if err := a.appendContent(txt); err != nil {
			return err
		}
	}

	if err := a.mergeReasoning(fields); err != nil {
		return err
	}

	if refusal, ok := fields["refusal"].(string); ok {
		a.refusal.WriteString(refusal)
	}

	return a.mergeCalls(fields)
}

// mergeCalls 并入 tool_calls 与旧式 function_call（聚合超限返回错误）。
func (a *streamAggregator) mergeCalls(fields map[string]any) error {
	if tcs, ok := fields["tool_calls"].([]any); ok {
		if err := a.mergeToolCallsChunk(tcs); err != nil {
			return err
		}
	}

	if functionCall, ok := fields["function_call"].(map[string]any); ok {
		if err := a.mergeFunctionCallChunk(functionCall); err != nil {
			return err
		}
	}

	return nil
}

// mergeReasoning 并入 reasoning_content / reasoning 别名（计入聚合总量上限）。
func (a *streamAggregator) mergeReasoning(fields map[string]any) error {
	rc, ok := deltaReasoning(fields)
	if !ok {
		return nil
	}

	if err := a.guard.add(len(rc)); err != nil {
		return err
	}

	a.reasoning.WriteString(rc)

	return nil
}

// appendContent 是「已取到正文」（gotAnyContent latch）的唯一写入点：delta 与
// message 两路 content 都必须经此并入，规约只有一份（issue #142）——
//
//	S1 空串不算「已取到正文」、不占 latch 名额：OpenAI 风格 role-only 首帧
//	   （delta.content=""）和整条空 message 帧是常态帧，若空串置位 latch，
//	   后续真正文会被 message 回退分支的 !gotAnyContent 守卫静默拒绝；
//	S2 空串本身也无可追加字节，跳过 WriteString 与追加语义自洽。
//
// 聚合总量超限返回错误（issue #113）。
func (a *streamAggregator) appendContent(txt string) error {
	if txt == "" {
		return nil
	}

	if err := a.guard.add(len(txt)); err != nil {
		return err
	}

	a.content.WriteString(txt)
	a.gotAnyContent = true

	return nil
}

// toolIndex 解析 tool_call 的归属 index，兼容上游省略 index 的形态。
//
// index 缺失兼容：OpenAI 规范要求 delta 帧的 tool_call 带 index（标记分片归属），
// 但部分上游省略它。此前缺 index 一律归 0——多调用场景下不同 call 被合并进同一
// index 槽，arguments 串联、name 互相覆盖（数据污染）。归位规则：
//   - 带 index → 按 index 累积（合规形态，零改动）；
//   - 缺 index 带 id 且 id 已见过 → 延续该 id 所在 index；
//   - 缺 index 带 id 且 id 是新的 → 开新序号（多调用不合并）；
//   - 缺 index 无 id → 追加到最近收到碎片的 index（单个调用的延续分片无 id
//     是标准形态），无既往则开新号。
//
// 聚合路径与逐帧透传路径共用本类型，避免两条路径对「缺 index」给出不同结论。
type toolIndex struct {
	seq      int
	order    []int
	byID     map[string]int
	occupied map[int]bool
}

func newToolIndex() *toolIndex {
	return &toolIndex{byID: map[string]int{}, occupied: map[int]bool{}}
}

// next 分配下一个不冲突的缺 index 序号：从 seq 起递增跳过既有 index
// （合规流的 index 是 0..N-1，缺 index 的补位不能覆盖它们）。
func (t *toolIndex) next() int {
	for {
		idx := t.seq

		t.seq++
		if !t.occupied[idx] {
			return idx
		}
	}
}

// resolve 返回单个 tool_call 的归属 index，并把 index/id 记入跨帧状态。
func (t *toolIndex) resolve(call map[string]any) int {
	if v, ok := call["index"].(float64); ok {
		idx := int(v)
		t.occupy(idx)
		t.remember(call, idx)

		return idx
	}

	if cid, _ := call["id"].(string); cid != "" {
		if idx, seen := t.byID[cid]; seen {
			return idx
		}

		idx := t.next()
		t.occupy(idx)
		t.byID[cid] = idx

		return idx
	}

	if len(t.order) > 0 {
		return t.order[len(t.order)-1] // 无 id 碎片：延续最近调用
	}

	idx := t.next()
	t.occupy(idx)

	return idx
}

// occupy 记录 index 已使用；首次出现时同步 order（结果按出现顺序输出）。
func (t *toolIndex) occupy(idx int) {
	if t.occupied[idx] {
		return
	}

	t.occupied[idx] = true
	t.order = append(t.order, idx)
}

// remember 记录 id → index，供后续缺 index 分片归位。
func (t *toolIndex) remember(call map[string]any, idx int) {
	if cid, _ := call["id"].(string); cid != "" {
		t.byID[cid] = idx
	}
}

// mergeToolCallsChunk 把一段 tool_calls 数组按 index 合并进累计表。
// delta（流式分片，按 index 累积）与 message（非 delta 整条）共用同一合并逻辑，
// 保证「上游给的身份/函数名不丢、arguments 拼接语义一致」。
// arguments 累计量计入聚合总量上限，超限返回错误（issue #113）。
func (a *streamAggregator) mergeToolCallsChunk(tcs []any) error {
	for _, tc := range tcs {
		call, ok := tc.(map[string]any)
		if !ok {
			continue
		}

		idx := a.tools.resolve(call)

		merged, seen := a.toolCalls[idx]
		if !seen {
			merged = map[string]any{"index": idx}
			a.toolCalls[idx] = merged
		}

		mergeToolCallDelta(merged, call)

		if fn, _ := call["function"].(map[string]any); fn != nil {
			if args, _ := fn["arguments"].(string); args != "" {
				if err := a.guard.add(len(args)); err != nil {
					return err
				}
			}
		}
	}

	return nil
}

// mergeFunctionCallChunk 合并旧式 function_call 分片：name 覆盖、arguments 拼接。
// 空占位（name/arguments 全空）是噪声，与 normalizeDelta 的 keepFunctionCall 同口径；
// arguments 累计量计入聚合总量上限。
func (a *streamAggregator) mergeFunctionCallChunk(chunk map[string]any) error {
	if !keepFunctionCall(chunk) {
		return nil
	}

	if a.functionCall == nil {
		a.functionCall = map[string]any{}
	}

	setStringIfNonEmpty(a.functionCall, chunk, "name")

	if args, _ := chunk["arguments"].(string); args != "" {
		if err := a.guard.add(len(args)); err != nil {
			return err
		}
	}

	appendStringIfNonEmpty(a.functionCall, chunk, "arguments")

	return nil
}

// result 由聚合状态构造最终的非流式响应；无有效事件返回 errEmptyStream，
// EOF 截断（未见过 [DONE]）返回 errTruncatedStream——两者都不是成功响应。
func (a *streamAggregator) result() (map[string]any, error) {
	if a.validEvents == 0 {
		// 上游返回 200 但没有任何有效数据事件（空流/只有 [DONE]/只有注释行）：
		// 不再合成空 content 的假成功响应，直接报错，由 handler 映射为 502 upstream_parse。
		return nil, errEmptyStream
	}

	if !a.sawDone && !a.sawFinish {
		// EOF 收尾但上游从未发 data: [DONE]（issue #105）：连接被切 / 代理中断 /
		// 上游崩溃。累积的 content 只是截断片段，不能当完整回答返回——非流式没有
		// 「补帧让客户端收尾」的余地，直接报上游解析失败，由 handler 映射 502。
		return nil, errTruncatedStream
	}

	if a.id == "" {
		a.id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}

	if a.created == 0 {
		a.created = float64(time.Now().Unix())
	}

	message := map[string]any{
		"role":    a.role,
		"content": a.content.String(),
	}
	if a.reasoning.Len() > 0 {
		message["reasoning_content"] = a.reasoning.String()
	}

	if a.refusal.Len() > 0 {
		message["refusal"] = a.refusal.String()
	}

	a.attachCalls(message)

	resp := map[string]any{
		"id":      a.id,
		"object":  "chat.completion",
		"created": int64(a.created),
		"model":   a.model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       message,
				"finish_reason": a.finishReason,
			},
		},
	}
	if a.usage != nil {
		// OpenAI 非流式 usage 必含 total_tokens。上游若只发 prompt_tokens +
		// completion_tokens（部分上游末帧缺 total），网关合成补齐——否则严格按
		// schema 校验的客户端收不到 total_tokens。已有 total 或二者缺一不补
		// （不臆造：单边有值无法合成可信的 total）。
		resp["usage"] = normalizeUsageCacheAliases(ensureUsageTotal(a.usage))
	}

	return resp, nil
}

// attachCalls 把累计的 tool_calls 与旧式 function_call 写入 message。
func (a *streamAggregator) attachCalls(message map[string]any) {
	a.attachToolCalls(message)
	a.attachFunctionCall(message)
}

// attachToolCalls 把累计的 tool_calls 按序写入 message（截断流剔除残缺参数）。
func (a *streamAggregator) attachToolCalls(message map[string]any) {
	if len(a.tools.order) == 0 {
		return
	}

	slices.Sort(a.tools.order)

	calls := make([]map[string]any, 0, len(a.tools.order))
	for _, idx := range a.tools.order {
		calls = append(calls, a.toolCalls[idx])
	}
	// P1b：流被截断时 tool_call 的 arguments 是残缺 JSON（解析失败），不把脏参数
	// 交给客户端——残留分片会被客户端解析成非法 JSON 卡死会话。截断的两个来源：
	//   - finish_reason=="length"（模型因 max_tokens 提前中止）；
	//   - 上游连接中断（EOF 收尾但未发 data: [DONE]，sawDone=false）。
	// 完整参数原样保留（正例零改动）；空参数（无参工具）不是截断，同样保留。
	// 此前只认 finish_reason=="length"，EOF 截断的 tool_calls 残缺参数被原样下发。
	if a.finishReason == "length" || !a.sawDone {
		calls = dropTruncatedToolCalls(calls)
	}

	if len(calls) > 0 {
		message["tool_calls"] = calls
	}
}

// attachFunctionCall 把累计的旧式 function_call 写入 message。截断流下参数残缺
// （与 tool_calls 同口径）时不写回，避免客户端解析非法 JSON；空占位不写回。
func (a *streamAggregator) attachFunctionCall(message map[string]any) {
	if len(a.functionCall) == 0 {
		return
	}

	truncatedStream := a.finishReason == "length" || !a.sawDone
	if args, _ := a.functionCall["arguments"].(string); truncatedStream && isTruncatedArguments(args) {
		return
	}

	message["function_call"] = a.functionCall
}

// ensureUsageTotal 在 usage 缺 total_tokens 但 prompt_tokens/completion_tokens 都在时
// 补齐 total = prompt + completion（通过新 map 合并，不修改原上游 map）。
// 任一缺失或已有 total 时原样返回。
func ensureUsageTotal(table map[string]any) map[string]any {
	if _, ok := table["total_tokens"]; ok {
		return table
	}

	num, pok := num64(table["prompt_tokens"])

	num2, cok := num64(table["completion_tokens"])
	if !pok || !cok {
		return table
	}

	out := make(map[string]any, len(table)+1)
	maps.Copy(out, table)
	out["total_tokens"] = num + num2

	return out
}

// num64 把 JSON number（float64/int64 均可）归一为 float64；非数字返回 ok=false。
//
// 注意与 payload.go 的类型 switch 语义不同：那边是翻译请求别名字段（非数字拒绝
// 整个请求），这边是聚合响应求和（非数字只跳过合成）。防后人合并两处。
func num64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	default:
		return 0, false
	}
}

// mergeToolCallDelta 把流式 tool_call 片段合并到累计对象：
// id/type/function.name 直覆盖（后续分片通常缺省），function.arguments 拼接。
func mergeToolCallDelta(merged, delta map[string]any) {
	setStringIfNonEmpty(merged, delta, "id")
	setStringIfNonEmpty(merged, delta, "type")

	table2, _ := delta["function"].(map[string]any)
	if table2 == nil {
		return
	}

	table, _ := merged["function"].(map[string]any)
	if table == nil {
		table = map[string]any{}
		merged["function"] = table
	}

	setStringIfNonEmpty(table, table2, "name")
	appendStringIfNonEmpty(table, table2, "arguments")
}

// setStringIfNonEmpty 把 src[key] 的非空字符串覆盖写入 dst[key]（缺失/空串不动）。
func setStringIfNonEmpty(dst, src map[string]any, key string) {
	if v, ok := src[key].(string); ok && v != "" {
		dst[key] = v
	}
}

// appendStringIfNonEmpty 把 src[key] 的非空字符串拼接到 dst[key] 已有字符串之后。
func appendStringIfNonEmpty(dst, src map[string]any, key string) {
	str, ok := src[key].(string)
	if !ok || str == "" {
		return
	}

	if prev, _ := dst[key].(string); prev != "" {
		dst[key] = prev + str
	} else {
		dst[key] = str
	}
}

// stripToolCallNames 收敛流式 tool_calls 的 name 语义为「每个 index 只出现一次」：
// 首片保留 function.name，同一 index 后续分片里的 name 键一律删除（无论上游是
// 空串还是重复非空串）。这是 OpenAI 官方流的真实形态——首帧带 name，后续帧只带
// arguments 片段、不再出现 name 键——因此是累加型与覆盖型客户端的共同祖先行为。
//
// 两类消费模型在该形态下同时正确：
//   - 累加型（官方 WorkBuddy/CodeBuddy `name += tc_function?.name || ""`）：
//     后续分片 name 键缺失 → 追加空串，累积 name 保持唯一，不再拼成 Bash×帧数（issue #82）。
//   - 覆盖型（hawklithm#2 / Grok Build `name ?? state.name` 或 `if (name) state.name = name`）：
//     后续分片 name 键缺失 → 保留已建好的首帧 name，不被空串意外清空。
//     键缺失是比空串更安全的形态：`??` 与 truthy 守卫对缺失键必然保留旧值，
//     而对空串，`??` 会误判为重设并清空工具名。
//
// seen 记录每个 index 是否已下发过非空 name；删除是幂等的。
// 只动 function.name 键，id/type/arguments 原样透传。
//
// 「已下发」只看非空 name：部分上游首片只带 id/arguments，name 随后续分片到达。
// 若空 name 的首片就占用该配额，真正的 name 会被当成重复而删掉，客户端最终拿到
// 没有 name 的调用（Anthropic 面会整块丢弃 tool_use）。空 name 键本身是噪声，
// 直接删掉但不占配额。
func stripToolCallNames(obj map[string]any, tools *toolIndex, seen map[int]bool) {
	forEachToolCallDelta(obj, tools, func(idx int, table map[string]any) {
		// OpenAI 流要求每片 tool_call 带 index；上游省略时按解析结果补齐，
		// 否则 SDK 的累加器拿到 None index 会丢调用。写 float64 与 JSON 解码
		// 形态一致：trackMaxToolIdx 等下游按 float64 读取。
		table["index"] = float64(idx)

		fn, _ := table["function"].(map[string]any)
		if seen[idx] {
			// 已发过首片：删除本分片的 name 键（存在即删，幂等）。
			if fn != nil {
				delete(fn, "name")
			}

			return
		}

		if name, _ := fn["name"].(string); name == "" {
			// 空 name（键缺失/空串）：本片不占用「已下发」配额，name 可能随后片到达。
			if fn != nil {
				delete(fn, "name")
			}

			return
		}

		seen[idx] = true
	})
}

// stripToolCallIDs 去除同一 tool_call index 后续分片里重复的 id 键。
// 官方流首片带 id，后续分片可能重复携带同一 id；累加型客户端会把 id 当字符串
// 拼接。保留首个 id，后续同 index 的 id 键删除。
func stripToolCallIDs(obj map[string]any, tools *toolIndex, seen map[int]string) {
	forEachToolCallDelta(obj, tools, func(idx int, table map[string]any) {
		cid, _ := table["id"].(string)

		if first, ok := seen[idx]; ok {
			if cid != "" && cid == first {
				delete(table, "id")
			}

			return
		}

		if cid != "" {
			seen[idx] = cid
		}
	})
}

// forEachToolCallDelta 遍历本帧 choices[].delta.tool_calls[]，把解出的 index 与
// tool_call 对象交给 visit。index 由 toolIndex 统一解析（兼容上游省略 index 的
// 形态），因此两个去重函数共用同一套归属规则。
func forEachToolCallDelta(obj map[string]any, tools *toolIndex, visit func(idx int, table map[string]any)) {
	choices, _ := obj["choices"].([]any)
	for _, ci := range choices {
		c, _ := ci.(map[string]any)
		if c == nil {
			continue
		}

		delta, _ := c["delta"].(map[string]any)
		if delta == nil {
			continue
		}

		tcs, _ := delta["tool_calls"].([]any)
		for _, tci := range tcs {
			table, _ := tci.(map[string]any)
			if table == nil {
				continue
			}

			visit(tools.resolve(table), table)
		}
	}
}

// trackFinishReason 记录本流是否已收到非空 finish_reason。
func (s *streamWriter) trackFinishReason(obj map[string]any) {
	choices, _ := obj["choices"].([]any)
	for _, ci := range choices {
		c, _ := ci.(map[string]any)
		if c == nil {
			continue
		}

		if fr, ok := c["finish_reason"].(string); ok && fr != "" {
			s.sawFinish = true
		}
	}
}

// normalizeFrame 以 OpenAI 流式规范白名单重建帧：仅保留标准字段，
// 剔除上游噪声（finish_reason:"" → null、空 content/refusal、空 tool_calls 列表、
// 空占位 function_call、顶层未知字段），空 delta 键一律省略，
// usage 缺失 → null，保证任意标准客户端按规范解析。
func normalizeFrame(obj map[string]any) map[string]any {
	out := map[string]any{}

	for _, k := range []string{"id", "object", "created", "model", "system_fingerprint", "service_tier"} {
		if v, ok := obj[k]; ok && v != nil {
			out[k] = v
		}
	}

	if _, ok := out["object"]; !ok {
		out["object"] = "chat.completion.chunk"
	}

	if _, ok := out["id"]; !ok {
		out["id"] = "chatcmpl-wb2api"
	}

	if chs, ok := obj["choices"].([]any); ok {
		nchs := make([]any, 0, len(chs))
		for _, ci := range chs {
			if c, ok := ci.(map[string]any); ok {
				nchs = append(nchs, normalizeChoice(c))
			}
		}

		out["choices"] = nchs
	}

	out["usage"] = normalizeFrameUsage(obj["usage"])

	return out
}

// normalizeChoice 以白名单重建单个 choice（index + 规范化 delta + finish_reason）。
func normalizeChoice(table2 map[string]any) map[string]any {
	table := map[string]any{}
	if idx, ok := table2["index"]; ok {
		table["index"] = idx
	}

	table["delta"] = normalizeDelta(table2["delta"])
	if fr, ok := table2["finish_reason"].(string); ok && fr != "" {
		table["finish_reason"] = fr
	} else {
		table["finish_reason"] = nil
	}

	return table
}

// normalizeDelta 以白名单重建 delta：仅保留非空的 role/content/reasoning_content/
// refusal，非空 tool_calls，以及非空占位的 function_call。
func normalizeDelta(raw any) map[string]any {
	table, ok := raw.(map[string]any)
	if !ok {
		return map[string]any{}
	}

	delta := map[string]any{}

	for _, k := range []string{"role", "content", "reasoning_content", "refusal"} {
		if v, ok := table[k].(string); ok && v != "" {
			delta[k] = v
		}
	}

	applyReasoningAlias(delta, table)

	if tcs, ok := table["tool_calls"].([]any); ok && len(tcs) > 0 {
		delta["tool_calls"] = tcs
	}

	applyFunctionCallDelta(delta, table)

	return delta
}

// applyReasoningAlias 把 reasoning 别名归一为 reasoning_content。
func applyReasoningAlias(delta, table map[string]any) {
	if _, ok := delta["reasoning_content"]; ok {
		return
	}

	if v, ok := table["reasoning"].(string); ok && v != "" {
		delta["reasoning_content"] = v
	}
}

// applyFunctionCallDelta 保留非空占位 function_call；空占位视为噪声剔除。
func applyFunctionCallDelta(delta, table map[string]any) {
	fc, ok := table["function_call"]
	if !ok || fc == nil || !keepFunctionCall(fc) {
		return
	}

	delta["function_call"] = fc
}

// keepFunctionCall 报告 function_call 是否非空占位：空占位（name/arguments 全空）
// 视为噪声剔除；非 map 形态保守保留。
func keepFunctionCall(fc any) bool {
	fcm, ok := fc.(map[string]any)
	if !ok {
		return true
	}

	n, _ := fcm["name"].(string)
	a, _ := fcm["arguments"].(string)

	return n != "" || a != ""
}

// normalizeFrameUsage 规范化 usage：map 形态走别名归一，缺失/其他形态原样（nil）。
func normalizeFrameUsage(rawUsage any) any {
	if u, ok := rawUsage.(map[string]any); ok {
		return normalizeUsageCacheAliases(u)
	}

	return rawUsage
}

// StreamOption StreamHint 的可选行为开关。
// WithErrorFrameObserver 只做旁路观测、不改透传字节；WithMarkupRepair 会改写出站
// 帧（把正文里的原生工具调用标记还原成 delta.tool_calls，见 dsml.go）。
type StreamOption func(*streamOptions)

type streamOptions struct {
	onErrorFrame func(payload string)
	markup       *MarkupRepair
	keepalive    time.Duration
}

// WithMarkupRepair 注册「模型原生工具调用标记」修复器（见 dsml.go）。
//
// 上游在没有 tools 的请求里会把工具调用吐成标记文本，Codex 这类只执行结构化
// function_call 的客户端于是把它当普通正文落进会话历史，回合直接结束、工具一次
// 都没跑。挂上修复器后，这类标记在透传前被还原成 delta.tool_calls。
//
// 传 nil、或传入未启用的修复器（客户端没声明过 tools）→ 与 Stream 行为逐字节一致。
func WithMarkupRepair(r *MarkupRepair) StreamOption {
	return func(o *streamOptions) { o.markup = r }
}

// WithKeepaliveInterval 在上游静默超过 d 时向客户端写一帧 SSE 注释心跳
// （": keepalive\n\n"），防止反代/Cloudflare 把长时间无帧的合法流当空闲连接切断
// （issue #54）。d<=0 关闭（与未设置时逐字节一致）。
func WithKeepaliveInterval(d time.Duration) StreamOption {
	return func(o *streamOptions) {
		if d > 0 {
			o.keepalive = d
		}
	}
}

// WithErrorFrameObserver 注册"上游以 error 帧报错"的观察者：透传该帧之前先用
// payload（原始 data 内容）回调一次。给调用方一个**账号处置**的挂载点——
// 上游「200 + error 帧」是真实形态（6004 限流、内容拦截、审核），此前网关在读
// 第一帧之前就把账号记成功，限流号被当成健康号；有观察者后可在流尾按帧分类处置。
func WithErrorFrameObserver(fn func(payload string)) StreamOption {
	return func(o *streamOptions) { o.onErrorFrame = fn }
}

// StreamHint 同 Stream，但上游 error 帧透出前把 hintFn(payload) 的返回值写入
// error.gateway_hint。hintFn 为 nil 或返回空串 → 原样透传（零改写）。
// 空流兜底 error 帧（"empty upstream stream"）不带 hint（网关本地故障形态
// 未覆盖，不编造）。
// opts：旁路观测开关（见 WithErrorFrameObserver），不改变任何透传字节。
func StreamHint(w http.ResponseWriter, r io.Reader, hintFn func(string) string, opts ...StreamOption) error {
	return newStreamWriter(w, hintFn, opts...).stream(r)
}

// clientWriteTimeout 单次向客户端写 SSE 帧（含 flush）的上限（issue #53）。
// 服务端不设全局 WriteTimeout（会误杀合法长流），慢客户端/半死 TCP 下
// Flush 可阻塞到内核重传超时，期间 goroutine 与账号在途租约都被占住。
// 健康连接单帧毫秒级完成，30s 只作「客户端完全不消费」的 stall 上界。
const clientWriteTimeout = 30 * time.Second

// streamWriter SSE 透传写入器的跨帧状态（StreamHint 的拆解，降低单函数复杂度）。
//
//nolint:gocritic // 类型与实现方法紧邻，拆分文件无收益。
type streamWriter struct {
	w      http.ResponseWriter
	fl     http.Flusher
	hintFn func(string) string
	opts   streamOptions
	// keepalive 上游静默超过该时长时写一帧 SSE 注释心跳；0 = 关闭。
	keepalive time.Duration

	// writeTimeout 单次写帧/Flush 的截止时长；<=0 不设截止（测试可注入短值）。
	writeTimeout time.Duration

	// sawDone 上游是否显式发过 data: [DONE]（issue #105）。两条读取路径
	// （普通 / keepalive）共用；收尾时据此区分「正常结束」与「EOF 截断」。
	sawDone bool

	// dataLines 当前 SSE 事件累积的 data: 行；按规范用 \n 拼接，遇空行/事件边界
	// 才写出一帧。单行 data 也走这里，保证多行 data 不再被拆成多个坏帧。
	dataLines []string
	// dataBytes 是 dataLines 的载荷总字节数：多行事件必须有总量上限，否则恶意/异常
	// 上游持续发 data 行且不发空行时会无限累积（单行上限只约束一行）。
	dataBytes int

	// toolCallSeen 跨帧记录 delta.tool_calls 里已发过首片的 index，
	// 供逐 chunk 透传时收敛 name 为「每 index 一次」（对齐 OpenAI 官方流）。
	toolCallSeen map[int]bool
	// toolCallIDs 跨帧记录每个 index 已发出的首个 tool_call id，供后续重复 id 去重。
	toolCallIDs map[int]string
	// sawFinish 本流是否已收到非空 finish_reason（用于 EOF 无 [DONE] 的优雅收尾）。
	sawFinish bool

	// tools 与聚合路径共用同一套 tool_call index 归位规则：缺 index 时按
	// id/最近调用归属，避免不同调用被当成同一 index 而误删 name。
	tools *toolIndex

	// firstID 透传流的消息级 id 基准：缓存首个非空上游 id，后续帧缺失/空串时复用
	// （issue #35：同一条 SSE 消息所有帧共用一个真实 id，后台按 id 归并；此前中间帧
	// 一律补 chatcmpl-wb2api 哨兵，造成同流 id 分裂）。全流无真实 id → 才出现哨兵。
	firstID string

	// 标记修复（见 dsml.go）：把上游正文里的原生工具调用标记还原成 delta.tool_calls。
	// 客户端没声明过 tools 时 repair.Enabled() 为 false → 整条流零改写零分配。
	//   - synthIdx：本流已还原的调用数（合成的 index 从上游最大 index 之后接续）；
	//   - maxToolIdx：上游已用过的最大 tool_call index，避免合成的 index 与真实调用撞车；
	//   - toolFinishSent：是否已发出过 finish_reason: tool_calls，供流尾兜底。
	repair         *MarkupRepair
	synthIdx       int
	maxToolIdx     int
	toolFinishSent bool
}

// newStreamWriter 初始化 SSE 透传写入器（设置 SSE headers + 跨帧状态）。
func newStreamWriter(w http.ResponseWriter, hintFn func(string) string, opts ...StreamOption) *streamWriter {
	var streamOptions2 streamOptions

	for _, f := range opts {
		if f != nil {
			f(&streamOptions2)
		}
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")

	fl, _ := w.(http.Flusher)

	return &streamWriter{
		w:            w,
		fl:           fl,
		hintFn:       hintFn,
		opts:         streamOptions2,
		writeTimeout: clientWriteTimeout,
		keepalive:    streamOptions2.keepalive,
		toolCallSeen: map[int]bool{},
		toolCallIDs:  map[int]string{},
		tools:        newToolIndex(),
		repair:       streamOptions2.markup,
		maxToolIdx:   -1,
	}
}

// writeAll 写出原始字节并 flush，同时在支持 ResponseController 的连接上设置
// 单次写截止（issue #53）。不支持 SetWriteDeadline 的 ResponseWriter（测试用
// ResponseRecorder 或中间件包装）忽略错误：拿不到截止支持不能反过来拒绝写流。
// 每次写前重设 deadline；流结束后由 stream 清空，避免过期 deadline 污染
// keep-alive 连接上的后续请求。
// 写失败统一包 errClientWrite（issue #105）：调用方据此把「客户端已走」与
// 「上游读失败」分开处置。
func (s *streamWriter) writeAll(payload string) error {
	return s.writeSegments("", payload, "")
}

// writeSegments 分段写出 prefix/payload/suffix 后 flush：SSE 帧头尾是常量，
// 分段写避免每帧为 "data: "+payload+"\n\n" 复制一次完整载荷。
func (s *streamWriter) writeSegments(prefix, payload, suffix string) error {
	s.setWriteDeadline(time.Now().Add(s.writeTimeout))

	for _, part := range [3]string{prefix, payload, suffix} {
		if _, werr := io.WriteString(s.w, part); werr != nil {
			return fmt.Errorf("%w: %w", errClientWrite, werr)
		}
	}

	if s.fl != nil {
		s.fl.Flush()
	}

	return nil
}

// setWriteDeadline 设置/清空底层连接的写截止；writeTimeout<=0 时不设置。
func (s *streamWriter) setWriteDeadline(deadline time.Time) {
	if s.writeTimeout <= 0 {
		return
	}

	// 不缓存 ResponseController：它不逃逸，逐帧构造留在栈上；缓存进字段反而
	// 让这唯一一份逃逸到堆（实测每流多 1 分配，逐帧零收益）。
	_ = http.NewResponseController(s.w).SetWriteDeadline(deadline)
}

// writeSSE 原样写出一帧并 flush。
func (s *streamWriter) writeSSE(payload string) error {
	return s.writeSegments("data: ", payload, "\n\n")
}

// writeSSEBytes 写出一帧已是 JSON 字节的载荷：直接写 []byte，省去
// []byte→string 的整帧拷贝（流式高频路径每帧都会产生一份 json.Marshal 结果）。
func (s *streamWriter) writeSSEBytes(payload []byte) error {
	s.setWriteDeadline(time.Now().Add(s.writeTimeout))

	if _, err := io.WriteString(s.w, "data: "); err != nil {
		return fmt.Errorf("%w: %w", errClientWrite, err)
	}

	if _, err := s.w.Write(payload); err != nil {
		return fmt.Errorf("%w: %w", errClientWrite, err)
	}

	if _, err := io.WriteString(s.w, "\n\n"); err != nil {
		return fmt.Errorf("%w: %w", errClientWrite, err)
	}

	if s.fl != nil {
		s.fl.Flush()
	}

	return nil
}

// writeRaw 原样写出一帧（绕过 normalizeFrame）。上游 error 帧（error-passthrough）
// 与空流错误帧需保留 error 字段，不能被白名单剥掉，故经此写出。
// gateway_hint：上游 error 帧透出前按 hintFn 附加 error.gateway_hint 字段
// （error 对象上加一个键，message/code/requestId 等原文不动；hintFn 为
// nil / 空串 / 非 JSON 帧 → 原样写出，零改写）。
func (s *streamWriter) writeRaw(payload string) error {
	if hint := frameGatewayHint(s.hintFn, payload); hint != "" {
		payload = attachHintToErrorFrame(payload, hint)
	}

	return s.writeSSE(payload)
}

// writeChunk 把一个 chunk 对象按规范白名单重建后写出并 flush，同时维护三件跨帧
// 状态：tool_calls name 收敛（每 index 仅首片带 name）、消息 id 续传、以及上游
// 已用过的最大 tool_call index（供标记修复接续编号）。
// fallback 是规范化失败时的兜底原文（与修复前「marshal 失败即原样透传」一致）。
func (s *streamWriter) writeChunk(obj map[string]any, fallback string) error {
	// 先按 index 收敛 tool_calls name/id（每 index 仅首片保留），再规范化透传。
	stripToolCallNames(obj, s.tools, s.toolCallSeen)
	stripToolCallIDs(obj, s.tools, s.toolCallIDs)
	s.trackFinishReason(obj)
	s.continueMessageID(obj)
	s.trackMaxToolIdx(obj)

	raw, err := json.Marshal(normalizeFrame(obj))
	if err != nil {
		return s.writeSSE(fallback)
	}

	return s.writeSSEBytes(raw)
}

// continueMessageID 维护消息 id 续传：首帧非空真实 id 缓存；后续帧缺 id / 空 id
// 一律用缓存值，有自己 id 的帧保持原样（不同流分裂的帧允许各自 id）。
func (s *streamWriter) continueMessageID(obj map[string]any) {
	if s.firstID == "" {
		if v, ok := obj["id"].(string); ok && v != "" {
			s.firstID = v
		}

		return
	}

	if v, ok := obj["id"].(string); !ok || v == "" {
		obj["id"] = s.firstID
	}
}

// trackMaxToolIdx 记录上游已用过的最大 tool_call index（供标记修复接续编号）。
//
//nolint:gocognit // 逐 choice/delta/tool_calls 提取最大 index。
func (s *streamWriter) trackMaxToolIdx(obj map[string]any) {
	chs, ok := obj["choices"].([]any)
	if !ok {
		return
	}

	for _, ci := range chs {
		c, _ := ci.(map[string]any)
		if c == nil {
			continue
		}

		d, _ := c["delta"].(map[string]any)
		if d == nil {
			continue
		}

		tcs, _ := d["tool_calls"].([]any)
		for _, tci := range tcs {
			tc, _ := tci.(map[string]any)
			if tc == nil {
				continue
			}

			if v, ok := tc["index"].(float64); ok && int(v) > s.maxToolIdx {
				s.maxToolIdx = int(v)
			}
		}
	}
}

// repairFrame 在透传前跑标记修复，并把一帧展开成「可能多帧」：正文帧（标记被摘掉
// 后的可见文本）+ N 个 delta.tool_calls 帧 + 收尾 finish_reason 帧。没有命中标记
// 时返回 nil（调用方直接写原帧），与修复前逐字节一致。
func (s *streamWriter) repairFrame(obj map[string]any) []map[string]any {
	if !s.repair.Enabled() {
		return nil
	}

	chs, ok := obj["choices"].([]any)
	if !ok {
		return nil
	}

	calls := s.feedRepair(chs)
	if len(calls) == 0 {
		// 本帧没命中标记；但本流此前还原过调用 → 把上游的 stop 收尾改写成
		// tool_calls，客户端据此知道「回合以工具调用结束」而不是「模型只说了话」。
		if s.synthIdx > 0 && rewriteFinishReason(obj, "tool_calls") {
			s.toolFinishSent = true
		}

		return nil
	}

	return s.expandMarkupCalls(obj, calls)
}

// feedRepair 把各 choice 的 delta.content 喂给修复器，返回还原出的调用；就地改写
// 正文（标记摘除后为空则删 content 键）。
func (s *streamWriter) feedRepair(chs []any) []MarkupCall {
	var calls []MarkupCall

	for _, ci := range chs {
		c, ok := ci.(map[string]any)
		if !ok {
			continue
		}

		table, ok := c["delta"].(map[string]any)
		if !ok {
			continue
		}

		txt, ok := table["content"].(string)
		if !ok || txt == "" {
			continue
		}

		emit, got := s.repair.Feed(txt)
		calls = append(calls, got...)

		if emit == "" {
			delete(table, "content")
		} else {
			table["content"] = emit
		}
	}

	return calls
}

// expandMarkupCalls 把命中标记的一帧展开成正文帧 + N 个 tool_calls 帧 + 收尾帧。
// 命中：finish_reason 必须挪到工具调用帧之后，否则客户端在读到调用之前就认为回合
// 已经结束。注意「本帧本来没有 finish_reason」时**不得**补一帧收尾——上游的收尾帧
// 随后就到，补了会变成两个 finish_reason（后续那个由 rewriteFinishReason 改写）。
func (s *streamWriter) expandMarkupCalls(obj map[string]any, calls []MarkupCall) []map[string]any {
	str := takeFinishReason(obj)
	out := []map[string]any{obj}

	for _, call := range calls {
		idx := s.maxToolIdx + 1 + s.synthIdx
		s.synthIdx++

		out = append(out, markupCallFrame(obj, idx, call))
	}

	if str == "" {
		return out
	}

	if str == "stop" {
		str = "tool_calls"
	}

	out = append(out, finishOnlyFrame(obj, str))
	s.toolFinishSent = str == "tool_calls"

	return out
}

// writeFrame 把 payload 按规范白名单重建后以 data: 帧写出并 flush。
// 仅 JSON 对象解析成功时计数记为一次有效转发（JSON 解析失败或顶层 null 照常
// 降级原样写出，但不计数）。
func (s *streamWriter) writeFrame(payload string) (int, error) {
	var obj map[string]any
	if json.Unmarshal([]byte(payload), &obj) != nil || obj == nil {
		// 非 JSON 帧或顶层 null（nil map 没有可透传字段，后续写 id 会 panic）：
		// 原样降级写出，不计数（与修复前一致）。
		if werr := s.writeSSE(payload); werr != nil {
			return 0, werr
		}

		return 0, nil
	}
	// 上游错误帧透传（error-passthrough）：带 error 键的帧**原样写出**，不走
	// normalizeFrame 白名单——白名单会剥掉 error 字段，客户端就看不到上游
	// code/msg/requestId。error.message 即上游原文（如 6004 限流、审核拦截），
	// 计入有效帧（避免误判空流补写 "empty upstream stream"）。
	if _, hasErr := obj["error"]; hasErr {
		return s.emitErrorFrame(payload, payload)
	}
	// 顶层 {code,msg} 业务错误帧（issue #244）：原帧若直接走白名单会被洗成空
	// chunk。观察者拿原始 payload 分类，客户端拿带原始 code/msg 的 error 帧。
	if code, msg, isErr := frameBusinessError(obj); isErr {
		return s.emitErrorFrame(payload, BusinessErrorFrame(payload, code, msg))
	}

	frames := s.repairFrame(obj)
	if frames == nil {
		if werr := s.writeChunk(obj, payload); werr != nil {
			return 0, werr
		}

		return 1, nil
	}

	for _, ch := range frames {
		if werr := s.writeChunk(ch, payload); werr != nil {
			return 0, werr
		}
	}

	return 1, nil
}

// emitErrorFrame 写出一个错误帧并计一次有效转发：观察者（账号处置用）拿到的
// 始终是上游原始 payload，写出的是 out（error 帧即原文，业务错误帧是重编码形态）。
func (s *streamWriter) emitErrorFrame(original, out string) (int, error) {
	if s.opts.onErrorFrame != nil {
		s.opts.onErrorFrame(original)
	}

	if werr := s.writeRaw(out); werr != nil {
		return 0, werr
	}

	return 1, nil
}

// stream 读取 r 并逐帧透传到 w（含空流兜底与尾部收尾）。
// 上游读错误（io.EOF 之外的 Read 错误）原样上抛，供调用方与「客户端写失败」
// 区分（issue #105）：读错误是上游/连接的问题，写失败是客户端已走。
// 单行超上限（issue #113）：向客户端写一帧明确 error 后收尾（[DONE] 照写），
// 并返回 ErrSSELineTooLong——客户端不会把半截超长行当正常内容，调用方按上游
// 失败处置账号。
//
//nolint:gocognit // 读帧/写帧/空流兜底/错误分类线性展开。
func (s *streamWriter) stream(r io.Reader) error {
	// 流结束（含写失败）清掉写截止：连接可能被 keep-alive 复用，
	// 残留的过期 deadline 会让后续请求的响应写立即失败。
	defer s.setWriteDeadline(time.Time{})

	if s.keepalive > 0 {
		return s.streamWithKeepalive(r)
	}

	reader := getSSEReader(r)
	defer putSSEReader(reader)

	validFrames := 0

	for {
		line, err := ReadSSELine(reader)
		if err != nil && !errors.Is(err, io.EOF) {
			// 超长行（防御上限，issue #113）：先给客户端一个明确的 error 帧再收尾，
			// 不把超限行的已读部分当内容透传（也不回退——那正是 OOM 的成因）。
			if errors.Is(err, ErrSSELineTooLong) {
				s.finishSizeLimit()
			}

			return err
		}

		n, done, werr := s.handleStreamLine(line)
		validFrames += n

		if werr != nil {
			// 多行 data 事件总量超限：与单行超限同口径，先写 error 帧 + [DONE]。
			if errors.Is(werr, ErrSSELineTooLong) {
				s.finishSizeLimit()
			}

			return werr // 客户端写失败（或帧解析失败）：原样上抛
		}

		if done {
			break
		}
		// 空行（帧分隔）吞掉：本函数自产 "\n\n"
		if errors.Is(err, io.EOF) {
			break
		}
	}

	return s.finish(&validFrames)
}

// lineResult 上游读取 goroutine 交给主循环的一行结果。
type lineResult struct {
	line string
	err  error
}

// streamWithKeepalive 在阻塞读取上游的同时按 keepalive 间隔写注释帧。
// 读放在独立 goroutine，主循环 select 读结果与心跳定时器；退出时关闭 r
// （若可关闭）以解除读阻塞，避免 goroutine 泄漏。
//
//nolint:gocognit // 读 goroutine 与心跳定时器双路 select，顺序即语义。
func (s *streamWriter) streamWithKeepalive(r io.Reader) error {
	lines, done, finished := asyncLines(r)
	// 返回前先关 done 再 Close 底层 reader（解除阻塞中的 Read），最后等读 goroutine
	// 真正退出。只关 done 不等待会让调用方在 goroutine 仍读 r 时继续使用/释放 r
	// （统计 reader 会把它包装的 bufio.Reader 归还池中，造成并发读写）。
	defer func() {
		close(done)

		if c, ok := r.(io.Closer); ok {
			_ = c.Close()

			<-finished
		}
	}()

	ticker := time.NewTicker(s.keepalive)
	defer ticker.Stop()

	lastWrite := time.Now()
	validFrames := 0

	for {
		select {
		case res := <-lines:
			n, stop, err := s.consumeLine(res, &lastWrite)
			validFrames += n

			if err != nil {
				// 单行超限（issue #113）：keepalive 路径同样先写明确 error 帧 +
				// [DONE]，再上抛让调用方按上游失败处置。
				if errors.Is(err, ErrSSELineTooLong) {
					s.finishSizeLimit()
				}

				return err
			}

			if stop {
				return s.finish(&validFrames)
			}
		case <-ticker.C:
			if err := s.keepaliveIfIdle(&lastWrite); err != nil {
				return err
			}
		}
	}
}

// asyncLines 启动逐行读取 goroutine；done 关闭后 goroutine 退出。
func asyncLines(r io.Reader) (<-chan lineResult, chan struct{}, <-chan struct{}) {
	reader := getSSEReader(r)
	lines := make(chan lineResult)
	done := make(chan struct{})
	finished := make(chan struct{})

	go func() {
		// LIFO：先归还 reader 再宣告退出，等待方看到 finished 时 reader 已不在使用。
		defer close(finished)
		defer putSSEReader(reader)

		for {
			line, err := ReadSSELine(reader)
			select {
			case lines <- lineResult{line: line, err: err}:
			case <-done:
				return
			}

			if err != nil {
				return
			}
		}
	}()

	return lines, done, finished
}

// consumeLine 处理一行并更新最近写出时间；stop=true 表示应进入收尾（[DONE]/EOF）。
func (s *streamWriter) consumeLine(res lineResult, lastWrite *time.Time) (n int, stop bool, err error) {
	n, stop, err = s.handleStreamLine(res.line)
	if n > 0 || strings.TrimSpace(res.line) != "" {
		*lastWrite = time.Now()
	}

	if err != nil || stop || res.err == nil {
		return n, stop, err
	}

	if errors.Is(res.err, io.EOF) {
		return n, true, nil
	}

	return n, false, res.err
}

// keepaliveIfIdle 静默超过间隔时写一帧 SSE 注释心跳。
func (s *streamWriter) keepaliveIfIdle(lastWrite *time.Time) error {
	if time.Since(*lastWrite) < s.keepalive {
		return nil
	}
	// 复用逐行写路径：升级到带写截止的实现（如 issue #53）后，心跳同样受
	// 单次写上限保护，不会在客户端不消费时无限阻塞。
	if _, _, err := s.handleStreamLine(": keepalive\n\n"); err != nil {
		return err
	}

	*lastWrite = time.Now()

	return nil
}

// handleStreamLine 处理一行，返回写入的有效帧数；done=true 表示遇 [DONE] 停止。
func (s *streamWriter) handleStreamLine(line string) (n int, done bool, err error) {
	trimmed := strings.TrimRight(line, "\r\n")
	switch {
	case strings.HasPrefix(trimmed, "data:"):
		payload, _ := sseDataPayload(trimmed) // SSE 允许 "data:" 后不带空格
		if sseDone(payload) {
			// 先吐出已累积的多行 data，再结束；[DONE] 统一在收尾写一次。
			n, werr := s.flushData()
			if werr != nil {
				return n, false, werr
			}

			s.sawDone = true

			return n, true, nil
		}

		// 非规范上游可能用单个 \n 分隔事件：上一行本身已是完整 JSON 时先落帧，
		// 避免把两个对象拼成一行。只校验单行（O(line)），不对累积缓冲反复 join。
		if len(s.dataLines) == 1 && json.Valid([]byte(s.dataLines[0])) {
			n, werr := s.flushData()
			if werr != nil {
				return n, false, werr
			}

			// flushData 已清零 dataLines/dataBytes，继续走下面的追加。
			return s.appendDataLine(n, payload)
		}

		return s.appendDataLine(0, payload)
	case trimmed == "":
		n, werr := s.flushData()
		return n, false, werr
	default:
		n, werr := s.flushThenWriteLine(line)
		return n, false, werr
	}
}

// flushThenWriteLine 先落帧已缓冲的 data，再原样透传非 data 行（`event:` / `id:` /
// `retry:` / 心跳注释）。否则 data 与空行之间插入的事件字段会先于数据写出，事件
// 类型/序号与数据分离（issue #421）。[DONE] 与空行两个分支同理。
func (s *streamWriter) flushThenWriteLine(line string) (int, error) {
	n, err := s.flushData()
	if err != nil {
		return n, err
	}

	return n, s.writeAll(line)
}

// appendDataLine 把一行 data 追加到当前事件缓冲，并施加事件总量上限：
// 单行上限只约束一行，异常上游持续发 data 行且不发空行时累积缓冲会无限增长。
// 与单行超限同口径返回 ErrSSELineTooLong（调用方写 error 帧 + [DONE]）。
func (s *streamWriter) appendDataLine(n int, payload string) (int, bool, error) {
	add := len(payload)
	if len(s.dataLines) > 0 {
		add++ // 行间 "\n"
	}

	if s.dataBytes+add > MaxSSELineBytes {
		return n, false, fmt.Errorf("%w: > %d bytes", ErrSSELineTooLong, MaxSSELineBytes)
	}

	s.dataLines = append(s.dataLines, payload)
	s.dataBytes += add

	return n, false, nil
}

// flushData 把一个完整 SSE 事件的多行 data 按规范用 \n 拼接后写出一帧。
func (s *streamWriter) flushData() (int, error) {
	if len(s.dataLines) == 0 {
		return 0, nil
	}

	payload := strings.Join(s.dataLines, "\n")
	s.dataLines = s.dataLines[:0]
	s.dataBytes = 0

	n, err := s.writeFrame(payload)
	if err != nil {
		return 0, err
	}

	return n, nil
}

// finishSizeLimit 单行/总量超限的收尾（issue #113）：先写一帧明确 error
// （绕过 normalizeFrame 保留 error 字段），再补 [DONE] 保证客户端能正常收尾。
// 与空流兜底同形态：HTTP 头已发出只能 200，error 帧是客户端唯一能看到的失败信号。
// 错误原因由 stream 原样返回给调用方（日志/观测），不在这里二次编造。
func (s *streamWriter) finishSizeLimit() {
	_ = s.writeRaw(`{"error":{"message":"upstream sse line exceeded size limit",` +
		`"type":"upstream_error","code":"upstream_parse"}}`)
	if _, err := io.WriteString(s.w, "data: [DONE]\n\n"); err != nil {
		return
	}

	if s.fl != nil {
		s.fl.Flush()
	}
}

// finish 收尾：标记修复尾部回吐 + 空流兜底 error 帧 + 保证恰好一个 [DONE]。
// s.sawDone 为上游是否显式发过 data: [DONE]；false（EOF 截断）时收尾后返回
// errTruncatedStream，让调用方把这次请求记为上游失败（issue #105）。
// [DONE] 兜底帧照写：客户端已开始读流，必须有一个收尾标记，不能中途悬挂。
func (s *streamWriter) finish(validFrames *int) error {
	if err := s.flushPending(validFrames); err != nil {
		return err
	}

	if werr := s.finishRepair(validFrames); werr != nil {
		return werr
	}

	s.writeFinishFallback(*validFrames)
	// 保证恰好写一个 [DONE]（上游漏发时兜底补上）。
	if err := s.writeAll("data: [DONE]\n\n"); err != nil {
		return err
	}

	return s.finishResult(*validFrames)
}

// flushPending 把尚未写出的事件数据落帧并计入有效帧数。
func (s *streamWriter) flushPending(validFrames *int) error {
	n, err := s.flushData()
	if err != nil {
		return err
	}

	*validFrames += n

	return nil
}

// writeFinishFallback 空流/截断时给客户端补一帧明确 error（HTTP 头已发出只能 200）。
func (s *streamWriter) writeFinishFallback(validFrames int) {
	switch {
	case validFrames == 0:
		_ = s.writeRaw(`{"error":{"message":"empty upstream stream",` +
			`"type":"upstream_error","code":"upstream_parse"}}`)
	case !s.sawDone && !s.sawFinish:
		_ = s.writeRaw(`{"error":{"message":"upstream stream truncated before [DONE]",` +
			`"type":"upstream_error","code":"upstream_parse"}}`)
	}
}

// finishResult 把收尾形态映射为哨兵错误（空流/截断都不是成功）。
func (s *streamWriter) finishResult(validFrames int) error {
	switch {
	case validFrames == 0:
		return errEmptyStream
	case !s.sawDone && !s.sawFinish:
		return errTruncatedStream
	default:
		return nil
	}
}

// finishRepair 标记修复的尾部收尾：流结束时仍未判定的字节一律原文回吐（绝不吞字节，
// 未闭合的块连起始标记一起交还）；若本流还原过调用、但上游从未给出收尾帧，补一帧
// finish_reason: tool_calls，保证客户端不会把「工具调用回合」读成「模型只说了话」。
func (s *streamWriter) finishRepair(validFrames *int) error {
	if !s.repair.Enabled() {
		return nil
	}

	if tail := s.repair.Flush(); tail != "" {
		tailObj := map[string]any{
			"object": "chat.completion.chunk",
			"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{"content": tail},
			}},
		}
		if werr := s.writeChunk(tailObj, ""); werr != nil {
			return werr
		}

		*validFrames++
	}

	if s.synthIdx > 0 && !s.toolFinishSent {
		if werr := s.writeChunk(finishOnlyFrame(map[string]any{}, "tool_calls"), ""); werr != nil {
			return werr
		}
	}

	return nil
}

// frameGatewayHint 取 error 帧的 gateway_hint（hintFn 缺失/异常返回空串 → 不附加）。
func frameGatewayHint(hintFn func(string) string, payload string) string {
	if hintFn == nil {
		return ""
	}
	// panic 隔离：hint 判定是补充功能，任何实现缺陷不得击穿流透传主路径。
	defer func() { _ = recover() }()

	return strings.TrimSpace(hintFn(payload))
}

// attachHintToErrorFrame 在 error 帧的 error 对象上附加 gateway_hint 字段。
// message/code/requestId 等既有键原样保留（只加不改）；非 JSON / 无 error 对象 →
// payload 原样返回（宁可不加 hint 也不破坏原文透传）。
func attachHintToErrorFrame(payload, hint string) string {
	var obj map[string]any
	if json.Unmarshal([]byte(payload), &obj) != nil {
		return payload
	}

	e, ok := obj["error"].(map[string]any)
	if !ok {
		return payload
	}

	e["gateway_hint"] = hint

	out, err := json.Marshal(obj)
	if err != nil {
		return payload
	}

	return string(out)
}

// takeFinishReason 取出并删除所有 choice 的 finish_reason，返回第一个非空值。
// 标记修复把一帧拆成多帧时需要它：finish_reason 必须落在工具调用帧之后，否则
// 客户端在读到调用之前就认为回合结束了。
func takeFinishReason(obj map[string]any) string {
	str := ""

	chs, _ := obj["choices"].([]any)
	for _, ci := range chs {
		table, _ := ci.(map[string]any)
		if table == nil {
			continue
		}

		if v, ok := table["finish_reason"].(string); ok && v != "" && str == "" {
			str = v
		}

		delete(table, "finish_reason")
	}

	return str
}

// rewriteFinishReason 把 choice 上「已有且为 stop」的 finish_reason 改写成 want。
// 本来就没有 finish_reason 的中间帧一律不动（绝不凭空造收尾），已有其他值
// （length / content_filter 等）时也不动——那些收尾语义更需要被客户端看到。
// 返回是否发生了改写。
func rewriteFinishReason(obj map[string]any, want string) bool {
	changed := false

	chs, _ := obj["choices"].([]any)
	for _, ci := range chs {
		table, _ := ci.(map[string]any)
		if table == nil {
			continue
		}

		if v, ok := table["finish_reason"].(string); ok && v == "stop" {
			table["finish_reason"] = want
			changed = true
		}
	}

	return changed
}

// markupCallFrame 以 src 帧为模板，构造一帧只带单个 delta.tool_calls 的 chunk。
// id 不在这里写：交给 writeChunk 的 id 续传逻辑补齐（src 帧先写出，firstID 已就位）。
func markupCallFrame(src map[string]any, idx int, call MarkupCall) map[string]any {
	frame := map[string]any{
		"object": "chat.completion.chunk",
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{
				"tool_calls": markupToolCalls([]MarkupCall{call}, idx),
			},
		}},
	}

	for _, k := range []string{"created", "model"} {
		if v, ok := src[k]; ok && v != nil {
			frame[k] = v
		}
	}

	return frame
}

// finishOnlyFrame 构造一帧只带 finish_reason 的收尾 chunk（空 delta）。
func finishOnlyFrame(src map[string]any, finish string) map[string]any {
	frame := map[string]any{
		"object": "chat.completion.chunk",
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": finish,
		}},
	}

	for _, k := range []string{"created", "model"} {
		if v, ok := src[k]; ok && v != nil {
			frame[k] = v
		}
	}

	return frame
}
