package upstream

// ModelTier 官方模型档位（/v3/config data.modelTiers 的展示子集）。
// 字段名与官方响应保持一致，便于 /v1/models 与面板直接透出。
type ModelTier struct {
	Enabled          bool            `json:"enabled"`
	Tier             string          `json:"tier,omitempty"`
	Priority         int             `json:"priority,omitempty"`
	RequiredUserType string          `json:"requiredUserType,omitempty"`
	Badge            *ModelTierBadge `json:"badge,omitempty"`
	Hover            *ModelTierHover `json:"hover,omitempty"`
}

// ModelTierBadge 档位徽章文案与配色（官方字段原样）。
type ModelTierBadge struct {
	Label        string `json:"label,omitempty"`
	ShortLabel   string `json:"shortLabel,omitempty"`
	LabelEn      string `json:"labelEn,omitempty"`
	ShortLabelEn string `json:"shortLabelEn,omitempty"`
	Color        string `json:"color,omitempty"`
}

// ModelTierHover 档位悬停说明与升级动作。
type ModelTierHover struct {
	TextZh string                `json:"textZh,omitempty"`
	TextEn string                `json:"textEn,omitempty"`
	Action *ModelTierHoverAction `json:"action,omitempty"`
}

// ModelTierHoverAction 升级提示按钮文案（官方字段原样）。
type ModelTierHoverAction struct {
	LabelZh string `json:"labelZh,omitempty"`
	LabelEn string `json:"labelEn,omitempty"`
}
