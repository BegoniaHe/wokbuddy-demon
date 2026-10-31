// payload.go 改写发往上游的 chat 请求体：
//  1. 强制 stream:true（上游拒绝非流式）
//  2. tool_choice 归一化（上游该字段是 string，对象形式会 400 code=11101）
//  3. image_url 归一化（上游只认 OpenAI 对象形态，字符串会 400 code=11101）
package upstream

import (
	"encoding/json"
	"log"
	"math"
	"slices"
	"strings"
)

// chatBodyRewrite 一次出站 chat body 改写的全部输入。零值 = 只做基础规范化
// （不带任何 chat 出站专属步骤）。
type chatBodyRewrite struct {
	sanitize         bool
	efforts          map[string][]string
	defaultEfforts   map[string]string
	maxOut           map[string]int64
	historyKeepTurns int

	// 以下六项仅供 chat 出站（基础规范化不管，零值 = 与旧入口同口径）：
	leadingSystem  bool   // 首条非 system 时补官方 fallback system
	injectCacheKey bool   // 注入 prompt_cache_key（按 uid 隔离）
	uid            string // cache key 的账号隔离段
	conversationID string // cache key 的会话哈希源
	requestID      string // 非空 → 给当前轮最后一条 user 消息注入 conversationRequestId
	agentCLI       bool   // 除首条 system 外每条消息补 agent:"cli"（官方 chat body 形态）
}

// rewriteChatBody 解码一次、套用全部改写、再编码一次。
//
// 出站链路原本是「规范化 → 补首条 system → 注入 cache key → 注入 conversation
// RequestId」四个独立函数，每个都整解整编一遍 body：几十 KB 的体每请求因此要
// unmarshal/marshal 3~4 轮。合并成一轮后编码顺序不变（map 键按字典序输出），
// 产物与串联版逐字节一致。
func rewriteChatBody(src []byte, rewrite *chatBodyRewrite) []byte {
	if len(src) == 0 {
		return src
	}

	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil || obj == nil {
		return src
	}

	normalizeChatBody(obj, rewrite)

	if rewrite.leadingSystem {
		ensureLeadingSystemObj(obj)
	}

	if rewrite.injectCacheKey {
		injectPromptCacheKeyObj(obj, rewrite.uid, rewrite.conversationID)
	}

	injectConversationRequestIDObj(obj, rewrite.requestID)

	if rewrite.agentCLI {
		injectAgentCLIObj(obj)
	}

	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}

	return out
}

// normalizeChatBody 对已解码 body 套用基础规范化序列（保持既有顺序语义）。
func normalizeChatBody(obj map[string]any, rewrite *chatBodyRewrite) {
	sanitize, efforts, defaultEfforts, maxOut, historyKeepTurns :=
		rewrite.sanitize, rewrite.efforts, rewrite.defaultEfforts, rewrite.maxOut, rewrite.historyKeepTurns

	// 先剥 realm 前缀：后续 max_tokens / thinking / effort 都按裸模型名查表，
	// 晚一步就会查空。正常链路 handler 已剥过一次，这里兜裸用/测试入口。
	normalizeModelName(obj)
	obj["stream"] = true
	// max_completion_tokens → max_tokens 翻译（吸收上游 PR #116，Closes #117）：
	// OpenAI 规范里 max_tokens 已 deprecated、max_completion_tokens 是新字段；
	// DeepSeek Harness 等新客户端只发别名。WorkBuddy 上游（CN /v2 与 global
	// /console 同源）只认 max_tokens——别名透传会被上游忽略后回落默认输出上限
	// （实测 32000），长流任务被截。
	translateMaxCompletionTokens(obj)
	// 目录动态值补默认输出上限（同 PR #116 的另一个缺口：多数客户端连别名都
	// 不发——Codex 实测不带任何限额字段，被上游默认 32000 截断）。必须先于
	// clamp：目录值同样是出站 max_tokens，不能让注入绕过 GPT 下限。
	injectCatalogMaxTokens(obj, maxOut)
	clampGPTMinMaxTokens(obj)
	// stream_options 仅当 body 未显式带时补 {include_usage: true}（D7）：
	// 官方 CLI 流式必发该字段，上游据此在末帧返回 usage 用量；显式带则不覆盖。
	if _, has := obj["stream_options"]; !has {
		obj["stream_options"] = map[string]any{"include_usage": true}
	}

	normalizeToolChoice(obj)
	normalizeStop(obj)
	normalizeToolSchemas(obj)
	normalizeRoles(obj)
	normalizeImageURL(obj)
	// 防御层：第三方把 reasoning_text / reasoning 当成 content part 回放时，上游 content
	// 类型白名单不含它们 → 400 code=11101。本仓库自带 responses→chat 转换器（responses.go
	// 里 reasoning item 走 default 丢弃），主链路不会产生该形态；这里只兜第三方客户端。
	dropReasoningContentParts(obj)
	// tool 配对三步（见 tool_pairing.go）：先合并再重排再清理。所有模型一律执行（独立于
	// deepseek-only 的 sanitize 开关）。这是「让请求通过」的安全网——不完整配对的
	// tool_calls/tool 结果会让上游对之后每条消息都返 400，必须先行剔除；
	// 插在结果中间的非 tool 消息（Codex image_resize_notice）同样判配对断裂，
	// 先 repack 挪后，再 cleanup 删孤儿，两侧同口径。
	//
	// 顺序不能换：mergeAdjacentToolCalls 必须最先跑——它把「背靠背的两条
	// assistant.tool_calls」合成一条（部分 agent 客户端回放并行调用的报文形状），是上游
	// deepseek 系模型 11148 的正面修复；先合并再 repack，repack 才看得到完整的一批调用。
	if msgs, ok := obj["messages"].([]any); ok {
		msgs, _ = mergeAdjacentToolCalls(msgs)
		msgs, _ = repackToolResultBlocks(msgs)
		msgs, _ = cleanupOrphanToolCalls(msgs)
		// 无改动时两步都返回原 slice，这里回写等于零操作；任一步重排/删除
		// （哪怕后续步骤零改动）也必须落到 obj——不能只在「最后一步改动」时回写，
		// 否则 repack 单独生效的结果会被原 slice 覆盖丢失。
		obj["messages"] = msgs
	}

	trimHistoryInObj(obj, historyKeepTurns) // issue #99：排在配对三步之后
	// DeepSeek 思维链开关（见 thinking.go）：注入 thinking.type=enabled + 缺档补默认档。
	// 先于 normalizeReasoningEffort 执行：补入的默认档也要走既有降级管线，
	// 模型不支持默认档时自动落到 ≤ 默认档的最高支持档（不出站不合规档位）。
	modelName, _ := obj["model"].(string)
	injectThinking(obj, lookupDefaultEffort(defaultEfforts, modelName))
	normalizeReasoningEffort(obj, efforts)
	// DeepSeek 多轮一致性：assistant 消息带 reasoning 痕迹时回填 reasoning_content
	// （requiresReasoningContentOnAssistantMessages，见 thinking.go）。
	backfillReasoningContent(obj)

	if sanitize {
		if msgs, ok := obj["messages"].([]any); ok {
			sanitizeMessages(msgs)
		}
	}
}

// translateMaxCompletionTokens 把 OpenAI 别名 max_completion_tokens 翻译为上游
// 认的 max_tokens（吸收上游 PR #116）。规则：显式 max_tokens 优先（别名只删）；
// 别名非正数值（0/null/负数）不翻译（0/null 语义是「未设置」，负数是非法值，
// 翻译等于把垃圾搬进 max_tokens）；非数值别名（字符串等畸形）不翻译（原样
// 透传由上游报 11101 参数错）。两域同口径：CN /v2 与 global /console 是同一套
// API，翻译不分 realm。
func translateMaxCompletionTokens(obj map[string]any) {
	alias, has := obj["max_completion_tokens"]
	delete(obj, "max_completion_tokens") // 无论翻译与否，别名一律删（减少 body 体积与排障噪音）

	if !has {
		return
	}

	// null 与缺失同义（下游 injectCatalogMaxTokens 也按未设置处理）：不能因为
	// 「键存在」就吞掉客户端真正想表达的 max_completion_tokens。
	if obj["max_tokens"] != nil {
		return // 显式 max_tokens 优先：别名只删不译
	}
	// json.Unmarshal 数字 → float64（整数去整后回写，避免 1.28e5 科学计数法/小数
	// 尾巴进上游 body）；其他数值类型防御性兼容（int 家族——手构造 map 的调用方）。
	switch item := alias.(type) {
	case float64:
		if item > 0 && item == float64(int64(item)) {
			obj["max_tokens"] = int64(item)
		}
	case int64:
		if item > 0 {
			obj["max_tokens"] = item
		}
	case int:
		if item > 0 {
			obj["max_tokens"] = int64(item)
		}
	}
}

// gptMinMaxTokens GPT 系上游接受的 max_tokens 下限。
const gptMinMaxTokens = 16

// clampGPTMinMaxTokens 把 GPT 系模型过小的 max_tokens 抬到下限。
//
// 背景：上游 GPT 系（实测 gpt-6-sol / gpt-6-luna / gpt-5.6-sol）对 max_tokens < 16
// 一律 400 code=11133 model_param_invalid（15 拒、16 过，同号同 body 对照）；hy4 等
// 非 GPT 模型无此限制。Claude Code 切模型时发 max_tokens 极小的探针，全号轮转同样
// 被拒 → 客户端 503，模型永远切不过去。账号与 body 其余部分无关，换号无用，只能
// 在发送前修。抬到下限只放宽输出上限、不改语义；未携带字段 / 非数值 / 已达下限一律不动。
func clampGPTMinMaxTokens(obj map[string]any) {
	model, _ := obj["model"].(string)
	if !strings.Contains(strings.ToLower(model), "gpt-") {
		return
	}

	var num int64

	switch n := obj["max_tokens"].(type) {
	case float64:
		num = int64(n)
	case int64:
		num = n
	case int:
		num = int64(n)
	default:
		return
	}

	if num < gptMinMaxTokens {
		obj["max_tokens"] = int64(gptMinMaxTokens)
		log.Printf("max_tokens clamped model=%s %d -> %d", model, num, gptMinMaxTokens)
	}
}

// normalizeModelName 把出站 model 剥成裸名：上游只认裸名，realm 前缀是网关侧
// 路由协议。model 缺失或非字符串时保持原样（不二次错误化坏 body）。
func normalizeModelName(obj map[string]any) {
	cur, ok := obj["model"].(string)
	if !ok {
		return
	}

	prefix, rest, ok := strings.Cut(cur, ":")
	if !ok || rest == "" || (prefix != "cn" && prefix != "global") {
		return
	}

	obj["model"] = rest
}

// injectCatalogMaxTokens 无显式 max_tokens 时按模型目录补默认输出上限。
// 上游对缺失限额的请求套保守默认（实测 32000）；目录声明的 maxOutputTokens 才是
// 模型真实预算。`max_tokens: null` 与缺失同义（JSON 里 null 即「未设置」），同样补；
// 非 null 的显式值优先。模型未收录或上限非正时零操作——目录也没值时 null 整键删除，
// 不透传（null 不是合法限额，透传只会把无效值带给上游）。
func injectCatalogMaxTokens(obj map[string]any, maxOut map[string]int64) {
	cur, has := obj["max_tokens"]
	if has && cur != nil {
		return
	}

	model, _ := obj["model"].(string)
	if v, ok := maxOut[model]; ok && v > 0 {
		obj["max_tokens"] = v

		return
	}

	if has {
		delete(obj, "max_tokens")
	}
}

// effortRank 档位从低到高。
var effortRank = map[string]int{"off": 0, "minimal": 1, "low": 2, "medium": 3, "high": 4, "xhigh": 5, "max": 6}

// normalizeReasoningEffort 按模型 supportedEfforts 降级 reasoning_effort（snake/camel 双字段兼容）。
//   - 请求档位模型支持 → 原样透传
//   - 请求档位不支持 → 改为 ≤请求档位的最高支持档（降级）
//   - 支持档全部高于请求档 → 取最低支持档（偏离最小）
//   - 未知模型/未知档位/未携带字段/模型未缓存 → 一律透传
func normalizeReasoningEffort(obj map[string]any, efforts map[string][]string) {
	// 关闭思考的档位（off/none/disabled）不属于降级管线，单独处理。
	if normalizeOffEffort(obj, efforts) {
		return
	}

	if len(efforts) == 0 {
		return
	}

	model, _ := obj["model"].(string)
	if model == "" {
		return
	}

	supported, ok := efforts[model]
	if !ok || len(supported) == 0 {
		return
	}

	key, reqStr, reqIdx, known := resolveRequestEffort(obj)
	if !known {
		return
	}
	// 在 ≤请求档位的支持档里选最高档；命中且与请求不同才改写。
	if applyBestEffort(obj, key, model, reqStr, supported, reqIdx) {
		return
	}
	// 支持档全部高于请求档：取最低支持档。
	if lowest := pickLowestEffort(supported); lowest != "" {
		obj[key] = lowest
		log.Printf("reasoning_effort floored model=%s %s -> %s", model, reqStr, lowest)
	}
}

// normalizeOffEffort 处理关闭思考档位：模型 supportedEfforts 声明支持时按支持
// 列表写法保留（支持档原样透传，不应被误删）；未声明支持则删除两个 effort 字段，
// 避免在模型缓存缺失/非 DeepSeek 时被就近抬成最弱正档。返回是否命中关闭意图。
func normalizeOffEffort(obj map[string]any, efforts map[string][]string) bool {
	if !effortValueOff(obj) {
		return false
	}

	if supported := supportedOffEffort(obj, efforts); supported != "" {
		if _, ok := obj["reasoning_effort"]; ok {
			obj["reasoning_effort"] = supported
		} else {
			obj["reasoningEffort"] = supported
		}

		return true
	}

	delete(obj, "reasoning_effort")
	delete(obj, "reasoningEffort")

	return true
}

// supportedOffEffort 返回模型支持列表里的关闭档位写法（off/none/disabled，
// 大小写/空白不敏感）；未缓存或未声明支持关闭档时返回空串。
func supportedOffEffort(obj map[string]any, efforts map[string][]string) string {
	model, _ := obj["model"].(string)
	if model == "" || len(efforts) == 0 {
		return ""
	}

	for _, item := range efforts[model] {
		switch strings.ToLower(strings.TrimSpace(item)) {
		case "off", "none", "disabled":
			return strings.TrimSpace(item)
		}
	}

	return ""
}

// resolveRequestEffort 解析请求携带的 reasoning 档位（snake/camel 双字段兼容）。
// known=false 表示未携带字段 / 非字符串 / 档位名未知，调用方一律透传。
func resolveRequestEffort(obj map[string]any) (key, reqStr string, reqIdx int, known bool) {
	if _, present := obj["reasoning_effort"]; present {
		key = "reasoning_effort"
	} else if _, present := obj["reasoningEffort"]; present {
		key = "reasoningEffort"
	} else {
		return "", "", 0, false
	}

	raw, ok := obj[key].(string)
	if !ok {
		return "", "", 0, false
	}

	reqStr = strings.TrimSpace(strings.ToLower(raw))
	reqIdx, known = effortRank[reqStr]

	return key, reqStr, reqIdx, known
}

// applyBestEffort 命中「≤请求档的最高支持档」时改写 obj[key]；返回是否命中。
func applyBestEffort(obj map[string]any, key, model, reqStr string, supported []string, reqIdx int) bool {
	best, bestIdx := "", -1

	for _, s := range supported {
		idx, k := effortRank[strings.TrimSpace(strings.ToLower(s))]
		if k && idx <= reqIdx && idx > bestIdx {
			best, bestIdx = s, idx
		}
	}

	if best == "" {
		return false
	}

	if !strings.EqualFold(best, reqStr) {
		obj[key] = best
		log.Printf("reasoning_effort downgraded model=%s %s -> %s", model, reqStr, best)
	}

	return true
}

// lowestEffortInit 档位排序的初始上界。
const lowestEffortInit = 1 << 30

// pickLowestEffort 取 supported 里 rank 最低的有效档位；无有效档返回 ""。
func pickLowestEffort(supported []string) string {
	lowest, lowestIdx := "", lowestEffortInit

	for _, s := range supported {
		idx, k := effortRank[strings.TrimSpace(strings.ToLower(s))]
		if k && idx < lowestIdx {
			lowest, lowestIdx = s, idx
		}
	}

	return lowest
}

// normalizeStop 把 OpenAI 兼容客户端常见的 `stop: "x"` 字符串形态归一为数组。
// 上游按数组校验，字符串会 400 code=11101（issue #246）。空串不转换，保持原语义：
// 是否把空串当「无停止序列」由上游决定，网关不替客户端丢弃字段。
func normalizeStop(obj map[string]any) {
	stop, ok := obj["stop"].(string)
	if !ok || stop == "" {
		return
	}

	obj["stop"] = []string{stop}
}

// normalizeRoles 把 messages 里的 developer 角色归一为 system。
//
// 背景：上游对 messages 的 role 字段做白名单校验，developer 不在白名单内，
// 命中即 HTTP 400 code=11-128。developer 是 OpenAI 新规范里 system 的别名
// （Codex / Cursor 等新客户端用它承载 system 级指令），改写为 system 不丢语义。
//
// 此归一化是「协议兼容」（补上游 role 白名单），不是「内容脱敏」，
// 因此有意与 SanitizeFingerprints / sanitize 参数解耦：即使 sanitize=false 也照常归一。
//
// 只认 developer 这一个值：其余 role（system/user/assistant/tool/任意未知值）一律原样保留，
// 不合并、不重排、不删除任何消息（上游对多 system 的行为尚未实测，合并会引入新变量）。
func normalizeRoles(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}

	for i, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}

		role, ok := msg["role"].(string)
		if !ok {
			continue
		}

		if strings.EqualFold(strings.TrimSpace(role), "developer") {
			msg["role"] = "system"

			log.Printf("role normalized developer->system idx=%d", i)
		}
	}
}

// normalizeImageURL 兼容 OpenAI chat 多模态内容的两种 image_url 写法。
//
// OpenAI Chat Completions 规范使用对象形态 {"url":"...","detail":"..."}，
// 部分客户端（以及 Responses -> Chat 转换器）会发送字符串形态 "data:..." 或
// "https://..."。WorkBuddy 上游只接受对象形态，字符串会返回 400 code=11101
// "cannot unmarshal string into ... ImageContent"。
//
// 这里只做形状转换：字符串转 {"url": 原值}；已有对象及其中 url/detail/mime_type
// 原样保留；空字符串、缺失值、对象内非法 url 一律不补默认值，让上游返回真实错误。
//
//nolint:gocognit // 字符串/对象/嵌套 part 形状转换线性展开。
func normalizeImageURL(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}

	for _, rawMsg := range msgs {
		msg, ok := rawMsg.(map[string]any)
		if !ok {
			continue
		}

		parts, ok := msg["content"].([]any)
		if !ok {
			continue
		}

		for _, rawPart := range parts {
			part, ok := rawPart.(map[string]any)
			if !ok || part["type"] != "image_url" {
				continue
			}

			imageURL, ok := part["image_url"].(string)
			if !ok || imageURL == "" {
				continue
			}

			part["image_url"] = map[string]any{"url": imageURL}
		}
	}
}

// dropReasoningContentParts 剔除 chat content 数组里的 reasoning_text / reasoning 片段。
//
// 定位：**出站防御层**。某些聚合器/客户端会把 `reasoning_text`（Responses 形态）或
// `reasoning` 当成 content part 回放进 messages[]，上游对该类型直接 400
// （code=11101 "unsupported content type"）。本仓库自带上游转换器，主链路不会产生
// 该形态；把本网关当 chat/completions 上游的第三方仍可能送进来，命中即整请求失败。
//
// 规则（与上游语义一致）：
//   - 删除 type 为 reasoning_text / reasoning 的片段（TrimSpace 后比较）；
//   - 删除后 content 为空数组且该消息无 tool_calls → 整条消息删除；
//   - 字符串 content、非对象片段、非数组 content 原样通过（不臆造改写）。
//
//nolint:gocognit // 片段过滤按类型/空数组/tool_calls 分层，顺序即语义。
func dropReasoningContentParts(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}

	out := make([]any, 0, len(msgs))

	for _, rawMsg := range msgs {
		msg, ok := rawMsg.(map[string]any)
		if !ok {
			out = append(out, rawMsg)

			continue
		}

		parts, ok := msg["content"].([]any)
		if !ok {
			out = append(out, rawMsg)

			continue
		}

		kept := make([]any, 0, len(parts))

		for _, rawPart := range parts {
			if part, isObj := rawPart.(map[string]any); isObj {
				partType, _ := part["type"].(string)
				if isReasoningPartType(partType) {
					continue
				}
			}

			kept = append(kept, rawPart)
		}

		if len(kept) == 0 {
			if _, hasToolCalls := msg["tool_calls"]; !hasToolCalls {
				continue
			}
		}

		msg["content"] = kept

		out = append(out, rawMsg)
	}

	obj["messages"] = out
}

// isReasoningPartType 报告 content part 的 type 是否属于思考片段
// （reasoning_text / reasoning，容忍前后空白）。
func isReasoningPartType(partType string) bool {
	switch strings.TrimSpace(partType) {
	case "reasoning_text", "reasoning":
		return true
	}

	return false
}

// LeadingSystemFallback 官方分发包里首条 system 的兜底文案。补的必须是官方
// 存在的字节，不能用自造句——自造句本身是渠道指纹。handler 侧补齐开头角色时
// 也复用同一常量，避免两处兜底文案漂移。
const LeadingSystemFallback = "You are CodeBuddy Code.\n"

// ensureLeadingSystemObj 首条消息非 system 时在 messages 最前补一条官方 fallback
// system。上游对部分账号灰度启用形态校验：messages[0] 不是 system 即 400 11-128
// "first message is not system prompt"。补的必须是官方分发包存在的字节
// （LeadingSystemFallback），不能用自造句——自造句本身是渠道指纹。
// 两个 realm 都套用；首条已是 system 时不重复注入。
func ensureLeadingSystemObj(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return
	}

	if first, ok := msgs[0].(map[string]any); ok {
		if role, _ := first["role"].(string); strings.EqualFold(strings.TrimSpace(role), "system") {
			return // 首条已是 system：不注入
		}
	}

	obj["messages"] = append([]any{map[string]any{"role": "system", "content": LeadingSystemFallback}}, msgs...)
}

// normalizeToolChoice 按上游 Go struct（string 类型）改写 OpenAI tool_choice。
//   - "none"            → 删 tool_choice + 删 tools/functions
//   - {"type":"none"}   → 同上
//   - {"type":"auto"/"required"} → 字符串 "auto"/"required"
//   - {"type":"function","function":{"name":"x"}} → 字符串 "x"
//   - 其他对象/非标量 → 删 tool_choice
func normalizeToolChoice(obj map[string]any) {
	suppress := func() {
		delete(obj, "tools")
		delete(obj, "functions")
	}

	tc, present := obj["tool_choice"]
	if !present {
		return
	}

	switch item := tc.(type) {
	case string:
		if strings.EqualFold(strings.TrimSpace(item), "none") {
			delete(obj, "tool_choice")
			suppress()
		}
	case map[string]any:
		normalizeToolChoiceObject(obj, item, suppress)
	default:
		delete(obj, "tool_choice")
	}
}

// normalizeToolChoiceObject 处理对象形态的 tool_choice（type 决定改写/删除）。
func normalizeToolChoiceObject(item, table2 map[string]any, suppress func()) {
	typ, _ := table2["type"].(string)

	typ = strings.ToLower(strings.TrimSpace(typ))
	switch typ {
	case "none":
		delete(item, "tool_choice")
		suppress()
	case "auto", "required":
		item["tool_choice"] = typ
	case "function":
		item["tool_choice"] = toolChoiceFunctionName(table2)
	default:
		delete(item, "tool_choice")
	}
}

// toolChoiceFunctionName 取 function 形态里的工具名（function.name 优先，回落裸 name）；
// 缺失/空串回落 "auto"。
func toolChoiceFunctionName(table map[string]any) string {
	name := ""
	if fn, ok := table["function"].(map[string]any); ok {
		name, _ = fn["name"].(string)
	}

	if name == "" {
		name, _ = table["name"].(string)
	}

	if name = strings.TrimSpace(name); name != "" {
		return name
	}

	return "auto"
}

// normalizeToolSchemas 规整 tools 子树的 JSON Schema：pattern 的非标转义 `\_`（→ `_`）
// + 只写 enum 不写 type 的属性补出可无损推断的 type。
//
// 上游对 tools[].function.parameters 做严格 JSON Schema/正则文法校验，pattern 含
// `\_`（转义的字面量下划线）会整体拒收：400 code=11129 invalid_function_call_
// parameters（displayMsg「工具定义不合规」）。`\_` 不是任何正则文法的合法转义，
// 但所有主流引擎（RE2/PCRE/JS Annex B）都宽容地视为 `_` 本身——上游校验器比它们
// 全部更严（对照 V8 严格文法 u 标志，唯一同样拒绝的实现）。实案：ZCode 的 exa 插件
// agent_run 工具 runId/previousRunId 带 `^agent\_run\_`，deepseek 系全家确定性 400
// → 网关侧归 ErrClient 只换号不罚但喂连败计数 → 轮转烧满 5 连败触发连败降权、
// 客户端 503（2026-09-29/30 两次实案）。schema 级拒绝换账号无用，只能在发送前修。
//
// 归一无损：`\_` 与 `_` 在所有引擎匹配语义相同（各引擎实测 + 上游对照探针：归一后
// 200），工具方功能不变。只动 tools 子树（pattern 值 + patternProperties 键）；
// 消息正文里的 `\_`（如 Windows 路径 C:\_x）不碰。其余非标转义（`\:` 等）未证实
// 触发，不扩面——有实案再议。独立于 sanitize 开关：这是「让请求通过」，不是脱敏。
func normalizeToolSchemas(obj map[string]any) {
	rawTools, ok := obj["tools"].([]any)
	if !ok {
		return
	}

	for _, raw := range rawTools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		// OpenAI 形态 tools[].function.parameters；裸 tools[].parameters 兼容。
		if fn, ok := tool["function"].(map[string]any); ok {
			normalizeSchemaTree(fn["parameters"])
		}

		normalizeSchemaTree(tool["parameters"])
	}
}

// normalizeSchemaTree 递归规整 schema 树的单个节点，两件事：
//
//  1. pattern 值与 patternProperties 键里的 `\_` → `_`（patternProperties 的键也是
//     正则；map 键不可原地改，命中时重建该层）；
//  2. 只写 enum、没写 type 的属性补出 type（见 inferEnumType）。
//
// 第 2 条的实案：工具参数 schema 写成 {"enum":["a","b"]} 而不给 type，Kimi 系模型
// 严格校验后整单 400 code=11133（Invalid request parameters，extError「the request
// parameters were rejected by the model provider」）——该模型对这类客户端等于全废；
// 补上 "type":"string" 即 200（hy3 / glm-5.3 不受影响）。
func normalizeSchemaTree(node any) {
	switch n := node.(type) {
	case map[string]any:
		if p, ok := n["pattern"].(string); ok && strings.Contains(p, `\_`) {
			n["pattern"] = strings.ReplaceAll(p, `\_`, `_`)
		}

		inferEnumType(n)

		normalizePatternProperties(n)

		for k, v := range n {
			normalizeSchemaChild(k, v)
		}
	case []any:
		for _, v := range n {
			normalizeSchemaTree(v)
		}
	}
}

// schemaValueKeys 承载**数据取值**而非子 schema 的键：default / examples / const /
// enum。递归必须在此止步——它们是值域，不是 schema 子树；进到默认值或示例里补
// type、把 `\_` 改成 `_`，改的是客户端写的数据本身（default 里的对象会凭空多出
// 一个 type 键），实际下发的默认值/示例就与原意不符。
var schemaValueKeys = []string{"default", "examples", "const", "enum"}

// schemaMapKeys 值为「名字 → 子 schema」映射的键。必须显式处理：键名由用户决定
// （properties 里完全可以有叫 enum / default 的属性），不能靠 schemaValueKeys 过滤。
var schemaMapKeys = []string{"properties", "patternProperties", "$defs", "definitions"}

// normalizeSchemaChild 按 schema 关键字归一递归方向：值域键不进入，名字映射键
// 逐个进，其余（items / anyOf / allOf / $ref 等）按普通子树进。
func normalizeSchemaChild(key string, value any) {
	switch {
	case slices.Contains(schemaValueKeys, key):
		return
	case slices.Contains(schemaMapKeys, key):
		children, ok := value.(map[string]any)
		if !ok {
			return
		}

		for _, child := range children {
			normalizeSchemaTree(child)
		}
	default:
		normalizeSchemaTree(value)
	}
}

// inferEnumType 给「有 enum 无 type」的 schema 节点补出可无损推断的 type；推不出唯一
// 类型就不写（不猜——让上游按原语义报错，好过补错类型改掉契约）。
//
// 只走 tools 子树的遍历，const 与 response_format 不在范围内：那是**输出**约束，
// 补 type 会改语义。
func inferEnumType(n map[string]any) {
	if _, hasType := n["type"]; hasType {
		return
	}

	enum, ok := n["enum"].([]any)
	if !ok || len(enum) == 0 {
		return
	}

	if kind, ok := enumScalarType(enum); ok {
		n["type"] = kind
	}
}

// enumScalarType 推断 enum 取值的唯一标量类型：全串 string、全整数 integer、
// 含小数 number、全布尔 boolean。
//
// 其余形态一律 ok=false（保持原样）：含 null（JSON Schema 里 null 要 type 联合，补单
// 值会改语义）、对象/数组、混合标量（"1" 与 1 并存、布尔与数字并存）都没有唯一无损
// 答案。数字按 json 解码后的 float64 看：整数值（含 1.0）算 integer，带小数才算
// number——JSON Schema 的 integer 接受 1.0，取更窄的那个。
func enumScalarType(enum []any) (string, bool) {
	kind := ""

	for _, item := range enum {
		cur, ok := enumItemType(item)
		if !ok {
			return "", false
		}

		if kind, ok = mergeEnumType(kind, cur); !ok {
			return "", false
		}
	}

	return kind, kind != ""
}

// enumItemType 单个 enum 取值的标量类型；非标量（null / 对象 / 数组）判否。
func enumItemType(item any) (string, bool) {
	switch value := item.(type) {
	case string:
		return "string", true
	case bool:
		return "boolean", true
	case float64:
		if value != math.Trunc(value) {
			return "number", true
		}

		return "integer", true
	case int, int64:
		return "integer", true
	default:
		return "", false
	}
}

// mergeEnumType 合并两个已推断类型：同类型不变，整型与小数混合取 number（仍是唯一
// 无损答案），其余混合放弃推断。
func mergeEnumType(kind, cur string) (string, bool) {
	switch {
	case kind == "" || kind == cur:
		return cur, true
	case (kind == "integer" && cur == "number") || (kind == "number" && cur == "integer"):
		return "number", true
	default:
		return "", false
	}
}

// normalizePatternProperties 改写 patternProperties 键里的 `\_`（map 键不可原地改，
// 命中时重建该层并写回 n）。
func normalizePatternProperties(n map[string]any) {
	props, ok := n["patternProperties"].(map[string]any)
	if !ok {
		return
	}

	rebuilt := false

	fixed := make(map[string]any, len(props))
	for item, iface := range props {
		if strings.Contains(item, `\_`) {
			item = strings.ReplaceAll(item, `\_`, `_`)
			rebuilt = true
		}

		fixed[item] = iface
	}

	if rebuilt {
		n["patternProperties"] = fixed
	}
}

// injectAgentCLIObj 给除 leading system 外的每条消息补 agent:"cli"。
// 官方 HistoryInterceptors 对每个输入项写 providerData.agent（主会话取当前 agent
// 名 `cli`），itemsToMessages 再把 providerData 摊进消息；leading system 由
// systemInstructions 单独 unshift，不带该键。客户端已自带非空值时保留，不覆盖。
func injectAgentCLIObj(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}

	for i, raw := range msgs {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}

		// 官方 itemsToMessages 先把历史项摊平成消息（保留 providerData，
		// 其中主会话每条都带 agent），再由调用方 unshift 一条 systemInstructions。
		// 所以只有首条 system 没有 agent，历史里其余 system 项照常带。
		if i == 0 {
			if role, _ := msg["role"].(string); role == "system" {
				continue
			}
		}

		if v, _ := msg["agent"].(string); v == "" {
			msg["agent"] = "cli"
		}
	}
}

// injectConversationRequestIDObj 给当前轮最后一条 user 消息补
// conversationRequestId / startsNewUserRequest（issue #249）。只改最后一条 user，
// 历史轮次不动；客户端自带值优先。requestID 为空或无 user 消息时零操作。
func injectConversationRequestIDObj(obj map[string]any, requestID string) {
	if requestID == "" {
		return
	}

	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}

	for _, raw := range slices.Backward(msgs) {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}

		if role, _ := msg["role"].(string); role != "user" {
			continue
		}

		if _, exists := msg["conversationRequestId"]; !exists {
			msg["conversationRequestId"] = requestID
		}

		if _, exists := msg["startsNewUserRequest"]; !exists {
			msg["startsNewUserRequest"] = true
		}

		break
	}
}
