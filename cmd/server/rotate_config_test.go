package main

import (
	"testing"
	"time"
)

// TestRotateConfigDefaults 缺省：次数 3、预算 0（不限）、状态码空（按分类换号）。
func TestRotateConfigDefaults(t *testing.T) {
	c, err := ParseConfig([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstream.RotateMax != 3 {
		t.Errorf("rotate_max=%d want 3", c.Upstream.RotateMax)
	}
	if c.RotateBudgetDur != 0 {
		t.Errorf("rotate_budget dur=%v want 0", c.RotateBudgetDur)
	}
	if len(c.Upstream.RotateStatusCodes) != 0 {
		t.Errorf("rotate_status_codes=%v want empty", c.Upstream.RotateStatusCodes)
	}
}

// TestRotateConfigParsed 显式配置生效。
func TestRotateConfigParsed(t *testing.T) {
	c, err := ParseConfig([]byte(`{"upstream":{"rotate_max":5,"rotate_budget":"90s","rotate_status_codes":[429,503]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstream.RotateMax != 5 {
		t.Errorf("rotate_max=%d want 5", c.Upstream.RotateMax)
	}
	if c.RotateBudgetDur != 90*time.Second {
		t.Errorf("rotate_budget dur=%v want 90s", c.RotateBudgetDur)
	}
	if len(c.Upstream.RotateStatusCodes) != 2 {
		t.Errorf("rotate_status_codes=%v want 2 items", c.Upstream.RotateStatusCodes)
	}
}

// TestRotateConfigInvalid 非法预算报错（不落盘）。
func TestRotateConfigInvalid(t *testing.T) {
	for _, raw := range []string{
		`{"upstream":{"rotate_budget":"oops"}}`,
		`{"upstream":{"rotate_budget":"-5s"}}`,
	} {
		if _, err := ParseConfig([]byte(raw)); err == nil {
			t.Errorf("raw=%s want error", raw)
		}
	}
}
