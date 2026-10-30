// probe.go 首帧否决窗口：在向客户端写出任何字节前，预览上游流的前若干个事件，
// 判断这一跳是否值得重试（首帧 error / 顶层业务错误 / 零输出 length）。
//
// 背景（issue #244）：上游 200 开流后第一帧就是 error 时，handler 已经提交响应头，
// 无法换号；零输出 + finish_reason=length 也会把「思考吃光预算」误判成账号失败。
// ProbeStream 只读取到出现实质输出（正文/工具调用/拒答）为止，把已读字节原样交给
// 后续透传，未出现实质输出时交回结构化失败原因。
package upstream

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
)

// StreamProbe 预览结果。
type StreamProbe struct {
	// Kind 首帧 error/顶层业务错误分类；ErrNone 表示可以继续透传。
	Kind ErrKind
	// Frame 触发失败的原始帧 payload（error/业务信封）。
	Frame string
	// ZeroOutputLength 上游 finish_reason=length 且没有正文/工具/拒答。
	ZeroOutputLength bool
	// Empty 流在出现实质输出前结束（空流/只有思考/只有 [DONE]）。
	Empty bool
	// FinishReason 预览到的非空 finish_reason（零输出判定用）。
	FinishReason string
	// Text 预览阶段累计的正文（续写起点）。
	Text string
	// HasTool 预览阶段是否出现工具调用（有工具时不做正文续写）。
	HasTool bool
	// SizeLimited 预览阶段撞上单行上限（MaxSSELineBytes，issue #113）：已读字节
	// 不再回放，由 handler 按 upstream_parse 收尾并换号。与透传层同一哨兵语义。
	SizeLimited bool
	// passThrough 已出现合法终态（如 content_filter）但无正文：直接透传，
	// 不按账号故障轮转。
	passThrough bool

	buffered []byte
	reader   *bufio.Reader
}

// Reader 返回「已预览字节 + 上游剩余流」的读取器。
// Release 之后上游剩余流不再可达，只剩已预览字节。
func (p *StreamProbe) Reader() io.Reader {
	if p.reader == nil {
		return bytes.NewReader(p.buffered)
	}

	return io.MultiReader(bytes.NewReader(p.buffered), p.reader)
}

// ReadCloser 返回可关闭的「已预览字节 + 上游剩余流」。
func (p *StreamProbe) ReadCloser(closer io.Closer) io.ReadCloser {
	return &probedReadCloser{Reader: p.Reader(), closer: closer}
}

// probedReadCloser 组合预览流与上游 body 的 Close。
type probedReadCloser struct {
	io.Reader

	closer io.Closer
}

// Close 关闭上游 body（nil 安全）。
func (p *probedReadCloser) Close() error {
	if p.closer == nil {
		return nil
	}

	return p.closer.Close()
}

// probeMaxBytes 预览缓冲上限：超过后不再尝试否决，直接放行（避免为异常长流
// 无界缓冲）。正常首帧/首个正文远小于该值。
const probeMaxBytes = 256 * 1024

// probeReaderSize 预览行缓冲大小：4KB（不是 sseReaderPool 那档 64KB）。
const probeReaderSize = 4 * 1024

// probeReaderPool 预览行缓冲的复用池：每请求一条、随请求存活整条流，
// 不池化就是每请求一次 4KB 垃圾（流式 B/op 里除 JSON 解码外最大的一笔）。
//
// 归还点不能放在 probedReadCloser.Close：keepalive 路径会先 Close 底层流解阻塞、
// 再等读 goroutine 退出（见 streamWithKeepalive），此刻 reader 可能正被读。
// 调用方必须在流结束后调 Release。
var probeReaderPool = sync.Pool{
	New: func() any { return bufio.NewReaderSize(nil, probeReaderSize) },
}

// getProbeReader 取一条预览行缓冲（池未命中时新建）。
func getProbeReader() *bufio.Reader {
	reader, _ := probeReaderPool.Get().(*bufio.Reader)
	if reader == nil {
		return bufio.NewReaderSize(nil, probeReaderSize)
	}

	return reader
}

// Release 归还预览行缓冲（幂等、nil 安全）。只能在整个流确实读完之后调用。
func (p *StreamProbe) Release() {
	if p == nil || p.reader == nil {
		return
	}

	p.reader.Reset(nil)
	probeReaderPool.Put(p.reader)

	p.reader = nil
}

// ProbeStream 预览上游 SSE 流。返回的 StreamProbe 总可安全调用 Reader。
//
// 行缓冲 4KB 而不是 64KB：预览阶段只需读到首个实质输出（通常一两帧），而预览
// 结束后这条 reader 只做透传——下游 stats/SSE 都以 ≥4KB 的缓冲读它，bufio 在
// 「缓冲空 + 请求长度 ≥ 缓冲大小」时直接读到目标切片，所以 64KB 缓冲在这里既
// 不会被填满（纯白占内存），也不省 syscall。缓冲本身跨请求复用（probeReaderPool），
// 调用方在流结束后必须 Release。
// 超长单行（>4KB）由 ReadSSELine 的分片收集处理，超过 MaxSSELineBytes 即失败。
func ProbeStream(r io.Reader) *StreamProbe {
	reader := getProbeReader()
	reader.Reset(r)

	probe := &StreamProbe{reader: reader}

	for len(probe.buffered) < probeMaxBytes {
		// 用 ReadSSELine 而不是 ReadString：单行超过 MaxSSELineBytes 时立即失败，
		// 不再无上限拼接——预览窗口的总量上限是「读完一行之后」才判的，挡不住
		// 单行把内存撑到任意大小（issue #113 的同一类风险）。
		line, err := ReadSSELine(probe.reader)
		if errors.Is(err, ErrSSELineTooLong) {
			// 部分行已从 bufio 消费掉：不能回放（会把半行拼给客户端），
			// 直接交给 handler 按上游失败收尾。
			probe.SizeLimited = true

			return probe
		}

		probe.buffered = append(probe.buffered, line...)
		probe.inspectLine(line)

		if probe.Kind != ErrNone || probe.ZeroOutputLength || probe.Empty ||
			probe.passThrough || probe.hasSubstantive() {
			return probe
		}

		if err != nil {
			if !probe.hasSubstantive() {
				probe.Empty = true
			}

			return probe
		}
	}

	return probe
}

// inspectLine 解析一行：出现实质输出即视为可放行；error/业务错误/零输出终止预览。
func (p *StreamProbe) inspectLine(line string) {
	payload, ok := sseDataPayload(strings.TrimRight(line, "\r\n"))
	if !ok {
		return
	}

	if sseDone(payload) {
		if !p.hasSubstantive() {
			p.Empty = true
		}

		return
	}

	var frame map[string]any
	if json.Unmarshal([]byte(payload), &frame) != nil {
		return
	}

	if _, hasErr := frame["error"]; hasErr {
		p.Kind = FrameKind(payload)
		if p.Kind == ErrNone {
			p.Kind = ErrServer
		}

		p.Frame = payload

		return
	}

	if _, _, isErr := frameBusinessError(frame); isErr {
		p.Kind = FrameKind(payload)
		if p.Kind == ErrNone {
			p.Kind = ErrClient
		}

		p.Frame = payload

		return
	}

	p.inspectChoices(frame)
}

// inspectChoices 累计正文/工具调用并读取 finish_reason。
func (p *StreamProbe) inspectChoices(frame map[string]any) {
	walkChoiceDeltas(frame, func(fr string) {
		if fr != "" {
			p.FinishReason = fr
		}
	}, p.inspectDelta)

	if p.hasSubstantive() {
		return
	}

	switch p.FinishReason {
	case "length":
		p.ZeroOutputLength = true
	case "content_filter":
		// 内容过滤是上游合法终态，不是账号故障；原样把终态帧交给客户端。
		p.passThrough = true
	case "", "stop":
	default:
		p.Kind = ErrServer
	}
}

// inspectDelta 记录正文/拒答/工具调用（工具调用无论参数是否完整都算实质输出）。
func (p *StreamProbe) inspectDelta(delta map[string]any) {
	if text, ok := delta["content"].(string); ok && text != "" {
		p.Text += text
	}

	if refusal, ok := delta["refusal"].(string); ok && refusal != "" {
		p.Text += refusal
	}

	if tools, ok := delta["tool_calls"].([]any); ok && len(tools) > 0 {
		p.HasTool = true
	}
}

// hasSubstantive 报告预览阶段是否已出现实质输出。
func (p *StreamProbe) hasSubstantive() bool {
	return p.Text != "" || p.HasTool
}
