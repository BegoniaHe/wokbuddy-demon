package quota

import (
	"testing"
	"time"
)

func TestBudgetsAndDedup(t *testing.T) {
	t.Parallel()

	store := New("")

	reset := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	if !store.Add(&Event{UID: "u1", Realm: "cn", Model: "m", ResetAt: reset, Used: 100, At: time.Time{}}) {
		t.Fatal("first add should succeed")
	}

	if store.Add(&Event{UID: "u1", Realm: "cn", Model: "m", ResetAt: reset, Used: 999, At: time.Time{}}) {
		t.Fatal("duplicate should be ignored")
	}

	store.Add(&Event{UID: "u2", Realm: "cn", Model: "m", ResetAt: reset, Used: 300, At: time.Time{}})
	budgets := store.Budgets()

	b := budgets["cn|m"]
	if b.Samples != 2 || b.Used != 200 {
		t.Fatalf("budget=%+v want used 200 samples 2", b)
	}
}
