package panel

import (
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

func TestGrantSourceAndStatus(t *testing.T) {
	source := inferGrantSource(upstream.CreditPackage{Name: "签到奖励"})
	if source != "签到/连登" {
		t.Fatalf("source=%q", source)
	}
	if got := inferGrantSource(upstream.CreditPackage{Name: "国内运营裂变包"}); got != "活动奖励" {
		t.Errorf("source=%q want 活动奖励", got)
	}
	if got := inferGrantSource(upstream.CreditPackage{Size: 1500}); got != "首次授权（按面额推断）" {
		t.Errorf("source=%q", got)
	}
	now := time.Now()
	if got := grantStatus(upstream.CreditPackage{Remain: 0, Size: 10, ExpiresAt: now.Add(time.Hour).UnixMilli()}, now); got != "已用完" {
		t.Errorf("status=%q", got)
	}
	if got := grantStatus(upstream.CreditPackage{Remain: 1, Size: 10, ExpiresAt: now.Add(-time.Hour).UnixMilli()}, now); got != "已过期" {
		t.Errorf("status=%q", got)
	}
	if got := grantStatus(upstream.CreditPackage{Remain: 1, Size: 10}, now); got != "可用（不过期）" {
		t.Errorf("status=%q", got)
	}
}
