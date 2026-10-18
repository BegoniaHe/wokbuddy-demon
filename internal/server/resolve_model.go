package server

import (
	"errors"
	"fmt"
	"strings"
)

var errCheckKeyRealmBindingFailed = errors.New("checkKeyRealmBinding failed")

// splitRealm 解析模型名的 "[realm:]model" 前缀，并报告是否显式带前缀。
// 取第一个 ":"，前段恰为 "cn"/"global" 才剥离；否则视为裸名，realm=cn。
// 大小写敏感（前缀必须是精确的小写枚举）。
func splitRealm(model string) (realm, bare string, prefixed bool) {
	prefix, rest, ok := strings.Cut(model, ":")
	if !ok || (prefix != "cn" && prefix != "global") {
		return "cn", model, false
	}

	return prefix, rest, true
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

// resolveKeyModel 解析模型白名单校验所需的最终出口与裸模型名：先应用别名，
// 再按命名密钥出口绑定（issue #94）覆盖 realm。绑定与请求显式 realm 冲突时
// 返回错误，调用方按 400 处理。
func resolveKeyModel(model string, aliases map[string]string, keyRealm string) (realm, bare string, err error) {
	realm, bare = resolveRequestModel(model, aliases)
	if keyRealm == "" {
		return realm, bare, nil
	}

	if err := checkKeyRealmBinding(model, aliases, keyRealm); err != nil {
		return realm, bare, err
	}

	return keyRealm, bare, nil
}

// checkKeyRealmBinding 校验命名密钥出口绑定（issue #94）是否与请求的显式 realm
// 冲突。裸模型名不冲突：由 key 绑定决定出口；显式 "cn:"/"global:" 前缀或别名
// 目标自带的 realm 才视为客户端明确指定，与绑定不同即报 400。
func checkKeyRealmBinding(model string, aliases map[string]string, keyRealm string) error {
	realm, explicit := explicitModelRealm(model, aliases)
	if !explicit || realm == keyRealm {
		return nil
	}

	return fmt.Errorf("该 API key 绑定 %s 出口，但请求模型 %q 指定了 %s；请去掉 realm 前缀或改用匹配的 key: %w", keyRealm, model, realm, errCheckKeyRealmBindingFailed)
}

// explicitModelRealm 报告请求模型是否显式指定 realm（前缀或别名目标的显式前缀）。
func explicitModelRealm(model string, aliases map[string]string) (string, bool) {
	realm, bare, prefixed := splitRealm(model)
	if prefixed {
		return realm, true
	}

	if target, ok := aliases[bare]; ok {
		if targetRealm, _, targetPrefixed := splitRealm(target); targetPrefixed {
			return targetRealm, true
		}
	}

	return "", false
}
