package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"go.uber.org/goleak"

	"github.com/BegoniaHe/wokbuddy-demon/internal/apikey"
	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/cst"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/prompt"
	"github.com/BegoniaHe/wokbuddy-demon/internal/redisstore"
	"github.com/BegoniaHe/wokbuddy-demon/internal/session"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
	"github.com/BegoniaHe/wokbuddy-demon/internal/usage"
)

var (
	errConnectionRefused     = errors.New("connection refused")
	errConnectionResetByPeer = errors.New("connection reset by peer")
)

// TestMain 默认关闭聊天表格日志（chatLogEnabled=false），消除 go test 期间的 stdout 噪音。
// 断言表格行输出的测试（logging_test.go 中的 ChatLogs/LogChatRow 系列）用 withChatLog 临时开启。
func TestMain(m *testing.M) {
	chatLogEnabled.Store(false)
	// 测试期去掉轮换退避的真实等待（默认 500ms/次，16 条轮换用例累计约 7s）。
	// 仅影响轮换间 sleep，不改任何换号/退避决策语义（无测试断言其时长）。
	rotateBackoffBase = 0

	goleak.VerifyTestMain(m,
		// 测试用的 httptest 服务与自定义 Transport 会保留 keep-alive 空闲连接，
		// 其读写循环属标准库连接池，不是被测代码泄漏的 goroutine。
		goleak.IgnoreAnyFunction("net/http.(*persistConn).readLoop"),
		goleak.IgnoreAnyFunction("net/http.(*persistConn).writeLoop"),
	)
}

const sseOK = "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"你好\"}}]}\n\n" +
	"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n" +
	"data: [DONE]\n\n"

// newFakeUpstream 返回一个 ChatStream 走 fake 的 upstream.Client。
// fake 依据 Authorization 头决定行为。
//
// models.dev 目录拉取（models.dev 的按需兜底）复用同一个 HTTP client，但目标不是
// 上游：这里按 host 短路返回空文档，既不联网也不进入 behavior（避免污染各用例的
// 「上游调用次数」断言）。
func newFakeUpstream(t *testing.T, behavior func(auth string) (status int, body string, isStream bool)) *upstream.Client {
	t.Helper()

	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Host == "models.dev" {
				return jsonResponse(http.StatusOK, "{}"), nil
			}

			authz := r.Header.Get("Authorization")
			status, body, isStream := behavior(authz)

			str := "application/json"
			if isStream {
				str = "text/event-stream"
			}

			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{str}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// bindStore 记录粘性绑定镜像调用（不联网），供 D4 端到端断言绑定收敛到最终成功号。
type bindStore struct {
	redisstore.Noop

	mu     sync.Mutex
	binds  map[string]string
	delCnt int
}

func newBindStore() *bindStore { return &bindStore{binds: map[string]string{}} }

func (b *bindStore) SetBind(key, uid string, ttl time.Duration) {
	b.mu.Lock()
	b.binds[key] = uid
	b.mu.Unlock()
}

func (b *bindStore) DelBind(key string) {
	b.mu.Lock()
	b.delCnt++
	delete(b.binds, key)
	b.mu.Unlock()
}

func (b *bindStore) LoadBinds() map[string]string {
	b.mu.Lock()
	defer b.mu.Unlock()

	out := map[string]string{}
	maps.Copy(out, b.binds)

	return out
}

func (b *bindStore) lastUID(key string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	u, ok := b.binds[key]

	return u, ok
}

// testPoolWith 构建一个所有账号 credits=1000 的池，并注入确定性随机源：
// randInt64N 恒返回 0 → pickWeighted 必选候选集中积分最高者（第一个）。
// 这让依赖"bad 先被选中"的轮转测试（如 TestChatRotatesOnHardCredit）完全确定，
// 不再受加权随机影响而 flake。
func testPoolWith(auths ...*auth.Auth) *pool.Pool {
	state := pool.New("")
	state.SetRandomSource(func(n int64) int64 { return 0 })

	for _, a := range auths {
		state.Add(a)
		state.SetCredits(a.UID, 1000, 0)
	}

	return state
}

// postChat 向 h 发一次 /v1/chat/completions 请求（body 为 JSON 字符串），返回响应记录器。
func postChat(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))

	return rec
}

// statusOf 取账号状态；账号缺失即 Fatal。
func statusOf(t *testing.T, p *pool.Pool, uid string) pool.Status {
	t.Helper()

	st, ok := p.Status(uid)
	if !ok {
		t.Fatalf("account %s status missing", uid)
	}

	return st
}

// assertChatSuccessUsage 断言单次成功 chat 的标准用量（请求数/用量次数/token 三元组 + 性能观测）。
func assertChatSuccessUsage(t *testing.T, p *pool.Pool, uid string) {
	t.Helper()

	status := statusOf(t, p, uid)
	if status.TokenUsage.RequestCount != 1 || status.TokenUsage.UsageCount != 1 ||
		status.TokenUsage.PromptTokens != 1 || status.TokenUsage.CompletionTokens != 1 || status.TokenUsage.TotalTokens != 2 {
		t.Errorf("token usage=%+v", status.TokenUsage)
	}

	if status.TokenUsage.LastLatencyMs < 1 || status.TokenUsage.LastTokensPerSecond == nil || *status.TokenUsage.LastTokensPerSecond <= 0 {
		t.Errorf("latest performance=%+v", status.TokenUsage)
	}
}

// assertCalls 断言每个 Authorization（完整 Bearer 头）的调用次数。
func assertCalls(t *testing.T, calls, want map[string]int) {
	t.Helper()

	if len(calls) != len(want) {
		t.Fatalf("calls=%v want %v", calls, want)
	}

	for k, v := range want {
		if calls[k] != v {
			t.Errorf("calls[%q]=%d want %d", k, calls[k], v)
		}
	}
}

// assertSoftRateCooling 断言账号处于 soft_rate 冷却，剩余时长落在 (0, maxSec]。
func assertSoftRateCooling(t *testing.T, status3 pool.Status, maxSec int64) {
	t.Helper()

	if !status3.Cooling || status3.CoolKind != "soft_rate" {
		t.Fatalf("应进入 soft_rate 冷却: %+v", status3)
	}

	if status3.CoolRemaining <= 0 || status3.CoolRemaining > maxSec {
		t.Errorf("cool_remaining_sec=%d want in (0,%d]", status3.CoolRemaining, maxSec)
	}
}

// assertHardCreditCooldown 断言硬积分冷却到次日 04:00（非固定 12h/配置时长）。
func assertHardCreditCooldown(t *testing.T, status4 pool.Status) {
	t.Helper()

	if !status4.Cooling {
		t.Fatalf("bad should be cooling: %+v", status4)
	}

	if status4.Reason != "余额不足" {
		t.Errorf("reason=%q", status4.Reason)
	}

	if status4.Until.In(cst.Loc).Hour() != 4 {
		t.Errorf("until hour(CST)=%d want 4 (next-day 04:00)", status4.Until.In(cst.Loc).Hour())
	}
	// 距次日 04:00 最长 28h（凌晨 00:00~04:00 间运行时 now→次日 04:00 跨度 > 24h，属正常）。
	if d := time.Until(status4.Until); d <= 0 || d > 28*time.Hour {
		t.Errorf("until %v not within (0,28h]: %v", status4.Until, d)
	}
}

// assertModelResetCooldown 断言 6004 带重置时间：账号级不 cooling，模型级单行 until ≈ reset。
func assertModelResetCooldown(t *testing.T, status2 pool.Status, reset time.Time) {
	t.Helper()

	if status2.Cooling {
		t.Fatalf("6004-with-reset should NOT set account-level cooling: %+v", status2)
	}

	if len(status2.RateLimitedModels) != 1 || status2.RateLimitedModels[0].Model != "glm-5.3" {
		t.Fatalf("want single model ledger row glm-5.3: %+v", status2.RateLimitedModels)
	}

	if d := status2.RateLimitedModels[0].Until.Sub(reset); d < -time.Second || d > time.Second {
		t.Errorf("model until=%v want ~reset=%v (diff %v)", status2.RateLimitedModels[0].Until, reset, d)
	}
}

// assertRepairedToolCall 断言 message 已把正文标记还原成单个 tool_call（正文无残留）。
func assertRepairedToolCall(t *testing.T, msg, choice map[string]any, wantContent, wantName, wantArg string) {
	t.Helper()

	if msg["content"] != wantContent {
		t.Errorf("content=%q", msg["content"])
	}

	tcs, _ := msg["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("tool_calls=%#v want 1 个", msg["tool_calls"])
	}

	fn, _ := tcs[0].(map[string]any)["function"].(map[string]any)
	if fn == nil || fn["name"] != wantName {
		t.Fatalf("function=%#v", tcs[0])
	}

	var args map[string]any
	if err := json.Unmarshal([]byte(fn["arguments"].(string)), &args); err != nil {
		t.Fatalf("arguments 不是合法 JSON: %v", fn["arguments"])
	}

	if args["cmd"] != wantArg {
		t.Errorf("cmd=%v", args["cmd"])
	}

	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason=%v want tool_calls", choice["finish_reason"])
	}
}

// assertStatusSummary 断言 /status 的汇总字段与池级字段。
func assertStatusSummary(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()

	var statusBody map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &statusBody); err != nil {
		t.Fatalf("status not json: %v", err)
	}

	if statusBody["total"] != float64(1) || statusBody["healthy"] != float64(1) ||
		statusBody["cooling"] != float64(0) || statusBody["disabled"] != float64(0) ||
		statusBody["in_flight_full"] != float64(0) {
		t.Errorf("summary=%v want total=1 healthy=1 cooling=0 disabled=0 in_flight_full=0", statusBody)
	}

	if statusBody["sticky_sessions"] != float64(0) {
		t.Errorf("sticky_sessions=%v want 0", statusBody["sticky_sessions"])
	}

	if statusBody["redis_mode"] != "noop" {
		t.Errorf("redis_mode=%v want noop", statusBody["redis_mode"])
	}
}

// assertPortrait 断言 /status 单账号健康画像字段。
func assertPortrait(t *testing.T, status pool.Status) {
	t.Helper()

	if !status.Cooling || status.CoolKind != "soft_rate" || status.CoolRemaining <= 0 {
		t.Errorf("cooling portrait=%+v", status)
	}

	if status.SuccessCount != 2 {
		t.Errorf("success_count=%d want 2", status.SuccessCount)
	}

	if status.ErrTotal != 1 {
		t.Errorf("err_total=%d want 1", status.ErrTotal)
	}

	if status.LastSuccessTime.IsZero() {
		t.Error("last_success should be set")
	}

	if status.LastErrTime.IsZero() {
		t.Error("last_err should be set")
	}
}

// TestGlobalInFlightLimit 全局在途满额时在选号前 503，不打上游（issue #92）。
func TestGlobalInFlightLimit(t *testing.T) {
	t.Parallel()

	var calls int

	client := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	state.SetMaxInFlight(0) // 不限单账号，隔离出全局上限
	state.SetMaxInFlightTotal(1)

	if !state.Acquire("u1") {
		t.Fatal("seed acquire failed")
	}

	handler := NewHandler(&Config{Pool: state, Upstream: client})
	body := []byte(`{"model":"glm-5.2","messages":[]}`)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body)))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d body=%s want 503", rec.Code, rec.Body)
	}

	if calls != 0 {
		t.Errorf("upstream calls=%d want 0", calls)
	}

	state.Release("u1")

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("after release code=%d body=%s", rec.Code, rec.Body)
	}

	if calls != 1 {
		t.Errorf("upstream calls=%d want 1", calls)
	}
}

// TestChatLargeBodyUnlimitedWhenZero server.max_body_bytes=0 时不限制：
// 数 MB 的合法 body 完整读入并照常打上游。
// TestChatRequestBodyLimit 超过 server.max_body_bytes 时返回 413，
// 且不消耗选号/上游调用（issue #86）。Content-Length 与 chunked 都覆盖。
func TestChatRequestBodyLimit(t *testing.T) {
	t.Parallel()

	var calls int

	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	handler := NewHandler(&Config{Pool: p, Upstream: up, MaxBodyBytes: 64})
	body := []byte(`{"model":"glm-5.2","messages":[],"pad":"` + strings.Repeat("a", 128) + `"}`)

	for _, chunked := range []bool{false, true} {
		calls = 0

		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
		if chunked {
			req.ContentLength = -1
		}

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("chunked=%v code=%d body=%s", chunked, rec.Code, rec.Body)
		}

		if !strings.Contains(rec.Body.String(), `"payload_too_large"`) {
			t.Errorf("chunked=%v missing payload_too_large: %s", chunked, rec.Body)
		}

		if calls != 0 {
			t.Errorf("chunked=%v upstream calls=%d want 0", chunked, calls)
		}
	}
}

// TestChatRequestBodyAtLimitAllowed 恰好等于上限的请求仍正常处理。
func TestChatRequestBodyAtLimitAllowed(t *testing.T) {
	t.Parallel()

	var calls int

	client := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	body := []byte(`{"model":"glm-5.2","messages":[]}`)
	h := NewHandler(&Config{Pool: p, Upstream: client, MaxBodyBytes: int64(len(body))})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if calls != 1 {
		t.Errorf("upstream calls=%d want 1", calls)
	}
}

func TestChatLargeBodyUnlimitedWhenZero(t *testing.T) {
	t.Parallel()

	var calls int

	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(&Config{Pool: p, Upstream: up})

	pad := strings.Repeat("a", 4<<20) // 4MB 合法 JSON 字符串值
	body := []byte(`{"model":"glm-5.2","messages":[],"pad":"` + pad + `"}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s (large valid body must proceed)", rec.Code, rec.Body)
	}

	if calls != 1 {
		t.Errorf("upstream calls=%d want 1", calls)
	}
}

// TestChatBadParamsFailsFastWithoutPenalty 上游 400 + Unmarshal chat params failed（11101）
// → 请求级错误：不罚账号（无冷却/无禁用/无熔断计数/无 errTotal），**且不轮转**——
// 同一 body 换号必然同样失败。端到端断言只打一次上游、直接回 400、账号完好。
func TestChatBadParamsFailsFastWithoutPenalty(t *testing.T) {
	t.Parallel()

	calls := map[string]int{}
	client := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		return 400, `{"code":11101,"msg":"Unmarshal chat params failed with error: unexpected EOF"}`, false
	})
	state := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	state.SetCredits("bad", 2000, 0) // 确定性源 r=0 → 先选 bad
	state.SetCredits("good", 1000, 0)
	h := NewHandler(&Config{Pool: state, Upstream: client})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s (want 400: request-level error must not be retried on other accounts)", rec.Code, rec.Body)
	}

	if calls["Bearer at-bad"] != 1 || calls["Bearer at-good"] != 0 {
		t.Errorf("calls=%v want bad 1 次、good 0 次（零轮转）", calls)
	}
	// 账号完好：无冷却、无禁用、无熔断计数、无 errTotal。
	st, _ := state.Status("bad")
	if st.Cooling || st.Disabled || st.ErrTotal != 0 || st.BreakerFails != 0 {
		t.Errorf("ErrBadParams must not penalize account: %+v", st)
	}
}

// TestChatBadParams400CarriesUpstreamBody 11101 的 400 响应必须包含上游原始
// 11101 信息（含 requestId，客户端据此排查），且不再出现空洞的 no_healthy_account。
func TestChatBadParams400CarriesUpstreamBody(t *testing.T) {
	t.Parallel()
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, `{"code":11101,"msg":"Unmarshal chat params failed with error: unexpected EOF"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(&Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))

	if rec.Code != 400 {
		t.Fatalf("code=%d body=%s (want 400)", rec.Code, rec.Body)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "11101") || !strings.Contains(body, "Unmarshal chat params failed") {
		t.Errorf("400 message should carry upstream 11101 info: %s", body)
	}

	if strings.Contains(body, "no_healthy_account") {
		t.Errorf("request-level failure must not be reported as account exhaustion: %s", body)
	}
}

func TestChatNonStreamAggregates(t *testing.T) {
	t.Parallel()
	client := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz != "Bearer at1" {
			t.Errorf("auth=%q", authz)
		}

		return 200, sseOK, true
	})
	handler := NewHandler(&Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: client,
	})

	rec := postChat(t, handler, `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}

	if resp["object"] != "chat.completion" {
		t.Errorf("object=%v", resp["object"])
	}

	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "你好" {
		t.Errorf("content=%q", msg["content"])
	}

	assertChatSuccessUsage(t, handler.cfg.Pool, "u1")
}

func TestChatStreamPassthrough(t *testing.T) {
	t.Parallel()
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	handler := NewHandler(&Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})

	rec := postChat(t, handler, `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}

	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("ct=%q", ct)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "你好") || !strings.Contains(body, "data: [DONE]") {
		t.Errorf("body=%q", body)
	}

	assertChatSuccessUsage(t, handler.cfg.Pool, "u1")
}

// TestChatStreamRecordsTTFT 流式成功时 TTFT 采样写入账号摘要与 usage 时序（issue #16）。
func TestChatStreamRecordsTTFT(t *testing.T) {
	t.Parallel()
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	rec := usage.New("")
	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(&Config{Pool: state, Upstream: up, Usage: rec})

	res := postChat(t, h, `{"model":"glm-5.2","stream":true,"messages":[]}`)
	if res.Code != 200 {
		t.Fatalf("code=%d body=%s", res.Code, res.Body)
	}

	if st := statusOf(t, state, "u1"); st.TokenUsage.LastTTFTMs < 1 {
		t.Errorf("last_ttft_ms=%d want >=1", st.TokenUsage.LastTTFTMs)
	}

	if s := rec.Snapshot(24, nil); s.Totals.AvgTTFTMs < 1 {
		t.Errorf("avg_ttft_ms=%v want >=1", s.Totals.AvgTTFTMs)
	}
}

func TestChatRecordsCreditForStreamAndSync(t *testing.T) {
	t.Parallel()

	for _, stream := range []bool{false, true} {
		name := "sync"
		if stream {
			name = "stream"
		}

		t.Run(name, func(t *testing.T) {
			t.Parallel()
			checkCreditRecording(t, stream)
		})
	}
}

// checkCreditRecording 断言流式/非流式下 credit 用量维度被正确记录。
func checkCreditRecording(t *testing.T, stream bool) {
	t.Helper()

	const sseCredit = "data: {\"id\":\"chatcmpl-credit\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"}}],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":6,\"total_tokens\":10,\"credit\":1.25}}\n\n" +
		"data: [DONE]\n\n"

	client := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, sseCredit, true
	})
	rec := usage.New("")
	handler := NewHandler(&Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: client,
		Usage:    rec,
	})

	body := `{"model":"glm-5.2","messages":[]}`
	if stream {
		body = `{"model":"glm-5.2","stream":true,"messages":[]}`
	}

	recorder := postChat(t, handler, body)
	if recorder.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", recorder.Code, recorder.Body)
	}

	assertCreditUsage(t, rec.Snapshot(24, nil))
}

// assertCreditUsage 断言单次请求的 credit 总量与账号/模型维度。
func assertCreditUsage(t *testing.T, snapshot usage.Snapshot) {
	t.Helper()

	if snapshot.Totals.Credits != 1.25 || snapshot.Totals.CreditSamples != 1 || snapshot.Totals.CreditTokens != 10 || snapshot.Totals.CreditsPer1MTokens != 125000 {
		t.Fatalf("usage totals = %+v, want credit=1.25 tokens=10 ratio=125000", snapshot.Totals)
	}

	if len(snapshot.CreditByAccount) != 1 || snapshot.CreditByAccount[0].Key != "u1" ||
		len(snapshot.CreditByModel) != 1 || snapshot.CreditByModel[0].Key != "glm-5.2" {
		t.Fatalf("credit dimensions = %+v / %+v", snapshot.CreditByAccount, snapshot.CreditByModel)
	}
}

func TestChatRotatesOnHardCredit(t *testing.T) {
	t.Parallel()

	calls := map[string]int{}
	client := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		if authz == "Bearer at-bad" {
			return 402, `{"code":1,"msg":"余额不足"}`, false
		}

		return 200, sseOK, true
	})
	state := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	// 让 bad 积分更高被先选中
	state.SetCredits("bad", 2000, 0)
	state.SetCredits("good", 1000, 0)
	h := NewHandler(&Config{Pool: state, Upstream: client, SoftCooldown: time.Minute})

	rec := postChat(t, h, `{"model":"glm-5.2","messages":[]}`)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	assertCalls(t, calls, map[string]int{"Bearer at-bad": 1, "Bearer at-good": 1})

	bad := statusOf(t, state, "bad")
	if !bad.Cooling || bad.Reason == "" {
		t.Errorf("bad account should be cooling: %+v", bad)
	}

	if bad.TokenUsage.RequestCount != 1 || bad.TokenUsage.UsageCount != 0 {
		t.Errorf("bad token usage=%+v", bad.TokenUsage)
	}

	good := statusOf(t, state, "good")
	if good.TokenUsage.RequestCount != 1 || good.TokenUsage.TotalTokens != 2 {
		t.Errorf("good token usage=%+v", good.TokenUsage)
	}
}

// TestChatSoftCoolsOnRateLimitBody 端到端回归 issue #28：上游用非 429 状态码
// （400 + 限流文案）表达模型侧限流时，该账号必须进入 CoolSoft 冷却，而不是只换号。
// 修复前 Classify 归 ErrClient → applyErrorPolicy 走 default 分支只换号不罚，
// 账号留在可用池里，下一个请求仍会被选中。
func TestChatSoftCoolsOnRateLimitBody(t *testing.T) {
	t.Parallel()

	calls := map[string]int{}
	client := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		if authz == "Bearer at-bad" {
			return 400, `{"code":1,"msg":"The model provider is rate-limiting requests. Please wait a moment and try again."}`, false
		}

		return 200, sseOK, true
	})
	state := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	// 让 bad 积分更高被先选中（与 TestChatRotatesOnHardCredit 同一确定性手法）。
	state.SetCredits("bad", 2000, 0)
	state.SetCredits("good", 1000, 0)

	const soft = 45 * time.Second

	handler := NewHandler(&Config{Pool: state, Upstream: client, SoftCooldown: soft})

	rec := postChat(t, handler, `{"model":"glm-5.2","messages":[]}`)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	assertCalls(t, calls, map[string]int{"Bearer at-bad": 1, "Bearer at-good": 1})
	// 冷却时长取自注入的 SoftCooldown，不依赖真实等待。
	assertSoftRateCooling(t, statusOf(t, state, "bad"), int64(soft/time.Second)+int64(soft/time.Second)/10+1)

	// 冷却生效：同一账号在冷却期内不得再被选中。
	before := calls["Bearer at-bad"]

	rec2 := postChat(t, handler, `{"model":"glm-5.2","messages":[]}`)
	if rec2.Code != 200 {
		t.Fatalf("second code=%d body=%s", rec2.Code, rec2.Body)
	}

	if calls["Bearer at-bad"] != before {
		t.Errorf("冷却中的账号不应再次被选中: calls=%v", calls)
	}
}

func TestApplyErrorPolicySoftRateExponentialBackoff(t *testing.T) {
	t.Parallel()
	// 吸收上游语义：带解析重置时间的 body 对齐墙钟；无时间文案走有界退避，且
	// **冷却中的重复触发不堆加**（旧「每次都翻倍」正是全池被推到 2h 封顶的元凶）。
	// 跨冷却期的堆加语义由 pool 包 CooldownSoftRate 测试覆盖；此处锚定 handler 侧
	// 的调用形态：连续 3 次软限流错误 → 首次 600s，后续不延长。
	state := pool.New("")
	state.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(&Config{Pool: state, SoftCooldown: 600 * time.Second})

	for i := 1; i <= 3; i++ {
		h.applyErrorPolicy("u1", upstream.ErrSoftRate, "", "", "", nil)

		status, _ := state.Status("u1")
		if !status.Cooling || status.CoolKind != "soft_rate" {
			t.Fatalf("call %d: 应为 soft_rate 冷却: %+v", i, status)
		}

		if status.SoftStreak != 1 {
			t.Errorf("call %d: 冷却中兜底探测不应推进 soft_streak, got %d", i, status.SoftStreak)
		}

		if status.CoolRemaining < 540 || status.CoolRemaining > 660 {
			t.Errorf("call %d: cool_remaining_sec=%d want ~600±10%%（不堆加）", i, status.CoolRemaining)
		}
	}
}

func TestApplyErrorPolicyNotFoundUsesFixedBase(t *testing.T) {
	t.Parallel()
	// 404 分流：偶发上游 404 的冷却基数固定 60s（notFoundCooldown），不取 soft_rate
	// 的 600s 基数，也不受其配置值影响。Cooldown 重构后 404 是固定时长冷却
	//（不堆加、不喂熔断），成功后自然到期恢复。
	state := pool.New("")
	state.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(&Config{Pool: state, SoftCooldown: 20 * time.Minute}) // soft_rate 配得很大，验证 404 不受其影响

	notFoundSec := int64(notFoundCooldown / time.Second)

	for i := 1; i <= 3; i++ {
		h.applyErrorPolicy("u1", upstream.ErrNotFound, "", "", "", nil)

		status, _ := state.Status("u1")
		if !status.Cooling || status.CoolKind != "soft_rate" {
			t.Fatalf("call %d: 应为 soft 冷却: %+v", i, status)
		}

		if status.SoftStreak != 0 {
			t.Errorf("call %d: 404 固定冷却不应推进 soft_streak, got %d", i, status.SoftStreak)
		}
		// 基数取自 notFoundCooldown（60s）而非注入的 soft_rate（20m），且重复触发不延长。
		if status.CoolRemaining < notFoundSec-3 || status.CoolRemaining > notFoundSec {
			t.Errorf("call %d: 404 cool_remaining_sec=%d want ~%d（固定基数 %ds，非 soft_rate）",
				i, status.CoolRemaining, notFoundSec, notFoundSec)
		}
	}
}

// TestNewHandlerSoftCooldownDefault 端到端：未注入 SoftCooldown 时基数回落到 600s（原为 60s）。
func TestNewHandlerSoftCooldownDefault(t *testing.T) {
	t.Parallel()
	client := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-bad" {
			return 429, `{"code":1,"msg":"rate limit"}`, false
		}

		return 200, sseOK, true
	})
	state := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	state.SetCredits("bad", 2000, 0)
	state.SetCredits("good", 1000, 0)
	h := NewHandler(&Config{Pool: state, Upstream: client}) // 不注入 SoftCooldown

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	status, _ := state.Status("bad")
	if !status.Cooling || status.CoolKind != "soft_rate" {
		t.Fatalf("bad 应进入 soft_rate 冷却: %+v", status)
	}

	if status.CoolRemaining < 540 || status.CoolRemaining > 660 {
		t.Errorf("default soft cooldown cool_remaining_sec=%d want 600±10%%", status.CoolRemaining)
	}
}

// TestChatStickyFollowsFinalSuccess 端到端验证 D4：粘性号失败换号成功后，会话绑定收敛到成功号。
func TestChatStickyFollowsFinalSuccess(t *testing.T) {
	t.Parallel()

	bindStore2 := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     bindStore2,
		Available: func() []string { return []string{"bad", "good"} },
	})
	state := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	// 先把会话预绑定到 bad（模拟历史粘性），bad 失败、good 成功 → 绑定应切到 good。
	client := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-bad" {
			return 500, `{"code":500}`, false
		}

		return 200, sseOK, true
	})
	handler := NewHandler(&Config{
		Pool:         state,
		Upstream:     client,
		Session:      sess,
		SoftCooldown: time.Minute,
	})
	// 预绑定：sess.Bind("conv-1", "bad")，然后请求体带同 conversation_id。
	sess.Bind("conv-1", "bad")

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[],"metadata":{"conversation_id":"conv-1"}}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	// 绑定必须收敛到最终成功的 good。
	if uid, ok := bindStore2.lastUID("conv-1"); !ok || uid != "good" {
		t.Fatalf("sticky binding should follow final success to good, got %s ok=%v (binds=%v)", uid, ok, bindStore2.binds)
	}
}

// TestChatStickySuccessKeepsBinding 粘性号直接成功 → 绑定不变（仍为该号）。
func TestChatStickySuccessKeepsBinding(t *testing.T) {
	t.Parallel()

	bindStore2 := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     bindStore2,
		Available: func() []string { return []string{"good"} },
	})
	p := testPoolWith(&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(&Config{Pool: p, Upstream: up, Session: sess, SoftCooldown: time.Minute})
	sess.Bind("conv-1", "good")

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[],"metadata":{"conversation_id":"conv-1"}}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if uid, ok := bindStore2.lastUID("conv-1"); !ok || uid != "good" {
		t.Fatalf("binding should stay good after success, got %s ok=%v", uid, ok)
	}
}

// TestChatStickyFullFallsBackToRotation 端到端验证 C3 语义：粘性号满载不可用时，
// 请求在同一轮内解绑并回落普通轮换选中健康账号，绑定收敛到最终成功号——而非空耗一轮。
func TestChatStickyFullFallsBackToRotation(t *testing.T) {
	t.Parallel()

	bindStore2 := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     bindStore2,
		Available: func() []string { return []string{"bad", "good"} },
	})
	state := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	// bad 占满唯一在途名额：PickByUID 将返回 nil（healthy 但 inFlight 满）→ 解绑 + 回落轮换。
	state.SetMaxInFlight(1)
	state.Acquire("bad")

	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	handler := NewHandler(&Config{
		Pool:         state,
		Upstream:     up,
		Session:      sess,
		SoftCooldown: time.Minute,
	})
	sess.Bind("conv-1", "bad")

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[],"metadata":{"conversation_id":"conv-1"}}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	// 满载粘性号被解绑，绑定收敛到最终成功号 good。
	if uid, ok := bindStore2.lastUID("conv-1"); !ok || uid != "good" {
		t.Fatalf("sticky binding should fall back to good, got %s ok=%v (binds=%v)", uid, ok, bindStore2.binds)
	}

	state.Release("bad")
}

func TestChatHardCreditCooldownUntilNextDay4AM(t *testing.T) {
	t.Parallel()
	client := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-bad" {
			return 402, `{"code":1,"msg":"余额不足"}`, false
		}

		return 200, sseOK, true
	})
	state := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	state.SetCredits("bad", 2000, 0) // bad 积分高，确定性源 → 先被选中
	state.SetCredits("good", 1000, 0)
	h := NewHandler(&Config{Pool: state, Upstream: client})

	rec := postChat(t, h, `{"model":"glm-5.2","messages":[]}`)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	// 硬信贷冷却必须是次日 04:00，而不是固定 12h/配置时长。
	assertHardCreditCooldown(t, statusOf(t, state, "bad"))
	// 立即换号成功：good 被选中。
	good := statusOf(t, state, "good")
	if good.Cooling || good.Disabled {
		t.Errorf("good should stay healthy: %+v", good)
	}
}

func TestChat429Code14018UsesHardCreditCooldown(t *testing.T) {
	t.Parallel()
	client := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-bad" {
			return 429, `{"code":14018,"msg":"Credits exhausted"}`, false
		}

		return 200, sseOK, true
	})
	state := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	state.SetCredits("bad", 2000, 2000)
	state.SetCredits("good", 1000, 1000)
	h := NewHandler(&Config{Pool: state, Upstream: client, SoftCooldown: time.Minute})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}

	status, ok := state.Status("bad")
	if !ok || !status.Cooling {
		t.Fatalf("14018 account should be cooling: %+v ok=%v", status, ok)
	}

	if status.Reason != "余额不足" || status.Until.In(cst.Loc).Hour() != 4 {
		t.Fatalf("14018 should use hard-credit cooldown, got reason=%q until=%v", status.Reason, status.Until)
	}

	if got := state.Pick(); got == nil || got.UID != "good" {
		t.Fatalf("hard-cooled 14018 account must not be fallback-picked, got %+v", got)
	}
}

// TestChat6004ModelResetCoolsToParsedTime 端到端回归 issue #31：上游 429 + code 6004
// +「将在 … 重置」→ 冷却 until 精确等于解析时间（而非 600s 固定基数/指数退避），
// 且记录触发模型 → 同模型请求仍被冷却、切模型请求按豁免可选。
func TestChat6004ModelResetCoolsToParsedTime(t *testing.T) {
	t.Parallel()
	// 用未来 5 分钟的重置时间（wall-clock）构造上游响应。
	reset := time.Now().Add(5 * time.Minute)
	str := reset.In(cst.Loc).Format("2006-01-02 15:04:05")

	var calls int

	client := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++

		if authz == "Bearer at-bad" {
			return 429, `{"code":6004,"msg":"将在 ` + str + ` UTC+8 重置"}`, false
		}

		return 200, sseOK, true
	})
	state := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	state.SetCredits("bad", 2000, 0)
	state.SetCredits("good", 1000, 0)
	// 隔离对 breaker 的干扰：熔断阈值默认 3，一次失败不触发。
	h := NewHandler(&Config{Pool: state, Upstream: client})

	rec := postChat(t, h, `{"model":"glm-5.3","messages":[]}`)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s (want 200 after rotate to good)", rec.Code, rec.Body)
	}
	// bad 已进入 6004 模型级独立冷却：账号级不 cooling，台账单行 until ≈ reset。
	assertModelResetCooldown(t, statusOf(t, state, "bad"), reset)
	// 记录触发模型（bad 池内 private 字段需经 Status 不可见，改用行为断言）：
	// 同模型 glm-5.3 的请求不应选中 bad（仍冷却）；
	// 不同模型 hy3-x 的请求应豁免冷却选中 bad（最高分）。
	state.SetRandomSource(func(n int64) int64 { return 0 })

	if same := state.PickExcludingForModel(nil, "glm-5.3"); same == nil || same.UID != "good" {
		t.Fatalf("same-model pick should skip bad (still cooling), got %+v", same)
	}

	if diff := state.PickExcludingForModel(nil, "hy3-x"); diff == nil || diff.UID != "bad" {
		t.Fatalf("different-model pick should bypass bad soft cooling, got %+v", diff)
	}
}

// TestChat6004WithoutResetFallsBackToBackoff 6004 无时间文案 → 退回 600s 基数软冷却
// （无法确认是模型级还是套餐额度耗尽，保守按账号级）。
func TestChat6004WithoutResetFallsBackToBackoff(t *testing.T) {
	t.Parallel()

	var calls int

	client := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++

		if authz == "Bearer at-bad" {
			return 429, `{"code":6004,"msg":"model usage limit exceeded"}`, false
		}

		return 200, sseOK, true
	})
	state := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	state.SetCredits("bad", 2000, 0)
	state.SetCredits("good", 1000, 0)
	h := NewHandler(&Config{Pool: state, Upstream: client, SoftCooldown: time.Minute})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.3","messages":[]}`)))

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	status, _ := state.Status("bad")
	if !status.Cooling || status.CoolKind != "soft_rate" {
		t.Fatalf("bad should be soft cooling: %+v", status)
	}
	// 冷却时长 = 注入 soft 基数(60s)，非解析时间（无重置文案）。
	if status.CoolRemaining <= 0 || status.CoolRemaining > 66 {
		t.Errorf("cool_remaining_sec=%d want ~60±10%% (soft base, not parsed)", status.CoolRemaining)
	}

	if len(status.RateLimitedModels) != 1 || status.RateLimitedModels[0].Model != "glm-5.3" || status.RateLimitedModels[0].Kind != "rate_limit" {
		t.Fatalf("rate-limited models=%+v, want audit row for glm-5.3", status.RateLimitedModels)
	}
}

// TestChatNonStreamErrorFrame 上游 HTTP 200 + error 帧不能被聚合成空成功；
// 非流式路径应按错误分类返回错误信封并冷却账号/模型。
func TestChatNonStreamErrorFrame(t *testing.T) {
	t.Parallel()

	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, "data: {\"error\":{\"code\":6004,\"message\":\"模型限流，将在 2026-09-27 01:00:00 重置\"}}\n\n" +
			"data: [DONE]\n\n", true
	})
	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	handler := NewHandler(&Config{Pool: state, Upstream: up, MaxRotate: 1})

	rec := postChat(t, handler, `{"model":"glm-5.2","messages":[]}`)
	if rec.Code != http.StatusTooManyRequests && rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if !strings.Contains(rec.Body.String(), "6004") && !strings.Contains(rec.Body.String(), "rate") {
		t.Errorf("error body lost upstream reason: %s", rec.Body)
	}
}

func TestChatAllUnavailableReturns503(t *testing.T) {
	t.Parallel()
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 402, `{"code":1,"msg":"余额不足"}`, false
	})
	handler := NewHandler(&Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 503 {
		t.Errorf("code=%d body=%s", rec.Code, rec.Body)
	}

	var e map[string]any
	json.Unmarshal(rec.Body.Bytes(), &e)

	if e["error"] == nil {
		t.Errorf("want error envelope: %s", rec.Body)
	}
}

func TestChatSessionDeadDisablesAtThreshold(t *testing.T) {
	t.Parallel()
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 401, `{"code":12153,"msg":"Offline user session not found"}`, false
	})
	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	handler := NewHandler(&Config{Pool: state, Upstream: up})

	// Issue #245：一次 401 只进入会话失效计数（面板显示待重登），连续达到阈值才禁用。
	for i := range 3 {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != 503 {
			t.Errorf("attempt %d code=%d", i, rec.Code)
		}
	}

	st, _ := state.Status("u1")
	if !st.Disabled {
		t.Errorf("account should be disabled after threshold: %+v", st)
	}
}

func TestChatTransportErrorDoesNotPenalize(t *testing.T) {
	t.Parallel()

	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	client := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return nil, errConnectionRefused
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	// 传输错误不喂熔断计数：一次 transport error 不应累计 errTotal 也不应熔断。
	h := NewHandler(&Config{Pool: state, Upstream: client})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))

	if rec.Code != 503 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	st, _ := state.Status("u1")
	if st.Cooling || st.ErrTotal != 0 {
		t.Fatalf("transport error should not penalize account: %+v", st)
	}
}

func TestChatHTTP5xxPenalizes(t *testing.T) {
	t.Parallel()

	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	// 熔断阈值 1：一次 5xx 即触发熔断（连续失败语义并入熔断器）。
	state.SetBreaker(1, time.Hour, time.Hour)

	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 500, `{"code":500}`, false
	})
	h := NewHandler(&Config{Pool: state, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))

	if rec.Code != 503 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	st, _ := state.Status("u1")
	if !st.Cooling {
		t.Fatalf("http 5xx should trip breaker (cooling) with threshold=1: %+v", st)
	}
}

func TestChatHTTP4xxClientDoesNotPenalize(t *testing.T) {
	t.Parallel()

	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, `{"code":400,"msg":"bad request"}`, false
	})
	h := NewHandler(&Config{Pool: state, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))

	if rec.Code != 503 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	st, _ := state.Status("u1")
	if st.Cooling || st.ErrTotal != 0 {
		t.Fatalf("generic 4xx should not penalize account: %+v", st)
	}
}

func TestModelsEndpoint(t *testing.T) {
	t.Parallel()

	// 每条都带权威 context/maxOutput（查找链第 1 级）：未知模型（如 gpt-6-sol）
	// 缺这两个值会让四级查找链在后台拉真实 models.dev；用例耗时随网络抖动，
	// goleak 在 make ci 的并行负载下可能抓到尚未退出的拉取 goroutine。
	cache := modelCatalog([]upstream.ModelInfo{
		{ID: "glm-5.2", ContextWindow: 1_000_000, MaxTokens: 131_072},
		{ID: "glm-5", ContextWindow: 1_000_000, MaxTokens: 131_072},
		{ID: "gpt-6-sol", ContextWindow: 1_000_000, MaxTokens: 131_072},
		{ID: "deepseek-v4-flash", ContextWindow: 1_000_000, MaxTokens: 384_000},
		{ID: "hy4", ContextWindow: 200_000, MaxTokens: 64_000},
	})
	handler := NewHandler(&Config{
		Pool:        testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream:    upstream.New(),
		modelsCache: cache,
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}

	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)

	if resp["object"] != "list" {
		t.Errorf("object=%v", resp["object"])
	}

	data := resp["data"].([]any)
	if len(data) < 5 {
		t.Errorf("models count=%d", len(data))
	}

	found := false

	for _, m := range data {
		if m.(map[string]any)["id"] == "cn:glm-5.2" {
			found = true
		}
	}

	if !found {
		t.Error("cn:glm-5.2 missing")
	}
}

func TestModelsDynamic(t *testing.T) {
	t.Parallel()

	cache := &modelsCache{}

	// 假上游返回动态模型（含 agents + maxInputTokens/maxOutputTokens + reasoning 档位）
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, `{"code":0,"data":{"models":[{"id":"dyn-model-a","maxInputTokens":65536,"maxOutputTokens":8192,"reasoning":{"effort":"medium","supportedEfforts":["low","medium","high"]}},{"id":"dyn-model-b","maxInputTokens":131072,"maxOutputTokens":16384},{"id":"glm-9.9","maxInputTokens":262144,"maxOutputTokens":32768}],"agents":[{"name":"cli","models":["dyn-model-a","dyn-model-b","glm-9.9"]}]}}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(&Config{Pool: p, Upstream: up, modelsCache: cache})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}

	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)

	data := resp["data"].([]any)
	if len(data) != 3 {
		t.Fatalf("want 3 dynamic models, got %d: %v", len(data), data)
	}

	ids := map[string]bool{}
	for _, m := range data {
		ids[m.(map[string]any)["id"].(string)] = true
	}

	if !ids["cn:dyn-model-a"] || !ids["cn:glm-9.9"] {
		t.Errorf("dynamic ids missing: %v", ids)
	}
	// 断言字段映射：maxInputTokens → context_length，maxOutputTokens → max_output_tokens
	assertDynamicModelFields(t, data)

	// 第二次调用走缓存（把上游关掉也成功）
	cache.RLock()
	cached := len(cache.ids)
	cache.RUnlock()

	if cached != 3 {
		t.Errorf("cache not populated: %d", cached)
	}
}

// assertDynamicModelFields 按模型 id 断言字段映射（context_length / max_output_tokens / effort 档位）。
func assertDynamicModelFields(t *testing.T, data []any) {
	t.Helper()

	for _, m := range data {
		table := m.(map[string]any)
		switch table["id"] {
		case "dyn-model-a":
			assertDynModelA(t, table)
		case "dyn-model-b":
			assertDynModelB(t, table)
		case "glm-9.9":
			assertGLM99(t, table)
		}
	}
}

// assertDynModelA 断言 dyn-model-a 的长度映射与 reasoning 档位透出。
func assertDynModelA(t *testing.T, table3 map[string]any) {
	t.Helper()

	if table3["context_length"].(float64) != 65536 {
		t.Errorf("dyn-model-a context_length=%v want 65536", table3["context_length"])
	}

	if table3["max_output_tokens"].(float64) != 8192 {
		t.Errorf("dyn-model-a max_output_tokens=%v want 8192", table3["max_output_tokens"])
	}
	// reasoning 档位透出：supported_efforts + default_effort
	efforts, _ := table3["supported_efforts"].([]any)
	if len(efforts) != 3 || efforts[0] != "low" {
		t.Errorf("dyn-model-a supported_efforts=%v", table3["supported_efforts"])
	}

	if table3["default_effort"] != "medium" {
		t.Errorf("dyn-model-a default_effort=%v want medium", table3["default_effort"])
	}
}

// assertDynModelB 断言上游未返回 reasoning → 两个档位字段都省略（客户端按自身默认）。
func assertDynModelB(t *testing.T, table map[string]any) {
	t.Helper()

	if _, has := table["supported_efforts"]; has {
		t.Errorf("dyn-model-b supported_efforts should be omitted, got %v", table["supported_efforts"])
	}

	if _, has := table["default_effort"]; has {
		t.Errorf("dyn-model-b default_effort should be omitted")
	}
}

// assertGLM99 断言 glm-9.9 的长度映射。
func assertGLM99(t *testing.T, table2 map[string]any) {
	t.Helper()

	if table2["context_length"].(float64) != 262144 {
		t.Errorf("glm-9.9 context_length=%v want 262144", table2["context_length"])
	}

	if table2["max_output_tokens"].(float64) != 32768 {
		t.Errorf("glm-9.9 max_output_tokens=%v want 32768", table2["max_output_tokens"])
	}
}

func TestModelsDynamicFallsBackToStatic(t *testing.T) {
	t.Parallel()

	cache := &modelsCache{}
	// 纯动态化（产品决策）：上游失败 → 空列表（200），不再回落静态表——
	// 拉不出目录即意味着上游不可用，假名单只会让客户端选到 11102 的模型。
	cache.Lock()
	cache.ids = nil
	cache.fetched = time.Time{}
	cache.lastFail = time.Time{}
	cache.Unlock()

	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 500, `boom`, false
	})
	h := NewHandler(&Config{Pool: p, Upstream: up, modelsCache: cache})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}

	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)

	data := resp["data"].([]any)
	if len(data) != 0 {
		t.Errorf("pure-dynamic failure should yield empty list, got %d", len(data))
	}
}

func TestModelsFetchFailurePenalizesAccount(t *testing.T) {
	t.Parallel()

	cache := &modelsCache{}
	// 吸收上游 9832283：models 拉取失败只进负缓存，不 NoteError——
	// NoteError 喂的是 chat 熔断器，models 端点 5xx 跨界惩罚 chat 通道健康号。
	cache.Lock()
	cache.ids = nil
	cache.fetched = time.Time{}
	cache.lastFail = time.Time{}
	cache.Unlock()

	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	state.SetBreaker(1, time.Hour, time.Hour) // 熔断阈值 1：若仍罚号，一次 fetch 失败即熔断

	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 500, `boom`, false
	})
	h := NewHandler(&Config{Pool: state, Upstream: up, modelsCache: cache})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}

	st, _ := state.Status("u1")
	if st.Cooling {
		t.Fatalf("models fetch failure must not trip chat breaker: %+v", st)
	}
}

func TestModelsNegativeCacheOnFetchFailure(t *testing.T) {
	t.Parallel()

	cache := &modelsCache{}
	cache.Lock()
	cache.ids = nil
	cache.fetched = time.Time{}
	cache.lastFail = time.Time{}
	cache.Unlock()

	var calls atomic.Int32 // FetchModels 企业/v3 两路并发探测回调，计数须原子

	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls.Add(1)
		return 500, `boom`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	handler := NewHandler(&Config{Pool: p, Upstream: up, modelsCache: cache})

	// 连续 3 次请求，上游持续 500 → 只应触发 1 次 fetch（负缓存生效），
	// 纯动态下空列表仍返回 200。
	for i := range 3 {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))

		if rec.Code != 200 {
			t.Fatalf("req %d: code=%d body=%s", i, rec.Code, rec.Body)
		}
	}
	// 一轮探测 = 4 次上游调用（企业 /console + /v2 兜底 + 桌面/IDE 两路 /v3，全部失败才进负缓存）。
	if got := calls.Load(); got != 4 {
		t.Errorf("want 4 probes (console + v2 + desktop/IDE v3), got %d", got)
	}

	// 冷却期结束（把失败时间戳拨回 10 分钟前）→ 应重新 fetch。
	cache.Lock()
	cache.lastFail = time.Now().Add(-10 * time.Minute)
	cache.Unlock()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	if rec.Code != 200 {
		t.Fatalf("after cooldown: code=%d", rec.Code)
	}

	if got := calls.Load(); got != 8 {
		t.Errorf("want 8 probes after cooldown (2 rounds x 4), got %d", got)
	}
}

func TestAPIKeyAuth(t *testing.T) {
	t.Parallel()

	handler := NewHandler(&Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: upstream.New(),
		APIKey:   "secret",
	})
	// 无 key
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 401 {
		t.Errorf("no key: code=%d", rec.Code)
	}
	// 错 key
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer wrong")

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 401 {
		t.Errorf("wrong key: code=%d", rec.Code)
	}
	// 对 key（请求会继续打到上游，但此处上游 client 会失败 —— 只要不是 401 就行）
	req = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer secret")

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Errorf("right key: code=%d", rec.Code)
	}
}

// TestNamedKeyAuth 命名密钥只作用于 /v1/*：接受于 models/chat，拒绝于 /status 与
// /metrics（运维面只认默认密钥）；默认密钥照旧；撤销后立即失效；命中时计入该密钥的请求数。
func TestNamedKeyAuth(t *testing.T) {
	t.Parallel()

	store := apikey.New("")

	rec, raw, err := store.Create("client-a")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	handler := NewHandler(&Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: upstream.New(),
		APIKey:   "secret",
		Keys:     store,
	})
	do := func(method, path, token string) int {
		req := httptest.NewRequest(method, path, strings.NewReader(`{}`))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}

		rw := httptest.NewRecorder()
		handler.ServeHTTP(rw, req)

		return rw.Code
	}

	cases := []struct {
		name, method, path, token string
		want                      int
	}{
		{"命名密钥可用于 /v1/models", http.MethodGet, "/v1/models", raw, 200},
		{"命名密钥不可用于 /status", http.MethodGet, "/status", raw, 401},
		{"命名密钥不可用于 /metrics", http.MethodGet, "/metrics", raw, 401},
		{"默认密钥可用于 /status", http.MethodGet, "/status", "secret", 200},
		{"默认密钥可用于 /metrics", http.MethodGet, "/metrics", "secret", 200},
		{"未知密钥被拒", http.MethodGet, "/v1/models", "sk-wb-nope", 401},
	}
	for _, c := range cases {
		if code := do(c.method, c.path, c.token); code != c.want {
			t.Errorf("%s: code=%d, want %d", c.name, code, c.want)
		}
	}
	// 仅那次成功的命名密钥请求计入用量（默认密钥与 401 不计）。
	if got := store.List()[0].Requests; got != 1 {
		t.Errorf("named key requests = %d, want 1", got)
	}

	if ok, err := store.Revoke(rec.ID); err != nil || !ok {
		t.Fatalf("revoke: %v, %v", ok, err)
	}

	if code := do(http.MethodGet, "/v1/models", raw); code != 401 {
		t.Errorf("revoked key on /v1/models: code=%d, want 401", code)
	}
}

// TestNamedKeyRequiredWhenDefaultEmpty 默认密钥为空但存在命名密钥时，/v1/* 必须鉴权。
func TestNamedKeyRequiredWhenDefaultEmpty(t *testing.T) {
	t.Parallel()

	store := apikey.New("")
	_, raw, _ := store.Create("client")
	handler := NewHandler(&Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: upstream.New(),
		Keys:     store,
	})
	// 无 token → 401
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	responseRecorder := httptest.NewRecorder()
	handler.ServeHTTP(responseRecorder, req)

	if responseRecorder.Code != 401 {
		t.Errorf("no token with keys present: code=%d, want 401", responseRecorder.Code)
	}
	// 正确命名密钥 → 200
	req = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+raw)

	responseRecorder = httptest.NewRecorder()
	handler.ServeHTTP(responseRecorder, req)

	if responseRecorder.Code != 200 {
		t.Errorf("valid named key: code=%d, want 200", responseRecorder.Code)
	}
}

func TestStatusEndpoint(t *testing.T) {
	t.Parallel()

	p := testPoolWith(&auth.Auth{UID: "u1", Nickname: "nick", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetCredits("u1", 42, 0)
	h := NewHandler(&Config{Pool: p, Upstream: upstream.New()})
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}

	body := rec.Body.String()
	if !strings.Contains(body, `"uid":"u1"`) || !strings.Contains(body, `"credits":42`) {
		t.Errorf("body=%s", body)
	}

	if strings.Contains(body, "AccessToken") || strings.Contains(body, `"at"`) {
		t.Error("token leaked in status output")
	}
	// Phase 3 汇总字段 + Phase v3 池级 sticky_sessions / redis_mode。
	assertStatusSummary(t, rec)
}

// TestStatusInFlightFull /status 透出满载计数：healthy 且占满在途的账号数。
func TestStatusInFlightFull(t *testing.T) {
	t.Parallel()

	state := testPoolWith(
		&auth.Auth{UID: "full", AccessToken: "at", ExpiresAt: 9999999999},
		&auth.Auth{UID: "free", AccessToken: "at", ExpiresAt: 9999999999},
	)
	state.SetMaxInFlight(1)

	state.Acquire("full")
	defer state.Release("full")

	h := NewHandler(&Config{Pool: state, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	var statusBody map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &statusBody); err != nil {
		t.Fatalf("status not json: %v", err)
	}

	if statusBody["in_flight_full"] != float64(1) {
		t.Errorf("in_flight_full=%v want 1", statusBody["in_flight_full"])
	}

	if statusBody["healthy"] != float64(2) {
		t.Errorf("healthy=%v want 2 (full is still healthy by state-machine semantics)", statusBody["healthy"])
	}
}

func TestStatusPortraitFields(t *testing.T) {
	t.Parallel()
	// Phase 3：/status 单账号需返回健康画像字段。
	p := testPoolWith(&auth.Auth{UID: "u1", Nickname: "nick", AccessToken: "at", ExpiresAt: 9999999999})
	p.NoteSuccess("u1")
	p.NoteSuccess("u1")
	p.NoteError("u1") // 记录 last_err + err_total（累计，不冷却）
	p.Cooldown("u1", pool.CoolSoft, time.Hour, "429 rate limit")
	h := NewHandler(&Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	var body struct {
		Accounts []pool.Status `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("status not json: %v", err)
	}

	if len(body.Accounts) != 1 {
		t.Fatalf("accounts=%d", len(body.Accounts))
	}

	assertPortrait(t, body.Accounts[0])
}

// TestHealthzEmptyPool 空池（healthy=0）→ 503，表示暂不可服务。
func TestHealthzEmptyPool(t *testing.T) {
	t.Parallel()

	h := NewHandler(&Config{Pool: pool.New(""), Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code=%d want 503 (healthy=0)", rec.Code)
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("healthz not json: %v body=%s", err, rec.Body)
	}

	if resp["healthy"] != float64(0) || resp["total"] != float64(0) {
		t.Errorf("healthz json=%v", resp)
	}
}

// TestHealthz503WhenNoHealthy 所有账号禁用/冷却 → 503。
func TestHealthz503WhenNoHealthy(t *testing.T) {
	t.Parallel()

	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.Disable("u1", "session dead")
	h := NewHandler(&Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503", rec.Code)
	}

	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("ct=%q want json", ct)
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("healthz not json: %v body=%s", err, rec.Body)
	}

	if resp["healthy"] != float64(0) || resp["total"] != float64(1) {
		t.Errorf("healthz json=%v want healthy=0 total=1", resp)
	}
}

// TestHealthz503WhenAllInFlightFull 全部账号 healthy 但都占满在途 → 503（与 chat 同口径），
// 且 healthy 语义未变（仍为 1）：满载不是状态机健康维度的变化，是探活口径单独叠加。
func TestHealthz503WhenAllInFlightFull(t *testing.T) {
	t.Parallel()

	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	state.SetMaxInFlight(1)

	state.Acquire("u1") // 占满唯一在途名额
	defer state.Release("u1")

	if state.ServableNow() {
		t.Fatal("servable should be false when the only healthy account is in-flight full")
	}

	h := NewHandler(&Config{Pool: state, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503 (healthy but all in-flight full)", rec.Code)
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("healthz not json: %v body=%s", err, rec.Body)
	}
	// healthy 语义未变：账号仍是 healthy（只占满在途，非冷却/禁用）。
	if resp["healthy"] != float64(1) || resp["total"] != float64(1) {
		t.Errorf("healthz json=%v want healthy=1 total=1 (healthy semantics unchanged)", resp)
	}
}

// TestHealthz200WhenHealthy 有健康账号 → 200。
func TestHealthz200WithHealthy(t *testing.T) {
	t.Parallel()

	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(&Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200", rec.Code)
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("healthz not json: %v body=%s", err, rec.Body)
	}

	if resp["healthy"] != float64(1) || resp["total"] != float64(1) {
		t.Errorf("healthz json=%v want healthy=1 total=1", resp)
	}
}

// TestHealthzServiceIdentity /healthz 无论 200 还是 503 都必须带网关身份标识
// （响应体 service 字段 + X-Service 头）：宿主探测打到同端口的旧服务/其他服务时，
// 对方即使返回 2xx 也不带本标识，宿主据此判"假成功"。
func TestHealthzServiceIdentity(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		setup    func(*pool.Pool)
		wantCode int
	}{
		{"healthy", func(*pool.Pool) {}, http.StatusOK},
		{"unhealthy", func(p *pool.Pool) { p.Disable("u1", "session dead") }, http.StatusServiceUnavailable},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
			testCase.setup(p)
			h := NewHandler(&Config{Pool: p, Upstream: upstream.New()})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

			if rec.Code != testCase.wantCode {
				t.Fatalf("code=%d want %d", rec.Code, testCase.wantCode)
			}

			if got := rec.Header().Get("X-Service"); got != ServiceName {
				t.Errorf("X-Service=%q want %q", got, ServiceName)
			}

			var resp map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("healthz not json: %v body=%s", err, rec.Body)
			}

			if resp["service"] != ServiceName {
				t.Errorf("service=%v want %q", resp["service"], ServiceName)
			}
		})
	}
}

// TestHealthzServiceIdentityWithoutAuth /healthz 保持无鉴权（负载均衡友好）：
// 配了 api_key 也不要求 Bearer，身份字段照常返回。
func TestHealthzServiceIdentityWithoutAuth(t *testing.T) {
	t.Parallel()

	handler := NewHandler(&Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: upstream.New(),
		APIKey:   "secret",
	})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("healthz must stay unauthenticated: code=%d", rec.Code)
	}

	if got := rec.Header().Get("X-Service"); got != ServiceName {
		t.Errorf("X-Service=%q want %q", got, ServiceName)
	}
}

func TestStatusRequiresAuth(t *testing.T) {
	t.Parallel()

	p := testPoolWith(&auth.Auth{UID: "u1", Nickname: "nick", AccessToken: "at", ExpiresAt: 9999999999})
	handler := NewHandler(&Config{Pool: p, Upstream: upstream.New(), APIKey: "secret"})

	// 无 token → 401
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))

	if rec.Code != 401 {
		t.Errorf("no token: code=%d", rec.Code)
	}

	// 带 token → 200
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	req.Header.Set("Authorization", "Bearer secret")
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Errorf("with token: code=%d", rec.Code)
	}

	// /healthz 无鉴权仍 200
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != 200 {
		t.Errorf("healthz: code=%d", rec.Code)
	}
}

// TestContentBlockedTriggersDegradedRetry passthrough 模式下首请求 400（11-128 文案）
// → 降级重试（Degraded）→ 200，客户端无感。验证第二次出站 body 为 Degraded。
func TestContentBlockedTriggersDegradedRetry(t *testing.T) {
	t.Parallel()
	// 记录每次出站请求体，断言第二次为 Degraded 文本。
	var bodies [][]byte

	client := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			bodies = append(bodies, raw)
			// 首次（body 含原始 system）返回 400 内容拦截；后续返回 200。
			if len(bodies) == 1 {
				return &http.Response{
					StatusCode: http.StatusBadRequest,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"code":"11-128","msg":"blocked by security policy"}`)),
				}, nil
			}

			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(&Config{Pool: p, Upstream: client, PromptMode: "passthrough"})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"system","content":"原始指纹"},{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s (want 200 after degraded retry)", rec.Code, rec.Body)
	}

	if len(bodies) != 2 {
		t.Fatalf("want 2 upstream calls (first 400 + retry), got %d", len(bodies))
	}
	// 第二次出站 body 的 messages 头部 system 内容应为 Degraded 文本。
	if !strings.Contains(string(bodies[1]), prompt.Degraded) {
		t.Errorf("second body should contain Degraded prompt: %s", bodies[1])
	}

	if strings.Contains(string(bodies[1]), "原始指纹") {
		t.Errorf("second body should not contain original system: %s", bodies[1])
	}
}

// TestContentBlockedStickyDegraded 降级后新请求直达 Degraded（不再先撞 400）。
func TestContentBlockedStickyDegraded(t *testing.T) {
	t.Parallel()

	var firstCall bool

	client := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if !firstCall {
			firstCall = true
			return 400, `{"code":"11-128","msg":"blocked by security policy"}`, false
		}

		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	handler := NewHandler(&Config{Pool: p, Upstream: client, PromptMode: "passthrough"})

	// 首请求触发降级 → 200。
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"system","content":"x"},{"role":"user","content":"hi"}]}`)))

	if rec.Code != 200 {
		t.Fatalf("first req code=%d", rec.Code)
	}
	// 降级粘性：新请求 Active()=true，body 已被 Rewrite(Degraded)，上游首字节即 200。
	// 但 fake 上游只对 firstCall 返回 400，之后都 200，无法区分"直达"与"重试"。
	// 用 degrade.Active() 直接断言粘性生效。
	if !handler.degrade.Active() {
		t.Fatal("degrade should be active after trigger")
	}
}

// TestContentBlockedCustomModeDoesNotDegrade custom 模式不触发降级重试
// （custom 已用自有提示词替换，不应再有 system 来源误报）；仍拦则直接回
// 400 content_blocked 防火墙文案（不轮转、不暴露账号/上游错误码）。
func TestContentBlockedCustomModeDoesNotDegrade(t *testing.T) {
	t.Parallel()
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, `{"code":"11-128","msg":"blocked by security policy"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	handler := NewHandler(&Config{Pool: p, Upstream: up, PromptMode: "custom", PromptText: "SYS"})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"system","content":"old"},{"role":"user","content":"hi"}]}`)))
	// custom 模式下仍拦 → 400 content_blocked（内容终态，换号无意义），不降级重试。
	if rec.Code != 400 {
		t.Fatalf("code=%d want 400 content_blocked (custom does not degrade)", rec.Code)
	}

	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, rec.Body.String())
	}

	if env.Error.Code != "content_blocked" {
		t.Errorf("error.code=%q want content_blocked", env.Error.Code)
	}
	// 错误透传（error-passthrough，吸收上游 5755fe3）：message 装上游 body 原文
	// （code/msg 原样），客户端必须看到真实错误才能排查；gateway_hint 并列补充。
	if !strings.Contains(rec.Body.String(), "11-128") {
		t.Errorf("message should carry upstream original body (passthrough): %s", rec.Body.String())
	}

	if !strings.Contains(rec.Body.String(), "gateway_hint") {
		t.Errorf("content_blocked should carry gateway_hint: %s", rec.Body.String())
	}

	if handler.degrade.Active() {
		t.Error("degrade should NOT be active in custom mode")
	}
}

// TestChannelRefusedAndAccountRestrictedSplit 11-128 分流与「账号受限」收窄：
//   - `11-128 + request illegal`：通道校验失败 → 不罚号、不降级重试，换号轮转，
//     轮转耗尽回 400 channel_refused 并透传原文；
//   - 裸 `request illegal`（无 code）：普通请求错误 → 不再硬禁用账号；
//   - `11140 + request illegal`：账号受限 → 仍禁用，原因文案可读。
func TestChannelRefusedAndAccountRestrictedSplit(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		body         string
		wantStatus   int
		wantCode     string
		wantDisabled bool
	}{
		{"11-128 通道校验失败", `{"code":"11-128","msg":"request illegal"}`, http.StatusBadRequest, "channel_refused", false},
		{"裸 request illegal 不禁用", `{"msg":"request illegal"}`, http.StatusServiceUnavailable, "no_healthy_account", false},
		{"11140 账号受限", `{"code":11140,"msg":"request illegal"}`, http.StatusServiceUnavailable, "no_healthy_account", true},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			var calls int

			client := newFakeUpstream(t, func(string) (int, string, bool) {
				calls++

				return http.StatusBadRequest, testCase.body, false
			})
			state := testPoolWith(
				&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
				&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
			)
			// 熔断阈值 1：若误罚 NoteError 一次即熔断，便于暴露「通道错误被当 5xx」。
			state.SetBreaker(1, time.Hour, time.Hour)
			h := NewHandler(&Config{Pool: state, Upstream: client, PromptMode: "custom", PromptText: "SYS"})

			rec := postChat(t, h, `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)

			if rec.Code != testCase.wantStatus {
				t.Fatalf("code=%d want %d, body=%s", rec.Code, testCase.wantStatus, rec.Body)
			}

			if !strings.Contains(rec.Body.String(), testCase.wantCode) {
				t.Errorf("error.code want %q, body=%s", testCase.wantCode, rec.Body)
			}

			if calls != 2 {
				t.Errorf("upstream calls=%d want 2（两个号各试一次）", calls)
			}

			for _, uid := range []string{"u1", "u2"} {
				status := statusOf(t, state, uid)
				if status.Disabled != testCase.wantDisabled {
					t.Errorf("%s disabled=%v want %v (%+v)", uid, status.Disabled, testCase.wantDisabled, status)
				}

				if !testCase.wantDisabled && (status.Cooling || status.ErrTotal != 0) {
					t.Errorf("%s 不应被罚：%+v", uid, status)
				}
			}
		})
	}
}

// TestContentBlockedDoesNotPenalizeAccount ErrContentBlocked 不罚账号（无冷却/熔断/NoteError）。
func TestContentBlockedDoesNotPenalizeAccount(t *testing.T) {
	t.Parallel()
	client := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, `{"code":"11-128","msg":"blocked by security policy"}`, false
	})
	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	// 熔断阈值 1：若误罚 NoteError 一次即熔断；content_blocked 不应喂熔断。
	state.SetBreaker(1, time.Hour, time.Hour)
	h := NewHandler(&Config{Pool: state, Upstream: client, PromptMode: "passthrough"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))

	st, _ := state.Status("u1")
	if st.Cooling || st.Disabled || st.ErrTotal != 0 {
		t.Fatalf("ErrContentBlocked should not penalize account: %+v", st)
	}
}

// TestCustomModeFingerprintSanitizePreserved custom 端到端：user 消息含 Claude Code
// 指纹句（PR39 fixture 串）→ Rewrite 注入自有 system → 经 sanitize → 出站 body 中
// 该指纹被改写、system 为自有提示词。证明两层（提示词替换 + 清洗）叠加工作。
//
// 两层各自职责（互不替代）：
//   - prompt.Rewrite 替换 system/developer 消息（消灭 system 来源指纹）；
//   - sanitizeMessages 改写 user/assistant 消息中残留的指纹串（兜底用户上下文）。
func TestCustomModeFingerprintSanitizePreserved(t *testing.T) {
	t.Parallel()
	// 捕获出站 body（prepareBody 已强制 stream + 归一 + 清洗后）。
	var sentBody []byte

	client := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			sentBody = raw

			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
	client.SanitizeFingerprints.Store(true) // 开启清洗层（与生产一致）

	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})

	const customSys = "我是网关自有提示词"

	handler := NewHandler(&Config{Pool: p, Upstream: client, PromptMode: "custom", PromptText: customSys})

	// user 消息含 Claude Code 指纹句（PR39 的 fixture 串，逐字精确指纹）。
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"glm-5.2",
		"stream":true,
		"messages":[
			{"role":"system","content":"You are Claude Code, Anthropic's official CLI for Claude."},
			{"role":"user","content":"You are Claude Code, Anthropic's official CLI for Claude. Main branch (you will usually use this for PRs)"}
		]
	}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	out := string(sentBody)
	// 1) system 为自有提示词，旧 system 内容零残留；原始指纹被改写而非删除。
	assertFingerprintSanitized(t, out, customSys)
	// 2) messages 头部恰好一条 system = 自有提示词（Rewrite 已删旧 system）。
	assertSingleSystemMessage(t, sentBody, customSys)
}

// assertFingerprintSanitized 断言出站 body 中 system 为自有提示词、原始指纹串被改写
// （"official CLI for Claude." → "...tool for Claude."；"Main branch (" → "Default branch ("）。
func assertFingerprintSanitized(t *testing.T, out, customSys string) {
	t.Helper()

	if !strings.Contains(out, customSys) {
		t.Errorf("out body should contain custom system prompt: %s", out)
	}

	if strings.Contains(out, "official CLI for Claude.") {
		t.Errorf("identity fingerprint not rewritten by sanitize in user msg: %s", out)
	}

	if strings.Contains(out, "Main branch (you will usually use this for PRs)") {
		t.Errorf("branch fingerprint not rewritten by sanitize in user msg: %s", out)
	}
	// 改写后的痕迹应在（证明 sanitize 层生效，不是"全删了"）。
	if !strings.Contains(out, "official CLI tool for Claude.") {
		t.Errorf("sanitized identity rewrite missing: %s", out)
	}

	if !strings.Contains(out, "Default branch (you will usually use this for PRs)") {
		t.Errorf("sanitized branch rewrite missing: %s", out)
	}
}

// assertSingleSystemMessage 断言 messages 头部恰好一条 system = 自有提示词。
func assertSingleSystemMessage(t *testing.T, sentBody []byte, customSys string) {
	t.Helper()

	var obj map[string]any
	if err := json.Unmarshal(sentBody, &obj); err != nil {
		t.Fatalf("out body not json: %v %s", err, sentBody)
	}

	msgs := obj["messages"].([]any)

	var systemCount int

	for _, m := range msgs {
		mm := m.(map[string]any)
		if mm["role"] == "system" {
			systemCount++

			if mm["content"] != customSys {
				t.Errorf("system content=%v want %q", mm["content"], customSys)
			}
		}
	}

	if systemCount != 1 {
		t.Errorf("want exactly 1 system message, got %d (all=%v)", systemCount, msgs)
	}
}

// --- 模型原生工具调用标记修复（端到端，见 upstream/dsml.go）-----------------

// markupFixture 拼一段「正文 + 原生工具调用标记」，模拟上游在没有 tools 的请求里
// 把工具调用吐成文本的形态。分隔符按码点构造，避免测试源码里出现裸全角字面量。
func markupFixture(prose, cmd string) string {
	return markupFixtureNamed(prose, "exec_command", cmd)
}

// markupFixtureNamed 同 markupFixture，但可指定工具名（用于弱判定的形态把关测试）。
func markupFixtureNamed(prose, name, cmd string) string {
	const pipe = "\uFF5C\uFF5C"

	tag := func(rest string) string { return "<" + pipe + "DSML" + pipe + " " + rest + ">" }
	ctag := func(rest string) string { return "</" + pipe + "DSML" + pipe + " " + rest + ">" }

	return prose + tag("calls") + "\n" +
		tag(`invoke name="`+name+`"`) + "\n" +
		tag(`parameter name="cmd" string="true"`) + cmd + ctag("parameter") + "\n" +
		ctag("invoke") + "\n" + ctag("calls")
}

// markupUpstreamSSE 用一段正文拼上游 SSE（role 首帧 + 正文帧 + stop 收尾帧 + [DONE]）。
func markupUpstreamSSE(content string) string {
	chunk := func(delta map[string]any, finish any) string {
		b, _ := json.Marshal(map[string]any{
			"id": "resp-1", "object": "chat.completion.chunk", "created": 1, "model": "glm-5.2",
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
		})

		return "data: " + string(b) + "\n\n"
	}

	return chunk(map[string]any{"role": "assistant"}, nil) +
		chunk(map[string]any{"content": content}, nil) +
		chunk(map[string]any{}, "stop") +
		"data: [DONE]\n\n"
}

// declaredToolsBody 是带 tools 声明的请求体（标记修复的启用前提）。
const declaredToolsBody = `"tools":[{"type":"function","function":{"name":"exec_command",` +
	`"parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}}]`

// TestChatNonStreamRepairsMarkupToolCalls 非流式：上游把工具调用吐成正文标记时，
// 网关还原成 message.tool_calls，正文不再残留标记，finish_reason 收敛为 tool_calls。
func TestChatNonStreamRepairsMarkupToolCalls(t *testing.T) {
	t.Parallel()

	content := markupFixture("先读一遍控制器。\n\n", "ls -la")
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, markupUpstreamSSE(content), true
	})
	handler := NewHandler(&Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	reqBody := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],` + declaredToolsBody + `}`
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody)))

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if strings.Contains(rec.Body.String(), "\uFF5C") {
		t.Fatalf("响应里仍残留标记: %s", rec.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}

	choice := resp["choices"].([]any)[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	assertRepairedToolCall(t, msg, choice, "先读一遍控制器。\n\n", "exec_command", "ls -la")
}

// TestChatStreamRepairsMarkupToolCalls 流式：同样还原成 delta.tool_calls，且收尾帧
// finish_reason 为 tool_calls、排在调用帧之后。
func TestChatStreamRepairsMarkupToolCalls(t *testing.T) {
	t.Parallel()

	content := markupFixture("先读一遍控制器。\n\n", "ls -la")
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, markupUpstreamSSE(content), true
	})
	handler := NewHandler(&Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	reqBody := `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}],` + declaredToolsBody + `}`
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody)))

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	body := rec.Body.String()
	if strings.Contains(body, "\uFF5C") {
		t.Fatalf("流式响应里仍残留标记: %s", body)
	}

	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("缺 [DONE] 收尾: %s", body)
	}

	scan := scanStreamFrames(t, body)
	if scan.text.String() != "先读一遍控制器。\n\n" {
		t.Errorf("正文错误: %q", scan.text.String())
	}

	if scan.calls != 1 {
		t.Errorf("tool_call 帧数=%d want 1", scan.calls)
	}

	if scan.lastCall < 0 || scan.finish < 0 {
		t.Fatalf("缺调用帧或收尾帧: lastCall=%d finish=%d", scan.lastCall, scan.finish)
	}

	if scan.finish < scan.lastCall {
		t.Errorf("收尾帧（第 %d 帧）必须排在调用帧（第 %d 帧）之后", scan.finish, scan.lastCall)
	}
}

// streamFrameScan 扫描 SSE 帧收集到的形态数据：正文、tool_call 数量与调用/收尾帧序号。
type streamFrameScan struct {
	text     strings.Builder
	calls    int
	lastCall int
	finish   int
}

// scanStreamFrames 解析流式响应体，统计正文、tool_call 帧与收尾帧序号。
func scanStreamFrames(t *testing.T, body string) *streamFrameScan {
	t.Helper()

	scan := &streamFrameScan{lastCall: -1, finish: -1}
	frameNo := 0

	for line := range strings.SplitSeq(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}

		frameNo++
		scan.frame(t, frameNo, payload)
	}

	return scan
}

// frame 解析单帧并累加统计。
func (s *streamFrameScan) frame(t *testing.T, frameNo int, payload string) {
	t.Helper()

	var obj map[string]any
	if err := json.Unmarshal([]byte(payload), &obj); err != nil {
		t.Fatalf("帧不是合法 JSON: %q (%v)", payload, err)
	}

	chs, _ := obj["choices"].([]any)
	if len(chs) == 0 {
		return
	}

	table, _ := chs[0].(map[string]any)

	d, _ := table["delta"].(map[string]any)
	if v, ok := d["content"].(string); ok {
		s.text.WriteString(v)
	}

	s.noteToolCalls(t, frameNo, d)

	if v, ok := table["finish_reason"].(string); ok && v != "" {
		s.finish = frameNo

		if v != "tool_calls" {
			t.Errorf("finish_reason=%q want tool_calls", v)
		}
	}
}

// noteToolCalls 累加 tool_call 帧并校验工具名。
func (s *streamFrameScan) noteToolCalls(t *testing.T, frameNo int, d map[string]any) {
	t.Helper()

	tcs, ok := d["tool_calls"].([]any)
	if !ok || len(tcs) == 0 {
		return
	}

	s.calls += len(tcs)
	s.lastCall = frameNo

	fn, _ := tcs[0].(map[string]any)["function"].(map[string]any)
	if fn == nil || fn["name"] != "exec_command" {
		t.Fatalf("调用帧 function=%#v", tcs[0])
	}
}

// --- 弱判定（名单为空）端到端：这是实测最常见的泄漏形态 ---------------------

// TestChatRepairsMarkupWithoutDeclaredTools 请求体里**没有** tools 声明时（tools 在
// 中转环节丢失是实测常态），弱判定必须照样把正文标记还原成 tool_calls——否则修复层
// 恰好对主场景不设防。夹具用生产环境真实泄漏的正文。
func TestChatRepairsMarkupWithoutDeclaredTools(t *testing.T) {
	t.Parallel()

	content := markupFixture("", "ls -la")
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, markupUpstreamSSE(content), true
	})
	h := NewHandler(&Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if strings.Contains(rec.Body.String(), "\uFF5C") {
		t.Fatalf("响应里仍残留标记: %s", rec.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}

	choice := resp["choices"].([]any)[0].(map[string]any)
	msg := choice["message"].(map[string]any)

	tcs, _ := msg["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("tool_calls=%#v want 1 个", msg["tool_calls"])
	}

	fn, _ := tcs[0].(map[string]any)["function"].(map[string]any)
	if fn == nil || fn["name"] != "exec_command" {
		t.Fatalf("function=%#v", tcs[0])
	}

	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason=%v want tool_calls", choice["finish_reason"])
	}
}

// TestChatMarkupRepairRejectsNonIdentifierToolName 弱判定的把关：名字不像工具名
// （模型在正文里「讨论」这段语法时的占位符）→ 原文透传，绝不改写成工具调用。
func TestChatMarkupRepairRejectsNonIdentifierToolName(t *testing.T) {
	t.Parallel()

	content := markupFixtureNamed("", "<tool_name>", "ls -la")
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, markupUpstreamSSE(content), true
	})
	h := NewHandler(&Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}

	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if _, ok := msg["tool_calls"]; ok {
		t.Fatalf("占位符工具名不得转换: %#v", msg["tool_calls"])
	}

	if got, _ := msg["content"].(string); !strings.Contains(got, "\uFF5C") {
		t.Fatalf("应原文透传，实际 content=%q", got)
	}
}

// TestChatMarkupRepairStrictModeRejectsUndeclaredTool 名单非空时走严格判定：
// 块内工具名不在名单里 → 不转换（即使名字形态合法）。
func TestChatMarkupRepairStrictModeRejectsUndeclaredTool(t *testing.T) {
	t.Parallel()

	content := markupFixtureNamed("", "rm_rf_everything", "ls -la")
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, markupUpstreamSSE(content), true
	})
	handler := NewHandler(&Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	reqBody := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],` + declaredToolsBody + `}`
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody)))

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}

	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if _, ok := msg["tool_calls"]; ok {
		t.Fatalf("未声明的工具名不得转换: %#v", msg["tool_calls"])
	}
}

// TestChatModelNotFoundDoesNotRotate 上游 11102（目录无此模型）是请求级事实：
// 一次尝试即返回 404，不按 MaxRotate 逐号重试，也不冷却账号。
func TestChatModelNotFoundDoesNotRotate(t *testing.T) {
	t.Parallel()

	calls := 0
	client := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return http.StatusBadRequest, `{"code":11102,"msg":"model [gpt-100] service info not found"}`, false
	})
	state := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u3", AccessToken: "at3", ExpiresAt: 9999999999},
	)
	h := NewHandler(&Config{Pool: state, Upstream: client})

	rec := postChat(t, h, `{"model":"gpt-100","messages":[]}`)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "model_not_found") {
		t.Fatalf("code=%d body=%s want 404 model_not_found", rec.Code, rec.Body)
	}

	if calls != 1 {
		t.Errorf("upstream calls=%d want 1 (must not rotate on 11102)", calls)
	}

	for _, uid := range []string{"u1", "u2", "u3"} {
		if st := statusOf(t, state, uid); st.Cooling {
			t.Errorf("%s should not be penalized on 11102: %+v", uid, st)
		}
	}
}

// TestChatModelBlockPlatformHint 模型全池阻塞且账号共享同一上游重置时刻 → hint 标注
// platform-level（issue #29）。
func TestChatModelBlockPlatformHint(t *testing.T) {
	t.Parallel()
	client := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	state := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	reset := time.Now().Add(time.Hour)
	state.CooldownSoftForModel("u1", time.Minute, reset, "glm-5.2", "6004 rate limited")
	state.CooldownSoftForModel("u2", time.Minute, reset, "glm-5.2", "6004 rate limited")
	h := NewHandler(&Config{Pool: state, Upstream: client})

	rec := postChat(t, h, `{"model":"glm-5.2","messages":[]}`)
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "rate_limit_exceeded") {
		t.Fatalf("code=%d body=%s want 429 rate_limit_exceeded", rec.Code, rec.Body)
	}

	if !strings.Contains(rec.Body.String(), "platform-level") {
		t.Errorf("model block hint should mark platform-level: %s", rec.Body)
	}
	// 429 必须带 Retry-After：解封时刻已知，客户端据此等待而不是立刻重试。
	if got := rec.Header().Get("Retry-After"); got == "" {
		t.Errorf("模型级 429 应带 Retry-After: header 缺失")
	}
}

// TestChatStreamTruncatedNoSuccess 上游流截断（EOF 无 [DONE]，issue #105）：
// 客户端收到 200 与补写的 [DONE]，但账号不记成功、不绑粘性。
func TestChatStreamTruncatedNoSuccess(t *testing.T) {
	t.Parallel()

	bindStore2 := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     bindStore2,
		Available: func() []string { return []string{"only"} },
	})
	state := testPoolWith(&auth.Auth{UID: "only", AccessToken: "at-only", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"half\"}}]}\n\n", true
	})
	h := NewHandler(&Config{Pool: state, Upstream: up, Session: sess, SoftCooldown: time.Minute})

	rec := postChat(t, h, `{"model":"glm-5.2","messages":[],"stream":true,"metadata":{"conversation_id":"conv-1"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	// 客户端必须拿到一个 [DONE] 收尾。
	if n := strings.Count(rec.Body.String(), "data: [DONE]"); n != 1 {
		t.Errorf("[DONE] count=%d want 1: %q", n, rec.Body)
	}
	// 截断不是成功：账号 success_count 保持 0，粘性不绑定。
	if s := statusOf(t, state, "only").SuccessCount; s != 0 {
		t.Errorf("success_count=%d want 0 (truncated stream is not success)", s)
	}

	if _, ok := bindStore2.lastUID("conv-1"); ok {
		t.Errorf("truncated stream must not bind sticky")
	}
}

// TestChatStreamReadErrorNoSuccess 上游流中读错误（非 EOF，issue #105）：
// 客户端已收到 200，但这是上游失败——账号不记成功、不绑粘性；客户端写失败
// 才按 Interrupted 记成功（两者由 writeAll 的 errClientWrite 区分）。
func TestChatStreamReadErrorNoSuccess(t *testing.T) {
	t.Parallel()

	bindStore2 := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     bindStore2,
		Available: func() []string { return []string{"only"} },
	})
	state := testPoolWith(&auth.Auth{UID: "only", AccessToken: "at-only", ExpiresAt: 9999999999})
	client := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body: io.NopCloser(io.MultiReader(
					strings.NewReader("data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"half\"}}]}\n\n"),
					iotest.ErrReader(errConnectionResetByPeer),
				)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	h := NewHandler(&Config{Pool: state, Upstream: client, Session: sess, SoftCooldown: time.Minute})

	rec := postChat(t, h, `{"model":"glm-5.2","messages":[],"stream":true,"metadata":{"conversation_id":"conv-1"}}`)
	// 已写出正文后发生读错误：头已发只能 200，但账号不记成功。
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if s := statusOf(t, state, "only").SuccessCount; s != 0 {
		t.Errorf("success_count=%d want 0 (upstream read error is not success)", s)
	}

	if _, ok := bindStore2.lastUID("conv-1"); ok {
		t.Errorf("upstream read error must not bind sticky")
	}
}

// TestChatStreamEmptyStreamNoSuccess 上游空流（200 + 0 有效帧，issue #105）：
// 账号不记成功、不绑粘性（写头后无法换号，只记失败）。
func TestChatStreamEmptyStreamNoSuccess(t *testing.T) {
	t.Parallel()

	bindStore2 := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     bindStore2,
		Available: func() []string { return []string{"only"} },
	})
	state := testPoolWith(&auth.Auth{UID: "only", AccessToken: "at-only", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, ": keepalive comment\n\n", true
	})
	h := NewHandler(&Config{Pool: state, Upstream: up, Session: sess, SoftCooldown: time.Minute})

	rec := postChat(t, h, `{"model":"glm-5.2","messages":[],"stream":true,"metadata":{"conversation_id":"conv-1"}}`)
	// 首帧否决后尚未写字节：空流按 502 upstream_parse 收口，而不是 200 假成功。
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if s := statusOf(t, state, "only").SuccessCount; s != 0 {
		t.Errorf("success_count=%d want 0 (empty stream is not success)", s)
	}

	if _, ok := bindStore2.lastUID("conv-1"); ok {
		t.Errorf("empty stream must not bind sticky")
	}
}

// TestChatNonStreamTruncatedRotates 非流式截断：客户端未收到任何字节，换号重试；
// 拿到完整流的号才记成功，截断号 success_count 保持 0（issue #105）。
func TestChatNonStreamTruncatedRotates(t *testing.T) {
	t.Parallel()

	calls := map[string]int{}
	client := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		if authz == "Bearer at-bad" {
			return 200, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"half\"}}]}\n\n", true
		}

		return 200, sseOK, true
	})
	state := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	h := NewHandler(&Config{Pool: state, Upstream: client, SoftCooldown: time.Minute})

	rec := postChat(t, h, `{"model":"glm-5.2","messages":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if calls["Bearer at-bad"] != 1 || calls["Bearer at-good"] != 1 {
		t.Errorf("calls=%v want bad once + good once (rotate after truncation)", calls)
	}

	if s := statusOf(t, state, "bad").SuccessCount; s != 0 {
		t.Errorf("bad success_count=%d want 0 (its stream was truncated)", s)
	}

	if s := statusOf(t, state, "good").SuccessCount; s != 1 {
		t.Errorf("good success_count=%d want 1", s)
	}
	// 截断号必须释放本轮在途租约：否则下一轮 heldUID 覆盖后名额永不归还。
	if n := statusOf(t, state, "bad").InFlight; n != 0 {
		t.Errorf("bad in_flight=%d want 0 (failed attempt must release lease)", n)
	}
}

// TestChatNonStreamTruncatedAllAccountsFails502 非流式截断且所有号都截断：
// 轮转耗尽后回 502，不把截断内容当成功写回（issue #105）。
func TestChatNonStreamTruncatedAllAccountsFails502(t *testing.T) {
	t.Parallel()
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"half\"}}]}\n\n", true
	})
	state := testPoolWith(&auth.Auth{UID: "only", AccessToken: "at-only", ExpiresAt: 9999999999})
	h := NewHandler(&Config{Pool: state, Upstream: up, SoftCooldown: time.Minute})

	rec := postChat(t, h, `{"model":"glm-5.2","messages":[]}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code=%d want 502 body=%s", rec.Code, rec.Body)
	}

	if s := statusOf(t, state, "only").SuccessCount; s != 0 {
		t.Errorf("success_count=%d want 0", s)
	}
}

// TestChatStreamLineTooLongNoSuccess 上游单行超过大小上限（issue #113）：
// 客户端收到 200 + error 帧 + [DONE]，但账号不记成功、不绑粘性。
func TestChatStreamLineTooLongNoSuccess(t *testing.T) {
	t.Parallel()

	bindStore2 := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     bindStore2,
		Available: func() []string { return []string{"only"} },
	})
	state := testPoolWith(&auth.Auth{UID: "only", AccessToken: "at-only", ExpiresAt: 9999999999})
	client := newFakeUpstream(t, func(authz string) (int, string, bool) {
		huge := strings.Repeat("x", upstream.MaxSSELineBytes+1024)

		return 200, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n" +
			"data: " + huge + "\n\n", true
	})
	h := NewHandler(&Config{Pool: state, Upstream: client, Session: sess, SoftCooldown: time.Minute})

	rec := postChat(t, h, `{"model":"glm-5.2","messages":[],"stream":true,"metadata":{"conversation_id":"conv-1"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "exceeded size limit") {
		t.Errorf("missing size-limit error frame: %q", body)
	}

	if n := strings.Count(body, "data: [DONE]"); n != 1 {
		t.Errorf("[DONE] count=%d want 1: %q", n, body)
	}
	// 防御上限命中 = 上游异常：不记成功、不绑粘性。
	if s := statusOf(t, state, "only").SuccessCount; s != 0 {
		t.Errorf("success_count=%d want 0 (size-limit hit is upstream failure)", s)
	}

	if _, ok := bindStore2.lastUID("conv-1"); ok {
		t.Errorf("size-limit failure must not bind sticky")
	}
}

// TestChatNonStreamLineTooLongRotates 非流式单行超限：换号重试；全部超限时
// 回 502（issue #113），不把部分内容当完整回答。
func TestChatNonStreamLineTooLongRotates(t *testing.T) {
	t.Parallel()

	huge := strings.Repeat("x", upstream.MaxSSELineBytes+1024)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, "data: " + huge + "\n\n", true
	})
	state := testPoolWith(&auth.Auth{UID: "only", AccessToken: "at-only", ExpiresAt: 9999999999})
	h := NewHandler(&Config{Pool: state, Upstream: up, SoftCooldown: time.Minute})

	rec := postChat(t, h, `{"model":"glm-5.2","messages":[]}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code=%d want 502 body=%s", rec.Code, rec.Body)
	}

	if s := statusOf(t, state, "only").SuccessCount; s != 0 {
		t.Errorf("success_count=%d want 0", s)
	}
}

// 粘性软逃逸后应优先分流到其他账号，而不是在同一轮立刻选回原号。
func TestChatStickySoftEscapePrefersOtherAccount(t *testing.T) {
	t.Parallel()

	st := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"busy", "good"} },
	})
	state := testPoolWith(
		&auth.Auth{UID: "busy", AccessToken: "at-busy", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	state.SetMaxInFlight(3)
	state.SetCredits("busy", 1000, 0)
	state.SetRandomSource(func(n int64) int64 { return 0 }) // 不排除时必选权重更高的 busy
	state.Acquire("busy")
	state.Acquire("busy")

	var used string

	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		used = authz
		return 200, sseOK, true
	})
	h := NewHandler(&Config{Pool: state, Upstream: up, Session: sess, SoftCooldown: time.Minute})
	sess.Bind("conv-1", "busy")

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[],"metadata":{"conversation_id":"conv-1"}}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if used != "Bearer at-good" {
		t.Fatalf("soft escape 后仍选回原号: authz=%q", used)
	}
}

// 单号池软逃逸后无其他账号可选：应回落原号继续服务，而不是 503。
func TestChatStickySoftEscapeFallsBackWhenNoAlternative(t *testing.T) {
	t.Parallel()

	st := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"busy"} },
	})
	state := testPoolWith(&auth.Auth{UID: "busy", AccessToken: "at-busy", ExpiresAt: 9999999999})
	state.SetMaxInFlight(3)
	state.Acquire("busy")
	state.Acquire("busy")

	var used string

	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		used = authz
		return 200, sseOK, true
	})
	h := NewHandler(&Config{Pool: state, Upstream: up, Session: sess, SoftCooldown: time.Minute})
	sess.Bind("conv-1", "busy")

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[],"metadata":{"conversation_id":"conv-1"}}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("单号池软逃逸不应失败: code=%d body=%s", rec.Code, rec.Body)
	}

	if used != "Bearer at-busy" {
		t.Fatalf("单号池应回落原号: authz=%q", used)
	}
}
