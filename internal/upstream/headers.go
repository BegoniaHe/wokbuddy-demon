// Package headers 构造出站请求头族。
//
// 头族按 **surface** 维度集中构造（见 applyHeaders）：chat 与 agenttool 四路复用 chat
// 头族，billing / report / refresh / v3-config / growth 走「非 chat 轻头族」base 再按面
// 覆盖差异项。同一 surface 的差异只在本文件表达——调用点不得各自 Set / Del，否则
// 「哪些面发什么头」会散到多个调用点，改一次口径要翻整仓。
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

// surface 上游请求面：决定出站头族（以及 chat 面的 body 编码）。
type surface int

const (
	surfaceChat surface = iota
	surfaceSearch
	surfaceWebFetch
	surfaceAgenticSearch
	surfaceImages
	surfaceModelBusy
	surfaceBilling
	surfaceReport
	surfaceRefresh
	surfaceV3Config
	surfaceGrowth
)

// chatFamily 是否复用 chat 头族。
//
// chat 与 agenttool 四路（search / webfetch / agentic_search / images）共用同一套
// 桌面 chat 指纹；四个 agenttool 面没有真机头集证据，按「不猜测」维持既有形态
// （见 internal/upstream/UPSTREAM.md §六）。
func (s surface) chatFamily() bool {
	switch s {
	case surfaceChat, surfaceSearch, surfaceWebFetch, surfaceAgenticSearch, surfaceImages:
		return true
	default:
		return false
	}
}

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

// chatBrandFor 返回官方 CLI chat UA 的品牌段（`cli/product.json` 的
// `applicationName`）：global → `workbuddy-ai`，其余 → `WorkBuddy`。
// 只放品牌字面量，不放版本号；版本仍取 profile / 显式覆盖。
func chatBrandFor(account *auth.Auth) string {
	if account != nil && account.IsGlobal() {
		return "workbuddy-ai"
	}

	return "WorkBuddy"
}

// defaultWorkBuddyUAFor 返回 profile 冻结的客户端出站 UA（官方 CLI chat 拦截器形状：
// `WorkBuddy/<ver> <brand>/<ver> CLI/<cliVer>`，品牌段来自 `cli/product.json` 的
// `applicationName`，不是 desktop surface 的同名字段）。
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

	return "WorkBuddy/" + v + " " + chatBrandFor(account) + "/" + v + " CLI/" + c.cliVersionFor(account)
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

// billingUA billing 域 report / banner 类显式 UA：单段 `WorkBuddy/<clientVersion>`
// （官方 banner/上报显式覆写形态，不带 CLI 段）。默认取 profile 字面量；显式
// client_version 覆盖时按该版本拼装；client_name="SaaS" 返回空串（不写 UA 键，
// 回落 Go 默认 UA）。
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

// billingReportUA billing 域 /v2/report 的 UA：显式 user_agent 优先，其余回落
// billingUA；空串表示不写 UA 键、回落 Go 默认 UA（client_name="SaaS"）。
func (c *Client) billingReportUA(a *auth.Auth) string {
	if c != nil && c.UserAgent != "" {
		return c.UserAgent
	}

	return c.billingUA(a)
}

// desktopUA 桌面产品面 UA：显式 user_agent 覆盖优先，否则 profile 冻结的桌面三段式
// （与 chat 的 chatUA 是两条 surface，不可互相顶替）。
func (c *Client) desktopUA(account *auth.Auth) string {
	if c != nil && c.UserAgent != "" {
		return c.UserAgent
	}

	return c.desktopUAFor(account)
}

// billingMeterUA billing meter 类面（get-user-resource / summary / packages /
// 签到 / balance）的 UA。
//
// 2026-10-28 真机抓包（mitmproxy 解密）证伪了「billing `context.http` 走
// RestOperations 全局桌面 UA 拦截器」的推断：这三个端点没有任何拦截器，UA 就是
// axios 库自身默认串。该值来自抓包而非包内片段，按仓库政策冻结在 profile 的
// emit.billingMeterUA（source=traffic），不在代码里按版本号拼。
//
// 优先级：显式 user_agent > profile 抓包值 > 桌面三段式（profile 未重抓时回落，
// 行为与灰度前一致）。client_name="SaaS" 是显式逃生门——不写 UA 键，保留 Go
// transport 默认 UA。
func (c *Client) billingMeterUA(account *auth.Auth) string {
	if c == nil || c.attributionClientName() == "SaaS" {
		return ""
	}

	if c.UserAgent != "" {
		return c.UserAgent
	}

	if p := c.profileFor(account); p != nil && p.Emit.BillingMeterUA != "" {
		return p.Emit.BillingMeterUA
	}

	return c.desktopUAFor(account)
}

// lightUA 轻头族各面的 UA：显式 user_agent 覆盖优先（各面自带默认见下）。
func (c *Client) lightUA(srf surface, account *auth.Auth) string {
	switch srf {
	case surfaceBilling:
		return c.billingMeterUA(account)
	case surfaceGrowth:
		// /v2/activity/growth/* 是 Node 原生 fetch 裸调用，真机 UA 直接是 "node"。
		return "node"
	case surfaceReport:
		return c.billingReportUA(account)
	case surfaceModelBusy:
		// 渲染层探针面：真机是与「桌面产品面」同源的三段式 UA。
		return c.desktopUA(account)
	case surfaceRefresh, surfaceV3Config:
		// refresh / v3-config：与 chat 同源的客户端 UA（v3-config 由调用方按
		// 探测目标换成 IDE / CLI UA，见 V3ConfigHeaders）。
		return c.userAgent(account)
	case surfaceChat, surfaceSearch, surfaceWebFetch, surfaceAgenticSearch, surfaceImages:
		// 走 chat 头族，不经轻头族。
		return ""
	}

	return ""
}

// resolveDeviceToken 解析签到专用 X-Device-Token 的取值。
// 优先级：auth.Auth.DeviceToken（每号）> Client.DeviceToken（config 全局）> 文件兜底。
// 三者皆空/读失败则返回空串（调用方不注入该头，优雅降级）。
// 官方只在 /v2/plugin/auth/state 与签到（daily-checkin / checkin-activity-status）
// 消费该头，本仓只在签到路径注入（见 checkinHeaders；#342）。
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

// checkinHeaders 签到专用请求头：X-Device-Token（取值为空时不注入）。
// 官方 5.9.3 只在该头的 /v2/plugin/auth/state 与签到消费点发；chat / images /
// search / webfetch / modelbusy 与 billing 默认面（report / travel / balance 等）
// 都不带（#342）。auth/state 是面板登录面，不经过 internal/upstream。
func (c *Client) checkinHeaders(a *auth.Auth) map[string]string {
	if tok := c.resolveDeviceToken(a); tok != "" {
		return map[string]string{"X-Device-Token": tok}
	}

	return nil
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

// applyHeaders 出站头族唯一构造入口：按 surface 分发到 chat 头族或轻头族。
//
//nolint:gocritic // ChatMeta 值传递与 ChatHeaders 保持同一公开 API。
func (c *Client) applyHeaders(
	srf surface, req *http.Request, account *auth.Auth, clientIP string, meta ChatMeta,
) {
	if srf.chatFamily() {
		c.chatFamilyHeaders(srf, req, account, clientIP, meta)

		return
	}

	c.lightHeaders(srf, req, account)
}

// chatFamilyHeaders chat 与 agenttool 四路的头族：CommonHeaders 基础 +
// chat 身份 / 归属 / 会话头族，按 surface 覆盖 Accept 与 Content-Type。
//
//nolint:gocritic // ChatMeta 值传递与 ChatHeaders 保持同一公开 API。
func (c *Client) chatFamilyHeaders(
	srf surface, req *http.Request, account *auth.Auth, clientIP string, meta ChatMeta,
) {
	c.CommonHeaders(req, account)

	// X-Codebuddy-Request: 1（风控闸门头）。真机 2026-10-28 抓包：7 条 chat 全带，
	// refresh / busy / report / v3-config / billing / growth 共 14 条非 chat 业务请求
	// 一条都没有，故只在本族（chat + agenttool 四路，agenttool 无真机证据、沿 chat
	// 头族一起发）注入；轻头族各面不发（#368）。
	req.Header.Set("X-Codebuddy-Request", "1")
	// chat 流式 Accept 覆盖 CommonHeaders 的非流式默认（D6）。
	req.Header.Set("Accept", "application/json, text/event-stream")
	// 官方 5.9.3 在 chat / images / search / webfetch 全发 X-Agent-Runtime；
	// 网关没有 session meta / CODEBUDDY_AGENT_RUNTIME 来源，统一发官方兜底值 other。
	req.Header.Set("X-Agent-Runtime", "other")
	// AccessToken 加锁快照：keepalive 定时刷新会在 a.mu 内改写它，锁外直读构成数据竞争
	// （见 auth.AccessTokenValue 注释）。
	if at := account.AccessTokenValue(); at != "" {
		req.Header.Set("Authorization", "Bearer "+at)
	} else {
		req.Header.Set("X-No-Authorization", "1")
	}

	if account.UID != "" {
		req.Header.Set("X-User-Id", account.UID)
	} else {
		req.Header.Set("X-No-User-Id", "1")
	}
	// 安全红线：绝不在 chat 请求里携带 X-Refresh-Token。
	// 企业与域头按 realm 分发：CN 走既有分支（EnterpriseID/Domain 原样透传，缺省 X-No-*）；
	// global 账号由 injectGlobalChatHeaders 统一注入国际客户端形态
	// （企业：X-Enterprise-Id + X-Tenant-Id；个人：X-No-Enterprise-Id=1；
	// X-Domain 账号 domain 优先，缺省 www.workbuddy.ai）。
	if account != nil && !account.IsGlobal() {
		injectCNChatHeaders(req, account)
	} else {
		c.injectGlobalChatHeaders(req, account)
	}
	// 用量归属头：默认伪造官方桌面端指纹（client_name 非空 = 自定义归属逃生门）。
	c.injectAttribution(req, account)
	// 企业/静态 Key 的 CLI 身份头（个人号不注入）。
	c.injectIdentityHeaders(req, account)
	// 客户端 IP 透传（仅 PassthroughIP=true 且本次请求带 IP）。
	c.injectClientIP(req, clientIP)
	// X-Device-Token 不注入 chat：官方只在该头的 /v2/plugin/auth/state 与签到
	// 消费点使用，chat / search / webfetch / images / modelbusy 都不带（#342）。
	// 签到专用注入见 Client.checkinHeaders。
	// 会话头族（对话/请求/消息/B3 链路），见 injectConversationHeaders。
	c.injectConversationHeaders(req, meta)

	c.chatSurfaceOverrides(srf, req, account)
}

// chatSurfaceOverrides chat 头族内的 surface 差异：agenttool 四路是同步 JSON / SSE，
// 头集与 chat 流式不同。
func (*Client) chatSurfaceOverrides(srf surface, req *http.Request, account *auth.Auth) {
	switch srf {
	case surfaceSearch, surfaceWebFetch, surfaceAgenticSearch:
		// 官方 WebSearchTool / fetchContentViaApi 头族：非流式 JSON，Content-Type
		// 带 charset；agentic_search 走 SSE，Accept 契约优先。
		req.Header.Set("Content-Type", "application/json;charset=UTF-8")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("X-Agent-Intent", "craft")

		if srf == surfaceAgenticSearch {
			req.Header.Set("Accept", "text/event-stream")
		}
		// X-Wb-Uid 只在 webfetch（与 knowledge_search）发，取值与 X-User-Id 同源。
		if srf == surfaceWebFetch && account != nil && account.UID != "" {
			req.Header.Set("X-Wb-Uid", account.UID)
		}
	case surfaceImages:
		// 出图是非流式 JSON：Accept 改回 JSON，Content-Type 保持 chat 形态。
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Content-Type", "application/json")
	case surfaceChat, surfaceModelBusy, surfaceBilling, surfaceReport, surfaceRefresh, surfaceV3Config,
		surfaceGrowth:
		// chat 面沿用上面构造的 chat 流式头集；轻头族各面已在 applyHeaders 分流。
	}
}

// lightHeaders 非 chat「轻头族」base：Accept / Content-Type / UA / Authorization /
// X-User-Id / X-Domain / X-Request-ID；各面的差异项见 lightSurfaceOverrides。
// 真机证据逐面见 UPSTREAM.md §六。
func (c *Client) lightHeaders(srf surface, req *http.Request, account *auth.Auth) {
	accept := "application/json"
	if srf == surfaceModelBusy {
		// 渲染层 wb.http.post 的宽松 Accept（真机 /v2/chat/busy）。
		accept = "application/json, text/plain, */*"
	}

	req.Header.Set("Accept", accept)
	req.Header.Set("Content-Type", "application/json")
	// AccessToken 加锁快照（同 chatFamilyHeaders：keepalive 可在 a.mu 内改写）。
	req.Header.Set("Authorization", "Bearer "+account.AccessTokenValue())
	// X-Request-ID：官方 HTTP 层给每条非 chat 业务请求补 32 hex 随机值（UUIDv7 形态）。
	// growth 面是 Node 原生 fetch，真机不带该头；chat 面的同名头是消息级 ID
	// （= X-Conversation-Message-ID），语义不同，见 injectConversationHeaders。
	if srf != surfaceGrowth {
		req.Header.Set("X-Request-ID", session.NewUUIDv7Hex())
	}

	if ua := c.lightUA(srf, account); ua != "" {
		req.Header.Set("User-Agent", ua)
	}

	if account != nil && account.UID != "" {
		req.Header.Set("X-User-Id", account.UID)
	}

	// X-Domain：账号 domain 优先，否则取 chat base 的 host（真机各面都是 host 形态）。
	req.Header.Set("X-Domain", domainHostFor(account, c.chatBase(account)))

	c.lightSurfaceOverrides(srf, req, account)
}

// lightSurfaceOverrides 轻头族各面的差异项。
//
//nolint:cyclop,gocyclo // 头族按 surface 线性展开；拆得更碎反而更难对照真机表。
func (*Client) lightSurfaceOverrides(srf surface, req *http.Request, account *auth.Auth) {
	switch srf {
	case surfaceModelBusy:
		// 渲染层高频探针：真机恰好 8 个轻头——Accept / Content-Type /
		// User-Agent / X-Request-ID / X-Product / Authorization / X-User-Id / X-Domain。
		// 没有会话四件套、acp-connection-id、baggage、b3 / trace、X-Requested-With、
		// X-Codebuddy-Request，也没有企业头（真机样本是个人号；企业号是否带
		// X-Enterprise-Id / X-Tenant-Id 无证据，按「不猜测」不发，多一条反而是
		// 更明显的非官方指纹。见 UPSTREAM.md §六，issue #371）。
		req.Header.Set("X-Product", "SaaS")
	case surfaceRefresh:
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		// X-Domain：账号 domain 优先；global 缺省固定 www.workbuddy.ai；CN 个人号
		// 无 domain 时官方回落 endpoint authority，这里取本次 refresh 请求的实际
		// host（可能是 chat 回落链里的备选域，不能只看首选 origin）。
		if d := account.DomainValue(); d != "" {
			req.Header.Set("X-Domain", d)
		} else if account.IsGlobal() {
			req.Header.Set("X-Domain", "www.workbuddy.ai")
		} else if host := req.URL.Host; host != "" {
			req.Header.Set("X-Domain", host)
		}

		injectEnterpriseHeaders(req, account)
	case surfaceV3Config:
		// 该端点对 Accept 也要宽松值（官方 RestOperations 默认）；轻头族 base 的
		// application/json 是收窄的，故在此还原。
		req.Header.Set("Accept", "application/json, text/plain, */*")
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		req.Header.Set("X-Product", "SaaS")
		injectEnterpriseHeaders(req, account)
	case surfaceGrowth:
		// 官方 growth 面是 Node fetch：UA=node、accept-language=*、sec-fetch-mode=cors，
		// 且**没有** X-Request-ID / X-Product / X-Requested-With / X-Codebuddy-Request。
		// Content-Type 在 GET 上也照发（真机如此，不按 HTTP 惯例省）。
		// 企业头维持既有行为（真机样本是个人号，是否该发无证据）。
		req.Header.Set("Accept-Language", "*")
		req.Header.Set("Sec-Fetch-Mode", "cors")

		if account.EnterpriseID != "" {
			req.Header.Set("X-Enterprise-Id", account.EnterpriseID)
			req.Header.Set("X-Tenant-Id", account.EnterpriseID)
		}
	case surfaceBilling, surfaceReport:
		// billing 域面（meter / report）：企业头只在真有值时发，个人号不发声明头。
		if account.EnterpriseID != "" {
			req.Header.Set("X-Enterprise-Id", account.EnterpriseID)
			req.Header.Set("X-Tenant-Id", account.EnterpriseID)
		}
	case surfaceChat, surfaceSearch, surfaceWebFetch, surfaceAgenticSearch, surfaceImages:
		// 不走轻头族（chatFamily() 已在 applyHeaders 分流）。
	}
}

// domainHostFor 轻头族的 X-Domain：账号 domain（去 scheme 与尾斜杠）优先，
// 否则取 chat base 的 host（真机为产品域 host 形态，不是完整 URL）。
func domainHostFor(account *auth.Auth, chatBase string) string {
	if account != nil {
		// Domain 加锁快照（见 auth.DomainValue：keepalive 刷新在 a.mu 内改写）。
		if d := strings.TrimSpace(account.DomainValue()); d != "" {
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

// CommonHeaders 模型目录系（FetchModels / 企业模型）共享的基础头：
// Content-Type / Accept / X-Requested-With / UA。
//
// 官方 CLI chat 拦截器与 daemon 网关头构造都不发 Origin / Referer /
// X-Machine-Id / X-Session-Id（见 UPSTREAM.md），这里也不注入，避免出站指纹
// 比官方客户端更多。web 端点（ClaimReward / FetchAccountProfile 等）在自己的
// 请求构造里按需设置 Origin / Referer。
func (c *Client) CommonHeaders(req *http.Request, account2 *auth.Auth) {
	req.Header.Set("Content-Type", "application/json")
	// Accept 非流式默认 application/json（D6：去掉宽松的 text/plain, */*）。
	// chat 流式路径在 chatFamilyHeaders 覆盖。
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("User-Agent", c.userAgent(account2))
	// chat/通用 API 不发 Accept-Language：官方 chat 路径没有该头（billing 域也只有
	// summary / packages 分页按 locale 带，见 fetchResourceSummary / fetchPackagePage）。
	// 为「看起来像浏览器」补该头反而增加非官方指纹（issue #249）。
}

// billingAcceptLanguage 按账号 realm 返回 billing 分页的 Accept-Language：
// global → en，cn → zh（官方 `resolveBillingAcceptLanguage` 返回语言短码，
// 不是 zh-CN / en-US）。只用于 summary 与 packages 两类请求，其余 billing
// 端点官方不发该头。
func billingAcceptLanguage(a *auth.Auth) string {
	if a != nil && a.IsGlobal() {
		return "en"
	}

	return "zh"
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

// ChatHeaders chat 面（/v2/chat/completions）请求头。
// clientIP 为本次请求的客户端 IP（按参数传递，不读共享字段——避免并发串扰）；
// PassthroughIP=false 或 clientIP 为空时不注入 IP 头。
// meta 为会话头族元数据，见 injectConversationHeaders。
//
//nolint:gocritic // ChatMeta 值传递保持公开 API 简单。
func (c *Client) ChatHeaders(req *http.Request, account3 *auth.Auth, clientIP string, meta ChatMeta) {
	c.applyHeaders(surfaceChat, req, account3, clientIP, meta)
}

// ImagesHeaders 出图面（/v2/images/generations）请求头。
//
//nolint:gocritic // ChatMeta 值传递与 ChatHeaders 保持同一公开 API。
func (c *Client) ImagesHeaders(req *http.Request, account *auth.Auth, clientIP string, meta ChatMeta) {
	c.applyHeaders(surfaceImages, req, account, clientIP, meta)
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
// 形态（X-IDE-Name/Type=WorkBuddy、X-Product=SaaS）。billingUA / billingMeterUA 与
// chatFamilyHeaders 共用它判断是否禁用 UA。
func (c *Client) attributionClientName() string {
	if c != nil && c.ClientName != "" {
		return c.ClientName
	}

	return "WorkBuddy"
}

// privateDataHeaderValue 返回 CLI chat 的 X-Private-Data 取值。
// CN 5.9.3 官方 `applyPrivateDataHeaderPolicy` 在个人号的默认分支发 false，
// 企业号因 `isEnterpriseEditionAccount`（enterpriseId 非空或 type 为
// ultimate / exclusive）命中同一判定而发 true；global 基线仍是 5.8.4，
// 只走 `DisableYuanbaoChannel=true` → `h=false` → true，等 global 升到 5.9.x
// 后按同一逻辑复核。
// 远端 abTest（DefaultModelOptimizationOff）与会话 meta `privateDataSticky`
// 都不在网关范围：本仓没有这两类状态来源，只对齐静态默认分支。
// 桌面专家 chat 路径不走本函数：官方桌面 `applyGatewayHeaders` 的兜底表
// 硬编码 true，见 setDesktopExpertHeaders。
func privateDataHeaderValue(account *auth.Auth) string {
	if account != nil && (account.IsGlobal() || account.IsEnterpriseEdition()) {
		return "true"
	}

	return "false"
}

// injectAttribution 注入桌面客户端 chat 归属头（X-Agent-Purpose / X-Agent-Intent /
// X-Agent-Type / X-IDE-* / X-Product / X-Private-Data）。仅在 chat 头族生效。
//
// 默认（ClientName 空）对齐官方桌面端 chat 指纹：
//
//	X-Agent-Purpose=conversation、X-Agent-Intent=craft、X-Agent-Type=main、
//	X-Private-Data=<见 privateDataHeaderValue>、
//	X-IDE-Name/Type=WorkBuddy、X-IDE-Version=<realm 版本>、X-Product=SaaS（=deploymentType）。
//
// 显式 ClientName 非空时 X-Product 与 X-IDE-Name/Type 跟随该值（自定义归因逃生门）。
func (c *Client) injectAttribution(req *http.Request, account4 *auth.Auth) {
	req.Header.Set("X-Agent-Purpose", "conversation")
	req.Header.Set("X-Agent-Intent", "craft")
	req.Header.Set("X-Agent-Type", "main")
	req.Header.Set("X-Private-Data", privateDataHeaderValue(account4))

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

// BillingHeaders billing meter 族（/billing/meter/*、balance、签到）请求头。
// 官方 `buildHeaders` 只有 Accept / Authorization / Content-Type / X-User-Id
// （外加按需的企业与 X-Domain 头），没有 Accept-Language、没有
// X-Requested-With，也没有 X-Device-Token（签到单独注入，见 Client.checkinHeaders）。
// UA 见 billingMeterUA（axios 默认串，取自抓包）。
func (c *Client) BillingHeaders(req *http.Request, account6 *auth.Auth) {
	c.applyHeaders(surfaceBilling, req, account6, "", ChatMeta{})
}

// ReportHeaders 活跃上报面（POST {billingBase}/v2/report）请求头。
func (c *Client) ReportHeaders(req *http.Request, account *auth.Auth) {
	c.applyHeaders(surfaceReport, req, account, "", ChatMeta{})
}

// ModelBusyHeaders 渲染层繁忙度探针面（/v2/chat/busy）请求头。
func (c *Client) ModelBusyHeaders(req *http.Request, account *auth.Auth) {
	c.applyHeaders(surfaceModelBusy, req, account, "", ChatMeta{})
}

// GrowthHeaders growth 面（/v2/activity/growth/*：任务、猫猫旅行、连登、礼包）请求头。
func (c *Client) GrowthHeaders(req *http.Request, account *auth.Auth) {
	c.applyHeaders(surfaceGrowth, req, account, "", ChatMeta{})
}

// RefreshHeaders refresh 端点专属头（X-Refresh-Token 只允许出现在这里）。
func (c *Client) RefreshHeaders(req *http.Request, account *auth.Auth) {
	c.applyHeaders(surfaceRefresh, req, account, "", ChatMeta{})
	req.Header.Set("X-Refresh-Token", account.RefreshToken)
	// X-Auth-Refresh-Source 对齐官方客户端 refresh 渠道标识 "plugin"（D3）。
	req.Header.Set("X-Auth-Refresh-Source", "plugin")

	// 官方 refresh 复用当前 trace span；网关侧无活动 span，故生成一组自洽的新链路 id。
	injectTraceHeaders(req, session.NewUUIDv7Hex(), session.NewUUIDv7Hex(), session.NewUUIDv7Hex())
}

// V3ConfigHeaders /v3/config 头族。该端点对 UA 敏感（必须带 CodeBuddy/CodeBuddyIDE
// 版本，否则 400 code=12403），UA 由调用方按探测目标给出；空串等价 codeBuddyIDEUA。
func (c *Client) V3ConfigHeaders(req *http.Request, account *auth.Auth, probeUA string) {
	c.applyHeaders(surfaceV3Config, req, account, "", ChatMeta{})

	if probeUA == "" {
		probeUA = codeBuddyIDEUA
	}

	req.Header.Set("User-Agent", probeUA)
}
