// sse.go 处理上游 SSE 流：聚合成单个 OpenAI 响应，或透传给客户端。
package upstream

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// errEmptyStream 上游返回 200 但没有有效 SSE 数据帧（空流/只有注释/[DONE]）。
// 用哨兵错误替代裸 fmt.Errorf：StreamHint 的调用方（handler 流式路径）需要区分
// 「上游空流」与「客户端断连写失败」——空流是上游缺陷，应记 502 观测；写失败是
// 客户端已走，日志口径不同。Aggregate 与 StreamHint 共用同一哨兵（errors.Is 判定）。
var errEmptyStream = errors.New("upstream stream contained no valid data events")

// IsEmptyStreamError 报告错误是否为「上游空流」（无有效 SSE 帧）——供 handler
// 在流式路径把空流记为失败观测（HTTP 头已发出只能 200，但日志/状态应收敛到
// upstream_parse 同语义），与客户端断连类错误区分。
func IsEmptyStreamError(err error) bool { return errors.Is(err, errEmptyStream) }

// Aggregate 读取完整 SSE 流，聚合 delta.content 为单个 OpenAI chat.completion 响应。
// 分片/半行由 bufio.Reader.ReadString 处理；遇到 "data: [DONE]" 结束。
// tool_calls 以流式 delta 到达（按 index 合并：首片带 id/type/name，后续只带 arguments 片段）。
func Aggregate(r io.Reader) (map[string]any, error) {
	a := newStreamAggregator()
	br := bufio.NewReaderSize(r, 64*1024)
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if a.handleLine(line) {
			break // 上游显式发过 data: [DONE]（正常收尾）
		}
		if err == io.EOF {
			break
		}
	}
	return a.result()
}

// streamAggregator Aggregate 的聚合状态机：把逐帧解析拆到方法上，降低单函数复杂度。
type streamAggregator struct {
	id, model     string
	created       float64
	content       strings.Builder
	reasoning     strings.Builder
	role          string
	finishReason  string
	usage         map[string]any
	gotAnyContent bool
	validEvents   int
	sawDone       bool // 上游显式发过 data: [DONE]（正常收尾）
	toolCalls     map[int]map[string]any
	toolOrder     []int
	// toolSeq 缺 index 的 tool_call 的分配序号源：跨帧延续「最近分配」槽位，
	// 同帧内递增（见 mergeToolCallsChunk 注释）。
	toolSeq int
	// idIndex id → 已分配的 index：跨帧持续（延续碎片的 id 常出现在后续帧），
	// 供缺 index 时按 id 归位既有调用（见 mergeToolCallsChunk 注释）。
	idIndex map[string]int
}

// newStreamAggregator 初始化聚合状态（role/finish_reason 有默认值）。
func newStreamAggregator() *streamAggregator {
	return &streamAggregator{
		role:         "assistant",
		finishReason: "stop",
		toolCalls:    map[int]map[string]any{},
		idIndex:      map[string]int{},
	}
}

// handleLine 处理一行：data: 帧解析聚合，[DONE] 返回 true（调用方停止读取）。
func (a *streamAggregator) handleLine(line string) bool {
	if !strings.HasPrefix(line, "data: ") {
		return false
	}
	payload := strings.TrimPrefix(line, "data: ")
	if payload == "[DONE]" {
		// 上游显式结束：停止读取，DONE 之后的任何数据一律忽略。
		a.sawDone = true
		return true
	}
	var chunk map[string]any
	if json.Unmarshal([]byte(payload), &chunk) == nil {
		// 有效事件计数：仅 JSON 解析成功的数据帧计入（解析失败沿用静默 continue）。
		a.validEvents++
		a.handleChunk(chunk)
	}
	return false
}

// handleChunk 并入一个数据帧的顶层字段与 choices。
func (a *streamAggregator) handleChunk(chunk map[string]any) {
	a.mergeChunkMeta(chunk)
	ch, ok := chunk["choices"].([]any)
	if !ok {
		return
	}
	for _, ci := range ch {
		c, _ := ci.(map[string]any)
		if c == nil {
			continue
		}
		a.handleChoice(c)
	}
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
func (a *streamAggregator) handleChoice(c map[string]any) {
	if fr, ok := c["finish_reason"].(string); ok && fr != "" {
		a.finishReason = fr
	}
	if delta, ok := c["delta"].(map[string]any); ok {
		a.handleDelta(delta)
	}
	// 有的上游把完整消息放在 message 里（非 delta）：整条并入（role/content/
	// reasoning_content/tool_calls），与 delta 分支同构。delta 已取过正文
	// （gotAnyContent）则跳过（避免与 delta 路径重复拼接——PR #134 的 latch 语义）。
	if msg, ok := c["message"].(map[string]any); ok && !a.gotAnyContent {
		a.mergeMessageFields(msg)
	}
}

// handleDelta 并入一个流式 delta（role/content/reasoning_content/tool_calls）。
func (a *streamAggregator) handleDelta(delta map[string]any) {
	if r2, ok := delta["role"].(string); ok && r2 != "" {
		a.role = r2
	}
	if txt, ok := delta["content"].(string); ok {
		a.appendContent(txt)
	}
	if rc, ok := delta["reasoning_content"].(string); ok {
		a.reasoning.WriteString(rc)
	}
	if tcs, ok := delta["tool_calls"].([]any); ok {
		a.mergeToolCallsChunk(tcs)
	}
}

// mergeMessageFields 把非 delta 的完整 message 内容并入聚合（message 是整条下发，
// 非流式拼接，content 只取一次）。role/reasoning_content/tool_calls 与 delta 分支
// 同构透出；content 同样置 gotAnyContent，与 delta 路径的 latch 语义一致
// （一帧整条 message 之后，后续 delta 帧不重复追加）。
func (a *streamAggregator) mergeMessageFields(msg map[string]any) {
	if r2, ok := msg["role"].(string); ok && r2 != "" {
		a.role = r2
	}
	if txt, ok := msg["content"].(string); ok {
		a.appendContent(txt)
	}
	if rc, ok := msg["reasoning_content"].(string); ok {
		a.reasoning.WriteString(rc)
	}
	if tcs, ok := msg["tool_calls"].([]any); ok {
		a.mergeToolCallsChunk(tcs)
	}
}

// appendContent 是「已取到正文」（gotAnyContent latch）的唯一写入点：delta 与
// message 两路 content 都必须经此并入，规约只有一份（issue #142）——
//
//	S1 空串不算「已取到正文」、不占 latch 名额：OpenAI 风格 role-only 首帧
//	   （delta.content=""）和整条空 message 帧是常态帧，若空串置位 latch，
//	   后续真正文会被 message 回退分支的 !gotAnyContent 守卫静默拒绝；
//	S2 空串本身也无可追加字节，跳过 WriteString 与追加语义自洽。
func (a *streamAggregator) appendContent(txt string) {
	if txt == "" {
		return
	}
	a.content.WriteString(txt)
	a.gotAnyContent = true
}

// nextToolIndex 分配下一个不冲突的缺 index 序号：从 toolSeq 起递增跳过既有
// index（合规流的 index 是 0..N-1，缺 index 的补位不能覆盖它们）。
func (a *streamAggregator) nextToolIndex() int {
	for {
		idx := a.toolSeq
		a.toolSeq++
		if _, used := a.toolCalls[idx]; !used {
			return idx
		}
	}
}

// toolCallIndex 解析单个 tool_call 的归属 index，兼容上游省略 index 的形态。
//
// index 缺失兼容：OpenAI 规范要求 delta 帧的 tool_call 带 index（标记分片归属），
// 但部分上游省略它。此前缺 index 一律归 0——多调用场景下不同 call 被合并进同一
// index 槽，arguments 串联、name 互相覆盖（数据污染）。修法按「id 优先、lastIdx 兜底」：
//   - 带 index → 按 index 累积（合规形态，零改动）；
//   - 缺 index 带 id 且 id 已见过 → 延续该 id 所在 index；
//   - 缺 index 带 id 且 id 是新的 → 开新序号（多调用不合并）；
//   - 缺 index 无 id → 追加到最近收到碎片的 index（单个调用的延续分片无 id
//     是标准形态），无既往则开新号。
func (a *streamAggregator) toolCallIndex(call map[string]any) int {
	if v, ok := call["index"].(float64); ok {
		return int(v)
	}
	cid, _ := call["id"].(string)
	if cid != "" {
		if mid, seen := a.idIndex[cid]; seen {
			return mid // 该 id 已归位过：延续既有调用（跨帧有效）
		}
		return a.nextToolIndex()
	}
	if len(a.toolOrder) > 0 {
		return a.toolOrder[len(a.toolOrder)-1] // 无 id 碎片：延续最近调用
	}
	return a.nextToolIndex()
}

// mergeToolCallsChunk 把一段 tool_calls 数组按 index 合并进累计表。
// delta（流式分片，按 index 累积）与 message（非 delta 整条）共用同一合并逻辑，
// 保证「上游给的身份/函数名不丢、arguments 拼接语义一致」。
func (a *streamAggregator) mergeToolCallsChunk(tcs []any) {
	for _, tc := range tcs {
		call, ok := tc.(map[string]any)
		if !ok {
			continue
		}
		idx := a.toolCallIndex(call)
		merged, seen := a.toolCalls[idx]
		if !seen {
			merged = map[string]any{"index": idx}
			a.toolCalls[idx] = merged
			a.toolOrder = append(a.toolOrder, idx)
		}
		if cid, _ := call["id"].(string); cid != "" {
			a.idIndex[cid] = idx
		}
		if cid, _ := merged["id"].(string); cid != "" {
			a.idIndex[cid] = idx
		}
		mergeToolCallDelta(merged, call)
	}
}

// result 由聚合状态构造最终的非流式响应；无有效事件返回 errEmptyStream。
func (a *streamAggregator) result() (map[string]any, error) {
	if a.validEvents == 0 {
		// 上游返回 200 但没有任何有效数据事件（空流/只有 [DONE]/只有注释行）：
		// 不再合成空 content 的假成功响应，直接报错，由 handler 映射为 502 upstream_parse。
		return nil, errEmptyStream
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
	a.attachToolCalls(message)
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

// attachToolCalls 把累计的 tool_calls 按序写入 message（截断流剔除残缺参数）。
func (a *streamAggregator) attachToolCalls(message map[string]any) {
	if len(a.toolOrder) == 0 {
		return
	}
	sort.Ints(a.toolOrder)
	calls := make([]map[string]any, 0, len(a.toolOrder))
	for _, idx := range a.toolOrder {
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

// ensureUsageTotal 在 usage 缺 total_tokens 但 prompt_tokens/completion_tokens 都在时
// 补齐 total = prompt + completion（通过新 map 合并，不修改原上游 map）。
// 任一缺失或已有 total 时原样返回。
func ensureUsageTotal(u map[string]any) map[string]any {
	if _, ok := u["total_tokens"]; ok {
		return u
	}
	pt, pok := num64(u["prompt_tokens"])
	ct, cok := num64(u["completion_tokens"])
	if !pok || !cok {
		return u
	}
	out := make(map[string]any, len(u)+1)
	for k, v := range u {
		out[k] = v
	}
	out["total_tokens"] = pt + ct
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
	df, _ := delta["function"].(map[string]any)
	if df == nil {
		return
	}
	mf, _ := merged["function"].(map[string]any)
	if mf == nil {
		mf = map[string]any{}
		merged["function"] = mf
	}
	setStringIfNonEmpty(mf, df, "name")
	appendStringIfNonEmpty(mf, df, "arguments")
}

// setStringIfNonEmpty 把 src[key] 的非空字符串覆盖写入 dst[key]（缺失/空串不动）。
func setStringIfNonEmpty(dst, src map[string]any, key string) {
	if v, ok := src[key].(string); ok && v != "" {
		dst[key] = v
	}
}

// appendStringIfNonEmpty 把 src[key] 的非空字符串拼接到 dst[key] 已有字符串之后。
func appendStringIfNonEmpty(dst, src map[string]any, key string) {
	v, ok := src[key].(string)
	if !ok || v == "" {
		return
	}
	if prev, _ := dst[key].(string); prev != "" {
		dst[key] = prev + v
	} else {
		dst[key] = v
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
// seen 记录每个 index 是否已发过首片（与 name 是否非空无关）；删除是幂等的。
// 只动 function.name 键，id/type/arguments 原样透传。
func stripToolCallNames(obj map[string]any, seen map[int]bool) {
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
			tc, _ := tci.(map[string]any)
			if tc == nil {
				continue
			}
			idx := 0
			if v, ok := tc["index"].(float64); ok {
				idx = int(v)
			}
			if seen[idx] {
				// 已发过首片：删除本分片的 name 键（存在即删，幂等）。
				if fn, _ := tc["function"].(map[string]any); fn != nil {
					delete(fn, "name")
				}
				continue
			}
			// 首现：保留 name 键原样（上游首片通常带非空 name；空 name 也照发，
			// 与 OpenAI 对「首帧无 name」的容忍一致），随后分片统一删除。
			seen[idx] = true
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
func normalizeChoice(c map[string]any) map[string]any {
	nc := map[string]any{}
	if idx, ok := c["index"]; ok {
		nc["index"] = idx
	}
	nc["delta"] = normalizeDelta(c["delta"])
	if fr, ok := c["finish_reason"].(string); ok && fr != "" {
		nc["finish_reason"] = fr
	} else {
		nc["finish_reason"] = nil
	}
	return nc
}

// normalizeDelta 以白名单重建 delta：仅保留非空的 role/content/reasoning_content/
// refusal，非空 tool_calls，以及非空占位的 function_call。
func normalizeDelta(raw any) map[string]any {
	d, ok := raw.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	delta := map[string]any{}
	for _, k := range []string{"role", "content", "reasoning_content", "refusal"} {
		if v, ok := d[k].(string); ok && v != "" {
			delta[k] = v
		}
	}
	if tcs, ok := d["tool_calls"].([]any); ok && len(tcs) > 0 {
		delta["tool_calls"] = tcs
	}
	if fc, ok := d["function_call"]; ok && fc != nil {
		// 空占位 function_call（name/arguments 全空）视为噪声剔除。
		if keepFunctionCall(fc) {
			delta["function_call"] = fc
		}
	}
	return delta
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

// Stream 透传上游 SSE 到 w（逐帧规范化后 flush），保证至少写一个 [DONE]。
// 调用方必须先设置过 status 200；本函数自设 SSE headers。
// 流式策略：逐帧透传（规范化已剥空 content 噪声），恢复与上游一致的平滑流式。
//
// StreamHint 变体（gateway_hint 任务）：上游 error 帧透传时附加
// error.gateway_hint 字段——message 原文不动，hint 并列补充；hintFn 返回空串
// 或 nil 时与 Stream 行为逐字节一致。
func Stream(w http.ResponseWriter, r io.Reader) error {
	return StreamHint(w, r, nil)
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
type streamWriter struct {
	w      http.ResponseWriter
	fl     http.Flusher
	hintFn func(string) string
	opts   streamOptions
	// keepalive 上游静默超过该时长时写一帧 SSE 注释心跳；0 = 关闭。
	keepalive time.Duration

	// writeTimeout 单次写帧/Flush 的截止时长；<=0 不设截止（测试可注入短值）。
	writeTimeout time.Duration

	// toolCallSeen 跨帧记录 delta.tool_calls 里已发过首片的 index，
	// 供逐 chunk 透传时收敛 name 为「每 index 一次」（对齐 OpenAI 官方流）。
	toolCallSeen map[int]bool

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
	var o streamOptions
	for _, f := range opts {
		if f != nil {
			f(&o)
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
		opts:         o,
		writeTimeout: clientWriteTimeout,
		keepalive:    o.keepalive,
		toolCallSeen: map[int]bool{},
		repair:       o.markup,
		maxToolIdx:   -1,
	}
}

// writeAll 写出原始字节并 flush，同时在支持 ResponseController 的连接上设置
// 单次写截止（issue #53）。不支持 SetWriteDeadline 的 ResponseWriter（测试用
// ResponseRecorder 或中间件包装）忽略错误：拿不到截止支持不能反过来拒绝写流。
// 每次写前重设 deadline；流结束后由 stream 清空，避免过期 deadline 污染
// keep-alive 连接上的后续请求。
func (s *streamWriter) writeAll(payload string) error {
	s.setWriteDeadline(time.Now().Add(s.writeTimeout))
	if _, werr := io.WriteString(s.w, payload); werr != nil {
		return werr
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
	_ = http.NewResponseController(s.w).SetWriteDeadline(deadline)
}

// writeSSE 原样写出一帧并 flush。
func (s *streamWriter) writeSSE(payload string) error {
	return s.writeAll("data: " + payload + "\n\n")
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
	// 先按 index 收敛 tool_calls name（每 index 仅首片保留，后续分片删 name 键），再规范化透传。
	stripToolCallNames(obj, s.toolCallSeen)
	s.continueMessageID(obj)
	s.trackMaxToolIdx(obj)
	raw, err := json.Marshal(normalizeFrame(obj))
	if err != nil {
		return s.writeSSE(fallback)
	}
	return s.writeSSE(string(raw))
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
// 时原样返回入参帧（单元素），与修复前逐字节一致。
func (s *streamWriter) repairFrame(obj map[string]any) []map[string]any {
	if !s.repair.Enabled() {
		return []map[string]any{obj}
	}
	chs, ok := obj["choices"].([]any)
	if !ok {
		return []map[string]any{obj}
	}
	calls := s.feedRepair(chs)
	if len(calls) == 0 {
		// 本帧没命中标记；但本流此前还原过调用 → 把上游的 stop 收尾改写成
		// tool_calls，客户端据此知道「回合以工具调用结束」而不是「模型只说了话」。
		if s.synthIdx > 0 && rewriteFinishReason(obj, "tool_calls") {
			s.toolFinishSent = true
		}
		return []map[string]any{obj}
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
		d, ok := c["delta"].(map[string]any)
		if !ok {
			continue
		}
		txt, ok := d["content"].(string)
		if !ok || txt == "" {
			continue
		}
		emit, got := s.repair.Feed(txt)
		calls = append(calls, got...)
		if emit == "" {
			delete(d, "content")
		} else {
			d["content"] = emit
		}
	}
	return calls
}

// expandMarkupCalls 把命中标记的一帧展开成正文帧 + N 个 tool_calls 帧 + 收尾帧。
// 命中：finish_reason 必须挪到工具调用帧之后，否则客户端在读到调用之前就认为回合
// 已经结束。注意「本帧本来没有 finish_reason」时**不得**补一帧收尾——上游的收尾帧
// 随后就到，补了会变成两个 finish_reason（后续那个由 rewriteFinishReason 改写）。
func (s *streamWriter) expandMarkupCalls(obj map[string]any, calls []MarkupCall) []map[string]any {
	fr := takeFinishReason(obj)
	out := []map[string]any{obj}
	for _, call := range calls {
		idx := s.maxToolIdx + 1 + s.synthIdx
		s.synthIdx++
		out = append(out, markupCallFrame(obj, idx, call))
	}
	if fr == "" {
		return out
	}
	if fr == "stop" {
		fr = "tool_calls"
	}
	out = append(out, finishOnlyFrame(obj, fr))
	s.toolFinishSent = fr == "tool_calls"
	return out
}

// writeFrame 把 payload 按规范白名单重建后以 data: 帧写出并 flush。
// 仅 JSON 解析成功时计数记为一次有效转发（JSON 解析失败照常降级原样写出，但不计数）。
func (s *streamWriter) writeFrame(payload string) (int, error) {
	var obj map[string]any
	if json.Unmarshal([]byte(payload), &obj) != nil {
		// 非 JSON 帧：原样降级写出，不计数（与修复前一致）。
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
		// 旁路通知（账号处置用）：原始 payload 交给观察者，透传字节不变。
		if s.opts.onErrorFrame != nil {
			s.opts.onErrorFrame(payload)
		}
		if werr := s.writeRaw(payload); werr != nil {
			return 0, werr
		}
		return 1, nil
	}
	for _, ch := range s.repairFrame(obj) {
		if werr := s.writeChunk(ch, payload); werr != nil {
			return 0, werr
		}
	}
	return 1, nil
}

// stream 读取 r 并逐帧透传到 w（含空流兜底与尾部收尾）。
func (s *streamWriter) stream(r io.Reader) error {
	// 流结束（含写失败）清掉写截止：连接可能被 keep-alive 复用，
	// 残留的过期 deadline 会让后续请求的响应写立即失败。
	defer s.setWriteDeadline(time.Time{})
	if s.keepalive > 0 {
		return s.streamWithKeepalive(r)
	}
	br := bufio.NewReaderSize(r, 64*1024)
	validFrames := 0
	for {
		line, err := br.ReadString('\n')
		n, done, werr := s.handleStreamLine(line)
		validFrames += n
		if werr != nil {
			return werr
		}
		if done {
			break
		}
		// 空行（帧分隔）吞掉：本函数自产 "\n\n"
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
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
func (s *streamWriter) streamWithKeepalive(r io.Reader) error {
	if c, ok := r.(io.Closer); ok {
		defer c.Close()
	}
	lines, done := asyncLines(r)
	defer close(done)

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
func asyncLines(r io.Reader) (<-chan lineResult, chan struct{}) {
	br := bufio.NewReaderSize(r, 64*1024)
	lines := make(chan lineResult)
	done := make(chan struct{})
	go func() {
		for {
			line, err := br.ReadString('\n')
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
	return lines, done
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
	case strings.HasPrefix(trimmed, "data: [DONE]"):
		// 上游显式结束：停止读取，DONE 之后的任何数据（含垃圾帧）一律不再透传。
		// [DONE] 统一在循环结束后写出，保证恰好一个。
		return 0, true, nil
	case strings.HasPrefix(trimmed, "data: "):
		n, werr := s.writeFrame(strings.TrimPrefix(trimmed, "data: "))
		return n, false, werr
	case trimmed != "":
		// 注释/其他行：原样透传
		if werr := s.writeAll(line); werr != nil {
			return 0, false, werr
		}
	}
	return 0, false, nil
}

// finish 收尾：标记修复尾部回吐 + 空流兜底 error 帧 + 保证恰好一个 [DONE]。
func (s *streamWriter) finish(validFrames *int) error {
	if werr := s.finishRepair(validFrames); werr != nil {
		return werr
	}
	// 空流（0 有效帧）：先写一帧 error（绕过 normalizeFrame 原样保留 error 字段），
	// 再补 [DONE] 保证客户端能正常收尾，并返回非 nil error 供调用方记录。
	// 网关本地空流兜底帧走 hintFn=nil 的直写路径：该形态未覆盖（不编造 hint），
	// 且 writeRaw 的 hintFn 闭包在空流路径下可能携带上一帧的上下文造成误配。
	if *validFrames == 0 {
		_ = s.writeRaw(`{"error":{"message":"empty upstream stream","type":"upstream_error","code":"upstream_parse"}}`)
	}
	// 保证恰好写一个 [DONE]（上游漏发时兜底补上）。
	if err := s.writeAll("data: [DONE]\n\n"); err != nil {
		return err
	}
	if *validFrames == 0 {
		return errEmptyStream
	}
	return nil
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
	fr := ""
	chs, _ := obj["choices"].([]any)
	for _, ci := range chs {
		c, _ := ci.(map[string]any)
		if c == nil {
			continue
		}
		if v, ok := c["finish_reason"].(string); ok && v != "" && fr == "" {
			fr = v
		}
		delete(c, "finish_reason")
	}
	return fr
}

// rewriteFinishReason 把 choice 上「已有且为 stop」的 finish_reason 改写成 want。
// 本来就没有 finish_reason 的中间帧一律不动（绝不凭空造收尾），已有其他值
// （length / content_filter 等）时也不动——那些收尾语义更需要被客户端看到。
// 返回是否发生了改写。
func rewriteFinishReason(obj map[string]any, want string) bool {
	changed := false
	chs, _ := obj["choices"].([]any)
	for _, ci := range chs {
		c, _ := ci.(map[string]any)
		if c == nil {
			continue
		}
		if v, ok := c["finish_reason"].(string); ok && v == "stop" {
			c["finish_reason"] = want
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
