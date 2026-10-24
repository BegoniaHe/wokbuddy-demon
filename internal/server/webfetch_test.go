package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/livecfg"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

func newWebFetchTestHandler(t *testing.T, enabled bool, respond func(*http.Request) (*http.Response, error)) *Handler {
	t.Helper()

	client := &upstream.Client{
		HTTP:          &http.Client{Transport: roundTripFunc(respond)},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}

	return NewHandler(&Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: client,
		Live:     livecfg.New(livecfg.Snapshot{WebFetchEndpoint: enabled}),
	})
}

func postFetch(t *testing.T, handler *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/tools/fetch", strings.NewReader(body)))

	return rec
}

// TestWebFetchEndpointDisabled 默认关闭 → 404，且不打上游。
func TestWebFetchEndpointDisabled(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	handler := newWebFetchTestHandler(t, false, func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return jsonResponse(200, `{}`), nil
	})

	if rec := postFetch(t, handler, `{"url":"https://93.184.216.34/a"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if calls.Load() != 0 {
		t.Fatalf("calls=%d want 0", calls.Load())
	}
}

// TestWebFetchRejectsNonCNAPIKey 命名密钥绑定 global 且 webfetch 只走 CN 账号：
// 入口直接 400，不消耗 CN 额度（与 /v1/search/messages 同口径）。
func TestWebFetchRejectsNonCNAPIKey(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	handler := newWebFetchTestHandler(t, true, func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return jsonResponse(200, `{"code":0,"content_type":"text/html","content":"ok"}`), nil
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/tools/fetch",
		strings.NewReader(`{"url":"https://93.184.216.34/a"}`))
	req = req.WithContext(context.WithValue(req.Context(), keyUsageKey{}, &keyUsage{id: "k1", realm: "global"}))
	rec := httptest.NewRecorder()

	handler.webFetchEndpointHandler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if calls.Load() != 0 {
		t.Fatalf("calls=%d want 0", calls.Load())
	}
}

// TestWebFetchRejectsSSRFTargets 私网/回环/CGNAT/云元数据/IPv6/file/ftp/userinfo 全部拒绝。
func TestWebFetchRejectsSSRFTargets(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	handler := newWebFetchTestHandler(t, true, func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return jsonResponse(200, `{"code":0,"content":"x"}`), nil
	})

	targets := []string{
		"http://127.0.0.1/",
		"http://10.0.0.1/",
		"http://169.254.169.254/latest/meta-data/",
		"http://100.64.0.1/",
		"http://[::1]/",
		"file:///etc/passwd",
		"ftp://example.com/a",
		"http://user:pass@93.184.216.34/",
		"http://0.0.0.0/",
	}

	for _, target := range targets {
		body := `{"url":` + strconvQuote(target) + `}`

		rec := postFetch(t, handler, body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid url") {
			t.Fatalf("target=%s code=%d body=%s", target, rec.Code, rec.Body)
		}
	}

	if calls.Load() != 0 {
		t.Fatalf("ssrf targets must not reach upstream, calls=%d", calls.Load())
	}
}

// TestWebFetchPublicPassthrough 公网 URL 转发官方端点，返回 DSH 需要的 title/content。
func TestWebFetchPublicPassthrough(t *testing.T) {
	t.Parallel()

	var gotPath, gotBody string

	handler := newWebFetchTestHandler(t, true, func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		raw := make([]byte, 4096)
		n, _ := r.Body.Read(raw)
		gotBody = string(raw[:n])

		return jsonResponse(200, `{"code":0,"msg":"OK","url":"https://93.184.216.34/article",`+
			`"content_type":"text/markdown","content":"# Title\n\nbody"}`), nil
	})

	rec := postFetch(t, handler, `{"url":"https://93.184.216.34/article"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if gotPath != "/agenttool/v1/webfetch" {
		t.Fatalf("path=%s", gotPath)
	}

	if !strings.Contains(gotBody, `"url":"https://93.184.216.34/article"`) {
		t.Fatalf("upstream body=%s", gotBody)
	}

	out := rec.Body.String()
	if !strings.Contains(out, `"title":"Title"`) || !strings.Contains(out, `"content":"# Title\n\nbody"`) {
		t.Fatalf("response=%s", out)
	}
}

// TestWebFetchMissingContentIs502 上游缺 content → 502，错误不泄露 URL。
func TestWebFetchMissingContentIs502(t *testing.T) {
	t.Parallel()

	handler := newWebFetchTestHandler(t, true, func(*http.Request) (*http.Response, error) {
		return jsonResponse(200, `{"code":0,"content_type":"text/html"}`), nil
	})

	rec := postFetch(t, handler, `{"url":"https://93.184.216.34/secret-path"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if strings.Contains(rec.Body.String(), "93.184.216.34") || strings.Contains(rec.Body.String(), "secret-path") {
		t.Fatalf("error leaked target url: %s", rec.Body)
	}
}

// TestWebFetchFinalURLRevalidated provider 重定向后的最终 URL 落内网 → 502。
func TestWebFetchFinalURLRevalidated(t *testing.T) {
	t.Parallel()

	handler := newWebFetchTestHandler(t, true, func(*http.Request) (*http.Response, error) {
		return jsonResponse(200, `{"code":0,"content_type":"text/html","content":"ok","url":"http://127.0.0.1/"}`), nil
	})

	rec := postFetch(t, handler, `{"url":"https://93.184.216.34/a"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

// TestWebFetchDeterministicErrorAfterRefreshNotRotated 401 → refresh → 重放返回非文本：
// 确定性失败必须直接终态，不能当传输层错误换号（每号都会重复同一份响应，白烧额度）。
func TestWebFetchDeterministicErrorAfterRefreshNotRotated(t *testing.T) {
	t.Parallel()

	var fetchCalls, refreshCalls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if strings.HasSuffix(r.URL.Path, "/v2/plugin/auth/token/refresh") {
			refreshCalls.Add(1)

			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"accessToken":"at-new","refreshToken":"rt-new","expiresIn":3600}}`))

			return
		}

		if fetchCalls.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":401,"msg":"unauthorized"}`))

			return
		}

		_, _ = w.Write([]byte(`{"code":0,"content_type":"image/png","content":"AAAA"}`))
	}))
	t.Cleanup(srv.Close)

	client := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	dir := t.TempDir()
	first := &auth.Auth{
		UID: "u1", AccessToken: "at-old", RefreshToken: "rt-old", ExpiresAt: 9999999999,
		FilePath: filepath.Join(dir, "u1.json"),
	}
	second := &auth.Auth{
		UID: "u2", AccessToken: "at-u2", RefreshToken: "rt-u2", ExpiresAt: 9999999999,
		FilePath: filepath.Join(dir, "u2.json"),
	}
	handler := NewHandler(&Config{
		Pool:     testPoolWith(first, second),
		Upstream: client,
		Live:     livecfg.New(livecfg.Snapshot{WebFetchEndpoint: true}),
	})

	rec := postFetch(t, handler, `{"url":"https://93.184.216.34/a.png"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	// 原请求 + 重放：确定性失败不得换号（否则第二号会再走一轮 401/refresh）。
	if got := fetchCalls.Load(); got != 2 {
		t.Fatalf("fetch calls=%d want 2（不换号）", got)
	}

	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("refresh calls=%d want 1", got)
	}
}

// TestWebFetchNoHealthyAccountIs503 池里没有可服务账号时保留 503，不能降级成
// 502（会误导客户端把「等号恢复」当成上游坏响应）。
func TestWebFetchNoHealthyAccountIs503(t *testing.T) {
	t.Parallel()

	handler := NewHandler(&Config{
		Pool: testPoolWith(),
		Upstream: &upstream.Client{
			HTTP:          &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, context.Canceled })},
			ChatBaseCN:    "https://fake.example",
			BillingBaseCN: "https://fake.example",
		},
		Live: livecfg.New(livecfg.Snapshot{WebFetchEndpoint: true}),
	})

	rec := postFetch(t, handler, `{"url":"https://93.184.216.34/a"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

func strconvQuote(s string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
}

// TestWebFetchForwardsSessionHeaders 入站会话头族原样透传（官方 fetchContentViaApi
// 同样携带；上游按会话聚合抓取流量）。
func TestWebFetchForwardsSessionHeaders(t *testing.T) {
	t.Parallel()

	var got http.Header

	handler := newWebFetchTestHandler(t, true, func(r *http.Request) (*http.Response, error) {
		got = r.Header.Clone()

		return jsonResponse(200, `{"code":0,"content_type":"text/plain","content":"ok"}`), nil
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/tools/fetch",
		strings.NewReader(`{"url":"https://93.184.216.34/a"}`))
	req.Header.Set("X-Conversation-Id", "conv-1")
	req.Header.Set("X-Conversation-Request-Id", "req-1")
	req.Header.Set("X-Trace-Id", "trace-1")

	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if got.Get("X-Conversation-Id") != "conv-1" || got.Get("X-Conversation-Request-Id") != "req-1" ||
		got.Get("X-Trace-Id") != "trace-1" {
		t.Fatalf("conversation headers=%q/%q/%q",
			got.Get("X-Conversation-Id"), got.Get("X-Conversation-Request-Id"), got.Get("X-Trace-Id"))
	}
}

// TestWebFetchMalformedResponseNotRotated 200 + 非 JSON 是确定性失败：不换号
// （换号只会重复同一份坏响应，白烧额度）。用两个账号，确保确实存在换号的机会。
func TestWebFetchMalformedResponseNotRotated(t *testing.T) {
	t.Parallel()

	var fetchCalls atomic.Int32

	client := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			fetchCalls.Add(1)

			return jsonResponse(200, `<html>bad gateway</html>`), nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	handler := NewHandler(&Config{
		Pool: testPoolWith(
			&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
			&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
		),
		Upstream: client,
		Live:     livecfg.New(livecfg.Snapshot{WebFetchEndpoint: true}),
	})

	rec := postFetch(t, handler, `{"url":"https://93.184.216.34/a"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if got := fetchCalls.Load(); got != 1 {
		t.Fatalf("fetch calls=%d want 1（确定性失败不换号）", got)
	}
}
