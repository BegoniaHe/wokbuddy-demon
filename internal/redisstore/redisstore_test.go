package redisstore

import (
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNormalizeURL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		url   string
		token string
		want  string
	}{
		{"完整rediss", "rediss://default:tok@host:6379", "ignored", "rediss://default:tok@host:6379"},
		{"完整redis", "redis://default:tok@host:6379", "ignored", "redis://default:tok@host:6379"},
		{"https host", "https://foo.upstash.io", "tok", "rediss://default:tok@foo.upstash.io:6379"},
		{"裸host", "foo.upstash.io", "tok", "rediss://default:tok@foo.upstash.io:6379"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			if got := normalizeURL(c.url, c.token); got != c.want {
				t.Errorf("normalizeURL(%q,%q)=%q want %q", c.url, c.token, got, c.want)
			}
		})
	}
}

func TestNormalizeURLStripsTrailingPath(t *testing.T) {
	t.Parallel()
	// 用户照抄 Upstash 控制台的 REST 地址，可能带任意路径——剥 scheme 只取 host:port 之前段。
	got := normalizeURL("https://foo.upstash.io", "t")
	if strings.Contains(got, "://foo.upstash.io") && !strings.HasSuffix(got, "foo.upstash.io:6379") {
		t.Errorf("unexpected: %s", got)
	}
}

func TestNewEmptyURLReturnsNoop(t *testing.T) {
	t.Parallel()

	if _, ok := New("", "").(Noop); !ok {
		t.Fatalf("empty url should return Noop")
	}
}

func TestNewBadSchemeReturnsNoop(t *testing.T) {
	t.Parallel()
	// 组装出的连接串含空格 → ParseURL 解析失败 → 降级 Noop，不 panic、不发网络请求。
	if _, ok := New("://bad host", "").(Noop); !ok {
		t.Fatalf("bad url should return Noop")
	}
}

func TestNoopMethods(t *testing.T) {
	t.Parallel()

	n := Noop{}
	n.SetBind("k", "u", time.Minute) // 不 panic
	n.DelBind("k")
	n.SaveState([]byte("{}"))

	if _, ok := n.LoadState(); ok {
		t.Error("Noop.LoadState should report not-found")
	}
}

func TestBindKeyPrefix(t *testing.T) {
	t.Parallel()

	if got := bindKey("abc"); got != bindPrefix+"abc" {
		t.Errorf("bindKey=%q want prefix", got)
	}
}

// TestZeroValueConcurrentInit（R3）：零值 Upstash 并发 goWrite/Close 时
// 队列/写者池只能初始化一次，不能出现字段写竞争。
func TestZeroValueConcurrentInit(t *testing.T) {
	t.Parallel()

	var store Upstash

	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			store.goWrite("k"+strconv.Itoa(i), func() {})
		})
	}

	wg.Go(func() {
		_ = store.Close()
	})

	wg.Wait()
}

// TestCloseDrainsQueuedWrites 锁定 Close 的停机契约：Close 前已提交但尚未执行的
// 写也必须在 Close 返回前执行，不能因关停被丢弃。
func TestCloseDrainsQueuedWrites(t *testing.T) {
	t.Parallel()

	var store Upstash

	release := make(chan struct{})
	started := make(chan struct{}, writeWorkers)

	var ran atomic.Int64

	// 占满全部写者，后续写只能排队，模拟 Close 前已提交但未执行的写。
	for i := range writeWorkers {
		store.goWrite("busy"+strconv.Itoa(i), func() {
			started <- struct{}{}

			<-release
			ran.Add(1)
		})
	}

	for range writeWorkers {
		<-started
	}

	const queued = 3

	for i := range queued {
		store.goWrite("queued"+strconv.Itoa(i), func() { ran.Add(1) })
	}

	closed := make(chan struct{})

	go func() {
		_ = store.Close()

		close(closed)
	}()

	// Close 必须等待排队写，不能在它们执行前返回。
	select {
	case <-closed:
		t.Fatal("Close returned before queued writes ran")
	case <-time.After(20 * time.Millisecond):
	}

	close(release)

	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return after writes completed")
	}

	if got, want := ran.Load(), int64(writeWorkers+queued); got != want {
		t.Fatalf("ran=%d want %d (queued writes must run before Close returns)", got, want)
	}
}

// recordInflight 记录本轮同时在途数峰值（同 key 只允许 1）。
func recordInflight(maxSeen, inflight *atomic.Int64) {
	maxSeen.Store(max(maxSeen.Load(), inflight.Add(1)))
	inflight.Add(-1)
}

// TestSameKeyWritesAreSerializedAndCoalesced（#417）：同一 key 的写必须按提交顺序
// 串行落地，且尚未开始的同 key 写只保留最后一笔（覆盖语义），避免每请求一次
// SetBind 在慢链路上按请求数堆积。
func TestSameKeyWritesAreSerializedAndCoalesced(t *testing.T) {
	t.Parallel()

	var store Upstash

	release := make(chan struct{})
	started := make(chan struct{})
	seen := make(chan int, 8)

	var (
		inflight atomic.Int64
		maxSeen  atomic.Int64
	)

	store.goWrite("k", func() {
		close(started)

		<-release

		recordInflight(&maxSeen, &inflight)

		seen <- 0
	})

	<-started

	for i := 1; i <= 5; i++ {
		store.goWrite("k", func() {
			recordInflight(&maxSeen, &inflight)

			seen <- i
		})
	}

	close(release)

	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	close(seen)

	if maxSeen.Load() != 1 {
		t.Fatalf("同 key 并发执行 %d 次，必须串行", maxSeen.Load())
	}

	var order []int
	for v := range seen {
		order = append(order, v)
	}

	// 首笔已开工不可合并；排队中的 5 笔只保留最后一笔。
	if len(order) != 2 || order[0] != 0 || order[1] != 5 {
		t.Fatalf("order=%v want [0 5]（首笔 + 合并后的最后一笔）", order)
	}
}

// TestDifferentKeysRunConcurrently：不同 key 的写由写者池并行执行，慢链路吞吐
// 不应退化为单写者的 1/RTT。
func TestDifferentKeysRunConcurrently(t *testing.T) {
	t.Parallel()

	var store Upstash

	release := make(chan struct{})
	started := make(chan struct{}, writeWorkers)

	for i := range writeWorkers {
		store.goWrite("k"+strconv.Itoa(i), func() {
			started <- struct{}{}

			<-release
		})
	}

	for range writeWorkers {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatalf("不同 key 的写没有并发执行（已启动 %d/%d）", len(started), writeWorkers)
		}
	}

	close(release)

	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}
