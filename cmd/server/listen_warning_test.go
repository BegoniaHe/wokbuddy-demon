package main

import (
	"strings"
	"testing"
)

// TestWeakAPIKeyListenWarning 弱 key（空 / test_key）叠加非回环监听才告警；
// 强 key 或仅回环监听不告警。文案不得包含 key 值。
func TestWeakAPIKeyListenWarning(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		apiKey string
		listen string
		want   bool
	}{
		{"空 key 绑定全部网卡", "", ":7863", true},
		{"空 key 指定 0.0.0.0", "", "0.0.0.0:7863", true},
		{"空 key IPv6 全部网卡", "", "[::]:7863", true},
		{"空 key 内网地址", "", "192.168.1.10:7863", true},
		{"占位 key 绑定全部网卡", placeholderAPIKey, ":7863", true},
		{"占位 key 内网地址", placeholderAPIKey, "192.168.1.10:7863", true},
		{"空 key 回环 v4", "", "127.0.0.1:7863", false},
		{"空 key 回环 v6", "", "[::1]:7863", false},
		{"空 key localhost", "", "localhost:7863", false},
		{"占位 key 回环", placeholderAPIKey, "127.0.0.1:7863", false},
		{"强 key 绑定全部网卡", "sk-strong-random", ":7863", false},
		{"强 key 内网地址", "sk-strong-random", "192.168.1.10:7863", false},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got := weakAPIKeyListenWarning(testCase.apiKey, testCase.listen)
			if testCase.want && got == "" {
				t.Fatalf("api_key 弱 + listen=%s 应告警", testCase.listen)
			}

			if !testCase.want && got != "" {
				t.Fatalf("不应告警: %s", got)
			}

			if testCase.apiKey != "" && got != "" && strings.Contains(got, testCase.apiKey) {
				t.Fatalf("告警不得包含 key 值: %s", got)
			}
		})
	}
}
