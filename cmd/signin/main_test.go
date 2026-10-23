package main

import (
	"errors"
	"fmt"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

var errTransport = errors.New("dial tcp: connection refused")

// upstreamError 构造完整 *upstream.Error（避免测试字面量漏字段触发 exhaustruct）。
func upstreamError(kind upstream.ErrKind, status int, msg string) *upstream.Error {
	return &upstream.Error{Kind: kind, Status: status, Msg: msg, Code: "", RetryAfter: 0}
}

// classifyRefreshFailure 只把凭证不可用的错误标成 AUTH_INVALID；上游 5xx/限流/
// 传输层抖动不能被误报成「需要重新登录」。
func TestClassifyRefreshFailure(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "session dead",
			err:  upstreamError(upstream.ErrSessionDead, 401, "offline"),
			want: "AUTH_INVALID",
		},
		{
			name: "wrapped session dead",
			err:  fmt.Errorf("refresh: %w", upstreamError(upstream.ErrSessionDead, 401, "offline")),
			want: "AUTH_INVALID",
		},
		{
			name: "upstream 5xx",
			err:  upstreamError(upstream.ErrServer, 500, "boom"),
			want: "FAIL",
		},
		{
			name: "upstream rate limit",
			err:  upstreamError(upstream.ErrSoftRate, 429, "slow down"),
			want: "FAIL",
		},
		{
			name: "transport error",
			err:  errTransport,
			want: "FAIL",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			if got := classifyRefreshFailure(testCase.err); got != testCase.want {
				t.Errorf("classifyRefreshFailure(%v)=%q want %q", testCase.err, got, testCase.want)
			}
		})
	}
}
