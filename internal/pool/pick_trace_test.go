package pool

import (
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// TestPickTrace 决策链记录候选、排除原因、Top 与最终 uid8。
func TestPickTrace(t *testing.T) {
	t.Parallel()

	pool := New("")
	for _, uid := range []string{"tried", "cooling", "healthy"} {
		pool.Add(&auth.Auth{UID: uid, AccessToken: "at", ExpiresAt: 9999999999})
		pool.SetCredits(uid, 1000, 0)
	}

	pool.Cooldown("cooling", CoolHard, time.Hour, "test")

	acct, trace := pool.PickExcludingForRealmTrace(map[string]bool{"tried": true}, "m", "cn")
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

// 等权重候选超过 top5 时必须真正打散，不能被 uid 排序重新钉死。
func TestRankCandidatesTieNotStarvedByUID(t *testing.T) {
	t.Parallel()

	pool := New("")
	for _, uid := range []string{"u0", "u1", "u2", "u3", "u4", "u5"} {
		pool.Add(&auth.Auth{UID: uid})
	}

	cands := pool.pickCandidates(nil, "", "", pool.now(), nil)
	if len(cands) != 6 {
		t.Fatalf("candidates=%d want 6", len(cands))
	}

	base := time.Now()
	for i := range 200 {
		_, top := pool.rankCandidates(cands, "", 1, 0, base.Add(time.Duration(i)*time.Nanosecond))
		for _, e := range top {
			if e.a.UID == "u5" {
				return
			}
		}
	}

	t.Fatal("权重并列时 u5 从未进入 top5：uid tie-break 仍在生效")
}
