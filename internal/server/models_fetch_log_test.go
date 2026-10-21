// models_fetch_log_test.go 钉住 /v1/models 动态模型目录拉取失败的可观测性
// （上游同步窗口 issue #187）：失败必须留一条 WARN，负缓存期内不刷屏，
// 正常拉取不产生该 WARN。
package server

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// logBuffer 并发安全的日志捕获（fetchDynamicModels 内部两路探测并发打日志）。
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	written, err := b.buf.Write(p)
	if err != nil {
		return written, fmt.Errorf("log buffer write: %w", err)
	}

	return written, nil
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

// captureLog 把标准 logger 重定向到返回的 buffer，测试结束恢复。
func captureLog(t *testing.T) *logBuffer {
	t.Helper()

	out := &logBuffer{}
	prev := log.Writer()

	log.SetOutput(out)

	t.Cleanup(func() { log.SetOutput(prev) })

	return out
}

// modelsTestClient 造一个只服务模型目录探测的假上游。
func modelsTestClient(status int, body string) *upstream.Client {
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

// TestModelsFetchFailureLogsOnce 拉取失败必有 1 行 WARN，且负缓存期内连打 3 次不刷屏。
//
//nolint:paralleltest // 该用例改写进程级日志出口，必须串行执行
func TestModelsFetchFailureLogsOnce(t *testing.T) {
	out := captureLog(t)

	// 两路探测（企业端点 + /v3/config）都 500。
	client := modelsTestClient(http.StatusInternalServerError, `{"code":500,"msg":"boom"}`)
	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	// 必须注入私有缓存：不注入会落到进程级 dynamicModelsCache，测试之间互相污染。
	h := NewHandler(&Config{Pool: state, Upstream: client, modelsCache: &modelsCache{}})

	for range 3 {
		if models := h.fetchDynamicModels(); models != nil {
			t.Fatalf("failed fetch must fall back to nil (built-in seed), got %d models", len(models))
		}
	}

	if got := strings.Count(out.String(), "CN 模型目录拉取失败"); got != 1 {
		t.Fatalf("WARN count=%d want 1（负缓存期内不刷屏），log=%s", got, out.String())
	}

	if !strings.Contains(out.String(), "[server] /v1/models") {
		t.Errorf("WARN 必须带 [server] /v1/models 前缀，log=%s", out.String())
	}
}

// TestModelsFetchSuccessNoWarn 正常拉取不产生该 WARN。
//
//nolint:paralleltest // 该用例改写进程级日志出口，必须串行执行
func TestModelsFetchSuccessNoWarn(t *testing.T) {
	out := captureLog(t)

	client := modelsTestClient(http.StatusOK,
		`{"code":0,"data":{"models":[{"id":"glm-5.2"}],"agents":[{"name":"cli","models":["glm-5.2"]}]}}`)
	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	// 必须注入私有缓存：不注入会落到进程级 dynamicModelsCache，测试之间互相污染。
	h := NewHandler(&Config{Pool: state, Upstream: client, modelsCache: &modelsCache{}})

	models := h.fetchDynamicModels()
	if len(models) == 0 {
		t.Fatalf("success fetch must return models, log=%s", out.String())
	}

	if strings.Contains(out.String(), "CN 模型目录拉取失败") {
		t.Errorf("正常拉取不应产生失败 WARN，log=%s", out.String())
	}
}

// TestModelsFetchEmptyListLogsWarn 上游返回空列表同样按失败处理并留 WARN。
//
//nolint:paralleltest // 该用例改写进程级日志出口，必须串行执行
func TestModelsFetchEmptyListLogsWarn(t *testing.T) {
	out := captureLog(t)

	client := modelsTestClient(http.StatusOK, `{"code":0,"data":{"models":[]}}`)
	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	// 必须注入私有缓存：不注入会落到进程级 dynamicModelsCache，测试之间互相污染。
	h := NewHandler(&Config{Pool: state, Upstream: client, modelsCache: &modelsCache{}})

	if models := h.fetchDynamicModels(); models != nil {
		t.Fatalf("empty list must fall back to nil, got %d", len(models))
	}

	if !strings.Contains(out.String(), "CN 模型目录") {
		t.Errorf("空列表同样要有 WARN，log=%s", out.String())
	}
}
