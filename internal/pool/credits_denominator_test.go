package pool

import (
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// TestEnterpriseSentinelDoesNotStarveNormals 企业 1<<40 哨兵不得充当权重分母，
// 也不得被二次放大到压死普通号。
func TestEnterpriseSentinelDoesNotStarveNormals(t *testing.T) {
	t.Parallel()

	poolState := New("")
	poolState.Add(&auth.Auth{UID: "ent"})
	poolState.Add(&auth.Auth{UID: "a"})
	poolState.Add(&auth.Auth{UID: "b"})
	poolState.SetCredits("ent", sentinelCredits, -1)
	poolState.SetCredits("a", 100_000, 100_000)
	poolState.SetCredits("b", 50_000, 50_000)

	cands := []*entry{poolState.byUID["ent"], poolState.byUID["a"], poolState.byUID["b"]}
	if got := maxCreditsOf(cands); got != 100_000 {
		t.Fatalf("denominator=%d want 100000", got)
	}

	now := time.Now()
	weightEnt := poolState.weightOf(poolState.byUID["ent"], 100_000, "", now)
	weightA := poolState.weightOf(poolState.byUID["a"], 100_000, "", now)
	weightB := poolState.weightOf(poolState.byUID["b"], 100_000, "", now)

	// 哨兵钳到满权重（与余额最多的正常号相同），普通号仍按余额分档。
	if weightEnt > weightA+0.001 {
		t.Fatalf("sentinel weight %.3f must not exceed full weight %.3f", weightEnt, weightA)
	}

	if weightA <= weightB {
		t.Fatalf("larger balance must weigh more: a=%v b=%v", weightA, weightB)
	}
}

// TestEnterpriseSentinelsExcludedFromDenominator 多个不限量号同时在场时，哨兵
// 仍不得进入分母（否则第二大的哨兵会继续压死普通号）。
func TestEnterpriseSentinelsExcludedFromDenominator(t *testing.T) {
	t.Parallel()

	poolState := New("")
	poolState.Add(&auth.Auth{UID: "ent1"})
	poolState.Add(&auth.Auth{UID: "ent2"})
	poolState.Add(&auth.Auth{UID: "a"})
	poolState.SetCredits("ent1", sentinelCredits, -1)
	poolState.SetCredits("ent2", sentinelCredits, -1)
	poolState.SetCredits("a", 70_000, 70_000)

	cands := []*entry{poolState.byUID["ent1"], poolState.byUID["ent2"], poolState.byUID["a"]}
	if got := maxCreditsOf(cands); got != 70_000 {
		t.Fatalf("denominator=%d want 70000", got)
	}

	// 全部候选都是哨兵：无正常分母，credits 项整体跳过。
	onlySentinels := []*entry{poolState.byUID["ent1"], poolState.byUID["ent2"]}
	if got := maxCreditsOf(onlySentinels); got != 0 {
		t.Fatalf("all-sentinel denominator=%d want 0", got)
	}
}

// TestPickWeightedIgnoresSentinelDenominator 加权抽签路径必须与排序路径共用
// maxCreditsOf：内联取最大余额会让哨兵当分母，把普通号的 credits 项压成 0。
func TestPickWeightedIgnoresSentinelDenominator(t *testing.T) {
	t.Parallel()

	poolState := New("")
	poolState.Add(&auth.Auth{UID: "ent"})
	poolState.Add(&auth.Auth{UID: "normal"})
	poolState.SetCredits("ent", sentinelCredits, -1)
	poolState.SetCredits("normal", 100_000, 100_000)

	ent := poolState.byUID["ent"]
	normal := poolState.byUID["normal"]

	// 抹平闲置补偿，只比较 credits 项。
	now := time.Now()
	ent.lastUsed, normal.lastUsed = now, now

	poolState.pickWeighted([]*entry{ent, normal}, "")

	if len(poolState.pickWeights) != 2 {
		t.Fatalf("weights=%v", poolState.pickWeights)
	}

	if poolState.pickWeights[1]*3 < poolState.pickWeights[0] {
		t.Fatalf("普通号被哨兵分母压权：ent=%d normal=%d",
			poolState.pickWeights[0], poolState.pickWeights[1])
	}
}
