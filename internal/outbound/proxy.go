// Package outbound 出站代理池（issue #15）：HTTP/HTTPS CONNECT 与 SOCKS5 多代理
// 轮换，带失败冷却与健康判定。零第三方依赖（SOCKS5 自实现握手）。
//
// 与账号健康隔离：代理层拨号失败返回 ErrProxyDial，调用方据此不惩罚账号。
package outbound

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrProxyDial 代理层拨号失败的哨兵：调用方（handler）据此跳过账号惩罚。
// 用 %w 包装底层错误，errors.Is 可穿透 http.Client 的 *url.Error。
var ErrProxyDial = errors.New("outbound proxy dial failed")

// 出站拨号参数（与 upstream 连接层口径一致：建连上限 + keepalive 探测）。
const (
	dialTimeout   = 10 * time.Second
	dialKeepAlive = 15 * time.Second
)

// Proxy 单个出站代理（HTTP/HTTPS CONNECT 或 SOCKS5）。
type Proxy struct {
	raw    string
	scheme string // http / https / socks5 / socks5h
	host   string
	addr   string // host:port
	user   string
	pass   string
}

// ParseProxy 解析代理 URL：支持 http://、https://、socks5://、socks5h://，
// 可选 user:pass 认证；缺省端口按 scheme 回落（80/443/1080）。
func ParseProxy(raw string) (*Proxy, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("proxy %q: %w", raw, err)
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, fmt.Errorf("proxy %q: 不支持的 scheme %q（http/https/socks5/socks5h）", raw, u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("proxy %q: 缺少 host", raw)
	}
	port := u.Port()
	if port == "" {
		switch scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		default:
			port = "1080"
		}
	}
	p := &Proxy{raw: raw, scheme: scheme, host: host, addr: net.JoinHostPort(host, port)}
	if u.User != nil {
		p.user = u.User.Username()
		p.pass, _ = u.User.Password()
	}
	return p, nil
}

// dial 经本代理建立到 addr 的连接（network 仅支持 tcp）。
func (p *Proxy) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("proxy: 不支持的 network %q", network)
	}
	if p.scheme == "socks5" || p.scheme == "socks5h" {
		return p.dialSOCKS5(ctx, addr)
	}
	return p.dialHTTPConnect(ctx, addr)
}

func (p *Proxy) dialer() *net.Dialer {
	return &net.Dialer{Timeout: dialTimeout, KeepAlive: dialKeepAlive}
}

// dialHTTPConnect 经 HTTP/HTTPS 代理的 CONNECT 隧道建立连接。
func (p *Proxy) dialHTTPConnect(ctx context.Context, addr string) (net.Conn, error) {
	conn, err := p.dialer().DialContext(ctx, "tcp", p.addr)
	if err != nil {
		return nil, err
	}
	if p.scheme == "https" {
		tconn := tls.Client(conn, &tls.Config{ServerName: p.host})
		if err := tconn.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, err
		}
		conn = tconn
	}
	var b strings.Builder
	b.WriteString("CONNECT " + addr + " HTTP/1.1\r\nHost: " + addr + "\r\n")
	if p.user != "" {
		b.WriteString("Proxy-Authorization: Basic " +
			base64.StdEncoding.EncodeToString([]byte(p.user+":"+p.pass)) + "\r\n")
	}
	b.WriteString("\r\n")
	if _, err := io.WriteString(conn, b.String()); err != nil {
		conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	// CONNECT 的 200 响应无 body；不读 resp.Body（它即隧道本体），仅取状态码。
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect}) //nolint:bodyclose // CONNECT 200 的 resp.Body 即隧道本体，不可关闭
	if err != nil {
		conn.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		conn.Close()
		return nil, fmt.Errorf("proxy CONNECT %s: %s", addr, resp.Status)
	}
	// br 可能已读入隧道首字节，包装以保留缓冲。
	return &bufferedConn{Conn: conn, r: br}, nil
}

// bufferedConn 保留 CONNECT 响应读取时缓冲的隧道字节。
type bufferedConn struct {
	net.Conn

	r *bufio.Reader
}

func (c *bufferedConn) Read(b []byte) (int, error) { return c.r.Read(b) }

// dialSOCKS5 经 SOCKS5 代理建立到 addr 的连接（自实现握手，无第三方依赖）。
func (p *Proxy) dialSOCKS5(ctx context.Context, addr string) (net.Conn, error) {
	conn, err := p.dialer().DialContext(ctx, "tcp", p.addr)
	if err != nil {
		return nil, err
	}
	if err := p.socks5Handshake(conn, addr); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// socks5Handshake 完成 SOCKS5 方法协商 + 认证 + CONNECT 请求。
func (p *Proxy) socks5Handshake(conn net.Conn, addr string) error {
	if err := p.socks5Negotiate(conn); err != nil {
		return err
	}
	return p.socks5Connect(conn, addr)
}

// socks5Negotiate 完成版本/方法协商与（可选）用户名密码认证。
func (p *Proxy) socks5Negotiate(conn net.Conn) error {
	methods := []byte{0x00} // no-auth
	if p.user != "" {
		methods = []byte{0x00, 0x02} // no-auth + username/password
	}
	greet := append([]byte{0x05, byte(len(methods))}, methods...) //nolint:gosec // methods 长度固定 ≤2
	if _, err := conn.Write(greet); err != nil {
		return err
	}
	var sel [2]byte
	if _, err := io.ReadFull(conn, sel[:]); err != nil {
		return err
	}
	if sel[0] != 0x05 {
		return errors.New("socks5: 版本不符")
	}
	switch sel[1] {
	case 0x00:
		return nil
	case 0x02:
		return p.socks5UserPass(conn)
	case 0xff:
		return errors.New("socks5: 无可接受认证方式")
	default:
		return fmt.Errorf("socks5: 不支持的认证方式 %#x", sel[1])
	}
}

// socks5Connect 发送 CONNECT 请求并读取应答。
func (p *Proxy) socks5Connect(conn net.Conn, addr string) error {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 0 || port > 65535 {
		return fmt.Errorf("socks5: 非法端口 %q", portStr)
	}
	req := []byte{0x05, 0x01, 0x00} // CONNECT
	switch ip := net.ParseIP(host); {
	case ip == nil:
		if len(host) > 255 {
			return errors.New("socks5: host 过长")
		}
		req = append(req, 0x03, byte(len(host))) //nolint:gosec // len ≤255 已校验
		req = append(req, host...)
	case ip.To4() != nil:
		req = append(req, 0x01)
		req = append(req, ip.To4()...)
	default:
		req = append(req, 0x04)
		req = append(req, ip.To16()...)
	}
	req = append(req, byte(port>>8), byte(port)) //nolint:gosec // port 已钳 [0,65535]
	if _, err := conn.Write(req); err != nil {
		return err
	}
	return socks5ReadReply(conn)
}

// socks5ReadReply 读取 CONNECT 应答并丢弃 BND.ADDR/BND.PORT。
func socks5ReadReply(conn net.Conn) error {
	var head [4]byte
	if _, err := io.ReadFull(conn, head[:]); err != nil {
		return err
	}
	if head[0] != 0x05 {
		return errors.New("socks5: 应答版本不符")
	}
	if head[1] != 0x00 {
		return fmt.Errorf("socks5: CONNECT 失败（rep=%#x）", head[1])
	}
	var n int
	switch head[3] {
	case 0x01:
		n = 4
	case 0x04:
		n = 16
	case 0x03:
		var l [1]byte
		if _, err := io.ReadFull(conn, l[:]); err != nil {
			return err
		}
		n = int(l[0])
	default:
		return fmt.Errorf("socks5: 非法 ATYP %#x", head[3])
	}
	_, err := io.ReadFull(conn, make([]byte, n+2)) // 丢弃 BND.ADDR + BND.PORT
	return err
}

// socks5UserPass 完成 SOCKS5 用户名/密码认证（RFC 1929）。
func (p *Proxy) socks5UserPass(conn net.Conn) error {
	if len(p.user) > 255 || len(p.pass) > 255 {
		return errors.New("socks5: 凭据过长")
	}
	buf := make([]byte, 0, 2+len(p.user)+1+len(p.pass))
	buf = append(buf, 0x01, byte(len(p.user))) //nolint:gosec // len ≤255 已校验
	buf = append(buf, p.user...)
	buf = append(buf, byte(len(p.pass))) //nolint:gosec // len ≤255 已校验
	buf = append(buf, p.pass...)
	if _, err := conn.Write(buf); err != nil {
		return err
	}
	var rep [2]byte
	if _, err := io.ReadFull(conn, rep[:]); err != nil {
		return err
	}
	if rep[1] != 0x00 {
		return errors.New("socks5: 认证被拒")
	}
	return nil
}
