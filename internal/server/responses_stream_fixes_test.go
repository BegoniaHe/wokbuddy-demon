// responses_stream_fixes_test.go 钉住 Responses 流式适配的四条修复：
//  1. name 晚到时参数不得丢弃（上游 stripToolCallNames 会保留空 name 的帧）；
//  2. 旧式 function_call（functions API）必须翻译成 tool_call；
//  3. usage 的 cached / cacheWrite 必须独立更新；
//  4. 无 choices 的首帧不得用占位 id 锁死 response.id。
package server

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestResponsesStreamKeepsArgsWhenNameArrivesLate 首片只有 id/arguments，name 在
// 后续分片到达时，先到的参数必须仍在最终 arguments 里。
func TestResponsesStreamKeepsArgsWhenNameArrivesLate(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	w := newResponsesStreamWriter(rec, "m", "", nil, nil)

	chunks := `data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"arguments":"{\"q\":"}}]}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"lookup","arguments":"1}"}}]}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
		"data: [DONE]\n\n"

	if _, err := w.Write([]byte(chunks)); err != nil {
		t.Fatal(err)
	}

	out := rec.Body.String()
	if !strings.Contains(out, `"arguments":"{\"q\":1}"`) {
		t.Fatalf("晚到 name 时参数被截断:\n%s", out)
	}

	if !strings.Contains(out, "event: response.output_item.added") {
		t.Fatalf("缺 output_item.added:\n%s", out)
	}
}

// TestResponsesStreamTranslatesLegacyFunctionCall 旧式 delta.function_call 必须
// 翻译成 function_call 事件，而不是完成一个空 message。
func TestResponsesStreamTranslatesLegacyFunctionCall(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	w := newResponsesStreamWriter(rec, "m", "", nil, nil)

	chunks := `data: {"id":"c1","choices":[{"index":0,"delta":{"function_call":{"name":"lookup","arguments":"{\"q\":1}"}}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"function_call"}]}` + "\n\n" +
		"data: [DONE]\n\n"

	if _, err := w.Write([]byte(chunks)); err != nil {
		t.Fatal(err)
	}

	out := rec.Body.String()
	if !strings.Contains(out, `"type":"function_call"`) || !strings.Contains(out, `"arguments":"{\"q\":1}"`) {
		t.Fatalf("旧式 function_call 未翻译:\n%s", out)
	}
}

// TestResponsesStreamUsageCacheCountersIndependent 末帧只补其中一项时，
// 另一项不能被覆盖回 0。
func TestResponsesStreamUsageCacheCountersIndependent(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	w := newResponsesStreamWriter(rec, "m", "", nil, nil)

	chunks := `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":7}}}` + "\n\n" +
		`data: {"id":"c1","choices":[],"usage":{"prompt_tokens_details":{"cache_write_tokens":5}}}` + "\n\n" +
		"data: [DONE]\n\n"

	if _, err := w.Write([]byte(chunks)); err != nil {
		t.Fatal(err)
	}

	if w.cachedTokens != 7 || w.cacheWrite != 5 {
		t.Fatalf("cached=%d cacheWrite=%d，末帧覆盖了已有计数", w.cachedTokens, w.cacheWrite)
	}
}

// TestResponsesStreamIgnoresChoicelessFirstFrame 无 choices 的首帧不触发流启动：
// 否则占位 id 会顶掉后续帧里的真实 id。
func TestResponsesStreamIgnoresChoicelessFirstFrame(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	w := newResponsesStreamWriter(rec, "m", "", nil, nil)

	chunks := `data: {"choices":[],"usage":{"prompt_tokens":1}}` + "\n\n" +
		`data: {"id":"chatcmpl-real","choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n" +
		"data: [DONE]\n\n"

	if _, err := w.Write([]byte(chunks)); err != nil {
		t.Fatal(err)
	}

	out := rec.Body.String()
	if strings.Contains(out, "resp_gateway") {
		t.Fatalf("占位 id 锁死了 response.id:\n%s", out)
	}

	if !strings.Contains(out, "chatcmpl-real") {
		t.Fatalf("缺失真实 id:\n%s", out)
	}
}
