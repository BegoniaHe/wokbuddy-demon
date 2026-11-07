package scheduler

import (
	"context"
	"net/http"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// TestClaimCompensationNowHonorsCanceledContext 已取消的 ctx 不应再派发任何领取：
// 修复前 ClaimCompensationNow 不接收 ctx 且裸 sem <- struct{}{}，客户端断连后
// 仍会对剩余账号打满上游（与已修的 CompensationStatusAll 口径不一致）。
func TestClaimCompensationNowHonorsCanceledContext(t *testing.T) {
	t.Parallel()

	fixture := &compensationFixture{
		httpStatus: http.StatusOK,
		statusBody: `{"code":0,"data":{"active":true,"credit_num":100}}`,
		claimBody:  `{"code":0,"data":{"credit_num":100}}`,
	}
	sched := fixture.scheduler(t,
		&auth.Auth{UID: "a", AccessToken: "at"},
		&auth.Auth{UID: "b", AccessToken: "at"},
		&auth.Auth{UID: "c", AccessToken: "at"},
	)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	rows, summary := sched.ClaimCompensationNow(ctx)

	if len(rows) != 3 {
		t.Fatalf("rows=%d want 3（取消不缩减行数，只是不派发）", len(rows))
	}

	if got := fixture.statusCalls.Load(); got != 0 {
		t.Errorf("ctx 已取消仍发起 %d 次状态查询，应为 0", got)
	}

	if got := fixture.claimCalls.Load(); got != 0 {
		t.Errorf("ctx 已取消仍发起 %d 次领取，应为 0", got)
	}

	if summary.Claimed != 0 {
		t.Errorf("ctx 已取消不应有领取成功，claimed=%d", summary.Claimed)
	}
}
