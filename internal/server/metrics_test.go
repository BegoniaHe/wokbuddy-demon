// metrics_test.go /metrics 指标端点（issue #13）：文本格式、鉴权同口径、埋点计数。
package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
	"github.com/BegoniaHe/wokbuddy-demon/internal/usage"
)

// getMetrics 向 h 发一次 GET /metrics，返回响应记录器。
func getMetrics(t *testing.T, h *Handler, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestMetricsExposition 账号/请求/Token 三类复用现有计数，histogram 为空时输出 _count 0。
func TestMetricsExposition(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	rec := usage.New("")
	rec.Add(time.Now(), "cn", "u1", "glm-5.2", usage.Delta{
		PromptTokens: 100, HasPromptTokens: true,
		CompletionTokens: 50, HasCompletion: true,
	}, true)
	h := NewHandler(Config{Pool: p, Upstream: upstream.New(), Usage: rec})

	r := getMetrics(t, h, nil)
	if r.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", r.Code, r.Body)
	}
	if ct := r.Header().Get("Content-Type"); ct != "text/plain; version=0.0.4; charset=utf-8" {
		t.Errorf("content-type=%q", ct)
	}
	body := r.Body.String()
	for _, want := range []string{
		"# TYPE wokbuddy_accounts gauge",
		`wokbuddy_accounts{state="ok"} 1`,
		`wokbuddy_accounts{state="disabled"} 0`,
		"# TYPE wokbuddy_requests_total counter",
		`wokbuddy_requests_total{model="glm-5.2",result="ok"} 1`,
		`wokbuddy_requests_total{model="glm-5.2",result="error"} 0`,
		`wokbuddy_tokens_total{model="glm-5.2",kind="prompt"} 100`,
		`wokbuddy_tokens_total{model="glm-5.2",kind="completion"} 50`,
		`wokbuddy_tokens_total{model="glm-5.2",kind="total"} 150`,
		"# TYPE wokbuddy_request_duration_seconds histogram",
		`wokbuddy_request_duration_seconds_bucket{result="ok",le="+Inf"} 0`,
		`wokbuddy_request_duration_seconds_sum{result="ok"} 0`,
		`wokbuddy_request_duration_seconds_count{result="ok"} 0`,
		`wokbuddy_request_duration_seconds_bucket{result="error",le="+Inf"} 0`,
		`wokbuddy_request_duration_seconds_count{result="error"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics body missing %q\n---\n%s", want, body)
		}
	}
}

// TestMetricsNoUsage Usage 为 nil 时省略请求/Token 族，其余照常 200。
func TestMetricsNoUsage(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})

	r := getMetrics(t, h, nil)
	if r.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", r.Code, r.Body)
	}
	body := r.Body.String()
	if strings.Contains(body, "wokbuddy_requests_total") || strings.Contains(body, "wokbuddy_tokens_total") {
		t.Errorf("Usage=nil 时不应出现请求/Token 族:\n%s", body)
	}
	if !strings.Contains(body, `wokbuddy_accounts{state="ok"} 1`) {
		t.Errorf("账号族应照常输出:\n%s", body)
	}
	if !strings.Contains(body, "wokbuddy_request_duration_seconds_count{result=\"ok\"} 0") {
		t.Errorf("histogram 族应照常输出:\n%s", body)
	}
}

// TestMetricsAuth 鉴权与主服务同口径：api_key 非空时缺/错 token → 401，正确 → 200。
func TestMetricsAuth(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: upstream.New(), APIKey: "secret"})

	if r := getMetrics(t, h, nil); r.Code != http.StatusUnauthorized {
		t.Errorf("no token: code=%d want 401", r.Code)
	}
	if r := getMetrics(t, h, map[string]string{"Authorization": "Bearer wrong"}); r.Code != http.StatusUnauthorized {
		t.Errorf("wrong token: code=%d want 401", r.Code)
	}
	if r := getMetrics(t, h, map[string]string{"Authorization": "Bearer secret"}); r.Code != http.StatusOK {
		t.Errorf("valid token: code=%d want 200", r.Code)
	}
}

// TestMetricsRecordsAttemptsAndRotations 端到端埋点：首个号 429 → 换号 → 第二个号 200，
// 断言耗时 histogram 观测两次（每次尝试一次）、换号计数 reason=soft_rate 记一次。
func TestMetricsRecordsAttemptsAndRotations(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-bad" {
			return 429, `{"code":1,"msg":"too many requests"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000, 0) // 确定性源 r=0 → 先选 bad
	p.SetCredits("good", 1000, 0)
	rec := usage.New("")
	h := NewHandler(Config{Pool: p, Upstream: up, Usage: rec})

	resp := postChat(t, h, `{"model":"glm-5.2","stream":true,"messages":[]}`)
	if resp.Code != http.StatusOK {
		t.Fatalf("chat code=%d body=%s", resp.Code, resp.Body)
	}

	body := getMetrics(t, h, nil).Body.String()
	// 每次尝试观测一次耗时，按结果分流：429 记 error、200 记 ok。
	if !strings.Contains(body, `wokbuddy_request_duration_seconds_count{result="ok"} 1`) {
		t.Errorf("成功尝试耗时 want count{result=ok} 1:\n%s", body)
	}
	if !strings.Contains(body, `wokbuddy_request_duration_seconds_count{result="error"} 1`) {
		t.Errorf("失败尝试耗时 want count{result=error} 1:\n%s", body)
	}
	if !strings.Contains(body, `wokbuddy_upstream_rotations_total{reason="soft_rate"} 1`) {
		t.Errorf("429 换号应记 soft_rate 一次:\n%s", body)
	}
	// 请求结果口径：429 记 error、200 记 ok（逐尝试，与 usage 同源）。
	if !strings.Contains(body, `wokbuddy_requests_total{model="glm-5.2",result="error"} 1`) {
		t.Errorf("want one error attempt:\n%s", body)
	}
	if !strings.Contains(body, `wokbuddy_requests_total{model="glm-5.2",result="ok"} 1`) {
		t.Errorf("want one ok attempt:\n%s", body)
	}
}

// TestLabelPairEscaping 标签值中的反斜杠/双引号/换行按 Prometheus 文本格式转义，
// 普通值走快路径原样输出。
func TestLabelPairEscaping(t *testing.T) {
	if got, want := labelPair("model", "a\"b\\c\nd"), `{model="a\"b\\c\nd"}`; got != want {
		t.Errorf("escaping: got %q want %q", got, want)
	}
	if got, want := labelPair("model", "glm-5.2"), `{model="glm-5.2"}`; got != want {
		t.Errorf("plain: got %q want %q", got, want)
	}
	if got, want := labelPair("result", "ok", "le", "+Inf"), `{result="ok",le="+Inf"}`; got != want {
		t.Errorf("pair: got %q want %q", got, want)
	}
}

// TestDurationHistogramBuckets histogram 累积分桶正确：观测值落入 le>=自身的最小桶
// （边界 le==观测值计入该桶），+Inf 等于总观测数，且 ok/error 分流。
func TestDurationHistogramBuckets(t *testing.T) {
	m := newMetricsRegistry()
	m.observeRequestDuration(0.03, true)
	m.observeRequestDuration(0.25, true) // 恰好命中桶边界
	m.observeRequestDuration(3, true)
	m.observeRequestDuration(0.02, false)
	m.observeRequestDuration(100, false)

	var b strings.Builder
	(&Handler{metrics: m}).writeDurationMetrics(&b)
	body := b.String()
	sumOK := formatFloat(0.03 + 0.25 + 3)
	sumErr := formatFloat(0.02 + 100)
	for _, want := range []string{
		`wokbuddy_request_duration_seconds_bucket{result="ok",le="0.05"} 1`,
		`wokbuddy_request_duration_seconds_bucket{result="ok",le="0.1"} 1`,
		`wokbuddy_request_duration_seconds_bucket{result="ok",le="0.25"} 2`,
		`wokbuddy_request_duration_seconds_bucket{result="ok",le="0.5"} 2`,
		`wokbuddy_request_duration_seconds_bucket{result="ok",le="5"} 3`,
		`wokbuddy_request_duration_seconds_bucket{result="ok",le="+Inf"} 3`,
		`wokbuddy_request_duration_seconds_sum{result="ok"} ` + sumOK,
		`wokbuddy_request_duration_seconds_count{result="ok"} 3`,
		`wokbuddy_request_duration_seconds_bucket{result="error",le="0.05"} 1`,
		`wokbuddy_request_duration_seconds_bucket{result="error",le="+Inf"} 2`,
		`wokbuddy_request_duration_seconds_sum{result="error"} ` + sumErr,
		`wokbuddy_request_duration_seconds_count{result="error"} 2`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q\n---\n%s", want, body)
		}
	}
}

// TestMetricsAccountsPartition state 是互斥分区：在途满载号只计入 in_flight_full，
// 不再被 ok 重复计；sum 等于账号总数。
func TestMetricsAccountsPartition(t *testing.T) {
	p := testPoolWith(
		&auth.Auth{UID: "full", AccessToken: "at", ExpiresAt: 9999999999},
		&auth.Auth{UID: "free", AccessToken: "at", ExpiresAt: 9999999999},
	)
	p.SetMaxInFlight(1)
	if !p.Acquire("full") {
		t.Fatal("Acquire(full) failed")
	}
	defer p.Release("full")

	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	body := getMetrics(t, h, nil).Body.String()
	if !strings.Contains(body, `wokbuddy_accounts{state="ok"} 1`) {
		t.Errorf("ok 应排除在途满载号，want 1:\n%s", body)
	}
	if !strings.Contains(body, `wokbuddy_accounts{state="in_flight_full"} 1`) {
		t.Errorf("in_flight_full want 1:\n%s", body)
	}
}

// TestMetricsPoolGauges 池级 gauge：在途、冷却账号数与按模型 TTFT 均值。
func TestMetricsPoolGauges(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.RecordModelHealth("u1", "glm-5.2", 1200, true, true)
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})

	body := getMetrics(t, h, nil).Body.String()
	for _, want := range []string{
		"# TYPE wokbuddy_pool_in_flight gauge",
		"wokbuddy_pool_in_flight 0",
		"# TYPE wokbuddy_pool_cooling gauge",
		"wokbuddy_pool_cooling 0",
		"# TYPE wokbuddy_model_ttft_ms gauge",
		`wokbuddy_model_ttft_ms{model="glm-5.2"} 1200`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics body missing %q\n---\n%s", want, body)
		}
	}
}
