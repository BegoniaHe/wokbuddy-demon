package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
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

// TestTruncKeepsValidUTF8 表格里的昵称常为中文：按字节截断必须回退到 rune 边界，
// 不能把多字节字符切成半个（终端会显示替换符）。
func TestTruncKeepsValidUTF8(t *testing.T) {
	t.Parallel()

	got := trunc("测试昵称", 11)
	if got != "测试昵" || !utf8.ValidString(got) {
		t.Fatalf("trunc=%q valid=%v want 测试昵", got, utf8.ValidString(got))
	}

	if got := trunc("uid-123", 36); got != "uid-123" {
		t.Fatalf("未超长时应原样返回: %q", got)
	}

	if got := short("错误：\n连接超时"); got != "错误： 连接超时" {
		t.Fatalf("换行应替换为空格: %q", got)
	}

	// 60 字节切点落在第 20 个汉字中间时必须回退，不能输出半个字符。
	if got := short("a" + strings.Repeat("错", 20)); !utf8.ValidString(got) {
		t.Fatalf("short 结果不是合法 UTF-8: %q", got)
	}
}

// 手写扁平 auth 常只带 accessToken、没有 refreshToken 与 expiresAt。此时
// NeedsRefresh 恒 true，若直接调用 RefreshTokenIfNeeded 会以 no refresh token
// 失败，并经 IsCredentialDead 判成 AUTH_INVALID——一个 AT 完全可用的账号被报
// 「需要重新登录」，整轮还会退出 1。
func TestRefreshIfNeededSkipsWithoutRefreshToken(t *testing.T) {
	t.Parallel()

	account := &auth.Auth{UID: "u1", AccessToken: "at-only", ExpiresAt: 0}

	// 前置条件：该账号确实处于「需要刷新」状态，否则用例没有覆盖力。
	if !account.NeedsRefresh(2 * time.Hour) {
		t.Fatal("前置条件不成立：无过期时间的账号应被视为需要刷新")
	}

	if err := refreshIfNeeded(account, nil); err != nil {
		t.Fatalf("无 refresh token 时应跳过刷新（返回 nil），err=%v", err)
	}
}
