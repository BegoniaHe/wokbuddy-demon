package pool

import (
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// TestPickTrace 决策链记录候选、排除原因、Top 与最终 uid8。
func TestPickTrace(t *testing.T) {
	p := New("")
	for _, uid := range []string{"tried", "cooling", "healthy"} {
		p.Add(&auth.Auth{UID: uid, AccessToken: "at", ExpiresAt: 9999999999})
		p.SetCredits(uid, 1000, 0)
	}
	p.Cooldown("cooling", CoolHard, time.Hour, "test")
	acct, trace := p.PickExcludingForRealmTrace(map[string]bool{"tried": true}, "m", "cn")
	if acct == nil || acct.UID != "healthy" {
		t.Fatalf("acct=%v", acct)
	}
	if trace.Excluded["tried"] != 1 || trace.Excluded["cooling"] != 1 {
		t.Fatalf("excluded=%v", trace.Excluded)
	}
	if trace.Candidates != 1 || trace.FinalUID == "" || len(trace.Top) != 1 {
		t.Fatalf("trace=%+v", trace)
	}
	if trace.Top[0].Tier != 1 {
		t.Fatalf("top=%+v", trace.Top[0])
	}
}
