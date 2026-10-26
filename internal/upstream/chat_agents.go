package upstream

import (
	"slices"
	"strings"
)

// modelAgent 上游模型目录的会话 agent 定义（/v3/config data.agents 与企业端点
// agents 同构）。白名单只来自上游响应，仓库不内置任何 agent 或模型清单。
type modelAgent struct {
	Name   string   `json:"name"`
	Models []string `json:"models"`
	Tags   []string `json:"tags"`
}

// modelChatWhitelist 一个来源的默认会话 agent 判定结果。
// decided=false 表示该来源没有可用 agent，不产生任何隐藏。
type modelChatWhitelist struct {
	decided     bool
	agentModels map[string]bool // 默认 agent 的 models 原文（可匹配 model.id 或 model.name）
}

// selectChatAgent 复刻官方 resolveConversationAgent 的选择顺序：
// tags 含 default → name=="cli" → 第一个 models 非空的 agent。
func selectChatAgent(agents []modelAgent) (modelAgent, bool) {
	usable := make([]modelAgent, 0, len(agents))
	for _, agent := range agents {
		if strings.TrimSpace(agent.Name) == "" {
			continue
		}

		usable = append(usable, agent)
	}

	for _, agent := range usable {
		if slices.Contains(agent.Tags, "default") {
			return agent, true
		}
	}

	for _, agent := range usable {
		if agent.Name == "cli" {
			return agent, true
		}
	}

	for _, agent := range usable {
		if len(agent.Models) > 0 {
			return agent, true
		}
	}

	return modelAgent{}, false
}

// newModelChatWhitelist 构造默认 agent 判定。没有可用 agent 或 agent models
// 为空时保持 undecided：此时不隐藏任何模型。
func newModelChatWhitelist(agents []modelAgent) modelChatWhitelist {
	agent, ok := selectChatAgent(agents)
	if !ok || len(agent.Models) == 0 {
		return modelChatWhitelist{}
	}

	whitelist := modelChatWhitelist{
		decided:     true,
		agentModels: make(map[string]bool, len(agent.Models)),
	}
	for _, id := range agent.Models {
		if id = strings.TrimSpace(id); id != "" {
			whitelist.agentModels[id] = true
		}
	}

	return whitelist
}

// modelMatchesAgent 官方按 model.id == agentModel 或 model.name == agentModel 命中。
func modelMatchesAgent(modelInfo *ModelInfo, agentModels map[string]bool) bool {
	return agentModels[modelInfo.ID] || agentModels[modelInfo.Name]
}

// isCustomModel 官方本地自定义模型：tags 含 custom，或 id 带 custom: /
// custom-local: 前缀。
func isCustomModel(modelInfo *ModelInfo) bool {
	if slices.Contains(modelInfo.Tags, "custom") {
		return true
	}

	id := strings.TrimSpace(modelInfo.ID)

	return strings.HasPrefix(id, "custom:") || strings.HasPrefix(id, "custom-local:")
}

// chatVisible 最终可见集合 = H ∪ custom ∪（仅 H 为空时）tags 含 chat 的模型。
// hNonEmpty 必须按目标模型集合计算（同一份白名单应用到不同来源时命中集不同）。
func (whitelist modelChatWhitelist) chatVisible(modelInfo *ModelInfo, hNonEmpty bool) bool {
	custom := isCustomModel(modelInfo)
	if hNonEmpty {
		return custom || modelMatchesAgent(modelInfo, whitelist.agentModels)
	}

	return custom || slices.Contains(modelInfo.Tags, "chat")
}

// apply 给尚无判定的条目写入 ChatVisible；已有显式判定不覆盖。
// H 是否为空按本次目标 infos 重新计算：企业端点白名单兜底 v3 时，两边目录可能
// 没有交集，不能沿用来源目录的命中结果。
func (whitelist modelChatWhitelist) apply(infos []ModelInfo) {
	if !whitelist.decided {
		return
	}

	hNonEmpty := false

	for i := range infos {
		if modelMatchesAgent(&infos[i], whitelist.agentModels) {
			hNonEmpty = true

			break
		}
	}

	for i := range infos {
		if infos[i].ChatVisible != nil {
			continue
		}

		visible := whitelist.chatVisible(&infos[i], hNonEmpty)
		infos[i].ChatVisible = &visible
	}
}

// applyChatWhitelistToMap 对按 id 建表的目录应用默认 agent 判定。
func applyChatWhitelistToMap(out map[string]ModelInfo, agents []modelAgent) {
	infos := make([]ModelInfo, 0, len(out))
	for id := range out {
		infos = append(infos, out[id])
	}

	whitelist := newModelChatWhitelist(agents)
	if !whitelist.decided {
		return
	}

	whitelist.apply(infos)

	for i := range infos {
		out[infos[i].ID] = infos[i]
	}
}

// fillChatVisible 同 id 合并时主来源判定优先；主来源 nil 时采用次来源判定。
func fillChatVisible(primary, secondary *ModelInfo) {
	if primary.ChatVisible == nil && secondary.ChatVisible != nil {
		primary.ChatVisible = secondary.ChatVisible
	}
}
