package redisstore

import (
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
// 队列/写者只能初始化一次，不能出现字段写竞争。
func TestZeroValueConcurrentInit(t *testing.T) {
	t.Parallel()

	var store Upstash

	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			store.goWrite(func() {})
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
	started := make(chan struct{})

	var ran atomic.Int64

	// 首个写阻塞住写者，后续写只能排队，模拟 Close 前已提交但未执行的写。
	store.goWrite(func() {
		close(started)
		<-release
		ran.Add(1)
	})

	<-started

	const queued = 3

	for range queued {
		store.goWrite(func() { ran.Add(1) })
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

	if got, want := ran.Load(), int64(1+queued); got != want {
		t.Fatalf("ran=%d want %d (queued writes must run before Close returns)", got, want)
	}
}

// TestWritesRunInSubmissionOrder（#417）：并发提交的写必须按提交顺序落地，慢的
// 先提交写不能被后提交的写越过，否则旧快照 / 已解绑的绑定会覆盖新值。
func TestWritesRunInSubmissionOrder(t *testing.T) {
	t.Parallel()

	var store Upstash

	const total = 32

	var (
		mu   sync.Mutex
		seen []int
	)

	for i := range total {
		store.goWrite(func() {
			// 先提交的写人为拖慢，验证后提交者不会越过它。
			time.Sleep(time.Millisecond)

			mu.Lock()

			seen = append(seen, i)
			mu.Unlock()
		})
	}

	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(seen) != total {
		t.Fatalf("ran=%d want %d", len(seen), total)
	}

	for idx, got := range seen {
		if got != idx {
			t.Fatalf("write order[%d]=%d want %d（写必须按提交顺序落地）", idx, got, idx)
		}
	}
}
