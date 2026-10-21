package main

import (
	"slices"
	"testing"
)

// TestDNSServersParse dns_servers 解析：去空白、去重、丢空项。
func TestDNSServersParse(t *testing.T) {
	t.Parallel()

	cfg, err := ParseConfig([]byte(`{
		"listen": ":7863",
		"dns_servers": [" 1.1.1.1 ", "223.5.5.5:5353", "1.1.1.1", ""]
	}`))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}

	want := []string{"1.1.1.1", "223.5.5.5:5353"}
	if !slices.Equal(cfg.DNSServers, want) {
		t.Fatalf("dns_servers=%v want %v", cfg.DNSServers, want)
	}
}

// TestDNSServersRejectsInvalid 非法写法（scheme / 路径 / 坏端口）拒绝启动。
func TestDNSServersRejectsInvalid(t *testing.T) {
	t.Parallel()

	for _, bad := range []string{
		"https://1.1.1.1",
		"1.1.1.1/dns-query",
		"1.1.1.1:0",
		"1.1.1.1:70000",
		"1.1.1.1:abc",
	} {
		if _, err := ParseConfig([]byte(`{"listen": ":7863", "dns_servers": ["` + bad + `"]}`)); err == nil {
			t.Errorf("dns_servers[%q] 应被拒绝", bad)
		}
	}
}

// TestDNSServersRestartRequired 出站解析器属装配期字段：改动必须提示重启。
func TestDNSServersRestartRequired(t *testing.T) {
	t.Parallel()

	oldCfg := Default()
	newCfg := Default()
	newCfg.DNSServers = []string{"1.1.1.1"}

	if got := restartRequiredFields(oldCfg, newCfg); !slices.Contains(got, "dns_servers") {
		t.Fatalf("dns_servers 改动必须在需重启清单，got %v", got)
	}
}

// TestDNSServersEnvOverride WB2A_DNS（逗号分隔）覆盖配置文件。
func TestDNSServersEnvOverride(t *testing.T) {
	// 不改 t.Parallel：环境变量是进程级状态。
	t.Setenv("WB2A_DNS", "9.9.9.9, 1.1.1.1 ")

	// parseConfigWithEnv 与 Load 同序（JSON → env → normalize）。
	cfg, err := parseConfigWithEnv([]byte(`{"listen": ":7863", "dns_servers": ["223.5.5.5"]}`))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}

	want := []string{"9.9.9.9", "1.1.1.1"}
	if !slices.Equal(cfg.DNSServers, want) {
		t.Fatalf("dns_servers=%v want %v（环境变量应覆盖配置）", cfg.DNSServers, want)
	}
}
