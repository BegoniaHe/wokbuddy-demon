// tlsdial.go 出站 TLS 拨号：把 http.Transport 的 TLS 握手从标准库默认行为换成
// 本仓可控的路径，以便
//   - 在 TLS 之上重写 CLI chat 头序（header_order.go，默认开）；
//   - 用 profile 的 tls 段复刻官方 Node/BoringSSL ClientHello（tls_fingerprint=node）。
//
// 自定义 DialTLSContext 一旦安装，http.Transport 会忽略 TLSClientConfig 与
// TLSHandshakeTimeout，因此握手、超时与证书校验都在这里落地。代理与按槽拨号仍走
// Client 既有的 dial 函数（SetDialer / slotClient），不绕过出站路由。
package upstream

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"

	utls "github.com/refraction-networking/utls"

	"github.com/BegoniaHe/wokbuddy-demon/internal/clientprofile"
)

// TLS 指纹模式。off = 标准库默认；node = 官方 Node/BoringSSL 形状（需 profile.tls）。
const (
	TLSFingerprintOff  = "off"
	TLSFingerprintNode = "node"
)

var (
	errUnknownTLSFingerprint = errors.New("upstream.tls_fingerprint 只接受 off / node")
	errMissingTLSProfile     = errors.New("tls_fingerprint=node 需要 data/client-profiles/<realm>.json 的 tls 段")
)

// tlsFingerprintConfig 已激活的 uTLS 指纹参数（来自 profile.tls）。
type tlsFingerprintConfig struct {
	spec *clientprofile.TLS
}

// tlsHandshakeState 会话恢复缓存（首次握手时惰性创建）。两种 TLS 实现各有自己的
// 接口类型，不能共用；缓存本身是并发安全的。
type tlsHandshakeState struct {
	goCache   tls.ClientSessionCache
	utlsCache utls.ClientSessionCache
}

// SetHeaderOrder 安装/移除 CLI chat 头序重写（启动期调用，改动需重启）。
// 只有 header_order 与 tls_fingerprint 都为关时才移除自定义 DialTLSContext，
// 让 http.Transport 回到标准库 TLS 路径。
func (c *Client) SetHeaderOrder(enabled bool) {
	c.headerOrder.Store(enabled)
	c.installCustomTLSDial()
}

// SetTLSFingerprint 设置 TLS 指纹模式（启动期调用，改动需重启）。
// node 模式要求 profile 带合法 tls 段；缺失即报错，不静默回落到标准库。
func (c *Client) SetTLSFingerprint(mode string) error {
	switch mode {
	case "", TLSFingerprintOff:
		c.tlsFingerprint.Store(nil)
	case TLSFingerprintNode:
		spec, err := c.profileTLSSpec()
		if err != nil {
			return err
		}

		c.tlsFingerprint.Store(&tlsFingerprintConfig{spec: spec})
	default:
		return fmt.Errorf("%w：%q", errUnknownTLSFingerprint, mode)
	}

	c.installCustomTLSDial()

	return nil
}

// profileTLSSpec 取已加载 realm 的 tls 段；LoadDir 已保证两 realm 一致。
func (c *Client) profileTLSSpec() (*clientprofile.TLS, error) {
	if c == nil || c.Profile == nil || c.Profile.Empty() {
		return nil, errMissingTLSProfile
	}

	for _, realm := range []string{"cn", "global"} {
		if profile := c.Profile.ForRealm(realm); profile != nil && profile.TLS != nil {
			return profile.TLS, nil
		}
	}

	return nil, fmt.Errorf("%w（当前 cn/global 均缺失）", errMissingTLSProfile)
}

// installCustomTLSDial 在全部共享 Transport 上安装或移除 DialTLSContext。
//
// 只在启动期调用：运行期改 Transport 字段会与在途请求构成数据竞争，故配置项
// 标记为需重启。安装后清空闲连接与按槽缓存，避免旧路径的连接被复用。
func (c *Client) installCustomTLSDial() {
	enabled := c.headerOrder.Load() || c.tlsFingerprint.Load() != nil

	c.forEachTransport(func(tr *http.Transport) {
		if enabled {
			tr.DialTLSContext = c.dialTLSContext
		} else {
			tr.DialTLSContext = nil
		}
	})

	c.closeIdleConnections()
	c.invalidateSlotClients()
}

// forEachTransport 遍历共享 Transport（含按 header 超时克隆的 chat client），每个
// 实例只处理一次。
func (c *Client) forEachTransport(apply func(*http.Transport)) {
	seen := make(map[*http.Transport]bool, 4)

	visit := func(client *http.Client) {
		if client == nil {
			return
		}

		transport, ok := client.Transport.(*http.Transport)
		if !ok || seen[transport] {
			return
		}

		seen[transport] = true
		apply(transport)
	}

	visit(c.HTTP)
	visit(c.ChatHTTP)

	c.timeoutMu.Lock()
	for _, client := range c.timeoutClients {
		visit(client)
	}
	c.timeoutMu.Unlock()
}

// dialTLSContext 共享 Transport 的稳定 TLS 拨号包装：每次握手读取当前代理拨号器与
// 指纹开关，与 c.dialContext 同口径（热改代理对新连接立即生效）。
func (c *Client) dialTLSContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return c.dialTLSWith(ctx, network, addr, c.dialContext)
}

// dialTLSWith 用给定底层拨号器建连并完成 TLS 握手。dial 负责返回已包装
// writeDeadlineConn 的连接（全局拨号器与按槽拨号器都在各自入口完成包装）。
func (c *Client) dialTLSWith(
	ctx context.Context, network, addr string,
	dial func(ctx context.Context, network, addr string) (net.Conn, error),
) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}

	raw, err := dial(ctx, network, addr)
	if err != nil {
		return nil, err
	}

	conn, err := c.handshakeTLS(ctx, raw, host)
	if err != nil {
		_ = raw.Close()

		return nil, err
	}

	return conn, nil
}

// handshakeTLS 完成握手并在需要时套上明文头序重写。失败直接返回错误，不回落标准库
// TLS（回落会泄漏调用方明确要求隐藏的指纹）。
func (c *Client) handshakeTLS(ctx context.Context, raw net.Conn, host string) (net.Conn, error) {
	handshakeCtx, cancel := context.WithTimeout(ctx, tlsHandshakeTimeout)
	defer cancel()

	var (
		conn net.Conn
		err  error
	)

	if config := c.tlsFingerprint.Load(); config != nil {
		conn, err = c.handshakeUTLS(handshakeCtx, raw, host, config.spec)
	} else {
		conn, err = c.handshakeGoTLS(handshakeCtx, raw, host)
	}

	if err != nil {
		return nil, err
	}

	if c.headerOrder.Load() {
		return &headerOrderConn{Conn: conn}, nil
	}

	return conn, nil
}

// handshakeGoTLS 标准库 TLS 握手：保留系统根证书、SNI、TLS1.2+ 与会话恢复。
func (c *Client) handshakeGoTLS(ctx context.Context, raw net.Conn, host string) (net.Conn, error) {
	state := c.tlsHandshakeState()

	conn := tls.Client(raw, &tls.Config{
		ServerName:         host,
		MinVersion:         tls.VersionTLS12,
		ClientSessionCache: state.goCache,
		RootCAs:            c.tlsRootCAs,
	})

	if err := conn.HandshakeContext(ctx); err != nil {
		return nil, fmt.Errorf("tls 握手失败 %s: %w", host, err)
	}

	return conn, nil
}

// handshakeUTLS 用 profile 的 ClientHelloSpec 完成真实握手。配置层同时禁用动态记录
// 分片，并打开会话恢复（第二次连接走 PSK + padding）。
func (c *Client) handshakeUTLS(
	ctx context.Context, raw net.Conn, host string, spec *clientprofile.TLS,
) (net.Conn, error) {
	helloSpec, err := clientHelloSpec(spec, host)
	if err != nil {
		return nil, fmt.Errorf("构造 ClientHello: %w", err)
	}

	state := c.tlsHandshakeState()

	uconn := utls.UClient(raw, &utls.Config{
		ServerName:                  host,
		MinVersion:                  utls.VersionTLS12,
		MaxVersion:                  utls.VersionTLS13,
		ClientSessionCache:          state.utlsCache,
		OmitEmptyPsk:                true,
		RootCAs:                     c.tlsRootCAs,
		DynamicRecordSizingDisabled: true,
	}, utls.HelloCustom)

	if err := uconn.ApplyPreset(helloSpec); err != nil {
		return nil, fmt.Errorf("应用 ClientHello spec: %w", err)
	}

	if err := uconn.HandshakeContext(ctx); err != nil {
		return nil, fmt.Errorf("tls 指纹握手失败 %s: %w", host, err)
	}

	return uconn, nil
}

// tlsHandshakeState 惰性创建两种 TLS 实现的会话缓存（每 Client 一份，跨连接/跨槽共享）。
func (c *Client) tlsHandshakeState() *tlsHandshakeState {
	c.tlsStateMu.Lock()
	defer c.tlsStateMu.Unlock()

	if c.tlsState == nil {
		c.tlsState = &tlsHandshakeState{
			goCache:   tls.NewLRUClientSessionCache(64),
			utlsCache: utls.NewLRUClientSessionCache(64),
		}
	}

	return c.tlsState
}

// FingerprintMode 返回当前生效的 TLS 指纹模式（面板/测试只读）。
func (c *Client) FingerprintMode() string {
	if c != nil && c.tlsFingerprint.Load() != nil {
		return TLSFingerprintNode
	}

	return TLSFingerprintOff
}

// HeaderOrderEnabled 是否启用 CLI chat 头序重写。
func (c *Client) HeaderOrderEnabled() bool {
	return c != nil && c.headerOrder.Load()
}
