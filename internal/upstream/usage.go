package upstream

import (
	"encoding/json"
	"maps"
	"math"
)

// normalizeUsageCacheAliases keeps cache-hit aliases consistent before the
// response leaves the gateway. Some WorkBuddy responses carry the real hit in
// prompt_tokens_details.cached_tokens while also emitting
// cache_read_input_tokens: 0 and cached_tokens: 0 compatibility aliases.
// Strict downstream parsers may prefer those zero aliases and lose the hit.
func normalizeUsageCacheAliases(usage map[string]any) map[string]any {
	best, ok := bestUsageCacheHitTokens(usage)
	if !ok || best <= 0 {
		return usage
	}

	out := cloneUsageMap(usage)
	out["cache_read_input_tokens"] = best
	out["cached_tokens"] = best
	out["prompt_cache_hit_tokens"] = best

	promptDetails := cloneUsageDetails(out, "prompt_tokens_details")
	promptDetails["cached_tokens"] = best
	out["prompt_tokens_details"] = promptDetails

	// Responses API consumers use this nested form. Preserve it when the
	// upstream already supplies it, but do not invent it for Chat-only clients.
	if _, exists := out["input_tokens_details"]; exists {
		inputDetails := cloneUsageDetails(out, "input_tokens_details")
		inputDetails["cached_tokens"] = best
		out["input_tokens_details"] = inputDetails
	}

	return out
}

func bestUsageCacheHitTokens(usage map[string]any) (float64, bool) {
	paths := []struct {
		section string
		key     string
	}{
		{"prompt_tokens_details", "cached_tokens"},
		{"", "prompt_cache_hit_tokens"},
		{"", "cache_read_input_tokens"},
		{"", "cached_tokens"},
		{"input_tokens_details", "cached_tokens"},
	}

	for _, path := range paths {
		var value any
		if path.section == "" {
			value = usage[path.key]
		} else if details, ok := usage[path.section].(map[string]any); ok {
			value = details[path.key]
		}

		if tokens, ok := positiveUsageNumber(value); ok {
			return tokens, true
		}
	}

	return 0, false
}

//nolint:cyclop,gocyclo // 上游数值可能是任意 JSON 数值类型，集中分支比反射更直白。
func positiveUsageNumber(value any) (float64, bool) {
	switch n := value.(type) {
	case json.Number:
		// 聚合帧解码走 UseNumber，usage 里的数字是 json.Number，只认 float64
		// 会让缓存命中观测恒为 0。
		parsed, err := n.Float64()

		return parsed, err == nil && parsed > 0
	case float64:
		return n, n > 0
	case float32:
		value := float64(n)
		return value, value > 0
	case int:
		return float64(n), n > 0
	case int64:
		return float64(n), n > 0
	case int32:
		return float64(n), n > 0
	case uint:
		return float64(n), n > 0
	case uint64:
		return float64(n), n > 0
	case uint32:
		return float64(n), n > 0
	default:
		return 0, false
	}
}

func cloneUsageMap(usage map[string]any) map[string]any {
	out := make(map[string]any, len(usage))
	maps.Copy(out, usage)

	return out
}

func cloneUsageDetails(usage map[string]any, key string) map[string]any {
	out := make(map[string]any)
	details, _ := usage[key].(map[string]any)
	maps.Copy(out, details)

	return out
}

// UsageCacheHitTokens 返回 usage 里的缓存命中 token 数（多别名取最优，口径与
// 回写给客户端的 normalizeUsageCacheAliases 一致）。供网关统计层（usage 桶 /
// reqlog）观测命中率使用；usage 缺失该维度时 ok=false。
func UsageCacheHitTokens(usage map[string]any) (float64, bool) {
	if usage == nil {
		return 0, false
	}

	return bestUsageCacheHitTokens(usage)
}

// UsageCacheMissTokens 返回 usage 里的缓存未命中 token 数：优先读上游显式的
// prompt_cache_miss_tokens，缺失时按 prompt_tokens - 命中 推导（推导值为负时
// 视为不可信，返回 ok=false）。
func UsageCacheMissTokens(usage map[string]any) (float64, bool) {
	if usage == nil {
		return 0, false
	}

	// 显式 miss 必须是正数才采信：上游常把该字段置 0 表示「未统计/未知」，
	// 采信 0 会让 hit/(hit+miss) 的命中率恒为 100%。hit 侧早已只取正数，这里对齐。
	if miss, ok := positiveUsageNumber(usage["prompt_cache_miss_tokens"]); ok {
		return miss, true
	}

	prompt, okP := UsageNumber(usage, "prompt_tokens")

	hit, okH := UsageCacheHitTokens(usage)
	if okP && okH && prompt-hit >= 0 {
		return prompt - hit, true
	}

	return 0, false
}

// UsagePromptTokens 返回 usage 里的 prompt token 总数（多别名取首个可用值）。
// 供网关统计层做缓存命中的**对称推导**：上游有时只回显式 miss、把 hit 留空/置 0，
// 此时需要 prompt 总数才能反推 hit。
func UsagePromptTokens(usage map[string]any) (float64, bool) {
	if usage == nil {
		return 0, false
	}

	for _, key := range []string{"prompt_tokens", "input_tokens"} {
		if value, ok := UsageNumber(usage, key); ok {
			return value, true
		}
	}

	return 0, false
}

// UsageLeafFloat 取任意 usage 叶子的有限数值（float64 / int / int64 / json.Number）。
// 非数值或 NaN 返回 false。
func UsageLeafFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, !math.IsNaN(n)
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()

		return f, err == nil && !math.IsNaN(f)
	default:
		return 0, false
	}
}

// UsageSumLeaves 两个 usage 数值叶子相加；任一不是有限数值、或和溢出返回 false
// （调用方直接覆盖，不写不可信结果）。
func UsageSumLeaves(dst, src any) (float64, bool) {
	srcNum, ok := UsageLeafFloat(src)
	if !ok {
		return 0, false
	}

	dstNum, ok := UsageLeafFloat(dst)
	if !ok {
		return 0, false
	}

	sum := dstNum + srcNum
	if math.IsInf(sum, 0) || math.IsNaN(sum) {
		return 0, false
	}

	return sum, true
}

// UsageNumber 从 usage 顶层取数值字段（JSON 数字可能是 float64 / json.Number 形态）。
//
// 网关的聚合路径改用 UseNumber 解码 SSE 帧后，usage 里的数字是 json.Number：
// 直接断言 float64 会静默失败（token 数变 0、total 不再重算）。所有读 usage 数值
// 的地方都走这里，两形态同口径。
func UsageNumber(usage map[string]any, key string) (float64, bool) {
	v, ok := usage[key]
	if !ok {
		return 0, false
	}

	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}

	return 0, false
}
