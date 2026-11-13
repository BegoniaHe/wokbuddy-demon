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

// TestWriteQueueBoundedWhenRedisStalls redis 不可用时队列按「不同 key 数」增长，
// 排空速率固定：超过上限必须丢最旧的写（并释放其 wg 计数），不能无限吃内存。
func TestWriteQueueBoundedWhenRedisStalls(t *testing.T) {
	t.Parallel()

	var store Upstash

	release := make(chan struct{})
	started := make(chan struct{}, writeWorkers)

	// 占满写者：后续写只能排队。
	for i := range writeWorkers {
		store.goWrite("busy"+strconv.Itoa(i), func() {
			started <- struct{}{}

			<-release
		})
	}

	for range writeWorkers {
		<-started
	}

	for i := range writeQueueMax + 5 {
		store.goWrite("k"+strconv.Itoa(i), func() {})
	}

	store.mu.Lock()
	queued, dropped := len(store.queue), store.dropped
	pending := len(store.pending)
	store.mu.Unlock()

	if queued > writeQueueMax {
		t.Fatalf("queue=%d want <= %d", queued, writeQueueMax)
	}

	if queued < writeQueueMax {
		t.Fatalf("queue=%d want 达到上限 %d", queued, writeQueueMax)
	}

	if dropped == 0 {
		t.Fatal("超限后应计入丢弃计数")
	}

	// 被丢弃的 key 不能残留 pending，否则同 key 新写会原地替换到已丢弃的 job 上。
	if pending != queued {
		t.Fatalf("pending=%d want %d（丢弃项须从 pending 摘除）", pending, queued)
	}

	close(release)

	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
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

// TestRedactSecretsHidesURLPassword 整条连接串里内嵌的口令也必须脱敏：
// ParseURL 失败时 net/url 的 *url.Error 会把连接串原样引号回显。
func TestRedactSecretsHidesURLPassword(t *testing.T) {
	t.Parallel()

	full := "rediss://default:SECRETPASS@host:6379"
	if got := urlPassword(full); got != "SECRETPASS" {
		t.Fatalf("urlPassword=%q want SECRETPASS", got)
	}

	// 无 scheme / 无 userinfo 时不产出口令。
	for _, input := range []string{"host:6379", "rediss://host:6379", "rediss://default@host:6379"} {
		if got := urlPassword(input); got != "" {
			t.Errorf("urlPassword(%q)=%q want 空串", input, got)
		}
	}

	msg := `parse "rediss://default:SECRETPASS@host:6379` + "\n" + `": net/url: invalid control character in URL`
	redacted := redactSecrets(msg, "", urlPassword(full), full)

	if strings.Contains(redacted, "SECRETPASS") {
		t.Fatalf("脱敏后仍含口令: %s", redacted)
	}
}

// TestWriterSurvivesPanic 写者必须隔离单条写的 panic。
//
// fn 里跑 go-redis 命令与调用方的序列化逻辑。一次 panic 若终结写者 goroutine，
// u.busy[job.key] 再也不会清除，claim 此后一直跳过该 key 的所有写（同 key 串行
// 变成永久阻塞），Close 的 wg.Wait 也只能靠超时兜底——镜像静默停更且无日志。
func TestWriterSurvivesPanic(t *testing.T) {
	t.Parallel()

	var store Upstash

	store.goWrite("k-panic", func() { panic("boom") })

	ran := make(chan struct{}, 1)

	// 同 key 的后续写必须照常执行（若 busy 未清，claim 会一直跳过它）。
	store.goWrite("k-panic", func() { ran <- struct{}{} })

	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("panic 之后同 key 的写未被执行（写者已死或 busy 未清除）")
	}

	// Close 必须能正常排空返回，而不是靠 10s 超时。
	done := make(chan struct{})

	go func() {
		_ = store.Close()

		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close 未在预期时间内返回（wg 计数未释放）")
	}
}
