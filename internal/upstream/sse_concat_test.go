// sse_concat_test.go 钉住「一个 data 行含多个首尾相接 JSON 对象」的拆分与透传
// （issue #498）：SSE 规范里 data 值是一个整体，客户端只能当单个 JSON 解析；上游
// 偶发连写会让整轮对话解析失败。只有每段都合法才拆，否则维持原样透传。
package upstream

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSplitConcatenatedJSON 表驱动：字符串内花括号/转义引号不算结构，半截对象、
// 括号不配平、纯文本一律不拆（宁可少切，不产出半截对象）。
func TestSplitConcatenatedJSON(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		payload string
		want    []string
	}{
		{
			name:    "两个对象空格分隔",
			payload: `{"a":1} {"b":2}`,
			want:    []string{`{"a":1}`, `{"b":2}`},
		},
		{
			name:    "两个对象紧邻",
			payload: `{"a":1}{"b":2}`,
			want:    []string{`{"a":1}`, `{"b":2}`},
		},
		{
			name:    "字符串内的花括号与方括号",
			payload: `{"a":{"s":"}{[]"},"b":"}"} {"c":3}`,
			want:    []string{`{"a":{"s":"}{[]"},"b":"}"}`, `{"c":3}`},
		},
		{
			name:    "转义引号在字符串内",
			payload: `{"a":"he said \"hi\" }"} {"b":2}`,
			want:    []string{`{"a":"he said \"hi\" }"}`, `{"b":2}`},
		},
		{
			name:    "多行 data 换行分隔",
			payload: "{\"a\":1}\n{\"b\":2}",
			want:    []string{`{"a":1}`, `{"b":2}`},
		},
		{
			name:    "数组与对象拼接",
			payload: `[1,2] {"a":1}`,
			want:    []string{`[1,2]`, `{"a":1}`},
		},
		{
			name:    "单个对象不拆",
			payload: `{"a":1}`,
			want:    nil,
		},
		{
			name:    "半截对象不拆",
			payload: `{"a":1} {"b":}`,
			want:    nil,
		},
		{
			name:    "括号不配平不拆",
			payload: `{"a":1} {"b":2`,
			want:    nil,
		},
		{
			name:    "纯文本不拆",
			payload: `hello world`,
			want:    nil,
		},
		{
			name:    "结构外字符串不拆",
			payload: `"a" "b"`,
			want:    nil,
		},
		{
			name:    "空串不拆",
			payload: "",
			want:    nil,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got := splitConcatenatedJSON(testCase.payload)
			if len(got) != len(testCase.want) {
				t.Fatalf("segments=%v want %v", got, testCase.want)
			}

			for i := range testCase.want {
				if got[i] != testCase.want[i] {
					t.Errorf("segment[%d]=%q want %q", i, got[i], testCase.want[i])
				}
			}
		})
	}
}

// TestStreamSplitsConcatenatedJSONFrames 端到端：粘帧输入经真实 StreamHint 透传后，
// 每个 SSE 事件恰好一个 data 行、每帧单个合法 JSON，内容不丢，[DONE] 恰好一次。
func TestStreamSplitsConcatenatedJSONFrames(t *testing.T) {
	t.Parallel()

	raw := `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"a"}}]} ` +
		`{"id":"c1","choices":[{"index":0,"delta":{"content":"b"}}]}` + "\n\n" +
		"data: [DONE]\n\n"
	rec := httptest.NewRecorder()

	if err := StreamHint(rec, strings.NewReader(raw), nil); err != nil {
		t.Fatalf("stream: %v", err)
	}

	body := rec.Body.String()
	if n := strings.Count(body, "data: [DONE]"); n != 1 {
		t.Errorf("[DONE] count=%d want 1: %q", n, body)
	}

	payloads := sseDataPayloads(t, body)
	if len(payloads) != 3 {
		t.Fatalf("data 帧数=%d want 3（两个内容帧 + [DONE]）: %q", len(payloads), body)
	}

	for _, payload := range payloads {
		if payload == "[DONE]" {
			continue
		}

		if !json.Valid([]byte(payload)) {
			t.Errorf("data 帧不是单个合法 JSON: %q", payload)
		}
	}

	if !strings.Contains(body, `"content":"a"`) || !strings.Contains(body, `"content":"b"`) {
		t.Errorf("拆分后内容丢失: %q", body)
	}
}

// TestStreamKeepsUnsplitFrameVerbatim 不可拆的粘帧（第二段是半截对象）维持原样
// 透传：宁可少切，不产出半截对象、也不丢内容。
func TestStreamKeepsUnsplitFrameVerbatim(t *testing.T) {
	t.Parallel()

	payload := `{"a":1} {"b":`
	rec := httptest.NewRecorder()

	// 不可拆帧不计有效帧，StreamHint 照既有语义返回空流错误（写出的字节不变）。
	_ = StreamHint(rec, strings.NewReader("data: "+payload+"\n\n"), nil)

	body := rec.Body.String()
	if payloads := sseDataPayloads(t, body); len(payloads) == 0 || payloads[0] != payload {
		t.Fatalf("不可拆帧必须原样透传，got %v: %q", payloads, body)
	}
}

// sseDataPayloads 按出现顺序取出 SSE 响应体里的 data 载荷。
func sseDataPayloads(t *testing.T, body string) []string {
	t.Helper()

	var payloads []string

	for line := range strings.SplitSeq(body, "\n") {
		if payload, ok := sseDataPayload(strings.TrimRight(line, "\r")); ok {
			payloads = append(payloads, payload)
		}
	}

	return payloads
}
