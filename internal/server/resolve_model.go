package server

import "strings"

// splitRealm 解析模型名的 "[realm:]model" 前缀，并报告是否显式带前缀。
// 取第一个 ":"，前段恰为 "cn"/"global" 才剥离；否则视为裸名，realm=cn。
// 大小写敏感（前缀必须是精确的小写枚举）。
func splitRealm(model string) (realm, bare string, prefixed bool) {
	idx := strings.IndexByte(model, ':')
	if idx < 0 {
		return "cn", model, false
	}
	prefix := model[:idx]
	if prefix != "cn" && prefix != "global" {
		return "cn", model, false
	}
	return prefix, model[idx+1:], true
}

// resolveModel 解析模型名协议（PLAN D6）：
//
//	分布式前缀： "[realm:]model"
//
// 导出为 ResolveModel（cmd/server/main.go 粘性闭包需要），包内简写 resolveModel。
func resolveModel(model string) (realm, bare string) {
	realm, bare, _ = splitRealm(model)
	return realm, bare
}

// ResolveModel 是 resolveModel 的导出面（跨包调用）。
func ResolveModel(model string) (realm, bare string) { return resolveModel(model) }

// resolveRequestModel 在 realm 前缀之上应用模型别名（issue #52）：
// 先剥请求的 realm 前缀，再按裸名查别名；别名目标的 realm 显式给出则以其为准，
// 省略时继承请求侧 realm（裸请求即 cn）。未命中别名 → 与 resolveModel 完全一致。
func resolveRequestModel(model string, aliases map[string]string) (realm, bare string) {
	realm, bare = resolveModel(model)
	target, ok := aliases[bare]
	if !ok {
		return realm, bare
	}
	targetRealm, targetBare, prefixed := splitRealm(target)
	if !prefixed {
		targetRealm = realm
	}
	// 目标裸名在配置校验阶段已保证非空；这里再兜一次，避免手工构造的
	// Config（测试/裸用）把空模型名透传到上游。
	if targetBare == "" {
		return realm, bare
	}
	return targetRealm, targetBare
}
