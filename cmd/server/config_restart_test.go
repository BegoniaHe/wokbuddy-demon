package main

import (
	"slices"
	"testing"
)

func TestRestartRequiredFieldsHotChangeEmpty(t *testing.T) {
	t.Parallel()

	oldCfg := Default()
	newCfg := Default()
	newCfg.Pool.CreditFloor = 100

	newCfg.Schedule.CheckinHours = []int{1, 2}
	if got := restartRequiredFields(oldCfg, newCfg); len(got) != 0 {
		t.Fatalf("hot-only changes reported restart: %v", got)
	}
}

func TestRestartRequiredFieldsSameConfig(t *testing.T) {
	t.Parallel()

	if got := restartRequiredFields(Default(), Default()); len(got) != 0 {
		t.Fatalf("identical config reported restart: %v", got)
	}
}

func TestRestartRequiredFieldsAssemblyChanges(t *testing.T) {
	t.Parallel()

	cases := []struct {
		field  string
		mutate func(*Config)
	}{
		{"listen", func(c *Config) { c.Listen = ":9000" }},
		{"auth_dir", func(c *Config) { c.AuthDir = "./other-auths" }},
		{"state_file", func(c *Config) { c.StateFile = "./data/other.json" }},
		{"server.read_timeout", func(c *Config) { c.Server.ReadTimeout = "60s" }},
		{"server.cors_origins", func(c *Config) { c.Server.CORSOrigins = []string{"https://example.test"} }},
		{"server.trusted_proxies", func(c *Config) { c.Server.TrustedProxies = []string{"10.0.0.0/8"} }},
		{"pricing.enabled", func(c *Config) { c.Pricing.Enabled = !c.Pricing.Enabled }},
		{"pricing.refresh_minutes", func(c *Config) { c.Pricing.RefreshMinutes++ }},
		{"pricing.exchange_rate", func(c *Config) { c.Pricing.ExchangeRate += 0.1 }},
		{"upstream.proxies", func(c *Config) { c.Upstream.Proxies = []string{"http://127.0.0.1:8080"} }},
		{"global.chat_base", func(c *Config) { c.Global.ChatBase = "https://example.test" }},
		{"prompt.mode", func(c *Config) { c.Prompt.Mode = "append" }},
		{"session_sticky.enabled", func(c *Config) { c.SessionSticky.Enabled = false }},
		{"upstream.client_version", func(c *Config) { c.Upstream.ClientVersion = "9.9.9" }},
		{"upstream.device_token", func(c *Config) { c.Upstream.DeviceToken = "tok" }},
		{"upstream.passthrough_ip", func(c *Config) { c.Upstream.PassthroughIP = true }},
	}
	for _, testCase := range cases {
		t.Run(testCase.field, func(t *testing.T) {
			t.Parallel()

			oldCfg := Default()
			newCfg := Default()
			testCase.mutate(newCfg)

			got := restartRequiredFields(oldCfg, newCfg)
			if !slices.Contains(got, testCase.field) {
				t.Fatalf("restart fields %v missing %q", got, testCase.field)
			}
		})
	}
}
