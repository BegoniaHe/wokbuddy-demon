package pool

import (
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

func TestRouteCooldownTripsAndRecovers(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	pool := New("")
	pool.SetClock(func() time.Time { return now })
	pool.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	pool.SetCredits("u1", 1000, 0)

	if a := pool.PickExcludingForRealm(nil, "m1", "cn"); a == nil {
		t.Fatal("precondition: pick should succeed")
	}

	for range routeFailureThreshold {
		pool.NoteRouteFailure("cn", "m1")
	}

	if a := pool.PickExcludingForRealm(nil, "m1", "cn"); a != nil {
		t.Fatalf("route should be cooled, got %s", a.UID)
	}

	if until := pool.RouteRecovery("cn", "m1"); until.IsZero() {
		t.Fatal("RouteRecovery should report cooldown")
	}
	// 其他域/模型不受影响。
	if a := pool.PickExcludingForRealm(nil, "m2", "cn"); a == nil {
		t.Fatal("other model should still be routable")
	}

	now = now.Add(routeCooldownDuration + time.Second)

	if a := pool.PickExcludingForRealm(nil, "m1", "cn"); a == nil {
		t.Fatal("route should recover after cooldown")
	}
}

func TestRouteFailureWindowSlides(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	pool := New("")
	pool.SetClock(func() time.Time { return now })
	pool.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	pool.SetCredits("u1", 1000, 0)

	pool.NoteRouteFailure("cn", "m1")
	pool.NoteRouteFailure("cn", "m1")

	now = now.Add(routeFailureWindow + time.Second)

	pool.NoteRouteFailure("cn", "m1") // 前两次已滑出窗口，不应熔断

	if a := pool.PickExcludingForRealm(nil, "m1", "cn"); a == nil {
		t.Fatal("stale failures must not trip route cooldown")
	}
}

func TestRouteSuccessClearsFailures(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	pool := New("")
	pool.SetClock(func() time.Time { return now })
	pool.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	pool.SetCredits("u1", 1000, 0)

	pool.NoteRouteFailure("cn", "m1")
	pool.NoteRouteFailure("cn", "m1")
	pool.NoteRouteSuccess("cn", "m1")
	pool.NoteRouteFailure("cn", "m1") // 成功已清零，仅 1 次

	if a := pool.PickExcludingForRealm(nil, "m1", "cn"); a == nil {
		t.Fatal("success should reset failure window")
	}
}
