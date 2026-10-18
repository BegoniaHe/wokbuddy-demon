// Package headers 构造三类上游请求头（common / chat / billing / refresh）。
// 规则来自 docs/api-reference.md §0/§4/§6。
package upstream

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/session"
)

const (
	// 官方分发包版本（按 realm 分档）。UA 的 `WorkBuddy/<ver>`、白名单头组
	// X-IDE-Version、桌面上报指纹的 ideVersion/extVersion 均取此值。
	//   cn     → WorkBuddy Desktop 5.8.2（内嵌 CLI 2.161.0）
	//   global → WorkBuddy AI Desktop 5.7.6（内嵌 CLI 2.156.0）
	// 两份官方包的 daemon UA 第二段均为 `WorkBuddy`（无 realm 品牌差异，仅版本不同）。
	// config upstream.client_version / cli_version 可整体覆盖（空 = 按 realm 取内置默认）。
	cnClientVersion     = "5.8.2"
	cnCliVersion        = "2.161.0"
	globalClientVersion = "5.7.6"
	globalCliVersion    = "2.156.0"

	originRefererCN     = "https://www.codebuddy.cn"
	originRefererGlobal = "https://www.workbuddy.ai"
)

// originRefererFor 按账号 realm 返回 Origin/Referer 基础域：
// global → https://www.workbuddy.ai；cn（含全局开关未开）→ https://www.codebuddy.cn。
func originRefererFor(a *auth.Auth) string {
	if a != nil && a.IsGlobal() {
		return originRefererGlobal
	}

	return originRefererCN
}

// clientVersionFor 生效的 WorkBuddy 客户端版本：Client.ClientVersion 非空则取之
// （全局覆盖），否则按账号 realm 取内置默认（cn 5.8.2 / global 5.7.6）。nil 账号按 cn。
func (c *Client) clientVersionFor(a *auth.Auth) string {
	if c != nil && c.ClientVersion != "" {
		return c.ClientVersion
	}

	if a != nil && a.IsGlobal() {
		return globalClientVersion
	}

	return cnClientVersion
}

// cliVersionFor 生效的 CLI 版本：Client.CliVersion 非空则取之（全局覆盖），
// 否则按账号 realm 取内置默认（cn 2.161.0 / global 2.156.0）。nil 账号按 cn。
func (c *Client) cliVersionFor(a *auth.Auth) string {
	if c != nil && c.CliVersion != "" {
		return c.CliVersion
	}

	if a != nil && a.IsGlobal() {
		return globalCliVersion
	}

	return cnCliVersion
}

// defaultWorkBuddyUAFor 组装默认客户端出站 UA（官方桌面端 daemon 代理层形状）：
// `WorkBuddy/<ver> WorkBuddy/<ver> CLI/<cliVer>`。
// 两份官方分发包（CN 5.8.2 / global 5.7.6）的 daemon UA 第二段均为 `WorkBuddy`
// （`buildVlmGatewayHeaders` / `Jr` 字面量），无 realm 品牌差异；仅版本号按 realm 分档。
// 官方无任何 UA 随机化，故默认确定性。
func (c *Client) defaultWorkBuddyUAFor(a *auth.Auth) string {
	v := c.clientVersionFor(a)
	return "WorkBuddy/" + v + " WorkBuddy/" + v + " CLI/" + c.cliVersionFor(a)
}

// userAgent 返回当前出站 UA（客户端出站路径：chat/refresh/FetchModels）。
// 优先级：Client.UserAgent（config user_agent）显式覆盖 > 按账号 realm 的默认 WorkBuddy 三段式。
// 显式覆盖兼容既有覆盖逻辑：用户配了即以用户值为准（自定义品牌/版本），
// 未配则走官方桌面端默认形态。
func (c *Client) userAgent(a *auth.Auth) string {
	if c != nil && c.UserAgent != "" {
		return c.UserAgent
	}

	return c.defaultWorkBuddyUAFor(a)
}

// billingUA 白名单类（billing/checkin/banner）出站 UA：单段 `WorkBuddy/<clientVersion>`
// （官方 banner/上报显式覆写形态，不带 CLI 段）。默认生效（伪造官方桌面端指纹）；
// 显式 client_name="SaaS" 才不设 UA（还原旧行为，Go 默认 UA）。
func (c *Client) billingUA(a *auth.Auth) string {
	if c == nil || c.attributionClientName() == "SaaS" {
		return ""
	}

	return "WorkBuddy/" + c.clientVersionFor(a)
}

// resolveDeviceToken 解析本次请求的 X-Device-Token 取值。
// 优先级：auth.Auth.DeviceToken（每号）> Client.DeviceToken（config 全局）> 文件兜底。
// 三者皆空/读失败则返回空串（调用方不注入该头，优雅降级）。
func (c *Client) resolveDeviceToken(a *auth.Auth) string {
	if a != nil && a.DeviceToken != "" {
		return a.DeviceToken
	}

	if c != nil && c.DeviceToken != "" {
		return c.DeviceToken
	}

	if c != nil && c.DeviceTokenFile != "" {
		return readDeviceTokenFile(c.DeviceTokenFile)
	}

	return ""
}

// injectDeviceToken 在 req 注入 X-Device-Token 头（仅当取到非空 token）。
func (c *Client) injectDeviceToken(req *http.Request, a *auth.Auth) {
	if tok := c.resolveDeviceToken(a); tok != "" {
		req.Header.Set("X-Device-Token", tok)
	}
}

// deriveAccountStableID 按 uid + 用途盐稳定派生 36 hex 设备/会话标识。
// 跨重启稳定（固定盐 "wb2a:"，不随进程换——这是与 session 包派生盐的本质差异：
// 那是会话键维度的进程级随机盐，重启换新；本函数是账号维度，必须跨重启恒定）、
// 账号间互异（uid 不同则不同）、同 uid 同用途恒同值（幂等）。用 sha256 与项目
// 既有派生（session/ids.go、cache_key.go）保持一致；截 36 hex 提供更长熵。
//
// 两个用途：
//   - purpose="machine" → X-Machine-ID（设备级，跨会话稳定）
//   - purpose="session" → X-Session-ID（账号固定会话，跨重启稳定）
//
// 与 injectDeviceToken 的 X-Device-Token 并存不冲突：那是登录时上游签发的
// 真实设备令牌（有则发，权威）；本对头是「每账号一台固定虚拟设备」的稳定指纹，
// 防多号被上游按设备指纹缺失/漂移关联风控。两者是不同头族，官方桌面端都发。
func deriveAccountStableID(uid, purpose string) string {
	sum := sha256.Sum256([]byte("wb2a:" + purpose + ":" + uid))
	return hex.EncodeToString(sum[:18]) // 36 hex chars
}

// injectAccountStableHeaders 在 req 注入 X-Machine-ID / X-Session-ID：按 uid 稳定
// 派生，跨重启固定、账号间互异。uid 为空时不注入（匿名请求无设备标识，上游不要求）。
func (c *Client) injectAccountStableHeaders(req *http.Request, account5 *auth.Auth) {
	if account5 == nil || account5.UID == "" {
		return
	}

	req.Header.Set("X-Machine-Id", deriveAccountStableID(account5.UID, "machine"))
	req.Header.Set("X-Session-Id", deriveAccountStableID(account5.UID, "session"))
}

// CommonHeaders 设置所有 API 共享的请求头。
func (c *Client) CommonHeaders(req *http.Request, account2 *auth.Auth) {
	req.Header.Set("Content-Type", "application/json")
	// Accept 非流式默认 application/json（D6：去掉宽松的 text/plain, */*）。
	// chat 流式路径在 ChatHeaders 覆盖为 event-stream。
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")

	origin := originRefererFor(account2)
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", c.userAgent(account2))
	// X-CodeBuddy-Request: 1（官方客户端风控闸门头，所有 API 请求必带，D1）。
	req.Header.Set("X-Codebuddy-Request", "1")
	// Accept-Language 按 realm 切（D5）：CN zh-CN，global en-US。官方客户端按账号域
	// 发对应语言标识，对齐避免上游风控按语言缺失误判。
	req.Header.Set("Accept-Language", acceptLanguageFor(account2))
	// X-Machine-ID / X-Session-ID：按 uid 稳定派生的账号级设备头（见
	// injectAccountStableHeaders）。注入在 CommonHeaders——chat 经 ChatHeaders
	// 叠加 CommonHeaders 天然继承；billing 域另行注入，全出站覆盖。
	c.injectAccountStableHeaders(req, account2)
}

// acceptLanguageFor 按账号 realm 返回 Accept-Language：global → en-US，cn → zh-CN。
func acceptLanguageFor(a *auth.Auth) string {
	if a != nil && a.IsGlobal() {
		return "en-US"
	}

	return "zh-CN"
}

// injectCodeBuddyRequest 在 req 注入 X-CodeBuddy-Request: 1。
// billing 域未走 CommonHeaders，单独注入保证全出站覆盖（D1）。
func (c *Client) injectCodeBuddyRequest(req *http.Request) {
	req.Header.Set("X-Codebuddy-Request", "1")
}

// ChatMeta 一次 chat 出站的会话头族元数据（issue #35：后台按 X-Conversation-Request-ID
// 聚合请求，官方客户端一次 user send 内所有 tool call/重试/换号复用同一个 ID）。
// handler 在轮转循环外生成 conversationID / conversationRequestID，循环内每次出站
// 复用同值；TraceID 透传入站值（空 = 回落 conversationRequestID）。
// messageID（消息级，每条独立）由 ChatHeaders 内部生成，无需外部可见。
type ChatMeta struct {
	ConversationID        string // X-Conversation-ID：body 提取的入站值，空则不发（透传优先，不伪造）
	ConversationRequestID string // X-Conversation-Request-ID / X-Root-Request-ID：聚合主键，必发
	TraceID               string // X-Trace-ID：入站透传值，空则回落 conversationRequestID
}

// ChatHeaders 在 common 之上加 chat 专属的账号头。
// 缺省字段用 X-No-* 约定（与 CodeBuddy 官方 CLI 一致）。
// clientIP 为本次请求的客户端 IP（按参数传递，不读共享字段——避免并发串扰）；
// PassthroughIP=false 或 clientIP 为空时不注入 IP 头。
// meta 为会话头族元数据（纯新增，不改既有头），见 injectConversationHeaders。
func (c *Client) ChatHeaders(req *http.Request, account3 *auth.Auth, clientIP string, meta ChatMeta) {
	c.CommonHeaders(req, account3)
	// chat 流式 Accept 覆盖 CommonHeaders 的非流式默认（D6）。
	req.Header.Set("Accept", "application/json, text/event-stream")
	// AccessToken 加锁快照：keepalive 定时刷新会在 a.mu 内改写它，锁外直读构成数据竞争
	// （见 auth.AccessTokenValue 注释）。
	if at := account3.AccessTokenValue(); at != "" {
		req.Header.Set("Authorization", "Bearer "+at)
	} else {
		req.Header.Set("X-No-Authorization", "1")
	}

	if account3.UID != "" {
		req.Header.Set("X-User-Id", account3.UID)
	} else {
		req.Header.Set("X-No-User-Id", "1")
	}
	// 安全红线：绝不在 chat 请求里携带 X-Refresh-Token。
	// 企业与域头按 realm 分发：CN 走既有分支（EnterpriseID/Domain 原样透传，缺省 X-No-*）；
	// global 账号由 injectGlobalChatHeaders 统一覆写为国际客户端形态
	// （X-No-Enterprise-Id=1 声明无企业 + X-Domain=www.workbuddy.ai 声明国际版域），
	// 且不回退 X-Domain 到登录会话原值——对齐 intl 项目出站头。
	if account3 != nil && !account3.IsGlobal() {
		injectCNChatHeaders(req, account3)
	} else {
		c.injectGlobalChatHeaders(req, account3)
	}
	// 用量归属头：默认伪造官方桌面端指纹（client_name 非空 = 自定义归属逃生门）。
	c.injectAttribution(req, account3)
	// 客户端 IP 透传（仅 PassthroughIP=true 且本次请求带 IP）。
	c.injectClientIP(req, clientIP)
	// 设备风控头：auth 每号 > config 全局 > 文件兜底；空则不注入。
	c.injectDeviceToken(req, account3)
	// 会话头族（对话/请求/消息/B3 链路），见 injectConversationHeaders。
	c.injectConversationHeaders(req, meta)
}

// injectCNChatHeaders CN 账号的 chat 域头：企业 ID 与 Domain 原样透传（缺省 X-No-*）。
func injectCNChatHeaders(req *http.Request, account *auth.Auth) {
	if account.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", account.EnterpriseID)
	} else {
		req.Header.Set("X-No-Enterprise-Id", "1")
	}

	if d := account.DomainValue(); d != "" {
		req.Header.Set("X-Domain", d)
	} else {
		req.Header.Set("X-No-Department-Info", "1")
	}
}

// injectGlobalChatHeaders global 账号（无企业 ID）的 chat 专属声明头，对齐 intl 项目
// （ANALYSIS-global-chat-solutions.md）：
//   - X-No-Enterprise-Id: 1  个人账号无企业 ID，显式声明（避免上游按缺省/可疑判定）
//   - X-Domain: www.workbuddy.ai  显式声明国际版域（与 Origin/Referer 同域）
//
// 仅 global realm 注入；CN 账号走既有 X-No-Department-Info 等分支，零回归。
func (c *Client) injectGlobalChatHeaders(req *http.Request, a *auth.Auth) {
	if a == nil || !a.IsGlobal() {
		return
	}

	req.Header.Set("X-No-Enterprise-Id", "1")
	req.Header.Set("X-Domain", "www.workbuddy.ai")
}

// injectConversationHeaders 注入官方客户端会话头族（issue #35 后台聚合）。
// 头族分四层，各司其职：
//   - X-Conversation-ID：会话级，多轮稳定（body 的 conversationId）。空则不发——
//     透传客户端原值优先，客户端没给就不伪造，避免误导后台建错会话。
//   - X-Conversation-Request-ID：**对话轮级聚合主键**，必发。一次 user send 内的
//     所有 tool call/重试/换号/降级复用同一个 → 后台按它聚合成一条（不再碎片化）。
//   - X-Conversation-Message-ID = X-Request-ID：消息级，每条独立（32 位 hex）。
//   - X-Root-Request-ID：= conversationRequestID（根请求追踪）。
//   - X-Trace-ID / traceparent / b3 / X-B3-*：链路族。官方桌面端 daemon 用
//     randomBytes(16/8/8) 生成 traceId/spanId/parentSpanId 三件套，同时写 traceparent、
//     b3、X-B3-TraceId/ParentSpanId/SpanId/Sampled、X-Trace-ID（Zr）。B3 规范只认
//     16/32 hex TraceId 与 16 hex SpanId；入站 conversationRequestID 非法时 TraceId
//     回落 messageID（恒 32 hex），SpanId 取 messageID[:16]、ParentSpanId 另取随机 16 hex。
//   - acp-connection-id：会话级 ACP 连接 id（官方为 session 级 UUID）。本网关按
//     conversationId 稳定派生 v4 形态 UUID；无 conversationId 时不注入。
func (c *Client) injectConversationHeaders(req *http.Request, meta ChatMeta) {
	convReqID := meta.ConversationRequestID
	if convReqID == "" {
		// 零值 meta（直接调 ChatHeaders 的调用方/测试）也要保证聚合主键必发：
		// 本级补一个 32 hex，调用方（handler）已生成稳定的不走到这里。
		convReqID = session.NewMessageID()
	}

	messageID := session.NewMessageID()

	if meta.ConversationID != "" {
		req.Header.Set("X-Conversation-Id", meta.ConversationID)
		req.Header.Set("Acp-Connection-Id", acpConnectionID(meta.ConversationID))
	}

	req.Header.Set("X-Conversation-Request-Id", convReqID)
	req.Header.Set("X-Conversation-Message-Id", messageID)
	req.Header.Set("X-Request-ID", messageID)
	req.Header.Set("X-Root-Request-Id", convReqID)

	traceID := meta.TraceID
	if traceID == "" {
		traceID = convReqID
	}

	req.Header.Set("X-Trace-Id", traceID)

	b3Trace := convReqID
	if !validTraceID(b3Trace) {
		b3Trace = messageID // 非法 B3 TraceId → 回落恒 32 hex 的消息级 ID
	}

	spanID := messageID[:16]
	parentSpanID := session.NewMessageID()[:16]
	// traceparent（W3C）：00-<32hex trace>-<16hex span>-01；b3（单头）：<trace>-<span>-1-<parent>。
	req.Header.Set("Traceparent", "00-"+b3Trace+"-"+spanID+"-01")
	req.Header.Set("B3", b3Trace+"-"+spanID+"-1-"+parentSpanID)
	req.Header.Set("X-B3-Traceid", b3Trace)
	req.Header.Set("X-B3-Parentspanid", parentSpanID)
	req.Header.Set("X-B3-Spanid", spanID)
	req.Header.Set("X-B3-Sampled", "1")
}

// acpConnectionID 由会话 id 稳定派生 acp-connection-id（官方桌面端为会话级 UUID）。
// 幂等：同一 conversationId 恒同值；输出为 v4 形态 UUID（8-4-4-4-12）。
func acpConnectionID(conversationID string) string {
	sum := sha256.Sum256([]byte("wb2a:acp:" + conversationID))
	h := hex.EncodeToString(sum[:16]) // 32 hex

	return h[0:8] + "-" + h[8:12] + "-4" + h[13:16] + "-a" + h[17:20] + "-" + h[20:32]
}

// validTraceID 判断 B3 TraceId 是否合法：16 或 32 位 hex（大小写均可）。
// 官方客户端生成的 conversationRequestId 是 32 位 hex（UUID 去横线），入站透传值
// 可能是任意形状（含横线/超长/非 hex），直接塞进 B3 头会破坏链路关联（issue #35）。
func validTraceID(str string) bool {
	if len(str) != 16 && len(str) != 32 {
		return false
	}

	for i := range len(str) {
		ch := str[i]
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') && (ch < 'A' || ch > 'F') {
			return false
		}
	}

	return true
}

// attributionClientName 生效的自定义归属名：ClientName 非空取之；空 = 默认官方桌面端
// 形态（X-IDE-Name/Type=WorkBuddy、X-Product=SaaS）。仅 billingUA 用它判断是否禁用 UA。
func (c *Client) attributionClientName() string {
	if c != nil && c.ClientName != "" {
		return c.ClientName
	}

	return "WorkBuddy"
}

// injectAttribution 注入桌面客户端 chat 归属头（X-Agent-Purpose / X-Agent-Intent /
// X-Agent-Type / X-IDE-* / X-Product / X-Private-Data）。仅在 chat/completions 路径生效。
//
// 默认（ClientName 空）对齐官方桌面端 chat 指纹：
//
//	X-Agent-Purpose=conversation、X-Agent-Intent=craft、X-Agent-Type=main、
//	X-Private-Data=true、X-IDE-Name/Type=WorkBuddy、X-IDE-Version=<realm 版本>、
//	X-Product=SaaS（=deploymentType）。
//
// 显式 ClientName 非空时 X-Product 与 X-IDE-Name/Type 跟随该值（自定义归因逃生门）。
func (c *Client) injectAttribution(req *http.Request, account4 *auth.Auth) {
	req.Header.Set("X-Agent-Purpose", "conversation")
	req.Header.Set("X-Agent-Intent", "craft")
	req.Header.Set("X-Agent-Type", "main")
	req.Header.Set("X-Private-Data", "true")

	name := "WorkBuddy"
	product := "SaaS" // 官方 chat X-Product=deploymentType（SaaS）

	if c != nil && c.ClientName != "" {
		name = c.ClientName
		product = c.ClientName
	}

	req.Header.Set("X-Ide-Name", name)
	req.Header.Set("X-Ide-Type", name)
	req.Header.Set("X-Ide-Version", c.clientVersionFor(account4))
	req.Header.Set("X-Product", product)
}

// injectClientIP 在 PassthroughIP 开启时把 clientIP 参数透传给上游（三等价头）。
func (c *Client) injectClientIP(req *http.Request, clientIP string) {
	if c == nil || !c.PassthroughIP || clientIP == "" {
		return
	}

	req.Header.Set("X-Forwarded-For", clientIP)
	req.Header.Set("X-Real-IP", clientIP)
	req.Header.Set("X-Client-Ip", clientIP)
}

// ExtractClientIP 从入站请求提取客户端 IP 首段（X-Forwarded-For 首段，回落 X-Real-IP）。
func ExtractClientIP(r *http.Request) string {
	if r == nil {
		return ""
	}

	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		for i := range len(xff) {
			if xff[i] == ',' {
				return strings.TrimSpace(xff[:i])
			}
		}

		return strings.TrimSpace(xff)
	}

	if realIP := strings.TrimSpace(r.Header.Get("X-Real-IP")); realIP != "" {
		return realIP
	}

	return ""
}

// BillingHeaders billing 接口请求头。
// UA 语义：默认**不设置**（保持现状，Go 客户端自带默认 UA）；仅当显式配置
// c.UserAgent 非空才覆盖——避免默认路径给 billing 引入新的 UA 指纹。
func (c *Client) BillingHeaders(req *http.Request, account6 *auth.Auth) {
	// AccessToken 加锁快照（同 ChatHeaders：keepalive 可在 a.mu 内改写）。
	req.Header.Set("Authorization", "Bearer "+account6.AccessTokenValue())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	c.injectCodeBuddyRequest(req)
	// Accept-Language 按 realm 切（D5，billing 域未走 CommonHeaders，单独注入）。
	req.Header.Set("Accept-Language", acceptLanguageFor(account6))

	if c != nil && c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	} else if ua := c.billingUA(account6); ua != "" {
		req.Header.Set("User-Agent", ua)
	}

	if account6.UID != "" {
		req.Header.Set("X-User-Id", account6.UID)
	}

	if account6.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", account6.EnterpriseID)
		req.Header.Set("X-Tenant-Id", account6.EnterpriseID)
	}

	if d := account6.DomainValue(); d != "" {
		req.Header.Set("X-Domain", d)
	}
	// 设备风控头：billing 域（report/travel/balance/checkin）同样注入。
	c.injectDeviceToken(req, account6)
}

// RefreshHeaders refresh 端点专属头（X-Refresh-Token 只允许出现在这里）。
func (c *Client) RefreshHeaders(req *http.Request, a *auth.Auth) {
	c.CommonHeaders(req, a)
	req.Header.Set("X-Refresh-Token", a.RefreshToken)

	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
	}
	// X-Auth-Refresh-Source 对齐官方客户端 refresh 渠道标识 "plugin"（D3）。
	req.Header.Set("X-Auth-Refresh-Source", "plugin")
}
