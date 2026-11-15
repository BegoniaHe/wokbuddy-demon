package upstream

// ModelTier 官方模型档位（/v3/config data.modelTiers 的展示子集）。
// 字段名与官方响应保持一致，便于 /v1/models 与面板直接透出。
type ModelTier struct {
	Enabled          bool            `json:"enabled"`
	Tier             string          `json:"tier,omitempty,omitzero"`
	Priority         int             `json:"priority,omitempty,omitzero"`
	RequiredUserType string          `json:"requiredUserType,omitempty,omitzero"`
	Badge            *ModelTierBadge `json:"badge,omitempty,omitzero"`
	Hover            *ModelTierHover `json:"hover,omitempty,omitzero"`
}

// ModelTierBadge 档位徽章文案与配色（官方字段原样）。
type ModelTierBadge struct {
	Label        string `json:"label,omitempty,omitzero"`
	ShortLabel   string `json:"shortLabel,omitempty,omitzero"`
	LabelEn      string `json:"labelEn,omitempty,omitzero"`
	ShortLabelEn string `json:"shortLabelEn,omitempty,omitzero"`
	Color        string `json:"color,omitempty,omitzero"`
}

// ModelTierHover 档位悬停说明与升级动作。
type ModelTierHover struct {
	TextZh string                `json:"textZh,omitempty,omitzero"`
	TextEn string                `json:"textEn,omitempty,omitzero"`
	Action *ModelTierHoverAction `json:"action,omitempty,omitzero"`
}

// ModelTierHoverAction 升级提示按钮文案（官方字段原样）。
type ModelTierHoverAction struct {
	LabelZh string `json:"labelZh,omitempty,omitzero"`
	LabelEn string `json:"labelEn,omitempty,omitzero"`
}
