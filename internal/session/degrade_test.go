package session

import (
	"testing"
	"time"
)

// TestDegradeAvoidsRebindUntilWindow 降级窗口内不得把同一会话绑回原账号。
func TestDegradeAvoidsRebindUntilWindow(t *testing.T) {
	r := routerWith(newCountingStore(), []string{"a1", "a2"}, time.Minute)
	r.Bind("c1", "a1")
	r.Degrade("c1", "a1", 30*time.Millisecond)

	uid, ok := r.Resolve("c1")
	if !ok {
		t.Fatal("resolve should fall back to another account")
	}
	if uid == "a1" {
		t.Fatalf("degraded account rebound during window: %s", uid)
	}

	time.Sleep(40 * time.Millisecond)
	// 窗口结束但 LowLoad 仍为 false：继续避开 a1。
	r.cfg.LowLoad = func(string, string) bool { return false }
	if uid, _ := r.Resolve("c1"); uid == "a1" {
		t.Fatalf("avoided account rebound before low load: %s", uid)
	}
}

// TestDegradeReincludesWhenLowLoad 窗口结束且低水位后允许回纳单账号池。
func TestDegradeReincludesWhenLowLoad(t *testing.T) {
	r := routerWith(newCountingStore(), []string{"a1"}, time.Minute)
	r.Bind("c1", "a1")
	r.Degrade("c1", "a1", 10*time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	r.cfg.LowLoad = func(string, string) bool { return true }

	uid, ok := r.Resolve("c1")
	if !ok || uid != "a1" {
		t.Fatalf("bind=%s ok=%v want a1 after low-load re-inclusion", uid, ok)
	}
}
