package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// TestStreamFirstFrameErrorVeto 首帧可换号失败不写 200：直接换号后成功（issue #244）。
func TestStreamFirstFrameErrorVeto(t *testing.T) {
	t.Parallel()

	errFrame := "data: {\"error\":{\"code\":6004,\"message\":\"model busy\"}}\n\ndata: [DONE]\n\n"
	okFrame := "data: {\"id\":\"1\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n" +
		"data: [DONE]\n\n"

	upstreamClient := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if strings.Contains(authz, "at1") {
			return 200, errFrame, true
		}

		return 200, okFrame, true
	})
	state := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	handler := NewHandler(&Config{Pool: state, Upstream: upstreamClient, MaxRotate: 3})

	rec := postChat(t, handler, `{"model":"glm-5.2","stream":true,"messages":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if !strings.Contains(rec.Body.String(), `"content":"ok"`) {
		t.Fatalf("body=%s", rec.Body)
	}

	if strings.Contains(rec.Body.String(), `"error"`) {
		t.Fatalf("client must not see first-frame error after successful rotation: %s", rec.Body)
	}
}

// TestStreamZeroOutputLengthRetriesThinkingDisabled 零输出 length 关闭思考重发一次（issue #244）。
func TestStreamZeroOutputLengthRetriesThinkingDisabled(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	var secondBody atomic.Value

	lengthFrame := "data: {\"id\":\"1\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"length\"}]}\n\ndata: [DONE]\n\n"
	okFrame := "data: {\"id\":\"1\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"

	upstreamClient := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if calls.Add(1) == 1 {
			return 200, lengthFrame, true
		}

		return 200, okFrame, true
	})
	// 包一层 Transport，记录第二次请求体。
	base := upstreamClient.HTTP.Transport
	upstreamClient.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if calls.Load() >= 1 {
			raw, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(strings.NewReader(string(raw)))
			secondBody.Store(string(raw))
		}

		return base.RoundTrip(r)
	})

	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	handler := NewHandler(&Config{Pool: state, Upstream: upstreamClient})

	rec := postChat(t, handler, `{"model":"glm-5.2","stream":true,"messages":[],"thinking":{"type":"enabled"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if calls.Load() != 2 {
		t.Fatalf("calls=%d want 2", calls.Load())
	}

	body, _ := secondBody.Load().(string)
	if !strings.Contains(body, `"thinking":{"type":"disabled"}`) {
		t.Fatalf("second request must disable thinking: %s", body)
	}

	if !strings.Contains(rec.Body.String(), `"content":"ok"`) {
		t.Fatalf("body=%s", rec.Body)
	}
}

// TestStreamContentFilterPassedThrough content_filter 终态直通客户端，不轮转、不罚号。
func TestStreamContentFilterPassedThrough(t *testing.T) {
	t.Parallel()

	frame := "data: {\"id\":\"1\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"content_filter\"}]}\n\n" +
		"data: [DONE]\n\n"

	var calls atomic.Int32

	upstreamClient := newFakeUpstream(t, func(string) (int, string, bool) {
		calls.Add(1)

		return 200, frame, true
	})
	state := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	handler := NewHandler(&Config{Pool: state, Upstream: upstreamClient, MaxRotate: 3})

	rec := postChat(t, handler, `{"model":"glm-5.2","stream":true,"messages":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if !strings.Contains(rec.Body.String(), `"finish_reason":"content_filter"`) {
		t.Fatalf("content_filter must reach client: %s", rec.Body)
	}

	if calls.Load() != 1 {
		t.Fatalf("content_filter must not rotate: calls=%d", calls.Load())
	}
}

// TestNonStreamContinuation 非流式 length 截断自动续写并拼接正文（issue #247）。
func TestNonStreamContinuation(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	client := newFakeUpstream(t, func(string) (int, string, bool) {
		if calls.Add(1) == 1 {
			return 200, "data: {\"id\":\"c1\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"message\":{\"role\":\"assistant\",\"content\":\"hello \"},\"finish_reason\":\"length\"}]}\n\ndata: [DONE]\n\n", true
		}

		return 200, "data: {\"id\":\"c1\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"message\":{\"role\":\"assistant\",\"content\":\"world\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", true
	})

	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	handler := NewHandler(&Config{Pool: state, Upstream: client})

	rec := postChat(t, handler, `{"model":"glm-5.2","messages":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	var parsed struct {
		Choices []struct {
			Message      map[string]any `json:"message"`
			FinishReason string         `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Fatal(err)
	}

	if len(parsed.Choices) != 1 || parsed.Choices[0].FinishReason != "stop" {
		t.Fatalf("choices=%+v", parsed.Choices)
	}

	if parsed.Choices[0].Message["content"] != "hello world" {
		t.Fatalf("content=%v", parsed.Choices[0].Message["content"])
	}

	if calls.Load() != 2 {
		t.Fatalf("calls=%d want 2", calls.Load())
	}
}

// TestNonStreamContinuationKeepsLastPrompt 续写段的 prompt 已含整段上下文：
// prompt/cache 取末段，completion/credit 跨段累加，total 按末段 prompt + 累计
// completion 重算（issue #432）。
func TestNonStreamContinuationKeepsLastPrompt(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	client := newFakeUpstream(t, func(string) (int, string, bool) {
		if calls.Add(1) == 1 {
			return 200, "data: {\"id\":\"c1\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"message\":{\"role\":\"assistant\",\"content\":\"hello \"},\"finish_reason\":\"length\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2,\"total_tokens\":12,\"credit\":1,\"prompt_tokens_details\":{\"cached_tokens\":3}}}\n\ndata: [DONE]\n\n", true
		}

		return 200, "data: {\"id\":\"c1\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"message\":{\"role\":\"assistant\",\"content\":\"world\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":4,\"total_tokens\":24,\"credit\":2,\"prompt_tokens_details\":{\"cached_tokens\":5}}}\n\ndata: [DONE]\n\n", true
	})

	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	handler := NewHandler(&Config{Pool: state, Upstream: client})

	rec := postChat(t, handler, `{"model":"glm-5.2","messages":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	var parsed struct {
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Fatal(err)
	}

	assertUsageNumbers(t, parsed.Usage, map[string]float64{
		"prompt_tokens": 20, "completion_tokens": 6, "total_tokens": 26, "credit": 3,
	})

	details, _ := parsed.Usage["prompt_tokens_details"].(map[string]any)
	if got, _ := details["cached_tokens"].(float64); got != 5 {
		t.Errorf("cached_tokens=%v want 5", details["cached_tokens"])
	}
}

// TestMergeUsageTotalsThreeSegments 三段时 prompt 仍等于末段，不随段数增长；
// completion 与 credit 等于三段之和（issue #432）。
func TestMergeUsageTotalsThreeSegments(t *testing.T) {
	t.Parallel()

	first := map[string]any{"usage": map[string]any{
		"prompt_tokens": 10.0, "completion_tokens": 2.0, "credit": 1.0, "total_tokens": 12.0,
	}}
	mergeUsageTotals(first, map[string]any{"usage": map[string]any{
		"prompt_tokens": 20.0, "completion_tokens": 4.0, "credit": 2.0, "total_tokens": 24.0,
	}})
	mergeUsageTotals(first, map[string]any{"usage": map[string]any{
		"prompt_tokens": 30.0, "completion_tokens": 8.0, "credit": 3.0, "total_tokens": 38.0,
	}})

	usage, _ := first["usage"].(map[string]any)
	assertUsageNumbers(t, usage, map[string]float64{
		"prompt_tokens": 30, "completion_tokens": 14, "credit": 6, "total_tokens": 44,
	})
}

// TestMergeUsageTotalsKeepsLastWithoutPrompt 末段缺少 prompt_tokens 时仍保留末段
// usage，不因 total 无法重算丢掉整段（issue #432）。
func TestMergeUsageTotalsKeepsLastWithoutPrompt(t *testing.T) {
	t.Parallel()

	first := map[string]any{"usage": map[string]any{
		"prompt_tokens": 10.0, "completion_tokens": 2.0, "credit": 1.0, "total_tokens": 12.0,
	}}
	mergeUsageTotals(first, map[string]any{"usage": map[string]any{
		"completion_tokens": 4.0, "credit": 2.0, "total_tokens": 9.0,
	}})

	usage, _ := first["usage"].(map[string]any)
	if _, ok := usage["prompt_tokens"]; ok {
		t.Errorf("prompt_tokens=%v want absent (last segment has none)", usage["prompt_tokens"])
	}

	assertUsageNumbers(t, usage, map[string]float64{
		"completion_tokens": 6, "credit": 3, "total_tokens": 9,
	})
}

func assertUsageNumbers(t *testing.T, usage map[string]any, want map[string]float64) {
	t.Helper()

	for key, value := range want {
		if got, _ := usage[key].(float64); got != value {
			t.Errorf("usage.%s=%v want %v", key, usage[key], value)
		}
	}
}

// TestStreamContinuationSumsUsage 流式截断续写与非流式同口径：prompt 取末段，
// completion/credit 累加，total 重算；首段 length 终态不得提前发给客户端
// （issue #247 / #432）。
//
//nolint:cyclop // 端到端断言，线性展开更直观。
func TestStreamContinuationSumsUsage(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	client := newFakeUpstream(t, func(string) (int, string, bool) {
		if calls.Add(1) == 1 {
			return 200, `data: {"id":"c1","model":"glm-5.2","choices":[{"index":0,"delta":{"content":"hello "}}]}` + "\n\n" +
				`data: {"id":"c1","model":"glm-5.2","choices":[{"index":0,"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12,"credit":1}}` + "\n\n" +
				"data: [DONE]\n\n", true
		}

		return 200, `data: {"id":"c1","model":"glm-5.2","choices":[{"index":0,"delta":{"content":"world"},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":4,"total_tokens":24,"credit":2}}` + "\n\n" +
			"data: [DONE]\n\n", true
	})

	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	handler := NewHandler(&Config{Pool: state, Upstream: client})

	rec := postChat(t, handler, `{"model":"glm-5.2","stream":true,"messages":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "hello ") || !strings.Contains(body, "world") {
		t.Fatalf("continuation body missing content: %s", body)
	}

	if strings.Contains(body, `"finish_reason":"length"`) {
		t.Fatalf("client must not see first-segment length before continuation: %s", body)
	}

	usage := map[string]any{}

	for line := range strings.SplitSeq(body, "\n") {
		if !strings.HasPrefix(line, "data: ") || strings.Contains(line, "[DONE]") {
			continue
		}

		var chunk map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk) != nil {
			continue
		}

		if u, ok := chunk["usage"].(map[string]any); ok {
			usage = u
		}
	}

	for key, want := range map[string]float64{"prompt_tokens": 20, "completion_tokens": 6, "total_tokens": 26, "credit": 3} {
		if got, _ := usage[key].(float64); got != want {
			t.Errorf("usage.%s=%v want %v (body=%s)", key, usage[key], want, body)
		}
	}

	if calls.Load() != 2 {
		t.Fatalf("calls=%d want 2", calls.Load())
	}
}

// TestNonStreamZeroOutputLengthRetriesThinkingDisabled 非流式零输出 length 也关闭
// thinking 重发一次，而不是把空 content 当成功（issue #244）。
func TestNonStreamZeroOutputLengthRetriesThinkingDisabled(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	var secondBody atomic.Value

	lengthFrame := "data: {\"id\":\"c1\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"message\":{\"role\":\"assistant\",\"content\":\"\"},\"finish_reason\":\"length\"}]}\n\ndata: [DONE]\n\n"
	okFrame := "data: {\"id\":\"c1\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"message\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"

	client := newFakeUpstream(t, func(string) (int, string, bool) {
		if calls.Add(1) == 1 {
			return 200, lengthFrame, true
		}

		return 200, okFrame, true
	})
	base := client.HTTP.Transport
	client.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if calls.Load() >= 1 {
			raw, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(strings.NewReader(string(raw)))
			secondBody.Store(string(raw))
		}

		return base.RoundTrip(r)
	})

	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	handler := NewHandler(&Config{Pool: state, Upstream: client})

	rec := postChat(t, handler, `{"model":"glm-5.2","messages":[],"thinking":{"type":"enabled"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if calls.Load() != 2 {
		t.Fatalf("calls=%d want 2", calls.Load())
	}

	body, _ := secondBody.Load().(string)
	if !strings.Contains(body, `"thinking":{"type":"disabled"}`) {
		t.Fatalf("second request must disable thinking: %s", body)
	}

	if !strings.Contains(rec.Body.String(), `"content":"ok"`) {
		t.Fatalf("body=%s", rec.Body)
	}
}

// TestApplyModelBlockOverrideBusy 纯 14003 全池阻塞返回 503 model_busy（issue #254）。
func TestApplyModelBlockOverrideBusy(t *testing.T) {
	t.Parallel()

	flow := &chatFlow{modelBlock: pool.ModelBlockStatus{Blocked: true, Busy: true, Count: 2}}
	status, code, _, _ := flow.applyModelBlockOverride(http.StatusServiceUnavailable, "no_healthy_account", "", "", false)

	if status != http.StatusServiceUnavailable || code != "model_busy" {
		t.Fatalf("status=%d code=%s", status, code)
	}

	accountLevel := &chatFlow{modelBlock: pool.ModelBlockStatus{Blocked: false}}

	status, code, _, _ = accountLevel.applyModelBlockOverride(http.StatusServiceUnavailable, "no_healthy_account", "", "", false)
	if status != http.StatusServiceUnavailable || code != "no_healthy_account" {
		t.Fatalf("account-level status=%d code=%s", status, code)
	}
}

// TestWaitForModelBusyRespectsCancel ctx 取消时立即返回（issue #254 的 10s 上限）。
func TestWaitForModelBusyRespectsCancel(t *testing.T) {
	t.Parallel()

	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	state.CooldownModelBusy("u1", "glm-5.2", "14003 model busy")

	flow := &chatFlow{h: NewHandler(&Config{Pool: state}), bareModel: "glm-5.2", realm: "cn"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()

	if flow.waitForModelBusy(ctx) {
		t.Fatal("canceled ctx must not wait")
	}

	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("canceled wait took %v", elapsed)
	}
}

// 聚合路径改用 UseNumber 解码 SSE 帧后，usage 里的数字是 json.Number。
// 只认 float64 会让累计 completion 被末段覆盖（多轮少算），total 也不再重算。
func TestMergeUsageTotalsWithJSONNumbers(t *testing.T) {
	t.Parallel()

	first := map[string]any{"usage": map[string]any{
		"prompt_tokens": json.Number("10"), "completion_tokens": json.Number("2"),
		"credit": json.Number("1.5"), "total_tokens": json.Number("12"),
	}}
	mergeUsageTotals(first, map[string]any{"usage": map[string]any{
		"prompt_tokens": json.Number("20"), "completion_tokens": json.Number("4"),
		"credit": json.Number("2"), "total_tokens": json.Number("24"),
	}})

	usage, _ := first["usage"].(map[string]any)

	if got, _ := upstream.UsageNumber(usage, "prompt_tokens"); got != 20 {
		t.Errorf("prompt_tokens=%v want 20（末段覆盖）", usage["prompt_tokens"])
	}

	if got, _ := upstream.UsageNumber(usage, "completion_tokens"); got != 6 {
		t.Errorf("completion_tokens=%v want 6（跨段累计）", usage["completion_tokens"])
	}

	if got, _ := upstream.UsageNumber(usage, "credit"); got != 3.5 {
		t.Errorf("credit=%v want 3.5（跨段累计）", usage["credit"])
	}

	if got, _ := upstream.UsageNumber(usage, "total_tokens"); got != 26 {
		t.Errorf("total_tokens=%v want 26（末段 prompt + 累计 completion）", usage["total_tokens"])
	}
}

// total 溢出/为负时不写回：+Inf 会让 encoding/json 直接失败，整个非流式响应变 500。
func TestRecomputeUsageTotalRejectsUnusableSum(t *testing.T) {
	t.Parallel()

	for _, usage := range []map[string]any{
		{"prompt_tokens": 1e308, "completion_tokens": 1e308},
		{"prompt_tokens": -5.0, "completion_tokens": 1.0},
	} {
		if recomputeUsageTotal(usage) {
			t.Fatalf("不可信求和 %v 不应触发 total 重算", usage)
		}

		if _, has := usage["total_tokens"]; has {
			t.Fatalf("不可信求和 %v 不应写入 total_tokens", usage)
		}
	}

	ok := map[string]any{"prompt_tokens": 10.0, "completion_tokens": 5.0}
	if !recomputeUsageTotal(ok) || ok["total_tokens"] != 15.0 {
		t.Fatalf("正常求和应写入 total=%v", ok["total_tokens"])
	}
}
