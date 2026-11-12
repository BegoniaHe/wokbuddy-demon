package main

import (
	"testing"
	"time"
)

// TestTTFBKeepaliveConfig 首帧等待期注释帧间隔（issue #500）：缺省 1500ms 开启，
// 显式 "0" 才关闭。
func TestTTFBKeepaliveConfig(t *testing.T) {
	t.Parallel()

	config := Default()
	if err := config.normalize(); err != nil {
		t.Fatalf("default normalize: %v", err)
	}

	if config.Upstream.TTFBKeepaliveInterval != "1500ms" {
		t.Errorf("default ttfb keepalive=%q want 1500ms", config.Upstream.TTFBKeepaliveInterval)
	}

	if config.TTFBKeepaliveIntervalDur != 1500*time.Millisecond {
		t.Errorf("default ttfb keepalive dur=%v want 1500ms", config.TTFBKeepaliveIntervalDur)
	}

	config = Default()
	config.Upstream.TTFBKeepaliveInterval = "0"

	if err := config.normalize(); err != nil {
		t.Fatalf("normalize 0: %v", err)
	}

	if config.TTFBKeepaliveIntervalDur != 0 {
		t.Errorf("ttfb keepalive dur=%v want 0（关闭）", config.TTFBKeepaliveIntervalDur)
	}
}

func TestTTFBKeepaliveConfigRejectsInvalid(t *testing.T) {
	t.Parallel()

	for _, v := range []string{"abc", "-5s"} {
		c := Default()

		c.Upstream.TTFBKeepaliveInterval = v
		if err := c.normalize(); err == nil {
			t.Errorf("%q: normalize 应报错", v)
		}
	}
}
