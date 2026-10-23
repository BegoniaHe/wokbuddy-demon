// Package upstream 封装对 CodeBuddy 上游（chat / billing / auth）的全部 HTTP 调用，
// 以及错误分类（驱动 pool 冷却状态机）。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/clientprofile"
	"github.com/BegoniaHe/wokbuddy-demon/internal/logfmt"
)

var (
	errDoJSONLimitFailed            = errors.New("doJSONLimit failed")
	errNoRefreshToken               = errors.New("no refreshToken")
	errRefreshFailedNoAccessTokenIn = errors.New("refresh_failed: no accessToken in response — re-login required")
	errModelsAPIReturnedEmptyList   = errors.New("models api returned empty list")
	errFetchEnterpriseModelsFailed  = errors.New("fetchEnterpriseModels failed")
	errParseEnterpriseModelsFailed  = errors.New("parseEnterpriseModels failed")
	errNoCLIAgentModelsFound        = errors.New("no cli agent models found")
	errV3ConfigReturnedEmptyModels  = errors.New("v3/config returned empty models")
	errFetchV3ConfigModelMapFailed  = errors.New("fetchV3ConfigModelMap failed")
	errParseV3ConfigEnvFailed       = errors.New("parseV3ConfigEnv failed")
	errFlexInt64Invalid             = errors.New("flexInt64: 容量值无法解析")
)

// ErrKind 错误分类，pool 据此决定冷却时长。
type ErrKind int

const (
	ErrNone           ErrKind = iota // 成功
	ErrHardCredit                    // 余额不足（402 或 body 关键词）→ 长冷却
	ErrSoftRate                      // 429 软限流 → 短冷却
	ErrSessionDead                   // 401 + 12153 offline session 失效 → 禁用
	ErrNotFound                      // 404 上游偶发 → 短冷却，不累计错误计数（防雪崩）
	ErrServer                        // 5xx 上游故障
	ErrContentBlocked                // 内容策略拦截（400 + 审核文案）→ 不罚账号，走降级重试
	ErrBadParams                     // 请求体解析失败（400 + Unmarshal chat params failed / 11101）→ 请求级错误：不罚号、不轮转，末端 400 透传原文
	ErrAccountFault                  // 账号级授权/配额故障（11140 request illegal / 14017 trial not activated）→ 冷却轮换，不无限重试
	ErrModelBlocked                  // 11136 账号缺该模型权限 → (账号,模型) 负缓存避让，换号重试
	ErrModelNotFound                 // 11102 上游模型目录无此模型 → 请求级 fail-fast：不罚号、不轮转、不写路由熔断
	ErrModelAccess                   // 11141 模型已下线 → 请求级 fail-fast + (账号,模型) 负缓存避让
	ErrWafBlock                      // 403 + 非业务信封体（APISIX WAF 拦截页/空体）→ 账号软冷却 + 抖动退避
	ErrPromptTooLong                 // 11115「prompt is too long」→ 请求级错误（上下文超限是请求的问题非账号的问题）：不罚号、不轮转，末端透传原文
	ErrImageInvalid                  // 图片请求格式/数据无效 → 请求级错误：不罚号、不轮转，末端透传原文
	ErrQueueWaiting                  // 6020 排队等待 → 可重试，携带 Retry-After，不换号
	ErrQueueFull                     // 6021/6022 队列满/用户并发上限 → 不可自愈，不轮转
	ErrClient                        // 其他 4xx / 业务错误
	// ErrChannelRefused 11-128 通道校验失败（`11-128 + request illegal`）：问题在
	// 客户端/通道一侧（UA、头、中间代理或入站形态），账号本身健康 → 不罚号、
	// 不降级重试，按通道级处理并继续轮转。追加在末尾以保持既有枚举值稳定。
	ErrChannelRefused
)

// errKindNames 错误类别的字符串名（未知类别回落 "none"）。
var errKindNames = map[ErrKind]string{
	ErrNone:           "none",
	ErrHardCredit:     "hard_credit",
	ErrSoftRate:       "soft_rate",
	ErrSessionDead:    "session_dead",
	ErrNotFound:       "not_found",
	ErrServer:         "server",
	ErrContentBlocked: "content_blocked",
	ErrBadParams:      "bad_params",
	ErrModelBlocked:   "model_blocked",
	ErrModelNotFound:  "model_not_found",
	ErrModelAccess:    "model_access",
	ErrWafBlock:       "waf_block",
	ErrPromptTooLong:  "prompt_too_long",
	ErrImageInvalid:   "image_invalid",
	ErrAccountFault:   "account_fault",
	ErrQueueWaiting:   "queue_waiting",
	ErrQueueFull:      "queue_full",
	ErrClient:         "client",
	ErrChannelRefused: "channel_refused",
}

func (k ErrKind) String() string {
	if name, ok := errKindNames[k]; ok {
		return name
	}

	return "none"
}

// Error 带分类的上游错误。
type Error struct {
	Kind   ErrKind
	Status int
	Msg    string
	// Code 响应信封里的首个结构化业务码（code/bizCode，顶层优先，再 data/error）。
	// Msg 会被截断到展示长度，结构化判定（签到幂等、trial 已领取等）必须依赖该字段；
	// 空串表示响应没有可解析的业务码，调用方再回落 Msg 文本匹配。
	Code string
	// RetryAfter 上游明示的等待时长（Retry-After 秒 / retry-after-ms /
	// x-ratelimit-reset 头解析，见 ParseRetryAfter）。零值 = 上游未明示，
	// 冷却时长回落调用方计算值。挂载点选在 Error 信封：Kind 决定「罚不罚」，
	// RetryAfter 决定「罚多久」，同为上游响应的一等公民。
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	return fmt.Sprintf("upstream %s (http %d): %s", e.Kind, e.Status, e.Msg)
}

// hardMarkers 余额不足关键词（小写比较 + 中文原文比较双通道）。
var hardMarkers = []string{
	"insufficient credit", "no credit", "credit exhausted", "credits exhausted", "out of credit",
	"quota exceeded", "quota exhaust", "payment required", "credit not enough",
	"not enough credit",
	"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "没有积分",
	"余额已耗尽", "额度已达上限",
}

// softRateMarkers 限流/节流关键词（小写比较 + 中文原文比较双通道）。
// 上游在状态码非 429 时也会返回限流语义（如 200 + code 11140
// "The model provider is rate-limiting requests."、400 + "rate limit"），
// 此类响应若不识别，账号既不被冷却也不喂熔断，下次请求仍会被选中（issue #28）。
//
// 词表按子串匹配，宁缺毋滥：只收录明确指向「请求速率/模型用量被节流」的措辞。
// 连字符形式（rate-limiting / rate-limited）需单列——Contains 不跨 '-'。
// "too many" 会命中 "too many tokens" 这类客户端参数错误，代价是该号被软冷却
// 一个 SoftCooldown（默认 60s）后自愈，远小于漏判限流导致反复选中同一号的代价。
var softRateMarkers = []string{
	"rate limit", // rate limit / rate limits / rate limiting
	"rate-limiting",
	"rate-limited",
	"too many requests",
	"too many",
	"usage limit", // usage limit reached / model usage limit exceeded（用量节流，非计费余额）
	"请求过于频繁", "限流", "用量已达上限",
}

var sessionDeadMarkers = []string{"Offline user session not found", "12153"}

// accountFaultMarkers 账号级授权/配额故障关键词（大小写不敏感子串匹配）。
//
// 定位：这类错误是**账号本身状态**决定的本机故障，不是请求格式、不是临时限流、
// 也不是内容误报——继续重试只会反复刷上游风控/配额检查，必须把该账号冷却轮换。
//   - code 14017（"trial not activated" / "The trial version is not yet activated"）→
//     上游 quota/quota_not_activated，register 未完成的试用未激活账号，同样账号级。
//
// 裸 "request illegal"（无 11140 code）**不在**本表：它此前会把普通请求错误升级成
// 硬禁用——上游本轮专门删掉了这条过宽 marker。账号受限改由 IsAccountRestricted
// （11140 **且** 该文案）单独判定，见其文档。
// 14017 文案唯一（无软限流歧义），可安全收录。
var accountFaultMarkers = []string{
	"trial not activated",
	"trial version is not yet activated",
}

// accountRestrictedCode 账号级授权封禁码（auth/auth_forbidden）。
const accountRestrictedCode = "11140"

// accountRestrictedMsg 「账号受限」的确定性文案。
const accountRestrictedMsg = "request illegal"

// channelRefusedCode 11-128 通道校验失败码。上游 code 字段带连字符，常见形态为
// `{"code":"11-128",...}`（部分版本是非法 JSON），因此按字节子串而不是 JSON 字段判定。
const channelRefusedCode = "11-128"

// IsChannelRefused 报告 body 是否是「11-128 通道校验失败」形态：码命中 11-128
// **且**文案含 request illegal。通道校验问题在客户端/通道一侧（UA、头、中间代理、
// 入站形态），账号健康，不该罚号也不该走内容拦截的降级重试。
func IsChannelRefused(body string) bool {
	return strings.Contains(body, channelRefusedCode) &&
		containsAnyDual(strings.ToLower(body), body, []string{accountRestrictedMsg})
}

// IsAccountRestricted 报告 body 是否是「账号受限」（11140 + request illegal）：
// 上游 auth/auth_forbidden，需人工在 WorkBuddy 提交反馈申诉后重新登录。
//
// 11140 **不能**只按 code 判定：该 code 也承载模型级限流文案（"The model provider
// is rate-limiting requests."），那种场景必须保持 ErrSoftRate。反过来也不能只按
// 文案判定：裸 "request illegal" 会命中普通请求错误。两者与（AND）才成立。
func IsAccountRestricted(body string) bool {
	return hasBusinessCode(body, accountRestrictedCode) &&
		containsAnyDual(strings.ToLower(body), body, []string{accountRestrictedMsg})
}

// IsUnconfirmedSafetyReview 报告 11140 + "content failed safety review" 形态。
// 该文案来自同类实现实证，官方包内未确认，因此**只记录日志**、不扩 contentBlocked
// marker（issue #246）。
func IsUnconfirmedSafetyReview(body string) bool {
	return hasBusinessCode(body, accountRestrictedCode) &&
		strings.Contains(strings.ToLower(body), "content failed safety review")
}

// contentBlockedMarkers 内容策略拦截关键词（大小写不敏感子串匹配）。
//
// 定位：上游按逐字精确指纹审核，system 来源的模板句（如 Claude Code/Codex
// 注入指令）触发 HTTP 400 + 以下文案。这是「误报」（合法流量被审核误杀），
// 非账号问题——该账号余额健康、未限流、session 未死，故 ErrContentBlocked
// 在 applyErrorPolicy 中不罚账号（无冷却/熔断/NoteError），改由网关降级重试。
var contentBlockedMarkers = []string{
	"blocked by security policy",
	"unapproved channel",
	"illegal api invocation",
}

// badParamsMarkers 请求体解析失败关键词（issue #41 连带）：HTTP 400 + 上游
// "Unmarshal chat params failed..."（code 11101）。这是"发给上游的 body 有问题"，
// 与账号健康无关——不罚号，但仍轮转（commit B）。
var badParamsMarkerMsg = "Unmarshal chat params failed"

// invalidImageMarkers 图片请求格式/数据无效（HTTP 400）的**文案**形态。这类错误由
// 请求内容决定，不是账号问题：换账号不会改变同一 body 的解析结果。上游常见形态包括
// `Parse message failed: invalid image_url content`、invalid_image_data、
// `replace the image`。
//
// 业务码 11135 不放在这里：code 判定必须容忍 JSON 空白（`"code": 11135`），
// 字面量 marker 只能覆盖紧凑形态，故统一走 codeMarker（见 Classify 的 400 分支，
// 与 hint.go 的 isInvalidImageData 同口径；上游 5d5223d 的 Copilot review 修复）。
var invalidImageMarkers = []string{
	"invalid image_url content",
	"invalid_image_data",
	"replace the image",
}

// 定位：上下文超限是**请求的问题不是账号的问题**——同一个 body 换任何账号发都会
// 超限，与 WAF fail-fast 同哲学（确定与账号无关的错误不罚号不轮转，白白浪费健康号
// 的请求配额）。marker 双通道：
//   - `"code":11115`：业务信封 code 字段（JSON 空格容差；`"code":"11115"` 字符串
//     形态也命中）；
//   - "prompt is too long"：msg 文案（大小写不敏感）。
//
// 只在 400/404/413 请求级状态码上判（429+11115 概率极低且属限流语义优先，
// 5xx 属服务端故障优先）。误判代价（好 body 被归 prompt_too_long）：不罚号 +
// 不轮转 + 透传原文，客户端看到上游原文可自行排查，代价可控。
var promptTooLongMarkers = []string{
	`"code":11115`,
	`"code": 11115`,
	`"code":"11115"`,
	"prompt is too long",
}

// isPromptTooLongStatus 11115 只在请求级 4xx 上判（见 promptTooLongMarkers 注释）。
func isPromptTooLongStatus(status int) bool {
	return status == http.StatusBadRequest || status == http.StatusNotFound ||
		status == http.StatusRequestEntityTooLarge
}

// alreadyCheckinMarkers "今天已签到"关键词（上游对重复签到返回 code!=0，
// 实测 code=10001/14001 "今天已签到"/"今日已签到"）。只对 *Error.Msg 做包含匹配，
// 网络层/解析层错误不在此识别（见 IsAlreadyCheckin）。
var (
	alreadyCheckinMarkers = []string{"已签到", "already"}
	// inactiveCheckinMarkers "活动未开启/未开放/已结束"等非错误态关键词（上游对未开放
	// 的签到活动返回 code!=0 + 明确文案）。保守取带「活动」上下文的显式短语：未命中即判
	// 真失败，避免把真失败误当 inactive 而漏报；也不误伤凭证类文案（如「登录态已过期」
	// 含「已过期」但非活动态）。文案按实测补充（见 issue #27）。
	inactiveCheckinMarkers = []string{
		"活动未开启", "活动未开放", "活动已结束", "活动已过期",
	}
	badParamsMarkerCode = `"code":11101`
)

// softRateResetLoc 上游 429 6004 文案中的重置时间固定按 UTC+8 解释（上游文案如此，
// 与容器时区无关）。
var softRateResetLoc = time.FixedZone("UTC+8", 8*60*60)

// modelRateLimitCode 明确指向「模型级 429 限流」的业务 code。
// 上游用它表达"该模型的使用量超限"（code 6004，msg 带「将在 … 重置」），
// 而不是账号整体被限流——账号健康，只是这个模型此刻被限（issue #31）。
const modelRateLimitCode = "6004"

// modelBusyCode 上游「当前模型繁忙」业务码：只停 (账号, 模型) 短窗口。
const modelBusyCode = "14003"

// softRateResetPatternCN/EN 匹配重置文案（CN「将在 … 重置」/ global 域英文
// "reset at <固定格式时间>"），捕获中间的时间串。
const (
	softRateResetPatternCN = `将在 (.+?) 重置`
	softRateResetPatternEN = `(?i)reset at (\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2})`
)

// 限流判定正则预编译为包级 var：IsModelRateLimit / ParseRateReset 在每次错误
// 分类、每个限流 body 上调用，函数体内 MustCompile 是纯浪费；错误风暴（429
// 轰炸）时尤甚。模式串均为纯常量。regexp 并发安全（匹配只读），无需额外锁。
var (
	reSoftRateResetCN = regexp.MustCompile(softRateResetPatternCN)
	reSoftRateResetEN = regexp.MustCompile(softRateResetPatternEN)
)

// softRateTimeLayout 上游重置时间的格式（无时区后缀；时区固定 UTC+8）。
const softRateTimeLayout = "2006-01-02 15:04:05"

// IsModelRateLimit 报告 429 body 是否明确指向模型级限流（业务 code 6004）
// 或模型繁忙（14003）。用于区分"账号级软限流"（按账号冷却）与"模型级限流"
// （切模型即可用；14003 另走短窗口，见 IsModelBusy）。
func IsModelRateLimit(body string) bool {
	// 结构化 code 字段精确匹配 6004/14003，避免 60040/140030 这类前缀误伤。
	return hasAnyBusinessCode(body, modelRateLimitCode, modelBusyCode)
}

// IsModelBusy 报告 body 是否是 14003「当前模型繁忙」：只停该模型短窗口。
func IsModelBusy(body string) bool {
	return hasBusinessCode(body, modelBusyCode)
}

// modelBlockCode 明确指向「该后端无此模型」的业务 code。
const modelBlockCode = "11102"

// modelBlockMsgMarker 11102 答复的确定性文案（官方 error message 固定短语）。
// 只收这个窄短语，不收 "model ... not found" 宽正则——后者会误伤其他业务的
// not found 措辞。
const modelBlockMsgMarker = "service info not found"

// ModelBlockReason 11136 负缓存条目在 pool.modelCooldowns 里的 reason 前缀。
// handler 写 BlockModelBackoff；pool.BlockModelClear 按 "11136"/"11158"/"11141"
// 前缀识别可恢复的模型避让条目（与 6004 条目的 "6004 model rate limit" reason
// 互不干扰）。
const ModelBlockReason = "11136 model not permitted"

// ModelForbiddenReason 11158 负缓存条目的 reason 前缀（账号类型级模型权限拒绝：
// `model [x] is not allowed for user type [y]`）。与 ModelBlockReason 分开记录，
// 让面板/排障能区分「该账号无该模型权限」与「该账号类型不允许该模型」。
const ModelForbiddenReason = "11158 model not allowed for user type"

// modelForbiddenMsgMarker 11158 答复的确定性文案。上游丢 code 时的外卡判据；
// 只收这个窄短语，不收 "not allowed" 宽匹配（会命中任意权限类措辞）。
const modelForbiddenMsgMarker = "is not allowed for user type"

// hasModelForbiddenMarker 报告 body 是否带 11158 的窄文案。
func hasModelForbiddenMarker(body string) bool {
	return body != "" && strings.Contains(strings.ToLower(body), modelForbiddenMsgMarker)
}

// ModelBlockReasonFor 返回模型权限拒绝 body 对应的负缓存 reason 前缀：11158（账号
// 类型级）优先于 11136（账号级）。两者都是 (账号,模型) 维度的可恢复避让，仅台账
// 文案不同，因此共用 BlockModelBackoff 的同一条通道。
func ModelBlockReasonFor(body string) string {
	if hasBusinessCode(body, "11158") || hasModelForbiddenMarker(body) {
		return ModelForbiddenReason
	}

	return ModelBlockReason
}

// IsModelBlocked 报告 body 是否是「上游模型目录无此模型」(11102) 的确定性答复。
//
// 只比对 code/msg 等独立字段，绝不做整段文本子串匹配：错误体还带 requestId 等字段，
// 拿整段文本匹配会把 "11102" 撞在 ID 上、误判一个本来能用的模型。判定 =
// code 字段精确等于 "11102"，或 msg/message 字段命中窄短语 "service info not
// found"（两者任一命中即真）。只看 400/404：429 带 11102 属限流语义。
// 字段遍历覆盖顶层与 error 子对象两层。
func IsModelBlocked(status int, body string) bool {
	if (status != http.StatusBadRequest && status != http.StatusNotFound) || body == "" {
		return false
	}
	// 轻量预检：body 既无 "11102" 又无 marker 时直接短路（大多数 4xx 零分配返回）。
	if !strings.Contains(body, modelBlockCode) && !strings.Contains(strings.ToLower(body), modelBlockMsgMarker) {
		return false
	}

	var root map[string]any
	if err := json.Unmarshal([]byte(body), &root); err != nil {
		return false
	}

	code, msg := modelBlockFields(root)
	if code == modelBlockCode {
		return true
	}

	return strings.Contains(strings.ToLower(msg), modelBlockMsgMarker)
}

// modelBlockFields 从顶层与 error 子对象两层抽取 code（code/bizCode/errCode/
// error_code，取首个非空）与 msg（msg/message，取首个非空）。用于 IsModelBlocked
// 的确定性判定。
func modelBlockFields(root map[string]any) (code, msg string) {
	nodes := []map[string]any{root}
	if inner, ok := root["error"].(map[string]any); ok {
		nodes = append(nodes, inner)
	}

	for _, node := range nodes {
		code = firstCodeField(node, code)
		msg = firstMsgField(node, msg)
	}

	return code, msg
}

// firstCodeField 返回 node 中首个非空 code 字段（code/bizCode/errCode/error_code 按序），
// current 已非空时原样返回（跨节点只取首个）。
func firstCodeField(node map[string]any, current string) string {
	if current != "" {
		return current
	}

	for _, key := range []string{"code", "bizCode", "errCode", "error_code"} {
		if v, ok := node[key]; ok && v != nil {
			// 空串（含纯空白）不算命中：继续尝试同节点内的后备键，与旧内联循环
			// 「code 仍为空则继续」的口径一致（否则 {"code":"","errCode":"11102"}
			// 会停在空 code，丢掉 11102 判定）。
			if s := strings.TrimSpace(fmt.Sprint(v)); s != "" {
				return s
			}
		}
	}

	return current
}

// firstMsgField 返回 node 中首个非空字符串 msg 字段（msg/message 按序），current
// 已非空时原样返回（跨节点只取首个）。
func firstMsgField(node map[string]any, current string) string {
	if current != "" {
		return current
	}

	for _, key := range []string{"msg", "message"} {
		if v, ok := node[key].(string); ok && v != "" {
			// 纯空白串 TrimSpace 后为空不算命中：继续尝试同节点内的后备键，与旧内联
			// 循环「msg 仍为空则继续」的口径一致。
			if s := strings.TrimSpace(v); s != "" {
				return s
			}
		}
	}

	return current
}

// businessCodeFields 业务码字段名。code 是主字段；bizCode 是客户端
// parseErrorMeta 的同义字段，协议漂移时避免静默漏判。
var businessCodeFields = []string{"code", "bizCode"}

// businessCode 从 JSON 错误信封提取首个结构化业务码（顶层优先，再 data/error/
// response，深度有界防病态嵌套）。返回空串表示响应没有可解析的业务码。
// 与 hasBusinessCode 的「整棵树找指定码」不同，这里只需稳定拿到一个代表码，
// 供签到/trial 等消费方在 Msg 被截断后仍能按码判定。
func businessCode(body string) string {
	var root any
	if err := json.Unmarshal([]byte(body), &root); err != nil {
		return ""
	}

	return findBusinessCode(root, 3)
}

// findBusinessCode 递归抽取第一个非空 code/bizCode。
func findBusinessCode(value any, depth int) string {
	if depth < 0 {
		return ""
	}

	switch node := value.(type) {
	case map[string]any:
		if code := firstCodeField(node, ""); code != "" {
			return code
		}

		for _, key := range []string{"data", "error", "response"} {
			if code := findBusinessCode(node[key], depth-1); code != "" {
				return code
			}
		}
	case []any:
		for _, child := range node {
			if code := findBusinessCode(child, depth-1); code != "" {
				return code
			}
		}
	}

	return ""
}

// hasBusinessCode 报告 JSON 错误信封是否携带指定业务码。信封可能在顶层、
// error 或 data（含 error.data）下，递归遍历解码后的结构。
func hasBusinessCode(body, want string) bool {
	return hasAnyBusinessCode(body, want)
}

// hasAnyBusinessCode 报告 body 是否命中 wants 中任一业务码。先做子串预检，
// 大多数不含目标码的错误体零 JSON 解析开销。
func hasAnyBusinessCode(body string, wants ...string) bool {
	if body == "" || len(wants) == 0 {
		return false
	}

	if !slices.ContainsFunc(wants, func(want string) bool { return strings.Contains(body, want) }) {
		return false
	}

	var root any
	if err := json.Unmarshal([]byte(body), &root); err != nil {
		return false
	}

	var walk func(any) bool

	walk = func(value any) bool {
		switch node := value.(type) {
		case map[string]any:
			if nodeHasBusinessCode(node, wants) {
				return true
			}

			for _, child := range node {
				if walk(child) {
					return true
				}
			}
		case []any:
			return slices.ContainsFunc(node, walk)
		}

		return false
	}

	return walk(root)
}

// nodeHasBusinessCode 报告节点自身是否携带 wants 中任一业务码。
func nodeHasBusinessCode(node map[string]any, wants []string) bool {
	for _, key := range businessCodeFields {
		code, ok := node[key]
		if !ok || code == nil {
			continue
		}

		if slices.Contains(wants, strings.TrimSpace(fmt.Sprint(code))) {
			return true
		}
	}

	return false
}

// hasBusinessEnvelope 报告错误 body 是否携带上游业务信封形态（JSON 且含
// `"code":` 或 `"msg":` 字段）。WAF 403 判定（IsWafBlocked）用「无业务信封」
// 区分 APISIX WAF 拦截页（HTML/空体/纯文本）与上游业务层 403（带 code/msg
// 信封，正常走既有分类）。JSON 解析不做：信封存在性只需字段名命中——
// 畸形 JSON 但含 `"msg":` 字样仍按业务响应保守处理（宁漏判 WAF 也不误罚
// 业务 403，后者有各自的权威分类）。
func hasBusinessEnvelope(body string) bool {
	return strings.Contains(body, `"code":`) || strings.Contains(body, `"msg":`)
}

// IsWafBlocked 报告 403 响应是否为 WAF 拦截形态：HTTP 403 且 body 无业务信封
// （无 `"code":`/`"msg":` JSON 字段——HTML 拦截页、空体、纯文本均命中）。
// 带业务信封的 403（11140 request illegal / 11-128 等）仍走既有分类链。
// 403 含 accountFault 文案的维持现状（ErrAccountFault），由 Classify 的规则序保证。
func IsWafBlocked(status int, body string) bool {
	return status == http.StatusForbidden && !hasBusinessEnvelope(body)
}

// retryAfterHeaderCandidates 冷却时长优先解析的响应头候选序列：
// retry-after（秒，RFC 7231）/ retry-after-ms（毫秒）/ x-ratelimit-reset
// （epoch 秒或毫秒，取 now+ 剩余量）。大小写不敏感（http.Header.Get 已归一）。
var retryAfterHeaderCandidates = []string{"Retry-After", "Retry-After-Ms", "X-Ratelimit-Reset"}

// retryAfterSanity 解析结果的上限（超过视为上游异常值丢弃，回落本地计算），
// 与 pool 的 softRateMax 默认 2h 同量级。
const retryAfterSanity = 2 * time.Hour

// ParseRetryAfter 从限流/拦截响应头解析上游明示的等待时长：
// 依次尝试 Retry-After（整数秒）→ retry-after-ms（整数毫秒）→
// x-ratelimit-reset（纯数字按 epoch 秒/毫秒推断；HTTP-Date 形态不支持——
// 上游族实践发的是数字）。任一头缺失/非法/非正/超上限则尝试下一头；
// 全部不可用返回 false（调用方回落既有计算值，绝不臆造等待时长）。
func ParseRetryAfter(h http.Header) (time.Duration, bool) {
	for _, name := range retryAfterHeaderCandidates {
		str := strings.TrimSpace(h.Get(name))
		if str == "" {
			continue
		}

		if !isAllDigits(str) {
			continue // 非纯数字（如 HTTP-Date）不解析，宁缺毋滥
		}

		n, ok := parseRetryNumber(str, name)
		if !ok {
			continue
		}

		if n <= 0 || n > retryAfterSanity {
			continue // 非正/异常大：丢弃（回落本地计算）
		}

		return n, true
	}

	return 0, false
}

// isAllDigits 报告 s 是否为纯数字（前置快筛，免 strconv 之后再判语义）。
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}

	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}

	return true
}

// parseRetryNumber 按头名口径把纯数字串折算成时长。x-ratelimit-reset 是
// epoch 时刻而非时长：秒口径（10 位）与毫秒口径（13 位）都按「now+ 该时刻
// 的剩余量」折算，已在过去则不可用。位数不足（8 位以下）无法判定 epoch
// 语义的丢弃（宁缺毋滥：短串多半是序号之类的误用头）。
func parseRetryNumber(str, headerName string) (time.Duration, bool) {
	// 上限 16 位防 int64 溢出（超过 epoch 毫秒的现实量级必非法）。
	if len(str) > 16 {
		return 0, false
	}

	var n int64
	for _, r := range str {
		n = n*10 + int64(r-'0')
	}

	switch headerName {
	case "Retry-After":
		// 先做上限校验再乘 time.Second：16 位数字乘 1e9 会溢出 int64 回绕成
		// 小正数，进而通过调用方的 retryAfterSanity 校验被当作合法等待时长。
		if n > int64(retryAfterSanity/time.Second) {
			return 0, false
		}

		return time.Duration(n) * time.Second, true
	case "Retry-After-Ms":
		if n > int64(retryAfterSanity/time.Millisecond) {
			return 0, false
		}

		return time.Duration(n) * time.Millisecond, true
	default: // X-Ratelimit-Reset：epoch → 剩余量
		sec := n
		if len(str) >= 12 { // 毫秒口径（13 位）；11 位边界按秒（误判代价是多算 1000 倍）
			sec = n / 1000
		}

		remain := time.Until(time.Unix(sec, 0))

		return remain, true
	}
}

// ParseRateReset 从任何限流响应 body 里统一解析「将在 … 重置」时间（上游 UTC+8 文案）。
// 成功返回解析出的**墙钟时刻**（按 UTC+8 解释），失败返回零值 + false。
//
// 是否走模型级豁免、时日对齐到 until 还是 modelCooldowns，由冷却决策侧（pool）按
// IsModelRateLimit 判定，本函数只负责「把上游明说的恢复时刻抽出来」。没有时间文案
// 的限流也照常由调用方退回有界退避（绝不臆造时间）。
func ParseRateReset(body string) (time.Time, bool) {
	// CN 文案优先；global 域 429 body 是英文形态（"will reset at YYYY-MM-DD HH:MM:SS
	// UTC+8"），此前只认中文 → global 限流解析不到恢复时刻，退回有界退避基数反复
	// 翻倍（修「global 域冷却指数翻倍」）。英文正则锚定固定格式时间，自然语言
	// （"reset at the end of the day"）不匹配。
	m := reSoftRateResetCN.FindStringSubmatch(body)
	if len(m) < 2 {
		m = reSoftRateResetEN.FindStringSubmatch(body)
	}

	if len(m) < 2 {
		return time.Time{}, false
	}

	ts := strings.TrimSpace(m[1])
	ts = strings.TrimSuffix(ts, " UTC+8") // 去掉后缀，固定按 softRateResetLoc 解释

	t, err := time.ParseInLocation(softRateTimeLayout, ts, softRateResetLoc)
	if err != nil {
		return time.Time{}, false
	}

	return t, true
}

// classifyEarly 判定优先级最高的几类：模型不存在、402、11-128 内容拦截、
// 客户端明确的请求级业务码。顺序即优先级。
//
//nolint:cyclop // 分类优先级线性展开。
func classifyEarly(status int, body string) (ErrKind, bool) {
	// 11136/11158 是账号级模型权限缺失：换一个账号可能就有权限，先判（与 11102 的
	// 平台级「目录无此模型」分开）。11158 另留文案外卡：上游丢 code 时仍按账号类型
	// 级权限拒绝处理，不落 ErrClient 逐号连败。
	if status != http.StatusTooManyRequests &&
		(hasAnyBusinessCode(body, modelAccessCodes...) || hasModelForbiddenMarker(body)) {
		return ErrModelBlocked, true
	}
	// 11141 平台下线同理：429 下保留限流语义。
	if status != http.StatusTooManyRequests && hasAnyBusinessCode(body, modelRetiredCodes...) {
		return ErrModelAccess, true
	}
	// 11102「上游模型目录无此模型」：所有账号共享同一目录，换号必然得到同一答复。
	// 归 ErrModelNotFound 请求级 fail-fast——若等到通用 4xx 兜底归 ErrClient 会逐号
	// 轮转，模型不存在时把一次失败放大 MaxRotate 倍。
	// 只认 400/404（见 IsModelBlocked），429+11102 落 status==429 层走限流语义。
	if IsModelBlocked(status, body) {
		return ErrModelNotFound, true
	}
	// 402：真正的计费余额耗尽状态码，最严、最不可自愈，最先判。
	if status == http.StatusPaymentRequired {
		return ErrHardCredit, true
	}
	// 11-128 有两种语义，必须先分流：`11-128 + request illegal` 是**通道校验失败**
	// （问题在客户端/通道一侧，账号健康、不该降级重试也不该罚号）；其余 11-128 才是
	// 上游内容审核/反探测拦截（常见形态是 code 字段带连字符 `"code":"11-128"`），归
	// ErrContentBlocked 走降级重试（与出站 sanitize 同语义）。
	if IsChannelRefused(body) {
		return ErrChannelRefused, true
	}

	if strings.Contains(body, channelRefusedCode) {
		return ErrContentBlocked, true
	}

	// 11133 model_param_invalid：官方文案是「模型无法处理此请求」，同时被 GIF
	// 不支持复用。不能一律当账号故障：缺/空 messages 是确定性请求体错误（不轮转），
	// 图像/GIF 形态是模型能力问题（不罚号），其余按请求级处理（issue #246）。
	if status != http.StatusTooManyRequests && hasBusinessCode(body, "11133") {
		return classifyModelParamInvalid(body), true
	}

	// 客户端明确的请求级业务码先于账号/限流层：同一 body 换号结果不变。
	return classifyRequestLevel(body)
}

// classifyModelParamInvalid 细分 11133：缺/空 messages → ErrBadParams 请求级
// fail-fast；图像/GIF → ErrImageInvalid（提示换模型或移除图片）；其余按官方
// 「模型无法处理此请求」归请求级错误，不喂账号连败。
func classifyModelParamInvalid(body string) ErrKind {
	param := businessErrorParam(body)
	if param == "messages" || param == "input" {
		return ErrBadParams
	}

	lower := strings.ToLower(body)
	if strings.Contains(lower, "image") || strings.Contains(lower, "gif") {
		return ErrImageInvalid
	}

	return ErrBadParams
}

// businessErrorParam 从上游错误信封提取 extError.param / error.data.param /
// 顶层 param（只取字符串，深度有界）。
//
//nolint:cyclop // 深度有界的递归提取。
func businessErrorParam(body string) string {
	var root any
	if json.Unmarshal([]byte(body), &root) != nil {
		return ""
	}

	var walk func(any, int) string

	walk = func(value any, depth int) string {
		if depth < 0 {
			return ""
		}

		node, ok := value.(map[string]any)
		if !ok {
			return ""
		}

		for _, key := range []string{"extError", "error"} {
			if child, ok := node[key].(map[string]any); ok {
				if p, ok := child["param"].(string); ok && strings.TrimSpace(p) != "" {
					return strings.TrimSpace(p)
				}
			}
		}

		if p, ok := node["param"].(string); ok && strings.TrimSpace(p) != "" {
			return strings.TrimSpace(p)
		}

		for _, key := range []string{"data", "error", "response"} {
			if p := walk(node[key], depth-1); p != "" {
				return p
			}
		}

		return ""
	}

	return walk(root, 3)
}

// Classify 按 HTTP 状态码 + body 判定错误类别。
//
// 判定顺序自「严」到「宽」，每层的先后都有语义依据：
//  0. 11136/11102 —— 账号级模型权限缺失与平台级「目录无此模型」分开：11136 换号
//     并做 (账号,模型) 避让；11102 是请求级事实（所有账号共享同一模型目录），
//     fail-fast 不轮转（只认 400/404，429+11102 属限流语义走 status==429 层）。
//  1. 402 —— 真正的计费余额耗尽状态码，最严、最不可自愈，最先判。
//  2. 11-128 —— 上游内容审核/反探测拦截码（code 字段带连字符），按内容拦截
//     处理，不罚账号、可降级重试。
//  3. requestLevelCodes —— 客户端判为 editInput/newTask/reupload 的确定性请求
//     错误（11125/11129/.../11155、12654、14405）：不罚号、不轮转。11136/11158
//     与 11141 已在第 0 层 classifyEarly 按状态码分流（429 保留限流语义）。
//  4. sessionDeadMarkers / accountFaultMarkers / 11142 / 11140+request illegal ——
//     需要人工重登的终态或账号级授权/配额故障。若 401 body 同时含 "12153" 与 "rate limit"（如网关
//     错误页混排），归 session_dead：短冷却救不活失效 session，误判为限流会让该死号
//     留在池中反复被选中。14017 常带 429，若落到 status==429 会误归 soft_rate
//     （账号级故障等不来自愈）。11140 的 model 级限流变体因 marker 不含该文案而
//     天然落到 softRateMarkers 层，不受影响。
//  5. 队列/额度/限流业务码 —— 6020-6022、6000-6008、10105/15001、
//     14015/14016/14017、11145/14002 等结构化码。
//  6. 429 + code 14018 —— 明确的账号积分耗尽，归 ErrHardCredit（issue #175）。
//     只认结构化业务码，不靠可能跨计费/限流两界的文案猜测。
//  7. status==429 —— 限流状态码兜底（先于 hardMarkers）：429 body 高频携带
//     "quota exceeded"/"额度不足" 等跨计费/限流两界的措辞，hardMarkers 先判会把
//     限流误归 ErrHardCredit 硬冷却到次日 04:00，白扔号约 12h。状态码是比关键词
//     更权威的信号；真正的余额耗尽由 402（第 1 层）或结构化额度码捕获，
//     非 429 状态码的 quota 措辞仍走下方 hardMarkers。
//  8. hardMarkers —— 非 429 响应携带计费关键词（200 业务信封 / 403 信封等）。
//  9. softRateMarkers —— 非 429 状态码携带限流文案（issue #28 修复点）。
//     位于此处可覆盖 200/400/403/5xx 各状态码。
//  10. 11115 —— 「prompt is too long」请求级语义：判在 404/5xx 与通用 4xx 兜底
//     之前（404 上打 11115 若落 ErrNotFound 会误冷却账号——上下文超限与账号无关）。
//  11. 404 / 5xx —— 与限流无关的常规分类。
//  12. IsWafBlocked —— 403 且无业务信封（HTML 拦截页/空体/纯文本）：APISIX WAF
//     拦截形态。判在通用 4xx 兜底**之前**：此前该形态落 ErrClient → 只换号不罚 →
//     连环 403。带业务信封的 403 已被上方各层捕获，走不到本层。
//  13. 内容策略/参数错误/其他 4xx —— 通用兜底。
func Classify(status int, body string) ErrKind {
	if kind, ok := classifyEarly(status, body); ok {
		return kind
	}

	lower := strings.ToLower(body)
	// sessionDead / accountFault 先于 status==429：账号级终态等不来自愈，限流状态码
	// 不得掩盖它们（429+14017 必须 accountFault，401+12153 混排 "rate limit" 必须
	// sessionDead——此层 marker 是精确词，比限流层的大范围子串更具体，具体优先于宽泛）。
	if kind, ok := classifySessionOrAccount(lower, body); ok {
		return kind
	}

	if kind, ok := classifyQueueOrQuota(body); ok {
		return kind
	}

	if kind, ok := classifyRateLimit(status, body); ok {
		return kind
	}

	if kind, ok := classifyBillingOrRate(lower, body); ok {
		return kind
	}
	// 11115「prompt is too long」：判在 404/5xx/WAF/内容策略/参数错误/通用 4xx
	// 之前——请求级语义最具体（上下文超限），须先于宽泛的状态码兜底（404 兜底会
	// 误归 ErrNotFound 只冷却不透传；ErrClient 只换号，浪费健康号配额）。
	if kind, ok := classifyPromptTooLong(status, lower, body); ok {
		return kind
	}

	if kind, ok := classifyStatusFallback(status, body); ok {
		return kind
	}

	if kind, ok := classifyGeneric4xx(status, lower, body); ok {
		return kind
	}
	// HTTP 200 但业务 code 非 0 且含余额关键词的情况已被上面 hardMarkers 捕获。
	return ErrNone
}

// classifySessionOrAccount 判定 sessionDead / accountFault：账号级终态等不来自愈，
// 限流状态码不得掩盖它们（429+14017 必须 accountFault，401+12153 混排 "rate limit"
// 必须 sessionDead——此层 marker 是精确词，比限流层的大范围子串更具体）。
func classifySessionOrAccount(lower, body string) (ErrKind, bool) {
	for _, m := range sessionDeadMarkers {
		if strings.Contains(body, m) {
			return ErrSessionDead, true
		}
	}

	// 11142 auth_required：结构化账号级认证故障。与 11140 不同，该码不承载
	// 限流文案，可安全按 code 判定（429+11142 仍先于限流层归账号故障）。
	if hasBusinessCode(body, "11142") {
		return ErrAccountFault, true
	}

	// 11140 + request illegal：账号级授权封禁（auth/auth_forbidden）。**不能**只看
	// 裸文案——那样会把普通请求错误升级成硬禁用（上游已删该过宽 marker）。
	if IsAccountRestricted(body) {
		return ErrAccountFault, true
	}

	if containsAnyDual(lower, body, accountFaultMarkers) {
		return ErrAccountFault, true
	}

	return ErrNone, false
}

// 官方客户端业务码：队列、限流、额度、认证/激活。
//   - 6020 排队等待（WAITING）：可重试，带 retry_after；
//   - 6021 队列满（FULL）、6022 用户并发上限（USER_LIMIT）：不可自愈；
//   - 6000-6008 限流/窗口用量（6004 由 handler 再做模型级判定）；
//   - 10105/11145/14001/14002/14012-14014/14018/14019/15001 额度类业务码；
//     其中 10105/15001 是账号级使用上限，走软限流而非计费硬冷却。
//   - 14015 授权过期（会话失效）；14016/14017 未激活。
const (
	queueWaitingCode = "6020"
	queueFullCode    = "6021"
	queueUserCode    = "6022"
)

var hardCreditCodes = []string{
	"11145", "14001", "14002", "14012", "14013", "14014", "14018", "14019",
}

// requestLevelCodes 客户端语义为 editInput/newTask/reupload/upgradeClient 的
// 请求级业务码：同一 body 换号结果不变，必须不罚号、不轮转、透传原文。
// 11136/11141 另由 modelAccessCodes / modelRetiredCodes 处理（模型级避让）。
var requestLevelCodes = []string{
	"11125", "11129", "11130", "11132", "11137", "11138", "11139",
	"11148", "11149", "11150", "11151", "11152", "11153", "11154", "11155",
	"12654", "14405",
}

// modelAccessCodes 账号级模型权限缺失（11136「无该模型权限」、11158「账号类型
// 不允许该模型」）：同一账号换请求仍会失败，但换账号可能成功 → 写 (账号,模型)
// 负缓存后继续换号。
//
// 11158 与 11136 共用码表，但 429+11158 保留限流语义（见 classifyEarly 的
// 状态门控）：400/404 才是账号级模型权限拒绝；429 是上游限流，按软限流换号，
// 避免把限流误判成整池权限缺失。
var modelAccessCodes = []string{"11136", "11158"}

// modelRetiredCodes 平台级模型下线（11141「该模型已下线」）：所有账号都会得到
// 同一答复，换号无意义，归 ErrModelAccess 请求级 fail-fast。
var modelRetiredCodes = []string{"11141"}

// ModelAccessReason 11141 负缓存条目的 reason 前缀。pool.BlockModelClear
// 按前缀识别可恢复的模型避让条目（11136/11158 与 11141 两族），不碰 6004 限流台账。
const ModelAccessReason = "11141 model retired"

// accountLimitCodes 账号级使用上限（10105 活跃会话数、15001 联网搜索次数）。
// 它们不是计费余额耗尽：套 ErrHardCredit 会硬冷却到次日 04:00，健康账号被
// 无谓冻结十余小时；按软限流做有界冷却，窗口恢复后自愈。
var accountLimitCodes = []string{"10105", "15001"}

// rateLimitCodes 客户端限流/窗口用量族。6004 的模型级语义由
// IsModelRateLimit 单独判定，这里统一先归 ErrSoftRate。
var rateLimitCodes = []string{"6000", "6001", "6002", "6003", "6004", "6005", "6006", "6007", "6008"}

// classifyRequestLevel 判定客户端明确的请求级业务码，归 ErrBadParams
// （不罚号、不轮转、透传原文）。11136/11158/11141 已在 classifyEarly 分流。
func classifyRequestLevel(body string) (ErrKind, bool) {
	if hasAnyBusinessCode(body, requestLevelCodes...) {
		return ErrBadParams, true
	}

	return ErrNone, false
}

// QueueCode 返回 body 命中的队列业务码（"6020"/"6021"/"6022"），未命中返回空串。
func QueueCode(body string) string {
	for _, code := range []string{queueWaitingCode, queueFullCode, queueUserCode} {
		if hasBusinessCode(body, code) {
			return code
		}
	}

	return ""
}

// queueFullTextMarkers 队列满载的**文案**形态（上游实测 `msg:"queue.full.title"`）。
// 结构化 code 是主判据；本表只兜「上游换状态码或丢 code」的外卡：渠道级容量事件
// 一旦落到 ErrSoftRate，applyErrorPolicy 会逐号 CooldownSoftRate + 轮转，把一次
// 集群排队放大成整池软冷却（2026-10-15 上游事故形态）。只收 queue.full 的连写
// 形态，普通 429（"too many requests"）不受影响。
var queueFullTextMarkers = []string{"queue.full", "queue full"}

// classifyQueueOrQuota 识别官方客户端的队列/额度/认证业务码。
func classifyQueueOrQuota(body string) (ErrKind, bool) {
	switch QueueCode(body) {
	case queueWaitingCode:
		return ErrQueueWaiting, true
	case queueFullCode, queueUserCode:
		return ErrQueueFull, true
	}

	// 无结构化码时的文案外卡（状态码无关）：渠道级满载不得落 ErrSoftRate 逐号冷却。
	if containsAnyDual(strings.ToLower(body), body, queueFullTextMarkers) {
		return ErrQueueFull, true
	}

	// 限流/窗口用量族（6000-6008）：6004 的模型级语义由 handler 用
	// IsModelRateLimit 单独判定，这里统一先归 ErrSoftRate。
	if hasAnyBusinessCode(body, rateLimitCodes...) {
		return ErrSoftRate, true
	}

	// 账号级使用上限（10105 会话数、15001 联网搜索）按软限流处理：不是余额
	// 耗尽，不能套用 ErrHardCredit 的次日 04:00 硬冷却。
	if hasAnyBusinessCode(body, accountLimitCodes...) {
		return ErrSoftRate, true
	}

	if hasBusinessCode(body, "14015") {
		return ErrSessionDead, true
	}

	if hasBusinessCode(body, "14016") || hasBusinessCode(body, "14017") {
		return ErrAccountFault, true
	}

	if hasAnyBusinessCode(body, hardCreditCodes...) {
		return ErrHardCredit, true
	}

	return ErrNone, false
}

// classifyRateLimit 判定 429 状态码。14018 是明确的账号积分耗尽业务码，必须先于
// 通用 429 兜底，否则会被误判为可自愈的软限流并在全池冷却时反复兜底选中
// （issue #175）。仅按结构化 code 判定；无该 code 的 "credits exhausted" 文案仍
// 保持普通 429 的软限流语义。
//
// status==429 先于 hardMarkers：限流响应 body 高频携带 "quota exceeded"/"额度不足"
// 等跨计费/限流两界的措辞，hardMarkers 先判会把限流误归 ErrHardCredit 硬冷却到次日
// 04:00，白扔号约 12h。状态码是比关键词更权威的信号：上游既然给了 429，就按限流
// 语义处理（宁可短冷却自愈，不可长冷却弃号）；真正的余额耗尽由 402（上层）或
// 14018（上层）捕获，非 429 状态码的 quota 措辞仍走 hardMarkers（历史语义不变）。
func classifyRateLimit(status int, body string) (ErrKind, bool) {
	if status != http.StatusTooManyRequests {
		return ErrNone, false
	}

	if hasBusinessCode(body, "14018") {
		return ErrHardCredit, true
	}

	return ErrSoftRate, true
}

// classifyPromptTooLong 判定「prompt is too long」请求级错误（11115）。请求级语义
// 最具体（上下文超限），须先于宽泛的状态码兜底。
func classifyPromptTooLong(status int, lower, body string) (ErrKind, bool) {
	if isPromptTooLongStatus(status) && isPromptTooLongBody(lower, body) {
		return ErrPromptTooLong, true
	}

	return ErrNone, false
}

// classifyStatusFallback 判定 404/5xx/WAF 403 三类状态码兜底（顺序即优先级）。
//
// WAF 403（无业务信封的拦截形态）判在内容策略/参数错误/通用 4xx 之前——这些层只认
// 带文案的 body，WAF 空体/HTML 永远不会命中它们的 marker，但落 ErrClient 兜底的代价
// 是「只换号不罚」（连环 403 根因），必须在兜底前分流。带信封的 403 在上方各层已有
// 权威分类，不受影响。
func classifyStatusFallback(status int, body string) (ErrKind, bool) {
	// 上游 401（含 APISIX invalid_token HTML 页）统一归会话失效：先由调用方在同号
	// refresh 后重放一次，仍失败才累计 session dead。若落 ErrClient，死号会留在池中
	// 每个请求刷一遍 401（issue #245）。
	if status == http.StatusUnauthorized {
		return ErrSessionDead, true
	}

	if status == http.StatusNotFound {
		return ErrNotFound, true
	}

	if status >= 500 {
		return ErrServer, true
	}

	if IsWafBlocked(status, body) {
		return ErrWafBlock, true
	}

	return ErrNone, false
}

// classifyBillingOrRate 判定非 429 状态码上的计费/限流文案（hardMarkers 先于
// softRateMarkers，历史语义不变）。
func classifyBillingOrRate(lower, body string) (ErrKind, bool) {
	if containsAnyDual(lower, body, hardMarkers) {
		return ErrHardCredit, true
	}

	if containsAnyDual(lower, body, softRateMarkers) {
		return ErrSoftRate, true
	}

	return ErrNone, false
}

// containsAnyDual 报告 lower/body 是否命中 marker 列表（小写比较 + 原文比较双通道）。
func containsAnyDual(lower, body string, markers []string) bool {
	for _, m := range markers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return true
		}
	}

	return false
}

// isPromptTooLongBody 报告 body 是否命中「prompt is too long」文案（原文比较 +
// 含大写字母的 marker 追加小写比较，与既有容差口径一致）。
func isPromptTooLongBody(lower, body string) bool {
	for _, m := range promptTooLongMarkers {
		if strings.Contains(body, m) || (m != strings.ToLower(m) && strings.Contains(lower, strings.ToLower(m))) {
			return true
		}
	}

	return false
}

// isImageInvalid 报告 400 响应是否命中图片格式/数据错误：11135 业务码走
// codeMarker（JSON 空白容差），文案走 invalidImageMarkers。此类错误是确定性的
// 请求级错误：同 body 换账号结果不变，直接 fail-fast，避免把健康账号轮转一遍后
// 仍把最终 503 返回给客户端。
func isImageInvalid(status int, lower string) bool {
	if status != http.StatusBadRequest {
		return false
	}

	if codeMarker(lower, "11135") {
		return true
	}

	return containsAnyLower(lower, invalidImageMarkers)
}

// containsAnyLower 报告 lower 是否包含 markers 中任一子串（markers 均为小写）。
func containsAnyLower(lower string, markers []string) bool {
	for _, m := range markers {
		if strings.Contains(lower, m) {
			return true
		}
	}

	return false
}

// classifyGeneric4xx 判定通用 4xx 兜底：图片格式/数据错误（请求级 fail-fast）、
// 内容策略拦截（误报信号，不罚账号，由网关降级重试处理）、请求体解析失败，最后
// 归 ErrClient。status < 400 时返回 false（交由调用方收口）。
func classifyGeneric4xx(status int, lower, body string) (ErrKind, bool) {
	if isImageInvalid(status, lower) {
		return ErrImageInvalid, true
	}

	if status < http.StatusBadRequest {
		return ErrNone, false
	}
	// 内容策略拦截（HTTP 400 + 审核文案）：判在通用 ErrClient 之前。
	// 这是误报信号，不罚账号，由网关降级重试处理（见 handler.applyErrorPolicy）。
	if containsAnyLower(lower, contentBlockedMarkers) {
		return ErrContentBlocked, true
	}
	// 请求体解析失败（HTTP 400 + Unmarshal chat params failed / code 11101）：
	// 这是"发给上游的 body 有问题"。网关侧截断已由 413 消灭（issue #41 commit A），
	// 剩余来源是客户端 JSON 本身畸形——换了账号照样 400，不该罚号（白白冷却好号）。
	// 归 ErrBadParams：不冷却/不熔断/不计错，且**不轮转**——11101 发生在上游解析
	// 请求体阶段，还没走到模型路由，所以"不同账号可能有不同模型权限"其实是
	// 11136（ErrModelBlocked）的理由，那里已有 (账号,模型) 负缓存避让。
	if strings.Contains(body, badParamsMarkerMsg) || strings.Contains(body, badParamsMarkerCode) {
		return ErrBadParams, true
	}

	return ErrClient, true
}

// apiEnvelope 上游统一信封。
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// Client 上游 HTTP 客户端。Base 字段可覆盖便于测试。
type Client struct {
	HTTP *http.Client

	// ChatHTTP 聊天 SSE 专用 client：无总时长上限（Timeout=0），首字节由
	// Transport.ResponseHeaderTimeout 约束，流中空闲由 IdleTimeout 约束。
	// 与 HTTP 共享同一个 *http.Transport 实例，连接池不重复。
	ChatHTTP *http.Client

	// RetryDelay 维护类计费调用（签到/余额/保活）瞬时错误重试的间隔基数；
	// <=0 回落包级默认 billingRetryDelay（2s）。仅供测试注入更短间隔，生产不设置。
	RetryDelay time.Duration

	// ConversationRequestBody 是否在 body 的当前轮最后一条 user 消息写入
	// conversationRequestId / startsNewUserRequest（features.conversation_request_body；
	// 默认 false，供真实账号小流量 A/B 后开启，issue #249）。
	ConversationRequestBody atomic.Bool

	// HeaderTimeout 聊天 SSE 首字节前（响应头）超时；<=0 表示未设置（生产由 config
	// normalize 到 120s/300s；裸用回落 transport 构造默认）。
	HeaderTimeout time.Duration
	// IdleTimeout 聊天 SSE 流中空闲超时；<=0 表示禁用空闲监控。
	IdleTimeout time.Duration

	// ModelTimeouts 按模型覆盖聊天头/空闲超时（issue #58）。键可为
	// "[realm:]model" 或裸模型名；未命中回落 HeaderTimeout/IdleTimeout。
	ModelTimeouts map[string]ModelTimeout
	// timeoutMu/timeoutClients 缓存按 header 超时克隆的 chat client（避免每请求克隆
	// Transport 导致连接池碎片化；条目数受配置模型数约束）。
	timeoutMu      sync.Mutex
	timeoutClients map[time.Duration]*http.Client

	// AccountRoute 返回账号绑定的出站代理槽与拨号器（issue #96）；空 = 默认。
	AccountRoute func(a *auth.Auth) (slot string, dial func(ctx context.Context, network, addr string) (net.Conn, error))
	// slotMu/slotClients 按「槽 + header 超时」缓存独立 Transport，避免同一账号的
	// 连接被其他出口复用；条目数受槽数 × 模型超时数约束。
	slotMu      sync.Mutex
	slotClients map[string]*http.Client

	// effortsMu/efforts 缓存各模型 supportedEfforts（FetchModels 刷新），供请求体 effort 降级。
	// 按 realm 分层桶（cn/global）：同模型名跨域探测的 effort 集合可能不同，
	// 混桶会互相污染（C-2）。
	effortsMu sync.RWMutex
	efforts   map[string]map[string][]string
	// defaultEfforts 缓存各模型 reasoning.defaultEffort（FetchModels 刷新），供
	// thinking.go 补档：缺显式 effort 时优先用模型声明默认档，空串回退硬编码 high。
	// 与 efforts 同 realm 分层桶（同 C-2 隔离原则），共用 effortsMu。
	defaultEfforts map[string]map[string]string
	// modelRates 缓存各模型当前生效积分倍率（规范化数值，如 "0.5"）。
	// 与 efforts 共用 realm 分层和锁；每次成功刷新模型目录时整体替换对应域。
	modelRates map[string]map[string]string
	// aliasMu/aliases 缓存各 realm 的上游模型别名 → 规范模型 ID。
	aliasMu sync.RWMutex
	aliases map[string]map[string]string

	// globalModels 缓存 global 模型名目录探测结果（成功 ∩ 静态 overlay；
	// 1h TTL + 5min 负缓存），见 global_models.go。按实例持有，测试新建 Client 即隔离。
	globalModels fetchGlobalModelsCache

	// SanitizeFingerprints 出站请求体黑名单指纹脱敏开关（默认 true；false 完全还原）。
	// 面板保存配置热改 + chat 热路径并发读写，用 atomic.Bool 消除数据竞争。
	SanitizeFingerprints atomic.Bool

	// UserAgent 出站 User-Agent 显式覆盖（非空时全路径生效，优先于默认形态）。
	// 空 = 按 realm 取部署 profile 的冻结字面量：chat/refresh/FetchModels 用
	// emit.chatUA，billing/checkin/banner 用 emit.billingUA。
	UserAgent string

	// ClientVersion WorkBuddy 客户端版本段（出站 UA 的 `WorkBuddy/<ver>` + X-IDE-Version）。
	// 空 = profile.xIdeVersion。显式配置优先于 profile。
	ClientVersion string

	// CliVersion 出站 UA 中 `CLI/<ver>` 段版本。空 = profile.source.cliVersion。
	// 显式配置优先于 profile。
	CliVersion string

	// Profile 部署侧客户端指纹快照（data/client-profiles/<realm>.json）。
	// 整份提供 UA / IDE 版本 / 构建 commit/releaseDate；启动期按启用 realm 强制校验，
	// 缺失即拒绝启动。装配期注入，运行期只读。
	Profile *clientprofile.Set

	// ClientName 自定义用量归属名：非空时 X-Product 与 X-IDE-Name/Type 跟随该值。
	// 空 = 官方桌面端默认（X-Product="SaaS"、X-IDE-Name/Type="WorkBuddy"）。
	ClientName string

	// PassthroughIP 是否透传客户端 IP 给上游（X-Forwarded-For/X-Real-IP 首段）。
	// 缺省 false（反代安全边界）；handler 在 chat 路径按请求把 clientIP 传入 ChatStream。
	PassthroughIP bool

	// DeviceToken 设备风控 Token（X-Device-Token 头）全局兜底来源：config upstream.device_token。
	// 解析优先级：auth.Auth.DeviceToken > DeviceToken（config）> DeviceTokenFile（文件）。
	DeviceToken string

	// DeviceTokenFile 设备 token 文件路径兜底（宿主落盘的桌面端 token，5 分钟读取缓存）。
	DeviceTokenFile string

	// BalanceObserver 余额观测回调（可选）：每次成功查询余额后回调 (uid, remain, total)，
	// 供本地积分账本记录变动。装配期注入、运行期只读；回调 panic 被隔离，
	// 且回调失败不影响余额查询主路径（账本只是观测附属物）。
	BalanceObserver func(uid string, remain, total int64)
	ChatBaseCN      string
	BillingBaseCN   string
	// WebBaseCN 官网（workbuddy.cn）域：部分「任务领奖」类接口只在此域提供
	// （Web 成长中心用；CLI 域 copilot.tencent.com 的同名路径返回 400）。
	WebBaseCN string

	// ChatBaseGlobal / BillingBaseGlobal 国际版（global realm）上游 base。
	// 空 = 缺省默认 https://www.workbuddy.ai（D5）。
	ChatBaseGlobal    string
	BillingBaseGlobal string

	// GlobalEnabled 是否启用 global realm 路由（config global.enabled，缺省 true）。
	// false 时即便用户 auth 写了 realm=global 也**不**路由到 global base——
	// chatBase/billingBase 返回 CN base，路径也走 CN（双保险，与 auth.Realm() 的开关闸呼应）。
	GlobalEnabled bool

	// refreshMu/refreshInFlight 同账号 refresh 单飞（按 UID 分桶）：同一时刻只允许
	// 一个 refresh 在途，其余调用等待并复用首个结果。上游对 refreshToken 反重放，
	// 并发第二发会带着已被消费的 RT 撞 12153 并整条 RT 链作废（账号从此恒 401，
	// 只能人工重登）。本 fork 的并发来源比上游更多：scheduler keepalive 全量刷新、
	// handler 临近过期刷新与 401 重放、cmd/signin 独立进程。单飞在这里是保号手段。
	refreshMu       sync.Mutex
	refreshInFlight map[string]*refreshFlight
}

// New 生产默认值。Transport 由 newTransport() 集中构造（连接层加固：真正禁 h2 /
// TLS 握手超时 / 短 keepalive 探测 / 失败清池，参数见 transport.go——吸收上游
// kongjianguan 4 连击实测经验）。
func New() *Client {
	tr := newTransport()
	client := &Client{
		HTTP:          &http.Client{Timeout: 120 * time.Second, Transport: tr},
		ChatHTTP:      &http.Client{Timeout: 0, Transport: tr}, // 无总时长；首字节由 ResponseHeaderTimeout 管
		ChatBaseCN:    defaultCNChatBase,
		BillingBaseCN: "https://www.codebuddy.cn",
		WebBaseCN:     "https://www.workbuddy.cn",
		// GlobalEnabled 缺省 true（与 config global.enabled 缺省 true 一致；纯 CN 部署行为不变：
		// CN 账号恒判 cn，global base 只在 realm=global 的账号上被使用）。
		GlobalEnabled: true,
	}
	client.SanitizeFingerprints.Store(true)

	return client
}

// SetDialer 替换出站底层拨号器（代理池接入点，issue #15）。base 为 nil 时回落
// 内置直连拨号器。保留 writeDeadlineConn 写 deadline 包装（连接层加固不回退）。
// 装配期调用（首次请求前）；HTTP 与 ChatHTTP 共用同一 Transport，设置一次即覆盖两者。
func (c *Client) SetDialer(base func(ctx context.Context, network, addr string) (net.Conn, error)) {
	if base == nil {
		base = newDialer().DialContext
	}

	duration := writeTimeout

	wrap := func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := base(ctx, network, addr)
		if err != nil {
			return nil, err
		}

		return &writeDeadlineConn{Conn: conn, writeTimeout: duration}, nil
	}
	if tr, ok := c.HTTP.Transport.(*http.Transport); ok {
		tr.DialContext = wrap
	}

	if c.ChatHTTP != nil {
		if tr, ok := c.ChatHTTP.Transport.(*http.Transport); ok {
			tr.DialContext = wrap
		}
	}
}

// chatHTTP 返回聊天专用 client；未设置（如测试只注入 HTTP）时回落 HTTP。
func (c *Client) chatHTTP() *http.Client {
	if c.ChatHTTP != nil {
		return c.ChatHTTP
	}

	return c.HTTP
}

// defaultGlobalBase 缺省 global base（D5：config 未覆盖时默认 workbuddy.ai）。
const defaultGlobalBase = "https://www.workbuddy.ai"

// CN 官方 origin 顺序：workbuddy.cn 为主，copilot.tencent.com / wb.tencentbuddy.com
// 为兜底（仅在首 token 前可重放）。显式覆盖 chat base 时只用该值。
const defaultCNChatBase = "https://www.workbuddy.cn"

var cnChatFallbackBases = []string{
	"https://copilot.tencent.com",
	"https://wb.tencentbuddy.com",
}

// chatOrigins 返回本次 chat 可尝试的 origin 列表（按优先级）。
func (c *Client) chatOrigins(a *auth.Auth) []string {
	if c.globalOn(a) {
		return []string{normalizeChatBase(c.globalChatBase())}
	}

	base := normalizeChatBase(c.ChatBaseCN)
	if base == "" {
		base = defaultCNChatBase
	}

	if base != defaultCNChatBase {
		return []string{base}
	}

	out := make([]string, 0, 1+len(cnChatFallbackBases))
	out = append(out, base)
	out = append(out, cnChatFallbackBases...)

	return out
}

// normalizeChatBase 归一 chat base：host / host/v2 / host/v2/chat/completions
// 三种输入都收敛为 host，保证最终 URL 只拼一次 /v2/chat/completions。
func normalizeChatBase(base string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	for _, suffix := range []string{"/v2/chat/completions", "/chat/completions", "/v2"} {
		if trimmed, ok := strings.CutSuffix(base, suffix); ok {
			return strings.TrimRight(trimmed, "/")
		}
	}

	return base
}

// replayableOriginStatus 首 token 前可切 origin 重放的状态码。
func replayableOriginStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

// globalChatBase 生效的 global chat base：Client.ChatBaseGlobal 非空取之，否则默认。
func (c *Client) globalChatBase() string {
	if c.ChatBaseGlobal != "" {
		return c.ChatBaseGlobal
	}

	return defaultGlobalBase
}

// globalBillingBase 生效的 global billing base：Client.BillingBaseGlobal 非空取之，否则默认。
func (c *Client) globalBillingBase() string {
	if c.BillingBaseGlobal != "" {
		return c.BillingBaseGlobal
	}

	return defaultGlobalBase
}

// globalOn 报告账号是否路由到 global 上游：GlobalEnabled 开且账号 Realm()==global。
// 双保险：config 开关是第一道闸（上游侧），auth.Realm() 的开关闸是第二道（账号侧）。
func (c *Client) globalOn(a *auth.Auth) bool {
	return c.GlobalEnabled && a != nil && a.Realm() == "global"
}

// 路径常量：CN 与 global 共用的 chat 出站路径（/v2 单路径）。
const chatCompletionsPath = "/v2/chat/completions"

// chatPaths 返回按 realm 的 chat 路径候选序列：
// global → [/v2]（#119 固定单路径：/console 挂腾讯云 WAF body 内容规则，反引号
// printf/whoami 等命令执行特征确定性 403；/v2 同 base 不挂该规则，实测等价端点。
// 已知取舍：若上游未来关闭 /v2，global chat 整体不可用——届时应重新启用 /console
// 路径，此注释即"坏了再说"的锚点）；cn → [/v2]（单元素，现状）。
func (c *Client) chatPaths(a *auth.Auth) []string {
	return []string{chatCompletionsPath}
}

// billing 域端点路径（billingBase + path）。balance/checkin 与 report（report.go）同域，
// 统一走 billingJSON 发请求。
const (
	billingMeterPath   = "/billing/meter/get-user-resource"    // global 首选（国际版无 /v2 前缀）
	dailyCheckinPath   = "/billing/meter/daily-checkin"        // global 首选
	billingMeterPathV2 = "/v2/billing/meter/get-user-resource" // CN 现状 / global fallback
	dailyCheckinPathV2 = "/v2/billing/meter/daily-checkin"

	checkinActivityStatusPath = "/v2/billing/meter/checkin-activity-status"
)

// billingMeterPaths 按 realm 返回 billing/meter 域路径候选序列：
// global → [无 /v2, 有 /v2]；cn → [有 /v2, 无 /v2]（官方桌面 CN 的 resourcePrefix
// 为空串，无 /v2 是官方形态；保留 /v2 优先以维持现状，404 再回落）。
// 仅作用于 get-user-resource / daily-checkin（/billing/meter/* 族）；report /v2/report
// 不参与，其他 billing 端点（growth 等）路径不含 /billing/meter 前缀，走原常量不受影响。
func (c *Client) billingMeterPaths(a *auth.Auth) []string {
	if c.globalOn(a) {
		return []string{billingMeterPath, billingMeterPathV2}
	}

	return []string{billingMeterPathV2, billingMeterPath}
}

// checkinMeterPaths 签到路径候选：两 realm 均 `/v2` 优先，404 时回落无 `/v2`
// （官方桌面端 billingPrefix=/v2；旧路径保留 fallback）。
func (c *Client) checkinMeterPaths(a *auth.Auth) []string {
	return []string{dailyCheckinPathV2, dailyCheckinPath}
}

// chatBase 返回按账号域生效的 chat base（已归一为 host）。
//
// 归一必须在这里做而不是只在 chat/search 各自做：配置允许 host / host/v2 /
// host/v2/chat/completions 三种形态，而 chatBase 同时被 images、modelbusy、
// v3/config、任务/旅行等「chat 同源但非 /v2/chat/completions」路径复用。
// 若返回原始配置值，配成 host/v2 时这些端点会拼出 /v2/v2/...。
func (c *Client) chatBase(a *auth.Auth) string {
	if c.globalOn(a) {
		return normalizeChatBase(c.globalChatBase())
	}

	return normalizeChatBase(c.ChatBaseCN)
}

// prepareBody 组装出站请求体（脱敏开关由 Client.SanitizeFingerprints 控制）。
// realm 为账号 Realm()（cn/global），供 efforts 缓存分桶（跨域 effort 集合不互相污染）。
func (c *Client) prepareBody(body []byte, realm, uid, conversationID string) []byte {
	efforts, defs := c.effortsSnapshot(realm), c.defaultEffortsSnapshot(realm)
	if realmKey(realm) == "global" {
		// global 域降级源 = 远端探测桶（权威）∪ 产品静态兜底表（全局 21 名内档位如
		// deepseek-v4.1-flash ['high']）。当前探测桶为空时也按静态表降级，不全程透传
		// （issue #84：往 WorkBuddy 上游发 low/max 非法，须降级到 high）。
		efforts, defs = globalEffortMap(efforts, defs)
	}

	body = PrepareBodyOptWithEffortsAndDefault(body, c.SanitizeFingerprints.Load(), efforts, defs)
	// prompt_cache_key 注入（P0 费用优化，费用降 ~17×）：按账号隔离的稳定缓存键，
	// 让同一客户端对同一账号的连续请求命中上游前缀缓存。
	body = InjectPromptCacheKey(body, uid, conversationID)

	return body
}

// effortsSnapshot 返回 effort 能力缓存的只读快照；nil 表示未知（透传不降级）。
// storeEfforts 在写入时已克隆输入并整体替换桶，快照发布后不可变，读路径零拷贝。
// 调用方只读，不得修改返回的 map/slice。
func (c *Client) effortsSnapshot(realm string) map[string][]string {
	c.effortsMu.RLock()
	defer c.effortsMu.RUnlock()

	bucket := c.efforts[realmKey(realm)]
	if len(bucket) == 0 {
		return nil
	}

	return bucket
}

// defaultEffortsSnapshot 返回指定 realm 的模型 defaultEffort 只读快照；
// 该域无探测或无声明默认档 → nil（thinking.go 回退硬编码 high）。
// 与 effortsSnapshot 同契约：写入时克隆，快照不可变，调用方只读。
func (c *Client) defaultEffortsSnapshot(realm string) map[string]string {
	c.effortsMu.RLock()
	defer c.effortsMu.RUnlock()

	bucket := c.defaultEfforts[realmKey(realm)]
	if len(bucket) == 0 {
		return nil
	}

	return bucket
}

// realmKey 归一化 efforts 缓存键：cn/global。空 realm 视为 cn（老调用/无前缀模型名）。
func realmKey(realm string) string {
	if realm == "" {
		return "cn"
	}

	return realm
}

func (c *Client) billingBase(a *auth.Auth) string {
	if c.globalOn(a) {
		return c.globalBillingBase()
	}

	return c.BillingBaseCN
}

// webBase 返回官网域（任务领奖类接口；未注入时回落默认）。
// realm 感知：global 账号切国际站 workbuddy.ai，CN 用 workbuddy.cn。
func (c *Client) webBase(a *auth.Auth) string {
	if c.globalOn(a) {
		return defaultGlobalBase
	}

	if c.WebBaseCN != "" {
		return c.WebBaseCN
	}

	return "https://www.workbuddy.cn"
}

// defaultJSONBodyLimit doJSON 默认响应体上限（1 MiB）。
const defaultJSONBodyLimit int64 = 1 << 20

// doJSON 发请求并解信封；HTTP 非 2xx 或业务 code != 0 时返回带 body 片段的 *Error。
// body 读失败（连接中断/空闲掐流/截断）返回普通错误（非 *Error）——半截 body 不进
// Classify，不参与账号惩罚（传输层故障不该喂熔断误罚号）。
func (c *Client) doJSON(req *http.Request) (json.RawMessage, error) {
	return c.doJSONLimit(req, defaultJSONBodyLimit)
}

// doJSONLimit 同 doJSON，但允许调用方指定响应体上限。超限返回明确错误而不是把
// 半截 body 交给 json.Unmarshal（后者只会报 "unexpected end of JSON input"）。
func (c *Client) doJSONLimit(req *http.Request, maxBytes int64) (json.RawMessage, error) {
	if maxBytes <= 0 {
		maxBytes = defaultJSONBodyLimit
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("doJSONLimit: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	if int64(len(raw)) > maxBytes {
		return nil, fmt.Errorf("response body exceeds %d bytes: %w", maxBytes, errDoJSONLimitFailed)
	}

	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))

		return nil, &Error{
			Kind: kind, Status: resp.StatusCode,
			Msg: truncate(string(raw), 200), Code: businessCode(string(raw)),
		}
	}

	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse failed: %w (body: %s)", err, truncate(string(raw), 120))
	}

	if env.Code != 0 {
		// 保留原始结构化信封交给 Classify：额度/限流/请求级码都依赖 code 字段，
		// 拼成 `code=N msg=...` 纯文本会让后续分类只能看文案。
		kind := Classify(resp.StatusCode, string(raw))
		if kind == ErrNone {
			kind = ErrClient
		}

		return nil, &Error{
			Kind: kind, Status: resp.StatusCode,
			Msg: truncate(string(raw), 200), Code: businessCode(string(raw)),
		}
	}

	return env.Data, nil
}

// refreshIOTimeout 刷新端点网络 I/O 上限（两段式锁外执行，防上游 hang 长占锁）。
const refreshIOTimeout = 30 * time.Second

// refreshTokenExpiresInMax refresh 响应 expiresIn 的量级上限（10 年，纯防御值：
// 实测 R-D 响应恒 5184000=60d）。超限视为上游脏数据，不写 ExpiresAt（保留旧值），
// 防止 NeedsRefresh 永假导致 token 永不刷新反而真过期失效。
const refreshTokenExpiresInMax = 10 * 365 * 24 * time.Hour

// RefreshToken 刷新 access token；成功时更新 a 的字段（缺省值保留旧值），
// 调用方负责 SaveAtomic。
//
// 并发安全模型（单飞 + 两段式）：
//   - **单飞**：同一账号（UID）同一时刻只允许一个 refresh 在途，其余调用等待并
//     原样复用首个结果。上游对 refreshToken 反重放：RT 被消费后再用会撞 12153，
//     并把整条 RT 链作废（账号从此恒 401，只能人工重登）。本 fork 的并发来源
//     更多（scheduler keepalive、handler 临近过期刷新与 401 重放、cmd/signin），
//     单飞是保号手段而非省流量。
//   - 首个调用内部仍是两段式：锁内读快照 → 锁外 I/O（30s）→ 锁内校验快照一致后
//     写回；写回前若发现另一刷新已完成（两 token 同时变化）则直接采用。
//   - 12153 兜底：磁盘 auth 文件已被其他进程（server + signin 双进程部署）刷新过
//     时采纳磁盘 token 重试一次——进程内单飞挡不住跨进程竞态。无 FilePath 或磁盘
//     RT 与内存一致时不重试（不做无限重试）。
func (c *Client) RefreshToken(account10 *auth.Auth) error {
	key := account10.UID
	if key == "" {
		// 无 UID 的临时 auth：退化为按对象指针去重，仍保证同一对象串行。
		key = fmt.Sprintf("%p", account10)
	}

	// 等待者用自己的快照合并 leader 的结果；与自己同一对象时 commit 守卫会跳过。
	rtSnapshot, atBefore, err := refreshTokenSnapshot(account10)
	if err != nil {
		return err
	}

	// 取票或等待既有票据。等待者不解锁任何账号锁，避免与首个调用的 I/O 串行化。
	c.refreshMu.Lock()

	if c.refreshInFlight == nil {
		c.refreshInFlight = make(map[string]*refreshFlight)
	}

	if flight, ok := c.refreshInFlight[key]; ok {
		c.refreshMu.Unlock()
		<-flight.done

		if flight.err == nil {
			commitRefreshedToken(account10, flight.tok, atBefore, rtSnapshot)
		}

		return flight.err
	}

	flight := &refreshFlight{done: make(chan struct{})}
	c.refreshInFlight[key] = flight
	c.refreshMu.Unlock()

	tok, err := c.refreshTokenOnce(account10)
	if err != nil && isSessionDeadErr(err) && adoptNewerTokensFromDisk(account10) {
		// 跨进程竞态：磁盘 RT 已被别的进程 rotate 过，用新 RT 再试一次。
		tok, err = c.refreshTokenOnce(account10)
	}

	// 先写结果再关 channel：等待者 <-flight.done 之后读到的一定是最终 err。
	flight.tok = tok
	flight.err = err
	close(flight.done)

	c.refreshMu.Lock()
	delete(c.refreshInFlight, key)
	c.refreshMu.Unlock()

	return err
}

// RefreshTokenIfNeeded 按需刷新（请求路径 / signin 用）：先快速判断是否临近
// 过期；真正需要刷新时复用 RefreshToken 的单飞结果。
func (c *Client) RefreshTokenIfNeeded(a *auth.Auth, within time.Duration) error {
	if !a.NeedsRefresh(within) {
		return nil
	}

	return c.RefreshToken(a)
}

// refreshFlight 是同一账号一次 refresh 的单飞票据：done 关闭即结果就绪。
type refreshFlight struct {
	done chan struct{}
	tok  refreshTokenResponse
	err  error
}

// IsCredentialDead 判定错误是否意味着会话/refresh token 已不可用：账号需要重新
// 登录，而不是继续换号。refresh 侧 401/403、12153 与「无 refresh token」都算。
// 供 handler / scheduler 共用，避免各处只看 *Error.Kind 漏掉无 RT 的失败（issue #245）。
func IsCredentialDead(err error) bool {
	if errors.Is(err, errNoRefreshToken) || errors.Is(err, errRefreshFailedNoAccessTokenIn) {
		return true
	}

	return isSessionDeadErr(err)
}

// isSessionDeadErr 判定错误是否为会话/RT 失效（12153 或 refresh 侧 401/403 映射）。
func isSessionDeadErr(err error) bool {
	var ue *Error

	return errors.As(err, &ue) && ue.Kind == ErrSessionDead
}

// adoptNewerTokensFromDisk 在 refresh 失败后回读 auth 文件：磁盘 RT 与内存不同
// （另一进程已刷新并写回）时采纳磁盘 token 并返回 true，供调用方重试一次。
// 读文件/解析失败、无 FilePath、RT 一致均返回 false（不改内存状态，保持失败语义）。
func adoptNewerTokensFromDisk(account *auth.Auth) bool {
	if account.FilePath == "" {
		return false
	}

	// 只读主文件，不回退 .bak：.bak 是上一次写入前的副本，其中的 RT 已在上一次
	// 刷新时被消费；拿它重试会触发上游反重放，作废整条 RT 链。主文件损坏时按
	// 失败处理，由 auth.LoadDir 在下次启动时用 .bak 恢复可加载的凭证。
	raw, err := os.ReadFile(account.FilePath)
	if err != nil {
		return false
	}

	disk, err := auth.Parse(raw)
	if err != nil || strings.TrimSpace(disk.RefreshToken) == "" {
		return false
	}

	account.Lock()
	defer account.Unlock()

	if disk.RefreshToken == account.RefreshToken {
		return false
	}

	account.AccessToken = disk.AccessToken
	account.RefreshToken = disk.RefreshToken

	if disk.ExpiresAt > 0 {
		account.ExpiresAt = disk.ExpiresAt
	}

	return true
}

// refreshTokenOnce 是单次刷新本体（调用方 RefreshToken 已保证同账号串行）。
func (c *Client) refreshTokenOnce(account10 *auth.Auth) (refreshTokenResponse, error) {
	// 第 1 段（锁内）：读快照。
	rtSnapshot, atBefore, err := refreshTokenSnapshot(account10)
	if err != nil {
		return refreshTokenResponse{}, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), refreshIOTimeout)
	defer cancel()

	// RefreshHeaders 读取 a 的字段（domain/uid 等）注入请求头——需在锁内取快照值，
	// 用一个显式逐字段拷贝的临时 auth 构造头（不拷贝 sync.Mutex，避免 vet copies-lock）。
	account10.Lock()
	hdrSnapshot := auth.Auth{
		AccessToken:  account10.AccessToken,
		RefreshToken: rtSnapshot,
		ExpiresAt:    account10.ExpiresAt,
		Domain:       account10.Domain,
		UID:          account10.UID,
		EnterpriseID: account10.EnterpriseID,
		Nickname:     account10.Nickname,
		DeviceToken:  account10.DeviceToken,
	}
	account10.Unlock()

	var data json.RawMessage

	origins := c.chatOrigins(account10)

	var lastErr error

	for i, base := range origins {
		endpoint := base + "/v2/plugin/auth/token/refresh"

		req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, http.NoBody)
		if reqErr != nil {
			return refreshTokenResponse{}, fmt.Errorf("RefreshToken: %w", reqErr)
		}

		c.RefreshHeaders(req, &hdrSnapshot)

		// 网络 I/O（锁外，30s 上限）。
		data, lastErr = c.doJSON(req)
		if lastErr == nil {
			break
		}

		lastErr = classifyRefreshError(lastErr)
		if i >= len(origins)-1 || !refreshReplayable(lastErr) {
			return refreshTokenResponse{}, lastErr
		}
	}

	tok, err := parseRefreshResponse(data)
	if err != nil {
		return refreshTokenResponse{}, err
	}

	// 第 2 段（锁内）：校验快照一致后写回。
	commitRefreshedToken(account10, tok, atBefore, rtSnapshot)

	return tok, nil
}

// commitRefreshedToken 锁内校验快照一致后写回新 token 字段。
func commitRefreshedToken(account4 *auth.Auth, tok refreshTokenResponse, atBefore, rtSnapshot string) {
	account4.Lock()
	defer account4.Unlock()
	// 写回守卫是 AND 语义：锁外期间另一刷新已完成 → 两 token 必同时变化（实测 R-D：
	// refresh 响应 accessToken/refreshToken 总是一起 rotate，写回也同时写两个），AND
	// 即「并发刷新已完成」判据；AND 与 OR 在真实形态下等价。唯 OR 会额外放弃的
	// 「只有单 token 变化」（如手工只改 auth 文件一个字段）不构成放弃条件——本次
	// 结果覆盖手工编辑。
	if account4.AccessToken != atBefore && account4.RefreshToken != rtSnapshot {
		// 锁外期间另一 goroutine 已完成刷新：新 token 已生效，本次结果不必再写
		// （实测 R-E：服务端无 rotation 撤销，并发双刷新拿到的两个新 token 都有效，
		// 后写覆盖先写二者等价可用；提前返回避免无意义覆盖与 ExpiresAt 抖动）。
		return
	}

	account4.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		account4.RefreshToken = tok.RefreshToken
	}

	if tok.Domain != "" {
		account4.Domain = tok.Domain
	}
	// preserveExpiry：响应缺 expiresIn 时保留旧过期时间，避免刷新风暴。
	// 实测 R-D 响应恒带 expiresIn=5184000（60d）——缺省分支仅为防御，保留旧值
	// 避免过期判定漂移。同理，超过 10 年的 expiresIn 按脏值处理保留旧值：
	// 实测 JWT exp-iat 与 expiresIn 严格自洽（R-F），超量级值只会是上游脏数据，
	// 照写会把 ExpiresAt 推到荒谬未来 → NeedsRefresh 永假 → token 永不刷新
	// 反而真过期失效。
	if tok.ExpiresIn > 0 && time.Duration(tok.ExpiresIn)*time.Second < refreshTokenExpiresInMax {
		account4.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	}
}

// refreshTokenResponse refresh 端点响应体（字段名与上游 JSON 对齐）。
type refreshTokenResponse struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresIn    int64  `json:"expiresIn"`
	Domain       string `json:"domain"`
}

// refreshTokenSnapshot 锁内读取 refresh/access token 快照；refreshToken 为空视为
// 无刷新凭证，返回错误（调用方直接失败）。
func refreshTokenSnapshot(account *auth.Auth) (rtSnapshot, atBefore string, err error) {
	account.Lock()
	rtSnapshot = account.RefreshToken
	atBefore = account.AccessToken
	account.Unlock()

	if strings.TrimSpace(rtSnapshot) == "" {
		return "", "", errNoRefreshToken
	}

	return rtSnapshot, atBefore, nil
}

// parseRefreshResponse 解析 refresh 响应；缺 accessToken 视为刷新失败（需重登）。
func parseRefreshResponse(data []byte) (refreshTokenResponse, error) {
	var tok refreshTokenResponse
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return refreshTokenResponse{}, errRefreshFailedNoAccessTokenIn
	}

	return tok, nil
}

// classifyRefreshError 把 refresh 端点的「纯凭证拒绝」映射为会话失效。
// 刷新端点 HTTP 401/403 且无更具体分类（ErrClient/ErrNone）时，说明 refresh
// token 本身已不可用，进入 session-dead 累计/禁用路径。WAF 403、账号封禁、
// 额度耗尽等已有更权威分类的错误保持原样，避免 CDN/WAF 403 被误判成会话死亡
// 而永久禁用账号。传输层错误不在此改写。
func classifyRefreshError(err error) error {
	var upstreamErr *Error
	if !errors.As(err, &upstreamErr) {
		return err
	}

	if upstreamErr.Status != http.StatusUnauthorized && upstreamErr.Status != http.StatusForbidden {
		return err
	}

	// 11142 auth_required 在 refresh 端点表示 refresh token 本身失效：账号需要
	// 重新登录，必须进入 session-dead 累计/禁用路径，不能按普通账号故障反复重试。
	if upstreamErr.Kind == ErrAccountFault && upstreamErr.Code == "11142" {
		return &Error{Kind: ErrSessionDead, Status: upstreamErr.Status, Msg: upstreamErr.Msg, Code: upstreamErr.Code}
	}

	if upstreamErr.Kind != ErrNone && upstreamErr.Kind != ErrClient {
		return err
	}

	return &Error{Kind: ErrSessionDead, Status: upstreamErr.Status, Msg: upstreamErr.Msg, Code: upstreamErr.Code}
}

// refreshReplayable 报告 refresh 失败是否值得换 origin 重试：仅传输层错误与
// 408/502/503/504；401/403 已映射为会话失效，不得跨 origin 掩盖。
func refreshReplayable(err error) bool {
	var ue *Error
	if !errors.As(err, &ue) {
		return true
	}

	return replayableOriginStatus(ue.Status)
}

// ChatStream 发 chat 请求并返回原始 SSE body 流（调用方负责 Close）。
// 等价于 ChatStreamContext(context.Background(), ...)：不带调用方取消语义。
// 需要客户端断开联动的调用方用 ChatStreamContext 传入请求 ctx。
//
// 两 realm 均走 /v2/chat/completions；CN 默认主域在首 token 前可按
// 408/502/503/504 或传输错误回落旧 origin，global 单 origin。
//
//nolint:gocritic // ChatMeta 值传递保持公开 API 简单。
func (c *Client) ChatStream(
	a *auth.Auth,
	body []byte,
	clientIP string,
	meta ChatMeta,
) (rc io.ReadCloser, status int, respBody []byte, err error) {
	return c.ChatStreamContext(context.Background(), a, body, clientIP, meta)
}

// ChatStreamContext 同 ChatStream，但从 ctx 派生请求 context：调用方（handler）传入
// r.Context() 后，客户端断连/请求取消会立即中断在途上游调用、释放连接与账号在途名额，
// 不再空转到 IdleTimeout。ctx 为 nil 时回落 Background。成功流的 cancel 仍由
// monitorBody 的 Close 接管（reqCtx 取消与显式 Close 任一触发即断）。
//
// 错误路径（≥400 且非 fallback 状态码）除 (status, respBody) 外还返回**已分类的**
// *Error（Kind 信封 + Retry-After 头解析）：客户端错误分类在此一次完成，handler
// 不再对 body 二次 Classify（消除「上游分类一次、网关再分类一次」的双路径漂移面），
// Retry-After 也随信封流动。respBody 仍原样返回（错误透传语义：message 透传上游
// 原文）。判定为 ErrNone 的响应（理论上不存在，防御）err 为 nil，handler 按
// respBody 自行兜底。
//
// global chat 自 #119 实测后固定走 /v2（chat 层无 fallback 链；billing 层的 404
// fallback 独立存在，语义不受影响）。ensureLeadingSystem 在 prepareBody 后统一套用：
// 首条消息非 system 时前置官方兜底 system（防 11-128 "first message is not system
// prompt"；CN/global 都需要）。
// modelFromBody 取聊天请求体里的 model（畸形/缺失 → 空串，走全局超时）。
func modelFromBody(body []byte) string {
	var peek struct {
		Model string `json:"model"`
	}

	_ = json.Unmarshal(body, &peek)

	return peek.Model
}

//nolint:gocritic // ChatMeta 值传递保持公开 API 简单；仅 header 构造读取一次。
func (c *Client) ChatStreamContext(
	ctx context.Context, account3 *auth.Auth, body []byte, clientIP string, meta ChatMeta,
) (rc io.ReadCloser, status int, respBody []byte, err error) {
	if ctx == nil {
		ctx = context.Background()
	}

	prepared := c.prepareBody(body, account3.Realm(), account3.UID, meta.ConversationID)
	prepared = ensureLeadingSystem(prepared)

	if c.ConversationRequestBody.Load() && meta.ConversationRequestID != "" {
		prepared = injectConversationRequestID(prepared, meta.ConversationRequestID)
	}

	headerTimeout, idleTimeout := c.modelTimeout(account3.Realm(), modelFromBody(prepared))
	chatHTTP := c.chatHTTPForAccount(account3, headerTimeout)
	path := c.chatPaths(account3)[0]
	origins := c.chatOrigins(account3)

	var lastErr error

	for index, base := range origins {
		attempt := c.chatStreamAt(ctx, account3, prepared, base, path, clientIP, meta, chatHTTP, idleTimeout)
		if attempt.err == nil && attempt.status < http.StatusBadRequest {
			return attempt.rc, attempt.status, nil, nil
		}

		lastErr = attempt.err
		if attempt.replay && index < len(origins)-1 {
			// 调用方已取消（客户端断连/超时）时不再向后续 origin 重放：
			// 请求已无消费者，继续发只会放大上游请求量。
			if ctx.Err() != nil {
				return nil, attempt.status, attempt.body, attempt.err
			}

			continue
		}

		return nil, attempt.status, attempt.body, attempt.err
	}

	return nil, 0, nil, lastErr
}

// chatAttempt 单 origin 的一次 chat 尝试结果。
type chatAttempt struct {
	rc     io.ReadCloser
	status int
	body   []byte
	err    error
	replay bool
}

// chatStreamAt 向单个 origin 发起 chat 请求。replay 表示首 token 前可换 origin 重放。
//
//nolint:gocritic // ChatMeta 值传递保持调用链简单。
func (c *Client) chatStreamAt(
	ctx context.Context, account3 *auth.Auth, prepared []byte, base, path, clientIP string,
	meta ChatMeta, chatHTTP *http.Client, idleTimeout time.Duration,
) chatAttempt {
	endpoint := base + path

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, endpoint, bytes.NewReader(prepared))
	if err != nil {
		return chatAttempt{err: fmt.Errorf("ChatStreamContext: %w", err)}
	}

	c.ChatHeaders(req, account3, clientIP, meta)
	// 从调用方 ctx 派生：保留取消传播（父 ctx 取消 → 本 ctx 取消），
	// 同时 monitorBody.Close 仍能独立 cancel 本分支（空闲掐流）。
	reqCtx, cancel := context.WithCancel(ctx)
	req = req.WithContext(reqCtx)

	resp, err := chatHTTP.Do(req)
	if err != nil {
		cancel()

		log.Printf("ERR: [upstream] chat_stream acct=%s origin=%s: transport error: %v",
			logfmt.Label(account3.UID, account3.NicknameValue()), base, err)
		roundTripCloseIdle(chatHTTP.Transport)

		return chatAttempt{err: fmt.Errorf("ChatStreamContext: %w", err), replay: true}
	}

	if resp.StatusCode < 400 {
		// 成功分支：cancel 所有权交给 monitorBody（其 Close 会 cancel）。
		return chatAttempt{rc: monitorBody(resp.Body, idleTimeout, cancel), status: resp.StatusCode}
	}

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	cancel()

	if readErr != nil {
		log.Printf("ERR: [upstream] chat_stream acct=%s: read body: %v", logfmt.Label(account3.UID, account3.NicknameValue()), readErr)

		return chatAttempt{err: fmt.Errorf("read body: %w", readErr), replay: true}
	}

	kind := Classify(resp.StatusCode, string(raw))
	log.Printf("WARN: [upstream] chat_stream acct=%s origin=%s: upstream %d %s body=%s",
		logfmt.Label(account3.UID, account3.NicknameValue()), base, resp.StatusCode, kind, RedactForLog(string(raw), 200))

	if kind == ErrNone {
		return chatAttempt{status: resp.StatusCode, body: raw}
	}

	upstreamErr := &Error{
		Kind: kind, Status: resp.StatusCode,
		Msg: truncate(string(raw), 200), Code: businessCode(string(raw)),
	}
	if d, ok := ParseRetryAfter(resp.Header); ok {
		upstreamErr.RetryAfter = d
	}

	return chatAttempt{status: resp.StatusCode, body: raw, err: upstreamErr, replay: replayableOriginStatus(resp.StatusCode)}
}

// ModelTimeout 单个模型的聊天超时覆盖；零值字段回落全局。
type ModelTimeout struct {
	Header time.Duration
	Idle   time.Duration
}

// modelTimeout 返回该请求生效的 chat header/idle 超时：先查 "realm:model"，
// 再查裸模型名，未命中回落全局值。
func (c *Client) modelTimeout(realm, model string) (header, idle time.Duration) {
	header, idle = c.HeaderTimeout, c.IdleTimeout
	if model == "" {
		return header, idle
	}

	if t, ok := c.ModelTimeouts[realm+":"+model]; ok {
		return applyTimeout(t, header, idle)
	}

	if t, ok := c.ModelTimeouts[model]; ok {
		return applyTimeout(t, header, idle)
	}

	return header, idle
}

// applyTimeout 用非零覆盖值替换全局默认。
func applyTimeout(t ModelTimeout, header, idle time.Duration) (time.Duration, time.Duration) {
	if t.Header > 0 {
		header = t.Header
	}

	if t.Idle > 0 {
		idle = t.Idle
	}

	return header, idle
}

// chatHTTPForHeader 返回 header 超时为 d 的 chat client：等于全局值时复用原 client，
// 否则缓存一份 Transport 克隆（连接池按超时值隔离，仍复用同一拨号/代理配置）。
func (c *Client) chatHTTPForHeader(duration time.Duration) *http.Client {
	base := c.chatHTTP()
	if duration <= 0 || duration == c.HeaderTimeout {
		return base
	}

	c.timeoutMu.Lock()
	defer c.timeoutMu.Unlock()

	if c.timeoutClients == nil {
		c.timeoutClients = map[time.Duration]*http.Client{}
	}

	if client := c.timeoutClients[duration]; client != nil {
		return client
	}

	tr, ok := base.Transport.(*http.Transport)
	if !ok {
		return base
	}

	cloned := tr.Clone()
	cloned.ResponseHeaderTimeout = duration
	client := &http.Client{Transport: cloned, Timeout: base.Timeout}
	c.timeoutClients[duration] = client

	return client
}

// chatHTTPForAccount 返回账号代理槽专用的 chat client（issue #96）。未绑定槽
// 或未配置路由时回落到默认 client；每个槽使用独立 Transport，连接池不跨账号共享。
func (c *Client) chatHTTPForAccount(account5 *auth.Auth, duration2 time.Duration) *http.Client {
	base := c.chatHTTPForHeader(duration2)
	if c.AccountRoute == nil || account5 == nil {
		return base
	}

	slot, dial := c.AccountRoute(account5)
	if slot == "" || dial == nil {
		return base
	}

	key := slot + "|" + duration2.String()

	c.slotMu.Lock()
	defer c.slotMu.Unlock()

	if client := c.slotClients[key]; client != nil {
		return client
	}

	tr, ok := base.Transport.(*http.Transport)
	if !ok {
		return base
	}

	cloned := tr.Clone()
	duration := writeTimeout
	cloned.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}

		return &writeDeadlineConn{Conn: conn, writeTimeout: duration}, nil
	}
	client := &http.Client{Transport: cloned, Timeout: base.Timeout}

	if c.slotClients == nil {
		c.slotClients = map[string]*http.Client{}
	}

	c.slotClients[key] = client

	return client
}

// ModelInfo 动态模型信息（含 maxInputTokens/maxOutputTokens + 上游模型对象全字段）。
// CN /console 与 global /v2 的模型对象同构，故共用此结构；上游省略的字段保持零值，
// /v1/models 侧按「空值省略」透出（不编造）。
type ModelInfo struct {
	ID            string
	Name          string
	ContextWindow int64    // = maxInputTokens
	MaxTokens     int64    // = maxOutputTokens（思考与最终回答共享此预算，上游无独立思考上限字段）
	Efforts       []string // reasoning.supportedEfforts（空=未知/固定档）
	DefaultEffort string   // reasoning.defaultEffort（新模型键）或 reasoning.effort（老模型键）；空=未返回

	// 模型目录全字段（models-full-fields）：
	Description        string   // descriptionZh 中文描述
	Credits            string   // credits 积分倍率原文（如 "x0.05"），仅展示不参与选号
	Tags               []string // tags 模型标签（含 badge:限时免费 等）
	Vendor             string   // vendor 厂商标识
	IsDefault          bool     // isDefault 是否默认模型
	SupportsReasoning  bool     // supportsReasoning 是否支持推理
	SupportsToolCall   bool     // supportsToolCall 是否支持工具调用
	OnlyReasoning      bool     // onlyReasoning 是否纯推理模型
	SupportsImages     bool     // 顶层 supportsImages（多模态能力，透出到 /v1/models）
	InputModalities    []string // 输入模态（inputModalities 或 modalities.input；空 = 未知）
	MaxAllowedSize     int64    // maxAllowedSize 最大允许上下文（与 maxInputTokens 口径并列，上游各自下发）
	CanDisableThinking bool     // reasoning.canDisableThinking：思考可关（off 档可用）
	ReasoningEffort    string   // reasoning.effort 推理模式（与 supportedEfforts 数组不同源）
	ReasoningSummary   string   // reasoning.summary 推理摘要模式（如 "auto"）
	Aliases            []string // 上游模型别名（请求按别名解析到本模型）

	// 优惠（modelPromotions，/v3/config data.modelPromotions）：Credits 是**牌价**
	// （转正后基准倍率），Promo* 是当前生效的限时优惠——面板据此显示「生效价 +
	// 标签 + 牌价」。PromoFactor 为 nil 表示无 machine-readable 折扣（如「错峰
	// 使用」只有时段文案无 factor），仅挂标签/提示。
	PromoFactor  *float64 // 折扣系数（0=限时免费，0.5=五折）；nil=无
	PromoCredits string   // 折扣后倍率原文（如 "0x" / "0.50x"），仅展示
	PromoLabel   string   // 徽章文案（限时免费 / 夜间折扣 / 错峰使用）
	PromoNote    string   // hover 说明原文（含时段/日期描述）
}

// dynModelEntry 上游模型目录（CN /console 与 global /v2 同构）的单条模型解析形态，
// FetchModels 与 global_models.go 的探测共用。iconUrl/descriptionEn/生成参数等
// 按「不透出」原则不解析。modelInfo() 是 dynEntry→ModelInfo 映射的单一事实来源，
// 杜绝两域映射漂移。
type dynModelEntry struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// ModelID / Model id 的宽松回退键（仅 global 目录的多信封兜底用，CN 目录
	// 不下发这两个键；字段加在这里只是让 typed 解析能"看见"它们）。
	ModelID         string   `json:"modelId"`
	Model           string   `json:"model"`
	Description     string   `json:"descriptionZh"`
	Credits         string   `json:"credits"`
	Tags            []string `json:"tags"`
	Aliases         []string `json:"aliases"`
	Vendor          string   `json:"vendor"`
	IsDefault       bool     `json:"isDefault"`
	MaxInputTokens  int64    `json:"maxInputTokens"`
	MaxOutputTokens int64    `json:"maxOutputTokens"`
	MaxAllowedSize  int64    `json:"maxAllowedSize"`
	Disabled        bool     `json:"disabled"`
	SupportsImages  bool     `json:"supportsImages"`
	// 输入模态（issue #109）：兼容两种上游形态——扁平 inputModalities 数组，
	// 或 models.dev 风格的 modalities.input。空 = 未下发。
	InputModalities []string `json:"inputModalities"`
	Modalities      struct {
		Input []string `json:"input"`
	} `json:"modalities"`
	SupportsReason bool `json:"supportsReasoning"`
	SupportsTool   bool `json:"supportsToolCall"`
	OnlyReasoning  bool `json:"onlyReasoning"`
	Reasoning      struct {
		Effort             string   `json:"effort"`
		Summary            string   `json:"summary"`
		DefaultEffort      string   `json:"defaultEffort"`
		CanDisableThinking bool     `json:"canDisableThinking"`
		SupportedEfforts   []string `json:"supportedEfforts"`
	} `json:"reasoning"`
}

// modelInfo 按解析条目构造 ModelInfo（dynEntry→ModelInfo 映射的单一事实来源）。
// defaultEffort 新老双键兼容：defaultEffort 优先，缺省回落 effort。
func (m *dynModelEntry) modelInfo() ModelInfo {
	def := m.Reasoning.DefaultEffort
	if def == "" {
		def = m.Reasoning.Effort
	}

	return ModelInfo{
		ID:                 m.ID,
		Name:               m.Name,
		ContextWindow:      m.MaxInputTokens,
		MaxTokens:          m.MaxOutputTokens,
		Efforts:            m.Reasoning.SupportedEfforts,
		DefaultEffort:      def,
		SupportsImages:     m.SupportsImages,
		InputModalities:    firstNonEmptyModalities(m.InputModalities, m.Modalities.Input),
		Description:        m.Description,
		Credits:            m.Credits,
		Tags:               m.Tags,
		Vendor:             m.Vendor,
		IsDefault:          m.IsDefault,
		SupportsReasoning:  m.SupportsReason,
		SupportsToolCall:   m.SupportsTool,
		OnlyReasoning:      m.OnlyReasoning,
		MaxAllowedSize:     m.MaxAllowedSize,
		CanDisableThinking: m.Reasoning.CanDisableThinking,
		ReasoningEffort:    m.Reasoning.Effort,
		ReasoningSummary:   m.Reasoning.Summary,
		Aliases:            m.Aliases,
	}
}

// nonChatModel 判定是否非对话模型（应从模型列表过滤掉）。
// 来源：harness buddy.ts:547-555。三类规则：
//   - id 前缀 nes-/completion-/codewise-：嵌入/补全/代码专用模型，选了报 code=11102。
//   - maxOutputTokens ≤ 256：tiny 输出非对话模型。
//   - tags 含生成类标签（图片/视频）：生成模型走各自专用端点，作为对话模型
//     选上去只会报 11102，非本网关用途。
//
// 生成类标签随上游扩充：早期只有 text-to-image，桌面端目录（2026-10-02 实测）
// 另有 text-to-video / image-to-video（seedance 系列）与 image-to-image
// （gpt-image 系列）——后者已由 text-to-image 覆盖，此处补齐视频两类。
// 注意本函数 CN 与 global 共用，新增标签对两域同时生效。
func nonChatModel(id string, maxOutputTokens int64, tags []string) bool {
	id = strings.ToLower(strings.TrimSpace(id))
	for _, p := range [...]string{"nes-", "completion-", "codewise-"} {
		if strings.HasPrefix(id, p) {
			return true
		}
	}

	if maxOutputTokens > 0 && maxOutputTokens <= 256 {
		return true
	}

	for _, t := range tags {
		switch t {
		case "text-to-image", "image-to-image", "text-to-video", "image-to-video":
			return true
		}
	}

	return false
}

// codeBuddyIDEUA /v3/config 要求能解析出 CodeBuddy 版本号的 UA。
// CLI 三段式 WorkBuddy UA 会拿到精简目录（flash 输出 128K、无 supportedEfforts）；
// 官方 IDE 头 `CodeBuddyIDE/4.12.0 CodeBuddy/4.12.0` 才返回完整能力
// （flash：393216 + low/high/max）。
// 版本号需随上游 IDE 发版跟进：UAn 版本过旧时该端点可能同样返回精简目录。
const codeBuddyIDEUA = "CodeBuddyIDE/4.12.0 CodeBuddy/4.12.0"

// codeBuddyCLIUA CLI 三段式 UA。**实测（2026-09-22）该端点对不同 UA 下发的模型集合不同**：
//   - IDE UA  → 14 条（10 个 chat：含 o4-mini / enhance-1.0 / auto-chat，**无 deepseek 系列**）
//   - CLI UA  → 22 条（22 个 chat：**含 deepseek-v4.1-flash / deepseek-v4.1-flash-sg /
//     gpt-6-astra / kimi-k2.8-preview**，但无 o4-mini / enhance-1.0 / auto-chat）
//
// 注意两点，都与旧注释相反，勿再按旧注释推断：
//  1. 旧注释称「CLI UA 拿到精简目录、IDE UA 才返回完整能力」——实测模型数量恰好相反，
//     但 **IDE 响应体积更大**（26003B vs 21111B），故「完整能力」应理解为**单条字段更全**，
//     而非模型更多。两路各有独有模型，缺一不可。
//  2. 该常量用于 global 侧第二路探测；CN 侧按官方客户端同时探测桌面端 UA 与 IDE UA。
const codeBuddyCLIUA = "CLI/2.63.2 CodeBuddy/2.63.2"

// FetchModels 调上游动态模型接口（CN 侧；global 账号见 global_models.go 家族）。
//
// v3-config-merge：动态目录 = /v3/config（主，IDE UA 完整能力版）+ 企业端点
// （/console，cli 面过滤，补缺）的并集，两路**并发**探测。合并去重 key = 模型 id，
// v3 条目优先（credits 等字段以 v3 为准），企业端点只补 v3 缺失的模型。
// /v3 失败（400/网络错/解析失败）不拖累企业端点结果——降级为仅企业端点，warn 日志；
// 反之亦然（两路独立容错）。
func (c *Client) FetchModels(account9 *auth.Auth) ([]ModelInfo, error) {
	type probeResult struct {
		infos []ModelInfo
		err   error
	}

	enterpriseCh := make(chan probeResult, 1)
	v3Ch := make(chan probeResult, 1)

	go func() {
		infos, err := c.fetchEnterpriseModels(account9)
		enterpriseCh <- probeResult{infos, err}
	}()
	go func() {
		infos, err := c.fetchV3Models(account9)
		v3Ch <- probeResult{infos, err}
	}()

	enterprise := <-enterpriseCh

	probeResult2 := <-v3Ch
	if enterprise.err != nil && probeResult2.err != nil {
		return nil, enterprise.err // 两路全失败：返回企业端点错误（既有调用方语义零漂移）
	}

	if probeResult2.err != nil {
		// /v3 失败降级：不拖累企业端点结果（降级仅企业端点 + warn）。
		log.Printf("WARN: [upstream] fetch models: v3/config probe failed (degraded to enterprise endpoint): %v", probeResult2.err)
	}

	if enterprise.err != nil {
		log.Printf("WARN: [upstream] fetch models: enterprise endpoint failed (v3/config only): %v", enterprise.err)
	}

	out := mergeModelInfos(probeResult2.infos, enterprise.infos)
	if len(out) == 0 {
		return nil, errModelsAPIReturnedEmptyList
	}

	c.storeModelRates(account9.Realm(), out)
	c.storeModelAliases(account9.Realm(), out)
	// 刷新 effort 能力缓存（供请求体降级；无 supportedEfforts 的模型不入桶）。
	// 空桶时跳过写：避免「某探测无档位数据」清掉既有桶。
	cache, defCache := buildEffortCaches(out)
	if len(cache) == 0 && len(defCache) == 0 {
		return out, nil
	}
	// 按探测账号的 realm 写入对应桶：CN 探测只进 cn 桶，global 同模型名不被污染（C-2）。
	c.storeEfforts(account9.Realm(), cache, defCache)

	return out, nil
}

// buildEffortCaches 从模型目录抽取 effort 能力桶：有 supportedEfforts 的入 cache，
// 有 defaultEffort 的入 defCache（供请求体降级）。
func buildEffortCaches(out []ModelInfo) (cache map[string][]string, defCache map[string]string) {
	cache = make(map[string][]string, len(out))

	defCache = make(map[string]string, len(out))
	for i := range out {
		modelInfo := &out[i]
		if len(modelInfo.Efforts) > 0 {
			cache[modelInfo.ID] = modelInfo.Efforts
		}

		if modelInfo.DefaultEffort != "" {
			defCache[modelInfo.ID] = modelInfo.DefaultEffort
		}
	}

	return cache, defCache
}

// mergeModelInfos 合并两路模型目录：primary 为主（同 id 以 primary 条目为准——
// credits 等字段以主端点为权威），secondary 只补 primary 缺失的 id。
// 去重 key = 模型 id；输出顺序 = primary 原序在前、secondary 补充项（secondary 原序）
// 在后——稳定输出，不依赖 map 迭代序。
func mergeModelInfos(primary, secondary []ModelInfo) []ModelInfo {
	if len(secondary) == 0 {
		return primary
	}

	seen := make(map[string]bool, len(primary)+len(secondary))

	out := make([]ModelInfo, 0, len(primary)+len(secondary))
	for i := range primary {
		modelInfo := &primary[i]
		if modelInfo.ID == "" || seen[modelInfo.ID] {
			continue
		}

		seen[modelInfo.ID] = true
		out = append(out, *modelInfo)
	}

	for i := range secondary {
		modelInfo2 := &secondary[i]
		if modelInfo2.ID == "" || seen[modelInfo2.ID] {
			continue
		}

		seen[modelInfo2.ID] = true
		out = append(out, *modelInfo2)
	}

	return out
}

// fetchEnterpriseModels 单路探测企业模型端点（/console/enterprises/personal/models）。
// 解析口径：agents[cli].models 过滤 + nonChatModel 剔除 + disabled 剔除。
func (c *Client) fetchEnterpriseModels(account7 *auth.Auth) ([]ModelInfo, error) {
	// CN 端点顺序：/console 兼容旧路径；空/缺 agents[cli] 时回落 /v2 企业端点，
	// /v2 仍无 cli 过滤集时接受完整 models（官方目录兜底）。
	var lastErr error

	for i, path := range []string{"/console/enterprises/personal/models", "/v2/enterprises/personal/models"} {
		out, err := c.fetchEnterpriseModelsPath(account7, path, i > 0)
		if err == nil {
			return out, nil
		}

		lastErr = err
	}

	return nil, lastErr
}

// fetchEnterpriseModelsPath 单端点探测；allowFull 允许在缺少 agents[cli] 时使用完整 models。
func (c *Client) fetchEnterpriseModelsPath(account7 *auth.Auth, path string, allowFull bool) ([]ModelInfo, error) {
	endpoint := c.chatBase(account7) + path

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("fetchEnterpriseModels: %w", err)
	}

	c.CommonHeaders(req, account7) // 复用共享请求头（Origin/Referer/UA/Accept/Content-Type）
	injectEnterpriseHeaders(req, account7)
	injectChatDomain(req, account7)
	// AccessToken 加锁快照（见 auth.AccessTokenValue：keepalive 刷新在 a.mu 内改写）。
	req.Header.Set("Authorization", "Bearer "+account7.AccessTokenValue())

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetchEnterpriseModels: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		// 读失败 → 传输层错误（handler 侧该路径不 NoteError）。
		return nil, fmt.Errorf("read body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models api status %d: %s: %w", resp.StatusCode, truncate(string(raw), 120), errFetchEnterpriseModelsFailed)
	}

	cliIDs, dynMap, err := parseEnterpriseModels(raw, allowFull)
	if err != nil {
		return nil, err
	}

	out := collectEnterpriseModels(cliIDs, dynMap)
	if len(out) == 0 {
		return nil, errModelsAPIReturnedEmptyList
	}

	return out, nil
}

// enterpriseModelsEnv /console/enterprises/personal/models 响应信封。
type enterpriseModelsEnv struct {
	Code int `json:"code"`
	Data struct {
		Models          []dynModelEntry `json:"models"`
		AvailableModels modelIDList     `json:"availableModels"`
		Agents          []struct {
			Name   string   `json:"name"`
			Models []string `json:"models"`
		} `json:"agents"`
	} `json:"data"`
}

// parseEnterpriseModels 解析企业端点响应：取 agents[cli].models 过滤集，并以
// nonChatModel 规则剔除的模型字段建 dynMap。
func parseEnterpriseModels(raw []byte, allowFull bool) ([]string, map[string]dynModelEntry, error) {
	var env enterpriseModelsEnv
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, nil, fmt.Errorf("models parse: %w", err)
	}

	if env.Code != 0 {
		return nil, nil, fmt.Errorf("models api code=%d: %w", env.Code, errParseEnterpriseModelsFailed)
	}

	var cliIDs []string

	for _, ag := range env.Data.Agents {
		if ag.Name == "cli" {
			cliIDs = ag.Models
			break
		}
	}

	if len(cliIDs) == 0 {
		if !allowFull {
			return nil, nil, errNoCLIAgentModelsFound
		}

		cliIDs = make([]string, 0, len(env.Data.Models))
		for i := range env.Data.Models {
			cliIDs = append(cliIDs, env.Data.Models[i].ID)
		}
	}

	cliIDs = filterModelIDsByAvailable(cliIDs, env.Data.AvailableModels)
	// dynMap 收集模型字段；nonChatModel 过滤在写入 dynMap 前执行，
	// 确保非对话条目（nes-/completion-/codewise- 前缀、maxOutputTokens≤256、
	// tags 含 text-to-image）根本不进返回列表（来源：harness buddy.ts:547-555）。
	dynMap := make(map[string]dynModelEntry, len(env.Data.Models))
	for i := range env.Data.Models {
		m := &env.Data.Models[i]
		if nonChatModel(m.ID, m.MaxOutputTokens, m.Tags) {
			continue
		}

		dynMap[m.ID] = *m
	}

	return cliIDs, dynMap, nil
}

// collectEnterpriseModels 按 cli 过滤集从 dynMap 取可用模型（disabled 剔除）。
func collectEnterpriseModels(cliIDs []string, dynMap map[string]dynModelEntry) []ModelInfo {
	out := make([]ModelInfo, 0, len(cliIDs))
	for _, id := range cliIDs {
		if m, ok := dynMap[id]; ok && !m.Disabled {
			out = append(out, m.modelInfo())
		}
	}

	return out
}

// fetchV3Models 并发探测 /v3/config（桌面端 UA 为主 + IDE UA 补缺，见 codeBuddyIDEUA）。
// v3 面取全量 models（不按 agents[cli] 过滤，与 global 探测口径一致），按同一
// nonChatModel 规则剔除非对话条目（selected 会选模型报 code=11102）。
// 失败返回错误（调用方降级为仅企业端点）。
func (c *Client) fetchV3Models(account *auth.Auth) ([]ModelInfo, error) {
	// CN 官方客户端同时用桌面端 UA 与 IDE UA 探测 /v3/config：两路模型集合不同，
	// 合并去重。桌面端为主（字段权威），IDE 只补缺失 id。
	type result struct {
		models []ModelInfo
		err    error
	}

	desktopCh := make(chan result, 1)
	ideCh := make(chan result, 1)

	go func() {
		models, err := c.v3ModelsForUA(account, c.defaultWorkBuddyUAFor(account))
		desktopCh <- result{models: models, err: err}
	}()
	go func() {
		models, err := c.v3ModelsForUA(account, codeBuddyIDEUA)
		ideCh <- result{models: models, err: err}
	}()

	desktop := <-desktopCh
	ide := <-ideCh

	switch {
	case desktop.err != nil && ide.err != nil:
		return nil, desktop.err
	case desktop.err != nil:
		return ide.models, nil //nolint:nilerr // 单路失败降级为另一路结果
	case ide.err != nil:
		return desktop.models, nil //nolint:nilerr // 单路失败降级为另一路结果
	default:
		out := mergeModelInfos(desktop.models, ide.models)
		if len(out) == 0 {
			return nil, errV3ConfigReturnedEmptyModels
		}

		return out, nil
	}
}

// v3ModelsForUA 单 UA 探测 /v3/config 并按 id 稳定排序输出（map 迭代序不可依赖）。
func (c *Client) v3ModelsForUA(a *auth.Auth, ua string) (models []ModelInfo, err error) {
	byID, err := c.fetchV3ConfigModelMap(a, ua)
	if err != nil {
		return nil, err
	}

	ids := make([]string, 0, len(byID))
	for id := range byID {
		mi := byID[id]
		if nonChatModel(mi.ID, mi.MaxTokens, mi.Tags) {
			continue
		}

		ids = append(ids, id)
	}

	sort.Strings(ids)

	out := make([]ModelInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, byID[id])
	}

	if len(out) == 0 {
		return nil, errV3ConfigReturnedEmptyModels
	}

	return out, nil
}

// v3ModelPromotion /v3/config data.modelPromotions 单条优惠定义（2026-09-23 实测
// 7 条：deepseek 系错峰五折、glm-5.2 夜间五折、hy3 与 hy4-preview-f 限时免费）。
// discount 只在部分条目上存在：有 factor 的可算生效价；「错峰使用」类只有时段
// 文案（factor 藏在 hover 文本里，无机器可读值），仅透出标签与说明。
type v3ModelPromotion struct {
	Enabled  bool     `json:"enabled"`
	Priority int      `json:"priority"`
	ModelIDs []string `json:"modelIds"`
	Badge    *struct {
		Label string `json:"label"`
	} `json:"badge"`
	Discount *struct {
		DiscountedCredits string  `json:"discountedCredits"`
		Factor            float64 `json:"factor"`
	} `json:"discount"`
	Hover *struct {
		TextZh string `json:"textZh"`
	} `json:"hover"`
	Schedule *v3PromoSchedule `json:"schedule"`
}

// v3PromoSchedule 优惠的生效时段定义（daily 窗口可跨午夜）。
type v3PromoSchedule struct {
	Daily      []v3PromoWindow `json:"daily"`
	Timezone   string          `json:"timezone"`  // 实测恒 Asia/Shanghai
	ValidFrom  string          `json:"validFrom"` // RFC3339，可缺省
	ValidUntil string          `json:"validUntil"`
}

// v3PromoWindow 单条每日时段，Start/End 为 "HH:MM"（End 可小于 Start 表示跨午夜）。
type v3PromoWindow struct {
	Start string `json:"start"` // "23:00"
	End   string `json:"end"`   // "7:50"（可跨午夜）
}

// promoZone 优惠时区：上游恒 Asia/Shanghai（UTC+8 无夏令时），用 FixedZone 免依赖
// 系统 tzdata（Windows 无 IANA 库时 LoadLocation 会失败）。
var promoZone = time.FixedZone("CST", 8*3600)

// promoClock 解析 "HH:MM" 为当日分钟数；坏值返回 (-1, false)。
func promoClock(hhmm string) (int, bool) {
	parts := strings.Split(hhmm, ":")
	if len(parts) != 2 {
		return -1, false
	}

	num, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))

	m, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil || num < 0 || num > 24 || m < 0 || m > 59 {
		return -1, false
	}

	return num*60 + m, true
}

// promoActive 评估优惠在 now 是否生效：enabled + validFrom/validUntil 内 + 落在
// 任一 daily 窗口（支持跨午夜，如 23:00→7:50）。schedule 为 nil 视为全天生效。
func promoActive(p *v3ModelPromotion, now time.Time) bool {
	if !p.Enabled {
		return false
	}

	v3PromoSchedule2 := p.Schedule
	if v3PromoSchedule2 == nil {
		return true
	}

	if !promoWithinValidRange(v3PromoSchedule2, now) {
		return false
	}

	if len(v3PromoSchedule2.Daily) == 0 {
		return true
	}

	return promoInDailyWindow(v3PromoSchedule2.Daily, now)
}

// promoWithinValidRange 报告 now 是否落在 validFrom/validUntil 区间内（缺省或
// 解析失败视为不设限）。
func promoWithinValidRange(v3PromoSchedule2 *v3PromoSchedule, now time.Time) bool {
	if v3PromoSchedule2.ValidFrom != "" {
		if from, err := time.Parse(time.RFC3339, v3PromoSchedule2.ValidFrom); err == nil && now.Before(from) {
			return false
		}
	}

	if v3PromoSchedule2.ValidUntil != "" {
		if until, err := time.Parse(time.RFC3339, v3PromoSchedule2.ValidUntil); err == nil && !now.Before(until) {
			return false
		}
	}

	return true
}

// promoInDailyWindow 报告 now 是否落在任一 daily 窗口内（支持跨午夜，如 23:00→7:50）。
func promoInDailyWindow(daily []v3PromoWindow, now time.Time) bool {
	cur := now.Hour()*60 + now.Minute()

	for _, w := range daily {
		num, ok1 := promoClock(w.Start)

		num2, ok2 := promoClock(w.End)
		if !ok1 || !ok2 {
			continue
		}

		if num <= num2 {
			if cur >= num && cur < num2 {
				return true
			}

			continue
		}

		if cur >= num || cur < num2 { // 跨午夜（23:00→7:50）
			return true
		}
	}

	return false
}

// applyModelPromotions 把当前生效的优惠挂到目录条目：同模型多条命中取 priority
// 最高（实测 glm-5.2 白天 badge-only(50) 与夜间五折(100) 靠 priority+daily 双轨
// 切换）。无 discount 对象的条目也挂标签/说明（错峰类），PromoFactor 留 nil。
func applyModelPromotions(out map[string]ModelInfo, promos []v3ModelPromotion) {
	if len(promos) == 0 || len(out) == 0 {
		return
	}

	now := time.Now().In(promoZone)
	for id, p := range bestPromotions(out, promos, now) {
		mi := out[id]
		applyPromo(&mi, p)
		out[id] = mi
	}
}

// bestPromotions 选出每个目录内模型当前生效的优惠：同模型多条命中取 priority 最高
// （实测 glm-5.2 白天 badge-only(50) 与夜间五折(100) 靠 priority+daily 双轨切换）。
func bestPromotions(out map[string]ModelInfo, promos []v3ModelPromotion, now time.Time) map[string]*v3ModelPromotion {
	best := make(map[string]*v3ModelPromotion)
	bestPrio := make(map[string]int)

	for i := range promos {
		v3ModelPromotion2 := &promos[i]
		if !promoActive(v3ModelPromotion2, now) {
			continue
		}

		for _, id := range v3ModelPromotion2.ModelIDs {
			if _, ok := out[id]; !ok {
				continue // 目录外模型（如同名 global 变体）不挂
			}

			if prio, seen := bestPrio[id]; !seen || v3ModelPromotion2.Priority > prio {
				best[id] = v3ModelPromotion2
				bestPrio[id] = v3ModelPromotion2.Priority
			}
		}
	}

	return best
}

// applyPromo 把单条优惠的标签/说明/折扣价挂到目录条目（无 discount 对象的条目也
// 挂标签/说明，错峰类 PromoFactor 留 nil）。
func applyPromo(modelInfo *ModelInfo, v3ModelPromotion2 *v3ModelPromotion) {
	if v3ModelPromotion2.Badge != nil {
		modelInfo.PromoLabel = v3ModelPromotion2.Badge.Label
	}

	if v3ModelPromotion2.Hover != nil {
		modelInfo.PromoNote = v3ModelPromotion2.Hover.TextZh
	}

	if v3ModelPromotion2.Discount != nil {
		f := v3ModelPromotion2.Discount.Factor
		modelInfo.PromoFactor = &f
		modelInfo.PromoCredits = v3ModelPromotion2.Discount.DiscountedCredits
	}
}

// storeEfforts 按 realm 写入 effort 能力缓存桶（efforts + defaultEfforts），并发安全。
// 供 CN FetchModels 与 global 探测共用：拉取到的模型档位落桶后，出站请求体
// normalizeReasoningEffort 才能按域降级。efforts 与 defs 均空时删除该 realm 桶
// （等价「该域无可降级档位」）。调用方负责在「无新数据」时跳过写。
func (c *Client) storeEfforts(realm string, efforts map[string][]string, defs map[string]string) {
	c.effortsMu.Lock()
	defer c.effortsMu.Unlock()

	if c.efforts == nil {
		c.efforts = make(map[string]map[string][]string)
	}

	if c.defaultEfforts == nil {
		c.defaultEfforts = make(map[string]map[string]string)
	}

	k := realmKey(realm)
	if len(efforts) == 0 && len(defs) == 0 {
		delete(c.efforts, k)
		delete(c.defaultEfforts, k)

		return
	}
	// 写入边界克隆一次（刷新频率低），让读路径直接返回不可变快照，无需每请求拷贝。
	c.efforts[k] = cloneEffortMap(efforts)
	c.defaultEfforts[k] = cloneStringMap(defs)
}

func cloneEffortMap(src map[string][]string) map[string][]string {
	if len(src) == 0 {
		return nil
	}

	dst := make(map[string][]string, len(src))
	for k, v := range src {
		dst[k] = append([]string(nil), v...)
	}

	return dst
}

func cloneStringMap(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}

	dst := make(map[string]string, len(src))
	maps.Copy(dst, src)

	return dst
}

// normalizeModelRate 把上游倍率原文规范化为可比较的数值键。
// 兼容 "x0.05" / "x0.05 credits" / "0.50x" 等形态；无法数值化时保留去除
// credits 后缀与空白后的原文，避免编造倍率。
func normalizeModelRate(raw string) string {
	str := strings.TrimSpace(raw)
	if str == "" {
		return ""
	}

	if strings.HasSuffix(strings.ToLower(str), "credits") {
		str = strings.TrimSpace(str[:len(str)-len("credits")])
	}

	if strings.HasPrefix(strings.ToLower(str), "x") {
		str = strings.TrimSpace(str[1:])
	} else if strings.HasSuffix(strings.ToLower(str), "x") {
		str = strings.TrimSpace(str[:len(str)-1])
	}

	if str == "" {
		return ""
	}

	v, err := strconv.ParseFloat(str, 64)
	if err != nil {
		return strings.TrimSpace(raw)
	}

	return strconv.FormatFloat(v, 'f', -1, 64)
}

// effectiveModelRate 返回模型当前生效倍率：有机器可读优惠时取折扣价，
// 否则取牌价；两者均缺省时为空。
func effectiveModelRate(mi *ModelInfo) string {
	if mi.PromoFactor != nil && strings.TrimSpace(mi.PromoCredits) != "" {
		return normalizeModelRate(mi.PromoCredits)
	}

	return normalizeModelRate(mi.Credits)
}

// storeModelRates 按 realm 整体替换模型倍率快照。目录成功刷新但没有可解析
// 倍率时写入空桶，使旧倍率不会继续冒充当前价。
func (c *Client) storeModelRates(realm string, infos []ModelInfo) {
	rates := make(map[string]string, len(infos))
	for i := range infos {
		modelInfo := &infos[i]
		if modelInfo.ID == "" {
			continue
		}

		if rate := effectiveModelRate(modelInfo); rate != "" {
			rates[modelInfo.ID] = rate
		}
	}

	c.effortsMu.Lock()
	defer c.effortsMu.Unlock()

	if c.modelRates == nil {
		c.modelRates = make(map[string]map[string]string)
	}

	c.modelRates[realmKey(realm)] = rates
}

// storeModelAliases 用最新目录整体替换指定 realm 的别名映射。
func (c *Client) storeModelAliases(realm string, infos []ModelInfo) {
	aliases := make(map[string]string)

	for i := range infos {
		info := &infos[i]
		for _, alias := range info.Aliases {
			alias = strings.TrimSpace(alias)
			if alias == "" || alias == info.ID {
				continue
			}

			aliases[alias] = info.ID
		}
	}

	c.aliasMu.Lock()
	if c.aliases == nil {
		c.aliases = map[string]map[string]string{}
	}

	c.aliases[realmKey(realm)] = aliases
	c.aliasMu.Unlock()
}

// UpstreamAliases 返回指定 realm 的上游别名 → 规范 ID 快照（只读副本）。
func (c *Client) UpstreamAliases(realm string) map[string]string {
	c.aliasMu.RLock()
	defer c.aliasMu.RUnlock()

	src := c.aliases[realmKey(realm)]
	if len(src) == 0 {
		return nil
	}

	out := make(map[string]string, len(src))
	maps.Copy(out, src)

	return out
}

// ResolveUpstreamAlias 把请求模型裸名解析为上游目录里的规范模型 ID。
func (c *Client) ResolveUpstreamAlias(realm, model string) (string, bool) {
	c.aliasMu.RLock()
	defer c.aliasMu.RUnlock()

	canonical, ok := c.aliases[realmKey(realm)][model]

	return canonical, ok && canonical != ""
}

// ModelRate 返回最近成功刷新的指定域模型生效倍率；未知返回空串。
func (c *Client) ModelRate(realm, model string) string {
	if c == nil || model == "" {
		return ""
	}

	c.effortsMu.RLock()
	defer c.effortsMu.RUnlock()

	return c.modelRates[realmKey(realm)][model]
}

// GlobalEffortSnapshot 导出 global 域 effort 能力缓存，供 /v1/models 输出
// reasoning_supported_efforts / reasoning_default_effort。返回只读快照（调用方
// 不得修改）；桶未填充（无 global 账号或从未探测）→ nil（调用方回落静态兜底表）。
func (c *Client) GlobalEffortSnapshot() (efforts map[string][]string, defaults map[string]string) {
	return c.effortsSnapshot("global"), c.defaultEffortsSnapshot("global")
}

// v3ConfigDomain /v3/config 的 X-Domain：优先账号落盘 domain，否则 chatBase host。
func v3ConfigDomain(a *auth.Auth, chatBase string) string {
	if a != nil {
		// Domain 加锁快照（见 auth.DomainValue：keepalive 刷新在 a.mu 内改写）。
		if d := strings.TrimSpace(a.DomainValue()); d != "" {
			d = strings.TrimPrefix(d, "https://")
			d = strings.TrimPrefix(d, "http://")

			return strings.TrimSuffix(d, "/")
		}
	}

	if u, err := url.Parse(chatBase); err == nil && u.Host != "" {
		return u.Host
	}

	return "copilot.tencent.com"
}

// fetchV3ConfigModelMap 拉官方 IDE 配置目录，按模型 id 建能力表。
// 该端点对 UA 敏感：必须带 CodeBuddy/CodeBuddyIDE 版本，否则 400 code=12403。
// ua 为该次请求的 User-Agent；空串等价 codeBuddyIDEUA。该端点对 UA 敏感且**不同 UA 下发
// 不同模型集合**（见 codeBuddyCLIUA 注释），global 探测据此并发两路取并集。
func (c *Client) fetchV3ConfigModelMap(a *auth.Auth, ua string) (map[string]ModelInfo, error) {
	req, err := c.newV3ConfigRequest(a, ua)
	if err != nil {
		return nil, err
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetchV3ConfigModelMap: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		// 读失败 → 传输层错误：半截 body 不进解析（不罚号）。
		return nil, fmt.Errorf("read body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("v3/config status %d: %s: %w", resp.StatusCode, truncate(string(raw), 120), errFetchV3ConfigModelMapFailed)
	}

	env, err := parseV3ConfigEnv(raw)
	if err != nil {
		return nil, err
	}

	out := buildV3ModelMap(env.Data.Models)
	mergeTrialBannerModels(out, env.Data.ProductFeaturesConfig.ModelTrialBanner.Banners)
	filterModelMapByAvailable(out, env.Data.AvailableModels)
	// 挂当前生效的限时优惠（modelPromotions）：Credits 字段是**牌价**（转正后基准
	// 倍率，如 hy4-preview-f 的 x0.29），而 WorkBuddy 客户端显示的是生效价（试用/
	// 折扣窗口内 factor 打折）——面板据此展示「生效价 + 标签 + 牌价」。
	applyModelPromotions(out, env.Data.ModelPromotions)

	if len(out) == 0 {
		return nil, errV3ConfigReturnedEmptyModels
	}

	return out, nil
}

// newV3ConfigRequest 构造 /v3/config 请求（该端点对 UA 敏感：必须带
// CodeBuddy/CodeBuddyIDE 版本，否则 400 code=12403；空串等价 codeBuddyIDEUA）。
func (c *Client) newV3ConfigRequest(account8 *auth.Auth, str string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, c.chatBase(account8)+"/v3/config", http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("newV3ConfigRequest: %w", err)
	}

	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	// AccessToken 加锁快照（同 fetchEnterpriseModels）。
	req.Header.Set("Authorization", "Bearer "+account8.AccessTokenValue())

	if account8 != nil && account8.UID != "" {
		req.Header.Set("X-User-Id", account8.UID)
	}

	injectEnterpriseHeaders(req, account8)
	req.Header.Set("X-Domain", v3ConfigDomain(account8, c.chatBase(account8)))
	req.Header.Set("X-Product", "SaaS")

	if str == "" {
		str = codeBuddyIDEUA
	}

	req.Header.Set("User-Agent", str)
	c.injectCodeBuddyRequest(req)

	return req, nil
}

// v3ConfigEnv /v3/config 响应信封。
type v3ConfigEnv struct {
	Code int `json:"code"`
	Data struct {
		Models          []dynModelEntry `json:"models"`
		AvailableModels modelIDList     `json:"availableModels"`
		// 试用模型横幅：上游把「N 天免费试用」的模型放在这里，**不在 data.models 里**。
		// 实测 global 侧 hy4-preview-f 只出现在此（modelId=hy4-preview-f、
		// targetModelId=hy4-preview、trialDays=14），纯 data.models 解析会漏掉它。
		ProductFeaturesConfig v3ProductFeaturesConfig `json:"productFeaturesConfig"`
		ModelPromotions       []v3ModelPromotion      `json:"modelPromotions"`
	} `json:"data"`
}

// v3ProductFeaturesConfig /v3/config data.productFeaturesConfig。
type v3ProductFeaturesConfig struct {
	ModelTrialBanner struct {
		Banners []v3TrialBanner `json:"banners"`
	} `json:"ModelTrialBanner"`
}

// v3TrialBanner 单条试用模型横幅。
type v3TrialBanner struct {
	ModelID       string `json:"modelId"`
	TargetModelID string `json:"targetModelId"`
}

// parseV3ConfigEnv 解析 /v3/config 响应并校验业务 code。
func parseV3ConfigEnv(raw []byte) (v3ConfigEnv, error) {
	var env v3ConfigEnv
	if err := json.Unmarshal(raw, &env); err != nil {
		return v3ConfigEnv{}, fmt.Errorf("v3/config parse: %w", err)
	}

	if env.Code != 0 {
		return v3ConfigEnv{}, fmt.Errorf("v3/config code=%d: %w", env.Code, errParseV3ConfigEnvFailed)
	}

	return env, nil
}

// stripCustomLocalModelPrefix 去掉官方客户端的本地模型前缀。
func stripCustomLocalModelPrefix(id string) string {
	return strings.TrimPrefix(id, "custom-local:")
}

// availableModelSet 把 availableModels 归一为集合（空 = 不限制）。
func availableModelSet(available []string) map[string]bool {
	if len(available) == 0 {
		return nil
	}

	set := make(map[string]bool, len(available))
	for _, id := range available {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}

		set[id] = true
		set[stripCustomLocalModelPrefix(id)] = true
	}

	if len(set) == 0 {
		return nil
	}

	return set
}

// filterModelIDsByAvailable 按 availableModels 过滤模型 id；空集合不限制。
func filterModelIDsByAvailable(ids, available []string) []string {
	set := availableModelSet(available)
	if set == nil {
		return ids
	}

	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if set[id] || set[stripCustomLocalModelPrefix(id)] {
			out = append(out, id)
		}
	}

	return out
}

// filterModelMapByAvailable 按 availableModels 过滤模型能力表；空集合不限制。
func filterModelMapByAvailable(out map[string]ModelInfo, available []string) {
	set := availableModelSet(available)
	if set == nil {
		return
	}

	for id := range out {
		if !set[id] && !set[stripCustomLocalModelPrefix(id)] {
			delete(out, id)
		}
	}
}

// buildV3ModelMap 按模型 id 建能力表（空 id 跳过）。
func buildV3ModelMap(models []dynModelEntry) map[string]ModelInfo {
	out := make(map[string]ModelInfo, len(models))
	for i := range models {
		m := &models[i]
		if strings.TrimSpace(m.ID) == "" || m.Disabled {
			continue
		}

		out[m.ID] = m.modelInfo()
	}

	return out
}

// mergeTrialBannerModels 补入试用横幅模型（ModelTrialBanner）：上游把「N 天免费试用」
// 的模型只放在这里，data.models 里没有，故纯目录解析会漏（实测 global 侧
// hy4-preview-f 即如此，但该模型**实际可调用**）。
//
// 元数据口径：能力字段（context/maxTokens/efforts/reasoning 等）从 targetModelId
// 的既有条目继承——试用版与其转正目标是同族模型，能力应当一致；
// 但 **Credits 与 Tags 显式清空**——它们描述的是"转正后"的计费与营销信息
// （如 hy4-preview 的 x0.29 与 badge），用在免费试用版上会误导下游展示。
//
// firstUseTimeKey / trialDays 属**账号级**试用状态，不透出给下游。
func mergeTrialBannerModels(out map[string]ModelInfo, banners []v3TrialBanner) {
	for _, item := range banners {
		id := strings.TrimSpace(item.ModelID)
		if id == "" {
			continue
		}

		if _, exists := out[id]; exists {
			continue
		}

		modelInfo := ModelInfo{ID: id}

		if tgt := strings.TrimSpace(item.TargetModelID); tgt != "" {
			if base, ok := out[tgt]; ok {
				modelInfo = base
				modelInfo.ID = id
			}
		}

		modelInfo.Credits = ""
		modelInfo.Tags = nil
		out[id] = modelInfo
	}
}

// UserResource 查询账号积分余额与总额度（所有套餐聚合）。remain 负值钳 0；
// total 取与 remain 同源的额度字段（CycleCapacitySize 优先，无周期额度退
// CapacitySize），上游缺 size 的套餐按 remain 兜底，保证百分比不超 100%。
// CreditPackage 单个积分包的构成明细（面板「积分构成」用）。
//
// 两个账号即使任务完成度完全一致，余额也可能相差上千——差别藏在包的**面额与
// 来源**里（「国内运营裂变包」「拉新权益包」按次发放，面额 6~1500 不等）。
// 只看聚合值看不出这件事，所以把逐包明细暴露出来。
type CreditPackage struct {
	Name   string `json:"name"`
	Remain int64  `json:"remain"`
	Used   int64  `json:"used"`
	Size   int64  `json:"size"`
	// EndTime 该包的失效时刻：优先 DeductionEndTime（可抵扣窗口结束，真「用不完
	// 就没了」），缺失依次回落 ExpiredTime / PackageEndTime / CycleEndTime（周期
	// 边界，仅兜底）。RFC3339 或上游墙钟字符串，前端取日期部分展示。
	EndTime string `json:"end_time,omitempty"`
	// ExpiresAt 与 EndTime 同源的 Unix 毫秒时间戳，供面板按精确剩余天数聚合。
	ExpiresAt int64 `json:"expires_at,omitempty"`
	// CreatedAt 发放时刻，RFC3339。**这是区分「首登赠送」与「活动奖励」的唯一依据**：
	// 两类包的 PackageName 与 PackageCode 完全相同（例如都是「国内运营裂变包」+
	// TCACA_code_007_*），只看名字无法区分，只有时间能说明它是不是账号首次授权那刻发的。
	CreatedAt string `json:"created_at,omitempty"`
	// PackageCode / SubProductCode 上游的包类型标识。同 Name 不同 Code 的包可能
	// 是不同来源；同 Code 不同面额则是同来源分批发放（首登 1500 与活动 300 即如此）。
	PackageCode    string `json:"package_code,omitempty"`
	SubProductCode string `json:"sub_product_code,omitempty"`
	SubProductName string `json:"sub_product_name,omitempty"`
	// Cycle 为 true 表示按周期发放的包（读 Cycle* 字段），否则读 Capacity*。
	Cycle bool `json:"cycle,omitempty"`
}

// CreditPackages 返回账号当前的逐包构成。remain/size 为各包求和。
//
// 字段选择与 UserResourceDetailed 的聚合口径一致：CycleCapacitySize > 0 时按
// 周期字段算，否则按 Capacity 字段算——两条路径不能混，否则同一个包会被算两次。
func (c *Client) CreditPackages(account2 *auth.Auth) ([]CreditPackage, int64, int64, error) {
	now := time.Now()
	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format(packageEndLayout),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format(packageEndLayout),
	}

	data, err := c.billingMeterJSON(account2, c.billingMeterPaths(account2), body)
	if err != nil {
		return nil, 0, 0, err
	}
	// 注意层级：doJSON 已经解过 apiEnvelope 并返回 env.Data，所以这里从
	// Response 开始解析——**不能**再套一层 Code/Data，否则 Accounts 恒为空，
	// 表现为「每个号都 0 个包」（实测踩过）。
	var resp billingPackagesResp
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, 0, 0, fmt.Errorf("packages parse: %w", err)
	}

	packs := resp.Response.Data.Accounts
	out := make([]CreditPackage, 0, len(packs))

	var sumRemain, sumSize int64

	for i := range packs {
		cp := creditPackageFromAccount(&packs[i])
		sumRemain += cp.Remain
		sumSize += cp.Size
		out = append(out, cp)
	}
	// 面额降序：大包一眼可见，正是差异最可能出现的地方。
	sort.SliceStable(out, func(i, j int) bool { return out[i].Size > out[j].Size })

	return out, sumRemain, sumSize, nil
}

// modelIDList availableModels 兼容字符串数组与对象数组（对象取 id/modelId/model）。
type modelIDList []string

// UnmarshalJSON 解析 availableModels 的两种上游形态。
func (m *modelIDList) UnmarshalJSON(raw []byte) error {
	var values []any
	if err := json.Unmarshal(raw, &values); err != nil {
		return fmt.Errorf("availableModels parse: %w", err)
	}

	out := make([]string, 0, len(values))
	for _, value := range values {
		switch item := value.(type) {
		case string:
			if item != "" {
				out = append(out, item)
			}
		case map[string]any:
			for _, key := range []string{"id", "modelId", "model"} {
				if id, ok := item[key].(string); ok && id != "" {
					out = append(out, id)
					break
				}
			}
		}
	}

	*m = out

	return nil
}

// parsePreciseCapacity 解析上游精确容量字段。上游以 JSON 字符串下发（如 "100"），
// 空串表示未提供（按 0 处理）；非法值返回 ok=false，调用方回落整数容量字段。
func parsePreciseCapacity(raw string) (int64, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, true
	}

	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, false
	}

	return value, true
}

// preciseTriple 解析一组 (remain, used, size) 精确容量；size 缺失/非正时视为不可用。
func preciseTriple(remainRaw, usedRaw, sizeRaw string) (remain, used, size int64, ok bool) {
	size, ok = parsePreciseCapacity(sizeRaw)
	if !ok || size <= 0 {
		return 0, 0, 0, false
	}

	return preciseTripleSize(remainRaw, usedRaw, size)
}

// preciseTripleSize 在 size 已解析的前提下取 remain/used。
func preciseTripleSize(remainRaw, usedRaw string, total int64) (remain, used, size int64, ok bool) {
	remain, ok = parsePreciseCapacity(remainRaw)
	if !ok {
		return 0, 0, 0, false
	}

	used, ok = parsePreciseCapacity(usedRaw)
	if !ok {
		return 0, 0, 0, false
	}

	return normalizePreciseAmounts(remain, used, total)
}

// slicePeriodUsage 切片型额度（CapacityType=4）的周期用量明细。
type slicePeriodUsage struct {
	SlicePeriodCapacitySizePrecise   string `json:"SlicePeriodCapacitySizePrecise"`
	SlicePeriodCapacityRemainPrecise string `json:"SlicePeriodCapacityRemainPrecise"`
	SlicePeriodCapacityUsedPrecise   string `json:"SlicePeriodCapacityUsedPrecise"`
}

// billingPackagesResp 积分包列表响应信封（从 Response 起，见上方层级注释）。
type billingPackagesResp struct {
	Response struct {
		Data struct {
			Accounts []billingPackageAccount `json:"Accounts"`
		} `json:"Data"`
	} `json:"Response"`
}

// billingPackageAccount 单个积分包的上游字段。
type billingPackageAccount struct {
	PackageName                string             `json:"PackageName"`
	CapacityType               int                `json:"CapacityType"`
	SlicePeriodUsageDetails    []slicePeriodUsage `json:"SlicePeriodUsageDetails"`
	CapacityRemain             int64              `json:"CapacityRemain"`
	CapacityUsed               int64              `json:"CapacityUsed"`
	CapacitySize               int64              `json:"CapacitySize"`
	CapacityRemainPrecise      string             `json:"CapacityRemainPrecise"`
	CapacityUsedPrecise        string             `json:"CapacityUsedPrecise"`
	CapacitySizePrecise        string             `json:"CapacitySizePrecise"`
	CycleCapacityRemain        int64              `json:"CycleCapacityRemain"`
	CycleCapacityUsed          int64              `json:"CycleCapacityUsed"`
	CycleCapacitySize          int64              `json:"CycleCapacitySize"`
	CycleCapacityRemainPrecise string             `json:"CycleCapacityRemainPrecise"`
	CycleCapacityUsedPrecise   string             `json:"CycleCapacityUsedPrecise"`
	CycleCapacitySizePrecise   string             `json:"CycleCapacitySizePrecise"`
	// 到期时间字段名在上游存在三种口径：ExpiredTime / PackageEndTime
	// 在 CN/global 实测字段全集里均恒 miss（见 UserResourceDetailed
	// 处注释），真实下发的是 CycleEndTime——三者都读，谁有值用谁。
	ExpiredTime    string `json:"ExpiredTime"`
	PackageEndTime string `json:"PackageEndTime"`
	CycleEndTime   string `json:"CycleEndTime"`
	// DeductionEndTime 可抵扣窗口结束（epoch 毫秒）——「这个包什么时候
	// 不能再花」的真失效时刻。CycleEndTime 是周期边界（额度重置点），
	// 两者语义不同：判「用不完就没了」以本字段为准，CycleEndTime 兜底
	// （OkRoromori 分支实测结论：请求参数叫 PackageEndTimeRange*，但
	// 响应里 ExpiredTime 恒空，真正的失效时刻只有这里下发）。
	DeductionEndTime int64 `json:"DeductionEndTime"`
	// 发放时刻（epoch 毫秒）。
	CreateTime     int64  `json:"CreateTime"`
	PackageCode    string `json:"PackageCode"`
	SubProductCode string `json:"SubProductCode"`
	SubProductName string `json:"SubProductName"`
}

// creditPackageFromAccount 把单个上游积分包映射为面板用的 CreditPackage。
func creditPackageFromAccount(billingPackageAccount3 *billingPackageAccount) CreditPackage {
	creditPackage := CreditPackage{
		Name:           billingPackageAccount3.PackageName,
		PackageCode:    billingPackageAccount3.PackageCode,
		SubProductCode: billingPackageAccount3.SubProductCode,
		SubProductName: billingPackageAccount3.SubProductName,
		Remain:         0,
		Used:           0,
		Size:           0,
		EndTime:        "",
		ExpiresAt:      0,
		CreatedAt:      "",
		Cycle:          false,
	}

	creditPackage.EndTime, creditPackage.ExpiresAt = creditPackageExpiry(billingPackageAccount3)
	if creditPackage.ExpiresAt == 0 && creditPackage.EndTime != "" {
		if end, perr := time.ParseInLocation(packageEndLayout, creditPackage.EndTime, softRateResetLoc); perr == nil {
			creditPackage.ExpiresAt = end.UnixMilli()
		}
	}
	// CreateTime 是 epoch 毫秒；0 表示上游没给，留空而不是伪造 1970。
	if billingPackageAccount3.CreateTime > 0 {
		creditPackage.CreatedAt = time.UnixMilli(billingPackageAccount3.CreateTime).Format(time.RFC3339)
	}

	creditPackage.Cycle, creditPackage.Remain, creditPackage.Used, creditPackage.Size = creditPackageAmounts(billingPackageAccount3)

	return creditPackage
}

// creditPackageExpiry 按上游口径选出失效时刻：切片型（CapacityType=4）以
// CycleEndTime 为周期边界；其余 DeductionEndTime 优先（epoch 毫秒，真「用不完就
// 没了」），依次回落 ExpiredTime / PackageEndTime / CycleEndTime。
// 返回 RFC3339 文本与同源 Unix 毫秒（后者仅在 DeductionEndTime 分支有值）。
func creditPackageExpiry(billingPackageAccount4 *billingPackageAccount) (endTime string, expiresAt int64) {
	// 切片型（含每日免费包）以周期边界为准；其余按可抵扣窗口 → ExpiredTime →
	// CycleEndTime。
	if billingPackageAccount4.CapacityType == 4 && billingPackageAccount4.CycleEndTime != "" {
		return billingPackageAccount4.CycleEndTime, 0
	}

	switch {
	case billingPackageAccount4.DeductionEndTime > 0:
		// 真失效时刻（可抵扣窗口结束），语义见字段注释：判「用不完就没了」
		// 用它而不是周期边界。epoch 毫秒 → RFC3339，与 CycleEndTime 字符串口径
		// 共存（前端统一 slice(0,10) 取日期）。ExpiresAt 直接用原始毫秒——
		// RFC3339 不是 packageEndLayout 形态，交给调用方解析会静默失败得 0。
		return time.UnixMilli(billingPackageAccount4.DeductionEndTime).Format(time.RFC3339), billingPackageAccount4.DeductionEndTime
	case billingPackageAccount4.ExpiredTime != "":
		return billingPackageAccount4.ExpiredTime, 0
	case billingPackageAccount4.PackageEndTime != "":
		return billingPackageAccount4.PackageEndTime, 0
	default:
		return billingPackageAccount4.CycleEndTime, 0
	}
}

// creditPackageAmounts 按周期字段优先口径算出包的 remain/used/size（与
// UserResourceDetailed 的 packageRemainUsed 同构，两处口径不能混）。
func creditPackageAmounts(billingPackageAccount2 *billingPackageAccount) (cycle bool, remain, used, size int64) {
	if preciseRemain, preciseUsed, preciseSize, isCycle, ok := preciseAmounts(respAccount{
		CapacityType:               billingPackageAccount2.CapacityType,
		SlicePeriodUsageDetails:    billingPackageAccount2.SlicePeriodUsageDetails,
		CapacityRemainPrecise:      billingPackageAccount2.CapacityRemainPrecise,
		CapacityUsedPrecise:        billingPackageAccount2.CapacityUsedPrecise,
		CapacitySizePrecise:        billingPackageAccount2.CapacitySizePrecise,
		CycleCapacityRemainPrecise: billingPackageAccount2.CycleCapacityRemainPrecise,
		CycleCapacityUsedPrecise:   billingPackageAccount2.CycleCapacityUsedPrecise,
		CycleCapacitySizePrecise:   billingPackageAccount2.CycleCapacitySizePrecise,
	}); ok {
		return isCycle, preciseRemain, preciseUsed, preciseSize
	}

	if billingPackageAccount2.CycleCapacitySize > 0 {
		cycle = true
		remain, size = billingPackageAccount2.CycleCapacityRemain, billingPackageAccount2.CycleCapacitySize

		used = size - remain
		if billingPackageAccount2.CycleCapacityUsed > used {
			used = billingPackageAccount2.CycleCapacityUsed
			remain = size - used
		}

		if remain < 0 {
			remain = 0
		}

		return cycle, remain, used, size
	}

	remain, used, size = billingPackageAccount2.CapacityRemain, billingPackageAccount2.CapacityUsed, billingPackageAccount2.CapacitySize
	if used == 0 && size > remain {
		used = size - remain
	}

	return cycle, remain, used, size
}

func (c *Client) UserResource(a *auth.Auth) (remain, total int64, err error) {
	remain, total, _, err = c.UserResourceDetailed(a, 0)
	return remain, total, err
}

// packageEndLayout 上游套餐到期时间的墙钟格式（UTC+8，与 softRateResetLoc 同口径）。
const packageEndLayout = "2006-01-02 15:04:05"

// parsePackageEndTime 统一解析上游套餐到期时间。空值、格式异常返回 false，
// 调用方据此保守地不把该包计入最早到期路由。
func parsePackageEndTime(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}

	t, err := time.ParseInLocation(packageEndLayout, raw, softRateResetLoc)
	if err != nil {
		return time.Time{}, false
	}

	return t, true
}

// UserResourceDetailed 在 UserResource 基础上额外返回「快过期」积分子集：
// soon > 0 且套餐 CycleEndTime 解析成功且到期时刻 ≤ now+soon 的余额计入 expiring
// （pool 据此优先消耗，避免官方活动赠送的奖励积分到期作废）；soon ≤ 0 时 expiring
// 恒 0（禁用分桶，行为与引入前一致）。expiring 是 remain 的一部分。
//
// 到期时间判据是 CycleEndTime（上游实测：CN/global 两域字段全集均无 PackageEndTime，
// 旧判据恒 miss 致 expiring 恒 0；CycleEndTime 是上游真实下发的到期时刻——
// global Bonus Pack 14 天赠送积分的到期时间即此字段）。解析失败/缺失的套餐保守
// 不计入 expiring（不误标为快过期而插队）。
// 单套餐取数统一调 packageRemainUsed（与 CreditPackages 同一事实来源，含 remain
// 钳 [0,size] 与 used 修正；消除双份逻辑漂移——旧中间 switch 只钳负值，上游脏数据
// CycleRemain>Size 时会高估）。
func (c *Client) UserResourceDetailed(a *auth.Auth, soon time.Duration) (remain, total, expiring int64, err error) {
	remain, total, expiring, _, _, err = c.UserResourceDetailedWithExpiry(a, soon)

	return remain, total, expiring, err
}

// notifyBalance 触发余额观测回调（装配期注入，缺省 nil = 关闭）。panic 隔离：
// 账本是观测附属物，任何异常都不得打断余额查询主路径。
func (c *Client) notifyBalance(account *auth.Auth, remain, total int64) {
	if c.BalanceObserver == nil || account == nil {
		return
	}

	defer func() {
		if recovered := recover(); recovered != nil {
			log.Printf("WARN: [upstream] balance observer panic: %v", recovered)
		}
	}()

	c.BalanceObserver(account.UID, remain, total)
}

// enterpriseUnlimitedRemain / enterpriseUnlimitedTotal 企业版「不限量」在池内的等价表示
// （上游 limitNum == -1；桌面端同判为 unlimited）。
//
// 池内没有"不限"状态位，而 credits 同时参与两处判定——路由权重（credits/maxCredits）与
// credit_floor（余额低于阈值则不接收费模型）——不限量必须两处都"不构成限制"，故剩余量
// 取一个足够大的值；total 记 -1 供面板显示「不限」。total 仅用于展示与快照，
// 不参与任何判定（已核对全仓库引用面）。
const (
	enterpriseUnlimitedRemain int64 = 1 << 40
	enterpriseUnlimitedTotal  int64 = -1
)

// enterpriseResource 查询**企业版账号**的分配额度，并归一到个人口径的
// (remain, total, expiring, earliestAt, earliestRemaining)。
//
// 为什么必须分流：企业额度不在「个人资源包」体系内——/billing/meter/get-user-resource*
// 对 enterpriseId 非空的账号恒返回空 Accounts（实测 code 0 且 Accounts null），
// 面板因此长期显示 0/未知（额度其实是有的）。
//
// 上游口径（实测 2026-10-07，企业号 a6239ec8）：
//
//	POST /v2/billing/meter/get-enterprise-user-usage   （X-Enterprise-Id 由 BillingHeaders 注入）
//	{"credit":812.45,"limitNum":2000,
//	 "cycleStartTime":"2026-09-27 00:00:00","cycleEndTime":"2026-10-26 23:59:59",
//	 "cycleResetTime":"2026-10-27 00:00:00"}
//
// credit = **本周期已用**（与个人口径 CapacityRemain「剩余」语义相反），
// limitNum = **分配给本账号的额度**。故 remain = limitNum - credit、total = limitNum。
// 注意：企业**池**总额度是另一回事，成员账号无权查询（实测所有池端点 403 not_authorized），
// 本函数只反映该账号被分配的额度。
//
// 分桶：企业配额按周期重置、未用完即作废，与个人「奖励积分到期作废」同性质，故把
// cycleEndTime 作为唯一到期批次——周期末企业号会被 prefer_expiring 优先选中（期望行为）。
// limitNum < 0（不限量）无作废语义，不参与分桶。
func (c *Client) enterpriseResource(
	account6 *auth.Auth, soon time.Duration,
) (remain, total, expiring int64, earliestAt time.Time, earliestRemaining int64, err error) {
	// 与个人口径一致：余额是签到后的紧邻调用，偶发 500 值得有界重试。
	var data json.RawMessage

	err = c.retryBillingTransient(func() error {
		var e error

		data, e = c.billingJSON(account6, "/v2/billing/meter/get-enterprise-user-usage", map[string]any{})

		return e
	})
	if err != nil {
		return 0, 0, 0, time.Time{}, 0, err
	}
	// 兼容上游两套字段命名（桌面端两条解析路径分别读 camelCase 与 snake_case）。
	limit, used, cycleEnd, cycleReset, err := parseEnterpriseQuota(data)
	if err != nil {
		return 0, 0, 0, time.Time{}, 0, err
	}
	// 不限量：池内无对应状态位，用大剩余量让路由权重与 credit_floor 都不构成限制。
	if limit < 0 {
		c.notifyBalance(account6, enterpriseUnlimitedRemain, enterpriseUnlimitedTotal)

		return enterpriseUnlimitedRemain, enterpriseUnlimitedTotal, 0, time.Time{}, 0, nil
	}

	remain = max(limit-roundHalfUp(used), 0)
	total = limit
	// 周期到期批次：cycleEndTime 优先，缺失回落 cycleResetTime。
	earliestAt, earliestRemaining, expiring = enterpriseExpiryBucket(cycleEnd, cycleReset, remain, time.Now(), soon)
	// 观测回调挂在具体成功路径（而不是 UserResourceDetailedWithExpiry 包装层），
	// 这样任何直接调用企业口径的余额查询（如 UserResourceSnapshot）都能记账。
	c.notifyBalance(account6, remain, total)

	return remain, total, expiring, earliestAt, earliestRemaining, nil
}

// enterpriseQuota 企业额度响应字段（兼容 camelCase 与 snake_case 两套命名）。
type enterpriseQuota struct {
	Credit         float64 `json:"credit"`
	LimitNum       int64   `json:"limitNum"`
	LimitNumSnake  int64   `json:"limit_num"`
	UsedNum        float64 `json:"used_num"`
	CycleEndTime   string  `json:"cycleEndTime"`
	CycleResetTime string  `json:"cycleResetTime"`
}

// parseEnterpriseQuota 解析企业额度响应，把两套字段命名归一到 limit/used。
func parseEnterpriseQuota(data json.RawMessage) (limit int64, used float64, cycleEnd, cycleReset string, err error) {
	var resp enterpriseQuota
	if uerr := json.Unmarshal(data, &resp); uerr != nil {
		return 0, 0, "", "", fmt.Errorf("enterprise resource parse: %w", uerr)
	}

	limit = resp.LimitNum
	if limit == 0 && resp.LimitNumSnake != 0 {
		limit = resp.LimitNumSnake
	}

	used = resp.Credit
	if used == 0 && resp.UsedNum != 0 {
		used = resp.UsedNum
	}

	return limit, used, resp.CycleEndTime, resp.CycleResetTime, nil
}

// roundHalfUp 把浮点额度四舍五入到最近整数（credit 是浮点如 812.45，池内 credits
// 是整数；向下取整会低报剩余额度，与"额度还有多少"的展示意图相悖）。
func roundHalfUp(used float64) int64 {
	usedInt := int64(used)
	if used-float64(usedInt) >= 0.5 {
		usedInt++
	}

	return usedInt
}

// enterpriseExpiryBucket 计算企业配额的周期到期批次：cycleEndTime 优先、缺失回落
// cycleResetTime；过期或零余额不入桶。limitNum<0（不限量）无作废语义不参与分桶。
func enterpriseExpiryBucket(
	cycleEnd, cycleReset string, remain int64, now time.Time, soon time.Duration,
) (earliestAt time.Time, earliestRemaining, expiring int64) {
	end, ok := parsePackageEndTime(cycleEnd)
	if !ok {
		end, ok = parsePackageEndTime(cycleReset)
	}

	if !ok || remain <= 0 || !end.After(now) {
		return time.Time{}, 0, 0
	}

	earliestAt, earliestRemaining = end, remain
	if soon > 0 && !end.After(now.Add(soon)) {
		expiring = remain
	}

	return earliestAt, earliestRemaining, expiring
}

// flexInt64 兼容上游把整数字段下发为 JSON 数字或字符串（如 "1000"）两种形态。
type flexInt64 int64

// UnmarshalJSON 接受 1000 / "1000" / "1000.0"；JSON null 按 0 处理。
// 空串按非法值处理：静默按 0 会跳过旧端点回退，让余额误报为 0。
func (f *flexInt64) UnmarshalJSON(raw []byte) error {
	if strings.TrimSpace(string(raw)) == "null" {
		*f = 0
		return nil
	}

	text, textErr := flexNumberText(raw)
	if textErr != nil {
		return textErr
	}

	if n, parseErr := strconv.ParseInt(text, 10, 64); parseErr == nil {
		*f = flexInt64(n)
		return nil
	}

	// 少数账号的容量以浮点串下发，截断取整。NaN/Inf 与超出 int64 范围的浮点
	// 转换到 int64 属实现相关行为，必须在转换前拒绝，否则会静默变成 0 并跳过
	// 旧端点回退，让余额误报为 0。
	n, parseErr := strconv.ParseFloat(text, 64)
	// 边界取闭区间两侧都拒绝：float64 无法精确表示 MinInt64 附近的差值，
	// "-9223372036854775809" 会被舍入成 MinInt64，放行后截断成合法整数。
	if parseErr != nil || math.IsNaN(n) || math.IsInf(n, 0) || n >= math.MaxInt64 || n <= math.MinInt64 {
		return fmt.Errorf("%w: %q", errFlexInt64Invalid, text)
	}

	*f = flexInt64(int64(n))

	return nil
}

// flexNumberText 归一容量标量文本：字符串形态按 JSON 规则解转义（\u0031 等）并
// 去空白，其余形态原样返回。空串非法——静默按 0 会跳过旧端点回退并误报余额。
func flexNumberText(raw []byte) (string, error) {
	text := strings.TrimSpace(string(raw))
	if strings.HasPrefix(text, `"`) {
		var decoded string
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return "", fmt.Errorf("%w: %w", errFlexInt64Invalid, err)
		}

		text = strings.TrimSpace(decoded)
	}

	if text == "" {
		return "", fmt.Errorf("%w: 空容量值", errFlexInt64Invalid)
	}

	return text, nil
}

// resourceSummaryResp get-user-resource-summary 响应（doJSON 已解信封）。
type resourceSummaryResp struct {
	Packages []struct {
		PackageCode         string    `json:"PackageCode"`
		CycleTotalCapacity  flexInt64 `json:"CycleTotalCapacity"`
		CycleRemainCapacity flexInt64 `json:"CycleRemainCapacity"`
		CycleUsedCapacity   flexInt64 `json:"CycleUsedCapacity"`
	} `json:"Packages"`
}

// fetchResourceSummary 查询额度汇总（官方桌面端主路径）。
func (c *Client) fetchResourceSummary(account *auth.Auth) (resourceSummaryResp, error) {
	var data json.RawMessage

	err := c.retryBillingTransient(func() error {
		var fetchErr error

		data, fetchErr = c.billingMeterJSON(account, []string{
			"/v2/billing/meter/get-user-resource-summary",
			"/billing/meter/get-user-resource-summary",
		}, map[string]any{})

		return fetchErr
	})
	if err != nil {
		return resourceSummaryResp{}, err
	}

	// 解码失败是确定性错误，不参与瞬时重试（否则每次余额刷新都要白等 2s+4s）；
	// 与 enterpriseResource / UserResourceDetailedWithExpiry 的重试范围保持一致。
	var out resourceSummaryResp
	if err := json.Unmarshal(data, &out); err != nil {
		return resourceSummaryResp{}, fmt.Errorf("resource summary parse: %w", err)
	}

	// summary 是主余额路径：在数据解析成功后触发观测，下游（personalResource /
	// UserResourceSnapshot 重构）无需再处理。空 packages 属「回落旧端点」，不记脏数据。
	if len(out.Packages) > 0 {
		remain, total := out.summaryTotals()
		c.notifyBalance(account, remain, total)
	}

	return out, nil
}

// summaryTotals 汇总 summary 包的 remain/total（负值钳 0，remain 不超过 total）。
func (r *resourceSummaryResp) summaryTotals() (remain, total int64) {
	for _, pkg := range r.Packages {
		size := max(int64(pkg.CycleTotalCapacity), 0)
		left := max(int64(pkg.CycleRemainCapacity), 0)
		// 总额缺失/为 0 时按剩余兜底，避免「有余额却 total=0」被旧端点聚合口径
		// 覆盖成 0/0（credit_floor 会因此误停账号）。
		size = max(size, left)

		remain += left
		total += size
	}

	return remain, total
}

// packagePageResp 积分包分页响应（doJSON 已解信封，从 Accounts/TotalCount 起）。
type packagePageResp struct {
	Accounts   []billingPackageAccount `json:"Accounts"`
	TotalCount int                     `json:"TotalCount"`
}

// packageExpiryTime 把单个包的到期字段解析为时刻。
func packageExpiryTime(pkg *billingPackageAccount) (time.Time, bool) {
	endTime, expiresAt := creditPackageExpiry(pkg)
	if expiresAt > 0 {
		return time.UnixMilli(expiresAt), true
	}

	if t, err := time.Parse(time.RFC3339, endTime); err == nil {
		return t, true
	}

	return parsePackageEndTime(endTime)
}

// 官方桌面端按包类型拆分查询，且上游要求显式下发 PackageCodes（缺失直接返回
// 10001 PackageCodes required）。付费包与免费/赠送包分属两个端点，码表不可混用。
var (
	paidResourcePackageCodes = []string{
		"TCACA_code_002_AkiJS3ZHF5",
		"TCACA_code_005_maRGyrHhw1",
		"TCACA_code_003_FAnt7lcmRT",
		"TCACA_code_023_4xbGhMrE6q",
		"TCACA_code_026_BaESVICNoi",
		"TCACA_code_027_0FCGVA6vSa",
		"TCACA_code_009_0XmEQc2xOf",
		"TCACA_code_038_OhvqZtiPKr",
		"TCACA_code_036_lupO5WgNdG",
	}
	freeResourcePackageCodes = []string{
		"TCACA_code_001_PqouKr6QWV",
		"TCACA_code_008_cfWoLwvjU4",
		"TCACA_code_035_ArVxJcGDsm",
		"TCACA_code_006_DbXS0lrypC",
		"TCACA_code_039_KRcQj7wUat",
		"TCACA_code_040_mi9rCYg46x",
		"TCACA_code_007_nzdH5h4Nl0",
		"TCACA_code_028_NtpWi0jzXs",
		"TCACA_code_037_WxOD3MpI2o",
		"TCACA_code_029_6wCGEWquYy",
		"TCACA_code_030_BjSt89qTvr",
	}
)

// resourcePackageExpiry 拉取 paid/free 积分包分页，计算最早到期批次与窗口内到期余额。
// 汇总接口已给出 remain/total；本函数只补逐包到期信息，失败不阻断汇总结果。
func (c *Client) resourcePackages(account *auth.Auth, now time.Time, soon time.Duration) (
	expiring int64, totals *resourceTotals,
) {
	totals = &resourceTotals{}

	// 切片日期窗口按上游 UTC+8 墙钟计算；用部署机本地时区会在非 UTC+8 环境把
	// 「今天」算错一天，拿到错误周期的免费包。
	nowCST := now.In(softRateResetLoc)
	dayStart := time.Date(nowCST.Year(), nowCST.Month(), nowCST.Day(), 0, 0, 0, 0, softRateResetLoc)
	dayEnd := dayStart.Add(24*time.Hour - time.Second)

	specs := []struct {
		path string
		body map[string]any
	}{
		{
			path: "/billing/meter/get-user-resource-paid-packages",
			body: map[string]any{
				"NeedRenewInfo": true, "Status": []int{0, 3}, "IsDisplayTotalInfo": true,
				"PackageCodes": paidResourcePackageCodes,
			},
		},
		{
			path: "/billing/meter/get-user-resource-free-packages",
			body: map[string]any{
				"Status": []int{0, 3}, "IsDisplayTotalInfo": true,
				"PackageCodes":         freeResourcePackageCodes,
				"SlicePeriodStartTime": dayStart.Format(packageEndLayout),
				"SlicePeriodEndTime":   dayEnd.Format(packageEndLayout),
			},
		},
	}

	for _, spec := range specs {
		if perr := c.collectPackageExpiry(account, spec.path, spec.body, now, soon, totals); perr != nil {
			log.Printf("WARN: [upstream] resource packages %s: %v (summary total kept)", spec.path, perr)
		}
	}

	return totals.expiring, totals
}

// collectPackageExpiry 分页拉取单类积分包并累加到期批次。TotalCount>0 时以其为
// 终止条件；否则以空页终止；单类最多 100 页（防御上游异常无限翻页）。
func (c *Client) collectPackageExpiry(
	account *auth.Auth, path string, base map[string]any,
	now time.Time, soon time.Duration, totals *resourceTotals,
) error {
	const pageSize = 100

	seen := 0

	for page := 1; page <= 100; page++ {
		resp, err := c.fetchPackagePage(account, path, base, page)
		if err != nil {
			return err
		}

		addPackageExpiry(resp.Accounts, now, soon, totals)

		// 空页必须终止：TotalCount 与过滤后页数不一致时，单靠 seen>=TotalCount
		// 会空翻到 100 页上限，给每个账号的余额刷新放大成上百次上游请求。
		if len(resp.Accounts) == 0 {
			break
		}

		seen += len(resp.Accounts)
		if resp.TotalCount > 0 {
			if seen >= resp.TotalCount {
				break
			}

			continue
		}

		if len(resp.Accounts) < pageSize {
			break
		}
	}

	return nil
}

// fetchPackagePage 拉取一页积分包。
func (c *Client) fetchPackagePage(account *auth.Auth, path string, base map[string]any, page int) (packagePageResp, error) {
	body := maps.Clone(base)
	body["PageNumber"] = page
	body["PageSize"] = 100

	data, err := c.billingMeterJSON(account, []string{"/v2" + path, path}, body)
	if err != nil {
		return packagePageResp{}, err
	}

	var resp packagePageResp
	if err := json.Unmarshal(data, &resp); err != nil {
		return packagePageResp{}, fmt.Errorf("packages parse: %w", err)
	}

	return resp, nil
}

// addPackageExpiry 把一页积分包中未过期且有余量的包累加进到期批次。
func addPackageExpiry(packages []billingPackageAccount, now time.Time, soon time.Duration, totals *resourceTotals) {
	for i := range packages {
		pkg := &packages[i]

		_, remain, _, _ := creditPackageAmounts(pkg)
		if remain <= 0 {
			continue
		}

		end, ok := packageExpiryTime(pkg)
		if !ok || !end.After(now) {
			continue
		}

		totals.addExpiry(end, remain, now, soon)
	}
}

// UserResourceDetailedWithExpiry 在 UserResourceDetailed 基础上返回最早未来到期批次：
// earliestAt 是最早的可用到期时刻，earliestRemaining 是同一时刻所有正余额包的剩余量之和。
// 已过期、剩余为 0、缺少或无法解析到期时间的包都不会成为最早批次；无有效批次时返回零值。
func (c *Client) UserResourceDetailedWithExpiry(
	account *auth.Auth, soon time.Duration,
) (remain, total, expiring int64, earliestAt time.Time, earliestRemaining int64, err error) {
	snap, err := c.UserResourceSnapshot(account, soon)
	if err != nil {
		return 0, 0, 0, time.Time{}, 0, err
	}

	for i := range snap.Buckets {
		bucket := &snap.Buckets[i]
		if earliestAt.IsZero() || bucket.ExpireAt.Before(earliestAt) {
			earliestAt, earliestRemaining = bucket.ExpireAt, bucket.Remain
		} else if bucket.ExpireAt.Equal(earliestAt) {
			earliestRemaining += bucket.Remain
		}
	}

	return snap.Remain, snap.Total, snap.Expiring, earliestAt, earliestRemaining, nil
}

// UserResourceSnapshot 查询余额并返回含逐日到期批次的完整快照。企业版走企业口径
// （额度体系无「资源包到期批次」，Buckets 恒空）。
func (c *Client) UserResourceSnapshot(account *auth.Auth, soon time.Duration) (ResourceSnapshot, error) {
	// 企业版额度不在「个人资源包」体系内：get-user-resource* 对 enterpriseId 非空账号
	// 恒返回空 Accounts（实测 code 0 且 Accounts null），故分流到企业口径端点。
	if account.IsEnterprise() {
		remain, total, expiring, earliestAt, earliestRemaining, err := c.enterpriseResource(account, soon)
		if err != nil {
			return ResourceSnapshot{}, err
		}

		// 企业口径只给「周期末」一个到期点：折算成单条日桶，保持与个人口径同形状
		// （快照 / 最早到期路由 / 面板「检查」看到的是同一份到期信息）。
		snap := ResourceSnapshot{Remain: remain, Total: total, Expiring: expiring}
		if !earliestAt.IsZero() && earliestRemaining > 0 {
			snap.Buckets = []ExpiryBucket{{
				Date:     expiryBucketDate(earliestAt),
				ExpireAt: earliestAt,
				Remain:   earliestRemaining,
			}}
		}

		return snap, nil
	}

	remain, total, expiring, totals, err := c.personalResource(account, soon)
	if err != nil {
		return ResourceSnapshot{}, err
	}

	return ResourceSnapshot{Remain: remain, Total: total, Expiring: expiring, Buckets: totals.bucketList()}, nil
}

// personalResource 个人号余额查询主体：返回汇总与含日桶的累加器。
func (c *Client) personalResource(account *auth.Auth, soon time.Duration) (int64, int64, int64, *resourceTotals, error) {
	now := time.Now()
	// 官方桌面端主路径：summary 汇总 remain/total；失败时回落旧 get-user-resource。
	if summary, serr := c.fetchResourceSummary(account); serr == nil && len(summary.Packages) > 0 {
		remain, total := summary.summaryTotals()
		expiring, totals := c.resourcePackages(account, now, soon)

		return remain, total, expiring, totals, nil
	} else if serr != nil {
		log.Printf("WARN: [upstream] resource summary failed: %v (fallback get-user-resource)", serr)
	}

	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format(packageEndLayout),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format(packageEndLayout),
	}
	// 余额查询同样做瞬时错误有界重试（签到后紧接着的 user-resource 偶发 500 会让
	// 该账号错过本次解冻/到期快照更新，只能等下一个刷新周期）。
	var data json.RawMessage

	err := c.retryBillingTransient(func() error {
		var e error

		data, e = c.billingMeterJSON(account, c.billingMeterPaths(account), body)

		return e
	})
	if err != nil {
		return 0, 0, 0, nil, err
	}

	var resp resourceDetailedResp
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, 0, 0, nil, fmt.Errorf("resource parse: %w", err)
	}

	var totals resourceTotals
	for i := range resp.Response.Data.Accounts {
		totals.add(&resp.Response.Data.Accounts[i], now, soon)
	}

	// 观测回调挂在具体成功路径（不是 UserResourceDetailedWithExpiry / UserResourceSnapshot
	// 包装层）：summary 已在 fetchResourceSummary 记账，企业口径在 enterpriseResource 记账，
	// 这里补旧端点回退路径。成功才回调，失败不写脏数据。
	c.notifyBalance(account, totals.remain, totals.total)

	return totals.remain, totals.total, totals.expiring, &totals, nil
}

// resourceDetailedResp get-user-resource 响应信封。
type resourceDetailedResp struct {
	Response struct {
		Data struct {
			Accounts []resourceAccount `json:"Accounts"`
		} `json:"Data"`
	} `json:"Response"`
}

// resourceAccount get-user-resource 的单个套餐字段。
type resourceAccount struct {
	PackageName                string             `json:"PackageName"`
	CapacityType               int                `json:"CapacityType"`
	SlicePeriodUsageDetails    []slicePeriodUsage `json:"SlicePeriodUsageDetails"`
	CycleEndTime               string             `json:"CycleEndTime"` // "2006-01-02 15:04:05"，缺省/空 = 无到期
	ExpiredTime                string             `json:"ExpiredTime"`
	PackageEndTime             string             `json:"PackageEndTime"`
	DeductionEndTime           int64              `json:"DeductionEndTime"`
	CapacitySize               int64              `json:"CapacitySize"`
	CapacityRemain             int64              `json:"CapacityRemain"`
	CapacityUsed               int64              `json:"CapacityUsed"`
	CapacitySizePrecise        string             `json:"CapacitySizePrecise"`
	CapacityRemainPrecise      string             `json:"CapacityRemainPrecise"`
	CapacityUsedPrecise        string             `json:"CapacityUsedPrecise"`
	CycleCapacitySize          int64              `json:"CycleCapacitySize"`
	CycleCapacityRemain        int64              `json:"CycleCapacityRemain"`
	CycleCapacityUsed          int64              `json:"CycleCapacityUsed"`
	CycleCapacitySizePrecise   string             `json:"CycleCapacitySizePrecise"`
	CycleCapacityRemainPrecise string             `json:"CycleCapacityRemainPrecise"`
	CycleCapacityUsedPrecise   string             `json:"CycleCapacityUsedPrecise"`
}

// ExpiryBucket 一个自然日（上游 CST 墙钟）的到期批次：同日多包合并。
// Date 是「YYYY-MM-DD」形态（与前端 expBatches 同口径，按日聚合）；
// ExpireAt 是该日最早到期时刻（展示/排序用，跨日聚合时不为零值）；
// Remain 是该日全部正余额包的剩余积分之和。
type ExpiryBucket struct {
	Date     string    `json:"date"`
	ExpireAt time.Time `json:"expire_at,omitzero"`
	Remain   int64     `json:"remain"`
}

// ResourceSnapshot 一次余额查询的完整快照：汇总 + 逐日到期批次。
// 快照与「检查」（面板 packages 明细）同口径——两路都按自然日聚合，
// 避免「快照只显示最早一批、检查能显示全部批次」的口径分裂。
type ResourceSnapshot struct {
	Remain   int64          `json:"remain"`
	Total    int64          `json:"total"`
	Expiring int64          `json:"expiring"`
	Buckets  []ExpiryBucket `json:"buckets,omitempty"`
}

// resourceTotals 个人资源聚合累加器。
type resourceTotals struct {
	remain, total, expiring int64
	earliestAt              time.Time
	earliestRemaining       int64
	// buckets 按自然日聚合的到期批次（date → 批次）；nil 表示尚无批次。
	buckets map[string]*ExpiryBucket
}

// add 累加单个套餐的余额与到期批次（余额钳非负、size 不高于 remain）。
func (t *resourceTotals) add(acct *resourceAccount, now time.Time, soon time.Duration) {
	r, _, size := packageRemainUsed(respAccount{
		CapacityType:               acct.CapacityType,
		SlicePeriodUsageDetails:    acct.SlicePeriodUsageDetails,
		CapacityRemain:             acct.CapacityRemain,
		CapacityUsed:               acct.CapacityUsed,
		CapacitySize:               acct.CapacitySize,
		CapacityRemainPrecise:      acct.CapacityRemainPrecise,
		CapacityUsedPrecise:        acct.CapacityUsedPrecise,
		CapacitySizePrecise:        acct.CapacitySizePrecise,
		CycleCapacityRemain:        acct.CycleCapacityRemain,
		CycleCapacityUsed:          acct.CycleCapacityUsed,
		CycleCapacitySize:          acct.CycleCapacitySize,
		CycleCapacityRemainPrecise: acct.CycleCapacityRemainPrecise,
		CycleCapacityUsedPrecise:   acct.CycleCapacityUsedPrecise,
		CycleCapacitySizePrecise:   acct.CycleCapacitySizePrecise,
	})
	if r < 0 {
		r = 0
	}

	if size < r {
		size = r
	}

	t.remain += r
	t.total += size

	if r <= 0 {
		return
	}

	end, ok := resourceAccountExpiry(acct)
	if !ok || !end.After(now) {
		return
	}

	t.addExpiry(end, r, now, soon)
}

// resourceAccountExpiry 选择单个资源包的到期时刻：
// CapacityType=4（切片/每日包）以 CycleEndTime 为刷新边界；其余优先
// DeductionEndTime（epoch 毫秒），再 ExpiredTime，最后 CycleEndTime。
func resourceAccountExpiry(acct *resourceAccount) (time.Time, bool) {
	// 切片/每日包以周期边界为准，但仅在字段存在时（与 creditPackageExpiry 同口径）：
	// CycleEndTime 缺失时继续按下面的通用优先级回落，不直接判「无到期」。
	if acct.CapacityType == 4 && acct.CycleEndTime != "" {
		return parsePackageEndTime(acct.CycleEndTime)
	}

	if acct.DeductionEndTime > 0 {
		return time.UnixMilli(acct.DeductionEndTime), true
	}

	if acct.ExpiredTime != "" {
		if t, ok := parsePackageEndTime(acct.ExpiredTime); ok {
			return t, true
		}
	}

	// 与「检查」（creditPackageExpiry）同优先级：ExpiredTime → PackageEndTime →
	// CycleEndTime。漏掉 PackageEndTime 会让仅带该字段的包回落到周期重置日，
	// 快照与逐包明细再次分叉。
	if acct.PackageEndTime != "" {
		if t, ok := parsePackageEndTime(acct.PackageEndTime); ok {
			return t, true
		}
	}

	return parsePackageEndTime(acct.CycleEndTime)
}

// addExpiry 更新最早到期批次并累计窗口内 expiring。
func (t *resourceTotals) addExpiry(end time.Time, r int64, now time.Time, soon time.Duration) {
	if t.earliestAt.IsZero() || end.Before(t.earliestAt) {
		t.earliestAt = end
		t.earliestRemaining = r
	} else if end.Equal(t.earliestAt) {
		t.earliestRemaining += r
	}
	// 分桶：仅 soon>0 且确实在窗口内 → expiring。
	if soon > 0 && !end.After(now.Add(soon)) {
		t.expiring += r
	}

	t.addBucket(end, r)
}

// expiryBucketDate 到期时刻所属的自然日（上游 CST 墙钟），口径与面板「检查」
// （expBatches 按 expire_at 的日期聚合）一致。
func expiryBucketDate(end time.Time) string {
	return end.In(softRateResetLoc).Format("2006-01-02")
}

// addBucket 把一笔到期额度累加进所属自然日的批次（同日取最早到期时刻）。
func (t *resourceTotals) addBucket(end time.Time, r int64) {
	if r <= 0 || end.IsZero() {
		return
	}

	date := expiryBucketDate(end)

	if t.buckets == nil {
		t.buckets = make(map[string]*ExpiryBucket, 2)
	}

	if bucket := t.buckets[date]; bucket != nil {
		bucket.Remain += r

		if end.Before(bucket.ExpireAt) {
			bucket.ExpireAt = end
		}

		return
	}

	t.buckets[date] = &ExpiryBucket{Date: date, ExpireAt: end, Remain: r}
}

// bucketList 返回按日期升序的批次快照（nil = 无批次）。
func (t *resourceTotals) bucketList() []ExpiryBucket {
	if len(t.buckets) == 0 {
		return nil
	}

	out := make([]ExpiryBucket, 0, len(t.buckets))
	for _, bucket := range t.buckets {
		out = append(out, *bucket)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Date < out[j].Date })

	return out
}

// respAccount 供 packageRemainUsed 解析的套餐字段（CreditPackages 的逐包结构同构）。
type respAccount struct {
	CapacityType               int
	SlicePeriodUsageDetails    []slicePeriodUsage
	CapacityRemain             int64
	CapacityUsed               int64
	CapacitySize               int64
	CapacityRemainPrecise      string
	CapacityUsedPrecise        string
	CapacitySizePrecise        string
	CycleCapacityRemain        int64
	CycleCapacityUsed          int64
	CycleCapacitySize          int64
	CycleCapacityRemainPrecise string
	CycleCapacityUsedPrecise   string
	CycleCapacitySizePrecise   string
}

// preciseAmounts 优先使用上游精确容量字段；CapacityType=4 时取首个切片周期明细。
// 返回 ok=false 表示没有可用的精确字段，调用方回落旧的整数字段；cycle 标识实际
// 采用的周期分支（切片型与 Cycle* 为 true），供上层设置 CreditPackage.Cycle。
//
//nolint:gocritic // respAccount 为一次性解析结构，值传递不影响性能。
func preciseAmounts(account respAccount) (remain, used, size int64, cycle, ok bool) {
	if account.CapacityType == 4 && len(account.SlicePeriodUsageDetails) > 0 {
		remain, used, size, ok = preciseSliceAmounts(account.SlicePeriodUsageDetails[0])

		return remain, used, size, true, ok
	}

	// 周期套餐优先 Cycle*（与旧整数口径一致）；两者都下发时 Capacity* 是总额，
	// 不能覆盖当周期余额。CycleSize 非法时直接回落整数容量字段，避免把总额
	// 当周期余额，或把周期分支的畸形值混入 Capacity 分支。
	cycleSize, ok := parsePreciseCapacity(account.CycleCapacitySizePrecise)
	if !ok {
		return 0, 0, 0, false, false
	}

	if cycleSize > 0 {
		remain, used, size, ok = preciseTripleSize(
			account.CycleCapacityRemainPrecise,
			account.CycleCapacityUsedPrecise,
			cycleSize,
		)

		return remain, used, size, true, ok
	}

	remain, used, size, ok = preciseTriple(
		account.CapacityRemainPrecise,
		account.CapacityUsedPrecise,
		account.CapacitySizePrecise,
	)

	return remain, used, size, false, ok
}

// preciseSliceAmounts 从首个切片周期明细取精确容量。
func preciseSliceAmounts(usage slicePeriodUsage) (remain, used, size int64, ok bool) {
	return preciseTriple(
		usage.SlicePeriodCapacityRemainPrecise,
		usage.SlicePeriodCapacityUsedPrecise,
		usage.SlicePeriodCapacitySizePrecise,
	)
}

// normalizePreciseAmounts 修正 used/remain 的缺失与越界（钳 [0,size]）。
func normalizePreciseAmounts(remain, used, size int64) (int64, int64, int64, bool) {
	if used <= 0 {
		used = max(size-remain, 0)
	}

	remain = max(min(remain, size), 0)
	if remain == 0 && used > 0 {
		remain = max(size-used, 0)
	}

	return remain, used, size, true
}

// packageRemainUsed 聚合单套餐的 remain/used/size（与 CreditPackages/cmd/credit 的
// 历史口径一致，收敛至此作为单一事实来源）。Cycle 期套餐优先：用 CycleCapacity
// 三字段，used 取 CycleUsed 与 size-remain 的较大者；否则回退 Capacity 三字段。
//
//nolint:gocritic // respAccount 为一次性解析结构，值传递不影响性能。
func packageRemainUsed(account respAccount) (remain, used, size int64) {
	if r, u, sz, _, ok := preciseAmounts(account); ok {
		return r, u, sz
	}

	if account.CycleCapacitySize > 0 {
		return cyclePackageAmounts(account)
	}

	remain = account.CapacityRemain
	used = account.CapacityUsed

	size = account.CapacitySize
	if used == 0 && size > remain {
		used = size - remain
	}

	return remain, used, size
}

// cyclePackageAmounts 周期套餐的 remain/used/size：remain 先钳 [0,size]，used 取
// size-remain 与 CycleUsed 的较大者。
//
//nolint:gocritic // respAccount 为一次性解析结构，值传递不影响性能。
func cyclePackageAmounts(account respAccount) (remain, used, size int64) {
	remain = max(account.CycleCapacityRemain, 0)
	size = account.CycleCapacitySize
	remain = min(remain, size)

	used = max(size-remain, account.CycleCapacityUsed)
	if size >= used {
		remain = size - used
	}

	return remain, used, size
}

// DailyCheckin 执行每日签到。已签到（业务 code 非 0）也返回错误，调用方按 msg 区分。
// 偶发上游 5xx（code 10000）做有界重试（见 retryBillingTransient）——单次抖动不再
// 让该账号整天漏签；「已签到」等业务错误不重试。
func (c *Client) DailyCheckin(a *auth.Auth) error {
	return c.retryBillingTransient(func() error {
		_, err := c.billingMeterJSON(a, c.checkinMeterPaths(a), map[string]any{})
		return err
	})
}

// CheckinActivityStatus 查询签到活动状态（官方桌面端 checkin-activity-status）。
// 返回 today_checked_in；请求失败时调用方回落直接签到路径。
func (c *Client) CheckinActivityStatus(a *auth.Auth) (todayChecked bool, err error) {
	data, err := c.billingMeterJSON(a, []string{checkinActivityStatusPath}, map[string]any{})
	if err != nil {
		return false, err
	}

	var status struct {
		TodayCheckedIn bool `json:"today_checked_in"`
	}
	if err := json.Unmarshal(data, &status); err != nil {
		return false, fmt.Errorf("checkin status parse: %w", err)
	}

	return status.TodayCheckedIn, nil
}

// IsAlreadyCheckin 报告 err 是否表示"今天已签到"（上游幂等拒绝重复签到）。
// 只认带分类的 *Error（业务 code 或 HTTP 错误）：网络层/解析层错误不得当作幂等成功，
// 否则停机补签遇到抖动会误记为 already，账号当天实际未签到却被判定正常。
func IsAlreadyCheckin(err error) bool {
	return matchCheckinMarker(err, alreadyCheckinMarkers)
}

// IsCheckinInactive 报告 err 是否表示"签到活动未开启/未开放/已结束"等非错误态。
// 与 IsAlreadyCheckin 同口径：只认 *Error.Msg，网络层/解析层错误不在此识别；
// 保守策略——未命中显式 inactive 关键词即返回 false（真失败不误判为 inactive）。
func IsCheckinInactive(err error) bool {
	return matchCheckinMarker(err, inactiveCheckinMarkers)
}

// matchCheckinMarker 对 *Error.Msg 做大小写不敏感的关键词包含匹配。
func matchCheckinMarker(err error, markers []string) bool {
	var upErr *Error
	if !errors.As(err, &upErr) {
		return false
	}

	lower := strings.ToLower(upErr.Msg)
	for _, m := range markers {
		if strings.Contains(upErr.Msg, m) || strings.Contains(lower, strings.ToLower(m)) {
			return true
		}
	}

	return false
}

// CheckinResult 签到结果分类：inactive（活动未开启）与 already（幂等）从真失败中分离，
// 供调度日志与面板按类呈现，避免把非错误态刷成 error 行。
type CheckinResult int

const (
	// CheckinOK 签到成功（err == nil）。
	CheckinOK CheckinResult = iota
	// CheckinAlready 上游幂等拒绝重复签到（"今天已签到"）。
	CheckinAlready
	// CheckinInactive 活动未开启/未开放/已结束等非错误态。
	CheckinInactive
	// CheckinFailed 其余业务/HTTP 错误（真失败）。
	CheckinFailed
)

// ClassifyCheckin 归类签到结果。分类只描述「结果语义」，不改变重试与幂等判定。
func ClassifyCheckin(err error) CheckinResult {
	code := checkinBusinessCode(err)
	switch {
	case err == nil:
		return CheckinOK
	case code == "1001":
		return CheckinAlready
	case code == "1002" || code == "1003":
		return CheckinInactive
	case IsAlreadyCheckin(err):
		return CheckinAlready
	case IsCheckinInactive(err):
		return CheckinInactive
	default:
		return CheckinFailed
	}
}

// checkinBusinessCode 从 *Error 提取签到业务码：优先结构化 Code（Msg 截断不影响），
// 再回落 Msg 文本匹配。非 *Error 返回空串。
func checkinBusinessCode(err error) string {
	var upstreamErr *Error
	if !errors.As(err, &upstreamErr) {
		return ""
	}

	for _, code := range []string{"1001", "1002", "1003"} {
		if upstreamErr.Code == code || matchBusinessCode(upstreamErr.Msg, code) {
			return code
		}
	}

	return ""
}

// matchBusinessCode 匹配结构化业务码（顶层/嵌套 code、字符串或数字），
// 完整比对避免 10012 被前缀匹配成 1001。
func matchBusinessCode(msg, code string) bool {
	return hasBusinessCode(msg, code)
}

func truncate(s string, n int) string {
	return logfmt.Truncate(s, n)
}
