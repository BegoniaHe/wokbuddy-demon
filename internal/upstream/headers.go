// Package headers 构造三类上游请求头（common / chat / billing / refresh）。
// 规则来自 docs/api-reference.md §0/§4/§6。
package upstream

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/clientprofile"
	"github.com/BegoniaHe/wokbuddy-demon/internal/session"
)

// profileFor 返回账号 realm 的部署 profile；未配置返回 nil（启动期已拒绝该状态）。
func (c *Client) profileFor(a *auth.Auth) *clientprofile.Profile {
	if c == nil || c.Profile == nil {
		return nil
	}

	if a != nil && a.IsGlobal() {
		return c.Profile.ForRealm("global")
	}

	return c.Profile.ForRealm("cn")
}

// clientVersionFor 生效的 WorkBuddy 客户端版本：显式 ClientVersion 优先，其次是
// profile 的 xIdeVersion。两者皆空返回空串（启动期已要求 profile 覆盖每个启用 realm）。
//
//nolint:dupl // client/cli 版本解析结构一致，仅字段与 selector 不同。
func (c *Client) clientVersionFor(account *auth.Auth) string {
	if c != nil && c.ClientVersion != "" {
		return c.ClientVersion
	}

	return c.profileVersion(account, func(p *clientprofile.Profile) string { return p.Emit.XIdeVersion })
}

// cliVersionFor 生效的 CLI 版本：显式 CliVersion 优先，其次是 profile 的
// source.cliVersion。两者皆空返回空串。
//
//nolint:dupl // 同 clientVersionFor：结构一致。
func (c *Client) cliVersionFor(account *auth.Auth) string {
	if c != nil && c.CliVersion != "" {
		return c.CliVersion
	}

	return c.profileVersion(account, func(p *clientprofile.Profile) string { return p.Source.CLIVersion })
}

// profileVersion 从账号所属 profile 读取版本字段。
func (c *Client) profileVersion(account *auth.Auth, pick func(*clientprofile.Profile) string) string {
	if c == nil {
		return ""
	}

	if p := c.profileFor(account); p != nil {
		return pick(p)
	}

	return ""
}

// defaultWorkBuddyUAFor 返回 profile 冻结的客户端出站 UA（官方 CLI chat 拦截器形状：
// `WorkBuddy/<ver> <brand>/<ver> CLI/<cliVer>`，品牌段来自 realm 的 applicationName）。
// 默认不拼接版本，直接使用 profile 字面量；
// 用户显式配 client_version / cli_version 时才按该版本重新拼装，避免 UA 与
// X-Ide-Version 自相矛盾。profile 缺失返回空串（启动期已拒绝该状态）。
func (c *Client) defaultWorkBuddyUAFor(account *auth.Auth) string {
	if c == nil {
		return ""
	}

	if c.ClientVersion == "" && c.CliVersion == "" {
		if p := c.profileFor(account); p != nil {
			return p.Emit.ChatUA
		}

		return ""
	}

	v := c.clientVersionFor(account)

	return "WorkBuddy/" + v + " WorkBuddy/" + v + " CLI/" + c.cliVersionFor(account)
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
// （官方 banner/上报显式覆写形态，不带 CLI 段）。默认取 profile 字面量；
// 显式 client_version 覆盖时按该版本拼装；client_name="SaaS" 才不设 UA（Go 默认 UA）。
func (c *Client) billingUA(account *auth.Auth) string {
	if c == nil || c.attributionClientName() == "SaaS" {
		return ""
	}

	if c.ClientVersion == "" {
		if p := c.profileFor(account); p != nil {
			return p.Emit.BillingUA
		}

		return ""
	}

	return "WorkBuddy/" + c.clientVersionFor(account)
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

// UUID version/variant 位掩码（RFC 4122）。
const (
	uuidVersionMask  = 0x0f
	uuidVariantMask  = 0x3f
	uuidVariantBits  = 0x80
	uuidVersion4Bits = 0x40
)

// deriveUUIDv4 从任意种子稳定派生 UUIDv4 形态字符串（8-4-4-4-12，version=4、
// variant=10xx）。用于设备/会话等需要跨重启稳定、但输出必须符合官方 UUID
// 字符集与版本位的标识；种子相同则输出恒同。
func deriveUUIDv4(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	b := sum[:16]
	b[6] = b[6]&uuidVersionMask | uuidVersion4Bits
	b[8] = b[8]&uuidVariantMask | uuidVariantBits

	h := hex.EncodeToString(b)

	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// CommonHeaders 设置所有 API 共享的请求头。
//
// 官方 CLI chat 拦截器与 daemon 网关头构造都不发 Origin / Referer /
// X-Machine-Id / X-Session-Id（见 UPSTREAM.md），这里也不注入，避免出站指纹
// 比官方客户端更多。web 端点（ClaimReward / FetchAccountProfile 等）在自己的
// 请求构造里按需设置 Origin / Referer。
func (c *Client) CommonHeaders(req *http.Request, account2 *auth.Auth) {
	req.Header.Set("Content-Type", "application/json")
	// Accept 非流式默认 application/json（D6：去掉宽松的 text/plain, */*）。
	// chat 流式路径在 ChatHeaders 覆盖为 event-stream。
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("User-Agent", c.userAgent(account2))
	// X-CodeBuddy-Request: 1（D1 风控闸门头）。官方 chat adapter 未注入，
	// daemon/ACP 注入；本仓无实测证明移除安全，按 D1 保留（见 UPSTREAM.md §六）。
	req.Header.Set("X-Codebuddy-Request", "1")
	// chat/通用 API 不发 Accept-Language：官方 chat 路径没有该头（只有 billing/
	// account 分页按 locale 带，见 BillingHeaders）。为「看起来像浏览器」补该头反而
	// 增加非官方指纹（issue #249）。
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
func (*Client) injectCodeBuddyRequest(req *http.Request) {
	req.Header.Set("X-Codebuddy-Request", "1")
}

// ChatMeta 一次 chat 出站的会话头族元数据（issue #35：后台按 X-Conversation-Request-ID
// 聚合请求，官方客户端一次 user send 内所有 tool call/重试/换号复用同一个 ID）。
// handler 在轮转循环外生成 conversationID / conversationRequestID，循环内每次出站
// 复用同值；TraceID 透传入站值（空 = 回落 conversationRequestID）。
// messageID（消息级，每条独立）由 ChatHeaders 内部生成，无需外部可见。
type ChatMeta struct {
	ConversationID        string // X-Conversation-ID：入站头/body 提取值，非空时优先
	ConversationHeaderID  string // X-Conversation-ID 出站兜底：handler 合成；仅进出站头，不进 prompt cache key。为空时最终生成 UUIDv4
	ConversationRequestID string // X-Conversation-Request-ID：聚合主键，必发
	RootRequestID         string // X-Root-Request-ID：入站透传值，空则回落 conversationRequestID
	ParentConversationID  string // X-Parent-Conversation-ID：入站透传值，空则不发
	AgentIntent           string // X-Agent-Intent：入站透传值，空则保留默认指纹
	AgentPurpose          string // X-Agent-Purpose：入站透传值，空则保留默认指纹
	AgentType             string // X-Agent-Type：入站透传值，空则保留默认指纹
	Compaction            string // X-Compaction：入站透传值，空则不发
	ContextCompacted      string // X-Context-Compacted：入站透传值，空则不发
	TraceID               string // X-Trace-ID：入站透传值，空则回落 conversationRequestID
}

// ChatHeaders 在 common 之上加 chat 专属的账号头。
// 缺省字段用 X-No-* 约定（与 CodeBuddy 官方 CLI 一致）。
// clientIP 为本次请求的客户端 IP（按参数传递，不读共享字段——避免并发串扰）；
// PassthroughIP=false 或 clientIP 为空时不注入 IP 头。
// meta 为会话头族元数据（纯新增，不改既有头），见 injectConversationHeaders。
//
//nolint:gocritic // ChatMeta 值传递保持公开 API 简单。
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
	// global 账号由 injectGlobalChatHeaders 统一注入国际客户端形态
	// （企业：X-Enterprise-Id + X-Tenant-Id；个人：X-No-Enterprise-Id=1；
	// X-Domain 账号 domain 优先，缺省 www.workbuddy.ai）。
	if account3 != nil && !account3.IsGlobal() {
		injectCNChatHeaders(req, account3)
	} else {
		c.injectGlobalChatHeaders(req, account3)
	}
	// 用量归属头：默认伪造官方桌面端指纹（client_name 非空 = 自定义归属逃生门）。
	c.injectAttribution(req, account3)
	// 企业/静态 Key 的 CLI 身份头（个人号不注入）。
	c.injectIdentityHeaders(req, account3)
	// 客户端 IP 透传（仅 PassthroughIP=true 且本次请求带 IP）。
	c.injectClientIP(req, clientIP)
	// 设备风控头：auth 每号 > config 全局 > 文件兜底；空则不注入。
	c.injectDeviceToken(req, account3)
	// 会话头族（对话/请求/消息/B3 链路），见 injectConversationHeaders。
	c.injectConversationHeaders(req, meta)
}

// injectCNChatHeaders CN 账号的 chat 域头：企业 ID / 租户 ID 与 Domain 原样透传
// （缺省 X-No-*）。
func injectCNChatHeaders(req *http.Request, account *auth.Auth) {
	injectEnterpriseHeaders(req, account)
	injectChatDomain(req, account)
}

// injectEnterpriseHeaders 按账号企业身份注入企业/租户头。
// 官方桌面端企业账号同时发 X-Enterprise-Id 与 X-Tenant-Id；个人账号显式声明
// X-No-Enterprise-Id=1，避免上游按缺失头做可疑判定。
func injectEnterpriseHeaders(req *http.Request, account *auth.Auth) {
	if account != nil && account.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", account.EnterpriseID)
		req.Header.Set("X-Tenant-Id", account.EnterpriseID)

		return
	}

	req.Header.Set("X-No-Enterprise-Id", "1")
}

// injectChatDomain 注入 chat 域头：账号 domain 优先；global 无 domain 时回落
// 国际版域 www.workbuddy.ai；CN 无 domain 保持既有 X-No-Department-Info 声明。
func injectChatDomain(req *http.Request, account *auth.Auth) {
	if d := account.DomainValue(); d != "" {
		req.Header.Set("X-Domain", d)

		return
	}

	if account != nil && account.IsGlobal() {
		req.Header.Set("X-Domain", "www.workbuddy.ai")

		return
	}

	// 有部门信息时由 injectIdentityHeaders 发 X-Department-Info 并删掉本声明
	// （两者互斥）。这里仍先写 X-No-Department-Info：只调 injectChatDomain 的
	// 端点（FetchModels 等）保持原有的「无部门」声明。
	req.Header.Set("X-No-Department-Info", "1")
}

// injectGlobalChatHeaders global 账号的 chat 专属声明头，对齐官方国际客户端：
//   - 企业账号：X-Enterprise-Id + X-Tenant-Id
//   - 个人账号：X-No-Enterprise-Id: 1
//   - X-Domain：账号 domain 优先，缺省 www.workbuddy.ai
func (*Client) injectGlobalChatHeaders(req *http.Request, account *auth.Auth) {
	if account == nil || !account.IsGlobal() {
		return
	}

	injectEnterpriseHeaders(req, account)
	injectChatDomain(req, account)
}

// injectConversationHeaders 注入官方客户端会话头族（issue #35 后台聚合）。
// 头族分四层，各司其职：
//   - X-Conversation-ID：会话级，多轮稳定（body 的 conversationId）。透传客户端
//     原值优先；handler 未给会话 ID 时用 ConversationHeaderID 兜底，再空则生成
//     UUIDv4（官方 session.id = crypto.randomUUID()）——上游渠道校验要求该头恒在。
//   - X-Conversation-Request-ID：**对话轮级聚合主键**，必发。一次 user send 内的
//     所有 tool call/重试/换号/降级复用同一个 → 后台按它聚合成一条（不再碎片化）。
//   - X-Conversation-Message-ID = X-Request-ID：消息级，每条独立（UUIDv7 32 hex）。
//   - X-Root-Request-ID：= conversationRequestID（根请求追踪）。
//   - X-Trace-ID / traceparent / b3 / X-B3-*：链路族。官方桌面端 daemon 用
//     randomBytes(16/8/8) 生成 traceId/spanId/parentSpanId 三件套，同时写 traceparent、
//     b3、X-B3-TraceId/ParentSpanId/SpanId/Sampled、X-Trace-ID（Zr）。B3 规范只认
//     16/32 hex TraceId 与 16 hex SpanId；入站 conversationRequestID 非法时 TraceId
//     回落 messageID（恒 32 hex），SpanId 取 messageID[:16]、ParentSpanId 另取随机 16 hex。
//   - acp-connection-id：会话级 ACP 连接 id（官方为 session 级 UUID）。本网关按
//     实际出站 conversationId（含 UUIDv4 兜底）稳定派生 v4 形态 UUID。
//
//nolint:gocritic // ChatMeta 值传递保持公开 API 简单；头族注入线性展开。
func (*Client) injectConversationHeaders(req *http.Request, meta ChatMeta) {
	convReqID := meta.ConversationRequestID
	if convReqID == "" {
		// 零值 meta（直接调 ChatHeaders 的调用方/测试）也要保证聚合主键必发：
		// 本级补一个 UUIDv7 32 hex，调用方（handler）已生成稳定的不走到这里。
		convReqID = session.NewUUIDv7Hex()
	}

	messageID := session.NewUUIDv7Hex()

	convID := meta.ConversationID
	if convID == "" {
		convID = meta.ConversationHeaderID
	}
	// 上游渠道校验要求 X-Conversation-ID 恒在（官方客户端每次请求都发）。
	// 客户端未给会话 ID 时用 UUIDv4 兜底（官方 session.id = crypto.randomUUID()）；
	// 只影响出站头，不覆盖 ConversationID（prompt cache key 的源）。
	if convID == "" {
		convID = session.NewUUIDv4()
	}

	req.Header.Set("X-Conversation-Id", convID)
	req.Header.Set("Acp-Connection-Id", acpConnectionID(convID))

	req.Header.Set("X-Conversation-Request-Id", convReqID)
	req.Header.Set("X-Conversation-Message-Id", messageID)
	req.Header.Set("X-Request-ID", messageID)

	if meta.RootRequestID != "" {
		req.Header.Set("X-Root-Request-Id", meta.RootRequestID)
	} else {
		req.Header.Set("X-Root-Request-Id", convReqID)
	}

	setHeaderIfNonEmpty(req, "X-Parent-Conversation-Id", meta.ParentConversationID)
	setHeaderIfNonEmpty(req, "X-Agent-Intent", meta.AgentIntent)
	setHeaderIfNonEmpty(req, "X-Agent-Purpose", meta.AgentPurpose)
	setHeaderIfNonEmpty(req, "X-Agent-Type", meta.AgentType)
	setHeaderIfNonEmpty(req, "X-Compaction", meta.Compaction)
	setHeaderIfNonEmpty(req, "X-Context-Compacted", meta.ContextCompacted)

	injectTraceHeaders(req, meta.TraceID, convReqID, messageID)
}

// injectTraceHeaders 注入链路头族（X-Trace-Id / traceparent / b3 / X-B3-*）。
// chat 与 refresh 共用：官方 refresh 复用当前 trace span；网关侧 refresh 没有
// 活动 span，由调用方生成一组自洽的新链路 id。
func injectTraceHeaders(req *http.Request, traceID, b3Seed, messageID string) {
	if traceID == "" {
		traceID = b3Seed
	}

	req.Header.Set("X-Trace-Id", traceID)

	b3Trace := b3Seed
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

// setHeaderIfNonEmpty 仅在 value 非空时设置请求头。
func setHeaderIfNonEmpty(req *http.Request, name, value string) {
	if value != "" {
		req.Header.Set(name, value)
	}
}

// acpConnectionID 由会话 id 稳定派生 acp-connection-id（官方桌面端为会话级 UUID）。
// 幂等：同一 conversationId 恒同值；输出为 v4 形态 UUID（8-4-4-4-12）。
func acpConnectionID(conversationID string) string {
	return deriveUUIDv4("wb2a:acp:" + conversationID)
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

// injectIdentityHeaders 注入官方 CLI 的身份头族（企业/静态 Key 形态）。
// 判据只看 enterpriseId / idSource：个人号即使 auth 文件残留 token_source
// 也不得注入身份头（issue #248，抓包级证据）。静态 Key 形态 enterpriseId 为空
// 但 idSource 非空，同样要发（issue #298）。
//
//nolint:cyclop,gocyclo // 身份头族按字段线性展开，拆分反而更碎。
func (*Client) injectIdentityHeaders(req *http.Request, account *auth.Auth) {
	if account == nil || account.UID == "" {
		return
	}

	method := account.TokenSource()

	enterprise := strings.TrimSpace(account.EnterpriseID)
	idSource := strings.TrimSpace(account.IDSource)

	if enterprise == "" && idSource == "" {
		return
	}

	if method != "" {
		req.Header.Set("X-Auth-Method", method)
	}

	if idSource != "" {
		req.Header.Set("X-Id-Source", idSource)
	}

	if department := strings.TrimSpace(account.DepartmentFullName); department != "" {
		req.Header.Del("X-No-Department-Info")
		req.Header.Set("X-Department-Info", department)
	}

	payload := map[string]any{"uin": account.UID}
	if enterprise != "" {
		payload["owner_uin"] = enterprise
	}

	if idSource != "" {
		payload["id_source"] = idSource
	}

	if method != "" {
		payload["token_source"] = method
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}

	req.Header.Set("X-Userinfo", base64.StdEncoding.EncodeToString(raw))
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
func (c *Client) RefreshHeaders(req *http.Request, account *auth.Auth) {
	c.CommonHeaders(req, account)
	req.Header.Set("X-Refresh-Token", account.RefreshToken)

	injectEnterpriseHeaders(req, account)

	// X-Domain：账号 domain 优先；global 缺省固定 www.workbuddy.ai；CN 个人号
	// 无 domain 时官方回落 endpoint authority，本仓同形回落 chat endpoint host。
	if d := account.DomainValue(); d != "" {
		req.Header.Set("X-Domain", d)
	} else if account.IsGlobal() {
		req.Header.Set("X-Domain", "www.workbuddy.ai")
	} else if host := hostOfChatOrigin(c, account); host != "" {
		req.Header.Set("X-Domain", host)
	}
	// X-Auth-Refresh-Source 对齐官方客户端 refresh 渠道标识 "plugin"（D3）。
	req.Header.Set("X-Auth-Refresh-Source", "plugin")

	// 官方 refresh 复用当前 trace span；网关侧无活动 span，故生成一组自洽的新链路 id。
	injectTraceHeaders(req, session.NewUUIDv7Hex(), session.NewUUIDv7Hex(), session.NewUUIDv7Hex())
}

// hostOfChatOrigin 返回账号 chat 首选 origin 的 host（CN 空配置已回落默认域）。
// 解析失败返回空串，调用方不注入 X-Domain。
func hostOfChatOrigin(c *Client, account *auth.Auth) string {
	origins := c.chatOrigins(account)
	if len(origins) == 0 {
		return ""
	}

	u, err := url.Parse(origins[0])
	if err != nil {
		return ""
	}

	return u.Host
}
