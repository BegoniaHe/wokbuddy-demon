package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/apikey"
	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
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
func paceSSEFrames(n, payloadLen int) string {
	var out strings.Builder

	payload := strings.Repeat("a", payloadLen)
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

// TestKeyPaceStreamTPMStretches 只配 tpm 时（issue #448 R4）：token 账只按
// Decode 阶段已吐出的 completion 累计，多帧流按该时间线推迟后续帧；不返回 429、
// 不影响 [DONE] 收尾。输入提示词长度不参与该时间线，故请求体只放短 prompt。
func TestKeyPaceStreamTPMStretches(t *testing.T) {
	t.Parallel()

	// tpm=2400 → 25ms/token；8 帧各约 10 completion token，合计约 2s。
	handler, _, raw := newPaceHandlerWith(t, apikey.Limits{TPM: 2400}, paceSSEFrames(8, 40))

	body := `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`

	code, out, elapsed := paceDo(t, handler, "/v1/chat/completions", raw, body, false)
	if code != http.StatusOK {
		t.Fatalf("code=%d body=%s（TPM 不应返回 429）", code, out)
	}

	if !strings.Contains(out, "[DONE]") {
		t.Fatalf("流式响应缺少 [DONE]: %s", out)
	}

	if elapsed < 1500*time.Millisecond {
		t.Fatalf("多帧流耗时=%v want ≥1.5s：tpm 未推迟后续帧", elapsed)
	}

	if elapsed > 10*time.Second {
		t.Fatalf("多帧流耗时=%v：tpm 整形等待失控", elapsed)
	}
}

// TestAttachStreamPacerWiresRequestCancellation 生产构造路径必须把请求取消
// 通道交给 keyPacer，streamWriter 才会在客户端断开后立即停止帧间等待。
func TestAttachStreamPacerWiresRequestCancellation(t *testing.T) {
	t.Parallel()

	store := apikey.New("")

	rec, _, err := store.Create("paced")
	if err != nil {
		t.Fatal(err)
	}

	if ok, err := store.UpdateLimits(rec.ID, apikey.Limits{RPM: 60}); !ok || err != nil {
		t.Fatalf("limits: %v %v", ok, err)
	}

	out := &pacerCaptureResponder{}
	ctx, cancel := context.WithCancel(context.Background())

	flow := &chatFlow{
		out:      out,
		keyUsage: &keyUsage{id: rec.ID, keys: store},
	}
	flow.attachStreamPacer(ctx, &chatStatsReader{})

	cancelable, ok := out.pacer.(interface{ Done() <-chan struct{} })
	if !ok {
		t.Fatalf("pacer=%T 未实现 Done()", out.pacer)
	}

	select {
	case <-cancelable.Done():
		t.Fatal("请求未取消时 Done 不应关闭")
	default:
	}

	cancel()

	select {
	case <-cancelable.Done():
	case <-time.After(time.Second):
		t.Fatal("上下文取消后 Done 未关闭")
	}
}

// pacerCaptureResponder 只捕获 attachStreamPacer 挂上的整形器；其余端口方法
// 不会在本用例路径被调用。
type pacerCaptureResponder struct {
	pacer upstream.ResponsePacer
}

func (r *pacerCaptureResponder) setStreamPacer(p upstream.ResponsePacer) { r.pacer = p }
func (r *pacerCaptureResponder) JSON(int, any)                           {}
func (r *pacerCaptureResponder) Error(int, string, string, string)       {}
func (r *pacerCaptureResponder) Header(string, string)                   {}
func (r *pacerCaptureResponder) Stream(
	io.Reader, func(string) string, *upstream.MarkupRepair, func(string),
) error {
	return nil
}

// TestKeyPacerChargesCompletionOnly 直接验 token 账的输入：seen 只提供
// completion 增量时，下一次查询按 tpm 产生等待；prompt 没有参数可入账。
func TestKeyPacerChargesCompletionOnly(t *testing.T) {
	t.Parallel()

	store := apikey.New("")

	rec, _, err := store.Create("paced")
	if err != nil {
		t.Fatal(err)
	}

	if ok, err := store.UpdateLimits(rec.ID, apikey.Limits{TPM: 1200}); !ok || err != nil {
		t.Fatalf("limits: %v %v", ok, err)
	}

	pacer := &keyPacer{
		keys:    store,
		id:      rec.ID,
		seen:    func() int64 { return 240 },
		maxWait: apikey.PaceMaxDelay,
	}

	// tpm=1200 → 50ms/token；240 completion token → 12s token 债务。
	if got := pacer.WaitBeforeWrite(); got != 12*time.Second {
		t.Fatalf("首查等待=%v want 12s（只按 240 completion token）", got)
	}

	// seen 未增长时不得重复入账；墙上时间只会让等待略减。
	if got := pacer.WaitBeforeWrite(); got <= 0 || got > 12*time.Second {
		t.Fatalf("重复查询等待=%v want (0, 12s] 且未重复入账", got)
	}
}

// TestKeyPacerReconcilesExactUsage 末帧 usage 到达后必须用精确 completion_tokens
// 替换本请求的估算，并把多估的 token 从跨请求时间线退回。
func TestKeyPacerReconcilesExactUsage(t *testing.T) {
	t.Parallel()

	store := apikey.New("")

	rec, _, err := store.Create("paced")
	if err != nil {
		t.Fatal(err)
	}

	if ok, err := store.UpdateLimits(rec.ID, apikey.Limits{TPM: 600}); !ok || err != nil {
		t.Fatalf("limits: %v %v", ok, err)
	}

	var exact atomic.Int64

	var exactSeen atomic.Bool

	pacer := &keyPacer{
		keys:    store,
		id:      rec.ID,
		seen:    func() int64 { return 300 },
		exact:   func() (int64, bool) { return exact.Load(), exactSeen.Load() },
		maxWait: apikey.PaceMaxDelay,
	}

	// 先按 300 token 入账 → 30s。
	if got := pacer.WaitBeforeWrite(); got < 29*time.Second || got > 30*time.Second {
		t.Fatalf("估算等待=%v want ≈30s", got)
	}

	exact.Store(100)
	exactSeen.Store(true)

	// 精确 usage 只有 100 token → 应退回 20s；PaceWait 在 now 仍应约 10s。
	if got := pacer.WaitBeforeWrite(); got < 9*time.Second || got > 10*time.Second {
		t.Fatalf("精确对账后等待=%v want ≈10s", got)
	}

	if wait := store.PaceWait(rec.ID, time.Now()); wait < 9*time.Second || wait > 10*time.Second {
		t.Fatalf("账本等待=%v want ≈10s（估算已替换为精确值）", wait)
	}
}

// TestKeyPacerReconcilesAfterWaitCap 等待预算耗尽后的末帧 usage 仍必须对账：
// 此时不再延迟写帧，但估算与精确值的差额不能永久留在跨请求账本。
func TestKeyPacerReconcilesAfterWaitCap(t *testing.T) {
	t.Parallel()

	store := apikey.New("")

	rec, _, err := store.Create("paced")
	if err != nil {
		t.Fatal(err)
	}

	if ok, err := store.UpdateLimits(rec.ID, apikey.Limits{TPM: 600}); !ok || err != nil {
		t.Fatalf("limits: %v %v", ok, err)
	}

	var exact atomic.Int64

	var exactSeen atomic.Bool

	pacer := &keyPacer{
		keys:    store,
		id:      rec.ID,
		seen:    func() int64 { return 300 },
		exact:   func() (int64, bool) { return exact.Load(), exactSeen.Load() },
		emitted: 300,
		waited:  apikey.PaceMaxDelay,
		maxWait: apikey.PaceMaxDelay,
	}

	// 模拟先前已按 300 token 拨入账本；预算已耗尽。
	store.NoteEmitted(rec.ID, time.Now(), 300)

	exact.Store(100)
	exactSeen.Store(true)

	if got := pacer.WaitBeforeWrite(); got != 0 {
		t.Fatalf("封顶后等待=%v want 0", got)
	}

	if wait := store.PaceWait(rec.ID, time.Now()); wait < 9*time.Second || wait > 10*time.Second {
		t.Fatalf("封顶后账本等待=%v want ≈10s（必须对账精确值）", wait)
	}
}

// TestKeyPacerCapsPerRequestWait 单请求累计等待不得越过 60s：最后 10s 的预算
// 只分配一次，之后即使账本仍有债务也直接放完剩余帧。
func TestKeyPacerCapsPerRequestWait(t *testing.T) {
	t.Parallel()

	store := apikey.New("")

	rec, _, err := store.Create("paced")
	if err != nil {
		t.Fatal(err)
	}

	if ok, err := store.UpdateLimits(rec.ID, apikey.Limits{RPM: 1}); !ok || err != nil {
		t.Fatalf("limits: %v %v", ok, err)
	}

	// 两次请求间隔把账本推到 60s 后；用固定时钟无法注入 keyPacer，因此这里只
	// 验证累计预算本身：最后 10s 只分配一次，下一帧起不再等待。
	now := time.Now()
	store.PaceRequest(rec.ID, now)
	store.PaceRequest(rec.ID, now)

	pacer := &keyPacer{
		keys:    store,
		id:      rec.ID,
		waited:  apikey.PaceMaxDelay - 10*time.Second,
		maxWait: apikey.PaceMaxDelay,
	}

	if got := pacer.WaitBeforeWrite(); got < 9*time.Second || got > 10*time.Second {
		t.Fatalf("最后预算等待=%v want (9s, 10s]", got)
	}

	if got := pacer.WaitBeforeWrite(); got != 0 {
		t.Fatalf("到顶后等待=%v want 0", got)
	}
}

// TestPaceTokensForCompletionOnly 非流式 TPS 账只取 completion_tokens：
// 相同 response 无论 prompt 多长，入账始终等于输出 token。
func TestPaceTokensForCompletionOnly(t *testing.T) {
	t.Parallel()

	resp := map[string]any{
		"usage": map[string]any{
			"prompt_tokens":     int64(100000),
			"completion_tokens": int64(42),
		},
	}

	if got := paceTokensFor(resp); got != 42 {
		t.Fatalf("paceTokensFor=%d want 42（只计 completion）", got)
	}

	if got := paceTokensFor(map[string]any{}); got != 0 {
		t.Fatalf("usage 缺失时=%d want 0", got)
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
