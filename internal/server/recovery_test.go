// recovery_test.go panic 兜底与 pprof 鉴权（issue #146）。
package server

import (
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// quietPanicLog 把 panic 日志写入 discard，避免 go test 输出整段 stack。
func quietPanicLog(t *testing.T) {
	t.Helper()

	prev := log.Writer()

	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(prev) })
}

// TestWithRecoveryReturns500 panic 后未写头 → 500 + OpenAI 错误体，进程不崩。
func TestWithRecoveryReturns500(t *testing.T) {
	t.Parallel()
	quietPanicLog(t)

	h := NewHandler(&Config{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	h.withRecovery(func(http.ResponseWriter, *http.Request) { panic("boom") })(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d want 500 body=%s", rec.Code, rec.Body)
	}

	if !strings.Contains(rec.Body.String(), `"code":"internal_error"`) {
		t.Errorf("body=%s want internal_error", rec.Body)
	}
}

// TestWithRecoveryAnthropicShape 入站为 Anthropic 端点时错误体保持 Anthropic 形态。
func TestWithRecoveryAnthropicShape(t *testing.T) {
	t.Parallel()
	quietPanicLog(t)

	h := NewHandler(&Config{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	h.withRecovery(func(http.ResponseWriter, *http.Request) { panic("boom") })(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d want 500", rec.Code)
	}

	body := rec.Body.String()
	if !strings.Contains(body, `"type":"error"`) || !strings.Contains(body, `"type":"api_error"`) {
		t.Errorf("body=%s want Anthropic error envelope", body)
	}
}

// TestWithRecoveryAbortHandlerRepanics ErrAbortHandler 是客户端断开语义，不得吞掉。
func TestWithRecoveryAbortHandlerRepanics(t *testing.T) {
	t.Parallel()

	handler := NewHandler(&Config{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)

	defer func() {
		rec := recover()

		err, ok := rec.(error)
		if !ok || !errors.Is(err, http.ErrAbortHandler) {
			t.Fatalf("recover=%v want http.ErrAbortHandler", rec)
		}
	}()

	handler.withRecovery(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) })(rec, req)
}

// TestWithRecoveryAfterHeaderWritten 已写头后只记录，不追加错误体/二次 WriteHeader。
func TestWithRecoveryAfterHeaderWritten(t *testing.T) {
	t.Parallel()
	quietPanicLog(t)

	h := NewHandler(&Config{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	h.withRecovery(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("partial"))

		panic("late boom")
	})(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("code=%d want 202", rec.Code)
	}

	if got := rec.Body.String(); got != "partial" {
		t.Fatalf("body=%q want %q (no panic payload appended)", got, "partial")
	}
}

// TestWithRecoveryAfterReadFrom 通过 ReadFrom（sendfile/io.Copy 路径）写出响应后
// panic，同样不得追加错误体：ReadFrom 绕过 recoveryWriter.Write，必须自行标记
// 已写头。
func TestWithRecoveryAfterReadFrom(t *testing.T) {
	t.Parallel()
	quietPanicLog(t)

	h := NewHandler(&Config{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)

	h.withRecovery(func(w http.ResponseWriter, _ *http.Request) {
		// LimitedReader 不实现 WriterTo，确保走到 recoveryWriter.ReadFrom。
		_, _ = io.Copy(w, &io.LimitedReader{R: strings.NewReader("partial"), N: 7})

		panic("late boom")
	})(rec, req)

	if got := rec.Body.String(); got != "partial" {
		t.Fatalf("body=%q want %q (no error envelope appended)", got, "partial")
	}
}

// TestWithRecoveryAfterFlush 只 Flush（尚未 Write）也已发送响应头，随后 panic 不得
// 再追加错误体。
func TestWithRecoveryAfterFlush(t *testing.T) {
	t.Parallel()
	quietPanicLog(t)

	h := NewHandler(&Config{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)

	h.withRecovery(func(w http.ResponseWriter, _ *http.Request) {
		w.(http.Flusher).Flush()

		panic("flush then boom")
	})(rec, req)

	if rec.Body.Len() != 0 {
		t.Fatalf("body=%q want empty (no error envelope appended)", rec.Body.String())
	}
}

// TestPprofAuth pprof 与 /metrics 同口径：api_key 非空时缺 token → 401，正确 → 200。
func TestPprofAuth(t *testing.T) {
	t.Parallel()

	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	handler := NewHandler(&Config{Pool: p, APIKey: "secret"})

	cases := []struct {
		path   string
		header string
		want   int
	}{
		{"/debug/pprof/", "", http.StatusUnauthorized},
		{"/debug/pprof/", "Bearer wrong", http.StatusUnauthorized},
		{"/debug/pprof/", "Bearer secret", http.StatusOK},
		{"/debug/pprof/cmdline", "Bearer secret", http.StatusOK},
	}
	for _, testCase := range cases {
		req := httptest.NewRequest(http.MethodGet, testCase.path, nil)
		if testCase.header != "" {
			req.Header.Set("Authorization", testCase.header)
		}

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != testCase.want {
			t.Errorf("%s auth=%q code=%d want %d", testCase.path, testCase.header, rec.Code, testCase.want)
		}
	}
}
