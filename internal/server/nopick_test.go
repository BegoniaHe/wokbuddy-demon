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
