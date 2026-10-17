package main

import (
	"testing"
	"time"
)

func TestKeepaliveConfig(t *testing.T) {
	c := Default()
	c.Upstream.KeepaliveInterval = "15s"
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.KeepaliveIntervalDur != 15*time.Second {
		t.Fatalf("keepalive=%v want 15s", c.KeepaliveIntervalDur)
	}

	c = Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("default normalize: %v", err)
	}
	if c.Upstream.KeepaliveInterval != "0" || c.KeepaliveIntervalDur != 0 {
		t.Fatalf("default keepalive=%q dur=%v want 0", c.Upstream.KeepaliveInterval, c.KeepaliveIntervalDur)
	}
}

func TestKeepaliveConfigRejectsInvalid(t *testing.T) {
	for _, v := range []string{"abc", "-5s"} {
		c := Default()
		c.Upstream.KeepaliveInterval = v
		if err := c.normalize(); err == nil {
			t.Errorf("%q: normalize 应报错", v)
		}
	}
}
