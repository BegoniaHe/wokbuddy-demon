package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/logincode"
)

// loginReply 起一个固定响应体的登录端点。
func loginReply(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	return srv
}

func TestPollTokenOncePending(t *testing.T) {
	t.Parallel()

	srv := loginReply(t, http.StatusOK, `{"code":11217,"msg":"login ing"}`)

	_, _, err := pollTokenOnce(srv.URL, "s", func(*http.Request) {}, srv.Client())
	if !errors.Is(err, errLoginPending) {
		t.Fatalf("err=%v want errLoginPending", err)
	}
}

func TestPollTokenOnceTerminal(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		code   int
		msg    string
		status int
	}{
		{"席位超限", logincode.LicenseSeatLimit, "许可席位已满", http.StatusOK},
		{"许可过期", logincode.LicenseExpired, "许可已过期", http.StatusBadRequest},
		{"试用过期", logincode.TrialExpired, "试用已结束", http.StatusForbidden},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			t.Parallel()

			srv := loginReply(t, item.status, `{"code":`+strconv.Itoa(item.code)+`,"msg":"`+item.msg+`"}`)

			_, _, err := pollTokenOnce(srv.URL, "s", func(*http.Request) {}, srv.Client())
			if err == nil {
				t.Fatal("terminal code should fail immediately")
			}

			if errors.Is(err, errLoginPending) {
				t.Errorf("终态不应包装成 pending: %v", err)
			}

			var codeErr *logincode.Error
			if !errors.As(err, &codeErr) || codeErr.Code != item.code {
				t.Fatalf("err=%v want code=%d", err, item.code)
			}

			if !strings.Contains(err.Error(), item.msg) {
				t.Errorf("err=%q 未透出上游 msg %q", err.Error(), item.msg)
			}
		})
	}
}

// roundTripFunc 构造固定错误的 transport，用于验证传输错误语义。
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

var errTestTransport = errors.New("network down")

// TestPollTokenOnceHTTPFailures 非信封 4xx 不再当作 pending：4xx 透出 HTTP 状态
// 与 state 过期提示；408/429 是暂时性拒绝，保留可重试语义、不引导重跑 login url；
// 5xx/传输错误保持原语义。
//
//nolint:cyclop // 表驱动多状态断言。
func TestPollTokenOnceHTTPFailures(t *testing.T) {
	t.Parallel()

	rejected := []int{
		http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusNotFound,
	}
	for _, status := range rejected {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			t.Parallel()

			srv := loginReply(t, status, `not json`)

			_, _, err := pollTokenOnce(srv.URL, "s", func(*http.Request) {}, srv.Client())
			if err == nil {
				t.Fatal("非信封 4xx 应返回错误")
			}

			if errors.Is(err, errLoginPending) {
				t.Errorf("非信封 4xx 不应包装成 pending: %v", err)
			}

			for _, want := range []string{
				"HTTP " + strconv.Itoa(status),
				"http_error: upstream " + strconv.Itoa(status),
				"state 可能已过期，请重新运行 login url",
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err=%q missing %q", err.Error(), want)
				}
			}
		})
	}

	for _, status := range []int{http.StatusRequestTimeout, http.StatusTooManyRequests} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			t.Parallel()

			srv := loginReply(t, status, `not json`)

			_, _, err := pollTokenOnce(srv.URL, "s", func(*http.Request) {}, srv.Client())
			if err == nil {
				t.Fatal("暂时性拒绝应返回错误")
			}

			if errors.Is(err, errLoginPending) {
				t.Errorf("暂时性拒绝不应包装成 pending: %v", err)
			}

			for _, want := range []string{
				"HTTP " + strconv.Itoa(status),
				"http_error: upstream " + strconv.Itoa(status),
				"temporarily unavailable",
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err=%q missing %q", err.Error(), want)
				}
			}

			if strings.Contains(err.Error(), "state 可能已过期") {
				t.Errorf("暂时性拒绝不得引导重跑 login url: %v", err)
			}
		})
	}

	gateway := loginReply(t, http.StatusServiceUnavailable, `{"code":0,"data":{}}`)

	_, _, err := pollTokenOnce(gateway.URL, "s", func(*http.Request) {}, gateway.Client())
	if errors.Is(err, errLoginPending) {
		t.Errorf("5xx 不应包装成 pending: %v", err)
	}

	if err == nil || !strings.Contains(err.Error(), "token endpoint error") {
		t.Errorf("5xx err=%v want token endpoint error", err)
	}

	transport := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errTestTransport // 任意传输错误；语义应保持 token endpoint error
	})}

	_, _, err = pollTokenOnce("https://example.invalid", "s", func(*http.Request) {}, transport)
	if errors.Is(err, errLoginPending) {
		t.Errorf("传输错误不应包装成 pending: %v", err)
	}

	if err == nil || !strings.Contains(err.Error(), "token endpoint error") {
		t.Errorf("传输错误 err=%v want token endpoint error", err)
	}
}
