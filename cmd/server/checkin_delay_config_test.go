package main

import (
	"testing"
	"time"
)

func TestCheckinAccountDelayConfig(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("default: %v", err)
	}
	if c.CheckinAccountDelayDur != 800*time.Millisecond {
		t.Fatalf("default delay=%v want 800ms", c.CheckinAccountDelayDur)
	}

	c = Default()
	c.Schedule.CheckinAccountDelay = "0"
	if err := c.normalize(); err != nil {
		t.Fatalf("zero: %v", err)
	}
	if c.CheckinAccountDelayDur != 0 {
		t.Fatalf("zero delay=%v", c.CheckinAccountDelayDur)
	}

	for _, bad := range []string{"abc", "-1s"} {
		c = Default()
		c.Schedule.CheckinAccountDelay = bad
		if err := c.normalize(); err == nil {
			t.Errorf("%q: normalize 应报错", bad)
		}
	}
}
