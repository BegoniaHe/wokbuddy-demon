// stream_continuation_usage_test.go 钉住流式续写的跨段用量合计（issue #539 的
// 验收口径）：续写是第二次独立计费的上游调用，但统计侧只包一条 chatStatsReader、
// applyUsage 是覆盖语义——两段 usage 帧能对上，是因为续写层先按「prompt 取末段、
// completion/credit 累加」把首段用量并进了续写帧（rewriteContinuationFrame，
// issue #247 / #432）。这组测试从 stats 侧复读该合计，防止有人在统计层再加一次。
package server

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// continuationStreams 构造首段/续写段两条 SSE 流与对应的 open 回调。
func continuationStreams(t *testing.T, first, second string) (*upstream.TextContinueReader, func()) {
	t.Helper()

	cont := upstream.NewTextContinueReader(io.NopCloser(strings.NewReader(first)), "", false,
		func(string) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader(second)), nil
		})

	return cont, cont.Release
}

// readStats 读完续写流并返回统计读数（不是客户端响应体，而是记账用的 stats）。
func readStats(t *testing.T, cont *upstream.TextContinueReader) *chatStatsReader {
	t.Helper()

	stats := newChatStatsReaderSince(cont, time.Now())
	t.Cleanup(stats.release)

	if _, err := io.ReadAll(stats); err != nil {
		t.Fatalf("read stream: %v", err)
	}

	return stats
}

// assertContinuationUsage 断言统计与两段之和一致：completion/total/credit 累加，
// prompt 取末段。
func assertContinuationUsage(
	t *testing.T, stats *chatStatsReader, wantTokens int, wantCredit float64, wantTotal int,
	wantPrompt int64,
) {
	t.Helper()

	if tokens, ok := stats.Tokens(); !ok || tokens != wantTokens {
		t.Errorf("completion_tokens=%d ok=%v want %d", tokens, ok, wantTokens)
	}

	if credit, ok := stats.Credit(); !ok || credit != wantCredit {
		t.Errorf("credit=%v ok=%v want %v", credit, ok, wantCredit)
	}

	if total, ok := stats.TotalTokens(); !ok || total != wantTotal {
		t.Errorf("total_tokens=%d ok=%v want %d", total, ok, wantTotal)
	}

	if usage := stats.Usage(); !usage.HasPromptTokens || usage.PromptTokens != wantPrompt {
		t.Errorf("prompt_tokens=%d has=%v want %d（末段值）",
			usage.PromptTokens, usage.HasPromptTokens, wantPrompt)
	}
}

// TestStreamLengthContinuationStatsSumsUsage length 截断续写：stats 的
// completion_tokens/credit/total_tokens 等于两段之和，prompt 等于末段值。
func TestStreamLengthContinuationStatsSumsUsage(t *testing.T) {
	t.Parallel()

	first := `data: {"id":"a","choices":[{"index":0,"delta":{"content":"hello "}}]}` + "\n\n" +
		`data: {"id":"a","choices":[{"index":0,"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12,"credit":1}}` + "\n\n" +
		"data: [DONE]\n\n"
	second := `data: {"id":"b","choices":[{"index":0,"delta":{"content":"world"},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":4,"total_tokens":24,"credit":2}}` + "\n\n" +
		"data: [DONE]\n\n"

	cont, release := continuationStreams(t, first, second)
	defer release()

	stats := readStats(t, cont)
	if !cont.Continued() {
		t.Fatal("length 截断未触发续写")
	}

	assertContinuationUsage(t, stats, 6, 3, 26, 20)
}

// TestStreamStopContinuationStatsSumsUsage 长文 stop 续写（ContinueOnStop）与
// length 截断同口径：stats 也必须是两段合计。
func TestStreamStopContinuationStatsSumsUsage(t *testing.T) {
	t.Parallel()

	first := `data: {"id":"a","choices":[{"index":0,"delta":{"content":"hello there"}}]}` + "\n\n" +
		`data: {"id":"a","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12,"credit":1}}` + "\n\n" +
		"data: [DONE]\n\n"
	second := `data: {"id":"b","choices":[{"index":0,"delta":{"content":" again"},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":4,"total_tokens":24,"credit":2}}` + "\n\n" +
		"data: [DONE]\n\n"

	cont, release := continuationStreams(t, first, second)
	defer release()

	cont.ContinueOnStop(3)

	stats := readStats(t, cont)
	if !cont.Continued() {
		t.Fatal("长文 stop 未触发续写")
	}

	assertContinuationUsage(t, stats, 6, 3, 26, 20)
}
