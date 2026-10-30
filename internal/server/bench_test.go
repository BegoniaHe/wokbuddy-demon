package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// benchmarkChatHandler 构造单账号 handler，上游固定回一段 SSE。
// 非流式请求也会走向上游的 SSE（聚合后回 JSON），所以两种模式共用同一份上游体；
// 差异只在客户端 body 的 stream 字段与下游写回形态。
func benchmarkChatHandler(b *testing.B) *Handler {
	b.Helper()

	client := &upstream.Client{HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "models.dev" {
			return jsonResponse(http.StatusOK, "{}"), nil
		}

		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(sseOK)),
		}, nil
	})}}

	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at-1", ExpiresAt: 9999999999})

	return NewHandler(&Config{Pool: p, Upstream: client})
}

func benchmarkChat(b *testing.B, body string) {
	b.Helper()

	h := benchmarkChatHandler(b)
	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	}
}

// BenchmarkChatNonStream 覆盖聚合成单个 JSON 响应的路径（gzip 出站 + SSE 聚合）。
func BenchmarkChatNonStream(b *testing.B) {
	benchmarkChat(b, `{"model":"glm-5.2","messages":[{"role":"user","content":"hello world"}]}`)
}

// BenchmarkChatStream 覆盖流式透传路径（gzip 出站 + 逐帧白名单重写）。
func BenchmarkChatStream(b *testing.B) {
	benchmarkChat(b, `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hello world"}]}`)
}
