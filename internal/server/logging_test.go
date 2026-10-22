package server

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/apikey"
	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/httpauth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/reqlog"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// TestSetChatLogOutputAcceptsDifferentWriterTypes：输出目标会从 *os.File
// （默认 stdout）切换为 io.MultiWriter（生产 main 注入面板镜像）。atomic.Value
// 在具体类型不一致时 Store 会 panic，故用 atomic.Pointer[io.Writer]。
//
//nolint:paralleltest // 该用例改写进程级日志出口，必须串行执行
func TestSetChatLogOutputAcceptsDifferentWriterTypes(t *testing.T) {
	old := currentChatLogOutput()

	t.Cleanup(func() { SetChatLogOutput(old) })

	SetChatLogOutput(os.Stdout)

	multi := io.MultiWriter(io.Discard, io.Discard)
	SetChatLogOutput(multi)

	if got := currentChatLogOutput(); got != multi {
		t.Fatalf("currentChatLogOutput() = %T, want injected MultiWriter", got)
	}
}

func TestChatStatsReaderTokensFromUsage(t *testing.T) {
	t.Parallel()

	r := newChatStatsReaderSince(strings.NewReader(sseOK), time.Now())
	// 保证 TTFB 跨过时钟粒度：Windows 上纯内存读取不足 1ms，time.Since 可能取 0。
	time.Sleep(3 * time.Millisecond)

	if _, err := io.Copy(io.Discard, r); err != nil {
		t.Fatalf("copy: %v", err)
	}

	toks, ok := r.Tokens()
	if !ok || toks != 1 {
		t.Fatalf("tokens=%d ok=%v, want 1/true (from usage, not rune count)", toks, ok)
	}

	assertReaderUsage(t, r)
}

// assertReaderUsage 断言 reader 采信 usage 三元组且 TTFB 已记录。
func assertReaderUsage(t *testing.T, r *chatStatsReader) {
	t.Helper()

	usage := r.Usage()
	if !usage.HasPromptTokens || usage.PromptTokens != 1 || !usage.HasCompletionTokens || usage.CompletionTokens != 1 || !usage.HasTotalTokens || usage.TotalTokens != 2 {
		t.Fatalf("usage=%+v, want prompt=1 completion=1 total=2", usage)
	}

	if r.TTFB() <= 0 {
		t.Errorf("ttfb=%v want >0", r.TTFB())
	}
}

func TestChatStatsReaderNoUsage(t *testing.T) {
	t.Parallel()

	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"
	r := newChatStatsReaderSince(strings.NewReader(sse), time.Now())

	_, _ = io.Copy(io.Discard, r)
	if toks, ok := r.Tokens(); ok || toks != 0 {
		t.Errorf("tokens=%d ok=%v, want 0/false for missing usage", toks, ok)
	}
}

// TestChatStatsReaderSubstantiveOutput 实质输出判定：正文/工具调用/拒绝都算，
// 旧式 function_call 不能因新加的 reasoning-only 护栏被误判为空回答。
func TestChatStatsReaderSubstantiveOutput(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		line string
		want bool
	}{
		{"content", `data: {"choices":[{"delta":{"content":"hi"}}]}`, true},
		{"tool_calls", `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c"}]}}]}`, true},
		{"refusal", `data: {"choices":[{"delta":{"refusal":"no"}}]}`, true},
		{"function_call", `data: {"choices":[{"delta":{"function_call":{"name":"Bash"}}}]}`, true},
		{"empty_function_call", `data: {"choices":[{"delta":{"function_call":{}}}]}`, false},
		{"reasoning_only", `data: {"choices":[{"delta":{"reasoning":"think"}}]}`, false},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			var stats chatStatsReader
			stats.parseSSELine(testCase.line)

			if got := stats.HasSubstantiveOutput(); got != testCase.want {
				t.Errorf("HasSubstantiveOutput=%v want %v", got, testCase.want)
			}
		})
	}
}

func TestChatStatsReaderLastFrameUsageWins(t *testing.T) {
	t.Parallel()

	sse := "data: {\"usage\":{\"completion_tokens\":5}}\n\n" +
		"data: {\"usage\":{\"completion_tokens\":12}}\n\n" +
		"data: [DONE]\n\n"
	r := newChatStatsReaderSince(strings.NewReader(sse), time.Now())
	_, _ = io.Copy(io.Discard, r)

	toks, ok := r.Tokens()
	if !ok || toks != 12 {
		t.Fatalf("tokens=%d ok=%v, want 12 (last frame wins)", toks, ok)
	}
}

func TestChatStatsReaderTTFBOnlyOnDataFrame(t *testing.T) {
	t.Parallel()

	start := time.Now().Add(-2 * time.Second)

	var chatStatsReader2 chatStatsReader

	chatStatsReader2.start = start
	chatStatsReader2.parseSSELine("event: ping")

	if chatStatsReader2.TTFB() != 0 {
		t.Errorf("non-data line must not set TTFB: %v", chatStatsReader2.TTFB())
	}

	chatStatsReader2.parseSSELine("data: {\"choices\":[]}")

	first := chatStatsReader2.TTFB()
	if first < time.Second {
		t.Errorf("first data frame TTFB=%v want >=2s", first)
	}

	chatStatsReader2.parseSSELine("data: {\"choices\":[]}")

	if chatStatsReader2.TTFB() != first {
		t.Errorf("second frame changed TTFB: %v -> %v", first, chatStatsReader2.TTFB())
	}
}

func TestChatStatsReaderBytesPassthrough(t *testing.T) {
	t.Parallel()

	sse := "data: {\"content\":\"你好\"}\n\ndata: [DONE]\n\n"
	r := newChatStatsReaderSince(strings.NewReader(sse), time.Now())

	out, _ := io.ReadAll(r)
	if string(out) != sse {
		t.Errorf("passthrough mismatch:\n got %q\nwant %q", out, sse)
	}
}

func TestRequestMetricsRecordsStream(t *testing.T) {
	t.Parallel()
	client := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, sseOK, true
	})
	reqLog := reqlog.New(reqlog.Config{})
	handler := NewHandler(&Config{
		Pool:       testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:   client,
		RequestLog: reqLog,
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[]}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if got := rec.Header().Get("X-Request-ID"); !strings.HasPrefix(got, "req-") {
		t.Fatalf("X-Request-Id=%q", got)
	}

	s := reqLog.Snapshot()
	if s.Completed != 1 || s.Succeeded != 1 || s.Failed != 0 || len(s.Recent) != 1 {
		t.Fatalf("metrics = %+v", s)
	}

	assertRequestEvent(t, s.Recent[0])
}

// assertRequestEvent 断言单条归档事件的关键字段。
func assertRequestEvent(t *testing.T, e reqlog.Event) {
	t.Helper()

	if e.Outcome != reqlog.OutcomeSuccess || e.Status != http.StatusOK || !e.OK || e.Model != "glm-5.2" || e.TotalTokens != 2 || e.Attempts != 1 {
		t.Fatalf("event = %+v", e)
	}
}

// 调用来源必须进归档事件：X-Forwarded-For 首段（反代后真实客户端）+ 截断后的 UA。
func TestRequestMetricsCapturesClientInfo(t *testing.T) {
	t.Parallel()
	client := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, sseOK, true
	})

	trusted, err := httpauth.ParseTrustedProxies([]string{"127.0.0.1/32", "10.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}

	reqLog := reqlog.New(reqlog.Config{})
	handler := NewHandler(&Config{
		Pool:             testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:         client,
		RequestLog:       reqLog,
		RecordClientInfo: true,
		TrustedProxies:   trusted,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[]}`))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")
	req.Header.Set("User-Agent", "python-requests/2.31.0")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	snapshot := reqLog.Snapshot()
	if len(snapshot.Recent) != 1 {
		t.Fatalf("recent = %+v", snapshot.Recent)
	}

	if got := snapshot.Recent[0].ClientIP; got != "203.0.113.7" {
		t.Errorf("client ip = %q want 203.0.113.7", got)
	}

	if got := snapshot.Recent[0].UserAgent; got != "python-requests/2.31.0" {
		t.Errorf("user agent = %q", got)
	}
}

// 开关关闭时不采集来源（归档里不出现 IP/UA），但请求指标照常记录。
func TestRequestMetricsClientInfoDisabled(t *testing.T) {
	t.Parallel()
	client := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, sseOK, true
	})
	reqLog := reqlog.New(reqlog.Config{})
	handler := NewHandler(&Config{
		Pool:       testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:   client,
		RequestLog: reqLog,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[]}`))
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	req.Header.Set("User-Agent", "python-requests/2.31.0")
	handler.ServeHTTP(httptest.NewRecorder(), req)

	snapshot := reqLog.Snapshot()
	if len(snapshot.Recent) != 1 {
		t.Fatalf("recent = %+v", snapshot.Recent)
	}

	if snapshot.Recent[0].ClientIP != "" || snapshot.Recent[0].UserAgent != "" {
		t.Fatalf("client info recorded while disabled: %+v", snapshot.Recent[0])
	}

	if snapshot.Recent[0].Model != "glm-5.2" {
		t.Errorf("model = %q, metrics should be unaffected", snapshot.Recent[0].Model)
	}
}

// 无代理头时回落到 TCP 对端地址——直连部署下这是唯一来源线索。
func TestClientIPForLogFallsBackToRemoteAddr(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	req.RemoteAddr = "198.51.100.9:54321"
	if got := clientIPForLog(req, nil); got != "198.51.100.9" {
		t.Errorf("got %q want 198.51.100.9", got)
	}
	// 对端不在可信网段：伪造的转发头必须被忽略。
	req.Header.Set("X-Real-IP", "192.0.2.5")
	req.Header.Set("X-Forwarded-For", "192.0.2.9, 10.1.1.1")

	if got := clientIPForLog(req, nil); got != "198.51.100.9" {
		t.Errorf("untrusted peer got %q want 198.51.100.9", got)
	}

	if got := clientIPForLog(nil, nil); got != "" {
		t.Errorf("nil request got %q want empty", got)
	}
}

// 可信反代下才采信转发头：XFF 从右往左跳过可信跳，缺失时才回落 X-Real-IP。
func TestClientIPForLogUsesTrustedProxy(t *testing.T) {
	t.Parallel()

	trusted, err := httpauth.ParseTrustedProxies([]string{"127.0.0.1/32"})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("X-Real-IP", "192.0.2.5")

	if got := clientIPForLog(req, trusted); got != "192.0.2.5" {
		t.Errorf("X-Real-IP got %q want 192.0.2.5", got)
	}

	req.Header.Del("X-Real-IP")
	req.Header.Set("X-Forwarded-For", "192.0.2.9, 10.1.1.1")

	if got := clientIPForLog(req, trusted); got != "10.1.1.1" {
		t.Errorf("XFF got %q want rightmost non-trusted 10.1.1.1", got)
	}
}

// 超长 UA 落盘前截断：UA 是客户端可控自由文本，不截断会把归档行撑爆。
func TestCaptureClientInfoTruncatesUserAgent(t *testing.T) {
	t.Parallel()

	requestTrace2 := &requestTrace{}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.RemoteAddr = "198.51.100.9:1234"
	req.Header.Set("User-Agent", strings.Repeat("A", 5000))
	requestTrace2.captureClientInfo(req, nil)

	if len(requestTrace2.userAgent) != maxUserAgentLen {
		t.Fatalf("ua len = %d want %d", len(requestTrace2.userAgent), maxUserAgentLen)
	}
}

func TestRequestMetricsDetectsStreamErrorFrame(t *testing.T) {
	t.Parallel()

	const sseErr = "data: {\"error\":{\"message\":\"upstream failed\"}}\n\ndata: [DONE]\n\n"

	client := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, sseErr, true
	})
	reqLog := reqlog.New(reqlog.Config{})
	handler := NewHandler(&Config{
		Pool:       testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:   client,
		RequestLog: reqLog,
	})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[]}`)))

	s := reqLog.Snapshot()
	if len(s.Recent) != 1 || s.Recent[0].Outcome != reqlog.OutcomeStreamError || s.Recent[0].OK {
		t.Fatalf("stream error metrics = %+v", s.Recent)
	}
}

func TestParseModelFromBody(t *testing.T) {
	t.Parallel()

	if got := parseModelFromBody([]byte(`{"model":"deepseek-v4-flash","stream":true}`)); got != "deepseek-v4-flash" {
		t.Errorf("got %q", got)
	}

	if got := parseModelFromBody([]byte(`{}`)); got != "-" {
		t.Errorf("got %q want -", got)
	}

	if got := parseModelFromBody([]byte(`not json`)); got != "-" {
		t.Errorf("got %q want -", got)
	}
}

func TestCompletionTokensExtraction(t *testing.T) {
	t.Parallel()

	got := completionTokens(map[string]any{
		"usage": map[string]any{"prompt_tokens": 10.0, "completion_tokens": 234.0, "total_tokens": 244.0},
	})
	if got != 234 {
		t.Errorf("got %d want 234", got)
	}

	if got := completionTokens(map[string]any{}); got != -1 {
		t.Errorf("missing usage: got %d want -1", got)
	}

	if got := completionTokens(map[string]any{"usage": map[string]any{}}); got != -1 {
		t.Errorf("missing completion_tokens: got %d want -1", got)
	}
}

// logChatRow 测试用精简入口：只验证无扩展字段的基础表格行格式。
func logChatRow(out io.Writer, ttfb, total time.Duration, model, mode, uid, nick string, status, toks int) {
	logChatRowEx(out, true, ttfb, total, model, mode, uid, nick, status, toks, "", "", 0, 0, false, "", "")
}

func TestLogChatRowFormat(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	logChatRow(&buf, 412*time.Millisecond, 27100*time.Millisecond, "deepseek-v4-flash", "stream", "00e26541abcdef", "示例号", http.StatusOK, 1234)

	out := buf.String()
	for _, want := range []string{
		"| #", "deepseek-v4", "| stream |", "| 200 |", "示例号(00e26541)", "TTFB=412ms", "tok=1234", "tok/s   |", "total=",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("row missing %q:\n%s", want, out)
		}
	}

	if strings.Contains(out, "00e26541abcdef") {
		t.Errorf("full uid leaked: %s", out)
	}
}

func TestLogChatRowNoUsageShowsDash(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	logChatRow(&buf, 0, time.Second, "glm-5.2", "sync", "s1", "", http.StatusServiceUnavailable, -1)

	out := buf.String()
	for _, want := range []string{"TTFB=-", "tok=-", "tok=-      |", "| 503 |"} {
		if !strings.Contains(out, want) {
			t.Errorf("row missing %q:\n%s", want, out)
		}
	}
}

func TestLogChatRowExtendedFields(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	logChatRowEx(&buf, true, 10*time.Millisecond, 2*time.Second, "glm-5.3", "stream", "u123456789", "示例号",
		http.StatusOK, 42, "req-abc123", reqlog.OutcomeSuccess, 2, 1.25, true, "", "")

	out := buf.String()
	for _, want := range []string{"rid=req-abc123", "out=success", "try=2", "credit=1.2500"} {
		if !strings.Contains(out, want) {
			t.Errorf("extended row missing %q:\n%s", want, out)
		}
	}
	// 来源未采集时不追加 src 段（旧行格式不变，方便既有日志解析脚本继续工作）。
	if strings.Contains(out, "src=") {
		t.Errorf("empty source must not add src field:\n%s", out)
	}
}

// 调用来源必须落进流水行：IP 用裸值，UA 用 ShortUA 压缩后的客户端标签。
func TestLogChatRowSourceFields(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	logChatRowEx(&buf, true, 10*time.Millisecond, 2*time.Second, "glm-5.3", "stream", "u123456789", "示例号",
		http.StatusOK, 42, "req-abc123", reqlog.OutcomeSuccess, 1, 0, false,
		"203.0.113.7", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")

	out := buf.String()
	for _, want := range []string{`src=203.0.113.7`, `ua="Chrome/120.0.0.0"`} {
		if !strings.Contains(out, want) {
			t.Errorf("source row missing %q:\n%s", want, out)
		}
	}
	// 完整浏览器 UA 不该整段铺进流水行（只留客户端标签）。
	if strings.Contains(out, "AppleWebKit") {
		t.Errorf("full UA leaked into row:\n%s", out)
	}
}

// 只有 IP 没有 UA（非浏览器客户端不带头）时，UA 列显示 "-"，IP 照常展示。
func TestLogChatRowSourcePartial(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	logChatRow(&buf, 0, time.Second, "m", "sync", "u", "", 200, 1)

	out := buf.String()
	if strings.Contains(out, "src=") {
		t.Errorf("logChatRow has no source, want no src field:\n%s", out)
	}
}

func TestLogChatRowSeqIncrements(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	logChatRow(&buf, 0, time.Second, "m", "sync", "u", "", 200, 1)
	logChatRow(&buf, 0, time.Second, "m", "sync", "u", "", 200, 1)
	out := buf.String()

	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d:\n%s", len(lines), out)
	}

	first := strings.Fields(lines[0])[1]

	second := strings.Fields(lines[1])[1]
	if !strings.HasPrefix(first, "#") || !strings.HasPrefix(second, "#") {
		t.Fatalf("seq columns missing: %q %q", first, second)
	}

	if first == second {
		t.Errorf("seq not incremented: %q == %q", first, second)
	}
}

func TestChatLogsStreamRow(t *testing.T) {
	t.Parallel()
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	handler := NewHandler(&Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})

	var buf bytes.Buffer

	handler.chatLogOut = &buf
	handler.chatLogEnabled = true

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[]}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}

	out := buf.String()
	for _, want := range []string{"| stream |", "| 200 |", "| u1 ", "TTFB=", "tok=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("stream row missing %q:\n%s", want, out)
		}
	}

	if !strings.Contains(out, "tok=1") {
		t.Errorf("tok: want precise usage completion_tokens: %s", out)
	}
}

func TestChatLogsSyncRowTTFBDash(t *testing.T) {
	t.Parallel()
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	handler := NewHandler(&Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})

	var buf bytes.Buffer

	handler.chatLogOut = &buf
	handler.chatLogEnabled = true

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}

	out := buf.String()
	for _, want := range []string{"| sync |", "| 200 |", "TTFB=-", "tok=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("sync row missing %q:\n%s", want, out)
		}
	}
}

func TestChatLogsErrorRow(t *testing.T) {
	t.Parallel()
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 402, `{"code":1,"msg":"余额不足"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	handler := NewHandler(&Config{Pool: p, Upstream: up})

	var buf bytes.Buffer

	handler.chatLogOut = &buf
	handler.chatLogEnabled = true

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != 503 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	out := buf.String()
	for _, want := range []string{"| u1 ", "| 503 |", "tok=-"} {
		if !strings.Contains(out, want) {
			t.Errorf("error row missing %q:\n%s", want, out)
		}
	}
}

func TestHealthzDoesNotLogTableRow(t *testing.T) {
	t.Parallel()

	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	handler := NewHandler(&Config{Pool: p, Upstream: newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, sseOK, true
	})})

	var buf bytes.Buffer

	handler.chatLogOut = &buf
	handler.chatLogEnabled = true

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}

	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	rec3 := httptest.NewRecorder()
	handler.ServeHTTP(rec3, httptest.NewRequest(http.MethodGet, "/status", nil))

	out := buf.String()
	if strings.Contains(out, "| #") {
		t.Errorf("healthz/models/status must not emit table rows:\n%s", out)
	}
}

// TestChatStatsReaderLineTooLong chatStatsReader 自己也是按行读取点：单行超过
// upstream.MaxSSELineBytes 时必须上抛 ErrSSELineTooLong，不把超长行的部分字节
// 拷贝给下游透传（issue #113）。
func TestChatStatsReaderLineTooLong(t *testing.T) {
	t.Parallel()

	huge := strings.Repeat("x", upstream.MaxSSELineBytes+2048)
	s := newChatStatsReaderSince(strings.NewReader("data: "+huge+"\n\n"), time.Now())
	buf := make([]byte, 64*1024)

	var err error
	for range 1000 {
		_, err = s.Read(buf)
		if err != nil {
			break
		}
	}

	if !errors.Is(err, upstream.ErrSSELineTooLong) {
		t.Fatalf("err=%v want ErrSSELineTooLong", err)
	}
}

// TestChatStatsReaderNormalLines 回归：正常 SSE 行的解析与透传不受上限影响。
func TestChatStatsReaderNormalLines(t *testing.T) {
	t.Parallel()

	chatStatsReader2 := newChatStatsReaderSince(strings.NewReader(sseOK), time.Now())

	var out strings.Builder

	buf := make([]byte, 4096)
	for {
		n, err := chatStatsReader2.Read(buf)
		out.Write(buf[:n])

		if err != nil {
			break
		}
	}

	if !strings.Contains(out.String(), "data: [DONE]") {
		t.Errorf("透传内容不完整: %q", out.String())
	}

	if chatStatsReader2.TTFB() <= 0 {
		t.Errorf("TTFB 未记录")
	}
}

// 命名密钥请求把 key_id 写进归档；默认密钥保持空。
func TestRequestMetricsRecordsNamedKeyID(t *testing.T) {
	t.Parallel()

	store := apikey.New("")

	keyRecord, raw, err := store.Create("client-a")
	if err != nil {
		t.Fatal(err)
	}

	client := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	reqLog := reqlog.New(reqlog.Config{})
	handler := NewHandler(&Config{
		Pool:       testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:   client,
		Keys:       store,
		RequestLog: reqLog,
		APIKey:     "default-key",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+raw)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	snapshot := reqLog.Snapshot()
	if len(snapshot.Recent) != 1 || snapshot.Recent[0].KeyID != keyRecord.ID {
		t.Fatalf("key id = %q want %q", snapshot.Recent[0].KeyID, keyRecord.ID)
	}
}
