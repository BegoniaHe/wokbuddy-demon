package pool

import (
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

func TestPressureFactorDampsHighLoad(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	pool := New("")
	pool.SetClock(func() time.Time { return now })
	pool.SetMaxInFlight(3)
	pool.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	pool.SetCredits("u1", 1000, 0)
	entry2 := pool.byUID["u1"]

	base := pool.weightOf(entry2, 1000, "m", now)
	if !pool.Acquire("u1") {
		t.Fatal("acquire 1")
	}

	num := pool.weightOf(entry2, 1000, "m", now)
	if !pool.Acquire("u1") {
		t.Fatal("acquire 2")
	}

	num2 := pool.weightOf(entry2, 1000, "m", now)
	pool.Release("u1")
	pool.Release("u1")

	if num != base {
		t.Fatalf("L=1 should not be damped: base=%v L1=%v", base, num)
	}

	if num2 >= num {
		t.Fatalf("L=2 weight=%v should be lower than L=1 %v", num2, num)
	}
}

func TestPressureFactorUnlimited(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	pool := New("")
	pool.SetClock(func() time.Time { return now })
	pool.SetMaxInFlight(0)
	pool.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	pool.SetCredits("u1", 1000, 0)
	entry2 := pool.byUID["u1"]

	base := pool.weightOf(entry2, 1000, "m", now)
	for range 5 {
		pool.Acquire("u1")
	}

	if got := pool.weightOf(entry2, 1000, "m", now); got != base {
		t.Fatalf("limit=0 must not damp: base=%v got=%v", base, got)
	}
}

func TestStickySoftEscapeAndLowLoad(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	pool := New("")
	pool.SetClock(func() time.Time { return now })
	pool.SetMaxInFlight(3)
	pool.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	pool.SetCredits("u1", 1000, 0)

	if _, kind := pool.PickByUIDForModel("u1", "m", ""); kind != StickyPicked {
		t.Fatalf("L=0 kind=%v want picked", kind)
	}

	pool.Acquire("u1")

	if _, kind := pool.PickByUIDForModel("u1", "m", ""); kind != StickyPicked {
		t.Fatalf("L=1 kind=%v want picked", kind)
	}

	pool.Acquire("u1")

	if _, kind := pool.PickByUIDForModel("u1", "m", ""); kind != StickyEscaped {
		t.Fatalf("L=2 kind=%v want escaped", kind)
	}

	pool.Release("u1")
	pool.Release("u1")

	if !pool.StickyLowLoad("u1", "m", "cn") {
		t.Fatal("idle account should be low-load")
	}

	pool.Acquire("u1")

	if pool.StickyLowLoad("u1", "m", "cn") {
		t.Fatal("L=1 of limit=3 should exceed low-load floor")
	}

	pool.Release("u1")
}

func TestFallbackProbeSingleFlight(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	pool := New("")
	pool.SetClock(func() time.Time { return now })
	pool.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	pool.SetCredits("u1", 1000, 0)
	pool.Cooldown("u1", CoolSoft, time.Minute, "429")

	if a := pool.Pick(); a == nil {
		t.Fatal("first fallback pick should succeed")
	}

	if a := pool.Pick(); a != nil {
		t.Fatalf("second concurrent fallback pick should be rejected, got %s", a.UID)
	}

	pool.NoteSuccess("u1")

	if a := pool.Pick(); a == nil {
		t.Fatal("probe window should clear on success")
	}
}

func TestJitterCooldownBounded(t *testing.T) {
	t.Parallel()

	d := time.Hour
	for range 200 {
		got := jitterCooldown(d)
		if got < 54*time.Minute || got > 66*time.Minute {
			t.Fatalf("jitter=%v outside ±10%%", got)
		}
	}
}

func TestReserveForRequest(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	pool.SetCreditFloor(150)
	pool.NoteModelCost("u1", "paid", 22.3, 100000) // 0.223 credits/1k
	pool.SetCredits("u1", 200, 0)                  // NoteModelCost 会实扣一次，重置到测试余额

	// prompt 100k + default output 2048 ≈ 22.75 credits；两次后余额不足以覆盖保底。
	if _, ok := pool.ReserveForRequest("u1", "paid", "cn", 100000, 0); !ok {
		t.Fatal("first reservation should fit")
	}

	if _, ok := pool.ReserveForRequest("u1", "paid", "cn", 100000, 0); !ok {
		t.Fatal("second reservation should fit")
	}

	if _, ok := pool.ReserveForRequest("u1", "paid", "cn", 100000, 0); ok {
		t.Fatal("third reservation should be blocked by credit floor")
	}

	pool.ReleaseReservation("u1", 22.75)

	if _, ok := pool.ReserveForRequest("u1", "paid", "cn", 1000, 100); !ok {
		t.Fatal("small reservation should fit after release")
	}
}

func TestReserveForRequestFreeModelNoop(t *testing.T) {
	t.Parallel()

	pool := New("")
	pool.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	pool.SetCredits("u1", 10, 0)
	pool.SetCreditFloor(150)
	pool.NoteModelCost("u1", "free", 0, 1000)

	amount, ok := pool.ReserveForRequest("u1", "free", "cn", 100000, 2048)
	if !ok || amount != 0 {
		t.Fatalf("free model amount=%v ok=%v want 0,true", amount, ok)
	}
}
