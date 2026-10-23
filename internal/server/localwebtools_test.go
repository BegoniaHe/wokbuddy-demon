package server

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
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
	} {
		ip := parseIP(t, raw)
		if !isBlockedIP(ip) {
			t.Errorf("%s should be blocked", raw)
		}
	}

	if isBlockedIP(parseIP(t, "93.184.216.34")) {
		t.Error("public IP blocked")
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
	h.webSearch = func(query string) (string, error) { return "result:" + query, nil }
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
	h.webSearch = func(string) (string, error) { return "result", nil }
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
