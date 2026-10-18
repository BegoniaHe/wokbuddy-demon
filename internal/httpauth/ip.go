package httpauth

import (
	"fmt"
	"net"
	"net/http"
	"strings"
)

// ParseTrustedProxies 把配置中的 CIDR（或单个 IP）解析为网段列表。
//
// 只接受显式配置的可信反代网段：转发头（X-Real-IP / X-Forwarded-For）只有在
// TCP 对端属于这些网段时才可采信。空列表 = 不信任任何转发头。
func ParseTrustedProxies(cidrs []string) ([]*net.IPNet, error) {
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, raw := range cidrs {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		if _, n, err := net.ParseCIDR(s); err == nil {
			out = append(out, n)
			continue
		}
		// 允许直接写单个 IP（IPv4 等价 /32，IPv6 等价 /128）。
		ip := net.ParseIP(s)
		if ip == nil {
			return nil, fmt.Errorf("trusted_proxies: 非法 CIDR / IP %q", raw)
		}
		bits := 128
		if ip.To4() != nil {
			ip = ip.To4()
			bits = 32
		}
		out = append(out, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
	}
	return out, nil
}

// ClientIP 解析真实客户端 IP。
//
// 安全前提：X-Real-IP 与 X-Forwarded-For 都是客户端可伪造的普通请求头。只有
// TCP 对端（RemoteAddr）落在 trusted 网段内时才采信转发头；否则一律返回对端
// 地址。这修掉两类问题：
//   - 直连公网时伪造 X-Real-IP 绕过 IP 白名单 / 登录锁定；
//   - 反代部署时所有请求都记成反代地址，限速把全部客户端算成同一来源。
//
// 取值顺序：
//  1. 对端不可信 → 对端地址；
//  2. 可采信 X-Real-IP（反代通常以 $remote_addr 覆盖写入）；
//  3. 从 X-Forwarded-For 右往左跳过可信反代，取第一个非可信地址；
//  4. 全部可信 → 最左侧地址；都取不到 → 对端地址。
func ClientIP(r *http.Request, trusted []*net.IPNet) string {
	if r == nil {
		return ""
	}
	peer := normalizeIP(r.RemoteAddr)
	if len(trusted) == 0 || !inNets(peer, trusted) {
		return peer
	}

	if realIP := normalizeIP(r.Header.Get("X-Real-IP")); realIP != "" {
		return realIP
	}

	parts := splitForwarded(r.Header.Values("X-Forwarded-For"))
	for i := len(parts) - 1; i >= 0; i-- {
		if inNets(parts[i], trusted) {
			continue
		}
		return parts[i]
	}
	if len(parts) > 0 {
		return parts[0]
	}
	return peer
}

// splitForwarded 解析 X-Forwarded-For 的全部值（多次出现按顺序拼接），
// 丢弃空项与非法 IP（如 "unknown"）。
func splitForwarded(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			if ip := normalizeIP(part); ip != "" {
				out = append(out, ip)
			}
		}
	}
	return out
}

// normalizeIP 兼容 "1.2.3.4"、"1.2.3.4:5678"、"[::1]:5678" 与 "[::1]"。
// 无法解析时返回空串。
func normalizeIP(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "[") {
		if host, _, err := net.SplitHostPort(s); err == nil {
			s = host
		} else if end := strings.IndexByte(s, ']'); end > 0 {
			s = s[1:end]
		}
	} else if host, _, err := net.SplitHostPort(s); err == nil {
		s = host
	}
	if ip := net.ParseIP(s); ip != nil {
		return ip.String()
	}
	return ""
}

func inNets(ip string, nets []*net.IPNet) bool {
	addr := net.ParseIP(ip)
	if addr == nil {
		return false
	}
	for _, n := range nets {
		if n != nil && n.Contains(addr) {
			return true
		}
	}
	return false
}
