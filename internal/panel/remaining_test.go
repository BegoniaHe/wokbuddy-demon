package panel

import (
	"slices"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/quota"
	"github.com/BegoniaHe/wokbuddy-demon/internal/usage"
)

// TestRemainingRowsStableOrdering 同一剩余量的多行必须稳定排序：行序来自 map 遍历，
// 若只按 Remaining 排序，每次请求的 ETag 会随机变化，304 缓存永远不命中。
func TestRemainingRowsStableOrdering(t *testing.T) {
	t.Parallel()

	now := time.Now()
	state := pool.New("")
	rec := usage.New("")

	for _, uid := range []string{"b", "a", "c"} {
		state.Add(&auth.Auth{UID: uid, AccessToken: "at", ExpiresAt: now.Add(time.Hour).Unix()})

		for _, model := range []string{"m2", "m1"} {
			rec.Add(now, "cn", uid, model, &usage.Delta{TotalTokens: 1, HasTotal: true}, true)
		}
	}

	panel := &Panel{cfg: Config{Pool: state, Usage: rec, Quota: quota.New("")}}

	want, _, _ := panel.remainingRows(now)
	if len(want) != 6 {
		t.Fatalf("rows=%d, want 6", len(want))
	}

	for i := range 50 {
		got, _, _ := panel.remainingRows(now)
		if !slices.Equal(got, want) {
			t.Fatalf("iteration %d: rows order changed\n got=%+v\nwant=%+v", i, got, want)
		}
	}
}
