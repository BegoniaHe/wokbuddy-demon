package main

import (
	"testing"
	"time"
)

func TestCheckinAccountDelayConfig(t *testing.T) {
	t.Parallel()

	config := Default()
	if err := config.normalize(); err != nil {
		t.Fatalf("default: %v", err)
	}

	if config.CheckinAccountDelayDur != 800*time.Millisecond {
		t.Fatalf("default delay=%v want 800ms", config.CheckinAccountDelayDur)
	}

	config = Default()

	config.Schedule.CheckinAccountDelay = "0"
	if err := config.normalize(); err != nil {
		t.Fatalf("zero: %v", err)
	}

	if config.CheckinAccountDelayDur != 0 {
		t.Fatalf("zero delay=%v", config.CheckinAccountDelayDur)
	}

	for _, bad := range []string{"abc", "-1s"} {
		config = Default()

		config.Schedule.CheckinAccountDelay = bad
		if err := config.normalize(); err == nil {
			t.Errorf("%q: normalize 应报错", bad)
		}
	}
}
