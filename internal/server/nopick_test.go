package server

import (
	"strings"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// TestNoPickHint 池耗尽时 hint 带成因计数；没有可排除账号时保持既有常量 hint
// （issue #607）。
func TestNoPickHint(t *testing.T) {
	t.Parallel()

	testPool := pool.New("")
	handler := &Handler{cfg: Config{Pool: testPool}}

	// 空池：拆解为空 → 既有常量 hint，不多出噪声。
	if got := handler.noPickHint("chat", "m", "cn", nil, false); got != upstream.NoHealthyAccountHint() {
		t.Fatalf("empty pool hint = %q, want base hint", got)
	}

	testPool.Add(&auth.Auth{UID: "cool", AccessToken: "at", ExpiresAt: 9999999999})
	testPool.Cooldown("cool", pool.CoolHard, time.Hour, "test")

	got := handler.noPickHint("chat", "m", "cn", nil, false)

	want := upstream.NoHealthyAccountHint() + "; no_pick{total=1 cooling=1}"
	if got != want {
		t.Fatalf("hint = %q, want %q", got, want)
	}

	// 基线文案必须还在：成因只是附加，不替换客户端可见的原口径。
	if !strings.HasPrefix(got, upstream.NoHealthyAccountHint()) {
		t.Fatalf("base hint disappeared: %q", got)
	}
}

// TestPickFailureHintCrossRealmIgnoresRealm 跨域回退开启时域不是约束：另一域的冷却
// 账号不能被算成「realm 排除」，否则运维会照着提示去查域配置（issue #607）。
func TestPickFailureHintCrossRealmIgnoresRealm(t *testing.T) {
	t.Parallel()

	global := &auth.Auth{UID: "g1", AccessToken: "at", ExpiresAt: 9999999999}
	if _, err := auth.BackfillRealmFor(global, "global"); err != nil {
		t.Fatalf("backfill realm: %v", err)
	}

	testPool := pool.New("")
	testPool.Add(global)
	testPool.Cooldown("g1", pool.CoolHard, time.Hour, "test")

	handler := &Handler{cfg: Config{Pool: testPool}}

	// 跨域回退开启：域不参与排除，如实报「冷却」。
	flow := &chatFlow{h: handler, realm: "cn", bareModel: "m", crossRealm: true}
	if got, want := flow.pickFailureHint(),
		upstream.NoHealthyAccountHint()+"; no_pick{total=1 cooling=1}"; got != want {
		t.Fatalf("cross-realm hint = %q, want %q", got, want)
	}

	// 未开跨域回退：域就是约束，global 号确实是按 realm 排除的。
	flow = &chatFlow{h: handler, realm: "cn", bareModel: "m"}
	if got, want := flow.pickFailureHint(),
		upstream.NoHealthyAccountHint()+"; no_pick{total=1 realm=1}"; got != want {
		t.Fatalf("single-realm hint = %q, want %q", got, want)
	}
}
