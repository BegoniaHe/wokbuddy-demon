package main

import (
	"strings"
	"testing"
)

// TestValidateRejectsInvalidConfig 钉住保存/加载前的显式校验：非法 / 自相矛盾配置
// 返回可读错误、不落盘；合法配置（含 Default）不受影响。
func TestValidateRejectsInvalidConfig(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr string // 非空 = 期望错误且错误含该子串
	}{
		{"default-ok", `{}`, ""},
		{"soft-rate-max-lt-base", `{"cooldown":{"soft_rate":"1h","soft_rate_max":"30m"}}`, "soft_rate_max"},
		{"breaker-max-lt-base", `{"pool":{"breaker_cooldown":"2h","breaker_cooldown_max":"30m"}}`, "breaker_cooldown_max"},
		{"degrade-max-lt-base", `{"pool":{"degrade_cooldown":"1h","degrade_cooldown_max":"10m"}}`, "degrade_cooldown_max"},
		{"negative-max-in-flight", `{"pool":{"max_in_flight":-1}}`, "max_in_flight"},
		{"negative-session-ttl", `{"session_sticky":{"ttl":"-5m"}}`, "session_sticky.ttl"},
		{"negative-gc-interval", `{"session_sticky":{"gc_interval":"-5m"}}`, "session_sticky.gc_interval"},
		{"global-disabled-with-base", `{"global":{"enabled":false,"chat_base":"https://x.example"}}`, "global.enabled"},
		{"global-disabled-no-base", `{"global":{"enabled":false}}`, ""},
		{"max-in-flight-zero-ok", `{"pool":{"max_in_flight":0}}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseConfig([]byte(c.raw))
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("want ok, got err=%v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want error containing %q, got nil", c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("err=%q want contains %q", err.Error(), c.wantErr)
			}
		})
	}
}

// TestDefaultConfigValidates Default() 归一化后必须通过校验（推荐配置自洽）。
func TestDefaultConfigValidates(t *testing.T) {
	if err := Default().normalize(); err != nil {
		t.Fatalf("default config must validate: %v", err)
	}
}
