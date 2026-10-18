package session

import (
	"fmt"
	"testing"
	"time"
)

// TestDegradeAvoidsRebindUntilWindow 降级窗口内不得把同一会话绑回原账号。
func TestDegradeAvoidsRebindUntilWindow(t *testing.T) {
	t.Parallel()

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
	t.Parallel()

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

// TestDegradeAvoidsRebindWhenHashFallsOnOriginal 回归：慢路径重分配前若丢弃降级条目，
// pickUID 就读不到 avoidUID，哈希恰好落在原账号时会把会话立即绑回去。
func TestDegradeAvoidsRebindWhenHashFallsOnOriginal(t *testing.T) {
	t.Parallel()

	r := routerWith(newCountingStore(), []string{"a1", "a2"}, time.Minute)
	key := ""

	for i := range 1000 {
		k := fmt.Sprintf("degrade-%d", i)
		if hashIndex(k, 2) == 0 {
			key = k
			break
		}
	}

	if key == "" {
		t.Fatal("no key hashes to a1")
	}

	r.Bind(key, "a1")
	r.Degrade(key, "a1", time.Minute)

	uid, ok := r.Resolve(key)
	if !ok {
		t.Fatal("resolve should fall back to another account")
	}

	if uid == "a1" {
		t.Fatalf("degraded account rebound despite avoid window: %s", uid)
	}
}

// TestDegradeWindowSurvivesRebindAndUnbind 降级窗口独立于当前绑定：重新分配
// 到其他账号后，即使该绑定失效或解绑，窗口内也不得绑回原账号。
func TestDegradeWindowSurvivesRebindAndUnbind(t *testing.T) {
	t.Parallel()

	avail := []string{"a1", "a2", "a3"}
	r := New(Config{TTL: time.Minute, Available: func() []string { return avail }})

	r.Bind("c1", "a1")
	r.Degrade("c1", "a1", time.Minute)

	if uid, ok := r.Resolve("c1"); !ok || uid == "a1" {
		t.Fatalf("降级后首次重分配 bind=%s ok=%v", uid, ok)
	}

	// a2 失效：重新分配必须避开 a1，落到 a3。
	avail = []string{"a1", "a3"}

	if uid, ok := r.Resolve("c1"); !ok || uid == "a1" {
		t.Fatalf("降级窗口内换绑失效后回到原账号: bind=%s ok=%v", uid, ok)
	}

	// 解绑后重新分配同样不得回到 a1。
	avail = []string{"a1", "a2", "a3"}

	r.Unbind("c1")

	if uid, ok := r.Resolve("c1"); !ok || uid == "a1" {
		t.Fatalf("降级窗口内解绑后回到原账号: bind=%s ok=%v", uid, ok)
	}
}

// TestDegradeKeepsPreviousWindow 换绑后的新账号再次降级，不得丢弃旧账号未过期的窗口。
// 键 "tw-window-0" 只避开 a2 时哈希恰好落在 a1，能暴露单槽覆盖。
func TestDegradeKeepsPreviousWindow(t *testing.T) {
	t.Parallel()

	const key = "tw-window-0"

	r := routerWith(newCountingStore(), []string{"a1", "a2", "a3"}, time.Minute)
	r.Bind(key, "a1")
	r.Degrade(key, "a1", time.Minute)
	r.Bind(key, "a2")
	r.Degrade(key, "a2", time.Minute)

	r.mu.RLock()
	list := r.avoid[key]
	r.mu.RUnlock()

	if len(list) != 2 {
		t.Fatalf("未保留两条降级窗口: %#v", list)
	}

	// a1/a2 都在窗口内：反复重分配只能落到 a3。
	for range 20 {
		r.Unbind(key)

		uid, ok := r.Resolve(key)
		if !ok || uid != "a3" {
			t.Fatalf("多窗口下重分配 bind=%s ok=%v want a3", uid, ok)
		}
	}
}

// TestTouchPreservesDegrade touch 只滚动 lastActive，不得覆盖并发 Degrade 写入的
// avoid 窗口（快路径读与 touch 之间可能已完成降级）。
func TestTouchPreservesDegrade(t *testing.T) {
	t.Parallel()

	r := routerWith(newCountingStore(), []string{"a1", "a2"}, time.Minute)
	r.Bind("c1", "a1")
	r.Degrade("c1", "a1", time.Minute)
	r.touch("c1", "a1", time.Now())

	r.mu.RLock()
	_, ok := r.entries["c1"]
	r.mu.RUnlock()

	if !ok {
		t.Fatal("降级中的条目应保留")
	}

	if list, ok := r.avoid["c1"]; !ok || len(list) != 1 || list[0].uid != "a1" || !list[0].until.After(time.Now()) {
		t.Fatalf("touch 不得清掉降级窗口: %#v", list)
	}
}

// TestTouchSkipsMirrorDuringDegrade 降级窗口内 touch 不得把 Degrade 删除的
// Redis 镜像写回（否则重启后会用旧绑定恢复会话）。
func TestTouchSkipsMirrorDuringDegrade(t *testing.T) {
	t.Parallel()

	countingStore2 := newCountingStore()
	r := routerWith(countingStore2, []string{"a1", "a2"}, time.Minute)
	r.Bind("c1", "a1")
	r.Degrade("c1", "a1", time.Minute)
	countingStore2.mu.Lock()
	before := countingStore2.setBinds
	countingStore2.mu.Unlock()

	r.touch("c1", "a1", time.Now())

	countingStore2.mu.Lock()
	after := countingStore2.setBinds
	_, mirrored := countingStore2.binds["c1"]
	countingStore2.mu.Unlock()

	if after != before {
		t.Fatalf("降级窗口内 touch 镜像了 %d 次 SetBind", after-before)
	}

	if mirrored {
		t.Fatal("降级窗口内 touch 把已删除的镜像写了回去")
	}
}

// TestTouchClearsExpiredAvoid 降级窗口结束并回纳后应清除 avoid*；
// 否则后续负载升高会让该会话再次被移走。
func TestTouchClearsExpiredAvoid(t *testing.T) {
	t.Parallel()

	r := routerWith(newCountingStore(), []string{"a1", "a2"}, time.Minute)
	r.Bind("c1", "a1")
	r.Degrade("c1", "a1", 10*time.Millisecond)
	time.Sleep(20 * time.Millisecond)

	r.cfg.LowLoad = func(string, string) bool { return true }

	if uid, ok := r.Resolve("c1"); !ok || uid != "a1" {
		t.Fatalf("bind=%s ok=%v want a1 after low-load re-inclusion", uid, ok)
	}

	r.mu.RLock()
	_, hasAvoid := r.avoid["c1"]
	r.mu.RUnlock()

	if hasAvoid {
		t.Fatal("回纳后 avoid 未清除")
	}

	r.cfg.LowLoad = func(string, string) bool { return false }
	if uid, ok := r.Resolve("c1"); !ok || uid != "a1" {
		t.Fatalf("回纳后再次受低水位门控: bind=%s ok=%v", uid, ok)
	}
}

// TestTouchDoesNotResurrectUnbound 快路径读后发生 Unbind，touch 不得把已删除的绑定写回。
func TestTouchDoesNotResurrectUnbound(t *testing.T) {
	t.Parallel()

	r := routerWith(newCountingStore(), []string{"a1"}, time.Minute)
	r.Bind("c1", "a1")
	r.Unbind("c1")
	r.touch("c1", "a1", time.Now())

	if r.Count() != 0 {
		t.Fatalf("touch 复活了已解绑的会话，count=%d", r.Count())
	}
}
