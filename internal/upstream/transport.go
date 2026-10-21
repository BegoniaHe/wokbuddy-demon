// transport.go 出站 Transport 构造的单一事实来源（连接层加固，吸收 kongjianguan
// 4 连击实测经验的前三件，见 .claude/reports/fork-scan-absorb.md T-2）：
// 真正禁 h2 / TLS 握手超时 / 短 keepalive 探测，全参数集中定义可测试可调整。
// 第四件 DisableKeepAlives 按报告 trade-off 不吸收（每请求 TLS 握手开销与连接
// 复用意图相反），分析见 .claude/reports/transport-hardening.md。
package upstream

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// 连接层参数集中定义（与 server/backoff.go 同风格：一处定义，测试可回读断言）。
const (
	// dialTimeout TCP 连接建立上限。半死连接的第一道闸：连不上就快速失败
	// 轮转换号，不再干等系统 TCP 重传窗口（kongjianguan 实测半开 TCP 单次
	// TTFB 卡 936s——默认 Dialer 无超时上限）。
	dialTimeout = 10 * time.Second
	// dialKeepAlive TCP keepalive 探测周期。默认 Dialer 2h 才发首个探测——
	// NAT 黑洞里 2h 足够连接半死且被复用。15s 周期让死连接在 15~30s 内被
	// 内核掐掉（RST/ETIMEDOUT），复用侧立即感知而非卡到重传窗口。
	dialKeepAlive = 15 * time.Second
	// tlsHandshakeTimeout TLS 握手上限。现役此前完全缺失——握手挂起时无任何
	// 层兜底（ResponseHeaderTimeout 只在请求写完后才计时），只能干等到
	// HTTP.Client.Timeout(120s)。
	tlsHandshakeTimeout = 10 * time.Second
	// idleConnTimeout 空闲连接池保留时长。从 90s 收到 30s：WAF 风暴后上游
	// NGI 常态性掐闲置连接，90s 池里的连接多半已死（kongjianguan 同款取值）；
	// 复用侧仍有 15s keepalive 兜底识别。
	idleConnTimeout = 30 * time.Second
	// responseHeaderTimeout 聊天 SSE 首字节前（响应头）硬上限。从 120s 收到
	// 60s：kongjianguan 笔记实录成功请求 TTFB 曾到 16s（慢模型冷启动），
	// 60s ≈ 3.75× 观测最坏健康首包，留足慢冷启动余量；同时把半死连接场景的
	// 单请求卡死从 2 分钟压到 1 分钟（MaxRotate 默认 3 次的最坏轮转从 6 分钟
	// 压到 3 分钟）。不取 kongjianguan 的 20s：其 20s 是 DisableKeepAlives+
	// timedConn 20s 写超时组合的取值，我们保留连接复用，须按自身慢冷启动
	// 观测留余量。语义核对（任务书设计纪律）：ResponseHeaderTimeout 只计响应
	// 头到达前的时长，头到达后 SSE 长流不受影响（流中空闲由 IdleTimeout 监控，
	// 见 idle.go），不误杀长流——transport_test.go 有显式回归。
	//
	// 注意：本常量只是 newTransport 的构造默认，main.go 会按 config
	// upstream.header_timeout_seconds 无条件覆盖。因此生产生效值 = config
	// 解析值（未配置时 normalize 回落 120s/300s），本 60s 仅作
	// 「Config 未接线/测试裸用」时的安全网。
	responseHeaderTimeout = 60 * time.Second
	// writeTimeout 单次出站 Write 的上限（每次 Write 前重设，非整请求总时长）。
	// 现役此前完全缺失写方向 deadline：聊天 SSE 路径用 http.Client{Timeout:0}
	// （client.go）且请求 ctx 无 deadline，对端停止读取请求体时 net/http 的 Write
	// 会无限阻塞——ResponseHeaderTimeout 只在请求完全写完后才计时，DialContext
	// 只管建连，无任何一层能打断写阶段。健康连接的每次 Write 毫秒级完成，60s 仅
	// 作「对端完全不消费」的 stall 上界，与 responseHeaderTimeout 同口径，保证
	// 半死连接快速失败并可轮换账号（issue #11；参考 Caddy tcpRWTimeoutConn /
	// Traefik connWithTimeouts 的「包装 conn + 每写设 deadline」做法）。
	//
	// HTTP 与 ChatHTTP 共用同一 Transport 实例（client.go），故本超时同时约束
	// 流式 SSE 与非流式 JSON 两条路径：JSON 路径上传请求体时对端停止消费，也会
	// 在 writeTimeout 处失败，而不再等到 HTTP.Timeout（120s）。这是有意的跨路径
	// 收紧，非仅聊天路径生效。
	writeTimeout = 60 * time.Second
)

// maxIdleConns / maxIdleConnsPerHost 连接池容量（既有值，一并集中定义）。
const (
	maxIdleConns        = 100
	maxIdleConnsPerHost = 20
)

// newDialer 构造出站拨号器（DialContext 的 Timeout/KeepAlive 参数集中于此，
// 供测试回读断言）。
func newDialer() *net.Dialer {
	dialer := &net.Dialer{
		Timeout:   dialTimeout,
		KeepAlive: dialKeepAlive,
	}
	// 出站 DNS 可指定（config dns_servers）：空 = 系统解析器（零回归）。
	if servers := currentDNSServers(); len(servers) > 0 {
		dialer.Resolver = newDNSResolver(servers)
	}

	return dialer
}

// writeDeadlineConn 包装拨号返回的 net.Conn，在每次 Write 前重设写 deadline，
// 让「对端停止读取请求体」的写阻塞有上限（issue #11）。只重写 Write：读方向
// 不设固定 deadline——首字节由 Transport.ResponseHeaderTimeout 管、流中空闲由
// idle.go 的 monitorBody 接管，加固定 read deadline 会误杀合法长 SSE 流。
//
// 不做 Traefik 式的写后清理：每次 Write 前必设新 deadline，且写 deadline 与
// Read 互不影响，残留的过期写 deadline 不会影响后续读或连接池复用。
type writeDeadlineConn struct {
	net.Conn

	writeTimeout time.Duration
}

func (c *writeDeadlineConn) Write(nums []byte) (int, error) {
	if c.writeTimeout > 0 {
		// 与标准库一致：设 deadline 失败只忽略（网络层下次写自会报错）。
		_ = c.SetWriteDeadline(time.Now().Add(c.writeTimeout))
	}

	return c.Conn.Write(nums)
}

// newTransport 构造共享出站 Transport（HTTP 与 ChatHTTP 同一实例，连接池不重复）。
// 分两层防半死连接：
//   - TLS 层：空 TLSNextProto 真正禁 h2（kongjianguan 二次修正的实证：ForceAttemptHTTP2=false
//     只对自定义 Dial 生效，默认 TLS 经 ALPN 仍协商出 h2，半死 h2 流复用表现为
//     "http2: timeout awaiting response headers"——唯一正确写法是置空映射，让 ALPN
//     完成后无 h2 协议可用，连接退回 HTTP/1.1）。
//   - TCP 层：DialContext 10s 建连上限 + 15s keepalive 探测，半开连接在建立期
//     和复用期都能被快速识别（见 dialTimeout/dialKeepAlive 注释）。
//   - 写方向：拨号返回的 conn 包上 writeDeadlineConn，每次 Write 前设 writeTimeout
//     上限，写阻塞不再无限（issue #11，见 writeTimeout 注释）。
func newTransport() *http.Transport {
	return newTransportWithWriteTimeout(writeTimeout)
}

// newTransportWithWriteTimeout 是 newTransport 的可测构造：writeTimeout 参数化，
// 便于测试用小超时验证写 deadline（wt<=0 关闭写 deadline）。
func newTransportWithWriteTimeout(duration time.Duration) *http.Transport {
	dialer := newDialer()
	base := dialer.DialContext

	return &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := base(ctx, network, addr)
			if err != nil {
				return nil, err
			}

			return &writeDeadlineConn{Conn: conn, writeTimeout: duration}, nil
		},
		// 空 TLSNextProto（非 nil）真正禁 h2：见函数注释。必须 make 而非 nil——
		// nil 表示「让标准库注入默认 h2 映射」（kongjianguan 实测：设
		// ForceAttemptHTTP2=false 后日志仍报 h2 timeout，正是这个陷阱）。
		TLSNextProto:          make(map[string]func(authority string, c *tls.Conn) http.RoundTripper),
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		MaxIdleConns:          maxIdleConns,
		MaxIdleConnsPerHost:   maxIdleConnsPerHost,
		IdleConnTimeout:       idleConnTimeout,
		ResponseHeaderTimeout: responseHeaderTimeout,
	}
}

// closeIdler 实现该接口的 RoundTripper 支持清空空闲连接池（*http.Transport、
// http2.Transport 等均满足；测试注入的自定义 RoundTripper 可选择性实现）。
type closeIdler interface {
	CloseIdleConnections()
}

// roundTripCloseIdle 在传输层请求失败后清掉 rt 所属 Transport 的空闲连接池
// （kongjianguan 第 4 件：失败连接可能仍留在空闲池里，等 IdleConnTimeout 才
// 过期，下一个请求会继续捡到它）。
//
// 挂载点（任务书「评估挂载点：错误分类处理处」的结论）：错误分类（Classify）
// 只见业务信封——传输层失败根本没有 body 可分类（见 doJSON/ChatStreamContext
// 对 read body 失败的处理：不进 Classify、不罚号）。这类失败的正确处理正是
// 连接层的池清理，故挂在与 Do 并列的传输层出口（ChatStreamContext 的 Do 错误
// 分支），而非 applyErrorPolicy。
//
// 关闭是 best-effort：rt 为 nil 或未实现 closeIdler（如测试注入的 rtFunc）时
// 静默跳过。CloseIdleConnections 只关空闲连接，不影响在途请求；瞬时代价是
// 下个请求多一次 TCP+TLS 握手，与半死连接被复用卡 60s 的风险完全不成比例。
func roundTripCloseIdle(rt http.RoundTripper) {
	if rt == nil {
		return
	}

	if ci, ok := rt.(closeIdler); ok {
		ci.CloseIdleConnections()
	}
}

// ---------------------------------------------------------------------------
// 出站 DNS 解析器（config dns_servers / 环境变量 WB2A_DNS）
// ---------------------------------------------------------------------------

// dnsDialTimeout 单个 DNS 服务器的建连+查询超时。取 5s：比系统解析器的常见
// 重试窗口短，多个服务器可快速轮换，同时不至于因一次慢查询误杀健康解析。
const dnsDialTimeout = 5 * time.Second

var (
	// dnsServers 出站解析器地址（host 或 host:port）。由 SetDNSServers 注入，
	// 在 upstream.New() 构造 Transport 时读取——**改动需重启进程生效**。
	dnsMu      sync.RWMutex
	dnsServers []string
)

// SetDNSServers 注入出站 DNS 服务器（config `dns_servers` 或环境变量 WB2A_DNS）。
// 空 = 用系统解析器（零回归）。必须在 upstream.New() **之前**调用：Transport 在
// New() 内一次性构造（Dialer 的 Resolver 属于连接层参数，运行期改无效，故该配置
// 列为「需重启」）。非法值由 config.normalize 拦截，这里不再校验。
func SetDNSServers(servers []string) {
	dnsMu.Lock()
	defer dnsMu.Unlock()

	if len(servers) == 0 {
		dnsServers = nil
		return
	}

	dnsServers = append([]string(nil), servers...)
}

// currentDNSServers 返回当前注入的 DNS 服务器副本。
func currentDNSServers() []string {
	dnsMu.RLock()
	defer dnsMu.RUnlock()

	if len(dnsServers) == 0 {
		return nil
	}

	return append([]string(nil), dnsServers...)
}

// newDNSResolver 构造使用指定服务器的解析器：
//   - PreferGo=true：走纯 Go 解析器，不用 cgo（行为可控、跨平台一致）；
//   - Dial 按顺序尝试服务器，首个成功即用；全部失败返回最后一个错误。
//
// 只替换 Resolver，不动 Dialer 的 Timeout/KeepAlive，也不改 DialContext 包装
// （写截止 writeDeadlineConn 必须保留——那不是 DNS 层能替换的东西）。
func newDNSResolver(servers []string) *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var lastErr error

			for _, server := range servers {
				conn, err := (&net.Dialer{Timeout: dnsDialTimeout}).DialContext(ctx, network, dnsServerAddr(server))
				if err == nil {
					return conn, nil
				}

				lastErr = err
			}

			return nil, fmt.Errorf("dns: 所有解析服务器均不可用: %w", lastErr)
		},
	}
}

// dnsServerAddr 补齐 DNS 服务器的缺省端口（53）。已带端口则原样返回；
// 带方括号的 IPv6（"[::1]"）先脱括号再由 JoinHostPort 统一加回，避免双重括号。
func dnsServerAddr(server string) string {
	if _, _, err := net.SplitHostPort(server); err == nil {
		return server
	}

	host := strings.TrimSuffix(strings.TrimPrefix(server, "["), "]")

	return net.JoinHostPort(host, "53")
}
