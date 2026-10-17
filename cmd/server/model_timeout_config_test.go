package main

import (
	"testing"
	"time"
)

func TestModelTimeoutConfig(t *testing.T) {
	c := Default()
	c.Upstream.ModelTimeouts = map[string]ModelTimeout{
		" cn:glm-5.2 ": {Header: "300s", Idle: "600s"},
		"gpt-5.6-sol":  {Header: "120s"},
	}
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	spec := c.Upstream.ModelTimeouts["cn:glm-5.2"]
	if spec.HeaderDur != 300*time.Second || spec.IdleDur != 600*time.Second {
		t.Fatalf("spec=%+v", spec)
	}
}

func TestModelTimeoutConfigRejectsInvalid(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec ModelTimeout
	}{
		{"非法 header", ModelTimeout{Header: "abc"}},
		{"负 idle", ModelTimeout{Idle: "-1s"}},
		{"两项皆空", ModelTimeout{}},
	} {
		c := Default()
		c.Upstream.ModelTimeouts = map[string]ModelTimeout{"m": tc.spec}
		if err := c.normalize(); err == nil {
			t.Errorf("%s: normalize 应报错", tc.name)
		}
	}
}
