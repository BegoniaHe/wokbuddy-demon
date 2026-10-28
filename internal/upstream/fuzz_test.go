package upstream

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
)

// FuzzPrepareBodyOpt 请求体改写管线：合法 JSON 输入必须输出合法 JSON，且不 panic。
func FuzzPrepareBodyOpt(f *testing.F) {
	seeds := []string{
		`{"model":"glm-5.2","messages":[]}`,
		`{"model":"deepseek-v4-flash","messages":[{"role":"assistant","reasoning_content":"x","tool_calls":[{"id":"a","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"a","content":"ok"}],"tools":[{"type":"function","function":{"name":"f"}}]}`,
		`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":"https://example.com/a.png"}]}],"tool_choice":{"type":"function","function":{"name":"f"}},"max_completion_tokens":128}`,
		`{"model":"m","messages":[{"role":"developer","content":"x"},{"role":"tool","content":"orphan"}]}`,
		`[]`,
		`null`,
		`{broken`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		out := PrepareBodyOptWithEffortsAndDefault(data, true,
			map[string][]string{"m": {"low", "high"}}, map[string]string{"m": "high"}, 0)
		if !json.Valid(data) {
			return
		}

		if !json.Valid(out) {
			t.Fatalf("合法输入产出了非法 JSON: in=%q out=%q", data, out)
		}
	})
}

// FuzzNormalizeFrame SSE 帧规范化：任意 JSON 对象不得 panic，输出必须可 marshal。
func FuzzNormalizeFrame(f *testing.F) {
	seeds := []string{
		`{"id":"x","choices":[{"index":0,"delta":{"content":"hi"}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
		`{"usage":{"prompt_tokens":1,"completion_tokens":2,"prompt_cache_hit_tokens":1}}`,
		`{"error":{"code":6004,"message":"limit"}}`,
		`{"choices":"bad","delta":null}`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, raw []byte) {
		var obj map[string]any
		if json.Unmarshal(raw, &obj) != nil {
			return
		}

		out := normalizeFrame(obj)
		if _, err := json.Marshal(out); err != nil {
			t.Fatalf("normalizeFrame 输出不可 marshal: %v", err)
		}
	})
}

// discardWriter 丢弃输出，只统计字节数（防单帧内存放大）。
type fuzzWriter struct {
	buf bytes.Buffer
}

func (w *fuzzWriter) Header() http.Header { return http.Header{} }
func (w *fuzzWriter) Write(p []byte) (int, error) {
	return w.buf.Write(p) //nolint:wrapcheck // io.Writer 必须原样透传底层 n 与 err
}
func (w *fuzzWriter) WriteHeader(int) {}
func (w *fuzzWriter) Flush()          {}

// FuzzHandleStreamLine SSE 行处理：任意输入不 panic，写出量有界。
func FuzzHandleStreamLine(f *testing.F) {
	seeds := []string{
		`data: {"id":"x","choices":[{"delta":{"content":"hi"}}]}`,
		`data: [DONE]`,
		`data: {broken`,
		`: ping`,
		``,
		`data: {"choices":[{"delta":{"content":"` + string(bytes.Repeat([]byte("长"), 100)) + `"}}]}`,
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, line string) {
		w := &fuzzWriter{}
		sw := newStreamWriter(w, nil)

		_, _, _ = sw.handleStreamLine(line)
		if w.buf.Len() > len(line)*2+4096 {
			t.Fatalf("单行输出异常放大: in=%d out=%d", len(line), w.buf.Len())
		}
	})
}
