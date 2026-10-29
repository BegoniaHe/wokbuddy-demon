package upstream

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// TestSetDialerDropsIdleConnections 出口热替换必须关掉旧出口的空闲连接（issue #39）：
// 共享 Transport 里已建好的空闲连接是在旧出口上拨出来的，复用它们等于热改不生效，
// 要等 IdleConnTimeout（30s）自然过期才换新出口。
func TestSetDialerDropsIdleConnections(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	t.Cleanup(server.Close)

	var oldCalls, newCalls atomic.Int64

	client := New()
	client.SetDialer(countingDial(&oldCalls))
	httpGetOK(t, client.HTTP, server.URL)

	client.SetDialer(countingDial(&newCalls))
	// 不手工 CloseIdleConnections：热改自身必须让旧空闲连接失效。
	httpGetOK(t, client.HTTP, server.URL)

	if oldCalls.Load() == 0 {
		t.Fatal("首请求应经首装拨号器")
	}

	if newCalls.Load() == 0 {
		t.Fatal("换拨号器后仍复用旧空闲连接（新拨号器一次都没被调用）")
	}
}

// TestSetAccountRouteDropsSlotIdleConnections 按槽 client 的空闲连接同样不得跨出口
// 复用：热改账号选路后旧槽的空闲连接必须关闭（否则新拨号器永远不被调用）。
func TestSetAccountRouteDropsSlotIdleConnections(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	t.Cleanup(server.Close)

	account := &auth.Auth{UID: "u-slot"}

	var oldCalls, newCalls atomic.Int64

	client := New()
	client.SetAccountRoute(func(*auth.Auth) (string, testDial) {
		return "slot-a", countingDial(&oldCalls)
	})
	httpGetOK(t, client.HTTPForAccount(account), server.URL)

	client.SetAccountRoute(func(*auth.Auth) (string, testDial) {
		return "slot-a", countingDial(&newCalls)
	})
	httpGetOK(t, client.HTTPForAccount(account), server.URL)

	if newCalls.Load() == 0 {
		t.Fatal("出口热替换后仍复用旧槽的空闲连接（新拨号器一次都没被调用）")
	}
}
