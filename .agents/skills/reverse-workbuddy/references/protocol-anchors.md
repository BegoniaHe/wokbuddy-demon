# 协议面锚点速查

调研中先在此定位本仓库对应实现，再逐项比对。行号会漂移，核对时以符号名为准。

## Realm 分域

- `internal/auth/auth.go`：`realmCN` / `realmGlobal`、`Realm()` / `ResolveRealm`。
  账号域判定的唯一来源；`a.IsGlobal()` 是各上游方法的金闸。
- 客户端权威声明：`<app-asar>/cli/product.json`
  （`endpoint` / `officialEndpoints` / `authentication.*`）。

## Base 与路径

- `internal/upstream/client.go`
  - `ChatBaseCN`（默认 `https://www.workbuddy.cn`）、`ChatBaseGlobal`；
    `globalOn()` 双闸（config `upstream.global_enabled` + 账号 realm）。
  - `chatCompletionsPath = "/v2/chat/completions"` 与 `chatPaths()`
    （global 曾走 `/console` 路径，因 WAF body 规则改回 `/v2` 单路径）。
  - `chatOrigins()` / `RefreshToken()`：CN 首 token 前按 workbuddy.cn →
    copilot.tencent.com → wb.tencentbuddy.com 回落。
  - `billingMeterPaths()`：两 realm 一致的无前缀优先（官方 `resourcePrefix` 空串），
    404 回落 `/v2`。`checkinMeterPaths()`：两 realm 均 `/v2` 优先
    （`DesktopAccountRepo.billingPrefix=/v2`），404 回落旧路径。
  - `WebBaseCN`（`https://www.workbuddy.cn`）：任务领奖 / profile 域。
  - `/v3/config` 拉取：`internal/upstream/client.go` 的 `fetchV3ConfigModelMap` /
    `newV3ConfigRequest`；头族见 `V3ConfigHeaders`（`internal/upstream/headers.go`）。
    可见性判定在此一并写入：`data.agents` 的默认会话 agent 白名单（#319）与
    `data.productFeaturesConfig.ModelRateLimitCap.lines[].paidId` 的付费线路变体
    （#402，`internal/upstream/paidline.go`，排除表 `yae` 见渲染层
    `renderer/assets/common-*.js` 的 `isIOAEnterpriseId` 常量）。
  - 企业模型目录：`/console/enterprises/<enterpriseId|personal>/models`（官方
    `CloudAgentProvider.getModels()` 按账号拼 `enterpriseId`），失败回落
    `/v2/enterprises/personal/models`（官方 daemon 侧固定 personal）。
- 桌面指纹上报：`internal/upstream/desktop.go`：`/v2/report`、
  `/v2/user-asset/appearance/set`、`desktopFingerprint`（commit / releaseDate
  来自部署 profile，源码无固定构建常量；`os` / `arch` 取报告服务
  `OSEventProcessor` 的 `process.platform` / `process.arch`（`win32` / `x64`），
  `osVersion` / `cpuModel` / `cpuCores` / `memorySize` 取宿主。事件体**不写**
  `userAgent`；BuddyEventProcessor 在无 buddy 上下文时写空串 `buddyId` /
  `buddyName`。旧口径 `userAgent=Node.js/22` / `os=Win32` 来自
  `CloudProductMetricsReporter` 的另一条上报路径，不属于本事件链）。

## 请求头族

- `internal/upstream/headers.go`
  - `userAgent()` / `desktopUAFor()` 默认取部署 profile 的冻结字面量
    （`internal/clientprofile`）；显式 config `user_agent` / `client_version` /
    `cli_version` 才覆盖版本段。billing meter 面**没有**桌面 UA 拦截器（2026-10-28
    抓包证伪了 `restOperations` 全局 `WorkbuddyUserAgentHttpInterceptor` 的推断），
    UA 是 axios 库默认串，冻结在 profile 的 `emit.billingMeterUA`（`source=traffic`，
    缺失回落 `desktopUA`）；`/v2/report` 用三段式 `desktopUA`。profile 的
    `emit.billingUA` 字段已无消费方，仅为旧 profile 文件保留（不要按它取 UA）。
  - chat UA 模板（`cli/dist/codebuddy-lite-wb.mjs` 的
    `UserAgentHttpInterceptor.buildUserAgent`）：
    `WorkBuddy/<platformVersion> <applicationName>/<platformVersion> CLI/<cliVersion>`。
    三段由 `ClientInfoProvider.initClientInfo` 拼装（CN 5.11.3 核对）：
    `platform: t?.platform || k0.Vo`、`platformVersion: t?.productVersion || k2.version`、
    `applicationName: t?.applicationName || ""`，其中 `t` 是 product 配置、`i` 是
    进程环境变量，且 `...i` 最后展开——**env 覆盖 product.json**。
    sidecar（`main/client-info-env.js` 的 `getCachedBaseEnv`）注入 `CLIENT_INFO_PLATFORM=WorkBuddy`、
    `CLIENT_INFO_PLATFORM_VERSION=<appVersion>`、`CLIENT_INFO_USER_AGENT_EXTENSION=CLI/<cli>`
    （`resolveWorkbuddyCliUserAgent`），所以第一段是 `WorkBuddy`，**不是** `cli/product.json`
    里 `platform:"CLI"` 那个值（该值只是 CLI 自身配置的默认，被 env 顶掉）。
    `applicationName` 不来自 env，取 `cli/product.json`（CN `WorkBuddy` / global
    `workbuddy-ai`）。`/v2/report` 的 desktop UA 是另一条 surface（`main/server.js`
    `buildVlmGatewayHeaders` 直接拼 `` [`WorkBuddy/${n}`,`WorkBuddy/${n}`,...`CLI/${r}`] ``），
    不共用该模板。
  - `X-Device-Token` 的注入面（真机 2026-10-28 抓包）：chat
    （`chatSurfaceOverrides` 的 `surfaceChat` 分支）与签到（`checkinHeaders()`，
    daily-checkin / checkin-activity-status）；官方另有 `/v2/plugin/auth/state`
    （面板登录面，不在本仓出站面）。其余面（summary / packages / report / busy /
    v3-config / refresh / growth / agenttool）都不带。文件兜底见
    `internal/upstream/device_token.go`。
    同一注入点**不发** `X-Device-Token-Error`：该头只在 Turing SDK 取不到 token 时
    出现，取值是 SDK 原生错误串（截断到 64 字符 ASCII），本仓没有同源错误域，
    伪造取值等于自造指纹；结论见 `internal/upstream/UPSTREAM.md` §六。
  - 会话头族 `X-Conversation-Id` / `-Request-Id` / `-Message-Id` /
    `X-Request-ID` / `X-Root-Request-Id`（`ChatMeta`）。
  - body 侧 `conversationRequestId`：官方桌面端在当前轮最后一条 user 消息的
    providerData 写 `conversationRequestId` / `startsNewUserRequest`，值与
    `X-Conversation-Request-ID` 逐字符一致；历史轮次客户端不提供该字段。
    证据：`cli/dist/codebuddy-lite-wb.mjs` 的 `convertUserMessage` /
    `providerData`；真机抓包见参考实现 `misakano7545/workbuddy2api-panel`
    的 issue #51（2026-10-22）。
  - 身份头 `X-Auth-Method` / `X-Userinfo`：客户端 `runIdentityHeaders()` 只要
    `auth.method` 非空就会发；但参考实现 issue #51 的 CN/global 个人号真机抓包
    （2026-10-22）两版均未出现这两个头，企业号才出现。个人号判据按
    `EnterpriseID`，不要按 `token_source`。
  - `Accept-Language`：官方 chat 路径不发；只有 billing 的 summary / packages
    分页带短码 `zh` / `en`（`resolveBillingAcceptLanguage`）。本仓 `CommonHeaders`
    与 `BillingHeaders` 都不发，只在 summary / packages 注入。
  - chat 桌面头组：`setDesktopExpertHeaders()`（`X-Domain` / `X-Product=SaaS` /
    `X-User-Id` / `X-Agent-Intent|Purpose|Type` /
    `X-Private-Data`（**固定 `true`**：官方桌面主进程 `applyGatewayHeaders` 兜底表
    与 `buildVlmGatewayHeaders` 都硬编码 `true`，CN 5.11.3 / global 5.9.4 同形；
    CLI chat 的 `privateDataHeaderValue()` 是另一条 surface：个人号 `false`、
    企业号（含 `type=ultimate|exclusive` 无 enterpriseId）`true`，CN / global 同源
    同形，判据只有 `isEnterpriseEditionAccount`、与域无关）/ `X-Ide-Name|Type|Version` /
    `X-Codebuddy-Request` / traceparent + b3 单头 + `X-B3-*` 四件套）。
  - `X-Agent-Runtime`：5.9.3 起官方在 chat / images / search / webfetch 全发
    （CN 5.11.3 仍单字面量 `AGENT_RUNTIME="X-Agent-Runtime"`，枚举
    `local-agent|cloud-agent|agent-agent` 未变）；
    chat 面发 `local-agent`（真机恒为该值，`other` 只是取不到会话 runtime 时的
    not-in-enum 兜底），agenttool 四路无真机证据故维持兜底值 `other`
    （见 `chatFamilyHeaders` / `chatSurfaceOverrides`）。`X-Wb-Uid` 只在
    `surfaceWebFetch` 发，取值与 `X-User-Id` 同值（官方 `fetchContentViaApi` 里与
    `X-User-Id` 相邻赋值，`X-Department-Info` 同组；CN 5.11.3 仍只有这一处）。
- 客户端侧对照：
  - `<app-asar>/main/*.js`：官方 UA、`X-IDE-*`、`X-Agent-*`、`X-Wb-Uid`、
    `X-Device-Token`。
  - `<app-asar>/renderer/assets/esm-*.js`：渲染层 `X-AI-Scenario|Channel|Model|
    Thinking|Online|SPACE` 族（仅 5.7.6 global 包见字面量；判断是否真出网）。
  - `<app-asar>/node_modules/openai/internal/detect-platform.js`：
    `X-Stainless-*` 族字面量（OpenAI SDK 自带，非上游自定义头）。

## 任务 / 活动域

- `internal/upstream/tasks.go`：`/v2/activity/growth/tasks{,-list,accept,
  reward/claim}`，chat 域与 webBase 域两种 claim 路径与 404 事实。
- `internal/upstream/streak.go`：`/activity/growth/{streak,lottery/summary,
  lottery/draw,redeem}`。
- `internal/upstream/blackcat.go`：`/activity/growth/{heatmap,makeup-cards/use}`。
- `internal/scheduler/travel*.go`：`/activity/growth/buddy/travel/*`。
- 客户端对照：`rg -o '/v2/activity/[a-z/-]*' <app-asar>`；billing/meter 族
  已覆盖 `checkin-activity-status`（`CheckinActivityStatus`）、
  `get-user-resource-summary`（`fetchResourceSummary`）、
  `get-user-resource-{paid,free}-packages`（`fetchPackagePage`）；
  `checkin-status`（无 `-activity`）已定性为 IDE/CLI 面：它只出现在
  `BackendProvider.getCheckinStatus`（CN 5.11.3 `main/account.js` /
  `renderer/assets/common-*.js`）与渲染层 IPC，桌面 daemon 自己走
  `${endpoint}/v2/billing/meter/checkin-activity-status`
  （`main/authority-snapshot.js`，本仓 `CheckinActivityStatus` 已覆盖）。
  本仓无调用方，**刻意不实现**，不再当首查项；理由见
  `internal/upstream/UPSTREAM.md` §六「checkin 面有两个端点」那条。

## 已知未覆盖 / 形态存疑入口

写报告时逐项给结论（覆盖 / 不需要 / 待落地）：

- `/v2/plugin/device/auth/code`（浏览器一次性登录码，登录态流程，与
  `internal/auth` 的 refresh 流不同源）。
- `/v2/chat/{queue/status,queue/cancel,prediction}`：CLI 排队形态，
  本仓库 chat 单轮请求未涉及。
- `/v2/chat/busy`：渲染层模型繁忙度探测（`renderer/assets/contexts-*.js`），
  单次最多 50 个模型，levels 为 `idle|normal|busy|congested|unavailable|unknown`；
  不是 CLI 排队。本仓已覆盖出站解析（`internal/upstream/modelbusy.go`）与入站只读
  预检（`internal/server/busy.go`）；不要让它参与选号。**global 域上游未部署这条
  路径（一律 404；见 issue #401 的 live 证据）**，出站侧按域记住「无此观测」后
  不再重打。
- `/v2/images/generations`、`/v2/images/edits`：官方 CLI 出图 / 图生图，
  CN 5.8.3 / global 5.8.4 / 两域 5.9.3 / cn 5.11.3 同源；**两者均已覆盖**（`internal/upstream/images.go`
  的 `GenerateImage` / `EditImage`，入站 `features.images_endpoint` 门控）。
  `edits` 是 JSON 形态（`Content-Type: application/json`，非 multipart），
  body 为 `model` / `prompt` / `size` + 可选 `image`（data URL / http URL / 裸
  base64，单个字符串或数组）+ 可选 `input_fidelity` 透传；见 UPSTREAM.md §六。
- `/agenttool/v1/search`、`/agenttool/v1/webfetch`、`/agenttool/v1/agentic_search`：
  官方联网搜索 / 抓取 / 综述式检索；本仓库均已覆盖
  （`internal/upstream/{search,webfetch,agentic_search}.go`）。
- `/v2/user-asset/*`：appearance/set 已覆盖，skill/assets 族未覆盖。
- `/v2/service-proxy`、`/v3/group/*`、`/v2/spans`：客户端内部域，非网关目标。

### cn 5.11.3 相对 cn 5.9.3 的面漂移（2026-11-15 静态核对）

粗扫口径（`protocol_sweep.mjs`）：端点 220 → 234（+21 / −7），头名 144 → 151
（+8 / −1）。global 5.9.3 → 5.9.4 差异集合为空。

新增端点（均无网关消费方，判 `skip`，不要为实现而实现）：

- `/v2/chat/decisions`（CLI `judge()`，30s 超时，`questions` / `safety_identifier`）。
- `/v2/geoblock`（`main/account.js`，配 `api.ipify.org` 做 IP 探测）。
- `/v2/security-log-report/v1/logs`（`main/server.js`，OTLP 安全审计上报，
  scope `workbuddy.security-audit`）。
- `/v2/user-asset/skill/get-list`（`main/skills-repo.js`，同面发
  `X-Client-Platform` / `X-WB-UA`）。
- `/v1/{relay-key,relay-key/consume,tunnels,web-bindings,shared,self/provision,me,
  authorize,$}`（CLI 本地隧道 / 中继密钥）。
- `/v1/customer/buddyone/*`、`/console/as/genie-baas/user/quota/check`、
  `/v2/as/conversations`、`/v2/service-proxy`。

消失端点：`/auth/{logout,refresh,status}`（职责已并入
`/v2/plugin/auth/{state,token/refresh,logout}`，本仓消费的正是后者，不受影响）、
`/console/agent-gateway/ima`、`/console/as/iom/getMemberPolicy`、
`/v2/agent-gateway/ima-mcp-oa`、`/v2/as/genie-baas`（IMA / 企业管理面）。

新增头族：`X-Client-Version` / `X-Client-Platform` / `X-Client-Product` 由
`main/client-identity.js` 的 `buildClientIdentityHeaders` 作为 outbound header
provider 注入（product 缺省 `workbuddy-desktop`，platform 归一为
`macos|windows|linux`），CLI bundle 只有常量声明，chat 出站不设；
`X-WB-UA` / `X-Client-Platform` 只出现在 skills-repo（connector-market）面；
`X-No-Tenant-Id` / `X-CodeBuddy-Session-Id` 分别是认证拦截器控制头与本地 mod
服务入站头，均不出网；`X-Qcloud-Env-Id` 只在泳道覆盖
（`getProductLaneOverride().resolve()`）命中时注入；`X-RateLimit-Dimension` /
`X-RateLimit-Layer` 是应答侧。消失 `X-Proxy-Error`（无消费方）。

cn 侧新增 `productFeaturesConfig.NetworkFailover`（global 无此段）：`region:
cn-saas`、`httpDns` 相关 IP、`routes["core-api"].fallbackAddresses.addresses =
["162.14.3.241"]`、熔断参数（`failureWindowMs` 等）。这是客户端 HTTPDNS / 容灾
能力，网关不模拟；只在解释「cn 客户端为何会直连 fallback IP」时引用。

## 客户端登录态落盘（auth session）

写「用网关凭证登录客户端」或排查登录态时的事实锚点（Win32 CN 5.11.3 / global 5.9.4 核对）：

- 路径：`<sharedAuthDirectory>/<authentication.id>.info`。
  `getWorkbuddyAuthFilePath()` = `path.join(getSharedAuthDirectory(),`${id}.info`)`，
  `getSharedAuthDirectory()` 按平台硬编码：win32
  `%USERPROFILE%\AppData\Local\CodeBuddyExtension\Data\Public\auth`（darwin 同名
  目录在 `Library/Application Support/` 下）。`authentication.id` 取
  `cli/product.json`，**两 realm 不同**：CN `workbuddy-desktop`、
  global `workbuddy-desktop-ai`（见 REFERENCE.md 的形态表）。wine prefix 里实测落在
  `drive_c/users/<u>/AppData/Local/CodeBuddyExtension/Data/Public/auth/`。
- 顶层结构：`{auth, account, accounts[], allAccounts[]}`；
  `auth` = `accessToken` / `refreshToken` / `tokenType` / `expiresAt`(ms) /
  `refreshExpiresAt`(ms) / `lastRefreshTime`(ms) / `expiresIn` / `refreshExpiresIn`
  / `sessionState` / `scope` / `notBeforePolicy` / `domain`；
  `accounts`/`allAccounts` 是账号切换快照，**刷新时为空会直接报
  `Failed to refresh session: account list is empty`**（`main/common2.js`）。
- 敏感字段（`auth.accessToken`、`auth.refreshToken`、`account|allAccounts[].nickname`、
  `phoneNumber`、`departmentFullName`，见 `main/credential-protection.js` 的字段表）
  在磁盘上是 `$wbEncrypted` 信封（AES-256-GCM，`suite=1`，AAD 只含
  framing/keyId/suite，见 `buildAuthenticatedContextAad`；本仓读取实现
  `internal/desktopcred/`）。但 `decodeString` 对普通字符串**原样放行**，所以明文
  注入可用，客户端读到后会走 migration 重新 seal。
- 账号元数据来源：`GET {endpoint}/v2/plugin/accounts`（`main/common2.js:getAccountSnapshot`
  拼 `${prefixPath}=/plugin`），返回对象字段与 `account` 一一对应；带
  `Authorization: Bearer` + `X-User-Id`，坏 token 直接 401。
- 登录态刷新：`/v2/plugin/auth/token/refresh`（`X-Refresh-Token`），与网关同源；
  文件目录有 watcher，外部改写会热加载（`main/file-authentication-storage.js`），
  登出靠 `<name>.info.logged-out` 标记。

无桌面包证的端点（`get-user-request-usage`、`/billing/ide/trial`、
`/billing/area/get-country-code` 等，契约来自第三方脚本 / 实测 / 参考实现）集中列在
`internal/upstream/UPSTREAM.md` §六「无桌面包证端点」表，写报告时按那张表取来源口径，
不要当成已对齐官方客户端。
