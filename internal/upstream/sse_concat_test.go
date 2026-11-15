// sse_concat_test.go 钉住「一个 data 行含多个首尾相接 JSON 对象」的拆分与透传
// （issue #498）：SSE 规范里 data 值是一个整体，客户端只能当单个 JSON 解析；上游
// 偶发连写会让整轮对话解析失败。只有每段都合法才拆，否则维持原样透传。
package upstream

import (
	"encoding/json"
	"errors"
	"io"
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

// TestProbeStreamVetoesConcatenatedErrorFrame 粘帧里的 error 段必须进首帧否决：
// 只看首个对象时预览会放行，客户端拿到 200 + error 帧（issue #538）。
func TestProbeStreamVetoesConcatenatedErrorFrame(t *testing.T) {
	t.Parallel()

	raw := `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"你好"}}]} ` +
		`{"error":{"code":6004,"message":"limited"}}` + "\n\n"

	probe := ProbeStream(strings.NewReader(raw))
	defer probe.Release()

	if probe.Kind == ErrNone {
		t.Fatalf("kind=ErrNone，粘帧里的 error 段未触发否决: %+v", probe)
	}

	if !strings.Contains(probe.Frame, "6004") {
		t.Fatalf("frame=%q want 粘帧里的 error 段", probe.Frame)
	}

	if !json.Valid([]byte(probe.Frame)) {
		t.Fatalf("frame 必须是单个合法 JSON（交策略层分类）: %q", probe.Frame)
	}
}

// TestAggregateConcatenatedErrorFrame 非流式粘帧带 error：必须上抛 FrameError，
// 不能静默丢弃后按空流返回 502（issue #538）。
func TestAggregateConcatenatedErrorFrame(t *testing.T) {
	t.Parallel()

	raw := `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"你好"}}]} ` +
		`{"error":{"code":6004,"message":"limited"}}` + "\n\n" +
		"data: [DONE]\n\n"

	_, err := Aggregate(strings.NewReader(raw))
	if !errors.Is(err, errErrorFrame) {
		t.Fatalf("err=%v want error frame", err)
	}

	if payload := ErrorFramePayload(err); !strings.Contains(payload, "6004") {
		t.Fatalf("payload=%q want error 段", payload)
	}
}

// TestAggregateConcatenatedContentFrames 粘帧里的正常内容也要并入聚合，不能只取
// 首个对象丢掉后续段（issue #538）。
func TestAggregateConcatenatedContentFrames(t *testing.T) {
	t.Parallel()

	raw := `data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":"a"}}]} ` +
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":"b"}}]}` + "\n\n" +
		"data: [DONE]\n\n"

	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}

	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "ab" {
		t.Fatalf("content=%q want %q", msg["content"], "ab")
	}
}

// TestTextContinueReaderSplitsConcatenatedFrames 续写层的粘帧同样要按段处理：
// 每帧单独 data 行且都是合法 JSON，error 段不再被当普通行整行透传（issue #538）。
func TestTextContinueReaderSplitsConcatenatedFrames(t *testing.T) {
	t.Parallel()

	raw := `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"a"}}]} ` +
		`{"error":{"code":6004,"message":"limited"}}` + "\n\n" +
		"data: [DONE]\n\n"

	r := NewTextContinueReader(closeString{strings.NewReader(raw)}, "", false, nil)
	defer r.Release()

	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}

	payloads := sseDataPayloads(t, string(out))
	for _, payload := range payloads {
		if payload == "[DONE]" {
			continue
		}

		if !json.Valid([]byte(payload)) {
			t.Fatalf("粘帧未按段拆分，data 载荷不是单个合法 JSON: %q（body=%q）", payload, out)
		}
	}

	if !strings.Contains(string(out), `"content":"a"`) || !strings.Contains(string(out), "6004") {
		t.Fatalf("拆段后内容丢失: %q", out)
	}
}

// TestTextContinueReaderKeepsUnsplitFrameVerbatim 续写层拆不动的粘帧仍整行原样
// 透传（与写出层同口径：宁可少切，不产出半截对象）。
func TestTextContinueReaderKeepsUnsplitFrameVerbatim(t *testing.T) {
	t.Parallel()

	payload := `{"a":1} {"b":`

	r := NewTextContinueReader(closeString{strings.NewReader("data: " + payload + "\n\n")}, "", false, nil)
	defer r.Release()

	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}

	if got := sseDataPayloads(t, string(out)); len(got) != 1 || got[0] != payload {
		t.Fatalf("不可拆帧必须原样透传，got %v: %q", got, out)
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
