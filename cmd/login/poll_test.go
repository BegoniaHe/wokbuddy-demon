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

func TestPollTokenOnceHTTPFailures(t *testing.T) {
	t.Parallel()

	nonJSON := loginReply(t, http.StatusBadRequest, `not json`)

	_, _, err := pollTokenOnce(nonJSON.URL, "s", func(*http.Request) {}, nonJSON.Client())
	if !errors.Is(err, errLoginPending) {
		t.Errorf("非 JSON 400 err=%v want errLoginPending", err)
	}

	gateway := loginReply(t, http.StatusServiceUnavailable, `{"code":0,"data":{}}`)

	_, _, err = pollTokenOnce(gateway.URL, "s", func(*http.Request) {}, gateway.Client())
	if errors.Is(err, errLoginPending) {
		t.Errorf("5xx 不应包装成 pending: %v", err)
	}

	if err == nil || !strings.Contains(err.Error(), "token endpoint error") {
		t.Errorf("5xx err=%v want token endpoint error", err)
	}
}
