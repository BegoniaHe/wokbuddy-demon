// dns_test.go 钉住出站 DNS 可指定（上游同步窗口 issue #193）：
// 空值零回归（系统解析器）、注入后查询确实走自定义服务器、地址补齐缺省端口。
package upstream

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// startFakeDNS 起一个最小 UDP「DNS 服务器」：只统计收到的查询并回 FORMERR，
// 用于证明解析请求确实经过该地址（不解析真实域名，避免触网）。
func startFakeDNS(t *testing.T) (addr string, queries *atomic.Int32) {
	t.Helper()

	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}

	counter := &atomic.Int32{}

	go func() {
		buf := make([]byte, 512)

		for {
			n, peer, readErr := conn.ReadFrom(buf)
			if readErr != nil {
				return
			}

			counter.Add(1)

			if n < 12 {
				continue
			}
			// 回一个 header-only 的 FORMERR（QR=1, rcode=1）：解析必然失败，
			// 但足以让调用方确认「查询发到了这里」。
			resp := make([]byte, 12)
			copy(resp, buf[:2]) // ID
			resp[2] = 0x81      // QR=1, RD=1
			resp[3] = 0x81      // RA=1, rcode=1（FORMERR）
			_, _ = conn.WriteTo(resp, peer)
		}
	}()

	t.Cleanup(func() { _ = conn.Close() })

	return conn.LocalAddr().String(), counter
}

// TestDialerResolverInjected 注入 DNS 服务器后，Dialer 带自定义 Resolver 且查询
// 确实打到该服务器。
//
//nolint:paralleltest // SetDNSServers 是进程级状态，必须串行执行
func TestDialerResolverInjected(t *testing.T) {
	addr, queries := startFakeDNS(t)

	SetDNSServers([]string{addr})

	t.Cleanup(func() { SetDNSServers(nil) })

	dialer := newDialer()
	if dialer.Resolver == nil {
		t.Fatal("注入 DNS 后 Dialer 必须带自定义 Resolver")
	}

	// 假服务器只回 FORMERR，解析必然失败——这里只关心「有没有打到它」。
	// 短 ctx：解析器默认会重试多轮，测试不需要等它跑完。
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, _ = dialer.Resolver.LookupHost(ctx, "upstream.test")

	if queries.Load() == 0 {
		t.Fatal("DNS 查询未经过注入的服务器")
	}
}

// TestDialerResolverDefaultNil 空配置零回归：Dialer 不带自定义 Resolver。
//
//nolint:paralleltest // SetDNSServers 是进程级状态，必须串行执行
func TestDialerResolverDefaultNil(t *testing.T) {
	SetDNSServers(nil)

	if dialer := newDialer(); dialer.Resolver != nil {
		t.Fatal("未配置 dns_servers 时不应注入 Resolver")
	}
}

// TestDNSServerAddr 缺省端口补齐：host → host:53、host:port 原样、IPv6 带括号。
func TestDNSServerAddr(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"1.1.1.1":        "1.1.1.1:53",
		"1.1.1.1:5353":   "1.1.1.1:5353",
		"dns.example":    "dns.example:53",
		"[2606:4700::1]": "[2606:4700::1]:53",
		"2606:4700::1":   "[2606:4700::1]:53",
	}

	for in, want := range cases {
		if got := dnsServerAddr(in); got != want {
			t.Errorf("dnsServerAddr(%q)=%q want %q", in, got, want)
		}
	}
}
