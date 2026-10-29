package upstream

import (
	"context"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// testDial 拨号器签名别名：用例里写全 func(context.Context, string, string)
// (net.Conn, error) 会把断言挤成一行两屏。
type testDial = func(ctx context.Context, network, addr string) (net.Conn, error)

// dialCounter 记录被调用次数的假拨号器（只用来分辨「用了哪一版出口」）。
type dialCounter struct {
	calls atomic.Int32
}

func (d *dialCounter) dial(context.Context, string, string) (net.Conn, error) {
	d.calls.Add(1)

	return nil, errUnused
}

// TestSetAccountRouteRebuildsSlotClients 出口热替换后必须重建按槽缓存的 client：
// 旧缓存里的 Transport 闭包着旧拨号器，复用等于热改不生效。
func TestSetAccountRouteRebuildsSlotClients(t *testing.T) {
	t.Parallel()

	client := New()
	acct := &auth.Auth{UID: "u-slot"}

	first := &dialCounter{}

	client.SetAccountRoute(func(*auth.Auth) (string, testDial) { return "slot-a", first.dial })

	before := client.HTTPForAccount(acct)
	if _, err := before.Transport.(*http.Transport).DialContext(context.Background(), "tcp", "127.0.0.1:9"); err == nil {
		t.Fatal("假拨号器应当报错")
	}

	if got := first.calls.Load(); got != 1 {
		t.Fatalf("首个 client 应使用 first 拨号器，calls=%d", got)
	}

	second := &dialCounter{}

	client.SetAccountRoute(func(*auth.Auth) (string, testDial) { return "slot-a", second.dial })

	after := client.HTTPForAccount(acct)
	if after == before {
		t.Fatal("出口替换后同一槽必须换新 client（旧 Transport 闭包着旧拨号器）")
	}

	if _, err := after.Transport.(*http.Transport).DialContext(context.Background(), "tcp", "127.0.0.1:9"); err == nil {
		t.Fatal("假拨号器应当报错")
	}

	if got := second.calls.Load(); got != 1 {
		t.Fatalf("新 client 应使用 second 拨号器，calls=%d", got)
	}

	if got := first.calls.Load(); got != 1 {
		t.Fatalf("旧拨号器不得被新 client 复用，calls=%d", got)
	}
}

// TestSetAccountRouteConcurrentWithSlotClient 出口热替换与逐请求选路并发安全。
// 面板保存配置会在服务运行期重接选路，而每个 chat/短请求都读它——普通字段
// 与在途请求构成数据竞争，本用例在 -race 下见证。
func TestSetAccountRouteConcurrentWithSlotClient(t *testing.T) {
	t.Parallel()

	client := New()
	acct := &auth.Auth{UID: "u-slot"}
	dial := func(context.Context, string, string) (net.Conn, error) { return nil, errUnused }

	stop := make(chan struct{})

	var wg sync.WaitGroup

	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
					_ = client.HTTPForAccount(acct)
				}
			}
		})
	}

	// 有界翻转：既覆盖「同槽换拨号器」也覆盖「换槽/停用」，无需 sleep。
	for i := range 200 {
		if i%3 == 0 {
			client.SetAccountRoute(nil)

			continue
		}

		slot := "slot-a"
		if i%2 == 0 {
			slot = "slot-b"
		}

		client.SetAccountRoute(func(*auth.Auth) (string, testDial) { return slot, dial })
	}

	close(stop)
	wg.Wait()
}
