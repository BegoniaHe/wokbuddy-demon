// logging.go 请求级表格日志：每个 /v1/chat/completions 请求结束后打印一行到 stdout。
package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/httpauth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/logfmt"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/reqlog"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// maxUserAgentLen 归档与面板展示保留的 UA 字节上限。UA 是客户端完全可控的
// 自由文本（浏览器动辄 150+ 字符，恶意客户端可以塞几 KB），落盘前必须截断，
// 否则一条请求就能把归档行撑大。截断只影响展示，不影响请求处理。
const maxUserAgentLen = 200

// chatSeq 进程级请求序号。
var chatSeq atomic.Int64

// chatLogEnabled 聊天表格日志总开关。生产恒 true；测试可并发改写，故用 atomic。
// 包级函数表达式完成默认值初始化（不用 init 函数）。
var chatLogEnabled = func() *atomic.Bool {
	enabled := &atomic.Bool{}
	enabled.Store(true)

	return enabled
}()

// chatLogOut 聊天表格日志的输出目标（atomic 持有 *io.Writer）。生产默认 os.Stdout；
// main 在启用管理面板时经 SetChatLogOutput 注入 MultiWriter，把每行镜像进
// /panel/api/logs 的环形缓冲，stdout 行为不变。
//
// 用 atomic.Pointer[io.Writer] 而非 atomic.Value：后者要求每次 Store 的具体类型
// 一致（*os.File 与 io.MultiWriter 的 *multiWriter 不同），混用会 panic。
var (
	chatLogOut     atomic.Pointer[io.Writer]
	chatLogOutOnce sync.Once
)

func ensureChatLogOut() {
	chatLogOutOnce.Do(func() {
		w := io.Writer(os.Stdout)
		chatLogOut.Store(&w)
	})
}

// SetChatLogOutput 替换聊天表格日志输出目标。
func SetChatLogOutput(w io.Writer) {
	if w != nil {
		ensureChatLogOut()
		chatLogOut.Store(&w)
	}
}

// currentChatLogOutput 读取当前输出目标。
func currentChatLogOutput() io.Writer {
	ensureChatLogOut()

	if w := chatLogOut.Load(); w != nil {
		return *w
	}

	return os.Stdout
}

// chatStat 单个 chat 请求的日志统计；handler 挂 defer，请求出口后落一行。
type chatStat struct {
	start     time.Time
	model     string
	mode      string // "stream" | "sync"
	uid       string // 完整 uid，展示时只取前 8 位
	nick      string // 账号昵称（随选号同步），流水行经 logfmt.Label 拼成 "昵称(uid8)"
	ttfb      time.Duration
	toks      int // <0 表示 usage 缺失 → 显示 "-"
	status    int
	requestID string
	outcome   string
	attempts  int
	credit    float64
	hasCredit bool
	// dailyLimited 本次请求因账号每日护栏全池达限而终止（issue #95）。
	dailyLimited     bool
	promptTokens     int64
	completionTokens int64
	totalTokens      int64
	// 缓存命中观测（usage.prompt_cache_hit_tokens / miss）：上游给到才有效。
	// 供 usage 桶命中率维度与 reqlog 逐次记录；缺失时 hasCache=false 不参与统计。
	cacheHit  int64
	cacheMiss int64
	hasCache  bool

	// 调用来源（客户端 IP / User-Agent）。空 = 未采集（logging.request_client_info
	// 关闭，或非 chat 路径），展示层一律以 "-" 兜底。
	clientIP  string
	userAgent string
	// decision 可选选号决策链（logging.decision_trace）；nil = 未启用/未采样。
	decision *reqlog.DecisionTrace

	// toolsDeclared / toolCalls 工具面统计（issue #329）：仅在本请求声明了 tools 时
	// 归档（未声明不进分母）；toolCalls > 0 才写 JSON（omitempty）。
	toolsDeclared int
	toolCalls     int

	out     io.Writer
	enabled bool
	logged  bool
}

// newChatStat 以请求进入 handler 的时刻为起点构造统计对象；toks 默认 -1（usage 缺失）。
// model 由调用方从已解析的入站输入传入，避免对同一请求体重复反序列化。
func newChatStat(now time.Time, model string, stream bool, out io.Writer, enabled bool) *chatStat {
	mode := "sync"
	if stream {
		mode = "stream"
	}

	if model == "" {
		model = "-"
	}

	return &chatStat{start: now, model: model, mode: mode, toks: -1, out: out, enabled: enabled}
}

// done 幂等落一行表格日志。
func (s *chatStat) done() {
	if s.logged {
		return
	}

	s.logged = true
	logChatRowEx(s.out, s.enabled, s.ttfb, time.Since(s.start), s.model, s.mode, s.uid, s.nick, s.status, s.toks,
		s.requestID, s.outcome, s.attempts, s.credit, s.hasCredit, s.clientIP, s.userAgent)
}

// chatStatsReader 在流式透传时抓取 SSE 末帧的 usage.completion_tokens 精确值，
// 并记录首个 data 帧的 TTFB；原始字节原样返回给下游透传。
// 注意：不做 rune 估算，token 数一律采信上游 usage。
type chatStatsReader struct {
	hasContent          bool
	hasToolCall         bool
	hasRefusal          bool
	finishReason        string
	br                  *bufio.Reader
	src                 io.Closer // 底层上游 body（可为 nil）；Close 透传以解除异步读取阻塞
	start               time.Time
	ttfb                time.Duration
	seen                bool // 已见过首个 data 帧（TTFB 只记一次）
	promptTokens        int
	completionTokens    int
	totalTokens         int
	hasPromptTokens     bool
	hasCompletionTokens bool
	hasTotalTokens      bool
	// credit 上游末帧 usage.credit（本次真实扣费积分），供成本台账（NoteModelCost）。
	hasCredit bool
	credit    float64
	// cacheHit/cacheMiss 上游末帧 usage.prompt_cache_hit_tokens / miss_tokens，
	// 供用量桶的命中率维度与 reqlog 逐次记录（issue #92）。
	hasCacheHit  bool
	cacheHit     int
	cacheMiss    int
	hasCacheMiss bool
	// 工具调用去重计数（issue #329）：流式 tool_calls 是分片增量。与 upstream 的
	// toolIndex 同口径：带 index 按 index 去重；缺 index 时 id 绑定到已有调用，新 id
	// 分配新序号；index/id 都缺的后续分片归最近一次调用（单个调用的延续分片无
	// index/id 是标准形态）。旧式 function_call 整块出现，记一条。仅用于归档统计，
	// 不影响透传字节。
	toolCallIndexes    map[int]struct{}
	toolCallIDs        map[string]int
	toolCallHasIndex   bool
	toolCallNextIndex  int
	legacyFunctionCall bool
	pend               string // 已读未返回的行缓存（string 直接 copy 进 dst，省一次转换分配）
}

// chatStatsReader 必须实现 io.Closer：keepalive 流返回前由 upstream 关闭并等待
// 读 goroutine 退出，之后才能安全地把 br 归还池中。
var _ io.Closer = (*chatStatsReader)(nil)

// statsReaderPool 复用 64KB 的 SSE 统计读取缓冲，避免每个流式请求一次大分配。
var statsReaderPool = sync.Pool{
	New: func() any { return bufio.NewReaderSize(nil, 64*1024) },
}

// newChatStatsReaderSince 以 since 为 TTFB 计时起点（通常是请求进入 handler 的时刻）。
func newChatStatsReaderSince(r io.Reader, since time.Time) *chatStatsReader {
	reader, _ := statsReaderPool.Get().(*bufio.Reader)
	if reader == nil {
		reader = bufio.NewReaderSize(nil, 64*1024)
	}

	reader.Reset(r)

	src, _ := r.(io.Closer)

	return &chatStatsReader{br: reader, src: src, start: since}
}

// Close 关闭底层上游 body。streamWithKeepalive 会在返回前关闭包装 reader 以
// 解除异步逐行读取的阻塞，然后等待读 goroutine 退出——否则本对象可能被归还
// 复用缓冲池时仍有 goroutine 在 Read（数据竞争 / nil 解引用）。
func (s *chatStatsReader) Close() error {
	if s == nil || s.src == nil {
		return nil
	}

	return s.src.Close()
}

// release 归还底层缓冲（幂等）。调用方必须保证此后不再读取该 reader。
func (s *chatStatsReader) release() {
	if s == nil || s.br == nil {
		return
	}

	s.br.Reset(nil)
	statsReaderPool.Put(s.br)
	s.br = nil
}

// TTFB 返回首个 data 帧到达耗时；无帧时为 0。
func (s *chatStatsReader) TTFB() time.Duration { return s.ttfb }

// Tokens 返回末帧 usage.completion_tokens 与是否缺失；无 usage 时 ok=false。
func (s *chatStatsReader) Tokens() (int, bool) { return s.completionTokens, s.hasCompletionTokens }

// Credit 返回末帧 usage.credit（本次真实扣费积分）与是否缺失。
func (s *chatStatsReader) Credit() (float64, bool) { return s.credit, s.hasCredit }

// TotalTokens 返回末帧 usage.total_tokens 与是否缺失。
func (s *chatStatsReader) TotalTokens() (int, bool) { return s.totalTokens, s.hasTotalTokens }

// Usage 返回流式响应中已收到的 token usage 字段。
func (s *chatStatsReader) Usage() *pool.TokenUsageDelta {
	return &pool.TokenUsageDelta{
		HasPromptTokens:     s.hasPromptTokens,
		PromptTokens:        int64(s.promptTokens),
		HasCompletionTokens: s.hasCompletionTokens,
		CompletionTokens:    int64(s.completionTokens),
		HasTotalTokens:      s.hasTotalTokens,
		TotalTokens:         int64(s.totalTokens),
	}
}

// sseUsageFrame SSE data 帧的 usage 子结构。字段用指针以区分「缺失」与「零值」。
type sseUsageFrame struct {
	PromptTokens         *int     `json:"prompt_tokens"`
	CompletionTokens     *int     `json:"completion_tokens"`
	TotalTokens          *int     `json:"total_tokens"`
	Credit               *float64 `json:"credit"`
	PromptCacheHitTokens *int     `json:"prompt_cache_hit_tokens"`
	PromptCacheMissTok   *int     `json:"prompt_cache_miss_tokens"`
}

// sseDeltaFrame 流式增量中与「是否产生实质输出」相关的字段。
type sseDeltaFrame struct {
	Content          *string         `json:"content"`
	ReasoningContent *string         `json:"reasoning_content"`
	Reasoning        *string         `json:"reasoning"`
	Refusal          *string         `json:"refusal"`
	ToolCalls        json.RawMessage `json:"tool_calls"`
	FunctionCall     json.RawMessage `json:"function_call"`
}

// sseChoiceFrame 单个 choice 的收尾与增量形态。
type sseChoiceFrame struct {
	Delta        sseDeltaFrame `json:"delta"`
	Message      sseDeltaFrame `json:"message"`
	FinishReason *string       `json:"finish_reason"`
}

// sseChunkFrame SSE data 帧的最小解析形态：usage + 输出健康度。
type sseChunkFrame struct {
	Usage   *sseUsageFrame   `json:"usage"`
	Choices []sseChoiceFrame `json:"choices"`
}

// parseSSELine 解析一行 "data: {...}"：首帧记 TTFB，含 usage 时采信精确 completion_tokens。
//
// 与上游透传侧（upstream.handleStreamLine）同口径：SSE 允许 "data:" 后不带空格，
// 只认 "data: " 会让这类上游的用量与首字延迟整段统计不到。
func (s *chatStatsReader) parseSSELine(line string) {
	payload, ok := sseDataPayload(line)
	if !ok {
		return
	}

	if payload == "[DONE]" {
		return
	}

	if !s.seen {
		s.seen = true
		s.ttfb = time.Since(s.start)
	}

	var chunk sseChunkFrame
	if json.Unmarshal([]byte(payload), &chunk) != nil {
		return
	}

	s.noteChoices(chunk.Choices)

	if chunk.Usage != nil {
		s.applyUsage(chunk.Usage)
	}
}

// noteChoices 记录流式输出健康度：是否有实质输出与最终 finish_reason。
func (s *chatStatsReader) noteChoices(choices []sseChoiceFrame) {
	for i := range choices {
		c := &choices[i]
		s.noteDelta(&c.Delta)
		s.noteDelta(&c.Message)

		if c.FinishReason != nil && *c.FinishReason != "" {
			s.finishReason = *c.FinishReason
		}
	}
}

// noteDelta 合并单个增量/完整消息的实质输出标记。
func (s *chatStatsReader) noteDelta(delta *sseDeltaFrame) {
	if delta.Content != nil && *delta.Content != "" {
		s.hasContent = true
	}

	if delta.Refusal != nil && *delta.Refusal != "" {
		s.hasRefusal = true
	}

	if rawPayloadPresent(delta.ToolCalls) {
		s.hasToolCall = true
		s.noteToolCalls(delta.ToolCalls)
	}
	// 旧式 function_call（非 tool_calls）同样是可消费的实质输出；空占位
	// {"name":"","arguments":""} 与 {} 一样不算（与 upstream.keepFunctionCall 同口径）。
	if functionCallHasPayload(delta.FunctionCall) {
		s.hasToolCall = true
		s.legacyFunctionCall = true
	}
}

// noteToolCalls 对流式 tool_calls 增量做去重计数（issue #329）。
func (s *chatStatsReader) noteToolCalls(raw json.RawMessage) {
	var calls []struct {
		Index *int   `json:"index"`
		ID    string `json:"id"`
	}

	if json.Unmarshal(raw, &calls) != nil {
		return
	}

	for _, call := range calls {
		s.noteToolCall(call.Index, call.ID)
	}
}

// noteToolCall 把单个增量归并到调用序号，规则与 internal/upstream 的 toolIndex 一致：
// 省略 index 的上游按 id 绑定调用，后续无 index/id 的 arguments 分片延续最近一次调用。
func (s *chatStatsReader) noteToolCall(index *int, id string) {
	if index != nil {
		s.markToolCallIndex(*index)

		if id != "" {
			s.bindToolCallID(id, *index)
		}

		return
	}

	if id != "" {
		if idx, ok := s.toolCallIDs[id]; ok {
			s.markToolCallIndex(idx)

			return
		}

		s.bindToolCallID(id, s.nextToolCallIndex())

		return
	}

	// 单个调用的延续分片无 index/id 是标准形态，归入最近一次调用；首个匿名分片
	// 没有可归属的调用时才新开序号。
	if !s.toolCallHasIndex {
		s.nextToolCallIndex()
	}
}

// markToolCallIndex 记录一个调用序号，并把它设为后续匿名分片的归属。
func (s *chatStatsReader) markToolCallIndex(index int) {
	if s.toolCallIndexes == nil {
		s.toolCallIndexes = map[int]struct{}{}
	}

	s.toolCallIndexes[index] = struct{}{}
	s.toolCallHasIndex = true
}

// bindToolCallID 把上游 tool_call id 绑定到调用序号。
func (s *chatStatsReader) bindToolCallID(id string, index int) {
	if s.toolCallIDs == nil {
		s.toolCallIDs = map[string]int{}
	}

	s.toolCallIDs[id] = index
}

// nextToolCallIndex 分配一个未被显式 index 占用的新序号（与 toolIndex.next 同义）。
func (s *chatStatsReader) nextToolCallIndex() int {
	for {
		index := s.toolCallNextIndex
		s.toolCallNextIndex++

		if _, used := s.toolCallIndexes[index]; !used {
			s.markToolCallIndex(index)

			return index
		}
	}
}

// ToolCallCount 本流实际产生的 tool_call 条数（去重后；不含正文标记还原出的调用）。
func (s *chatStatsReader) ToolCallCount() int {
	count := len(s.toolCallIndexes)
	if s.legacyFunctionCall {
		count++
	}

	return count
}

// rawPayloadPresent 报告 JSON 片段是否是有效负载（排除空值/空数组/空对象占位）。
func rawPayloadPresent(raw json.RawMessage) bool {
	value := strings.TrimSpace(string(raw))

	return value != "" && value != "null" && value != "[]" && value != "{}"
}

// functionCallHasPayload 报告旧式 function_call 是否含实际负载：空占位
// {"name":"","arguments":""} 不算一次调用；非对象形态保守按有负载处理。
func functionCallHasPayload(raw json.RawMessage) bool {
	if !rawPayloadPresent(raw) {
		return false
	}

	var fc map[string]any
	if json.Unmarshal(raw, &fc) != nil {
		return true
	}

	return functionCallMapHasPayload(fc)
}

// functionCallMapHasPayload 报告 function_call 对象的 name/arguments 是否至少一项非空。
func functionCallMapHasPayload(fc map[string]any) bool {
	name, _ := fc["name"].(string)
	args, _ := fc["arguments"].(string)

	return name != "" || args != ""
}

// HasSubstantiveOutput 报告流中是否产生正文、工具调用或拒答。
// 仅有 reasoning/reasoning_content 的流不算完成的回答。
func (s *chatStatsReader) HasSubstantiveOutput() bool {
	return s.hasContent || s.hasToolCall || s.hasRefusal
}

// FailedFinish 报告流是否以下游无法消费的 finish_reason 结束且没有实质输出。
func (s *chatStatsReader) FailedFinish() bool {
	return !s.HasSubstantiveOutput() && (s.finishReason == "error" || s.finishReason == "length")
}

// FinishReason 返回最近一个非空 finish_reason（空 = 未观测到）。
func (s *chatStatsReader) FinishReason() string { return s.finishReason }

// applyUsage 把末帧 usage 的非空字段写入统计（缺失字段不动，保留上一帧值）。
func (s *chatStatsReader) applyUsage(sseUsageFrame2 *sseUsageFrame) {
	if sseUsageFrame2.PromptTokens != nil {
		s.hasPromptTokens = true
		s.promptTokens = *sseUsageFrame2.PromptTokens
	}

	if sseUsageFrame2.CompletionTokens != nil {
		s.hasCompletionTokens = true
		s.completionTokens = *sseUsageFrame2.CompletionTokens
	}

	if sseUsageFrame2.TotalTokens != nil {
		s.hasTotalTokens = true
		s.totalTokens = *sseUsageFrame2.TotalTokens
	}

	if sseUsageFrame2.Credit != nil {
		s.hasCredit = true
		s.credit = *sseUsageFrame2.Credit
	}

	if sseUsageFrame2.PromptCacheHitTokens != nil {
		s.hasCacheHit = true
		s.cacheHit = *sseUsageFrame2.PromptCacheHitTokens
	}

	if sseUsageFrame2.PromptCacheMissTok != nil {
		s.hasCacheMiss = true
		s.cacheMiss = *sseUsageFrame2.PromptCacheMissTok
	}
}

// cacheTokenTriple 缓存三维观测（prompt / hit / miss）与各自的「是否有值」标记。
// 流式从末帧 SSE usage 填，非流式从聚合响应 usage 填，之后共用同一套推导。
type cacheTokenTriple struct {
	prompt, hit, miss          int64
	hasPrompt, hasHit, hasMiss bool
}

// cacheTokensFromUsage 由三维观测推导 (hit, miss)：两侧都缺失 → ok=false
// （不参与命中率统计，不臆造）；其余按 resolveCacheMiss / resolveCacheHit 的口径。
func cacheTokensFromUsage(t cacheTokenTriple) (hit, miss int64, ok bool) {
	if !t.hasHit && !t.hasMiss {
		return 0, 0, false
	}

	return resolveCacheHit(t), resolveCacheMiss(t), true
}

// resolveCacheMiss 未命中 token：上游显式值优先，缺失时按 prompt - hit 推导。
func resolveCacheMiss(t cacheTokenTriple) int64 {
	if t.hasMiss {
		return t.miss
	}

	if t.hasPrompt && t.prompt >= t.hit {
		return t.prompt - t.hit
	}

	return 0
}

// resolveCacheHit 命中 token：显式值优先；显式值为 0 或缺失、且上游给了**正的**
// 显式 miss 时按 prompt - miss **对称反推**——上游「接近全命中」时可能只回显式
// miss、把 hit 置 0 或留空，单向推导会把它误判成缓存完全失效（假阳性 WARN）。
//
// 反推的两个前提缺一不可：
//   - miss 必须是显式且 > 0：两侧都显式给 0（或无 miss 只有 prompt）时没有任何
//     「命中」证据，反推会凭空造出 100% 命中（与「不臆造」的设计目标相反）；
//   - prompt > miss：自造命中的代价比「不可得」更大，保持 0。
func resolveCacheHit(t cacheTokenTriple) int64 {
	if t.hasHit && t.hit > 0 {
		return t.hit
	}

	if t.hasMiss && t.miss > 0 && t.hasPrompt && t.prompt > t.miss {
		return t.prompt - t.miss
	}

	return 0
}

// CacheTokens 返回末帧 usage 的缓存命中 / 未命中 token 数，推导口径见
// cacheTokensFromUsage。
func (s *chatStatsReader) CacheTokens() (hit, miss int64, ok bool) {
	return cacheTokensFromUsage(cacheTokenTriple{
		prompt:    int64(s.promptTokens),
		hit:       int64(s.cacheHit),
		miss:      int64(s.cacheMiss),
		hasPrompt: s.hasPromptTokens,
		hasHit:    s.hasCacheHit,
		hasMiss:   s.hasCacheMiss,
	})
}

// Read 返回原始数据，同时解析统计 TTFB/token。
// 切行与单行上限统一走 upstream.ReadSSELine：与透传侧同一份实现（同一
// MaxSSELineBytes / ErrSSELineTooLong，issue #113）。统计层自己也是按行读取点，
// 不设限会把超长行原样拷贝给下游透传。
func (s *chatStatsReader) Read(nums []byte) (int, error) {
	if s.pend != "" {
		return s.take(nums), nil
	}

	line, err := upstream.ReadSSELine(s.br)
	if err != nil && !errors.Is(err, io.EOF) {
		// 超长行 / 读错误：上抛，不拷贝部分行。
		return 0, fmt.Errorf("stats sse line: %w", err)
	}

	if line != "" {
		s.parseSSELine(line)
		s.pend = line

		return s.take(nums), nil
	}

	if err == nil {
		return 0, nil
	}
	// 结尾无剩余数据：保持 io.EOF 原样，上层流循环据此判定正常结束。
	return 0, io.EOF
}

// take 把已读未返回的行拷进 dst 并推进偏移，返回本次交付字节数。
//
// pend 保持 string：copy([]byte, string) 是内建允许的方向，不需要先转 []byte，
// 每帧省一次整行转换分配 + 一次额外 memcpy（流式请求每帧都会走这里）。
func (s *chatStatsReader) take(dst []byte) int {
	n := copy(dst, s.pend)
	s.pend = s.pend[n:]

	return n
}

// rewriteModel 把 outbound chat body 的 model 字段替换为 bare（保留其余字段原样）。
// 仅当 bare != 原 model 时由 chatCompletions 调用；body 不可解析时原样返回（不二次错误化）。
func rewriteModel(body []byte, bare string) []byte {
	if len(body) == 0 || bare == "" {
		return body
	}

	// UseNumber：seed / 大整数 id 这类字段按字面量保留。走 float64 会把
	// 9007199254740993 变成 ...992，出图 seed 就悄悄变了。
	var obj map[string]any

	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()

	if err := dec.Decode(&obj); err != nil {
		return body
	}

	if cur, ok := obj["model"].(string); !ok || cur == bare {
		return body
	}

	obj["model"] = bare

	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}

	return out
}

// usageDeltaFromResponse 从非流式聚合响应中提取明确存在的 token 字段。
func usageDeltaFromResponse(resp map[string]any) *pool.TokenUsageDelta {
	delta := &pool.TokenUsageDelta{}

	table, ok := resp["usage"].(map[string]any)
	if !ok {
		return delta
	}

	if n, ok := readUsageInt(table, "prompt_tokens"); ok {
		delta.HasPromptTokens, delta.PromptTokens = true, n
	}

	if n, ok := readUsageInt(table, "completion_tokens"); ok {
		delta.HasCompletionTokens, delta.CompletionTokens = true, n
	}

	if n, ok := readUsageInt(table, "total_tokens"); ok {
		delta.HasTotalTokens, delta.TotalTokens = true, n
	}

	return delta
}

// readUsageInt 从 usage map 取一个整数字段，兼容 float/整型/json.Number 形态。
func readUsageInt(u map[string]any, key string) (int64, bool) {
	v, ok := u[key]
	if !ok {
		return 0, false
	}

	switch n := v.(type) {
	case float64:
		return int64(n), true
	case float32:
		return int64(n), true
	case int:
		return int64(n), true
	case int64:
		return n, true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	default:
		return 0, false
	}
}

// completionTokens 从 Aggregate 返回的响应中提取 usage.completion_tokens；缺失返回 -1。
func completionTokens(resp map[string]any) int {
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return -1
	}

	v, ok := u["completion_tokens"].(float64)
	if !ok {
		return -1
	}

	return int(v)
}

type requestTraceKey struct{}

// requestTrace 在一次 chat 请求内共享标识与最终统计，ServeHTTP 出口统一记账。
type requestTrace struct {
	id    string
	path  string
	start time.Time
	stat  *chatStat
	// stats 本次请求内每轮 chat 统计（本地 web 工具循环会多次调用 serveChat）。
	// stat 保留最后一轮（状态码回落用），stats 供归档合并为请求级汇总（issue #419）。
	stats []*chatStat
	// 调用来源，进入 handler 时一次性采集（见 ServeHTTP / captureClientInfo）。
	clientIP  string
	userAgent string
	// keyID 命中的命名密钥 ID；默认密钥 / 未鉴权为空。由 withClientAuth 回填。
	keyID string
	// account/model/outcome 非 chat 路径（搜索等）显式写入的记账元数据：
	// 那些路径没有 chatStat，但归档仍要能回答「谁、哪个模型、成没成」。
	account string
	model   string
	outcome string
	// fallbackStatus 处理器未写响应体时用于归档的状态（如调用方断开 499）。
	// 优先级高于 stat：本地工具循环中断时最后一轮 chatStat 仍是成功状态，
	// 不能用它冒充最终结果。
	fallbackStatus int
	// forcedOutcome 显式覆盖归档结果（中断等没有 chatStat 可表达的终态）。
	// 与 outcome 分开：outcome 是搜索等非 chat 路径的记账元数据，/v1/messages
	// 的 web_search 前置会先写它，随后真正的 chat 结果（含失败）不能被它盖掉。
	forcedOutcome string
}

// noteChatStat 登记一轮 chat 统计：stat 保留最后一轮，stats 逐轮累积。
// 归档在 event() 里按请求合并（本地 web 工具循环多轮，issue #419）。
func (t *requestTrace) noteChatStat(stat *chatStat) {
	if t == nil || stat == nil {
		return
	}

	t.stat = stat
	t.stats = append(t.stats, stat)
}

// chatStatSummary 返回归档用的统计：单轮直接复用，多轮合并为请求级汇总。
//
// 合并口径（issue #419）：attempts / tokens / credit / 缓存 / 工具调用数逐轮累加，
// 与命名密钥用量（recordKeyUsage 累加）保持一致；ttfb、toolsDeclared、decision
// 取首轮（请求级属性）；model / uid / nick / outcome / status 取最后一轮，
// 反映最终对外表现。
func (t *requestTrace) chatStatSummary() *chatStat {
	switch len(t.stats) {
	case 0:
		return t.stat
	case 1:
		return t.stats[0]
	}

	merged := *t.stats[0]
	merged.attempts = 0
	merged.promptTokens, merged.completionTokens, merged.totalTokens = 0, 0, 0
	merged.credit, merged.hasCredit = 0, false
	merged.cacheHit, merged.cacheMiss, merged.hasCache = 0, 0, false
	merged.toolCalls = 0
	merged.dailyLimited = false
	merged.toks = -1

	for _, roundStat := range t.stats {
		merged.attempts += roundStat.attempts
		merged.promptTokens += roundStat.promptTokens
		merged.completionTokens += roundStat.completionTokens
		merged.totalTokens += roundStat.totalTokens
		merged.credit += roundStat.credit
		merged.hasCredit = merged.hasCredit || roundStat.hasCredit
		merged.cacheHit += roundStat.cacheHit
		merged.cacheMiss += roundStat.cacheMiss
		merged.hasCache = merged.hasCache || roundStat.hasCache
		merged.toolCalls += roundStat.toolCalls
		merged.dailyLimited = merged.dailyLimited || roundStat.dailyLimited

		if roundStat.toks >= 0 {
			if merged.toks < 0 {
				merged.toks = 0
			}

			merged.toks += roundStat.toks
		}
	}

	last := t.stats[len(t.stats)-1]
	merged.model, merged.uid, merged.nick = last.model, last.uid, last.nick
	merged.outcome, merged.status = last.outcome, last.status

	return &merged
}

// noteResult 记录非 chat 路径的账号/模型/结果（搜索端点调用；nil 安全）。
func (t *requestTrace) noteResult(account, model, outcome string) {
	if t == nil {
		return
	}

	t.account, t.model, t.outcome = account, model, outcome
}

// noteForcedOutcome 记录必须盖过底层统计的请求级终态（如本地工具循环中断），
// 保留 chat 统计里已有的账号/模型（nil 安全）。
func (t *requestTrace) noteForcedOutcome(outcome string) {
	if t == nil {
		return
	}

	t.forcedOutcome = outcome
}

// captureClientInfo 采集调用来源（客户端 IP + 截断后的 UA）。开关关闭时保持空串：
// 来源信息比 token 计数敏感，是否落盘由 logging.request_client_info 决定。
func (t *requestTrace) captureClientInfo(r *http.Request, trusted []*net.IPNet) {
	if t == nil || r == nil {
		return
	}

	t.clientIP = clientIPForLog(r, trusted)
	t.userAgent = logfmt.Truncate(r.UserAgent(), maxUserAgentLen)
}

// clientIPForLog 提取用于日志展示的客户端 IP。
//
// 与透传上游同一口径（httpauth.ClientIP）：只有 TCP 对端属于配置的可信反代
// 网段时才采信转发头，否则一律用 RemoteAddr——否则任何客户端都能用伪造的
// X-Forwarded-For 污染请求日志与审计记录。对端地址解析失败时保持空串
// （面板显示 "-"）。
func clientIPForLog(r *http.Request, trusted []*net.IPNet) string {
	return httpauth.ClientIP(r, trusted)
}

// dashIfEmpty 空串统一显示 "-"（来源字段未采集时不留空白列）。
func dashIfEmpty(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}

	return s
}

func requestTraceFrom(r *http.Request) *requestTrace {
	if r == nil {
		return nil
	}

	tr, _ := r.Context().Value(requestTraceKey{}).(*requestTrace)

	return tr
}

// finalEvent 定性最终状态：调用方已断开（499）等「不写响应体」的路径没有 header
// 可捕获，回落到处理器显式状态（fallbackStatus），再回落 chat 统计，避免记成 200。
func (t *requestTrace) finalEvent(observed int) reqlog.Event {
	status := observed
	if status == 0 && t.fallbackStatus != 0 {
		status = t.fallbackStatus
	}

	if status == 0 && t.stat != nil {
		status = t.stat.status
	}

	if status == 0 {
		status = http.StatusOK
	}

	return t.event(status)
}

func (t *requestTrace) event(status int) reqlog.Event {
	event := reqlog.Event{
		Time:      t.start,
		RequestID: t.id,
		Path:      t.path,
		Status:    status,
	}
	duration := time.Since(t.start)
	event.DurationMs = max(duration.Milliseconds(), 1)

	stat := t.chatStatSummary()
	if stat == nil {
		// 非 chat 路径（搜索）：用 handler 写入的元数据，保持与 chat 行同字段口径。
		event.Account, event.Model = t.account, t.model

		if t.outcome != "" {
			event.Outcome = t.outcome
		}
	} else {
		applyChatStat(&event, stat)
	}
	// 只有请求级终态（中断）盖过底层统计：/v1/messages 的 web_search 前置也会写
	// outcome，但它不代表随后 chat 的成败。
	if t.forcedOutcome != "" {
		event.Outcome = t.forcedOutcome
	}

	event.KeyID = t.keyID
	event.ClientIP = t.clientIP

	event.UserAgent = t.userAgent
	if stat != nil {
		event.Decision = stat.decision
	}

	if event.Outcome == "" {
		if status >= 200 && status < 300 {
			event.Outcome = reqlog.OutcomeSuccess
		} else {
			event.Outcome = reqlog.OutcomeHTTPError
		}
	}

	event.OK = status >= 200 && status < 300 && event.Outcome == reqlog.OutcomeSuccess

	return event
}

// applyChatStat 把 chat 统计拷进归档事件（chat 行与搜索行同字段口径）。
func applyChatStat(event *reqlog.Event, chatStat2 *chatStat) {
	event.Account = logfmt.Label(chatStat2.uid, chatStat2.nick)
	event.Model = chatStat2.model
	event.Outcome = chatStat2.outcome
	event.TTFBMs = chatStat2.ttfb.Milliseconds()
	event.Attempts = chatStat2.attempts
	event.PromptTokens = chatStat2.promptTokens
	event.CompletionTokens = chatStat2.completionTokens
	event.TotalTokens = chatStat2.totalTokens
	event.Credit = chatStat2.credit
	event.HasCredit = chatStat2.hasCredit
	event.CacheHitTokens = chatStat2.cacheHit
	event.CacheMissTokens = chatStat2.cacheMiss
	// 工具面：只有声明了 tools 的请求才写，未声明保持零值 + omitempty（不进分母）。
	if chatStat2.toolsDeclared > 0 {
		event.ToolsDeclared = chatStat2.toolsDeclared
		event.ToolCalls = chatStat2.toolCalls
	}
}

// responseObserver 捕获 handler 实际写出的 HTTP 状态，同时保留 Flusher/Unwrap，
// 避免破坏 SSE 逐帧刷新。
type responseObserver struct {
	http.ResponseWriter

	status int
}

func (o *responseObserver) WriteHeader(code int) {
	if o.status == 0 {
		o.status = code
	}

	o.ResponseWriter.WriteHeader(code)
}

func (o *responseObserver) Write(p []byte) (int, error) {
	if o.status == 0 {
		o.status = http.StatusOK
	}

	return o.ResponseWriter.Write(p)
}

// WriteString 同 recoveryWriter：不显式转发就不能提升底层的 io.StringWriter，
// SSE 每帧的载荷会被 io.WriteString 退化成一整份 []byte 拷贝。
func (o *responseObserver) WriteString(data string) (int, error) {
	if o.status == 0 {
		o.status = http.StatusOK
	}

	return io.WriteString(o.ResponseWriter, data) //nolint:wrapcheck // io.Writer 实现原样透传错误
}

func (o *responseObserver) Flush() {
	if f, ok := o.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (o *responseObserver) Unwrap() http.ResponseWriter { return o.ResponseWriter }

// 请求流水行的固定列宽（显示列宽，非字节）。取固定宽度而不是让内容自然长度撑开，
// 是为了让 stdout 里成百上千行能竖着扫——否则模型名长短不一、中文昵称按字节补空格
// 错位，根本没法用肉眼对齐着一列列看（这正是上一版 11 字节硬截断要解决的问题）。
const (
	// chatModelWidth 覆盖 realm 前缀 + 最长模型名："global:" (7) + "deepseek-v4.1-flash" (19) = 26。
	// 旧的 11 字节截断会把 "cn:deepseek-v4-flash" 切成 "cn:deepseek"，让人误以为是另一个模型。
	chatModelWidth = 26
	// chatAcctWidth 容纳 "昵称(uid8)"：中文昵称按 2 列/字算，5 字中文 + "(xxxxxxxx)" = 20 列。
	chatAcctWidth = 22
	chatTTFBWidth = 8
	chatTokWidth  = 6
	chatRateWidth = 11 // 形如 "183.6tok/s"
)

// logChatRowEx 是带请求 ID、结果、重试、积分与调用来源字段的扩展流水行。旧调用保持
// 原格式；requestID 非空时才追加扩展字段；来源两参均为空时不追加来源段。
//
//nolint:revive // 日志字段按协议顺序平铺，改 struct 会增加调用点构造负担。
func logChatRowEx(
	out io.Writer, enabled bool, ttfb, total time.Duration, model, mode, uid, nick string, status, toks int,
	requestID, outcome string, attempts int, credit float64, hasCredit bool,
	clientIP, userAgent string,
) {
	if !enabled {
		return
	}

	seq := chatSeq.Add(1)
	model = logfmt.Pad(logfmt.Truncate(model, chatModelWidth), chatModelWidth)
	// 账号标签只补不截：超宽时宁可让该行变宽，也不丢昵称信息（昵称是排查的主线索）。
	acct := logfmt.Pad(logfmt.Label(uid, nick), chatAcctWidth)
	tokField, tokpsField := tokFields(toks, total, ttfb)

	ttfbMS := "-"
	if ttfb > 0 {
		ttfbMS = fmt.Sprintf("%dms", ttfb.Milliseconds())
	}

	extra := extraFields(requestID, outcome, status, attempts, credit, hasCredit)
	src := sourceFields(clientIP, userAgent)
	fmt.Fprintf(out, "| #%03d | %s | %s | %s | %d | %s | TTFB=%s | tok=%s | %s | total=%.1fs |%s%s\n",
		seq,
		time.Now().Format("15:04:05"),
		model,
		mode,
		status,
		acct,
		logfmt.Pad(ttfbMS, chatTTFBWidth),
		logfmt.Pad(tokField, chatTokWidth),
		logfmt.Pad(tokpsField, chatRateWidth),
		total.Seconds(),
		extra,
		src,
	)
}

// tokFields 计算 tok 与 tok/s 两列（toks<0 = usage 缺失，两列均显示 "-"）。
// 速率与用量账本走同一个函数（扣掉 TTFB），避免流水行与面板数字口径漂移（issue #34）。
func tokFields(toks int, total, ttfb time.Duration) (tokField, tokpsField string) {
	if toks < 0 {
		return "-", "-"
	}

	tokField = strconv.Itoa(toks)
	if tps, ok := tokensPerSecond(int64(toks), total, ttfb); ok {
		return tokField, fmt.Sprintf("%.1ftok/s", tps)
	}

	return tokField, "0.0tok/s"
}

// extraFields 组装请求 ID / 结果 / 重试 / 积分扩展段；requestID 为空时返回空串
// （旧行格式保持不变）。outcome 为空时按 status 兜底推断。
func extraFields(requestID, outcome string, status, attempts int, credit float64, hasCredit bool) string {
	if requestID == "" {
		return ""
	}

	if outcome == "" {
		outcome = reqlog.OutcomeHTTPError
		if status >= 200 && status < 300 {
			outcome = reqlog.OutcomeSuccess
		}
	}

	creditField := "-"
	if hasCredit {
		creditField = fmt.Sprintf("%.4f", credit)
	}

	return fmt.Sprintf(" rid=%s | out=%s | try=%d | credit=%s |", requestID, outcome, attempts, creditField)
}

// sourceFields 组装调用来源段：IP 用可解析的裸值（便于 grep），UA 用 ShortUA 压缩后
// 的客户端标签并加引号（标签内可能含空格，如 `OpenAI/Python 1.30.0` 只取 OpenAI/Python）。
// 两者都未采集时返回空串，旧行格式保持不变。
func sourceFields(clientIP, userAgent string) string {
	if clientIP == "" && userAgent == "" {
		return ""
	}

	ua := "-"
	if s := logfmt.ShortUA(userAgent); s != "" {
		ua = `"` + s + `"`
	}

	return fmt.Sprintf(" src=%s ua=%s |", dashIfEmpty(clientIP), ua)
}

// cacheMissSignal 低命中率告警信号（issue #92 P1 轻量化）：同一模型此前观测到过
// 命中、本次大前缀（≥1000 tok）整段未命中（命中率 <10%）时打一条 WARN，每模型
// 10 分钟冷却。只报「本可命中却重算」的可行动信号——新模型/新会话的首请求天然
// 全 miss，不在此列。
type cacheMissSignal struct {
	mu      sync.Mutex
	every   time.Duration
	everHit map[string]bool
	last    map[string]time.Time
}

// newCacheMissSignal 构造告警信号；every 为同模型的 WARN 冷却间隔。
func newCacheMissSignal(every time.Duration) *cacheMissSignal {
	return &cacheMissSignal{
		every:   every,
		everHit: map[string]bool{},
		last:    map[string]time.Time{},
	}
}

var cacheMissWarn = newCacheMissSignal(10 * time.Minute)

// noteCacheTokens 记录一次观测：有命中 → 标记该模型可命中；整段未命中且此前
// 命中过 → 触发 WARN（冷却内不重复）。
//
// 入参是已推导后的三维值（见 cacheTokensFromUsage）：上游未回 miss 时调用方已按
// prompt 反推为未命中，所以这里 miss<=0 只可能是「上游显式回 0」。两侧都显式 0 时
// 没有任何命中/未命中证据，不能按整段重算告警（否则对称反推修完仍会刷假阳性）。
func (c *cacheMissSignal) noteCacheTokens(model string, prompt, hit, miss int64) {
	if model == "" {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if hit > 0 {
		c.everHit[model] = true
		return
	}
	// hit==0 才可能是整段重算。小前缀（<1000 tok）无告警价值；miss<=0 是显式 0
	// （无证据），同样跳过。
	if prompt < 1000 || miss <= 0 {
		return
	}

	if !c.everHit[model] {
		return
	}

	now := time.Now()
	if t, ok := c.last[model]; ok && now.Sub(t) < c.every {
		return
	}

	c.last[model] = now
	log.Printf("WARN: [server] cache miss: model=%s 本次大前缀未命中（prompt=%d miss=%d，此前观测到过命中）；"+
		"上游前缀缓存重算，费用会显著升高", model, prompt, miss)
}
