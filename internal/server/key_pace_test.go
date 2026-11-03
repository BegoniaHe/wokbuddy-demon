package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/apikey"
	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// newPaceHandler 构造一个带命名密钥（限速 lim）与固定上游流的 handler。上游固定
// 返回 sseOK（两帧正文 + [DONE]），流式第二帧才会被整形器推迟。
func newPaceHandler(t *testing.T, lim apikey.Limits) (*Handler, *apikey.Store, string) {
	t.Helper()

	return newPaceHandlerWith(t, lim, sseOK)
}

// newPaceHandlerWith 同上，但由调用方指定上游流内容。
func newPaceHandlerWith(t *testing.T, lim apikey.Limits, stream string) (*Handler, *apikey.Store, string) {
	t.Helper()

	store := apikey.New("")

	rec, raw, err := store.Create("paced")
	if err != nil {
		t.Fatal(err)
	}

	if ok, err := store.UpdateLimits(rec.ID, lim); err != nil || !ok {
		t.Fatalf("limits: %v %v", ok, err)
	}

	handler := NewHandler(&Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 200, stream, true }),
		APIKey:   "secret",
		Keys:     store,
		// 开保活：整形等待期间写侧空闲会写心跳帧，顺带覆盖「读 goroutine 持续
		// 解析正文 / 写 goroutine 同时查询整形器」的并发路径（-race 下可验）。
		KeepaliveInterval: 10 * time.Millisecond,
	})

	return handler, store, raw
}

// paceSSEFrames 构造 n 个正文帧 + 收尾帧 + [DONE]：正文帧多到读侧会持续产出
// token 估算，覆盖多帧整形与读写并发路径。
func paceSSEFrames(n int) string {
	var out strings.Builder

	payload := strings.Repeat("a", 40) // 每帧约 10 token
	for range n {
		out.WriteString(`data: {"id":"chatcmpl-1","object":"chat.completion.chunk",` +
			`"created":1753600000,"model":"glm-5.2","choices":[{"index":0,` +
			`"delta":{"content":"` + payload + `"}}]}` + "\n\n")
	}

	out.WriteString(`data: {"id":"chatcmpl-1","object":"chat.completion.chunk",` +
		`"created":1753600000,"model":"glm-5.2","choices":[{"index":0,"delta":{},` +
		`"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,` +
		`"total_tokens":2}}` + "\n\n")
	out.WriteString("data: [DONE]\n\n")

	return out.String()
}

// paceDo 发一次请求并返回状态码、响应体与总耗时（含整形等待）。
func paceDo(t *testing.T, handler *Handler, path, token, body string, anthropic bool) (int, string, time.Duration) {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)

	if anthropic {
		req.Header.Set("Anthropic-Version", "2023-06-01")
	}

	rec := httptest.NewRecorder()

	start := time.Now()

	handler.ServeHTTP(rec, req)

	return rec.Code, rec.Body.String(), time.Since(start)
}

// TestKeyPaceStreamRPMStretches 只配 rpm 时（issue #448 R3）：不返回 429，首帧
// 立即转出，连发的第二发被整形账本推迟（等价于把下一发请求推迟出去）。
func TestKeyPaceStreamRPMStretches(t *testing.T) {
	t.Parallel()

	// rpm=120 → 每发间隔 500ms；连发第二发应被推迟约一个间隔。
	handler, _, raw := newPaceHandler(t, apikey.Limits{RPM: 120})

	body := `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`

	code, out, first := paceDo(t, handler, "/v1/chat/completions", raw, body, false)
	if code != http.StatusOK {
		t.Fatalf("首发 code=%d body=%s", code, out)
	}

	if !strings.Contains(out, "[DONE]") {
		t.Fatalf("流式响应缺少 [DONE]: %s", out)
	}

	code, out, second := paceDo(t, handler, "/v1/chat/completions", raw, body, false)
	if code != http.StatusOK {
		t.Fatalf("第二发 code=%d body=%s（RPM 不应再返回 429）", code, out)
	}

	if second < first+400*time.Millisecond {
		t.Fatalf("连发第二发耗时=%v（首发 %v）：rpm 整形未推迟写出", second, first)
	}
}

// TestKeyPaceStreamTPMStretches 只配 tpm 时（issue #448 R4）：token 账按
// 「prompt 估算 + 已吐出正文」累计到时间线，连发的下一发据此推迟后续帧；不返回
// 429、不影响 [DONE] 收尾。
func TestKeyPaceStreamTPMStretches(t *testing.T) {
	t.Parallel()

	// tpm=24000 → 2.5ms/token；约 470 ASCII 字符 ≈ 118 token ≈ 295ms，再加 8 个
	// 正文帧的增量账。多帧流同时覆盖读写并发路径。
	handler, _, raw := newPaceHandlerWith(t, apikey.Limits{TPM: 24000}, paceSSEFrames(8))

	body := `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"` +
		strings.Repeat("a", 400) + `"}]}`

	code, out, first := paceDo(t, handler, "/v1/chat/completions", raw, body, false)
	if code != http.StatusOK {
		t.Fatalf("首发 code=%d body=%s（TPM 不应返回 429）", code, out)
	}

	if !strings.Contains(out, "[DONE]") {
		t.Fatalf("流式响应缺少 [DONE]: %s", out)
	}

	code, out, second := paceDo(t, handler, "/v1/chat/completions", raw, body, false)
	if code != http.StatusOK {
		t.Fatalf("第二发 code=%d body=%s", code, out)
	}

	if second < first+150*time.Millisecond {
		t.Fatalf("连发第二发耗时=%v（首发 %v）：tpm 整形未推迟后续帧", second, first)
	}
}

// TestKeyPaceNonStreamDelayed 非流式（issue #448 R8）：状态码仍是 200、响应体是
// 正常 JSON，只有写出被推迟；不在超速时返回 429。
func TestKeyPaceNonStreamDelayed(t *testing.T) {
	t.Parallel()

	handler, _, raw := newPaceHandler(t, apikey.Limits{RPM: 120})

	body := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`

	code, out, first := paceDo(t, handler, "/v1/chat/completions", raw, body, false)
	if code != http.StatusOK {
		t.Fatalf("首发 code=%d body=%s", code, out)
	}

	if !strings.Contains(out, `"choices"`) {
		t.Fatalf("非流式响应应为 JSON: %s", out)
	}

	code, out, second := paceDo(t, handler, "/v1/chat/completions", raw, body, false)
	if code != http.StatusOK {
		t.Fatalf("第二发 code=%d body=%s（应仍为 200）", code, out)
	}

	if second < first+400*time.Millisecond {
		t.Fatalf("连发第二发耗时=%v（首发 %v）：非流式写出未被推迟", second, first)
	}
}

// TestKeyPaceNoLimitsNoDelay 未配任何限速（issue #448 R1）：不进整形账本，连发
// 两发都不产生额外等待。
func TestKeyPaceNoLimitsNoDelay(t *testing.T) {
	t.Parallel()

	handler, _, raw := newPaceHandler(t, apikey.Limits{})

	body := `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`

	for i := range 2 {
		code, out, elapsed := paceDo(t, handler, "/v1/chat/completions", raw, body, false)
		if code != http.StatusOK {
			t.Fatalf("第 %d 发 code=%d body=%s", i, code, out)
		}

		if elapsed > 400*time.Millisecond {
			t.Fatalf("第 %d 发耗时=%v：无限速密钥不应有任何整形等待", i, elapsed)
		}
	}
}

// TestKeyPaceAnthropicStream Anthropic 面（issue #448 R9）：整形器在协议转写
// 之下按帧生效，/v1/messages 的连发同样被推迟，事件形态不变。
func TestKeyPaceAnthropicStream(t *testing.T) {
	t.Parallel()

	handler, _, raw := newPaceHandler(t, apikey.Limits{RPM: 120})

	body := `{"model":"glm-5.2","max_tokens":16,"stream":true,` +
		`"messages":[{"role":"user","content":"hi"}]}`

	code, out, first := paceDo(t, handler, "/v1/messages", raw, body, true)
	if code != http.StatusOK {
		t.Fatalf("首发 code=%d body=%s", code, out)
	}

	for _, want := range []string{"message_start", "message_stop"} {
		if !strings.Contains(out, want) {
			t.Fatalf("Anthropic 流缺少 %s: %s", want, out)
		}
	}

	code, out, second := paceDo(t, handler, "/v1/messages", raw, body, true)
	if code != http.StatusOK {
		t.Fatalf("第二发 code=%d body=%s", code, out)
	}

	if second < first+400*time.Millisecond {
		t.Fatalf("Anthropic 连发第二发耗时=%v（首发 %v）：整形未生效", second, first)
	}
}

// TestKeyPaceGeminiStream Gemini 面（issue #448 R9）：同一账本同样推迟写出。
func TestKeyPaceGeminiStream(t *testing.T) {
	t.Parallel()

	handler, _, raw := newPaceHandler(t, apikey.Limits{RPM: 120})

	body := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
	path := "/v1beta/models/glm-5.2:streamGenerateContent?alt=sse"

	code, out, first := paceDo(t, handler, path, raw, body, false)
	if code != http.StatusOK {
		t.Fatalf("首发 code=%d body=%s", code, out)
	}

	if !strings.Contains(out, `"finishReason"`) {
		t.Fatalf("Gemini 流形态不正确: %s", out)
	}

	code, out, second := paceDo(t, handler, path, raw, body, false)
	if code != http.StatusOK {
		t.Fatalf("第二发 code=%d body=%s", code, out)
	}

	if second < first+400*time.Millisecond {
		t.Fatalf("Gemini 连发第二发耗时=%v（首发 %v）：整形未生效", second, first)
	}
}

// dropWrite 第一帧写完即失败的客户端连接：模拟客户端在流中段断开。
// 每次 SSE 帧写 3 段（"data: " / 载荷 / "\n\n"），第 2 段失败即中断在首帧。
type dropWrite struct {
	header http.Header
	writes int
}

// errClientGoneTest 客户端已走的测试错误（err113：测试同样不用动态错误）。
var errClientGoneTest = errors.New("client gone")

func newDropWrite() *dropWrite { return &dropWrite{header: http.Header{}} }

func (w *dropWrite) Header() http.Header { return w.header }

func (w *dropWrite) WriteHeader(int) {}

func (w *dropWrite) Write(payload []byte) (int, error) {
	w.writes++
	if w.writes > 1 {
		return 0, errClientGoneTest
	}

	return len(payload), nil
}

// TestKeyPaceDropOnClientGone 客户端断开（issue #448 验收 7）：写出失败后本密钥
// 未兑现的整形债务整条退还，后续请求不再被旧节奏压着。
func TestKeyPaceDropOnClientGone(t *testing.T) {
	t.Parallel()

	// rpm=6 → 10s/发：第二发的请求时间线会被拨到 10s 后。
	handler, store, raw := newPaceHandler(t, apikey.Limits{RPM: 6})
	body := `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`

	// 首发正常完成，把请求时间线拨到「现在」。
	if code, out, _ := paceDo(t, handler, "/v1/chat/completions", raw, body, false); code != http.StatusOK {
		t.Fatalf("首发 code=%d body=%s", code, out)
	}

	// 第二发首帧就写失败（客户端已走），整形债务必须被退还。
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+raw)
	handler.ServeHTTP(newDropWrite(), req)

	recs := store.List()
	if len(recs) != 1 {
		t.Fatalf("记录数=%d want 1", len(recs))
	}

	if wait := store.PaceWait(recs[0].ID, time.Now()); wait != 0 {
		t.Fatalf("断开后整形债务未被退还：等待=%v want 0", wait)
	}
}
