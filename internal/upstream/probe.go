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
	"io"
	"strings"
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
	// passThrough 已出现合法终态（如 content_filter）但无正文：直接透传，
	// 不按账号故障轮转。
	passThrough bool

	buffered []byte
	reader   *bufio.Reader
}

// Reader 返回「已预览字节 + 上游剩余流」的读取器。
func (p *StreamProbe) Reader() io.Reader {
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

// ProbeStream 预览上游 SSE 流。返回的 StreamProbe 总可安全调用 Reader。
func ProbeStream(r io.Reader) *StreamProbe {
	probe := &StreamProbe{reader: bufio.NewReaderSize(r, 64*1024)}

	for len(probe.buffered) < probeMaxBytes {
		line, err := probe.reader.ReadString('\n')

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
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, "data: ") {
		return
	}

	payload := strings.TrimPrefix(line, "data: ")
	if payload == "[DONE]" {
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

	if _, _, isErr := BusinessFrameError(payload); isErr {
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
//
//nolint:cyclop // 逐 choice 解析 delta/message 与终态，线性分支更直观。
func (p *StreamProbe) inspectChoices(frame map[string]any) {
	choices, _ := frame["choices"].([]any)

	for _, raw := range choices {
		choice, _ := raw.(map[string]any)
		if choice == nil {
			continue
		}

		if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
			p.FinishReason = fr
		}

		if delta, ok := choice["delta"].(map[string]any); ok {
			p.inspectDelta(delta)
		}

		if msg, ok := choice["message"].(map[string]any); ok {
			p.inspectDelta(msg)
		}
	}

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
