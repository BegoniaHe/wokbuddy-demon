package main

import "testing"

func TestDecisionTraceConfig(t *testing.T) {
	c := Default()
	c.Logging.DecisionTrace = true
	c.Logging.DecisionTraceSample = 0.25
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if !c.Logging.DecisionTrace || c.Logging.DecisionTraceSample != 0.25 {
		t.Fatalf("logging=%+v", c.Logging)
	}

	for _, bad := range []float64{-0.1, 1.1} {
		c = Default()
		c.Logging.DecisionTraceSample = bad
		if err := c.normalize(); err == nil {
			t.Errorf("sample=%v 应报错", bad)
		}
	}
}
