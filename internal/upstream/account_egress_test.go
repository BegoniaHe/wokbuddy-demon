package upstream

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// egressProbe 记录槽拨号器被调用次数，并把连接真正建到 httptest 服务器
// （issue #39：断言账号作用域内的出站确实经绑定槽，而不是 c.HTTP 直连）。
type egressProbe struct{ calls atomic.Int64 }

func (p *egressProbe) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	p.calls.Add(1)

	var dialer net.Dialer

	conn, err := dialer.DialContext(ctx, network, addr)
	if err != nil {
		return nil, fmt.Errorf("test dial: %w", err)
	}

	return conn, nil
}

// newEgressClient 构造真实 Transport 的 Client：全部 base 指向 httptest 服务器，
// 账号路由接到记录调用的槽拨号器。
func newEgressClient(t *testing.T) (*Client, *auth.Auth, *egressProbe) {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"today_checked_in":false}}`))
	}))
	t.Cleanup(server.Close)

	client := New()
	client.ChatBaseCN = server.URL
	client.BillingBaseCN = server.URL
	client.WebBaseCN = server.URL
	// 计费/签到重试间隔：避免单次抖动用例白等默认 2s。
	client.RetryDelay = time.Millisecond

	probe := &egressProbe{}

	client.SetAccountRoute(func(*auth.Auth) (string, testDial) {
		return "slot-a", probe.dial
	})

	account := &auth.Auth{
		UID: "u-egress", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999,
	}

	return client, account, probe
}

// TestAccountEgressUsesSlotDial 逐个出站面断言：传入账号时确实经槽拨号器
// （issue #39 覆盖 chat 之外的余额/签到/保活/上报/模型探测/工具面）。
//
// 响应体对多数端点不是合法业务数据，函数可能返回错误——本用例只关心「有没有
// 走槽」，故不校验返回值。
func TestAccountEgressUsesSlotDial(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		call func(*Client, *auth.Auth)
	}{
		{"FetchModels", func(c *Client, a *auth.Auth) { _, _ = c.FetchModels(a) }},
		{"CreditPackages", func(c *Client, a *auth.Auth) { _, _, _, _ = c.CreditPackages(a) }},
		{"DailyCheckin", func(c *Client, a *auth.Auth) { _ = c.DailyCheckin(a) }},
		{"CheckinActivityStatus", func(c *Client, a *auth.Auth) { _, _ = c.CheckinActivityStatus(a) }},
		{"RefreshToken", func(c *Client, a *auth.Auth) { _ = c.RefreshToken(a) }},
		{"ReportChatActivity", func(c *Client, a *auth.Auth) {
			_ = c.ReportChatActivity(a, "conv-1", "req-1")
		}},
		{"ReportDesktopEvent", func(c *Client, a *auth.Auth) {
			_ = c.ReportDesktopEvent(a, DesktopEvent{"eventCode": "app_open"})
		}},
		{"TravelStatus", func(c *Client, a *auth.Auth) { _, _ = c.TravelStatus(a) }},
		{"FetchAccountProfile", func(c *Client, a *auth.Auth) { _, _ = c.FetchAccountProfile(a) }},
		{"ModelBusy", func(c *Client, a *auth.Auth) { _, _ = c.ModelBusy(context.Background(), a, []string{"glm-5.2"}) }},
		{"WebFetch", func(c *Client, a *auth.Auth) {
			_, _ = c.WebFetch(context.Background(), a, WebFetchRequest{URL: "https://example.com"}, ChatMeta{})
		}},
		{"Search", func(c *Client, a *auth.Auth) {
			_, _ = c.Search(context.Background(), a, "hello world", SearchOptions{}, ChatMeta{})
		}},
		{"AgenticSearch", func(c *Client, a *auth.Auth) {
			_, _ = c.AgenticSearch(context.Background(), a, "hello world", "public", ChatMeta{})
		}},
		{"EditImage", func(c *Client, a *auth.Auth) {
			_, _, _ = c.EditImage(context.Background(), a, []byte(`{}`), "", ChatMeta{})
		}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			client, account, probe := newEgressClient(t)
			testCase.call(client, account)

			if probe.calls.Load() == 0 {
				t.Fatal("出站未经槽拨号器（回落了 c.HTTP 直连）")
			}
		})
	}
}

// TestSetDialerHotSwap 运行期替换拨号器（issue #39 热生效）：替换后新连接走新
// 拨号器。替换只改 atomic 指针 + 关闭空闲连接 + 清按槽缓存，不就地改 Transport
// （故与在途请求无数据竞争，`go test -race` 覆盖）。
func TestSetDialerHotSwap(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	client := New()

	var oldCalls, newCalls atomic.Int64

	client.SetDialer(countingDial(&oldCalls))
	httpGetOK(t, client.HTTP, server.URL)

	if oldCalls.Load() == 0 {
		t.Fatal("首请求应经首装拨号器")
	}

	// 不手工关连接池：SetDialer 自身必须丢弃旧出口的空闲连接（否则连接池复用
	// 会让第二次请求不再拨号，热改要等 IdleConnTimeout 才生效）。
	client.SetDialer(countingDial(&newCalls))
	httpGetOK(t, client.HTTP, server.URL)

	if newCalls.Load() == 0 {
		t.Fatal("替换后新连接应走新拨号器")
	}
}

// countingDial 返回「记一次调用 + 真实建连」的拨号器。
func countingDial(counter *atomic.Int64) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		counter.Add(1)

		var dialer net.Dialer

		conn, err := dialer.DialContext(ctx, network, addr)
		if err != nil {
			return nil, fmt.Errorf("test dial: %w", err)
		}

		return conn, nil
	}
}

// httpGetOK 发一次 GET 并关闭响应体（只关心是否有请求真的发出去）。
func httpGetOK(t *testing.T, hc *http.Client, url string) {
	t.Helper()

	resp, err := hc.Get(url)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}

	_ = resp.Body.Close()
}

// TestHTTPForAccountFallsBackWithoutRoute 未绑定槽（AccountRoute 返回空）时回落
// c.HTTP：与 issue #39 之前的零回归语义一致。
func TestHTTPForAccountFallsBackWithoutRoute(t *testing.T) {
	t.Parallel()

	client := New()

	if got := client.HTTPForAccount(&auth.Auth{UID: "u"}); got != client.HTTP {
		t.Fatal("未配置路由时应回落 c.HTTP")
	}

	if got := client.HTTPForAccount(nil); got != client.HTTP {
		t.Fatal("nil 账号应回落 c.HTTP")
	}

	client.SetAccountRoute(func(*auth.Auth) (string, testDial) {
		return "", nil
	})

	if got := client.HTTPForAccount(&auth.Auth{UID: "u"}); got != client.HTTP {
		t.Fatal("空槽应回落 c.HTTP")
	}
}

// TestHTTPForAccountCachesPerSlot 同一槽复用缓存 client，不同槽互不共享；
// 是否 fail-closed 由 AccountRoute 的 dial 决定（此处断言不混用连接池）。
func TestHTTPForAccountCachesPerSlot(t *testing.T) {
	t.Parallel()

	client := New()
	dial := func(context.Context, string, string) (net.Conn, error) { return nil, errUnused }

	client.SetAccountRoute(func(a *auth.Auth) (string, testDial) {
		return a.UID, dial
	})

	first := client.HTTPForAccount(&auth.Auth{UID: "slot-a"})
	if other := client.HTTPForAccount(&auth.Auth{UID: "slot-b"}); other == first {
		t.Fatal("不同槽不得共用 client")
	}

	if again := client.HTTPForAccount(&auth.Auth{UID: "slot-a"}); again != first {
		t.Fatal("同一槽应复用缓存 client")
	}

	// 短请求面与 chat 面的键前缀不同：不能共用实例（Transport 参数不同）。
	if chat := client.chatHTTPForAccount(&auth.Auth{UID: "slot-a"}, 0); chat == first {
		t.Fatal("chat 与短请求面不得共用 client")
	}

	// 出口热替换后必须清空缓存：旧出口的空闲连接不得继续被复用。
	client.SetDialer(nil)

	if fresh := client.HTTPForAccount(&auth.Auth{UID: "slot-a"}); fresh == first {
		t.Fatal("SetDialer 后应丢弃按槽缓存的 client")
	}
}
