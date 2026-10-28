// chat_body_gzip_test.go chat 出站 body gzip（issue #365）。
package upstream

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// capturedChatRequest 一次 chat 出站尝试的可观测项。
type capturedChatRequest struct {
	url             string
	contentEncoding string
	contentType     string
	acceptEncoding  string
	body            []byte
}

// chatGzipTransport 记录每次尝试并按 statuses 依次回状态码；最后一个状态码复用。
type chatGzipTransport struct {
	mu       sync.Mutex
	captured []capturedChatRequest
	statuses []int
	calls    int
}

func (tr *chatGzipTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(r.Body)

	tr.mu.Lock()
	tr.captured = append(tr.captured, capturedChatRequest{
		url:             r.URL.String(),
		contentEncoding: r.Header.Get("Content-Encoding"),
		contentType:     r.Header.Get("Content-Type"),
		acceptEncoding:  r.Header.Get("Accept-Encoding"),
		body:            body,
	})

	status := http.StatusOK
	if tr.calls < len(tr.statuses) {
		status = tr.statuses[tr.calls]
	}

	tr.calls++
	tr.mu.Unlock()

	if status < http.StatusBadRequest {
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: {\"id\":\"x\"}\n\n")),
		}, nil
	}

	return jsonResp(status, `{"code":5000,"msg":"upstream busy"}`), nil
}

// TestChatBodyGzipCompressesOnceAndReusesAcrossOrigins chat 出站 body 必须 gzip
// （首两字节 1f 8b + Content-Encoding: gzip），且压缩只做一次——跨 origin 重放
// 复用同一份字节；同时不得手设 Accept-Encoding（手设会关掉 Go 透明解压，SSE 直接坏）。
func TestChatBodyGzipCompressesOnceAndReusesAcrossOrigins(t *testing.T) {
	t.Parallel()

	transport := &chatGzipTransport{statuses: []int{http.StatusServiceUnavailable, http.StatusOK}}
	client := &Client{
		HTTP:     &http.Client{Transport: transport},
		ChatHTTP: &http.Client{Transport: transport},
		Profile:  testProfileSet(t),
		// ChatBaseCN 留空 = 内置默认域（chatOrigins 返回默认域 + 回落链，可触发重放）。
	}

	rc, status, _, err := client.ChatStream(
		&auth.Auth{AccessToken: "at", UID: "u1"},
		[]byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`),
		"", ChatMeta{},
	)
	if err != nil || status != http.StatusOK {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}

	if rc != nil {
		rc.Close()
	}

	transport.mu.Lock()
	defer transport.mu.Unlock()

	if len(transport.captured) != 2 {
		t.Fatalf("attempts=%d want 2（首次 503 后应换 origin 重放）", len(transport.captured))
	}

	for i, hit := range transport.captured {
		assertGzippedChatAttempt(t, i, hit)
	}

	// 压缩只做一次：两条尝试的 body 字节完全一致。
	if !bytes.Equal(transport.captured[0].body, transport.captured[1].body) {
		t.Error("跨 origin 重放的 body 字节不一致（压缩被重复执行）")
	}

	assertGunzippedModel(t, transport.captured[0].body, "glm-5.2")
}

// assertGunzippedModel 断言 body 可 gunzip 出原 JSON，且 model 字段与请求一致。
func assertGunzippedModel(t *testing.T, body []byte, wantModel string) {
	t.Helper()

	reader, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}

	raw, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("解压后不是合法 JSON: %v (%s)", err, raw)
	}

	if payload["model"] != wantModel {
		t.Errorf("解压后 model=%v want %s", payload["model"], wantModel)
	}
}

// assertGzippedChatAttempt 断言单次尝试带 gzip 体与编码头，且没有手设 Accept-Encoding。
func assertGzippedChatAttempt(t *testing.T, index int, hit capturedChatRequest) {
	t.Helper()

	if hit.contentEncoding != "gzip" {
		t.Errorf("attempt %d Content-Encoding=%q want gzip", index, hit.contentEncoding)
	}

	if len(hit.body) < 2 || hit.body[0] != 0x1f || hit.body[1] != 0x8b {
		t.Errorf("attempt %d body 首两字节不是 gzip 魔数: % x", index, hit.body[:min(2, len(hit.body))])
	}

	if hit.acceptEncoding != "" {
		t.Errorf("attempt %d 不应手设 Accept-Encoding，got %q", index, hit.acceptEncoding)
	}

	if hit.contentType != "application/json" {
		t.Errorf("attempt %d Content-Type=%q want application/json", index, hit.contentType)
	}
}

// TestChatBodyNotGzippedOnCustomBase 显式覆盖 chat base（非官方中继）时不压缩：
// 与官方「自定义 base_url 跳过压缩」同义。
func TestChatBodyNotGzippedOnCustomBase(t *testing.T) {
	t.Parallel()

	transport := &chatGzipTransport{}
	client := &Client{
		HTTP:       &http.Client{Transport: transport},
		ChatHTTP:   &http.Client{Transport: transport},
		ChatBaseCN: "https://relay.example.invalid",
		Profile:    testProfileSet(t),
	}

	rc, status, _, err := client.ChatStream(
		&auth.Auth{AccessToken: "at", UID: "u1"},
		[]byte(`{"model":"glm-5.2","messages":[]}`),
		"", ChatMeta{},
	)
	if err != nil || status != http.StatusOK {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}

	if rc != nil {
		rc.Close()
	}

	transport.mu.Lock()
	defer transport.mu.Unlock()

	if len(transport.captured) != 1 {
		t.Fatalf("attempts=%d want 1（自定义 base 不回落）", len(transport.captured))
	}

	hit := transport.captured[0]
	if hit.contentEncoding != "" {
		t.Errorf("自定义 base Content-Encoding=%q want empty", hit.contentEncoding)
	}

	if len(hit.body) >= 2 && hit.body[0] == 0x1f && hit.body[1] == 0x8b {
		t.Error("自定义 base 不应压缩 body")
	}
}
