package main

import "testing"

func TestDecisionTraceConfig(t *testing.T) {
	t.Parallel()

	if c := Default(); c.Logging.DecisionTraceSample != 0.1 {
		t.Fatalf("默认采样率=%v want 0.1（与 README/config.example.json 对齐）", c.Logging.DecisionTraceSample)
	}

	config := Default()
	config.Logging.DecisionTrace = true

	config.Logging.DecisionTraceSample = 0.25
	if err := config.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}

	if !config.Logging.DecisionTrace || config.Logging.DecisionTraceSample != 0.25 {
		t.Fatalf("logging=%+v", config.Logging)
	}

	for _, bad := range []float64{-0.1, 1.1} {
		config = Default()

		config.Logging.DecisionTraceSample = bad
		if err := config.normalize(); err == nil {
			t.Errorf("sample=%v 应报错", bad)
		}
	}
}
