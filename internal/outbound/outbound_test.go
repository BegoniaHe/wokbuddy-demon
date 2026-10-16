package outbound

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
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
