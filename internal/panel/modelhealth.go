// modelhealth.go 面板单模型探测与首字延迟（TTFT）历史（issue #110）。
//
// 定位：给「模型与档位」页一个主动探测入口——对单个模型发一次最小请求
// （短 prompt、小 max_tokens、硬超时），记录首字延迟、输出 token、成功/失败与
// 上游错误分类，写入独立状态文件（与 scripts/probe_max_tokens.py 的只读文件
// 分离）。
//
// 边界：
//   - 探测走 upstream 客户端，**不经过**账号池租约/健康台账/用量记录——探测失败
//     不影响选号与请求路由，也不污染 /v1 用量；
//   - 不做自动全量扫描，不阻塞请求路径；
//   - 同一模型单飞（重复点击 409），全局并发上限 2。
package panel

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/atomicwrite"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

const (
	// modelProbeMaxConcurrent 全局并发探测上限（面板可见，避免一次点击风暴打上游）。
	modelProbeMaxConcurrent = 2
	// modelProbeMaxTokens 探测请求的 max_tokens：只验证「能出首字」，不测长输出。
	modelProbeMaxTokens = 16
	// modelProbeMaxReadBytes 探测读取上限：防异常上游在该请求上无限吐数据
	// （与 SSE 上限同哲学，探测不需要完整回答）。
	modelProbeMaxReadBytes = 256 << 10
	// modelProbeSamplesPerModel 每模型保留的探测样本数（滚动窗口）。
	modelProbeSamplesPerModel = 50
	// modelProbeWindow 健康统计的滚动窗口：最近 N 个样本。
	modelProbeWindow = 20
)

// modelProbeTimeout 单次探测硬超时：短 prompt + 小 max_tokens 的正常首字远小于
// 此值；超时即失败并记录，不让一次探测长期占用 worker。包级 var 仅为测试可注入
// 小值（生产恒 30s，默认不变）。
var modelProbeTimeout = 30 * time.Second

// modelProbeSample 单次探测结果。
type modelProbeSample struct {
	At           time.Time `json:"at"`
	OK           bool      `json:"ok"`
	TTFTMs       int64     `json:"ttft_ms,omitempty"`       // 首个内容帧耗时（首字延迟）
	OutputTokens int64     `json:"output_tokens,omitempty"` // 上游 usage.completion_tokens
	DurationMs   int64     `json:"duration_ms"`
	ErrorClass   string    `json:"error_class,omitempty"` // transport / upstream 分类 / no_account
	Error        string    `json:"error,omitempty"`
}

// modelProbeState 探测历史状态文件（data/model_probes.json）。
type modelProbeState struct {
	Version int                           `json:"version"`
	Samples map[string][]modelProbeSample `json:"samples"` // key = realm:model
}

// modelProbeHealth 单模型的健康聚合（GET /panel/api/model_health）。
type modelProbeHealth struct {
	Last        *modelProbeSample `json:"last,omitempty"`
	Success     int               `json:"success"`
	Failure     int               `json:"failure"`
	AvgTTFTMs   int64             `json:"avg_ttft_ms,omitempty"`
	P50TTFTMs   int64             `json:"p50_ttft_ms,omitempty"`
	P95TTFTMs   int64             `json:"p95_ttft_ms,omitempty"`
	LastProbeAt string            `json:"last_probe_at,omitempty"`
	Window      int               `json:"window"`
}

// modelProbe 主动探测单个模型。
//
//	POST /panel/api/model_probes  {"model":"glm-5.2","realm":"cn"}
//
// 返回本次样本；同一模型+realm 已在探测中返回 409。
func (p *Panel) modelProbe(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model string `json:"model"`
		Realm string `json:"realm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}

	model := trimModelPrefix(req.Model)
	if model == "" {
		writeErr(w, http.StatusBadRequest, "model required")
		return
	}

	realm := req.Realm
	if realm == "" {
		realm = modelRealm(req.Model)
	}

	if realm != "cn" && realm != "global" {
		writeErr(w, http.StatusBadRequest, "realm must be cn or global")
		return
	}

	key := realm + ":" + model
	if !p.tryStartProbe(key) {
		writeErr(w, http.StatusConflict, "该模型正在探测中，请稍后")
		return
	}
	defer p.finishProbe(key)

	if !p.acquireProbeSlot(r.Context()) {
		writeErr(w, http.StatusServiceUnavailable, "并发探测已达上限，请稍后重试")
		return
	}
	defer p.releaseProbeSlot()

	sample := p.runModelProbe(r.Context(), realm, model)
	p.recordProbeSample(key, &sample)
	writeJSON(w, http.StatusOK, map[string]any{"ok": sample.OK, "key": key, "sample": sample})
}

// modelHealth 返回所有已探测模型的滚动健康聚合。
func (p *Panel) modelHealth(w http.ResponseWriter, r *http.Request) {
	state := p.loadProbeState()

	out := make(map[string]modelProbeHealth, len(state.Samples))
	for key, samples := range state.Samples {
		out[key] = summarizeProbeSamples(samples)
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "models": out})
}

// modelRealm 从模型 id 前缀推断 realm（缺省 cn）。
func modelRealm(model string) string {
	if len(model) > 7 && model[:7] == "global:" {
		return "global"
	}

	if len(model) > 3 && model[:3] == "cn:" {
		return "cn"
	}

	return "cn"
}

// trimModelPrefix 去掉 "cn:" / "global:" 前缀（上游只认裸名）。
func trimModelPrefix(model string) string {
	switch {
	case len(model) > 7 && model[:7] == "global:":
		return model[7:]
	case len(model) > 3 && model[:3] == "cn:":
		return model[3:]
	default:
		return model
	}
}

// tryStartProbe 尝试占用某模型的探测槽位（单飞）。
func (p *Panel) tryStartProbe(key string) bool {
	p.probeMu.Lock()
	defer p.probeMu.Unlock()

	if p.probeInflight == nil {
		p.probeInflight = map[string]bool{}
	}

	if p.probeInflight[key] {
		return false
	}

	p.probeInflight[key] = true

	return true
}

// finishProbe 释放模型探测槽位。
func (p *Panel) finishProbe(key string) {
	p.probeMu.Lock()
	delete(p.probeInflight, key)
	p.probeMu.Unlock()
}

// acquireProbeSlot 获取全局并发探测名额；ctx 取消时返回 false。
// 已取消时不能只靠 select：sem 有空位会与 ctx.Done 同时就绪，随机选择仍可能
// 占用名额并空跑一次上游探测（与 acquireSem 同口径）。
func (p *Panel) acquireProbeSlot(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}

	p.probeMu.Lock()
	if p.probeSem == nil {
		p.probeSem = make(chan struct{}, modelProbeMaxConcurrent)
	}

	sem := p.probeSem
	p.probeMu.Unlock()

	select {
	case sem <- struct{}{}:
		// 覆盖「检查后、取名额前」刚取消的窗口；已取消则立即归还名额。
		if ctx.Err() != nil {
			<-sem
			return false
		}

		return true
	case <-ctx.Done():
		return false
	}
}

// releaseProbeSlot 释放全局并发探测名额。
func (p *Panel) releaseProbeSlot() {
	p.probeMu.Lock()
	sem := p.probeSem
	p.probeMu.Unlock()

	if sem != nil {
		<-sem
	}
}

// runModelProbe 执行一次最小探测并返回样本（永不 panic，失败写入 ErrorClass）。
func (p *Panel) runModelProbe(parent context.Context, realm, model string) modelProbeSample {
	start := time.Now()
	sample := modelProbeSample{At: start}

	ctx, cancel := context.WithTimeout(parent, p.probeTimeout)
	defer cancel()

	uids := p.cfg.Pool.AvailableUIDsForRealm(realm)
	if len(uids) == 0 {
		return probeFailure(start, "no_account", "no available account in realm "+realm)
	}

	auth := p.cfg.Pool.AuthByUID(uids[0])
	if auth == nil {
		return probeFailure(start, "no_account", "account not loaded")
	}

	body, err := json.Marshal(probeRequestBody(model))
	if err != nil {
		return probeFailure(start, "internal", err.Error())
	}

	rc, status, respBody, err := p.cfg.Upstream.ChatStreamContext(ctx, auth, body, "", upstream.ChatMeta{})
	if err != nil {
		return probeFailure(start, probeErrorClass(err), err.Error())
	}
	defer rc.Close()

	if status >= http.StatusBadRequest {
		return probeFailure(start, upstream.Classify(status, string(respBody)).String(), truncateProbeError(string(respBody)))
	}

	ttft, tokens, errClass, errMsg := readProbeStream(start, rc)

	sample.DurationMs = time.Since(start).Milliseconds()
	if errClass != "" {
		return probeFailure(start, errClass, errMsg)
	}

	sample.OK = true
	sample.TTFTMs = ttft
	sample.OutputTokens = tokens

	return sample
}

func probeFailure(start time.Time, class, msg string) modelProbeSample {
	sample := modelProbeSample{At: start, ErrorClass: class, Error: msg}
	sample.DurationMs = time.Since(start).Milliseconds()

	return sample
}

// probeChunk 探测流的最小解析形态：error / usage / 首个内容帧。
type probeChunk struct {
	Error *json.RawMessage `json:"error"`
	Usage *struct {
		CompletionTokens *int64 `json:"completion_tokens"`
	} `json:"usage"`
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				ID string `json:"id"`
			} `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
}

// errProbeLineTooLong 探测流单行超过剩余读取上限。
var errProbeLineTooLong = errors.New("probe line exceeds size limit")

// readProbeLine 读一行（含 '\n'），单行累计超过 limit 时立即失败，不继续扩容。
// 语义对齐 bufio.Reader.ReadString：行尾不带 '\n' 时返回该行 + io.EOF。
// 不用 ReadString：无换行超长行会被反复 concat，仅在行后检查挡不住单行溢出
// （与 issue #113 的 SSE 单行上限同哲学）。
func readProbeLine(reader *bufio.Reader, limit int) (string, int, error) {
	var builder strings.Builder

	n := 0

	for {
		chunk, err := reader.ReadSlice('\n')
		if n+len(chunk) > limit {
			return "", n, errProbeLineTooLong
		}

		builder.Write(chunk)

		n += len(chunk)
		if err == nil {
			return builder.String(), n, nil
		}

		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}

		return builder.String(), n, err
	}
}

// readProbeStream 读探测流：首个内容帧耗时 = TTFT；末帧 usage 取输出 token。
// 读上限 modelProbeMaxReadBytes；[DONE] 或 EOF 结束。单行与总量都在累积时
// 检查，异常上游无法靠无换行长行把内存撑到上限之外。
func readProbeStream(start time.Time, rc io.Reader) (ttft, tokens int64, errClass, errMsg string) {
	br := bufio.NewReaderSize(rc, 64*1024)

	read := 0
	for read < modelProbeMaxReadBytes {
		line, n, err := readProbeLine(br, modelProbeMaxReadBytes-read)
		read += n

		if payload, isData := ssePayload(trimLine(line)); isData {
			done, t, tok, ec, em := handleProbeFrame(payload, start, ttft, tokens)
			ttft, tokens = t, tok

			if ec != "" {
				return 0, 0, ec, em
			}

			if done {
				break
			}
		}

		if err != nil {
			break
		}
	}

	if ttft == 0 {
		// 有响应但没等到内容帧：空流/只有注释——按解析失败记录，不伪造首字。
		return 0, tokens, "upstream_parse", "probe stream produced no content frame"
	}

	return ttft, tokens, "", ""
}

// handleProbeFrame 处理一帧 data payload（error 帧 / [DONE] / 内容与 usage）。
func handleProbeFrame(payload string, start time.Time, ttft, tokens int64) (done bool, nTTFT, nTokens int64, errClass, errMsg string) {
	if payload == "[DONE]" {
		return true, ttft, tokens, "", ""
	}

	var chunk probeChunk
	if json.Unmarshal([]byte(payload), &chunk) != nil {
		return false, ttft, tokens, "", "" // 非 JSON 帧：探测忽略
	}

	if chunk.Error != nil {
		return true, 0, 0, upstream.FrameKind(payload).String(), truncateProbeError(payload)
	}

	if ttft == 0 && chunkHasContent(chunk) {
		ttft = sinceMs(start)
	}

	if chunk.Usage != nil && chunk.Usage.CompletionTokens != nil {
		tokens = *chunk.Usage.CompletionTokens
	}

	return false, ttft, tokens, "", ""
}

// chunkHasContent 报告该帧是否携带首字证据：正文、思维链增量或首个工具调用。
// 推理模型可能在小 max_tokens 下只产出 reasoning_content，工具调用型响应则可能
// 只产出 tool_calls；两者都证明上游已开始有效输出，不能记为解析失败。
func chunkHasContent(c probeChunk) bool {
	for _, ch := range c.Choices {
		if ch.Delta.Content != "" || ch.Delta.ReasoningContent != "" || len(ch.Delta.ToolCalls) > 0 {
			return true
		}
	}

	return false
}

// sinceMs 返回自 start 起的毫秒数（下限 1）。
func sinceMs(start time.Time) int64 {
	ms := time.Since(start).Milliseconds()
	if ms < 1 {
		return 1
	}

	return ms
}

// trimLine 去掉行尾 CR/LF。
func trimLine(line string) string {
	for line != "" && (line[len(line)-1] == '\n' || line[len(line)-1] == '\r') {
		line = line[:len(line)-1]
	}

	return line
}

// ssePayload 从 "data: ..." 行取 payload。
func ssePayload(line string) (string, bool) {
	const prefix = "data: "
	if len(line) <= len(prefix) || line[:len(prefix)] != prefix {
		return "", false
	}

	return line[len(prefix):], true
}

// probeErrorClass 把传输层错误归一为可读分类（探测只做观测，不参与路由）。
func probeErrorClass(err error) string {
	if ue, ok := errors.AsType[*upstream.Error](err); ok {
		return ue.Kind.String()
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}

	return "transport"
}

// truncateProbeError 截断错误原文（面板展示用；不写日志、不含凭证）。
func truncateProbeError(str string) string {
	const maxLen = 200
	if len(str) > maxLen {
		return str[:maxLen] + "…"
	}

	return str
}

// recordProbeSample 追加样本并落盘（滚动裁剪到 modelProbeSamplesPerModel）。
func (p *Panel) recordProbeSample(key string, sample *modelProbeSample) {
	// 读-改-写全程持锁：并发探测不同模型时避免后写覆盖先写（丢样本）。
	p.probeStateMu.Lock()
	defer p.probeStateMu.Unlock()

	state := p.loadProbeState()
	samples := state.Samples[key]

	samples = append(samples, *sample)
	if len(samples) > modelProbeSamplesPerModel {
		samples = samples[len(samples)-modelProbeSamplesPerModel:]
	}

	state.Samples[key] = samples
	if err := p.saveProbeState(state); err != nil {
		log.Printf("WARN: [panel] save model probe state failed: %v", err)
	}
}

// loadProbeState 读状态文件（缺失/损坏 → 空状态；损坏只记日志，不让面板空白崩溃）。
func (p *Panel) loadProbeState() *modelProbeState {
	state := &modelProbeState{Version: 1, Samples: map[string][]modelProbeSample{}}
	if p.cfg.ProbeStateFile == "" {
		return state
	}

	raw, err := os.ReadFile(p.cfg.ProbeStateFile)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("WARN: [panel] read model probe state: %v", err)
		}

		return state
	}

	var loaded modelProbeState
	if err := json.Unmarshal(raw, &loaded); err != nil {
		log.Printf("WARN: [panel] parse model probe state: %v", err)
		return state
	}

	if loaded.Samples != nil {
		state.Samples = loaded.Samples
	}

	return state
}

// saveProbeState 原子写回状态文件（tmp + rename，权限 0600）。
func (p *Panel) saveProbeState(state *modelProbeState) error {
	if p.cfg.ProbeStateFile == "" {
		return nil
	}

	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("saveProbeState: %w", err)
	}

	tmp := p.cfg.ProbeStateFile + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("saveProbeState: %w", err)
	}

	if err := atomicwrite.Rename(tmp, p.cfg.ProbeStateFile); err != nil {
		return fmt.Errorf("saveProbeState: %w", err)
	}

	return nil
}

// summarizeProbeSamples 聚合最近 modelProbeWindow 个样本：成功/失败数、avg/p50/p95 TTFT。
func summarizeProbeSamples(samples []modelProbeSample) modelProbeHealth {
	modelProbeHealth2 := modelProbeHealth{Window: modelProbeWindow}
	if len(samples) == 0 {
		return modelProbeHealth2
	}

	last := samples[len(samples)-1]
	modelProbeHealth2.Last = &last
	modelProbeHealth2.LastProbeAt = last.At.Format(time.RFC3339)

	window := samples
	if len(window) > modelProbeWindow {
		window = window[len(window)-modelProbeWindow:]
	}

	var ttfts []int64

	for _, s := range window {
		if s.OK {
			modelProbeHealth2.Success++

			if s.TTFTMs > 0 {
				ttfts = append(ttfts, s.TTFTMs)
			}

			continue
		}

		modelProbeHealth2.Failure++
	}

	if len(ttfts) > 0 {
		slices.Sort(ttfts)

		var sum int64
		for _, v := range ttfts {
			sum += v
		}

		modelProbeHealth2.AvgTTFTMs = sum / int64(len(ttfts))
		modelProbeHealth2.P50TTFTMs = percentile(ttfts, 50)
		modelProbeHealth2.P95TTFTMs = percentile(ttfts, 95)
	}

	return modelProbeHealth2
}

// percentile 取有序切片的 p 分位（最近秩法，p ∈ [0,100]）。
func percentile(sorted []int64, p int) int64 {
	if len(sorted) == 0 {
		return 0
	}
	// 最近秩法：idx = ceil(p·n/100)-1（p95/10 个样本取第 10 个）。
	idx := (p*len(sorted)+99)/100 - 1
	idx = max(idx, 0)
	idx = min(idx, len(sorted)-1)

	return sorted[idx]
}

// probeRequestBody 构造最小探测请求（具体结构体：避免 any 导致 marshal 错误不可检）。
func probeRequestBody(model string) struct {
	Model     string `json:"model"`
	Stream    bool   `json:"stream"`
	MaxTokens int    `json:"max_tokens"`
	Messages  []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
} {
	var body struct {
		Model     string `json:"model"`
		Stream    bool   `json:"stream"`
		MaxTokens int    `json:"max_tokens"`
		Messages  []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}

	body.Model = model
	body.Stream = true
	body.MaxTokens = modelProbeMaxTokens
	body.Messages = []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}{{Role: "user", Content: "ping"}}

	return body
}
