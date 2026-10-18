// clock_test.go 钉住注入时钟（SetClock）使退避 / 冷却判定确定性化（issue #20）：
// 时序用例推进虚拟时间即可越过冷却窗口，不再依赖真实 sleep。
package pool

import (
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// TestClockInjectionSoftCooldownExpiry 软冷却到期由注入时钟驱动。
func TestClockInjectionSoftCooldownExpiry(t *testing.T) {
	t.Parallel()

	pool := New("")
	now := time.Now()

	pool.SetClock(func() time.Time { return now })
	pool.Add(&auth.Auth{UID: "u1"})
	pool.Cooldown("u1", CoolSoft, time.Minute, "429")

	if st, _ := pool.Status("u1"); !st.Cooling {
		t.Fatal("冷却期内应 Cooling")
	}

	now = now.Add(2 * time.Minute) // 推进虚拟时间越过冷却截止

	if st, _ := pool.Status("u1"); st.Cooling {
		t.Fatal("注入时钟越过后应恢复可选")
	}
}

// TestClockInjectionBreakerBackoff 熔断退避窗口由注入时钟驱动。
func TestClockInjectionBreakerBackoff(t *testing.T) {
	t.Parallel()

	pool := New("")
	now := time.Now()

	pool.SetClock(func() time.Time { return now })
	pool.SetBreaker(1, time.Minute, time.Hour) // 阈值 1：一次错误即熔断
	pool.Add(&auth.Auth{UID: "u1"})
	pool.NoteError("u1")

	if st, _ := pool.Status("u1"); !st.Cooling {
		t.Fatal("熔断后应 Cooling")
	}

	now = now.Add(2 * time.Minute)

	if st, _ := pool.Status("u1"); st.Cooling {
		t.Fatal("熔断退避到期后应恢复")
	}
}
