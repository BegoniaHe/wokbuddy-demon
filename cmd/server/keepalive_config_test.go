package main

import (
	"testing"
	"time"
)

func TestKeepaliveConfig(t *testing.T) {
	t.Parallel()

	config := Default()

	config.Upstream.KeepaliveInterval = "15s"
	if err := config.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}

	if config.KeepaliveIntervalDur != 15*time.Second {
		t.Fatalf("keepalive=%v want 15s", config.KeepaliveIntervalDur)
	}

	config = Default()
	if err := config.normalize(); err != nil {
		t.Fatalf("default normalize: %v", err)
	}

	if config.Upstream.KeepaliveInterval != "0" || config.KeepaliveIntervalDur != 0 {
		t.Fatalf("default keepalive=%q dur=%v want 0", config.Upstream.KeepaliveInterval, config.KeepaliveIntervalDur)
	}
}

func TestKeepaliveConfigRejectsInvalid(t *testing.T) {
	t.Parallel()

	for _, v := range []string{"abc", "-5s"} {
		c := Default()

		c.Upstream.KeepaliveInterval = v
		if err := c.normalize(); err == nil {
			t.Errorf("%q: normalize 应报错", v)
		}
	}
}
