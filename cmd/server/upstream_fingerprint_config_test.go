package main

import "testing"

// TestUpstreamFingerprintConfig 出站指纹两个开关的缺省值与取值域：
// header_order 缺省 true（显式 false 保留），tls_fingerprint 缺省 off，只接受 off/node。
func TestUpstreamFingerprintConfig(t *testing.T) {
	t.Parallel()

	defaults := Default()
	if defaults.Upstream.HeaderOrder == nil || !*defaults.Upstream.HeaderOrder {
		t.Error("默认 header_order 应为 true")
	}

	if got := defaults.Upstream.TLSFingerprint; got != upstreamTLSFingerprintOff {
		t.Errorf("默认 tls_fingerprint=%q want off", got)
	}
}

func TestUpstreamFingerprintConfigDefaults(t *testing.T) {
	t.Parallel()

	config, err := ParseConfig([]byte(`{}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if config.Upstream.HeaderOrder == nil || !*config.Upstream.HeaderOrder {
		t.Error("缺省 header_order 应为 true")
	}

	if got := config.Upstream.TLSFingerprint; got != upstreamTLSFingerprintOff {
		t.Errorf("缺省 tls_fingerprint=%q want off", got)
	}
}

func TestUpstreamFingerprintConfigExplicitFalse(t *testing.T) {
	t.Parallel()

	config, err := ParseConfig([]byte(`{"upstream":{"header_order":false}}`))
	if err != nil {
		t.Fatalf("parse false: %v", err)
	}

	if config.Upstream.HeaderOrder == nil || *config.Upstream.HeaderOrder {
		t.Error("显式 header_order=false 应保留")
	}
}

func TestUpstreamFingerprintConfigNode(t *testing.T) {
	t.Parallel()

	// node 在 config 层合法（profile 校验在启动装配期）。
	config, err := ParseConfig([]byte(`{"upstream":{"tls_fingerprint":"node"}}`))
	if err != nil {
		t.Fatalf("parse node: %v", err)
	}

	if got := config.Upstream.TLSFingerprint; got != upstreamTLSFingerprintNode {
		t.Errorf("tls_fingerprint=%q want node", got)
	}
}

func TestUpstreamFingerprintConfigRejectsUnknown(t *testing.T) {
	t.Parallel()

	if _, err := ParseConfig([]byte(`{"upstream":{"tls_fingerprint":"chrome"}}`)); err == nil {
		t.Fatal("未知 tls_fingerprint 应报错")
	}
}
