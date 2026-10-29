package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// TestChatSlotFullWaitsForReleasedSlot 本域在途名额占满、但很快就会释放时等一次名额
// 再重选（issue #403）：不再立刻 503。
func TestChatSlotFullWaitsForReleasedSlot(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	client := newFakeUpstream(t, func(string) (int, string, bool) {
		calls.Add(1)

		return http.StatusOK, sseOK, true
	})
	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	state.SetMaxInFlight(1)

	if !state.Acquire("u1") {
		t.Fatal("Acquire(u1) 失败，无法构造「名额占满」")
	}
	// 150ms 后归还名额：请求应当等到名额再选号成功（轮询间隔 slotWaitPoll）。
	go func() {
		time.Sleep(150 * time.Millisecond)
		state.Release("u1")
	}()

	handler := NewHandler(&Config{Pool: state, Upstream: client})

	rec := postChat(t, handler, `{"model":"glm-5.2","messages":[]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s (want 200 after waiting for a slot)", rec.Code, rec.Body)
	}

	if n := calls.Load(); n != 1 {
		t.Errorf("upstream calls=%d want 1 (waited the slot, no extra attempts)", n)
	}

	if st := statusOf(t, state, "u1"); st.Cooling || st.Disabled || st.ErrTotal != 0 {
		t.Errorf("等待名额不该罚号: %+v", st)
	}
}

// TestSlotWaitIsBounded 名额始终不释放时等待有界：预算耗尽即返回 false，请求不会被挂死。
// 直接用较小预算跑 waitForSlot，避免用例为了验证上限真等满 modelBusyWaitMax。
func TestSlotWaitIsBounded(t *testing.T) {
	t.Parallel()

	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	state.SetMaxInFlight(1)

	if !state.Acquire("u1") {
		t.Fatal("Acquire(u1) 失败，无法构造「名额占满」")
	}

	flow := &chatFlow{h: NewHandler(&Config{Pool: state}), bareModel: "glm-5.2", realm: "cn"}

	started := time.Now()

	if flow.waitForSlot(t.Context(), 150*time.Millisecond) {
		t.Fatal("名额始终未释放，waitForSlot 不该报告可以重选")
	}

	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("等待 %s 才返回，超出预算（预算 150ms）", elapsed)
	}

	if flow.busyWaited {
		t.Error("waitForSlot 不该自己记账 busyWaited（由调用方记账）")
	}
}

// TestSlotWaitRespectsCancel ctx 取消时立即返回 false（等待受入站 ctx 约束）。
func TestSlotWaitRespectsCancel(t *testing.T) {
	t.Parallel()

	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	state.SetMaxInFlight(1)

	if !state.Acquire("u1") {
		t.Fatal("Acquire(u1) 失败，无法构造「名额占满」")
	}

	flow := &chatFlow{h: NewHandler(&Config{Pool: state}), bareModel: "glm-5.2", realm: "cn"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	started := time.Now()

	if flow.waitForSlot(ctx, time.Second) {
		t.Fatal("canceled ctx must not report a slot")
	}

	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("canceled wait took %v", elapsed)
	}
}

// TestSlotWaitNotEnteredWhenSlotFree 名额本就可用时不进等待（pickAccount 已能选到号）。
func TestSlotWaitNotEnteredWhenSlotFree(t *testing.T) {
	t.Parallel()

	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	state.SetMaxInFlight(1)

	flow := &chatFlow{h: NewHandler(&Config{Pool: state}), bareModel: "glm-5.2", realm: "cn"}
	if flow.waitForSlot(t.Context(), time.Second) {
		t.Error("名额未占满，不该进入等待")
	}
}

// TestChatInFlightFullStill503WhenNoSlotFrees 名额永不释放：等不到就按既有路径收敛为
// 503，且不打上游、不出现 upstream_parse（等待期不占名额、也不产生额外调用）。
func TestChatInFlightFullStill503WhenNoSlotFrees(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	client := newFakeUpstream(t, func(string) (int, string, bool) {
		calls.Add(1)

		return http.StatusOK, sseOK, true
	})
	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	state.SetMaxInFlight(1)

	if !state.Acquire("u1") {
		t.Fatal("Acquire(u1) 失败，无法构造「名额占满」")
	}

	defer state.Release("u1")

	handler := NewHandler(&Config{Pool: state, Upstream: client})

	// 等待预算没有配置面（与 issue #254 同口径），用例用入站 ctx 超时提前结束等待。
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)).WithContext(ctx)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d body=%s (want 503 while the only slot stays taken)", rec.Code, rec.Body)
	}

	if strings.Contains(rec.Body.String(), "upstream_parse") {
		t.Errorf("名额满不是上游解析失败: %s", rec.Body)
	}

	if n := calls.Load(); n != 0 {
		t.Errorf("upstream calls=%d want 0 (no slot means no upstream call)", n)
	}
}
