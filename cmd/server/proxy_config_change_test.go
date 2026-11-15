// proxy_config_change_test.go 钉住「代理出口只在相关字段变化时重建」：
// 重建会同步拉取全部代理订阅（每个最长 10s、串行），而热应用发生在配置已落盘
// 之后且在 configWriteMu 内——无关字段的保存不该被订阅网络卡住。
package main

import "testing"

func TestProxyConfigChangedOnlyProxyFields(t *testing.T) {
	t.Parallel()

	base := Default()

	// 无关字段变化不触发重建。
	unrelated := Default()
	unrelated.Schedule.CheckinHours = []int{8, 20}
	unrelated.Pool.MaxInFlight = base.Pool.MaxInFlight + 1

	if proxyConfigChanged(base, unrelated) {
		t.Fatal("非代理字段变化不应触发代理注册表重建")
	}

	cases := map[string]func(*Config){
		"proxies":             func(c *Config) { c.Upstream.Proxies = []string{"socks5://127.0.0.1:1080"} },
		"proxy_subscriptions": func(c *Config) { c.Upstream.ProxySubscriptions = []string{"https://example.com/sub"} },
		"proxy_slots": func(c *Config) {
			c.Upstream.ProxySlots = []ProxySlot{{ID: "a", Name: "A", URL: "socks5://127.0.0.1:1080", Enabled: true}}
		},
		"realm_proxy_slots": func(c *Config) {
			c.Upstream.RealmProxySlots = map[string]string{"cn": "a"}
		},
	}

	for name, mutate := range cases {
		changed := Default()
		mutate(changed)

		if !proxyConfigChanged(base, changed) {
			t.Fatalf("%s 变化必须触发代理注册表重建", name)
		}
	}
}

// TestProxyConfigChangedNilBaseline 基线不可用（配置损坏）时保守重建。
func TestProxyConfigChangedNilBaseline(t *testing.T) {
	t.Parallel()

	if !proxyConfigChanged(nil, Default()) {
		t.Fatal("nil 基线应触发重建")
	}
}
