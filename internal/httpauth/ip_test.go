package httpauth

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func mustProxies(t *testing.T, cidrs ...string) []*net.IPNet {
	t.Helper()

	nets, err := ParseTrustedProxies(cidrs)
	if err != nil {
		t.Fatalf("ParseTrustedProxies(%v): %v", cidrs, err)
	}

	return nets
}

func TestParseTrustedProxies(t *testing.T) {
	t.Parallel()

	nets := mustProxies(t, "127.0.0.1", "::1", "10.0.0.0/8", "  ")
	if len(nets) != 3 {
		t.Fatalf("nets=%d want 3", len(nets))
	}

	if _, err := ParseTrustedProxies([]string{"not-a-cidr"}); err == nil {
		t.Fatal("非法 CIDR 应报错")
	}
}

func TestClientIPIgnoresForwardedFromUntrustedPeer(t *testing.T) {
	t.Parallel()

	trusted := mustProxies(t, "127.0.0.1/32")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "198.51.100.9:1234"
	req.Header.Set("X-Real-IP", "203.0.113.1")
	req.Header.Set("X-Forwarded-For", "203.0.113.2, 203.0.113.3")

	if got := ClientIP(req, trusted); got != "198.51.100.9" {
		t.Fatalf("ClientIP=%q want peer address", got)
	}
}

// XFF 优先于 X-Real-IP：Caddy 缺省会透传客户端自带的 X-Real-IP，
// 只处理 X-Forwarded-For，先信 X-Real-IP 会留下任意伪造来源的口子。
func TestClientIPPrefersForwardedForOverRealIP(t *testing.T) {
	t.Parallel()

	trusted := mustProxies(t, "127.0.0.1/32")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-Real-IP", "203.0.113.1")
	req.Header.Set("X-Forwarded-For", "203.0.113.2")

	if got := ClientIP(req, trusted); got != "203.0.113.2" {
		t.Fatalf("ClientIP=%q want X-Forwarded-For", got)
	}
}

func TestClientIPFallsBackToRealIPWithoutForwardedFor(t *testing.T) {
	t.Parallel()

	trusted := mustProxies(t, "127.0.0.1/32")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-Real-IP", "203.0.113.1")

	if got := ClientIP(req, trusted); got != "203.0.113.1" {
		t.Fatalf("ClientIP=%q want X-Real-IP fallback", got)
	}
}

func TestClientIPSkipsTrustedHops(t *testing.T) {
	t.Parallel()

	trusted := mustProxies(t, "10.0.0.0/8")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.3:1234"
	// 客户端伪造左侧地址；右侧是真实反代追加链。
	req.Header.Set("X-Forwarded-For", "203.0.113.99, 198.51.100.7, 10.0.0.2")

	if got := ClientIP(req, trusted); got != "198.51.100.7" {
		t.Fatalf("ClientIP=%q want first non-trusted from right", got)
	}
}

func TestClientIPAllHopsTrusted(t *testing.T) {
	t.Parallel()

	trusted := mustProxies(t, "10.0.0.0/8")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.3:1234"
	req.Header.Set("X-Forwarded-For", "10.0.0.1, 10.0.0.2")

	if got := ClientIP(req, trusted); got != "10.0.0.1" {
		t.Fatalf("ClientIP=%q want leftmost trusted", got)
	}
}

func TestClientIPNoTrustedConfig(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "198.51.100.9:1234"
	req.Header.Set("X-Real-IP", "203.0.113.1")

	if got := ClientIP(req, nil); got != "198.51.100.9" {
		t.Fatalf("ClientIP=%q want peer address", got)
	}
}

func TestNormalizeIPVariants(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"1.2.3.4":      "1.2.3.4",
		"1.2.3.4:5678": "1.2.3.4",
		"[::1]:5678":   "::1",
		"[::1]":        "::1",
		" ::1 ":        "::1",
		"unknown":      "",
		"":             "",
		// 前导零不是规范形态，ParseIP 直接拒掉：不能走「原串即规范」的快路径。
		"01.2.3.4":       "",
		"1.2.3.004":      "",
		"1.2.3.4.5":      "",
		"::ffff:1.2.3.4": "1.2.3.4",
	}
	for in, want := range cases {
		if got := normalizeIP(in); got != want {
			t.Errorf("normalizeIP(%q)=%q want %q", in, got, want)
		}
	}
}

// TestNormalizeIPMatchesNetIPString 规范 IPv4 快路径（直接返回原串，省掉
// net.IP.String() 的每次分配）必须与 ParseIP + String() 的结果完全一致。
func TestNormalizeIPMatchesNetIPString(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		"0.0.0.0", "9.9.9.9", "10.0.0.1", "127.0.0.1", "255.255.255.255",
		"1.2.3.4", "192.0.2.1", "198.51.100.9", "203.0.113.7",
		"::1", "2001:db8::1", "fe80::1", "::ffff:192.0.2.1",
	} {
		ip := net.ParseIP(raw)
		if ip == nil {
			t.Fatalf("ParseIP(%q)=nil（用例本身非法）", raw)
		}

		if got, want := normalizeIP(raw), ip.String(); got != want {
			t.Errorf("normalizeIP(%q)=%q want %q", raw, got, want)
		}
	}
}
