// handler_caller_gone_test.go 钉住「调用方主动断开不罚号、不记上游错误」的归因
// （issue #404 口径扩展到传输层路径）。这些用例只构造 chatFlow 的局部状态，不走
// 完整 HTTP 主路径。
package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/reqlog"
	"github.com/BegoniaHe/wokbuddy-demon/internal/session"
	"github.com/BegoniaHe/wokbuddy-demon/internal/usage"
)

// errTestConnectionReset 传输层读错误样本（静态错误，err113 禁止就地 errors.New）。
var errTestConnectionReset = errors.New("read: connection reset by peer")

// TestTransportErrorCallerGoneNotPenalized 客户端在等待上游时主动断开：必须按
// interrupted + 499 收尾，记账号成功、保留粘性绑定、且不进出口失败台账。
func TestTransportErrorCallerGoneNotPenalized(t *testing.T) {
	t.Parallel()

	sess := session.New(session.Config{
		TTL:       time.Minute,
		Available: func() []string { return []string{"u1"} },
	})
	sess.Bind("sk", "u1")

	handler := NewHandler(&Config{Pool: testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999},
	), Session: sess})

	acct := &auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}
	flow := &chatFlow{
		h: handler, st: &chatStat{uid: "u1"}, realm: "cn", bareModel: "m",
		stickyKey: "sk", stickyUID: "u1",
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	action := flow.handleTransportError(ctx, acct, errTestConnectionReset, time.Now(), 0)
	if action != rotDone {
		t.Fatalf("action=%v want rotDone（rotBreak 会让调用方再写最终错误，覆盖 499 归因）", action)
	}

	if flow.st.status != clientClosedRequestStatus || flow.st.outcome != reqlog.OutcomeInterrupted {
		t.Fatalf("status=%d outcome=%q want 499 %q", flow.st.status, flow.st.outcome, reqlog.OutcomeInterrupted)
	}

	if !flow.callerGone {
		t.Error("callerGone 未置位：后续用量归因会把中断记成上游错误")
	}

	if uid, ok := sess.Resolve("sk"); !ok || uid != "u1" {
		t.Error("粘性绑定被客户端断连解绑（f.fail 不应在该路径调用）")
	}

	handler.exitFail.mu.Lock()
	hits := len(handler.exitFail.hits)
	handler.exitFail.mu.Unlock()

	if hits != 0 {
		t.Errorf("exitFail 台账=%d 条 want 0（客户端断连不是出口故障）", hits)
	}
}

// TestRecordAttemptCallerGoneCountsOK 中断尝试按成功口径记用量：即使没有 usage
// 也不计入 Errors，否则面板错误率与 wokbuddy_requests_total{result="error"} 会被
// 客户端「点停止」刷高。
func TestRecordAttemptCallerGoneCountsOK(t *testing.T) {
	t.Parallel()

	handler := NewHandler(&Config{Pool: testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999},
	)})
	handler.cfg.Usage = usage.New("")

	newFlow := func() *chatFlow {
		return &chatFlow{h: handler, st: &chatStat{uid: "u1"}, realm: "cn", bareModel: "m"}
	}

	interrupted := newFlow()
	interrupted.callerGone = true
	interrupted.recordAttempt("u1", &pool.TokenUsageDelta{}, 0, false, time.Now(), 0)

	failed := newFlow()
	failed.recordAttempt("u1", &pool.TokenUsageDelta{}, 0, false, time.Now(), 0)

	rows := handler.cfg.Usage.AggregateByModel()
	if len(rows) != 1 {
		t.Fatalf("usage rows=%d want 1", len(rows))
	}

	if rows[0].Requests != 2 {
		t.Errorf("requests=%d want 2（失败也计入请求数）", rows[0].Requests)
	}

	if rows[0].Errors != 1 {
		t.Errorf("errors=%d want 1（只有真失败那次计错；中断按成功口径）", rows[0].Errors)
	}
}
