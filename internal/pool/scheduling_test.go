package pool

import (
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

func TestPressureFactorDampsHighLoad(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	p := New("")
	p.SetClock(func() time.Time { return now })
	p.SetMaxInFlight(3)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetCredits("u1", 1000, 0)
	e := p.byUID["u1"]

	base := p.weightOf(e, 1000, "m", now)
	if !p.Acquire("u1") {
		t.Fatal("acquire 1")
	}
	l1 := p.weightOf(e, 1000, "m", now)
	if !p.Acquire("u1") {
		t.Fatal("acquire 2")
	}
	l2 := p.weightOf(e, 1000, "m", now)
	p.Release("u1")
	p.Release("u1")

	if l1 != base {
		t.Fatalf("L=1 should not be damped: base=%v L1=%v", base, l1)
	}
	if l2 >= l1 {
		t.Fatalf("L=2 weight=%v should be lower than L=1 %v", l2, l1)
	}
}

func TestPressureFactorUnlimited(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	p := New("")
	p.SetClock(func() time.Time { return now })
	p.SetMaxInFlight(0)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetCredits("u1", 1000, 0)
	e := p.byUID["u1"]
	base := p.weightOf(e, 1000, "m", now)
	for range 5 {
		p.Acquire("u1")
	}
	if got := p.weightOf(e, 1000, "m", now); got != base {
		t.Fatalf("limit=0 must not damp: base=%v got=%v", base, got)
	}
}

func TestStickySoftEscapeAndLowLoad(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	p := New("")
	p.SetClock(func() time.Time { return now })
	p.SetMaxInFlight(3)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetCredits("u1", 1000, 0)

	if _, kind := p.PickByUIDForModel("u1", "m", ""); kind != StickyPicked {
		t.Fatalf("L=0 kind=%v want picked", kind)
	}
	p.Acquire("u1")
	if _, kind := p.PickByUIDForModel("u1", "m", ""); kind != StickyPicked {
		t.Fatalf("L=1 kind=%v want picked", kind)
	}
	p.Acquire("u1")
	if _, kind := p.PickByUIDForModel("u1", "m", ""); kind != StickyEscaped {
		t.Fatalf("L=2 kind=%v want escaped", kind)
	}
	p.Release("u1")
	p.Release("u1")

	if !p.StickyLowLoad("u1", "m", "cn") {
		t.Fatal("idle account should be low-load")
	}
	p.Acquire("u1")
	if p.StickyLowLoad("u1", "m", "cn") {
		t.Fatal("L=1 of limit=3 should exceed low-load floor")
	}
	p.Release("u1")
}

func TestFallbackProbeSingleFlight(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	p := New("")
	p.SetClock(func() time.Time { return now })
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetCredits("u1", 1000, 0)
	p.Cooldown("u1", CoolSoft, time.Minute, "429")

	if a := p.Pick(); a == nil {
		t.Fatal("first fallback pick should succeed")
	}
	if a := p.Pick(); a != nil {
		t.Fatalf("second concurrent fallback pick should be rejected, got %s", a.UID)
	}
	p.NoteSuccess("u1")
	if a := p.Pick(); a == nil {
		t.Fatal("probe window should clear on success")
	}
}

func TestJitterCooldownBounded(t *testing.T) {
	d := time.Hour
	for range 200 {
		got := jitterCooldown(d)
		if got < 54*time.Minute || got > 66*time.Minute {
			t.Fatalf("jitter=%v outside ±10%%", got)
		}
	}
}

func TestReserveForRequest(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetCreditFloor(150)
	p.NoteModelCost("u1", "paid", 22.3, 100000) // 0.223 credits/1k
	p.SetCredits("u1", 200, 0)                  // NoteModelCost 会实扣一次，重置到测试余额

	// prompt 100k + default output 2048 ≈ 22.75 credits；两次后余额不足以覆盖保底。
	if _, ok := p.ReserveForRequest("u1", "paid", "cn", 100000, 0); !ok {
		t.Fatal("first reservation should fit")
	}
	if _, ok := p.ReserveForRequest("u1", "paid", "cn", 100000, 0); !ok {
		t.Fatal("second reservation should fit")
	}
	if _, ok := p.ReserveForRequest("u1", "paid", "cn", 100000, 0); ok {
		t.Fatal("third reservation should be blocked by credit floor")
	}
	p.ReleaseReservation("u1", 22.75)
	if _, ok := p.ReserveForRequest("u1", "paid", "cn", 1000, 100); !ok {
		t.Fatal("small reservation should fit after release")
	}
}

func TestReserveForRequestFreeModelNoop(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetCredits("u1", 10, 0)
	p.SetCreditFloor(150)
	p.NoteModelCost("u1", "free", 0, 1000)
	amount, ok := p.ReserveForRequest("u1", "free", "cn", 100000, 2048)
	if !ok || amount != 0 {
		t.Fatalf("free model amount=%v ok=%v want 0,true", amount, ok)
	}
}
