package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

func checkinScheduler(t *testing.T, delay time.Duration) (*Scheduler, *fakeUpstream) {
	t.Helper()

	fakeUpstream2 := &fakeUpstream{resourceRemain: 500}
	srv := fakeUpstream2.server()
	t.Cleanup(srv.Close)

	state := pool.New("")
	state.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	state.Add(&auth.Auth{UID: "u2", AccessToken: "at2", RefreshToken: "rt2", ExpiresAt: 9999999999})

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}

	return New(&Config{Pool: state, Upstream: up, CheckinAccountDelay: delay}), fakeUpstream2
}

// TestRunCheckinAccountDelay 相邻账号签到至少间隔配置时长。
func TestRunCheckinAccountDelay(t *testing.T) {
	t.Parallel()

	const delay = 40 * time.Millisecond

	s, f := checkinScheduler(t, delay)
	s.RunCheckinNowCtx(context.Background())

	times := f.checkinTimes()
	if len(times) != 2 {
		t.Fatalf("checkin calls=%d want 2", len(times))
	}

	if gap := times[1].Sub(times[0]); gap < delay {
		t.Fatalf("账号间隔=%v want >= %v", gap, delay)
	}
}

// TestRunCheckinDelayCancel ctx 取消立即退出，不等满间隔。
func TestRunCheckinDelayCancel(t *testing.T) {
	t.Parallel()
	scheduler, fakeUpstream2 := checkinScheduler(t, 2*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		scheduler.RunCheckinNowCtx(ctx)
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for fakeUpstream2.checkinCalls.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	if fakeUpstream2.checkinCalls.Load() != 1 {
		t.Fatalf("首个账号未签到：calls=%d", fakeUpstream2.checkinCalls.Load())
	}

	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ctx 取消后未退出")
	}

	if got := fakeUpstream2.checkinCalls.Load(); got != 1 {
		t.Fatalf("取消后仍执行了签到：calls=%d", got)
	}
}
