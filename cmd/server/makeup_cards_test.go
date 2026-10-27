package main

import (
	"strings"
	"testing"
)

// TestMakeupCardsPerRoundConfig 缺省 0；正数原样保留；负数启动报错且错误含字段名。
func TestMakeupCardsPerRoundConfig(t *testing.T) {
	t.Parallel()

	if got := Default().Schedule.MakeupCardsPerRound; got != 0 {
		t.Fatalf("默认 makeup_cards_per_round=%d want 0（不限）", got)
	}

	cfg, err := ParseConfig([]byte(`{"schedule":{"makeup_cards_per_round":3}}`))
	if err != nil {
		t.Fatalf("parse positive: %v", err)
	}

	if got := cfg.Schedule.MakeupCardsPerRound; got != 3 {
		t.Errorf("makeup_cards_per_round=%d want 3", got)
	}

	if _, err := ParseConfig([]byte(`{"schedule":{"makeup_cards_per_round":-1}}`)); err == nil {
		t.Fatal("负数应拒绝启动")
	} else if !strings.Contains(err.Error(), "makeup_cards_per_round") {
		t.Errorf("错误信息应含字段名，got %v", err)
	}
}
