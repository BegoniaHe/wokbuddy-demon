package main

import "testing"

// TestProxyConfig 出站代理配置（issue #15）：缺省为空（直连）；合法 URL 通过；
// 非法 URL 在解析阶段拒绝（不落盘）。
func TestProxyConfig(t *testing.T) {
	c, err := ParseConfig([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Upstream.Proxies) != 0 {
		t.Errorf("default proxies=%v want empty", c.Upstream.Proxies)
	}

	c, err = ParseConfig([]byte(`{"upstream":{"proxies":["http://u:p@127.0.0.1:8080","socks5://127.0.0.1:1080"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Upstream.Proxies) != 2 {
		t.Errorf("proxies=%v want 2", c.Upstream.Proxies)
	}

	if _, err := ParseConfig([]byte(`{"upstream":{"proxies":["ftp://h:21"]}}`)); err == nil {
		t.Error("unsupported proxy scheme must be rejected")
	}
}
