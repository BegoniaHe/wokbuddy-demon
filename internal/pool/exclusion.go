// exclusion.go 选号失败的成因拆解（issue #607）：池耗尽时按 pick 的排除顺序把
// 「为什么没号」归类，供 503 的 gateway_hint 与 WARN 日志使用。
//
// 选号本身不产日志也不产诊断（热路径零额外开销），成因只在失败现场补一次采集：
// 默认配置下运维看不到「积分保底挡了 20 个」还是「在途全满」，而两者的处置方式
// 完全不同（充值 / 等待 / 调并发）。
package pool

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// excludeReasonOrder 成因展示顺序：与选号的实际判定顺序一致——pickCandidates 先跳过
// tried，再由 excludeReason 按 realm → partition → 模型规则 → 模型冷却 → 冷却 →
// 积分保底 → 日限额 → 在途占满 归一类（先命中者归类，与选号口径同源）。
var excludeReasonOrder = []string{
	"tried", "route_cooldown", "realm", "partition",
	"model_rule", "model_cooldown", "cooling",
	"credit_floor", "daily_limit", "in_flight_full",
}

// ExclusionSummary 返回一次选号失败的成因计数（成因 → 账号数）。
//
// 只在选号失败时调用：持锁遍历池一次，判定与 pickCandidates/excludeReason 完全同源，
// 不额外要求调用方持有锁。池为空或全部账号可选时返回空 map（hint 保持原样）。
func (p *Pool) ExclusionSummary(
	tried map[string]bool, reqModel, realm string, media bool,
) map[string]int {
	if p == nil {
		return nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// 路由级避让先于账号级排除生效（与 pickTraceWith 的早退判定同源）。
	now := p.now()
	if p.routeCoolingLocked(realm, reqModel, now) {
		return map[string]int{"route_cooldown": 1}
	}

	out := make(map[string]int, 4)

	for uid, entry2 := range p.byUID {
		if tried[uid] {
			out["tried"]++

			continue
		}

		if reason := p.excludeReason(entry2, reqModel, realm, media, now); reason != "" {
			out[reason]++
		}
	}

	return out
}

// NoPickSummary 把成因计数格式化成 `no_pick{total=2 in_flight_full=2}`；空 map 返回空串。
// 已知成因按 excludeReasonOrder 输出（稳定顺序），未知成因按名字字典序追加在后。
func NoPickSummary(excluded map[string]int) string {
	if len(excluded) == 0 {
		return ""
	}

	total := 0

	for _, count := range excluded {
		total += count
	}

	var builder strings.Builder

	fmt.Fprintf(&builder, "no_pick{total=%d", total)

	for _, reason := range excludeReasonOrder {
		if count := excluded[reason]; count > 0 {
			fmt.Fprintf(&builder, " %s=%d", reason, count)
		}
	}

	for _, reason := range slices.Sorted(maps.Keys(excluded)) {
		if slices.Contains(excludeReasonOrder, reason) || excluded[reason] <= 0 {
			continue
		}

		fmt.Fprintf(&builder, " %s=%d", reason, excluded[reason])
	}

	builder.WriteByte('}')

	return builder.String()
}
