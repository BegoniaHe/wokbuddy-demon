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
  来自部署 profile，源码无固定构建常量；body 的 `userAgent` 固定为
  `Node.js/22`（Electron 主进程 `navigator.userAgent`），与 HTTP 头桌面 UA
  不同源；`os` 为 `Win32`（官方 `navigator.platform`））。

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
    `platform` 由 sidecar 注入固定为 `WorkBuddy`；`applicationName` 取
    `cli/product.json`（CN `WorkBuddy` / global `workbuddy-ai`）；extension 为
    `CLI/<cli>`。服务端不注入 `CLIENT_INFO_APPLICATION_NAME`。`/v2/report` 的
    desktop UA 是另一条 surface，不共用该模板。
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
    `VF` 与 `buildVlmGatewayHeaders` 都硬编码 `true`，CN / global 5.9.3 同形；
    CLI chat 的 `privateDataHeaderValue()` 是另一条 surface：个人号 `false`、
    企业号（含 `type=ultimate|exclusive` 无 enterpriseId）`true`，CN / global 5.9.3
    同源同形，判据只有 `isEnterpriseEditionAccount`、与域无关）/ `X-Ide-Name|Type|Version` /
    `X-Codebuddy-Request` / traceparent + b3 单头 + `X-B3-*` 四件套）。
  - `X-Agent-Runtime`：5.9.3 起官方在 chat / images / search / webfetch 全发；
    chat 面发 `local-agent`（真机恒为该值，`other` 只是取不到会话 runtime 时的
    not-in-enum 兜底），agenttool 四路无真机证据故维持兜底值 `other`
    （见 `chatFamilyHeaders` / `chatSurfaceOverrides`）。`X-Wb-Uid` 只在
    `surfaceWebFetch` 发，取值与 `X-User-Id` 同值（官方仅 webfetch /
    knowledge_search 发）。
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
  `BackendProvider.getCheckinStatus`（CN 5.9.3 `main/account.js@399217` /
  `renderer/assets/common-Cklb-JUT.js@443241`）与渲染层 IPC，桌面 daemon 自己走
  `${endpoint}/v2/billing/meter/checkin-activity-status`
  （`main/authority-snapshot.js@463251`，本仓 `CheckinActivityStatus` 已覆盖）。
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
  CN 5.8.3 / global 5.8.4 / 两域 5.9.3 同源；**两者均已覆盖**（`internal/upstream/images.go`
  的 `GenerateImage` / `EditImage`，入站 `features.images_endpoint` 门控）。
  `edits` 是 JSON 形态（`Content-Type: application/json`，非 multipart），
  body 为 `model` / `prompt` / `size` + 可选 `image`（data URL / http URL / 裸
  base64，单个字符串或数组）+ 可选 `input_fidelity` 透传；见 UPSTREAM.md §六。
- `/agenttool/v1/search`、`/agenttool/v1/webfetch`、`/agenttool/v1/agentic_search`：
  官方联网搜索 / 抓取 / 综述式检索；本仓库均已覆盖
  （`internal/upstream/{search,webfetch,agentic_search}.go`）。
- `/v2/user-asset/*`：appearance/set 已覆盖，skill/assets 族未覆盖。
- `/v2/service-proxy`、`/v3/group/*`、`/v2/spans`：客户端内部域，非网关目标。

无桌面包证的端点（`get-user-request-usage`、`/billing/ide/trial`、
`/billing/area/get-country-code` 等，契约来自第三方脚本 / 实测 / 参考实现）集中列在
`internal/upstream/UPSTREAM.md` §六「无桌面包证端点」表，写报告时按那张表取来源口径，
不要当成已对齐官方客户端。
