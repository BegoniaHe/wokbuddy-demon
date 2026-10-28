# 上游跟进记录（Upstream Tracking）

本文件记录本仓库（`BegoniaHe/wokbuddy-demon`）的**上游溯源**与**跟进状态**，
供后续维护者判断「哪些来自上游、哪些是本分支独有」，以及上游是否还有可跟进的新提交。

> 维护约定：每次做上游相关的重大决策（换基线、合并上游提交、上游删库/改名）时更新本文件。

## 一、溯源链（Lineage）

| 层级 | 仓库 | 状态与说明 |
| --- | --- | --- |
| 原始上游（original upstream） | `Sliverkiss/workbuddy2api` | 项目起点。**已删库**，公开 git 历史不可再获取 |
| 前序 fork（previous fork） | `linguo2625469/workbuddy2api-panel` | 在原始上游基础上重构可视化运维层（Web 面板 + 任务体系）；**已停止维护** |
| 参考 fork（reference fork） | `misakano7545/workbuddy2api-panel` | 当前提交级审计对象；只作线索，不直接吸收 |
| 本仓库（this repo） | `BegoniaHe/wokbuddy-demon` | 2026-10-16 **全新 git 历史重建**，独立演进 |

## 二、同步基线（Sync Baseline）

- **原始上游代码同步点**：`ea8b1e5` —— `Sliverkiss/workbuddy2api` 删库前的最后一次更新。
  本仓库（经前序 fork）的账号池调度、错误分类、提示词体系等核心设计均源自该版本。
- **前序 fork 同步点**：`linguo2625469/workbuddy2api-panel` 的 `main` 末次提交
  （`Merge PR #152`，`fix(panel): 积分到期提醒卡片默认可见`）；该 fork 已停止维护。
- **参考 fork 审计基线**：`misakano7545/workbuddy2api-panel` `main` @
  `33ce2f15025c59e38b71da5b92cde403a2ca35d1`（2026-10-26）。该基线与本仓库无共同祖先，
  是逻辑审计游标，不是本仓库祖先。
- **本仓库重建方式**：**丢弃全部 git 历史**，以当前工作区快照做初始提交。
  重建前的完整提交历史，请回溯前序 fork 仓库。

## 三、上游当前状态（Upstream Status）

| 上游 | 状态 | 跟进影响 |
| --- | --- | --- |
| `Sliverkiss/workbuddy2api` | 已删除（404） | 无新提交可跟进；原作者保留随时止损的权利 |
| `linguo2625469/workbuddy2api-panel` | **已宣告停止维护**（head `73a7560` 附「停止维护声明」，仍可 fetch） | 仅作溯源；不跟踪新提交 |
| `misakano7545/workbuddy2api-panel` | 活跃参考 fork | 当前审计对象；只做提交级审计，不直接吸收 |

**结论**：本仓库不再跟踪前序 fork 的停维护声明，也不直接吸收参考 fork 的代码。
`sync-upstream` 的默认流程是：**对照最新官方 Win32 客户端深度审计 → 确认缺口 →
在 `BegoniaHe/wokbuddy-demon` 开 Issue**。2026-10-25 已审计参考 fork 最近窗口
（2026-10-23..25，30 条非 merge 提交），确认 4 个缺口并开 Issue #282–#285。
2026-10-26 续审 `4662949..33ce2f1`（19 条非 merge + 5 条 merge；官方 Win32
包版本未变），确认 5 个缺口并开 Issue #293–#297（redisstore `LoadBinds` N+1、
credit CLI refresh/坏文件、登录轮询可观测性、池状态迁移日志、弱 `api_key`
启动告警）；其余按 `skip` / `revisit` 落账。参考 fork 与 linguo 分叉后另有约 370
条非 merge 提交，多数已由本仓库独立演进覆盖或属运行时/面板扩展，不纳入 pending；
后续审计从 `33ce2f1` 之后开始。

## 四、本分支相对基线的演进（节选）

- **可视化运维层**：内嵌 Web 管理面板（账号运维 / 用量与积分 / 模型档位 / 在线改配置 / 运行日志 / 任务中心）。
- **任务体系**：成长任务一键完成、任务中心全账号扫描与执行队列（含小程序口径任务）。
- **工程治理**（2026-10-16 重建时引入）：module path 改为 `github.com/BegoniaHe/wokbuddy-demon`、
  依赖升级至最新、`golangci-lint` v2 严格规则集（见 `.golangci.yml`）、`gofumpt` + `gci` 统一格式。

## 五、跟进策略（Maintenance Policy）

1. **官方客户端是协议权威**：参考 fork 的提交只是线索；先对照最新 WorkBuddy Win32
   包确认行为，再判断本仓库是否有缺口。包内无证据的结论按 `revisit`，不当事实。
2. **只审计、不吸收**：不做 `git merge` / `rebase` / `cherry-pick`，不改实现代码；
   确认缺口后只开 Issue。实现走独立的 issue-driven PR，由人类维护者评审。
3. **接口变更自行适配**：官方行为（CodeBuddy / WorkBuddy 接口）变更时，由本仓库在
   `internal/upstream/` 内自行适配，不依赖参考 fork 修复。
4. **溯源完整性**：本文件与 `LICENSE` 共同保留原始作者与各 fork 的署名；每次审计的
   逐提交处置记录于 `upstream-decisions.jsonl`（决策账本），游标见 `upstream-sync.yaml`。
   新处置使用 `issue` / `skip` / `revisit`；历史 `adapt` / `cherry-pick` 记录保留可读。

## 六、官方客户端协议基线

本仓库的协议对齐以官方 WorkBuddy Desktop **Win32** 静态包（`win32-x64-user`）为唯一
事实来源（不运行官方客户端、不请求上游）。macOS 构建只作辅助对照，不参与出站指纹：

| realm | 平台 | 客户端版本 | CLI | commit | releaseDate |
| --- | --- | --- | --- | --- | --- |
| cn | win32-x64-user | 取自部署 profile | 取自部署 profile | 取自部署 profile | 取自部署 profile |
| global | win32-x64-user | 取自部署 profile | 取自部署 profile | 取自部署 profile | 取自部署 profile |

运行时从 `data/client-profiles/<realm>.json` 读取部署快照并整份覆盖对应 realm；
cn 恒需、global 仅在 `global.enabled` 时需要，文件缺失或校验失败都拒绝启动。
仓库源码不保留具体版本 / commit / releaseDate，避免把某一版官方指纹固定成内置回退。

采用的桌面端 / CLI 行为：

- CN origin 主域 `www.workbuddy.cn`，首 token 前可回落 `copilot.tencent.com` / `wb.tencentbuddy.com`。
- 企业账号 chat 同时发 `X-Enterprise-Id` / `X-Tenant-Id`；个人 global 发 `X-No-Enterprise-Id: 1`。
- 官方 CLI chat 拦截器与 daemon 网关都不发 `Origin` / `Referer` / `X-Machine-Id` / `X-Session-Id`；本仓库的 `CommonHeaders` 同样不注入，只有 web 端点按自身契约构造 `Origin` / `Referer`。
- chat UA 由 CLI 层 `cli/dist/codebuddy-lite-wb.mjs` 的 `UserAgentHttpInterceptor.buildUserAgent` 构造；第二段是 `cli/product.json` 的 `applicationName`（CN `WorkBuddy` / global `workbuddy-ai`），显式 `client_version` / `cli_version` 覆盖时同样按 realm 取品牌；`/v2/report` 的 UA 是 desktop surface，两者不可混用。
- 401 同号 refresh + replay；refresh 401/403 映射会话失效；同账号/同 RT refresh 单飞（60s 按需复用）。
- 出站 `X-Conversation-ID` / `acp-connection-id` 恒发：客户端没给会话 ID 时生成 UUIDv4（不影响 prompt cache key）。
  `acp-connection-id` 是会话级**带连字符 UUIDv7**（真机主/辅助请求同值、version=7、时间戳前缀可解），
  按 conversationId 稳定派生、前缀取进程启动时刻前 1 小时内的确定性偏移；主请求的 `X-Conversation-ID`
  真机实测是 v4（辅助请求才是 v7），故该头兜底**保持 v4**（#372）。
- 出站头族按 **surface** 集中构造（`internal/upstream/headers.go` 的 `applyHeaders`）：chat 与 agenttool 四路复用 chat 头族，billing / report / refresh / v3-config / growth 走轻头族 base（Accept / Content-Type / UA / Authorization / X-User-Id / X-Domain / X-Request-ID）再按面覆盖。surface 差异只在该文件表达，调用点不得各自 Set / Del。
- `upstream.user_agent` 是**全部出站**的显式逃生门：各面自带默认（chat 三段式 / billing meter 抓包串 / report 三段式 / growth `node`）都让位给该值；`client_name="SaaS"` 仍是 billing meter 面的「不写 UA 键」逃生门（report 面不受它影响）。
- 首条消息非 system 时补官方 `"You are CodeBuddy Code.\n"` fallback（CN/global 都补）。
- `6004` 带重置、`14003` 模型级限流只冷该模型；`14003` 用 10s→1m 短窗且不喂路由熔断；`6004` 无重置按账号级保守退避并记模型审计；`11158` 仅在 400/404 时与 `11136` 同归账号级模型权限避让，429+11158 归限流。
- billing/meter 的 summary / packages / get-user-resource 无前缀优先、`/v2` 回落（官方桌面 `resourcePrefix` 为空串）；签到例外保持 `/v2` 优先（官方 `DesktopAccountRepo.billingPrefix` 覆写为 `/v2`）。packages 按**桌面 daemon 用量链**取形：`UsageService.getPersonalUsage`（CN 5.9.3 `main/authority-snapshot.js@469554`）复用 `main/account.js` 的 `fetchUserResources` / `fetchAllPages`（`@378532`，`pageSize:200` 且以「不足 200 条」终止）与 `fetchResourcePackagesPage`（`@468153`），body 为 `{PageNumber, PageSize, PackageCodes, Status:[0,3], ...extra}`，paid 的 extra 是 `NeedRenewInfo:true`、free 的 extra 是 `buildSlicePeriodRange()`（`@378135`，导出名 `P`）的当日 `SlicePeriodStartTime`/`SlicePeriodEndTime` 窗口。`OnlyValidPeriod` / 无 slice 窗口的形态属渲染层 `CloudAccountRepo.validCreditPackages`（`@418501`，面板用量页用）那条 surface，本仓不采用。billing 的 `buildHeaders` 不带 UA，且**没有**桌面 UA 拦截器：2026-10-28 抓包证伪了「RestOperations 全局 `WorkbuddyUserAgentHttpInterceptor`」的推断，meter 面的 UA 就是 axios 库默认串，冻结在 profile 的 `emit.billingMeterUA`（`source=traffic`；字段缺失时回落 `desktopUA`，需重抓 profile 后才逐字生效）。`Accept-Language` / `X-Device-Token` 默认不发：summary 与 packages 由调用方带 `Accept-Language: zh|en`（短码，不是 `zh-CN`/`en-US`），`/v2/report` 的 UA 走三段式 `desktopUA`（显式 `user_agent` 优先），`X-Device-Token` 只在签到注入（`/v2/plugin/auth/state` 是面板登录面，不经 `internal/upstream`）。分 surface 取 base：桌面 report 走 `desktopBase`（即 `chatBase`），Web report 走 `webBase`，growth 任务 / 旅行走 `chatBase`，billing / 签到 / 活跃上报走 `billingBase`。本仓 CN billing 默认改为 `workbuddy.cn`（官方 5.9.3 桌面 billing / 签到 / 活跃上报全部打产品域，2026-10-28 真机 14 条业务请求 0 条打 `codebuddy.cn`）；显式配 `cn.billing_base=https://www.codebuddy.cn` 可退回旧口径。小程序埋点固定 `codebuddy.cn`，不跟随覆盖。这是 surface 取舍，不是遗漏。
- checkin 面有两个端点，本仓只做桌面那条：`/billing/meter/checkin-activity-status` 由官方桌面 daemon 走 `${endpoint}/v2/billing/meter/checkin-activity-status`（CN 5.9.3 `main/authority-snapshot.js@463251`，body `{}` + `getTuringDeviceTokenResult` 的 token 头），本仓 `CheckinActivityStatus` 覆盖的正是这条，也是调度器唯一调用方；`/billing/meter/checkin-status`（无 `-activity`）只出现在 IDE/CLI 侧 `BackendProvider.getCheckinStatus`（CN 5.9.3 `main/account.js@399217`、`renderer/assets/common-Cklb-JUT.js@443241`）与渲染层 IPC 面，本仓没有调用方，**刻意不实现**（不加推测性接口）。
- `X-Device-Token-Error` 只在 Turing SDK 取不到 token 时发，取值是 SDK 原生错误串（CN 5.9.3 `main/turing-sdk.js@674` 的 `unsupported platform <platform>`、`@817` 的 `native/turing-sdk is missing from the application bundle`），发出前经 `sanitizeTuringHeaderValue` 截断到 64 字符 ASCII（`main/authority-snapshot.js@452286`；取用点 `getTuringDeviceTokenResult@454671`、`buildHeadersWithTuringToken@466339`（`checkin-activity-status` 的调用点 `@463317`））。本仓 device token 走 `upstream.device_token_file` / 每号 auth 文件的 `device_token` 兜底（`resolveDeviceToken`），没有同源错误域，因此**不发该头**——伪造取值等于自造指纹。
- `/v2/report`（活跃上报）面 UA 用三段式 `desktopUA`、`Content-Type: application/json;charset=UTF-8`、Accept 宽松值（真机同形）；单段 `billingReportUA` 的旧口径已删除（#367）。
- **刻意偏离：billing 面 `Accept-Encoding`**。真机是 `gzip, compress, deflate, br`；本仓不显式设置，交给 Go transport（发 `gzip` 并透明解压响应）。手工 Set 该头会关掉 `Transport.DisableCompression=false` 的自动解压，SSE 与 JSON 响应都得自己解，代价远大于收益，故不跟。
- chat 出站 body **无条件 gzip**（`Content-Encoding: gzip`，`Content-Type: application/json` 不带 charset）：官方 `productFeatures.RequestBodyGzip` CN 为 true，包内 `GzipRequestProcessor` 没有字节阈值（真机主请求 45KB、辅助请求 1.2KB 都压）。唯一跳过条件是显式自定义 base——本仓对齐为「`cn.chat_base` / `global.chat_base` 被显式覆盖时不压」。不手设 `Accept-Encoding`（会关掉 Go 透明解压，SSE 直接坏），`Content-Length` 由 `bytes.Reader` 交给 `http.NewRequest` 算压缩后长度（#365）。
- SSE 接受 `delta.reasoning`、重复 tool id 去重、`finish_reason` 后 EOF 正常收尾；reasoning-only 不记成功。
- 签到两 realm `/v2` 优先；余额 summary 为主、旧端点兜底；模型目录应用 `availableModels` / `disabled` / `aliases`（企业 `/console/enterprises/<enterpriseId|personal>/models` 按账号拼接，`/v2/enterprises/personal/models` 回落仍固定 personal）。`/v1/models` 发现输出再按官方默认会话 agent（`/v3/config data.agents`，企业端点 `agents` 兜底；`tags` 含 default → `name=cli` → 首个有 models）裁剪，仅隐藏展示条目，`FetchModels`、面板与请求路由保持全量。
- growth `/v2` 优先；travel claim 空对象 `{}` 首选（官方桌面固定 `post(path, {}, productHeaders)`，是空 JSON 对象不是零字节 body），`{record_id}` 仅作 400 兜底（record_id 为 0 时不兜底，调度器也不因缺它跳过领奖）；桌面 report `X-Domain` 为 host 形态，body 指纹带 `userAgent=Node.js/22`（官方 Electron 主进程 `navigator.userAgent`）与 `os=Win32`（官方 `navigator.platform`），与 HTTP 头 UA 不同源。
- images 两个端点都已覆盖：`/v2/images/generations`（`GenerateImage`）与 `/v2/images/edits`（`EditImage`），均受入站 `features.images_endpoint` 门控、单次尝试不换号。edits 是 **JSON 形态**（官方 `ImageService.editImage`：`Content-Type: application/json`，**不是 multipart**），body 为 `model` / `prompt` / `size`（默认 1024x1024）+ 可选 `image`（data URL / http URL / 裸 base64，单个字符串或字符串数组）+ 可选 `input_fidelity` 原样透传；头族与 generations 相同（`ImagesHeaders`）。CN 5.9.3 `cli/dist/codebuddy-lite-wb.mjs@6220394`。
- 队列业务码 `6020/6021/6022`、限流族 `6000-6008`、额度/认证码按客户端枚举实现；请求级码（`11125/11129/.../11155`、`12654/14405`）不罚号不轮转，`11136` 换号并做模型级避让，`11141` 模型下线 fail-fast，`11102` 模型目录不存在按请求级 404 fail-fast（不换号），`11-128` 入站按内容拦截。
- SSE error 帧与 HTTP 200 业务信封优先按结构化 `code`（含 `bizCode`）分类，取不到再回落 message；客户端 `data.statusCode`/`actions`、`cgicode/ret/retcode` 与 `172xx` 技能码尚未进入聊天主路径验证，暂不实现。

非 chat 端点的头族取舍（刻意统一指纹，不是遗漏）：

| surface | 官方形态 | 本仓 | 取舍 |
| --- | --- | --- | --- |
| search / webfetch | raw `fetch`，`CLI/<desktop version>` UA，头更少 | 复用 chat 头族 | 统一指纹，避免同账号多客户端 |
| webfetch 的 `X-Wb-Uid` | 与 `X-User-Id` 同值（账号 uid） | `WebFetch` 单独注入 | 官方只在 webfetch / knowledge_search 发，不扩散到 search / agentic_search / images |
| agentic_search | `Content-Type` / `Authorization` / 可选 `X-Conversation-Request-ID` | 复用 chat 头族 + `Accept: text/event-stream` | 同上；SSE 契约优先 |
| images（generations + edits） | Content-Type / Authorization / conversation / identity | 复用 chat 头族 | 同上；两个端点同族同策略 |
| growth（`/v2/activity/growth/*`） | Node 原生 `fetch`：`user-agent: node`（显式 `upstream.user_agent` 仍优先，见下）、`accept-language: *`、`sec-fetch-mode: cors`、`accept-encoding: br, gzip, deflate`，无 `X-Request-ID` / `X-Product` / `X-Requested-With` | 独立轻头族（`surfaceGrowth`，`GrowthHeaders`） | 2026-10-28 真机 `buddy/info` 逐字比对（#375）。实测只命中 `buddy/info` 一条；`travel/status\|depart\|claim`、`streak`、`tasks`、`heatmap`、`lottery`、`makeup-cards` 同仓库同域同为 `productHeaders` 来源，按**同面假设**复用，尚无逐路径抓包 |
| chat/busy | renderer `wb.http.post`，8 个轻头（`Accept` 宽松值 / `Content-Type` / 三段 UA / `X-Request-ID` / `X-Product: SaaS` / `Authorization` / `X-User-Id` / `X-Domain`） | 独立轻头族（`surfaceModelBusy`） | 2026-10-28 真机：该路径没有任何会话头 / `acp-connection-id` / `baggage` / b3 / `x-stainless-*` / `X-Codebuddy-Request`（#371）。企业账号的 `X-Enterprise-Id` / `X-Tenant-Id` 取舍：真机样本是个人号，无证据，按「不猜测」不发 |

证据来自部署 profile 与 `.tmp/workbuddy` 解包（2026-10-27）：CN 5.9.3 包内
`X-Agent-Runtime` 官方在 chat / images / search / webfetch 全发，枚举是
`local-agent` / `cloud-agent` / `agent-agent`，`other` 只是取不到会话 runtime 时的
not-in-enum 兜底值。2026-10-28 chat 真机全量请求恒为 `local-agent`（`other` 0 次），
故 chat 面发 `local-agent`；agenttool 四路**仅 chat 有真机证据**，维持兜底值 `other`，
抓到各自的真机头集后再扩（#373）。
同版 `X-Wb-Uid` 只出现在 webfetch（与 knowledge_search），取值与 `X-User-Id` 同源，
故只在 `surfaceWebFetch` 注入，不扩散到 search / agentic_search / images。
版本值仍只来自部署 profile，不进入仓库内置常量。

chat / refresh 头族差异（已对齐 / 刻意保留 / 不适用）：

| 项 | 状态 | 依据 |
| --- | --- | --- |
| refresh trace（`X-Trace-Id` / `Traceparent` / `B3` / `X-B3-*`） | 已对齐 | 官方 `cli/dist/codebuddy-lite-wb.mjs` 的 `AccountScopedAuth.refreshSession()` 追加 `traceSpan.requestHeaders`；网关无活动 span，故生成自洽的新链路 id |
| chat `X-Device-Token` | 已对齐（维护者已拍板：发） | 真机 chat 主请求与辅助请求**都带**该头，与产品面公共头同层出现（`X-Private-Data` / `X-Codebuddy-Request` / `X-IDE-*` / `X-Product` / `X-Domain` / `X-User-Id` / `baggage` / trace 族 / `x-stainless-*` 同层），不在 `buildConversationHeaders` 的会话头族里；`#342` 的「只在 auth/state 与签到消费」对 chat 不成立（#376）。Trade-off：该头是风控标识，跨 surface 复用会把 chat 与签到关联到同一设备——发 = 更像官方，不发 = 更少设备关联；本仓**发**。其余面（summary / packages / report / busy / v3-config / refresh / growth / agenttool）真机不带，维持不发 |
| chat `Accept` | 已对齐 | 真机 chat（含 `stream: true`）仍发 `application/json`，不改 `text/event-stream`（#377）。agenttool 四路真机未取证，search / webfetch 保持 `application/json`、agentic_search 保持 `text/event-stream`（SSE 契约优先），仅 chat 有真机证据 |
| chat trace id 取值 | 已对齐 | 真机 `X-Trace-ID` / `X-B3-TraceId` / `traceparent` 的 trace-id 是 32 hex **随机值**（第 13 位为 4，无 v7 时间戳前缀），三处同值且 ≠ `X-Conversation-Request-ID`；本仓改为独立生成（`session.NewTraceID`），入站透传 `X-Trace-ID` 仍优先（#377） |
| refresh 头集与 body | 已对齐 | 真机 11 头 + body `{}`（`Content-Length: 2`）：`Accept: application/json, text/plain, */*`（宽松值，不沿用轻头族默认）、`Authorization`（与 `X-Refresh-Token` **双发**，与 chat 相反）、`X-User-Id`、`X-Request-ID`（32 hex）、`X-Product: SaaS`、`X-Requested-With`、三段式 UA、`X-Domain`（endpoint authority）、`X-Auth-Refresh-Source: plugin`（#370） |
| 轻头族 `X-Domain` | 已对齐 | 官方 `enterpriseHeaders()` 无 domain 时回落 **endpoint authority**；轻头族各面（refresh / v3-config / billing / report / busy / growth）统一按本次请求 URL 的 host 取值，不回落 chat base——billing 面走 `billing_base`，取 chat host 会把别的域发给 billing。`cn.chat_base` / `cn.billing_base` 覆盖时各自跟随 |
| 非 chat 业务面的 `X-Request-ID` | 已对齐（refresh / v3-config / billing / busy / report） | 官方 HTTP 层 `applyRequestId` 对每条业务请求补 32 hex（UUIDv7 形态）；`/v2/activity/growth/*` 是 Node 裸 fetch 面，真机**不带**该头，故 growth 不在覆盖范围（#369 / #375） |
| `X-Stainless-*` | 不补 | 由 OpenAI Node SDK 自动生成；本仓出站不是 Node SDK，补等于伪造 SDK 运行时版本 |
| `X-Codebuddy-Request: 1` | 仅 chat 族（chat + agenttool 四路） | 2026-10-28 真机抓包：7 条 chat 全带；refresh / busy / report / v3-config / billing / growth 共 14 条业务请求 0 命中（#368）。agenttool 四路无真机证据，沿 chat 头族一起发 |
| 个人号 `X-No-Enterprise-Id` / `X-No-Department-Info` | 刻意保留 | 非 chat 端点也声明无部门信息（#298），按 chat-only 收窄会回退该修复 |
| W3C `baggage`（chat） | 已对齐（4 条） | 真机逐字 `buddyId=,buddyName=,codebuddy.session_id=<会话 id>,codebuddy.conversation_request_id=<请求 id>`；两个 buddy 键是官方无 buddy 上下文时的空值形态，两个 codebuddy 键与同请求 `X-Conversation-Id` / `X-Conversation-Request-Id` 同值。包内还有 `conversation_message_id` / `root_request_id` 两个键常量，但 chat 真机**没发**，多发反而更明显；`tracestate` 仅跨进程 span 携带时下发，网关不伪造（#364） |
| `X-Private-Data`（CLI chat） | 已对齐（按账号身份） | CN 5.9.3 的 `applyPrivateDataHeaderPolicy` 在个人号默认分支发 `false`、企业号（`isEnterpriseEditionAccount`：enterpriseId 非空或 type 为 ultimate/exclusive）发 `true`；global 基线仍是 5.8.4，走 `DisableYuanbaoChannel=true` 仍发 `true`，升到 5.9.x 后按同一逻辑复核。远端 abTest / 会话 meta `privateDataSticky` 不在网关范围 |
| `X-Private-Data`（桌面专家路径） | 已对齐（固定 `true`） | 官方桌面主进程 chat 走 `applyGatewayHeaders`，兜底表 `VF` 里该头硬编码 `true`；`buildVlmGatewayHeaders` 同样硬编码。与 CLI chat 分属两条 surface，不能共用阈值；CN 5.9.3 / global 5.8.4 同形 |
| custom model 的 `X-Private-Data` | 不适用 | 本仓没有 custom 模型上游路由（`isCustomModel` 仅用于模型目录可见性），无触达路径 |

无桌面包证端点（**实现依据 ≠ 已对齐官方客户端**；检索范围：截至 CN 5.9.3、CN 5.8.3、global 5.8.4 三包 `app-asar` 内 `*.js` / `*.mjs` 均为 0 命中，路径字面量不出现）：

| 路径 | 实现位置（符号为准，行号仅提示） | 契约来源 / 口径 |
| --- | --- | --- |
| `/billing/meter/get-user-request-usage` | `internal/upstream/requestusage.go:19-20`（#31） | 第三方 WorkDaddy 脚本 `credit-request-usage.js` + 实测 |
| `/billing/ide/trial` | `internal/upstream/trial.go:13`（`global_register.go` 亦有记录） | 实测 / 参考实现 |
| `/billing/area/get-country-code` | `internal/upstream/global_register.go:124` | 实测 / 参考实现 |
| `/billing/meter/claim-gift`、`claim-compensation`、`compensation-status` | `internal/upstream/blackcat.go:92,97`、`compensation.go:28` | 本文件末尾「桌面包内没有证据的任务体系（连登/抽奖/补签/礼包/补偿/校园活动）继续以 Web 契约为准」总口径已覆盖，不单独论证 |

读这张表时不要反推「官方客户端里有这个端点」：前三条的形态来自第三方脚本、参考实现或实测，`claim-*` / `compensation-status` 出自 Web 契约；四行在桌面 static 包内都没有任何字面量证据。要重新论证时按上表列为起点，而不是把它们当作已对齐桌面。

活跃上报 `/v2/report` 的 `chat_request_send` 事件按 CN 5.9.3 形状发（#366）：删掉
`maxToken` / `maxSteps` / `temperature` / `maxRetries` / `skillCount` / `totalCount` 与
`fileUri`；补 `isMaxMode`（网关无 thinking 覆盖状态 → `false`）、`source` / `inputAttemptId` /
`connectorId`（官方条件发，网关无状态则整键不发）、`vcsType` / `vcsRepo` / `vcsBranchName` /
`vcsRevId`（无工作区 → 空串）；`agentType` 发 `main`，`agentName` 取官方空值形态。
该事件的键集会否被上游校验未知，改动后建议做一次账号回归（点亮 growth 连登与
`first_buddy` 任务）。

桌面包内没有证据的任务体系（连登/抽奖/补签/礼包/补偿/校园活动）继续以 Web 契约为准，不随桌面端迁移。

## 七、参考 fork 审计工具（sync-upstream 技能）

- 技能位置：`.agents/skills/sync-upstream/`（OpenCode 经 `.opencode/opencode.json`
  的 `skills.paths` 加载）。
- 上游远端：`upstream` → `https://github.com/misakano7545/workbuddy2api-panel.git`
  （branch `main`）。首次使用执行 `git fetch --prune upstream`。
- 审计游标（逻辑基线，位于参考 fork 历史中，非本仓库祖先）：
  `33ce2f15025c59e38b71da5b92cde403a2ca35d1`（2026-10-26 建立）。
- 正式审计前先刷新官方 Win32 包并跑 `reverse-workbuddy` 的
  `download_latest.mjs` / `protocol_sweep.mjs`；官方静态包缺失时停止审计。
- 常用命令：

  ```bash
  git fetch --prune upstream
  python3 .agents/skills/sync-upstream/scripts/inspect_upstream.py
  python3 .agents/skills/sync-upstream/scripts/upstream_decisions.py validate
  python3 .agents/skills/sync-upstream/scripts/upstream_decisions.py list --path internal/upstream
  ```

---

_发布形态：滚动更新，无版本号；`appVersion`（`cmd/server/main.go`）为编译期注入的构建标识，非发布版本。_
_最近更新：2026-10-26（参考 fork 审计基线 `33ce2f1`；窗口 `4662949..33ce2f1` 开 Issue #293–#297；不直接吸收）。_
