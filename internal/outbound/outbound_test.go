package outbound

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseProxy(t *testing.T) {
	cases := []struct {
		raw, scheme, addr, user string
		wantErr                 bool
	}{
		{"http://127.0.0.1:8080", "http", "127.0.0.1:8080", "", false},
		{"http://u:p@host:3128", "http", "host:3128", "u", false},
		{"https://proxy.example", "https", "proxy.example:443", "", false},
		{"socks5://127.0.0.1", "socks5", "127.0.0.1:1080", "", false},
		{"socks5h://u@h:1081", "socks5h", "h:1081", "u", false},
		{"ftp://h:21", "", "", "", true},
		{"http://", "", "", "", true},
	}
	for _, c := range cases {
		p, err := ParseProxy(c.raw)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseProxy(%q) want error", c.raw)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseProxy(%q) err=%v", c.raw, err)
			continue
		}
		if p.scheme != c.scheme || p.addr != c.addr || p.user != c.user {
			t.Errorf("ParseProxy(%q)=%+v want scheme=%s addr=%s user=%s", c.raw, p, c.scheme, c.addr, c.user)
		}
	}
}

// connectProxy 起一个最小 HTTP CONNECT 代理，隧道到目标。
func connectProxy(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "need CONNECT", http.StatusMethodNotAllowed)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijack", http.StatusInternalServerError)
			return
		}
		client, _, err := hj.Hijack()
		if err != nil {
			return
		}
		defer client.Close()
		target, err := net.Dial("tcp", r.Host)
		if err != nil {
			client.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
			return
		}
		defer target.Close()
		client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
		done := make(chan struct{}, 2)
		go func() { io.Copy(target, client); done <- struct{}{} }()
		go func() { io.Copy(client, target); done <- struct{}{} }()
		<-done
	}))
}

// helloServer 目标 HTTP 服务，返回 "hello"。
func helloServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "hello")
	}))
}

func httpGetViaConn(t *testing.T, conn net.Conn, addr string) string {
	t.Helper()
	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", addr)
	body, _ := io.ReadAll(conn)
	return string(body)
}

func TestPoolDialViaHTTPProxy(t *testing.T) {
	proxy := connectProxy(t)
	defer proxy.Close()
	target := helloServer(t)
	defer target.Close()

	pool, err := New([]string{proxy.URL})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pool.DialContext(context.Background(), "tcp", target.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial via proxy: %v", err)
	}
	defer conn.Close()
	if body := httpGetViaConn(t, conn, target.Listener.Addr().String()); !strings.Contains(body, "hello") {
		t.Fatalf("body=%q want hello", body)
	}
}

func TestPoolFallbackAndCooldown(t *testing.T) {
	dead := deadAddr(t)
	proxy := connectProxy(t)
	defer proxy.Close()
	target := helloServer(t)
	defer target.Close()

	pool, err := New([]string{"http://" + dead, proxy.URL})
	if err != nil {
		t.Fatal(err)
	}
	// 首个（dead）失败 → 冷却 → 回落代理成功。
	conn, err := pool.DialContext(context.Background(), "tcp", target.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial should fall back to proxy: %v", err)
	}
	conn.Close()
	// dead 已冷却：第二次直接走代理。
	if !pool.inCooldown(pool.states[0], time.Now()) {
		t.Error("failed proxy should be in cooldown")
	}
}

func TestPoolAllFailReturnsProxyDial(t *testing.T) {
	pool, err := New([]string{"http://" + deadAddr(t)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.DialContext(context.Background(), "tcp", "127.0.0.1:9")
	if !errors.Is(err, ErrProxyDial) {
		t.Fatalf("err=%v want ErrProxyDial", err)
	}
}

func TestPoolEmptyIsDirect(t *testing.T) {
	pool, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	if pool.Len() != 0 {
		t.Fatalf("Len=%d want 0", pool.Len())
	}
	target := helloServer(t)
	defer target.Close()
	conn, err := pool.DialContext(context.Background(), "tcp", target.Listener.Addr().String())
	if err != nil {
		t.Fatalf("direct dial: %v", err)
	}
	defer conn.Close()
	if body := httpGetViaConn(t, conn, target.Listener.Addr().String()); !strings.Contains(body, "hello") {
		t.Fatalf("body=%q want hello", body)
	}
}

func TestPoolDialViaSOCKS5(t *testing.T) {
	ln := socks5Server(t)
	defer ln.Close()
	target := helloServer(t)
	defer target.Close()

	pool, err := New([]string{"socks5://" + ln.Addr().String()})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pool.DialContext(context.Background(), "tcp", target.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial via socks5: %v", err)
	}
	defer conn.Close()
	if body := httpGetViaConn(t, conn, target.Listener.Addr().String()); !strings.Contains(body, "hello") {
		t.Fatalf("body=%q want hello", body)
	}
}

// deadAddr 返回一个拒绝连接的地址（监听后立即关闭，端口随即不可达）。
func deadAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// socks5Server 起一个最小 no-auth SOCKS5 代理（CONNECT）。
func socks5Server(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go serveSOCKS5(c)
		}
	}()
	return ln
}

func serveSOCKS5(c net.Conn) {
	defer c.Close()
	head := make([]byte, 2)
	if _, err := io.ReadFull(c, head); err != nil {
		return
	}
	if _, err := io.ReadFull(c, make([]byte, int(head[1]))); err != nil {
		return
	}
	c.Write([]byte{0x05, 0x00}) // no-auth
	req := make([]byte, 4)
	if _, err := io.ReadFull(c, req); err != nil {
		return
	}
	var host string
	switch req[3] {
	case 0x01:
		b := make([]byte, 4)
		io.ReadFull(c, b)
		host = net.IP(b).String()
	case 0x03:
		l := make([]byte, 1)
		io.ReadFull(c, l)
		b := make([]byte, int(l[0]))
		io.ReadFull(c, b)
		host = string(b)
	case 0x04:
		b := make([]byte, 16)
		io.ReadFull(c, b)
		host = net.IP(b).String()
	}
	pb := make([]byte, 2)
	if _, err := io.ReadFull(c, pb); err != nil {
		return
	}
	port := int(pb[0])<<8 | int(pb[1])
	target, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 2*time.Second)
	if err != nil {
		c.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	defer target.Close()
	c.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	done := make(chan struct{}, 2)
	go func() { io.Copy(target, c); done <- struct{}{} }()
	go func() { io.Copy(c, target); done <- struct{}{} }()
	<-done
}

// hangProxy 起一个「接受 TCP 但永不响应握手」的代理（issue #106 的卡死形态）。
func hangProxy(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// 握住连接不写任何字节：握手读必然无限阻塞。
			t.Cleanup(func() { c.Close() })
		}
	}()
	return ln
}

// setHandshakeTimeout 在测试期替换握手超时（用完恢复）。
func setHandshakeTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := handshakeTimeout
	handshakeTimeout = d
	t.Cleanup(func() { handshakeTimeout = old })
}

// TestHTTPConnectHandshakeTimeout HTTP CONNECT 代理接受连接但不响应：握手必须在
// 设定超时内失败（issue #106），且 goroutine 不残留（watcher 随错误路径退出）。
func TestHTTPConnectHandshakeTimeout(t *testing.T) {
	setHandshakeTimeout(t, 150*time.Millisecond)
	ln := hangProxy(t)
	defer ln.Close()
	p, err := ParseProxy("http://" + ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	before := runtime.NumGoroutine()
	start := time.Now()
	_, err = p.dial(context.Background(), "tcp", "127.0.0.1:9")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("hang proxy must fail")
	}
	if elapsed > 2*time.Second {
		t.Errorf("handshake took %v: deadline not enforced", elapsed)
	}
	// watcher goroutine 退出：等一小会儿再比对（Close 由 ctx watcher 或读失败触发）。
	time.Sleep(100 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > before+2 {
		t.Errorf("goroutines leaked: before=%d after=%d", before, after)
	}
}

// TestSOCKS5HandshakeTimeout SOCKS5 代理接受连接但不响应：协商必须在设定
// 超时内失败（issue #106）。
func TestSOCKS5HandshakeTimeout(t *testing.T) {
	setHandshakeTimeout(t, 150*time.Millisecond)
	ln := hangProxy(t)
	defer ln.Close()
	p, err := ParseProxy("socks5://" + ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = p.dial(context.Background(), "tcp", "127.0.0.1:9")
	if err == nil {
		t.Fatal("hang socks5 proxy must fail")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("handshake took %v: deadline not enforced", elapsed)
	}
}

// TestHandshakeCtxCancel 握手读阻塞中取消 ctx：watcher 必须立即关闭连接、
// 让 dial 快速失败（issue #106）——不等满 handshakeTimeout。
func TestHandshakeCtxCancel(t *testing.T) {
	setHandshakeTimeout(t, 30*time.Second) // 足够长：只有 ctx 取消能提前打断
	ln := hangProxy(t)
	defer ln.Close()
	p, err := ParseProxy("http://" + ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err = p.dial(ctx, "tcp", "127.0.0.1:9")
	if err == nil {
		t.Fatal("ctx cancel must interrupt handshake")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("cancel took %v: watcher not closing conn", elapsed)
	}
}

// TestHandshakeSuccessClearsDeadline 握手成功后一次性 deadline 必须被清除：
// 隧道连接上的长读（聊天 SSE）不受 handshakeTimeout 误杀（issue #106 验收）。
// 行为断言：等到超过 handshakeTimeout 后再走隧道——若 deadline 未清，读会立即
// 以 timeout 失败；能拿到响应即证明已清。
func TestHandshakeSuccessClearsDeadline(t *testing.T) {
	setHandshakeTimeout(t, 150*time.Millisecond)
	proxy := connectProxy(t)
	defer proxy.Close()
	target := helloServer(t)
	defer target.Close()

	pool, err := New([]string{proxy.URL})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pool.DialContext(context.Background(), "tcp", target.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	// 静默超过握手超时：残留 deadline 会让后续读立即失败。
	time.Sleep(300 * time.Millisecond)
	if body := httpGetViaConn(t, conn, target.Listener.Addr().String()); !strings.Contains(body, "hello") {
		t.Fatalf("tunnel broken after handshake (deadline not cleared?): %q", body)
	}
}

// TestProxyDialFailuresWrapped 代理握手失败仍包装 ErrProxyDial（调用方据此
// 不惩罚账号）——issue #106 不改变代理池的健康语义。
func TestProxyDialFailuresWrapped(t *testing.T) {
	setHandshakeTimeout(t, 150*time.Millisecond)
	ln := hangProxy(t)
	defer ln.Close()
	pool, err := New([]string{"http://" + ln.Addr().String()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.DialContext(context.Background(), "tcp", "127.0.0.1:9"); !errors.Is(err, ErrProxyDial) {
		t.Fatalf("err=%v want ErrProxyDial", err)
	}
}

// TestHandshakeFailureClosesConn 握手超时后底层连接必须被关闭（issue #106）：
// 代理侧读到 EOF 即证明 fd 已回收，不是只让调用方返回而连接悬空。
func TestHandshakeFailureClosesConn(t *testing.T) {
	setHandshakeTimeout(t, 150*time.Millisecond)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	closed := make(chan struct{})
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		// 不响应握手；读方在客户端关闭时得到 EOF/错误。
		buf := make([]byte, 256)
		for {
			if _, err := c.Read(buf); err != nil {
				close(closed)
				c.Close()
				return
			}
		}
	}()

	pool, err := New([]string{"http://" + ln.Addr().String()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.DialContext(context.Background(), "tcp", "127.0.0.1:9"); err == nil {
		t.Fatal("hang proxy must fail")
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("proxy-side conn was not closed after handshake timeout")
	}
}
