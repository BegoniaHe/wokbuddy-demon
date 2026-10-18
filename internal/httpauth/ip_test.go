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

func TestClientIPTrustsRealIPFromProxy(t *testing.T) {
	t.Parallel()

	trusted := mustProxies(t, "127.0.0.1/32")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-Real-IP", "203.0.113.1")
	req.Header.Set("X-Forwarded-For", "203.0.113.2")

	if got := ClientIP(req, trusted); got != "203.0.113.1" {
		t.Fatalf("ClientIP=%q want X-Real-IP", got)
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
	}
	for in, want := range cases {
		if got := normalizeIP(in); got != want {
			t.Errorf("normalizeIP(%q)=%q want %q", in, got, want)
		}
	}
}
