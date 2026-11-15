// paid_status_test.go 钉住付费状态观测（issue #529）：三态透出、nil 不回落成免费、
// 观测时刻随写、持久化往返无损。
package pool

import (
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

func TestPaidStatusTriState(t *testing.T) {
	t.Parallel()

	now := time.Now()
	testPool := newTestPool(t, "")
	testPool.SetClock(func() time.Time { return now })
	testPool.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})

	status, ok := testPool.Status("u1")
	if !ok || status.PaidUser != nil || !status.PaidObserved.IsZero() {
		t.Fatalf("未观测应为未知且无观测时刻: %+v", status)
	}

	free := false
	testPool.SetPaidStatus("u1", &free)
	assertPaidStatus(t, testPool, &free, now)

	// 字段缺失（nil）回到未知：不保留旧值，也不回落成免费。
	testPool.SetPaidStatus("u1", nil)
	assertPaidStatus(t, testPool, nil, now)

	// 持久化往返：已观测值原样恢复。
	paid := true
	testPool.SetPaidStatus("u1", &paid)
	assertPaidStatusPersisted(t, testPool, now)
}

// assertPaidStatus 断言 Status 的付费三态与观测时刻。
func assertPaidStatus(t *testing.T, testPool *Pool, want *bool, wantObserved time.Time) {
	t.Helper()

	status, ok := testPool.Status("u1")
	if !ok {
		t.Fatal("u1 不在池中")
	}

	switch {
	case want == nil && status.PaidUser != nil:
		t.Fatalf("want 未知（nil）, got %v", *status.PaidUser)
	case want != nil && status.PaidUser == nil:
		t.Fatalf("PaidUser=nil want %v", *want)
	case want != nil && *status.PaidUser != *want:
		t.Fatalf("PaidUser=%v want %v", *status.PaidUser, *want)
	}

	if !status.PaidObserved.Equal(wantObserved) {
		t.Errorf("观测时刻=%v want %v", status.PaidObserved, wantObserved)
	}
}

// assertPaidStatusPersisted 断言已观测的付费状态经 stateAccount 往返无损。
func assertPaidStatusPersisted(t *testing.T, testPool *Pool, want time.Time) {
	t.Helper()

	testPool.mu.RLock()
	account := buildStateAccount(testPool.byUID["u1"], want)
	testPool.mu.RUnlock()

	restored := buildEntryFromState("u1", &account, want)
	if restored.paidUser == nil || !*restored.paidUser {
		t.Fatalf("往返后 paid=%v want true", restored.paidUser)
	}

	if !restored.paidObserved.Equal(want) {
		t.Errorf("往返观测时刻=%v want %v", restored.paidObserved, want)
	}
}
