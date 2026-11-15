// proxysub_test.go 钉住代理订阅导入（issue #490）：三种输入形态、非法条目单独跳过、
// 刷新失败保留上一次成功结果、订阅 URL 打码。这些用例改写包级 fetcher 与缓存，
// 必须串行执行。
package main

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/outbound"
	"github.com/BegoniaHe/wokbuddy-demon/internal/panel"
)

// errProxySubTestFailed 测试用静态错误（err113）。
var errProxySubTestFailed = errors.New("订阅不可用")

func TestParseProxySubscriptionURLList(t *testing.T) {
	t.Parallel()

	text := strings.Join([]string{
		"# 注释",
		"https://1.2.3.4:8080",
		"socks5://user:p%40ss@proxy.example:1080",
		"",
		"ss://YWVzLTEyOC1nY206cGFzcw==@5.6.7.8:8388#node",
		"// 注释",
	}, "\n")

	proxies, skipped := parseProxySubscription(text)

	if len(proxies) != 2 {
		t.Fatalf("proxies=%v want 2", proxies)
	}

	if len(skipped) != 1 || !strings.Contains(skipped[0], "ss://") {
		t.Errorf("skipped=%v want 1 条 ss 跳过", skipped)
	}

	for _, proxy := range proxies {
		if _, err := outbound.ParseProxy(proxy); err != nil {
			t.Errorf("解析结果应可被 ParseProxy 接受: %q: %v", proxy, err)
		}
	}
}

func TestParseProxySubscriptionBase64(t *testing.T) {
	t.Parallel()

	list := "http://10.0.0.1:3128\nhttps://user:pass@10.0.0.2:8443\n"
	encoded := base64.StdEncoding.EncodeToString([]byte(list))

	proxies, skipped := parseProxySubscription(encoded)
	if len(proxies) != 2 || len(skipped) != 0 {
		t.Fatalf("proxies=%v skipped=%v want 2/0", proxies, skipped)
	}
}

func TestParseProxySubscriptionClashBlock(t *testing.T) {
	t.Parallel()

	text := `port: 7890
proxies:
  - name: "office-http"
    type: http
    server: 1.2.3.4
    port: 8080
    username: alice
    password: "s3cret"
  - name: "home-socks"
    type: socks5
    server: proxy.example
    port: 1080
  - name: "ss-node"
    type: ss
    server: 5.6.7.8
    port: 8388
proxy-groups:
  - name: auto
`

	proxies, skipped := parseProxySubscription(text)

	if len(proxies) != 2 {
		t.Fatalf("proxies=%v want 2", proxies)
	}

	if proxies[0] != "http://alice:s3cret@1.2.3.4:8080" {
		t.Errorf("http 条目=%q", proxies[0])
	}

	if proxies[1] != "socks5://proxy.example:1080" {
		t.Errorf("socks5 条目=%q", proxies[1])
	}

	if len(skipped) != 1 || !strings.Contains(skipped[0], "ss") {
		t.Errorf("skipped=%v want 1 条 ss 跳过", skipped)
	}
}

func TestParseProxySubscriptionClashInline(t *testing.T) {
	t.Parallel()

	text := `proxies:
  - {name: inline, type: https, server: 9.9.9.9, port: 443, username: u, password: p}
`

	proxies, skipped := parseProxySubscription(text)
	if len(proxies) != 1 || len(skipped) != 0 {
		t.Fatalf("proxies=%v skipped=%v", proxies, skipped)
	}

	if proxies[0] != "https://u:p@9.9.9.9:443" {
		t.Errorf("entry=%q", proxies[0])
	}
}

// TestParseProxySubscriptionClashTLS tls:true 的 http 节点必须导出成 https，
// 否则代理口令会以明文发到出口（issue #531）。
func TestParseProxySubscriptionClashTLS(t *testing.T) {
	t.Parallel()

	text := `proxies:
  - name: tls-http
    type: http
    server: 10.1.2.3
    port: 443
    tls: true
    username: alice
    password: secret
`

	proxies, skipped := parseProxySubscription(text)
	if len(proxies) != 1 || len(skipped) != 0 {
		t.Fatalf("proxies=%v skipped=%v want 1/0", proxies, skipped)
	}

	if proxies[0] != "https://alice:secret@10.1.2.3:443" {
		t.Errorf("tls:true 的 http 节点必须导出成 https，got %q", proxies[0])
	}
}

// TestParseProxySubscriptionClashSocks5TLSUnsupported socks5 + tls 无出站支持：
// 跳过并给原因，不得降级成明文 socks5（issue #531）。
func TestParseProxySubscriptionClashSocks5TLSUnsupported(t *testing.T) {
	t.Parallel()

	text := `proxies:
  - name: socks-tls
    type: socks5
    server: 10.1.2.4
    port: 1080
    tls: true
`

	proxies, skipped := parseProxySubscription(text)
	if len(proxies) != 0 {
		t.Fatalf("socks5+tls 不得降级成明文导出，proxies=%v", proxies)
	}

	if len(skipped) != 1 || !strings.Contains(strings.ToLower(skipped[0]), "tls") {
		t.Errorf("skipped=%v want 1 条 TLS 不支持原因", skipped)
	}
}

// TestParseProxySubscriptionClashNestedBlocks 嵌套列表（alpn）与嵌套映射（ws-opts）
// 不得拆出新条目、不得吞掉后续外层字段（issue #531）。
func TestParseProxySubscriptionClashNestedBlocks(t *testing.T) {
	t.Parallel()

	text := `proxies:
  - name: nested
    type: http
    server: 1.2.3.4
    port: 8080
    alpn:
      - h2
      - http/1.1
    ws-opts:
      path: /ws
      headers:
        Host: example.com
    username: alice
    password: secret
  - name: next
    type: socks5
    server: 5.6.7.8
    port: 1080
`

	proxies, skipped := parseProxySubscription(text)
	if len(skipped) != 0 {
		t.Fatalf("skipped=%v want 0", skipped)
	}

	want := []string{
		"http://alice:secret@1.2.3.4:8080",
		"socks5://5.6.7.8:1080",
	}
	if len(proxies) != len(want) {
		t.Fatalf("proxies=%v want %v", proxies, want)
	}

	for i := range want {
		if proxies[i] != want[i] {
			t.Errorf("proxies[%d]=%q want %q", i, proxies[i], want[i])
		}
	}
}

// TestParseProxySubscriptionClashTrailingComment 行尾注释（# 前需空白）不得让
// 整条被拒或污染端口值（issue #531）。
func TestParseProxySubscriptionClashTrailingComment(t *testing.T) {
	t.Parallel()

	text := `proxies:
  - name: commented  # 家宽
    type: https
    server: 7.7.7.7
    port: 443 # 备用
`

	proxies, skipped := parseProxySubscription(text)
	if len(proxies) != 1 || len(skipped) != 0 {
		t.Fatalf("proxies=%v skipped=%v want 1/0", proxies, skipped)
	}

	if proxies[0] != "https://7.7.7.7:443" {
		t.Errorf("entry=%q want https://7.7.7.7:443", proxies[0])
	}
}

func TestMaskProxySubURL(t *testing.T) {
	t.Parallel()

	got := panel.MaskProxySubURL("https://user:tok@sub.example.com/api/v1/x?token=secret")
	if got != "https://sub.example.com" {
		t.Errorf("mask=%q want scheme://host", got)
	}

	if strings.Contains(got, "secret") || strings.Contains(got, "tok") {
		t.Errorf("mask 泄露 token: %q", got)
	}

	if got := panel.MaskProxySubURL("not a url"); !strings.Contains(got, "invalid") {
		t.Errorf("非法 URL 应回落到占位: %q", got)
	}
}

// TestResolveProxySubscriptionsKeepsLastSuccess 第二次拉取失败：沿用上一次成功结果。
//
//nolint:paralleltest // 改写包级 fetcher 与订阅缓存，必须串行。
func TestResolveProxySubscriptionsKeepsLastSuccess(t *testing.T) {
	restoreProxySubFetcher(t)

	fail := false

	proxySubFetcher = func(string) (string, error) {
		if fail {
			return "", errProxySubTestFailed
		}

		return "http://10.0.0.1:3128\n", nil
	}

	if got := resolveProxySubscriptions([]string{"https://sub.example/s"}); len(got) != 1 {
		t.Fatalf("首次解析=%v want 1", got)
	}

	fail = true

	got := resolveProxySubscriptions([]string{"https://sub.example/s"})
	if len(got) != 1 || got[0] != "http://10.0.0.1:3128" {
		t.Fatalf("刷新失败应沿用上次结果, got %v", got)
	}
}

func TestNormalizeProxySubscriptions(t *testing.T) {
	t.Parallel()

	cfg := &Config{}
	cfg.Upstream.ProxySubscriptions = []string{
		"  https://sub.example/api?token=x  ",
		"",
	}

	if err := cfg.normalizeProxySubscriptions(); err != nil {
		t.Fatalf("合法订阅被拒: %v", err)
	}

	if len(cfg.Upstream.ProxySubscriptions) != 1 ||
		cfg.Upstream.ProxySubscriptions[0] != "https://sub.example/api?token=x" {
		t.Errorf("归一结果=%v", cfg.Upstream.ProxySubscriptions)
	}

	for _, bad := range []string{"ftp://sub.example/x", "sub.example/x", ""} {
		cfg := &Config{}
		cfg.Upstream.ProxySubscriptions = []string{bad}

		if bad == "" {
			continue
		}

		if err := cfg.normalizeProxySubscriptions(); err == nil {
			t.Errorf("%q 应被拒绝", bad)
		}
	}
}

// TestBuildProxyRegistryMergesSubscription 订阅结果并入同一份全局池。
//
//nolint:paralleltest // 改写包级 fetcher，必须串行。
func TestBuildProxyRegistryMergesSubscription(t *testing.T) {
	restoreProxySubFetcher(t)

	proxySubFetcher = func(string) (string, error) {
		return "http://10.0.0.1:3128\nhttp://10.0.0.2:3128\n", nil
	}

	cfg := &Config{}
	cfg.Upstream.Proxies = []string{"socks5://127.0.0.1:1080"}
	cfg.Upstream.ProxySubscriptions = []string{"https://sub.example/s"}

	reg, err := buildProxyRegistry(cfg)
	if err != nil {
		t.Fatalf("buildProxyRegistry: %v", err)
	}

	proxyPool := reg.Global()
	if proxyPool == nil || proxyPool.Len() != 3 {
		t.Fatalf("全局池条数=%v want 3", proxyPool)
	}
}

// restoreProxySubFetcher 还原包级 fetcher（用例串行执行）。
func restoreProxySubFetcher(t *testing.T) {
	t.Helper()

	original := proxySubFetcher

	t.Cleanup(func() { proxySubFetcher = original })
}
