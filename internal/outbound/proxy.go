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
	"sync"
	"time"
)

// ErrProxyDial 代理层拨号失败的哨兵：调用方（handler）据此跳过账号惩罚。
// 用 %w 包装底层错误，errors.Is 可穿透 http.Client 的 *url.Error。
var ErrProxyDial = errors.New("outbound proxy dial failed")

// 出站拨号参数（与 upstream 连接层口径一致：建连上限 + keepalive 探测）。
const (
	dialTimeout   = 10 * time.Second
	dialKeepAlive = 15 * time.Second

	// SOCKS5 协议常量（RFC 1928 / RFC 1929）。
	socks5Version         = 0x05
	socks5AuthNone        = 0x00
	socks5AuthUserPass    = 0x02
	socks5AuthNoAvailable = 0xff
	socks5CmdConnect      = 0x01
	socks5AtypIPv4        = 0x01
	socks5AtypDomain      = 0x03
	socks5AtypIPv6        = 0x04
	socks5ReplySuccess    = 0x00
	maxSocks5HostLen      = 255
)

// handshakeTimeout 代理握手（HTTP CONNECT / SOCKS5 协商+认证+CONNECT）的一次性
// deadline（issue #106）。DialContext 的 10s 只覆盖「连到代理」——代理接受 TCP
// 后不返回握手应答时没有任何一层能打断，调用方 goroutine、fd 与账号池在途租约
// 会一直占用（聊天路径 http.Client{Timeout:0} 最直接暴露）。与 dialTimeout 同值：
// 握手与建连同属「到达代理并可用」的上界。
//
// deadline 是**一次性**的：握手成功后由 endHandshake(…, true) 清除，长流（聊天
// SSE）从此只受上游 idle/写 deadline 约束。包级 var 仅为测试可注入小值（生产
// 恒 10s，默认值不变）。
var handshakeTimeout = dialTimeout

var (
	errURL                   = errors.New("非法 URL")
	errParseProxyFailed      = errors.New("ParseProxy failed")
	errProxyHost             = errors.New("proxy 地址缺少 host")
	errDialFailed            = errors.New("dial failed")
	errDialHTTPConnectFailed = errors.New("dialHTTPConnect failed")
	errSocks5                = errors.New("socks5: 版本不符")
	errSocks52               = errors.New("socks5: 无可接受认证方式")
	errSocks5NegotiateFailed = errors.New("socks5Negotiate failed")
	errSocks5ConnectFailed   = errors.New("socks5Connect failed")
	errSocks5Host            = errors.New("socks5: host 过长")
	errSocks53               = errors.New("socks5: 应答版本不符")
	errSocks5ReadReplyFailed = errors.New("socks5ReadReply failed")
	errSocks54               = errors.New("socks5: 凭据过长")
	errSocks55               = errors.New("socks5: 认证被拒")
)

// Proxy 单个出站代理（HTTP/HTTPS CONNECT 或 SOCKS5）。
type Proxy struct {
	raw              string
	scheme           string // http / https / socks5 / socks5h
	host             string
	addr             string // host:port
	user             string
	pass             string
	handshakeTimeout time.Duration
}

// redactURLError 去掉 url.Error 携带的原始 URL（可能含 user:pass），只保留解析原因。
func redactURLError(err error) error {
	if uerr, ok := errors.AsType[*url.Error](err); ok {
		return uerr.Err
	}

	return errURL
}

// ParseProxy 解析代理 URL：支持 http://、https://、socks5://、socks5h://，
// 可选 user:pass 认证；缺省端口按 scheme 回落（80/443/1080）。
func ParseProxy(raw string) (*Proxy, error) {
	trimmed := strings.TrimSpace(raw)
	// 四段式 host:port:user:pass（无 scheme）按 socks5 处理，见 isFourPartProxy。
	if isFourPartProxy(trimmed) {
		return parseFourPartProxy(trimmed)
	}

	url2, err := url.Parse(trimmed)
	if err != nil {
		// 不直接回显 raw / err：两者都可能含 user:pass。
		return nil, fmt.Errorf("proxy 地址解析失败: %w", redactURLError(err))
	}

	scheme := strings.ToLower(url2.Scheme)
	if !supportedProxyScheme[scheme] {
		return nil, fmt.Errorf("proxy: 不支持的 scheme %q（http/https/socks5/socks5h）: %w",
			url2.Scheme, errParseProxyFailed)
	}

	host := url2.Hostname()
	if host == "" {
		return nil, errProxyHost
	}

	port := url2.Port()
	if port == "" {
		port = defaultProxyPort(scheme)
	}

	proxy := &Proxy{
		raw:              raw,
		scheme:           scheme,
		host:             host,
		addr:             net.JoinHostPort(host, port),
		handshakeTimeout: handshakeTimeout,
	}
	if url2.User != nil {
		proxy.user = url2.User.Username()
		proxy.pass, _ = url2.User.Password()
	}

	return proxy, nil
}

// supportedProxyScheme 出站支持的代理 scheme 集合。
var supportedProxyScheme = map[string]bool{
	"http": true, "https": true, "socks5": true, "socks5h": true,
}

// isFourPartProxy 判定四段式 host:port:user:pass（无 scheme）：机场/代理商家最常见
// 的默认口令形态（issue #39）。含 [ 的 IPv6 字面量与带 scheme/路径/认证的串不适用
// 此形态，交回标准 URL 解析。
func isFourPartProxy(trimmed string) bool {
	return !strings.Contains(trimmed, "://") && strings.Count(trimmed, ":") >= 3 &&
		!strings.ContainsAny(trimmed, "[/@")
}

// defaultProxyPort 缺省端口按 scheme 回落（80/443/1080）。
func defaultProxyPort(scheme string) string {
	switch scheme {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return "1080"
	}
}

// Masked 脱敏后的代理地址 scheme://host:port：**永不**包含 user/pass，面板展示
// 与日志一律用它（issue #39）。
func (p *Proxy) Masked() string {
	return p.scheme + "://" + p.addr
}

// redact 把本代理的口令/用户名从任意字符串里抹掉，并截断到 limit 字符。
// 用于把底层错误串落到健康快照（错误可能回显 URL）。
func (p *Proxy) redact(text string, limit int) string {
	if p.pass != "" {
		text = strings.ReplaceAll(text, p.pass, "***")
	}

	if p.user != "" {
		text = strings.ReplaceAll(text, p.user, "***")
	}

	runes := []rune(text)
	if len(runes) > limit {
		return string(runes[:limit])
	}

	return text
}

// parseFourPartProxy 解析四段式 host:port:user:pass（按 socks5 落地）。
// 错误信息一律不含原始串——它含口令。
func parseFourPartProxy(raw string) (*Proxy, error) {
	parts := strings.SplitN(raw, ":", 4)
	if len(parts) != 4 {
		return nil, errParseProxyFailed
	}

	host := strings.TrimSpace(parts[0])
	if host == "" {
		return nil, errProxyHost
	}

	portStr := strings.TrimSpace(parts[1])

	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("proxy: 端口必须是 1..65535 的整数: %w", errParseProxyFailed)
	}

	return &Proxy{
		raw:              raw,
		scheme:           "socks5",
		host:             host,
		addr:             net.JoinHostPort(host, strconv.Itoa(port)),
		user:             parts[2],
		pass:             parts[3],
		handshakeTimeout: handshakeTimeout,
	}, nil
}

// dial 经本代理建立到 addr 的连接（network 仅支持 tcp）。
func (p *Proxy) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("proxy: 不支持的 network %q: %w", network, errDialFailed)
	}

	if p.scheme == "socks5" || p.scheme == "socks5h" {
		return p.dialSOCKS5(ctx, addr)
	}

	return p.dialHTTPConnect(ctx, addr)
}

func (*Proxy) dialer() *net.Dialer {
	return &net.Dialer{Timeout: dialTimeout, KeepAlive: dialKeepAlive}
}

// beginHandshake 在代理握手开始前给底层 conn 设置一次性 deadline，并启动
// ctx watcher：ctx 取消（客户端断开 / 请求超时）时关闭连接，把阻塞在握手
// 读/写里的 goroutine 放出来（issue #106）。返回的清理函数必须被调用
// （成功路径清 deadline + 停 watcher；失败路径由调用方 defer 同样语义）。
//
// 为什么两个机制都要：deadline 覆盖「代理接受连接但不响应」的卡死；watcher
// 覆盖「ctx 在 deadline 前被取消」——后者 deadline 到了自然也会失败，但
// watcher 让取消**立即**生效，不等满 handshakeTimeout。
func beginHandshake(ctx context.Context, conn net.Conn, timeout time.Duration) (clearDeadline, stop func()) {
	_ = conn.SetDeadline(time.Now().Add(timeout))
	done := make(chan struct{})

	var once sync.Once

	stopWatcher := func() { once.Do(func() { close(done) }) }

	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close() // 唤醒阻塞中的握手读写
		case <-done:
		}
	}()

	clearDeadline = func() {
		_ = conn.SetDeadline(time.Time{}) // 一次性 deadline：清掉，长流不受限
	}

	return clearDeadline, stopWatcher
}

// endHandshake 是握手出口的统一清理：stop 停 watcher（幂等），ok=true
// （握手成功）时 clear 清掉一次性 deadline；ok=false（失败路径）时 deadline
// 留着无害——连接随即被调用方 Close。
func endHandshake(clearDeadline, stop func(), ok bool) {
	stop()

	if ok {
		clearDeadline()
	}
}

// dialHTTPConnect 经 HTTP/HTTPS 代理的 CONNECT 隧道建立连接。
func (p *Proxy) dialHTTPConnect(ctx context.Context, addr string) (net.Conn, error) {
	conn, err := p.dialer().DialContext(ctx, "tcp", p.addr)
	if err != nil {
		return nil, fmt.Errorf("dialHTTPConnect: %w", err)
	}
	// 握手期 deadline + ctx watcher（issue #106）：见 beginHandshake。
	// defer 保证四条退出路径（TLS 握手失败 / 写失败 / 读应答失败 / 非 200）都停
	// watcher，不留 goroutine。
	clearDeadline, stop := beginHandshake(ctx, conn, p.handshakeTimeout)
	defer endHandshake(clearDeadline, stop, false)

	if p.scheme == "https" {
		tconn := tls.Client(conn, &tls.Config{ServerName: p.host})
		if herr := tconn.HandshakeContext(ctx); herr != nil {
			conn.Close()
			return nil, fmt.Errorf("dialHTTPConnect: %w", herr)
		}

		conn = tconn
	}

	var builder strings.Builder
	builder.WriteString("CONNECT " + addr + " HTTP/1.1\r\nHost: " + addr + "\r\n")

	if p.user != "" {
		builder.WriteString("Proxy-Authorization: Basic " +
			base64.StdEncoding.EncodeToString([]byte(p.user+":"+p.pass)) + "\r\n")
	}

	builder.WriteString("\r\n")

	if _, werr := io.WriteString(conn, builder.String()); werr != nil {
		conn.Close()
		return nil, fmt.Errorf("dialHTTPConnect: %w", werr)
	}

	reader := bufio.NewReader(conn)
	// CONNECT 的 200 响应无 body；不读 resp.Body（它即隧道本体），仅取状态码。
	//nolint:bodyclose // CONNECT 200 的 resp.Body 即隧道本体，不可关闭
	resp, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("dialHTTPConnect: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		conn.Close()
		return nil, fmt.Errorf("proxy CONNECT %s: %s: %w", addr, resp.Status, errDialHTTPConnectFailed)
	}
	// 握手成功：清掉一次性 deadline（长流从此只受上游 idle/写 deadline 约束），
	// 并显式停 watcher（defer 的 endHandshake(…, false) 不会重复清 deadline）。
	endHandshake(clearDeadline, stop, true)
	// br 可能已读入隧道首字节，包装以保留缓冲。
	return &bufferedConn{Conn: conn, r: reader}, nil
}

// bufferedConn 保留 CONNECT 响应读取时缓冲的隧道字节。
type bufferedConn struct {
	net.Conn

	r *bufio.Reader
}

func (c *bufferedConn) Read(b []byte) (int, error) {
	return c.r.Read(b) //nolint:wrapcheck // io.Reader 必须原样透传 io.EOF，包装会破坏 io.Copy 的等值判断
}

// dialSOCKS5 经 SOCKS5 代理建立到 addr 的连接（自实现握手，无第三方依赖）。
func (p *Proxy) dialSOCKS5(ctx context.Context, addr string) (net.Conn, error) {
	conn, err := p.dialer().DialContext(ctx, "tcp", p.addr)
	if err != nil {
		return nil, fmt.Errorf("dialSOCKS5: %w", err)
	}
	// 握手期 deadline + ctx watcher（issue #106）：见 beginHandshake。
	clearDeadline, stop := beginHandshake(ctx, conn, p.handshakeTimeout)
	if err := p.socks5Handshake(conn, addr); err != nil {
		endHandshake(clearDeadline, stop, false)
		conn.Close()

		return nil, err
	}

	endHandshake(clearDeadline, stop, true)

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
	methods := []byte{socks5AuthNone} // no-auth
	if p.user != "" {
		methods = []byte{socks5AuthNone, socks5AuthUserPass} // no-auth + username/password
	}

	greet := append([]byte{socks5Version, byte(len(methods))}, methods...) //nolint:gosec // methods 长度固定 ≤2
	if _, err := conn.Write(greet); err != nil {
		return fmt.Errorf("socks5Negotiate: %w", err)
	}

	var sel [2]byte
	if _, err := io.ReadFull(conn, sel[:]); err != nil {
		return fmt.Errorf("socks5Negotiate: %w", err)
	}

	if sel[0] != socks5Version {
		return errSocks5
	}

	switch sel[1] {
	case socks5AuthNone:
		return nil
	case socks5AuthUserPass:
		return p.socks5UserPass(conn)
	case socks5AuthNoAvailable:
		return errSocks52
	default:
		return fmt.Errorf("socks5: 不支持的认证方式 %#x: %w", sel[1], errSocks5NegotiateFailed)
	}
}

// socks5Connect 发送 CONNECT 请求并读取应答。
func (*Proxy) socks5Connect(conn net.Conn, addr string) error {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("socks5Connect: %w", err)
	}

	port, err := strconv.Atoi(portStr)
	if err != nil || port < 0 || port > 65535 {
		return fmt.Errorf("socks5: 非法端口 %q: %w", portStr, errSocks5ConnectFailed)
	}

	req := []byte{socks5Version, socks5CmdConnect, socks5AuthNone} // CONNECT

	switch ip := net.ParseIP(host); {
	case ip == nil:
		if len(host) > maxSocks5HostLen {
			return errSocks5Host
		}

		req = append(req, socks5AtypDomain, byte(len(host))) //nolint:gosec // len ≤255 已校验
		req = append(req, host...)
	case ip.To4() != nil:
		req = append(req, socks5AtypIPv4)
		req = append(req, ip.To4()...)
	default:
		req = append(req, socks5AtypIPv6)
		req = append(req, ip.To16()...)
	}

	req = append(req, byte(port>>8), byte(port)) //nolint:gosec // port 已钳 [0,65535]
	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("socks5Connect: %w", err)
	}

	return socks5ReadReply(conn)
}

// socks5ReadReply 读取 CONNECT 应答并丢弃 BND.ADDR/BND.PORT。
func socks5ReadReply(conn net.Conn) error {
	var head [4]byte
	if _, err := io.ReadFull(conn, head[:]); err != nil {
		return fmt.Errorf("socks5ReadReply: %w", err)
	}

	if head[0] != socks5Version {
		return errSocks53
	}

	if head[1] != socks5ReplySuccess {
		return fmt.Errorf("socks5: CONNECT 失败（rep=%#x）: %w", head[1], errSocks5ReadReplyFailed)
	}

	var n int

	switch head[3] {
	case socks5AtypIPv4:
		n = 4
	case socks5AtypIPv6:
		n = 16
	case socks5AtypDomain:
		var l [1]byte
		if _, err := io.ReadFull(conn, l[:]); err != nil {
			return fmt.Errorf("socks5ReadReply: %w", err)
		}

		n = int(l[0])
	default:
		return fmt.Errorf("socks5: 非法 ATYP %#x: %w", head[3], errSocks5ReadReplyFailed)
	}

	_, err := io.ReadFull(conn, make([]byte, n+2)) // 丢弃 BND.ADDR + BND.PORT
	if err != nil {
		return fmt.Errorf("socks5ReadReply: %w", err)
	}

	return nil
}

// socks5UserPass 完成 SOCKS5 用户名/密码认证（RFC 1929）。
func (p *Proxy) socks5UserPass(conn net.Conn) error {
	if len(p.user) > 255 || len(p.pass) > 255 {
		return errSocks54
	}

	buf := make([]byte, 0, 2+len(p.user)+1+len(p.pass))
	buf = append(buf, 0x01, byte(len(p.user))) //nolint:gosec // len ≤255 已校验
	buf = append(buf, p.user...)
	buf = append(buf, byte(len(p.pass))) //nolint:gosec // len ≤255 已校验

	buf = append(buf, p.pass...)
	if _, err := conn.Write(buf); err != nil {
		return fmt.Errorf("socks5UserPass: %w", err)
	}

	var rep [2]byte
	if _, err := io.ReadFull(conn, rep[:]); err != nil {
		return fmt.Errorf("socks5UserPass: %w", err)
	}

	if rep[1] != socks5ReplySuccess {
		return errSocks55
	}

	return nil
}
