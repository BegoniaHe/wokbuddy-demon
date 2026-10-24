package upstream

import "testing"

// TestApplyModelTiers 档位挂载：enabled=false 不挂、priority 最高者胜、
// 目录外不凭空造条目、requiredUserType 只透出不过滤。
//
//nolint:cyclop // 表驱动断言字段较多，拆开反而难读。
func TestApplyModelTiers(t *testing.T) {
	t.Parallel()

	out := map[string]ModelInfo{
		"glm-5.2": {ID: "glm-5.2"},
		"kimi":    {ID: "kimi"},
	}

	applyModelTiers(out, []v3ModelTier{
		{
			ID: "low", Enabled: new(false), ModelIDs: []string{"glm-5.2"}, Priority: 100,
			Tier: "standard", Badge: &ModelTierBadge{Label: "停用档"},
		},
		{
			ID: "std", Enabled: new(true), ModelIDs: []string{"glm-5.2"}, Priority: 10,
			Tier: "standard", Badge: &ModelTierBadge{Label: "标准"},
		},
		{
			ID: "pro", Enabled: nil, ModelIDs: []string{"glm-5.2"}, Priority: 20,
			Tier: "advanced", RequiredUserType: "advanced",
			Badge: &ModelTierBadge{Label: "进阶", ShortLabel: "进阶"},
			Hover: &ModelTierHover{TextZh: "升级后可用", Action: &ModelTierHoverAction{LabelZh: "升级"}},
		},
		{
			ID: "outside", Enabled: new(true), ModelIDs: []string{"not-in-catalog"}, Priority: 999,
			Tier: "flagship",
		},
	})

	glm := out["glm-5.2"]
	if glm.Tier == nil {
		t.Fatal("glm-5.2 tier missing")
	}

	if glm.Tier.Tier != "advanced" || glm.Tier.Priority != 20 {
		t.Fatalf("tier=%+v want priority 20 advanced", glm.Tier)
	}

	if glm.Tier.RequiredUserType != "advanced" {
		t.Fatalf("requiredUserType=%q", glm.Tier.RequiredUserType)
	}

	if glm.Tier.Badge == nil || glm.Tier.Badge.Label != "进阶" {
		t.Fatalf("badge=%+v", glm.Tier.Badge)
	}

	if glm.Tier.Hover == nil || glm.Tier.Hover.Action == nil || glm.Tier.Hover.Action.LabelZh != "升级" {
		t.Fatalf("hover=%+v", glm.Tier.Hover)
	}

	if out["kimi"].Tier != nil {
		t.Fatalf("kimi tier=%+v want nil", out["kimi"].Tier)
	}

	if _, ok := out["not-in-catalog"]; ok {
		t.Fatal("catalog-outside model must not be created")
	}
}

// TestApplyModelTiersEmptyNoChange 空/全禁用不改变目录。
func TestApplyModelTiersEmptyNoChange(t *testing.T) {
	t.Parallel()

	out := map[string]ModelInfo{"glm-5.2": {ID: "glm-5.2"}}
	applyModelTiers(out, nil)

	if out["glm-5.2"].Tier != nil {
		t.Fatal("nil tiers must not attach")
	}

	applyModelTiers(out, []v3ModelTier{{ID: "x", Enabled: new(false), ModelIDs: []string{"glm-5.2"}}})

	if out["glm-5.2"].Tier != nil {
		t.Fatal("disabled tier must not attach")
	}
}
