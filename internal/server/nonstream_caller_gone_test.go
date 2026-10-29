package server

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/reqlog"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// callerGoneUpstream 构造一个「上游刚开始回帧、调用方就断开」的上游 client：首次调用
// 即取消入站 ctx，body 的读操作随后返回 context.Canceled——非流式聚合期间客户端或
// CDN 回源超时的形态（issue #404）。
func callerGoneUpstream(calls *atomic.Int32, cancel context.CancelFunc) *upstream.Client {
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			cancel()

			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(iotest.ErrReader(context.Canceled)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

// TestNonStreamCallerGoneIsInterrupted 非流式「调用方断开」记 499/interrupted，不是
// 502 upstream_parse（issue #404）：不写响应体、不轮转、不罚号。
func TestNonStreamCallerGoneIsInterrupted(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})

	var buf bytes.Buffer

	handler := NewHandler(&Config{Pool: state, Upstream: callerGoneUpstream(&calls, cancel), SoftCooldown: time.Minute})
	handler.chatLogOut = &buf
	handler.chatLogEnabled = true

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)).WithContext(ctx)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// 连接已关：不写响应体，更不能出现 upstream_parse。
	if rec.Body.Len() != 0 {
		t.Fatalf("调用方已断开时不该写响应体，got %d bytes: %s", rec.Body.Len(), rec.Body)
	}

	// 请求日志：499，且不混进上游故障样本。
	row := buf.String()
	if !strings.Contains(row, "| 499 |") {
		t.Errorf("请求日志状态应为 499:\n%s", row)
	}

	if strings.Contains(row, "upstream_parse") {
		t.Errorf("请求日志不该出现 upstream_parse:\n%s", row)
	}

	if n := calls.Load(); n != 1 {
		t.Errorf("upstream calls=%d want 1 (调用方已走，不轮转)", n)
	}

	acctStatus := statusOf(t, state, "u1")
	if acctStatus.Cooling || acctStatus.Disabled || acctStatus.ErrTotal != 0 || acctStatus.BreakerFails != 0 {
		t.Errorf("调用方断开不是账号问题，不该罚号: %+v", acctStatus)
	}

	if acctStatus.SuccessCount != 1 {
		t.Errorf("success_count=%d want 1（与流式 interrupted 同口径：上游帧无恙）", acctStatus.SuccessCount)
	}
}

// TestRetryNonStreamAggregateErrorKeepsParseWhenCtxAlive 上游真断（ctx 未取消）：仍按
// 502 upstream_parse 记账并换号——#404 的改动只接管「调用方已断开」，不放松上游失败。
func TestRetryNonStreamAggregateErrorKeepsParseWhenCtxAlive(t *testing.T) {
	t.Parallel()

	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	flow := &chatFlow{h: NewHandler(&Config{Pool: state}), bareModel: "glm-5.2", realm: "cn", st: &chatStat{}}
	acct := &auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}

	if action := flow.retryNonStreamAggregateError(t.Context(), acct, upstream.ErrSSEAggregateTooLong,
		time.Now(), 0); action != rotContinue {
		t.Fatalf("action=%v want rotContinue（上游失败照常换号）", action)
	}

	if flow.st.status != http.StatusBadGateway {
		t.Errorf("status=%d want 502", flow.st.status)
	}

	if flow.lastErr == nil || !strings.Contains(flow.lastErr.Error(), "upstream_parse") {
		t.Errorf("lastErr=%v want upstream_parse 包装", flow.lastErr)
	}

	if flow.st.outcome == "interrupted" {
		t.Error("ctx 未取消时不该记 interrupted")
	}
}

// TestRetryNonStreamAggregateErrorCtxCanceled 入口 ctx 已取消：一律 499/interrupted，
// 不写 upstream_parse、不换号（issue #404 的第二道闸）。
func TestRetryNonStreamAggregateErrorCtxCanceled(t *testing.T) {
	t.Parallel()

	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	flow := &chatFlow{h: NewHandler(&Config{Pool: state}), bareModel: "glm-5.2", realm: "cn", st: &chatStat{}}
	acct := &auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if action := flow.retryNonStreamAggregateError(ctx, acct, context.Canceled, time.Now(), 0); action != rotDone {
		t.Fatalf("action=%v want rotDone（调用方已走，不再轮转）", action)
	}

	if flow.st.status != clientClosedRequestStatus {
		t.Errorf("status=%d want 499", flow.st.status)
	}

	if flow.st.outcome != reqlog.OutcomeInterrupted {
		t.Errorf("outcome=%q want interrupted", flow.st.outcome)
	}

	if flow.lastErr != nil {
		t.Errorf("lastErr=%v want nil（调用方断开不是错误分类）", flow.lastErr)
	}
}
