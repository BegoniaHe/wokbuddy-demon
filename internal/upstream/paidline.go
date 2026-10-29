// paidline.go 官方「付费线路」隐藏规则（issue #402）。
//
// 官方客户端菜单对模型有两条独立的可见性规则：#319 已做「默认会话 agent 的 models
// 白名单」，这里补第二条——`productFeaturesConfig.ModelRateLimitCap.lines[]` 里的
// `paidId`（付费线路变体）不单独成行：它并进 `freeId` 那一行，免费额度撞顶时客户端
// 才切过去。
//
// 隐藏只发生在渲染层（`filterVisiblePaidLineModels`），目录层反而由
// `injectPaidLineModels` 把 `freeId` / `paidId` 补进 `agents[].models`——所以 #319 的
// 白名单会把这些付费变体一并列出来，需要这一步收尾。
//
// 语义照抄官方：
//   - eligible = `account.type ∈ {personal, pro}` 且 `enterpriseId` 不在排除表；
//   - 只有 eligible 且 lines 非空才考虑启用，且必须**至少一项 paidId 命中当前目录**
//     才整体过滤（官方 `e.some(...) ? filter : slice`）；
//   - 只隐藏命中的 paidId，`freeId` 一律不动；
//   - 上游不下发 `ModelRateLimitCap` 时完全不动（不内置官方 `UR` 缺省行，不替上游
//     做产品决策）。
//
// 只写 `ModelInfo.ChatVisible`：不影响路由、面板全量目录与 effort 能力缓存。
package upstream

import (
	"log"
	"strings"
	"sync"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// v3ModelRateLimitCap /v3/config data.productFeaturesConfig.ModelRateLimitCap。
//
// 官方两种读取形态（`productFeaturesConfig[Wa.ModelRateLimitCap]` 与
// `productFeaturesConfig.ModelRateLimitCap`）的键名是同一个字符串，且 encoding/json
// 的字段名匹配本身大小写不敏感，一个 tag 即可覆盖，不需要额外兜底解析。
type v3ModelRateLimitCap struct {
	Lines []v3PaidLine `json:"lines"`
}

// v3PaidLine 单条付费线路：官方 `parseLine` 要求 freeId / paidId 都非空才收。
type v3PaidLine struct {
	FreeID string `json:"freeId"`
	PaidID string `json:"paidId"`
}

// paidLineExclusionNoticeOnce 排除表缺失的提示只打一次，避免每次拉目录刷日志。
var paidLineExclusionNoticeOnce sync.Once

// isEligiblePaidLineUser 对齐官方 `isEligiblePaidLineUser`：
// `type ∈ {personal, pro}` 且 `enterpriseId` 不在排除表。
//
// 官方排除表（`yae`）是压缩包里的常量数组，本仓无处可依；取不到就按「不排除」处理
// 并记一次日志——自行发明名单会把合法个人号静默挡在规则外，比多隐藏几个变体更糟。
func isEligiblePaidLineUser(account *auth.Auth) bool {
	if account == nil {
		return false
	}

	switch strings.ToLower(strings.TrimSpace(account.EditionType)) {
	case "personal", "pro":
	default:
		return false
	}

	paidLineExclusionNoticeOnce.Do(func() {
		log.Printf("INFO: [upstream] paid-line filter: official enterpriseId exclusion list " +
			"unavailable, treating every personal/pro account as eligible (issue #402)")
	})

	return true
}

// applyPaidLineFilter 按官方 `filterVisiblePaidLineModels` 隐藏命中的 paidId 变体。
//
// 只要目录里有任何一项 paidId 命中就整体启用；一项都不命中时不做任何隐藏（官方同样
// 是 `some` 命中才 filter），避免上游换了模型集合后规则空转出意外结果。
func applyPaidLineFilter(out map[string]ModelInfo, account *auth.Auth, rateCap v3ModelRateLimitCap) {
	if len(out) == 0 || len(rateCap.Lines) == 0 || !isEligiblePaidLineUser(account) {
		return
	}

	paidIDs := paidLineIDSet(rateCap)

	// 官方「有一项 paidId 命中才整体启用」：一项都不命中时不做任何隐藏，避免上游
	// 换了模型集合后规则空转出意外结果。
	if !anyModelIDIn(out, paidIDs) {
		return
	}

	hidden := hidePaidLineModels(out, paidIDs)

	log.Printf("INFO: [upstream] paid-line filter: realm=%s hidden=%d", account.Realm(), hidden)
}

// paidLineIDSet 收集 lines[] 里非空的 paidId（缺 paidId 的 line 官方 parseLine 直接丢弃）。
func paidLineIDSet(rateCap v3ModelRateLimitCap) map[string]bool {
	paidIDs := make(map[string]bool, len(rateCap.Lines))
	for _, line := range rateCap.Lines {
		if id := strings.TrimSpace(line.PaidID); id != "" {
			paidIDs[id] = true
		}
	}

	return paidIDs
}

// anyModelIDIn 报告目录里是否至少有一个 id 命中集合。
func anyModelIDIn(out map[string]ModelInfo, ids map[string]bool) bool {
	for id := range out {
		if ids[id] {
			return true
		}
	}

	return false
}

// hidePaidLineModels 把命中的 paidId 打成不可见，返回隐藏条数（freeId 不动）。
func hidePaidLineModels(out map[string]ModelInfo, paidIDs map[string]bool) int {
	hidden := 0

	for id := range out {
		if !paidIDs[id] {
			continue
		}

		info := out[id]
		visible := false
		info.ChatVisible = &visible
		out[id] = info
		hidden++
	}

	return hidden
}
