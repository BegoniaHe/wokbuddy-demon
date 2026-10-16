// global 模型目录探测：纯动态产出模型名及其窗口 / 能力元数据（v3-config-merge）。
//
// 探测并发四路：/v3/config 的三种 UA（桌面端主路 + IDE + CLI）与企业端点家族
// （/v2 → /console 补缺），并集 = v3 条目为主、企业端点补 v3 缺失的 id
// （如 gpt-5.3-codex 只在 /v2 下发）。倍率字段（credits）虽随目录下发，但
// 只透出展示，不注入 costTier、不参与选号。
//
// 纯动态：不再回落任何静态名单——拉不出目录即意味着该域上游不可用，
// 假名单只会让客户端选到 11102 的模型（产品决策：无兜底）。
package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// GlobalModelNames 国际版（global realm）历史静态名单（PLAN §7.2 附录 21 名）。
// 纯动态化后**不再作为模型目录的基底/兜底**：/v1/models 只透出上游实际下发的模型。
// 保留仅作历史对照（global e2e 观测日志差集参照）。
var GlobalModelNames = []string{
	"default-model",
	"fast-model",
	"balanced-model",
	"primary-model",
	"hy4-preview",
	"gpt-5.6-sol",
	"gpt-5.6-terra",
	"deep-model",
	"deepseek-v4.1-flash",
	"gpt-6-astra",
	"hy4-preview-f",
	"hy3",
	"glm-5.2",
	"gpt-5.6-luna",
	"gpt-5.5",
	"gpt-5.4",
	"gpt-5.3-codex",
	"gemini-3.5-flash",
	"glm-5.3",
	"kimi-k3",
	"kimi-k2.6",
}

// fetchGlobalModelsCache 探测结果缓存（语义参照 CN 侧 handler.dynamicModelsCache：1h TTL +
// 5min 失败负缓存）。按 Client 实例持有（effortsMu 同模式），测试新建 Client 即隔离。
// Mutex 内嵌，与 modelList 无并发读路径竞争（唯一读写点本文件内）。
type fetchGlobalModelsCache struct {
	sync.Mutex

	names    []string    // 成功缓存：并集模型名（已去重）；nil = 未探测/失败
	infos    []ModelInfo // 成功缓存：对象形态的全字段条目（窄表/失败形态为 nil）
	fetched  time.Time
	lastFail time.Time
}

// globalModelsTTL / globalModelsFailCooldown 探测缓存时长：成功 1h，失败 5min 负缓存。
const (
	globalModelsTTL          = time.Hour
	globalModelsFailCooldown = 5 * time.Minute
)

// globalModelsProbePaths global 企业模型目录端点候选序列（按 realm 切 base，路径"家族"）：
// /v2 家族优先（实测 /v2/enterprises/personal/models 200 含完整模型表），
// /console 作 fallback（同域旧路径，或 500）。v3-config-merge 后该家族降为企业补充路
// （/v3/config 为主路，与家族并发探测；gpt-5.3-codex 等家族独有模型经此进并集）。
var globalModelsProbePaths = []string{
	"/v2/enterprises/personal/models",
	"/console/enterprises/personal/models",
}

// FetchGlobalModels 探测 global 账号的模型名目录并返回**模型名列表**（无元数据）。
//
// 纯动态：成功返回并集结果（去重），缓存 1h；失败（两路全非 2xx / 解析失败 /
// 空列表）记 5min 负缓存，返回 nil（无静态回落）。缓存/负缓存命中：直接返回，零上游调用。
//
// 调用方负责：仅在有 global 账号时调用（无则不探测）；GlobalEnabled 关闭时（逃生门）
// 不得调用——本方法由 globalOn(a) 内部兜底，若账号因开关回落 cn 则返回 nil。
func (c *Client) FetchGlobalModels(a *auth.Auth) []string {
	names, _ := c.fetchGlobalModelsOnce(a)
	return names
}

// FetchGlobalModelInfos 探测 global 账号的模型目录并返回全字段 ModelInfo 列表。
// 与 FetchGlobalModels 共享同一次探测与缓存（names + infos 一体落缓存）：
// 对象形态 200 → 全字段条目；窄表形态 / 探测失败 / 负缓存 / 非 global 路由账号
// → nil（调用方按 ID 名单输出裸条目，不编造字段）。
// 账号因 GlobalEnabled 开关回落 cn 时不探测（globalOn 兜底，零上游调用）。
func (c *Client) FetchGlobalModelInfos(a *auth.Auth) []ModelInfo {
	_, infos := c.fetchGlobalModelsOnce(a)
	return infos
}

// fetchGlobalModelsOnce 单次探测决策（缓存命中/负缓存/触发探测），返回 (names, infos)。
// 纯动态：成功 = 并集结果去重；一切失败 = nil（不回落静态）。
// infos 仅对象形态成功探测时非 nil。
func (c *Client) fetchGlobalModelsOnce(a *auth.Auth) (names []string, infos []ModelInfo) {
	if !c.globalOn(a) {
		// 逃生门兜底：账号不路由 global 上游 → 不探测（零上游调用）。
		return nil, nil
	}
	if names, infos, hit := c.globalModelsCacheHit(); hit {
		return names, infos
	}

	names, infos, efforts, defaults, err := c.probeGlobalModels(a)
	if err != nil || len(names) == 0 {
		// 探测失败：负缓存 + 返回 nil（effort 桶不写，prepareBody 走 globalEffortMap 静态兜底）。
		c.markGlobalModelsFail()
		return nil, nil
	}
	// global 域 effort 能力：探测下发的 supportedEfforts/defaultEffort 权威写入 global 桶
	// （raw remote，不并入静态表——静态兜底在 prepareBody 的 globalEffortMap 与
	// /v1/models 的 EffortListing 里按需 fallback）。空探测不写（防清既有桶）。
	if len(efforts) > 0 || len(defaults) > 0 {
		c.storeEfforts("global", efforts, defaults)
	}
	c.storeModelRates("global", infos)

	// 成功：探测结果去重。names/infos 均落缓存；倍率等选号敏感字段只透出展示，
	// 不注入 costTier。
	merged := dedupeNames(names)
	c.storeGlobalModels(merged, infos)
	return merged, infos
}

// globalModelsCacheHit 报告探测缓存/负缓存是否命中；命中时返回缓存的 (names, infos)。
func (c *Client) globalModelsCacheHit() (names []string, infos []ModelInfo, hit bool) {
	c.globalModels.Lock()
	defer c.globalModels.Unlock()
	if len(c.globalModels.names) > 0 && time.Since(c.globalModels.fetched) < globalModelsTTL {
		return c.globalModels.names, c.globalModels.infos, true
	}
	if !c.globalModels.lastFail.IsZero() && time.Since(c.globalModels.lastFail) < globalModelsFailCooldown {
		// 负缓存冷却期内：避免反复打上游，直接按失败处理（无静态回落）。
		return nil, nil, true
	}
	return nil, nil, false
}

// markGlobalModelsFail 记录一次探测失败（负缓存 + 清空成功缓存）。
func (c *Client) markGlobalModelsFail() {
	c.globalModels.Lock()
	c.globalModels.lastFail = time.Now()
	c.globalModels.names = nil
	c.globalModels.infos = nil
	c.globalModels.Unlock()
}

// storeGlobalModels 写入成功探测缓存并清除负缓存。
func (c *Client) storeGlobalModels(names []string, infos []ModelInfo) {
	c.globalModels.Lock()
	c.globalModels.names = names
	c.globalModels.infos = infos
	c.globalModels.fetched = time.Now()
	c.globalModels.lastFail = time.Time{}
	c.globalModels.Unlock()
}

// dedupeNames 去重模型名（保原序，跳过空串）。
func dedupeNames(names []string) []string {
	seen := make(map[string]bool, len(names))
	merged := make([]string, 0, len(names))
	for _, id := range names {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		merged = append(merged, id)
	}
	return merged
}

// probeGlobalModels 发起一次 global 模型目录探测（v3-config-merge + desktop-ua）：
// /v3/config（三路 UA：桌面端主路 + IDE + CLI）与企业端点家族（/v2 → /console 兜底，
// 补缺）**并发**探测后并集合并。返回模型名列表（已合并、未再去重——去重在
// fetchGlobalModelsOnce）、全字段 ModelInfo（对象形态；窄表为 nil）及 effort
// 能力桶（supportedEfforts/defaultEffort，可为空）。
//
// 三路 UA 缺一不可（2026-10-02 实测，同一 global 账号）：
//   - 桌面端 UA → 29 条：**唯一含 gpt-6-sol / gpt-6-luna / grok-4.7 / gemini-3.8-flash**
//   - IDE UA    → 13 条：独有 o4-mini / enhance-1.0 / auto-chat
//   - CLI UA    → 22 条：独有 deepseek 系列 / gpt-6-astra / kimi-k2.8-preview
//
// 桌面端 UA 为主路：它是项目 chat 路径实际使用的 UA（与客户端看到的目录同源），
// 且实测在两路共有的模型上字段完整度不低于另两路（窗口/effort/credits 齐全）。
// 合并口径：桌面端条目字段权威，IDE/CLI 只补桌面端缺失的模型 id；去重 key =
// 模型 id，输出顺序稳定。全部 v3 路失败且企业端点也失败才返回错误；单路失败
// 降级为其余路结果 + warn 日志，互不拖累。
func (c *Client) probeGlobalModels(a *auth.Auth) (names []string, infos []ModelInfo, efforts map[string][]string, defaults map[string]string, err error) {
	// 桌面端 UA（主路）：项目 chat 路径实际使用的 UA，与官方客户端看到的目录同源。
	// 按账号 realm 生成（global → `WorkBuddy AI` 平台段），故必须传 a 而非用常量。
	// 三路 v3 并发（桌面端 / IDE / CLI，各有独有模型，见上方 UA 注释）。
	v3DesktopCh := make(chan probeResult, 1)
	go func() { v3DesktopCh <- c.probeV3Route(a, c.defaultWorkBuddyUAFor(a)) }()
	v3IDECh := make(chan probeResult, 1)
	go func() { v3IDECh <- c.probeV3Route(a, codeBuddyIDEUA) }()
	v3CLICh := make(chan probeResult, 1)
	go func() { v3CLICh <- c.probeV3Route(a, codeBuddyCLIUA) }()
	enterpriseCh := make(chan probeResult, 1)
	go func() { enterpriseCh <- c.probeEnterpriseRoute(a) }()
	v3Desktop := <-v3DesktopCh
	v3IDE := <-v3IDECh
	v3CLI := <-v3CLICh
	enterprise := <-enterpriseCh
	// v3 三路自合并：桌面端路字段权威（主路），IDE/CLI 只补主路缺失的模型 id
	// （只补 id，字段仍取自各自条目——主路没有该 id 时才轮到它们）。
	// 逐路容错：全失败才带 err 进入下游降级判断，部分失败 warn 后继续。
	v3 := mergeV3Routes([]struct {
		label string
		res   probeResult
	}{
		{"desktop-UA", v3Desktop},
		{"IDE-UA", v3IDE},
		{"CLI-UA", v3CLI},
	})

	if v3.err != nil && enterprise.err != nil {
		// 两路全失败 → 负缓存语义（等价原家族端点全非 2xx）。
		return nil, nil, nil, nil, v3.err
	}
	if v3.err != nil {
		// /v3 失败降级：不拖累企业端点结果（降级仅企业端点 + warn）。
		log.Printf("WARN: [upstream] global models: v3/config probe failed (degraded to enterprise endpoint): %v", v3.err)
		names, infos, efforts, defaults = extractEfforts(enterprise.infos)
		return names, infos, efforts, defaults, nil //nolint:nilerr // v3 降级成功，企业端点结果有效
	}
	if enterprise.err != nil {
		log.Printf("WARN: [upstream] global models: enterprise endpoint failed (v3/config only): %v", enterprise.err)
		names, infos, efforts, defaults = extractEfforts(v3.infos)
		return names, infos, efforts, defaults, nil //nolint:nilerr // 企业端点失败但 v3 结果有效
	}
	return mergeGlobalProbeResults(v3, enterprise)
}

// probeV3Route 单次 /v3/config 探测（UA 参数化）并过滤非对话模型，返回该路结果。
// 该端点对不同 UA 下发**不同模型集合**：桌面端 / IDE / CLI 三路各有独有模型，故并发取并集。
func (c *Client) probeV3Route(a *auth.Auth, ua string) probeResult {
	// chatBase 已按 realm 切 global base。
	byID, perr := c.fetchV3ConfigModelMap(a, ua)
	if perr != nil {
		return probeResult{err: perr}
	}
	ids := make([]string, 0, len(byID))
	outInfos := make([]ModelInfo, 0, len(byID))
	for _, mi := range byID {
		if nonChatModel(mi.ID, mi.MaxTokens, mi.Tags) {
			continue
		}
		ids = append(ids, mi.ID)
		outInfos = append(outInfos, mi)
	}
	sort.Strings(ids) // map 迭代序随机，排序保输出稳定
	return probeResult{names: ids, infos: outInfos}
}

// probeEnterpriseRoute 企业端点家族探测：/v2 首选 → /console 兜底（既有探活序，零回归）。
func (c *Client) probeEnterpriseRoute(a *auth.Auth) probeResult {
	var lastErr error
	for _, path := range globalModelsProbePaths {
		names, infos, perr := c.globalModelsOnce(a, path)
		if perr != nil {
			lastErr = perr
			continue
		}
		return probeResult{names: names, infos: infos}
	}
	return probeResult{err: lastErr}
}

// mergeGlobalProbeResults 两路皆成功时合并：v3 为主、企业端点补缺（含 effort 桶合并，
// v3 权威）。
func mergeGlobalProbeResults(
	v3, enterprise probeResult,
) (names []string, infos []ModelInfo, efforts map[string][]string, defaults map[string]string, err error) {
	v3Names, v3Infos, v3Efforts, v3Defaults := extractEfforts(v3.infos)
	if len(v3Names) == 0 {
		v3Names = v3.names
	}
	entNames, entInfos, entEfforts, entDefaults := extractEfforts(enterprise.infos)
	if len(entNames) == 0 {
		entNames = enterprise.names
	}
	names, infos = mergeGlobalCatalog(v3Names, v3Infos, entNames, entInfos)
	efforts = mergeEffortBuckets(v3Efforts, entEfforts)
	defaults = mergeEffortDefaults(v3Defaults, entDefaults)
	return names, infos, efforts, defaults, nil
}

// probeResult 单路模型目录探测结果（/v3/config 某 UA 路，或企业端点家族）。
// infos 为对象形态条目（窄表探测为 nil），names 与之平行；err 非 nil 表示该路失败。
type probeResult struct {
	names []string
	infos []ModelInfo
	err   error
}

// mergeV3Routes 按优先级合并 /v3/config 多路 UA 探测结果：routes 必须**按权威
// 顺序**传入（主路在前）。前序路的字段权威，后续路只补前序路缺失的模型 id
// （条目字段仍取自其所属路——该 id 只有那一路知道）。
//
// 容错：全路失败才返回 err（取首路错误，与既有「v3 全失败」降级语义一致）；
// 部分失败 warn 日志后用成功路继续，单路故障不拖垮整次探测。
// 全路成功但并集为空时保留空结果（由调用方按「空列表 = 失败」处理）。
func mergeV3Routes(routes []struct {
	label string
	res   probeResult
},
) probeResult {
	var out probeResult
	var firstErr error
	nOK := 0
	for _, r := range routes {
		if r.res.err != nil {
			log.Printf("WARN: [upstream] global models: v3/config %s probe failed: %v", r.label, r.res.err)
			if firstErr == nil {
				firstErr = r.res.err
			}
			continue
		}
		nOK++
		if out.err != nil || (len(out.names) == 0 && len(out.infos) == 0) {
			// 首个成功路：整体作为基底（字段权威）。
			out = r.res
			continue
		}
		// 后续成功路：只补缺失的 id。
		mergedNames, mergedInfos := mergeGlobalCatalog(out.names, out.infos, r.res.names, r.res.infos)
		out.names, out.infos = mergedNames, mergedInfos
	}
	if nOK == 0 {
		return probeResult{err: firstErr}
	}
	return out
}

// extractEfforts 从条目列表抽取 effort 能力桶（supportedEfforts 数组优先；
// 缺数组但 defaultEffort 单档非空也入 defaults 桶）并顺带返回有序 names。
func extractEfforts(infos []ModelInfo) (names []string, out []ModelInfo, efforts map[string][]string, defaults map[string]string) {
	names = make([]string, 0, len(infos))
	for _, mi := range infos {
		if mi.ID == "" {
			continue
		}
		names = append(names, mi.ID)
		out = append(out, mi)
		if len(mi.Efforts) > 0 {
			if efforts == nil {
				efforts = make(map[string][]string)
			}
			efforts[mi.ID] = mi.Efforts
		}
		if mi.DefaultEffort != "" {
			if defaults == nil {
				defaults = make(map[string]string)
			}
			defaults[mi.ID] = mi.DefaultEffort
		}
	}
	return names, out, efforts, defaults
}

// mergeGlobalCatalog 两路合并（v3 主、企业补缺）：names 按 id 去重（v3 原序在前、
// 企业端点补充项在其原序后追加——稳定输出）；infos 同步合并（v3 条目字段权威，
// 企业端点条目只在 id 缺失时进并集）。
// 窄表形态（infos nil）时保持 nil——无对象字段不编造。
func mergeGlobalCatalog(
	primaryNames []string, primaryInfos []ModelInfo, secondaryNames []string, secondaryInfos []ModelInfo,
) (names []string, infos []ModelInfo) {
	if len(secondaryNames) == 0 {
		return primaryNames, primaryInfos
	}
	seen := make(map[string]bool, len(primaryNames)+len(secondaryNames))
	out := make([]string, 0, len(primaryNames)+len(secondaryNames))
	out = appendUniqueNames(out, seen, primaryNames)
	var outInfos []ModelInfo
	if primaryInfos != nil {
		outInfos = make([]ModelInfo, 0, len(primaryInfos)+len(secondaryInfos))
		outInfos = append(outInfos, primaryInfos...)
	}
	for _, id := range secondaryNames {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
		// 窄表企业响应（secondaryInfos nil / 超出条目数）时该 id 无对象字段，
		// infos 保持原样（调用方按 id 名单输出裸条目，不编造字段）。
		if mi, ok := secondaryInfosByID(secondaryInfos, id); ok {
			outInfos = append(outInfos, mi)
		}
	}
	return out, outInfos
}

// appendUniqueNames 把 names 中非空且未出现过的 id 追加到 out，并更新 seen。
func appendUniqueNames(out []string, seen map[string]bool, names []string) []string {
	for _, id := range names {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// secondaryInfosByID 在 secondaryInfos 中查找 id 对应条目。
func secondaryInfosByID(secondaryInfos []ModelInfo, id string) (ModelInfo, bool) {
	for _, mi := range secondaryInfos {
		if mi.ID == id {
			return mi, true
		}
	}
	return ModelInfo{}, false
}

// mergeEffortBuckets 合并两路 effort 桶：主路（v3）权威，企业端点只补主路缺失的模型档位。
func mergeEffortBuckets(primary, secondary map[string][]string) map[string][]string {
	if len(secondary) == 0 {
		return primary
	}
	out := primary
	if out == nil {
		out = make(map[string][]string, len(secondary))
	}
	for id, v := range secondary {
		if _, ok := out[id]; !ok {
			out[id] = v
		}
	}
	return out
}

// mergeEffortDefaults 合并两路 defaultEffort：主路（v3）权威，企业端点只补缺失。
func mergeEffortDefaults(primary, secondary map[string]string) map[string]string {
	if len(secondary) == 0 {
		return primary
	}
	out := primary
	if out == nil {
		out = make(map[string]string, len(secondary))
	}
	for id, v := range secondary {
		if _, ok := out[id]; !ok {
			out[id] = v
		}
	}
	return out
}

// globalModelsOnce 单端点探测。2xx + 解析出非空名单 → (names, infos, nil)；否则 (nil, nil, err)。
func (c *Client) globalModelsOnce(a *auth.Auth, path string) ([]string, []ModelInfo, error) {
	url := c.chatBase(a) + path // 按 realm 切 base：global 账号 → global base
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, nil, err
	}
	c.CommonHeaders(req, a) // 共享请求头（Origin/Referer/UA），与 FetchModels 同款
	// AccessToken 加锁快照（见 auth.AccessTokenValue：keepalive 刷新在 a.mu 内改写）。
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		// 读失败 → 传输层错误：半截 body 不进解析（探测负缓存走 lastFail，不罚号）。
		return nil, nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("global models status %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	names, infos, _, _, err := parseGlobalModelNames(raw)
	return names, infos, err
}

// parseGlobalModelNames 多信封兼容解析模型目录（国际站曾在多种 envelope 间切换，
// 只认单一形态会把登录成功的账号误判为"无模型"）：
//   - 信封：code 存在且非 0 才拒（无 code 字段也放行）；payload = data（非空非
//     null 时）否则整包；
//   - 模型数组定位：payload 本身是数组，或是 map 下 models/items/list/data/result
//     任一键（递归一层层往下找第一个非空数组）——覆盖 data.models / data.items /
//     data.list / 顶层 models 等变体；
//   - 数组主形态：对象数组按 dynModelEntry 全字段解析（与 CN 目录同构，字段零
//     漂移）：maxInputTokens/maxOutputTokens/maxAllowedSize/supportsReasoning/
//     supportsImages/reasoning.*；id 缺省时回退 name；disabled 剔除；
//   - 窄表：字符串数组 → 仅 ID，元数据留空（窗口由调用方四级查找链兜底）；
//   - 动态兜底：主形态解析不出任何可用条目时（上游换了键名），逐对象宽松解析——
//     id 依次回退 id/modelId/model/name，窗口键回退 contextWindow/maxTokens，
//     reasoning 档位（defaultEffort 新键优先、effort 老键兜底）保留。
//
// 同时产出 effort 能力桶（supportedEfforts/defaultEffort）。
// 解析成功但名单为空 → 返回错误（等价"该端点没给全"）。
func parseGlobalModelNames(raw []byte) (names []string, infos []ModelInfo, efforts map[string][]string, defaults map[string]string, err error) {
	payload, err := resolveGlobalPayload(raw)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	arr, ok := resolveGlobalModelsArray(payload)
	if !ok {
		return nil, nil, nil, nil, errors.New("global models empty list")
	}
	// 主形态：对象数组 → dynModelEntry 全字段（与 CN FetchModels 共用解析，零口径漂移）。
	if names, infos, efforts, defaults, ok := parseGlobalModelEntries(arr); ok {
		return names, infos, efforts, defaults, nil
	}
	// 可用条目为 0（键名全对不上）：落入下方动态兜底，不在这里报空。
	// 窄表：字符串数组 → 仅 ID（无 effort 元数据、无对象字段 → infos nil）。
	if names, isStr := parseGlobalNarrowTable(arr); isStr {
		if len(names) > 0 {
			return names, nil, nil, nil, nil
		}
		return nil, nil, nil, nil, errors.New("global models empty list")
	}
	// 动态兜底：逐对象宽松解析（上游换键名时不至于整域空列表）。
	return parseGlobalModelLooseArray(arr)
}

// resolveGlobalPayload 解信封：code 非 0 拒绝（字段缺失 = 放行，兼容无信封直出的目录
// 端点）；payload = data（非空非 null 时）否则整包。
func resolveGlobalPayload(raw []byte) (json.RawMessage, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("global models parse: %w", err)
	}
	if codeRaw, ok := envelope["code"]; ok {
		var code int
		if json.Unmarshal(codeRaw, &code) == nil && code != 0 {
			return nil, fmt.Errorf("global models code=%d", code)
		}
	}
	if data, ok := envelope["data"]; ok {
		if t := strings.TrimSpace(string(data)); t != "" && t != "null" {
			return data, nil
		}
	}
	return json.RawMessage(raw), nil
}

// parseGlobalNarrowTable 窄表形态：字符串数组 → 仅 ID（去空白）。isStringArray=false
// 表示不是字符串数组（调用方回落动态兜底）。
func parseGlobalNarrowTable(arr json.RawMessage) (names []string, isStringArray bool) {
	var strs []string
	if json.Unmarshal(arr, &strs) != nil {
		return nil, false
	}
	out := make([]string, 0, len(strs))
	for _, id := range strs {
		if id = strings.TrimSpace(id); id != "" {
			out = append(out, id)
		}
	}
	return out, true
}

// looseCatalog 动态兜底逐条解析的累积结果。
type looseCatalog struct {
	names    []string
	infos    []ModelInfo
	efforts  map[string][]string
	defaults map[string]string
}

// addString 并入混合数组里的裸 ID 条目。
func (lc *looseCatalog) addString(id string) {
	lc.names = append(lc.names, id)
	lc.infos = append(lc.infos, ModelInfo{ID: id})
}

// addModel 并入宽松解析出的模型条目，并归集 effort 能力桶。
func (lc *looseCatalog) addModel(mi ModelInfo) {
	lc.names = append(lc.names, mi.ID)
	lc.infos = append(lc.infos, mi)
	if len(mi.Efforts) > 0 {
		if lc.efforts == nil {
			lc.efforts = make(map[string][]string)
		}
		lc.efforts[mi.ID] = mi.Efforts
	}
	if mi.DefaultEffort != "" {
		if lc.defaults == nil {
			lc.defaults = make(map[string]string)
		}
		lc.defaults[mi.ID] = mi.DefaultEffort
	}
}

// parseGlobalModelLooseArray 动态兜底：逐对象宽松解析（上游换键名时不至于整域空列表）。
func parseGlobalModelLooseArray(arr json.RawMessage) (names []string, infos []ModelInfo, efforts map[string][]string, defaults map[string]string, err error) {
	var items []any
	if json.Unmarshal(arr, &items) != nil {
		return nil, nil, nil, nil, errors.New("global models parse: unsupported payload shape")
	}
	lc := &looseCatalog{
		names: make([]string, 0, len(items)),
		infos: make([]ModelInfo, 0, len(items)),
	}
	for _, item := range items {
		switch v := item.(type) {
		case string: // 混合数组里的裸 ID：按窄表条目处理
			if id := strings.TrimSpace(v); id != "" {
				lc.addString(id)
			}
		case map[string]any:
			if mi, ok := parseGlobalModelLoose(v); ok {
				lc.addModel(mi)
			}
		}
	}
	if len(lc.names) == 0 {
		return nil, nil, nil, nil, errors.New("global models empty list")
	}
	return lc.names, lc.infos, lc.efforts, lc.defaults, nil
}

// parseGlobalModelEntries 解析主形态（对象数组 → dynModelEntry 全字段，与 CN FetchModels
// 共用解析，零口径漂移）：id 回退链 id→modelId→model→name，disabled 剔除；顺带产出
// effort 能力桶。ok=false 表示该形态解析不出任何可用条目（键名对不上），调用方回落动态兜底。
func parseGlobalModelEntries(arr json.RawMessage) (names []string, infos []ModelInfo, efforts map[string][]string, defaults map[string]string, ok bool) {
	var entries []dynModelEntry
	if json.Unmarshal(arr, &entries) != nil {
		return nil, nil, nil, nil, false
	}
	names = make([]string, 0, len(entries))
	infos = make([]ModelInfo, 0, len(entries))
	for _, m := range entries {
		id := modelEntryID(m)
		if id == "" || m.Disabled {
			continue
		}
		names = append(names, id)
		mi := m.modelInfo()
		mi.ID = id // name 兜底形态下 id 取自 name，对齐 names 输出
		infos = append(infos, mi)
		efforts, defaults = addEffortBucket(m, id, efforts, defaults)
	}
	if len(names) == 0 {
		return nil, nil, nil, nil, false
	}
	return names, infos, efforts, defaults, true
}

// modelEntryID 按 id→modelId→model→name 回退链取模型 id。
func modelEntryID(m dynModelEntry) string {
	if m.ID != "" {
		return m.ID
	}
	if m.ModelID != "" {
		return m.ModelID
	}
	if m.Model != "" {
		return m.Model
	}
	return m.Name
}

// addEffortBucket 把单模型的 supportedEfforts/defaultEffort 写入能力桶（按需建桶）。
func addEffortBucket(m dynModelEntry, id string, efforts map[string][]string, defaults map[string]string) (map[string][]string, map[string]string) {
	if len(m.Reasoning.SupportedEfforts) > 0 {
		if efforts == nil {
			efforts = make(map[string][]string)
		}
		efforts[id] = m.Reasoning.SupportedEfforts
	}
	if d := m.Reasoning.DefaultEffort; d != "" {
		if defaults == nil {
			defaults = make(map[string]string)
		}
		defaults[id] = d
	}
	return efforts, defaults
}

// resolveGlobalModelsArray 从 payload 定位模型数组：payload 本身是数组直接用；
// 是 map 则依次尝试 models/items/list/data/result 键（递归下钻，取第一个能解析出
// 数组的分支）。找不到数组 → false。
func resolveGlobalModelsArray(payload json.RawMessage) (json.RawMessage, bool) {
	trimmed := strings.TrimSpace(string(payload))
	if strings.HasPrefix(trimmed, "[") {
		return payload, true
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(payload, &obj) != nil {
		return nil, false
	}
	for _, key := range []string{"models", "items", "list", "data", "result"} {
		if nested, ok := obj[key]; ok {
			if arr, ok2 := resolveGlobalModelsArray(nested); ok2 {
				return arr, true
			}
		}
	}
	return nil, false
}

// parseGlobalModelLoose 单模型对象的宽松解析（多信封兜底路径）：id 依次回退
// id/modelId/model/name；窗口/上限键回退 contextWindow/maxTokens；reasoning 档位
// defaultEffort 新键优先、effort 老键兜底。disabled 剔除。Credits 恒不解析
// （PLAN §3.D2：倍率不进 global 路径）。
func parseGlobalModelLoose(obj map[string]any) (ModelInfo, bool) {
	id := looseModelID(obj)
	if id == "" {
		return ModelInfo{}, false
	}
	if disabled, ok := obj["disabled"].(bool); ok && disabled {
		return ModelInfo{}, false
	}
	mi := ModelInfo{
		ID:             id,
		Name:           looseString(obj, "name"),
		ContextWindow:  looseInt(obj, "maxInputTokens", "contextWindow"),
		MaxTokens:      looseInt(obj, "maxOutputTokens", "maxTokens"),
		MaxAllowedSize: looseInt(obj, "maxAllowedSize"),
	}
	mi.SupportsReasoning, _ = obj["supportsReasoning"].(bool)
	mi.SupportsImages, _ = obj["supportsImages"].(bool)
	fillLooseReasoning(&mi, obj)
	return mi, true
}

// looseString 取 obj[key] 的字符串并去空白。
func looseString(obj map[string]any, key string) string {
	s, _ := obj[key].(string)
	return strings.TrimSpace(s)
}

// looseModelID 按 id→modelId→model→name 回退链取宽松解析的模型 id。
func looseModelID(obj map[string]any) string {
	id := looseString(obj, "id")
	for _, k := range []string{"modelId", "model", "name"} {
		if id != "" {
			break
		}
		id = looseString(obj, k)
	}
	return id
}

// looseInt 按 keys 顺序取首个正数的数值字段（float64 → int64），全缺返回 0。
func looseInt(obj map[string]any, keys ...string) int64 {
	for _, k := range keys {
		if n, ok := obj[k].(float64); ok && n > 0 {
			return int64(n)
		}
	}
	return 0
}

// fillLooseReasoning 宽松解析 reasoning 子对象：defaultEffort 新键优先、effort 老键
// 兜底；canDisableThinking；supportedEfforts 数组去空白。
func fillLooseReasoning(mi *ModelInfo, obj map[string]any) {
	r, ok := obj["reasoning"].(map[string]any)
	if !ok {
		return
	}
	mi.DefaultEffort = looseString(r, "defaultEffort")
	if mi.DefaultEffort == "" {
		mi.DefaultEffort = looseString(r, "effort") // 老模型键兜底，与 CN 侧同款
	}
	mi.CanDisableThinking, _ = r["canDisableThinking"].(bool)
	arr, ok := r["supportedEfforts"].([]any)
	if !ok {
		return
	}
	for _, item := range arr {
		if s, ok := item.(string); ok {
			if s = strings.TrimSpace(s); s != "" {
				mi.Efforts = append(mi.Efforts, s)
			}
		}
	}
}
