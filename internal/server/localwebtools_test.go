package server

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

func TestValidateWebURLRejectsInternal(t *testing.T) {
	t.Parallel()

	bad := []string{
		"http://127.0.0.1/", "http://localhost/", "http://10.1.2.3/",
		"http://192.168.1.1/", "http://169.254.169.254/latest/meta-data/",
		"http://[::1]/", "file:///etc/passwd", "ftp://example.com/",
	}
	for _, raw := range bad {
		if err := validateWebURL(context.Background(), raw); err == nil {
			t.Errorf("%q should be rejected", raw)
		}
	}
}

func TestValidateWebURLAllowsPublicIP(t *testing.T) {
	t.Parallel()

	if err := validateWebURL(context.Background(), "https://93.184.216.34/path?q=1"); err != nil {
		t.Fatalf("public literal IP rejected: %v", err)
	}
}

func TestBlockedIPClasses(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.0.1", "169.254.1.1",
		"::1", "fc00::1", "fe80::1",
		// 非私网但不可公网访问：CGNAT/Tailscale、0/8、IETF、基准测试与保留段。
		"100.64.1.1", "100.100.100.100", "0.1.2.3", "192.0.0.1", "198.18.0.1", "240.0.0.1",
		// IPv6 文档/基准/丢弃段。
		"100::1", "2001:2::1", "2001:db8::1", "3fff::1",
		// IPv4 过渡/转换前缀：NAT64（含内网 IPv4）、Teredo、6to4。
		"64:ff9b::a00:1", "64:ff9b::7f00:1", "64:ff9b:1::a00:1", "2001::a00:1", "2002:a00:1::",
		// IPv4 兼容/转换形态与已废弃的 site-local。
		"::7f00:1", "::ffff:0:7f00:1", "fec0::1",
	} {
		ip := parseIP(t, raw)
		if !isBlockedIP(ip) {
			t.Errorf("%s should be blocked", raw)
		}
	}

	if isBlockedIP(parseIP(t, "93.184.216.34")) {
		t.Error("public IP blocked")
	}

	// NAT64 知名前缀按内嵌 IPv4 判定：公网目标必须放行（IPv6-only/DNS64 环境）。
	for _, raw := range []string{"64:ff9b::808:808", "64:ff9b::5db8:d822"} {
		if isBlockedIP(parseIP(t, raw)) {
			t.Errorf("%s (public NAT64) should be allowed", raw)
		}
	}
}

func parseIP(t *testing.T, raw string) net.IP {
	t.Helper()

	parsed := net.ParseIP(raw)
	if parsed == nil {
		t.Fatalf("bad ip %s", raw)
	}

	return parsed
}

// recordingUpstream 按调用顺序返回预置 SSE 流，并记录请求体。
func recordingUpstream(t *testing.T, streams []string, seen *[]string) *upstream.Client {
	t.Helper()

	var idx int

	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			*seen = append(*seen, string(raw))

			body := streams[len(streams)-1]
			if idx < len(streams) {
				body = streams[idx]
			}

			idx++

			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN: "https://fake.example", BillingBaseCN: "https://fake.example",
	}
}

const toolCallSSE = "data: {\"id\":\"chatcmpl-1\",\"created\":1753600000,\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"web_search\",\"arguments\":\"{\\\"query\\\":\\\"go\\\"}\"}}]}}]}\n\n" +
	"data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
	"data: [DONE]\n\n"

func TestLocalWebToolLoop(t *testing.T) {
	t.Parallel()

	var seen []string

	up := recordingUpstream(t, []string{toolCallSSE, sseOK}, &seen)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(&Config{Pool: p, Upstream: up, LocalWebTools: true, LocalWebToolRounds: 3})
	h.webSearch = func(_ context.Context, query string) (string, error) { return "result:" + query, nil }
	body := `{"model":"glm-5.2","input":"search go","tools":[{"type":"web_search"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if len(seen) != 2 {
		t.Fatalf("upstream calls=%d want 2", len(seen))
	}

	if !strings.Contains(seen[1], `"role":"tool"`) || !strings.Contains(seen[1], "result:go") {
		t.Errorf("second request missing tool result: %s", seen[1])
	}

	if !strings.Contains(rec.Body.String(), `"output_text":"你好"`) {
		t.Errorf("final response missing text: %s", rec.Body)
	}
}

func TestLocalWebToolRoundLimit(t *testing.T) {
	t.Parallel()

	var seen []string

	up := recordingUpstream(t, []string{toolCallSSE}, &seen)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(&Config{Pool: p, Upstream: up, LocalWebTools: true, LocalWebToolRounds: 1})
	h.webSearch = func(context.Context, string) (string, error) { return "result", nil }
	body := `{"model":"glm-5.2","input":"search","tools":[{"type":"web_search"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if len(seen) != 2 {
		t.Fatalf("upstream calls=%d want 2 (1 tool round + final)", len(seen))
	}

	var finalReq map[string]any
	if err := json.Unmarshal([]byte(seen[1]), &finalReq); err != nil {
		t.Fatal(err)
	}
	// tool_choice:"none" 会被出站归一化成「删除 tools + tool_choice」，
	// 这正是停止代跑后禁止模型继续调工具的最终形态。
	if _, has := finalReq["tools"]; has {
		t.Errorf("final request should not declare tools: %s", seen[1])
	}

	if _, has := finalReq["tool_choice"]; has {
		t.Errorf("final request should not carry tool_choice: %s", seen[1])
	}
}

func TestLocalWebToolsDisabled(t *testing.T) {
	t.Parallel()

	var seen []string

	up := recordingUpstream(t, []string{sseOK}, &seen)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(&Config{Pool: p, Upstream: up})
	body := `{"model":"glm-5.2","input":"hi","tools":[{"type":"web_search"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if len(seen) != 1 {
		t.Fatalf("upstream calls=%d want 1", len(seen))
	}
}

// 拨号阶段必须拦截内网 IP，闭合 validateWebURL 与连接之间的 DNS rebinding 窗口。
func TestDialWebFetchRejectsBlockedIP(t *testing.T) {
	t.Parallel()

	for _, addr := range []string{"127.0.0.1:80", "10.0.0.1:80", "192.168.1.1:443", "[::1]:80"} {
		if _, err := dialWebFetch(context.Background(), "tcp", addr); err == nil {
			t.Errorf("%s should be rejected", addr)
		} else if !strings.Contains(err.Error(), "内网") {
			t.Errorf("%s err=%v want 内网 address rejection", addr, err)
		}
	}
}

// TestDeclaredLocalWebToolsAdditionalTools Codex 0.162+ 把工具放在
// input.additional_tools 时，本地 web 工具识别必须与工具声明合并同口径。
func TestDeclaredLocalWebToolsAdditionalTools(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"model":"m",
		"input":[
			{"type":"additional_tools","role":"developer","tools":[{"type":"web_search"},{"type":"web_fetch"}]},
			{"type":"message","role":"user","content":"hi"}
		]
	}`)

	wanted, ok := declaredLocalWebTools(body)
	if !ok || !wanted["web_search"] || !wanted["web_fetch"] {
		t.Fatalf("wanted=%v ok=%v", wanted, ok)
	}
}

// twoToolCallSSE 单轮内返回两个本地工具调用：用于验证取消后不再执行剩余工具。
const twoToolCallSSE = "data: {\"id\":\"chatcmpl-1\",\"created\":1753600000,\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[" +
	"{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"web_search\",\"arguments\":\"{\\\"query\\\":\\\"a\\\"}\"}}," +
	"{\"index\":1,\"id\":\"call_2\",\"type\":\"function\",\"function\":{\"name\":\"web_search\",\"arguments\":\"{\\\"query\\\":\\\"b\\\"}\"}}" +
	"]}}]}\n\n" +
	"data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
	"data: [DONE]\n\n"

// TestLocalWebToolLoopStopsOnRequestCancel 入站请求取消后：工具侧 ctx 立刻收到取消
// 信号，剩余工具与后续上游轮次都不再发起（issue #415）。
func TestLocalWebToolLoopStopsOnRequestCancel(t *testing.T) {
	t.Parallel()

	var seen []string

	up := recordingUpstream(t, []string{twoToolCallSSE, sseOK}, &seen)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	handler := NewHandler(&Config{Pool: p, Upstream: up, LocalWebTools: true, LocalWebToolRounds: 3})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		calls     atomic.Int32
		sawCancel atomic.Bool
	)

	handler.webSearch = func(toolCtx context.Context, _ string) (string, error) {
		calls.Add(1)
		// 模拟客户端在工具抓取中断开。
		cancel()

		if err := toolCtx.Err(); err != nil {
			sawCancel.Store(true)

			return "", err
		}

		return "result", nil
	}

	body := `{"model":"glm-5.2","input":"search go","tools":[{"type":"web_search"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)).WithContext(ctx)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !sawCancel.Load() {
		t.Fatal("工具调用未收到入站请求的取消信号")
	}

	if n := calls.Load(); n != 1 {
		t.Fatalf("工具调用 %d 次，want 1（取消后不得继续发起剩余工具）", n)
	}

	if len(seen) != 1 {
		t.Fatalf("上游收到 %d 次请求，want 1（取消后不得再开新一轮）", len(seen))
	}
}

// TestLocalWebToolLoopPreservesRequestLevelError 内层请求级错误保留状态码与分类
// code，message 有界且不含完整上游 body（issue #416）。
func TestLocalWebToolLoopPreservesRequestLevelError(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		body     string
		wantCode string
	}{
		{
			name:     "content_blocked",
			body:     `{"code":"11-128","msg":"blocked by security policy ` + strings.Repeat("lorem ipsum ", 400) + `TAIL-MARKER"}`,
			wantCode: "content_blocked",
		},
		{
			name:     "prompt_too_long",
			body:     `{"code":11115,"msg":"prompt is too long: ` + strings.Repeat("lorem ipsum ", 400) + `TAIL-MARKER"}`,
			wantCode: "prompt_too_long",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			upstreamClient := newFakeUpstream(t, func(string) (int, string, bool) {
				return http.StatusBadRequest, testCase.body, false
			})
			p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
			// custom 提示词模式：content_blocked 不触发降级重试，单次上游调用即可观察。
			handler := NewHandler(&Config{
				Pool: p, Upstream: upstreamClient, LocalWebTools: true, LocalWebToolRounds: 3,
				PromptMode: "custom", PromptText: "SYS",
			})

			body := `{"model":"glm-5.2","input":"search go","tools":[{"type":"web_search"}]}`
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("code=%d want 400（保留内层状态码），body=%s", rec.Code, rec.Body)
			}

			var env struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("unmarshal: %v (body=%s)", err, rec.Body)
			}

			if env.Error.Code != testCase.wantCode {
				t.Errorf("error.code=%q want %q", env.Error.Code, testCase.wantCode)
			}

			if len(env.Error.Message) > localWebToolErrorMessageLimit {
				t.Errorf("message %d 字节超过上限 %d", len(env.Error.Message), localWebToolErrorMessageLimit)
			}

			if strings.Contains(env.Error.Message, "TAIL-MARKER") {
				t.Errorf("message 透传了完整上游 body 尾部: %q", env.Error.Message)
			}
		})
	}
}
