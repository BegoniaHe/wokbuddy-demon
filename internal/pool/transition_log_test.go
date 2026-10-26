package pool

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// capturePoolLog 串行捕获 log 输出：调用期间替换标准 logger，结束后恢复。
// 本测试不加 t.Parallel()，避免与其他并行用例争用全局 logger。
func capturePoolLog(t *testing.T) *bytes.Buffer {
	t.Helper()

	var buf bytes.Buffer

	prevOutput := log.Writer()
	prevFlags := log.Flags()

	log.SetOutput(&buf)
	log.SetFlags(0)

	t.Cleanup(func() {
		log.SetOutput(prevOutput)
		log.SetFlags(prevFlags)
	})

	return &buf
}

// TestPoolTransitionWarnings 禁用 / 熔断 / 连败降权各产生一条 WARN，含账号
// 标签与原因、截止时刻、轮次或连续失败数；不打印 token。
//
//nolint:paralleltest // 串行替换全局 logger，不能与并行用例同时跑。
func TestPoolTransitionWarnings(t *testing.T) {
	t.Run("禁用", func(t *testing.T) {
		buf := capturePoolLog(t)

		pool := New("")
		pool.Add(&auth.Auth{UID: "u-disable-1", Nickname: "禁用号", AccessToken: "secret-token"})
		pool.Disable("u-disable-1", "account banned")

		out := buf.String()
		for _, want := range []string{"WARN: [pool] account disabled", "禁用号(u-disabl)", `reason="account banned"`} {
			if !strings.Contains(out, want) {
				t.Fatalf("log missing %q: %s", want, out)
			}
		}

		if strings.Contains(out, "secret-token") {
			t.Fatalf("log leaked token: %s", out)
		}
	})

	t.Run("熔断", func(t *testing.T) {
		buf := capturePoolLog(t)

		pool := New("")
		pool.SetBreaker(1, time.Hour, time.Hour)
		pool.Add(&auth.Auth{UID: "u-breaker-1", Nickname: "熔断号", AccessToken: "secret-token"})
		pool.NoteError("u-breaker-1")

		out := buf.String()
		for _, want := range []string{"WARN: [pool] breaker open", "熔断号(u-breake)", "until=", "retryCount=1"} {
			if !strings.Contains(out, want) {
				t.Fatalf("log missing %q: %s", want, out)
			}
		}

		if strings.Contains(out, "secret-token") {
			t.Fatalf("log leaked token: %s", out)
		}
	})

	t.Run("连败降权", func(t *testing.T) {
		buf := capturePoolLog(t)

		pool := New("")
		pool.SetDegrade(2, time.Hour, 2*time.Hour)
		pool.Add(&auth.Auth{UID: "u-degrade-1", Nickname: "降权号", AccessToken: "secret-token"})
		pool.NoteFailures("u-degrade-1")
		pool.NoteFailures("u-degrade-1")

		out := buf.String()
		for _, want := range []string{"WARN: [pool] account degraded", "降权号(u-degrad)", "until=", "consecutiveFails=2"} {
			if !strings.Contains(out, want) {
				t.Fatalf("log missing %q: %s", want, out)
			}
		}

		if strings.Contains(out, "secret-token") {
			t.Fatalf("log leaked token: %s", out)
		}
	})
}
