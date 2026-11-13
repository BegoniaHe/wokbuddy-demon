// exitfail_test.go 钉住传输层失败的出口维度归因（issue #493）：拨号/TLS/EOF 不再
// 按当前被选号记连败，改为记到直连/代理池/代理槽；换号继续的行为不变。
package server

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/outbound"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// upstreamClientWithTransport 构造只注入 Transport 的上游客户端（CN base 指向假域，
// 请求不会真的出网：Transport 直接返回错误/响应）。
func upstreamClientWithTransport(
	t *testing.T, roundTrip roundTripFunc,
) *upstream.Client {
	t.Helper()

	return &upstream.Client{
		HTTP:          &http.Client{Transport: roundTrip},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

// sseResponse 构造一个 200 + SSE 的假响应。
func sseResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// TestChatTransportErrorNotChargedToAccounts 两个号都传输层失败：都不记连败，且
// 轮转确实走完了两个号（换号继续行为保留）。
func TestChatTransportErrorNotChargedToAccounts(t *testing.T) {
	t.Parallel()

	state := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)

	var calls atomic.Int64

	client := upstreamClientWithTransport(t, func(*http.Request) (*http.Response, error) {
		calls.Add(1)

		return nil, errConnectionRefused
	})
	handler := NewHandler(&Config{Pool: state, Upstream: client})

	rec := postChat(t, handler, `{"model":"glm-5.2","messages":[]}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}

	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls=%d want 2（换号继续行为保留）", got)
	}

	for _, uid := range []string{"u1", "u2"} {
		st, _ := state.Status(uid)
		if st.ConsecutiveFails != 0 || st.BreakerFails != 0 || st.Cooling {
			t.Errorf("%s 不应被传输层失败罚号: %+v", uid, st)
		}
	}

	handler.exitFail.mu.Lock()
	hits := len(handler.exitFail.hits["direct"])
	warned := !handler.exitFail.warned["direct"].IsZero()
	handler.exitFail.mu.Unlock()

	if hits != 2 {
		t.Errorf("出口台账 direct hits=%d want 2（多个号归因到同一出口）", hits)
	}

	if !warned {
		t.Error("多号同时失败应触发一条出口级 WARN")
	}
}

// TestChatTransportErrorStillRotatesToNextAccount 首个号传输层失败后仍换号并成功。
func TestChatTransportErrorStillRotatesToNextAccount(t *testing.T) {
	t.Parallel()

	state := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)

	client := upstreamClientWithTransport(t, func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.Header.Get("Authorization"), "at1") {
			return nil, errConnectionRefused
		}

		return sseResponse(sseOK), nil
	})
	handler := NewHandler(&Config{Pool: state, Upstream: client})

	rec := postChat(t, handler, `{"model":"glm-5.2","messages":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}

	st, _ := state.Status("u2")
	if st.ConsecutiveFails != 0 {
		t.Errorf("成功号不应有连败, got %d", st.ConsecutiveFails)
	}
}

// TestExitLabel 出口归因标签：代理槽 > 全局池 > 直连。
func TestExitLabel(t *testing.T) {
	t.Parallel()

	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	account := state.AuthByUID("u1")

	t.Run("direct", func(t *testing.T) {
		t.Parallel()

		handler := NewHandler(&Config{Pool: state, Upstream: newFakeUpstream(t, nil)})
		if got := handler.exitLabel(account); got != "direct" {
			t.Errorf("label=%q want direct", got)
		}
	})

	t.Run("slot", func(t *testing.T) {
		t.Parallel()

		handler := NewHandler(&Config{Pool: state, Upstream: newFakeUpstream(t, nil)})
		handler.cfg.Upstream.SetAccountRoute(func(*auth.Auth) (string, func(context.Context, string, string) (net.Conn, error)) {
			return "office", nil
		})

		if got := handler.exitLabel(account); got != "slot:office" {
			t.Errorf("label=%q want slot:office", got)
		}
	})

	t.Run("global", func(t *testing.T) {
		t.Parallel()

		handler := NewHandler(&Config{Pool: state, Upstream: newFakeUpstream(t, nil)})

		reg, err := outbound.NewRegistry([]string{"http://127.0.0.1:1"}, nil, nil)
		if err != nil {
			t.Fatalf("registry: %v", err)
		}

		handler.cfg.Upstream.SetProxyRegistry(reg)

		if got := handler.exitLabel(account); got != "global" {
			t.Errorf("label=%q want global", got)
		}
	})
}
