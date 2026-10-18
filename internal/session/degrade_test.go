package session

import (
	"fmt"
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

// TestDegradeAvoidsRebindWhenHashFallsOnOriginal 回归：慢路径重分配前若丢弃降级条目，
// pickUID 就读不到 avoidUID，哈希恰好落在原账号时会把会话立即绑回去。
func TestDegradeAvoidsRebindWhenHashFallsOnOriginal(t *testing.T) {
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

// TestTouchPreservesDegrade touch 只滚动 lastActive，不得覆盖并发 Degrade 写入的
// avoid 窗口（快路径读与 touch 之间可能已完成降级）。
func TestTouchPreservesDegrade(t *testing.T) {
	r := routerWith(newCountingStore(), []string{"a1", "a2"}, time.Minute)
	r.Bind("c1", "a1")
	r.Degrade("c1", "a1", time.Minute)
	r.touch("c1", "a1", time.Now())

	r.mu.RLock()
	e, ok := r.entries["c1"]
	r.mu.RUnlock()
	if !ok {
		t.Fatal("降级中的条目应保留")
	}
	if e.avoidUID != "a1" || !e.avoidUntil.After(time.Now()) {
		t.Fatalf("touch 不得清掉降级窗口: avoidUID=%q avoidUntil=%v", e.avoidUID, e.avoidUntil)
	}
}

// TestTouchSkipsMirrorDuringDegrade 降级窗口内 touch 不得把 Degrade 删除的
// Redis 镜像写回（否则重启后会用旧绑定恢复会话）。
func TestTouchSkipsMirrorDuringDegrade(t *testing.T) {
	st := newCountingStore()
	r := routerWith(st, []string{"a1", "a2"}, time.Minute)
	r.Bind("c1", "a1")
	r.Degrade("c1", "a1", time.Minute)
	st.mu.Lock()
	before := st.setBinds
	st.mu.Unlock()

	r.touch("c1", "a1", time.Now())

	st.mu.Lock()
	after := st.setBinds
	_, mirrored := st.binds["c1"]
	st.mu.Unlock()
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
	r := routerWith(newCountingStore(), []string{"a1", "a2"}, time.Minute)
	r.Bind("c1", "a1")
	r.Degrade("c1", "a1", 10*time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	r.cfg.LowLoad = func(string, string) bool { return true }

	if uid, ok := r.Resolve("c1"); !ok || uid != "a1" {
		t.Fatalf("bind=%s ok=%v want a1 after low-load re-inclusion", uid, ok)
	}
	r.mu.RLock()
	e := r.entries["c1"]
	r.mu.RUnlock()
	if e.avoidUID != "" {
		t.Fatalf("回纳后 avoid 未清除: avoidUID=%q", e.avoidUID)
	}

	r.cfg.LowLoad = func(string, string) bool { return false }
	if uid, ok := r.Resolve("c1"); !ok || uid != "a1" {
		t.Fatalf("回纳后再次受低水位门控: bind=%s ok=%v", uid, ok)
	}
}

// TestTouchDoesNotResurrectUnbound 快路径读后发生 Unbind，touch 不得把已删除的绑定写回。
func TestTouchDoesNotResurrectUnbound(t *testing.T) {
	r := routerWith(newCountingStore(), []string{"a1"}, time.Minute)
	r.Bind("c1", "a1")
	r.Unbind("c1")
	r.touch("c1", "a1", time.Now())

	if r.Count() != 0 {
		t.Fatalf("touch 复活了已解绑的会话，count=%d", r.Count())
	}
}
