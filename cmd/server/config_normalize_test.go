// config_normalize_test.go 钉住配置归一化的三处边界：
//   - listen 只给「纯端口」补 ":"，漏写端口的 host 不再被拼成 ":host"；
//   - upstream.proxies 归一为去空白后的串（否则纯空白差异会触发出口注册表重建）；
//   - mergeConfigMaps 忽略分段对象的 null（否则整段静默回落默认值并落盘）。
package main

import (
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/panel"
)

// TestNormalizeListenOnlyAddsColonToBarePort 只有纯数字 listen 补冒号；
// 带 host 但漏端口的值原样交给 net.Listen 报「unknown port」，不伪造 ":" 前缀。
func TestNormalizeListenOnlyAddsColonToBarePort(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"7863":           ":7863",
		":7863":          ":7863",
		"127.0.0.1":      "127.0.0.1",
		"localhost":      "localhost",
		"127.0.0.1:8080": "127.0.0.1:8080",
		"[::1]:8080":     "[::1]:8080",
	}

	for raw, want := range cases {
		config, err := ParseConfig([]byte(`{"listen":"` + raw + `"}`))
		if err != nil {
			t.Fatalf("listen=%q err=%v", raw, err)
		}

		if config.Listen != want {
			t.Errorf("listen=%q → %q, want %q", raw, config.Listen, want)
		}
	}
}

// TestNormalizeProxiesTrims 代理串归一为去空白后的值：热生效判定按原串比较，
// 纯空白差异会让每条配置保存都重建出口注册表（含串行拉订阅）。
func TestNormalizeProxiesTrims(t *testing.T) {
	t.Parallel()

	config, err := ParseConfig([]byte(`{"upstream":{"proxies":[" socks5://127.0.0.1:1080 "]}}`))
	if err != nil {
		t.Fatal(err)
	}

	if len(config.Upstream.Proxies) != 1 || config.Upstream.Proxies[0] != "socks5://127.0.0.1:1080" {
		t.Fatalf("proxies=%q", config.Upstream.Proxies)
	}
}

// TestMergeConfigMapsIgnoresNullSection 分段对象写成 null 时保留原值：
// ParseConfigInto 对非指针结构体忽略 null，整段会静默回落默认值并永久落盘。
func TestMergeConfigMapsIgnoresNullSection(t *testing.T) {
	t.Parallel()

	cur := map[string]any{
		"upstream": map[string]any{"proxies": []any{"socks5://127.0.0.1:1080"}},
		"listen":   ":7863",
	}

	merged := mergeConfigMaps(cur, map[string]any{
		"upstream": nil,
		"listen":   ":9000",
	})

	upstream, ok := merged["upstream"].(map[string]any)
	if !ok {
		t.Fatalf("upstream 被 null 抹掉: %#v", merged["upstream"])
	}

	if _, kept := upstream["proxies"]; !kept {
		t.Fatalf("upstream.proxies 丢失: %#v", upstream)
	}

	if merged["listen"] != ":9000" {
		t.Errorf("顶层标量应照常覆盖，listen=%v", merged["listen"])
	}
}

// TestRestoreMaskedProxySecrets 面板回显的掩码项在保存时还原成原文，
// 否则一次「保存配置」就把所有代理口令与订阅 token 抹掉。
func TestRestoreMaskedProxySecrets(t *testing.T) {
	t.Parallel()

	current := map[string]any{
		"upstream": map[string]any{
			"proxies":             []any{"socks5://alice:s3cret@10.0.0.1:1080"},
			"proxy_subscriptions": []any{"https://sub.example/api?token=tok"},
			"proxy_slots": []any{
				map[string]any{"id": "cn", "url": "http://bob:hunter2@10.0.0.2:8080"},
			},
		},
	}

	incoming := map[string]any{
		"upstream": map[string]any{
			"proxies":             []any{panel.MaskProxyEntry("socks5://alice:s3cret@10.0.0.1:1080")},
			"proxy_subscriptions": []any{panel.MaskProxySubURL("https://sub.example/api?token=tok")},
			"proxy_slots": []any{
				map[string]any{"id": "cn", "url": panel.MaskProxyEntry("http://bob:hunter2@10.0.0.2:8080")},
			},
		},
	}

	restoreMaskedProxySecrets(incoming, current)

	upstream := incoming["upstream"].(map[string]any)
	if got := upstream["proxies"].([]any)[0]; got != "socks5://alice:s3cret@10.0.0.1:1080" {
		t.Errorf("proxies 未还原: %v", got)
	}

	if got := upstream["proxy_subscriptions"].([]any)[0]; got != "https://sub.example/api?token=tok" {
		t.Errorf("订阅未还原: %v", got)
	}

	slot := upstream["proxy_slots"].([]any)[0].(map[string]any)
	if got := slot["url"]; got != "http://bob:hunter2@10.0.0.2:8080" {
		t.Errorf("槽 url 未还原: %v", got)
	}
}

// TestMaskProxyRoundTrip 回显口径与还原口径必须完全一致：面板回显用的
// panel.MaskProxyEntry 就是还原时比对的掩码，任何一侧单独改动都会让「没改动」
// 变成「写回掩码字面量」，代理凭据一次保存就被抹空。
func TestMaskProxyRoundTrip(t *testing.T) {
	t.Parallel()

	raws := []string{
		"socks5://alice:s3cret@10.0.0.1:1080",
		"1.2.3.4:1080:user:pass", // 四段式：url.Parse 报错，两侧都落到占位串
		"not a url",
		"http://",
	}

	for _, raw := range raws {
		current := map[string]any{"upstream": map[string]any{"proxies": []any{raw}}}
		incoming := map[string]any{"upstream": map[string]any{
			"proxies": []any{panel.MaskProxyEntry(raw)},
		}}

		restoreMaskedProxySecrets(incoming, current)

		got := incoming["upstream"].(map[string]any)["proxies"].([]any)[0]
		if got != raw {
			t.Errorf("raw=%q 还原成 %v（掩码口径不一致，字面量会被写进配置）", raw, got)
		}
	}
}
