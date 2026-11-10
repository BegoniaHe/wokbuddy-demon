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
  `592fb01af53dcaf25cc45ba3496730850b207830`（2026-11-10）。该基线与本仓库无共同祖先，
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
条非 merge 提交，多数已由本仓库独立演进覆盖或属运行时/面板扩展，不纳入 pending。
2026-11-01 续审 `5caddab..4d7d1e67`（31 条非 merge；官方 Win32 仍是 cn 5.9.3 /
global 5.9.4），确认 2 个缺口并开 Issue #432（续写 usage 把 prompt 按段数累加）、
#433（登录三端点漏发 `X-Product`）；头序、JA3、`X-No-*` 取值等只靠抓包的项记
`revisit`。2026-11-02 续审 `4d7d1e67..edcfdf80`（22 条非 merge + 2 条 merge；官方
Win32 仍是 cn 5.9.3 / global 5.9.4），新开 #438（`/v3/config` 缺 `Connection: close`）、
#439（积分保底不能按域覆盖）；别名通配与面板默认拒绝分别链到既有 #436、#437。
2026-11-03 续审 `edcfdf80..f6c4a451`（4 项内容；官方 Win32 未变），新开 #446（cockpit
导入丢 `enterprise_id` 且 importCheckin 无企业门控）、#447（effort 降级把 `off` 当可选档）。
2026-11-04 续审 `f6c4a451..77d4762f`（12 条内容 + 5 条 PR 合入；官方 Win32 未变），
新开 #454：流式读取在 `[DONE]` 处停读、关闭时先取消 context 且不读流尾，上游连接
无法回池复用；到期加权窗口默认值记 `revisit`（本仓权重模型与参考 fork 不同，需本仓
真实批次分布），其余按 `skip` 落账。
2026-11-04 再续审 `77d4762f..dd01a526`（1 条提交，三块内容；官方 Win32 未变），
全部记 `revisit` / 不适用：tools 子树脱敏的代码事实成立（`payload.go` 只洗 messages），
但上游是否会拦工具描述里的模板句无证据、出处自述未实测；轮询指纹跳过对应参考 fork 的
`innerHTML` 重建形态，本仓面板是 React 渲染；`AutoBot.yml` 属参考侧自动化。
2026-11-06 续审 `dd01a526..dab1b08c`（15 条非 merge + 9 条纯 merge；官方 Win32 未变），
整批是参考 fork 自建的一整套「视频生成」产品簇（出视频端点 / MCP / 水印裁切 / 任务台账 /
OpenAI Video 资源形状 / Responses 托管 `image_generation` 工具）。官方两 realm 静态包确认
`cli/dist/codebuddy-lite-wb.mjs` 的 `VideoService` 有 `/v2/videos/generations` +
`/v2/videos/tasks`，且按 `text-to-video` / `image-to-video` 标签给模型分类，本仓无视频端点、
`nonChatModel` 又把视频模型从 `/v1/models` 剔除，确认一个缺口并开 #467（已实现）；图片
`b64_json` / `n>1` 只靠参考 fork 真机实测、官方包无证据，记 `revisit`；9 个纯 merge 及本仓
已覆盖（`clientGone` 只看请求 ctx）、形态不存在（reqlog `purgeCh`、面板分域 tab、水印/台账）、
或属参考侧产品包装的提交一律 `skip`。该段审计记录最初落在 `fix/stream-close-drain-tail`
分支的 `4766f469`（未合入 `main`），2026-11-08 复核其账本前 264 行与 `main` 同源后并入本轮。
2026-11-08 续审 `dab1b08c..29db8786`（33 条非 merge + 6 条纯 merge，共 39 项；官方 Win32
仍是 cn 5.9.3 / global 5.9.4），确认一个协议缺口并开 #476：业务码 `11134` 官方文案为「模型
服务不可用，请切换模型重试」、CLI `classifyErrorDetail` 的账号级 quota/auth 子分类不含它，
本仓 `Classify` 却无分流 → 5xx 落 `ErrServer` 罚号、200 信封 / 4xx 落 `ErrClient` 换号并
Unbind 粘性；`retry_policy.retry_after` 字段与末端状态码无包内证据，列 #476 证据边界等实测。
`4fb7cb42` 的盲盒消费 + `10105` 自救（端点 0 命中、依赖第三方脚本实测；本仓 scheduler 无
网页活跃腿形态）记 `revisit`；其余（`$wbEncrypted` 本仓已由 `internal/desktopcred` 覆盖、
断流终态 `errTruncatedStream` 已覆盖、兜底 system 本仓用官方字节、入站体上限 / 11140 连坐
已覆盖、面板 / AutoBot / 续写预算 / `model_eff` 属参考侧产品）按 `skip` 落账，6 个纯 merge
按 `merge-bubble` skip。
2026-11-08 再续审 `29db8786..771c887f`（4 条非 merge + 0 条 merge，均 2026-11-08；官方
Win32 版本未变，仍是 cn 5.9.3 / global 5.9.4）。整批是参考 fork 网关内部工程、无官方协议
缺口，0 开单：`WB2API_PPROF` 独立 pprof 监听本仓已由带运维鉴权的 `/debug/pprof/*`
（`internal/server/handler.go:392-397`）覆盖，记 `skip`；sanitize 指纹预检去 RE2 折叠、
新增 `internal/jsonscan` 让请求体只解析一次两条纯 CPU 优化（线上数字来自参考 fork 自身部署、
官方静态包 0 证据）记 `revisit`，等本仓自身负载 profile；`<think:SUF>` 拆回 thinking 块是上游
vLLM parser 偶发 + 真机实测（官方 Win32 全树 `think:SUF` 0 命中），其 messages 流式 handleChunk
认整条 `message` 的 parity 与本仓形态不符（官方 `streamPaths=choices.delta.*` /
`responsePaths=choices.message.*`，本仓非流式认 `message`、流式 `normalizeChoice` 白名单只
重建 `delta`），整体记 `revisit`。

2026-11-10 续审 `771c887f..592fb01a`（20 条非 merge + 3 条纯 merge；官方 Win32 版本未变，
仍是 cn 5.9.3 / global 5.9.4；重跑 `download_latest --dry-run` / `download_latest` /
`protocol_sweep` 复核）。按新政策（后端上游功能尽量适配，商业化/面板技术栈/计费除外）本轮
共开 5 个功能簇 Issue，另有 1 个独立面板导入缺口：

- **#477**（面板导入）：`585ae7f2` / `cd884503` 的 cockpit 导入——本仓
  `internal/panel/import.go` 只接受扁平 `[]cockpitAccount`（无嵌套/驼峰/字典/信封归一，
  `.go:129-133`），`expires_at` 无条件 `/1000`（`.go:246`），落盘前不 `MkdirAll(AuthDir)`
  （`.go:270-287`）；启动期 `LoadDir` 用 `filepath.Glob` 不建目录、`SaveAtomic`→`CreateTemp`
  父目录缺失即 ENOENT，全新部署首次导入必失败。
- **#478**（续写/救援机制）：`6e7fe628` 长文续接、`07f8ae03` reasoning-only 救援、
  `c2003af1` 预告句续写、`1037397c` 工具轮续写、`478d2aa8` debugcap 取证、`4bdbd607`
  续写诊断日志。本仓 `internal/server` 无 `continue.go`/`starved.go`/`narration.go`/
  `toolonly.go`，零输出兜底只认 `finish_reason=length`（`handler.go:3179-3199`），
  「声明工具却 stop」仅 WARN（`handler.go:3174-3177`）。
- **#479**（号池治理 + 代理完整适配）：`592fb01a` 代理订阅导入 / 号池分区 / 额度窗口探针，
  `bfdba519` 传输层失败归因到出口。本仓已有 `internal/outbound/{pool,proxy,registry}.go`、
  `upstream.proxies`/`proxy_slots`/`realm_proxy_slots`（`cmd/server/config.go:344-353`），
  但 `proxies` 只收 URL 字面量；无 `scope.go`/`window.go`/`proxypool`；
  `CooldownSoftForModel` 只被动冷却（`cooldown.go:239-278`）；传输层失败仍对当前被选号
  `NoteFailures`（`handler.go:2712-2726`），`ErrProxyDial` 分支已不罚号（`2697-2706`）。
- **#480**（运维可观测性）：`18a67761` 日志落盘+ZIP 导出、上游劣化滑窗告警（选号策略本仓
  已有 `internal/pool/strategy.go`，不重复实现），`44bc0595` 工具调用缺名 WARN。
- **#481**（会话粘性）：`d165e6e1` 只发 `user_id` 的客户端按内容派生键粘账号。本仓
  `handler.go:1855-1873` 缓存槽已用 `session.DeriveCacheKey`，账号绑定仍只认 sessKey。
- **#482**（出站修补 + 配置收敛）：`f987bacc` 空 text 段补位 / Kimi·GLM reasoning 镜像、
  `d78131ba` `/console/account` 平铺响应与空昵称不静默成功、`89c9b7d9` 流式 flush 收敛、
  `877324fe` `thinking.default_effort` 统一到 `DefaultGatewayEffort`。`11151` 归请求级本仓
  已覆盖（`client.go:974`，`client_test.go:138` 断言 `400+11151→ErrBadParams`），官方包
  `errorCode.11151`=「Empty messages found」确认语义；`profile.go:47-65` 只按信封解。

其余 6 条按 `skip` 落账：`c5ce29b1` 多文件预览 UI（面板技术栈）、`7028676c` `.gitignore`、
`24d0b61b` `cache_audit.py`（参考侧脚本），以及 3 条纯 merge（`1aa917d4`/`5702ab65`/
`cffb0389`）按 `merge-bubble`。后续审计从 `592fb01a` 之后开始。

## 四、本分支相对基线的演进（节选）

- **可视化运维层**：内嵌 Web 管理面板（账号运维 / 用量与积分 / 模型档位 / 在线改配置 / 运行日志 / 任务中心）。
- **任务体系**：成长任务一键完成、任务中心全账号扫描与执行队列（含小程序口径任务）。
- **工程治理**（2026-10-16 重建时引入）：module path 改为 `github.com/BegoniaHe/wokbuddy-demon`、
  依赖升级至最新、`golangci-lint` v2 严格规则集（见 `.golangci.yml`）、`gofumpt` + `gci` 统一格式。

## 五、跟进策略（Maintenance Policy）

1. **官方客户端是协议权威**：参考 fork 的提交只是线索；先对照最新 WorkBuddy Win32
   包确认行为，再判断本仓库是否有缺口。包内无证据的结论按 `revisit`，不当事实。
2. **后端上游功能尽量适配**：除**商业化**、**面板技术栈**、**计费**三类外，参考 fork /
   hub 启发的后端能力（出站协议修补、号池治理、代理、续写救援、会话粘性、运维可观测性、
   配置收敛）默认按「应适配」开 Issue，而不是以「官方静态包无字面量证据」为由跳过。
   官方包证据只决定该能力的**协议口径**，不再决定是否适配。
3. **只审计、不吸收**：不做 `git merge` / `rebase` / `cherry-pick`，不改实现代码；
   确认缺口后只开 Issue。实现走独立的 issue-driven PR，由人类维护者评审。
4. **接口变更自行适配**：官方行为（CodeBuddy / WorkBuddy 接口）变更时，由本仓库在
   `internal/upstream/` 内自行适配，不依赖参考 fork 修复。
5. **溯源完整性**：本文件与 `LICENSE` 共同保留原始作者与各 fork 的署名；每次审计的
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
- 企业账号 chat 同时发 `X-Enterprise-Id` / `X-Tenant-Id`；CLI chat 的个人号不发任何
  `X-No-*`（桌面专家 chat 仍发 `X-No-Enterprise-Id: 1`，见 #455）。
- 官方 CLI chat 拦截器与 daemon 网关都不发 `Origin` / `Referer` / `X-Machine-Id` / `X-Session-Id`；本仓库的 `CommonHeaders` 同样不注入，只有 web 端点按自身契约构造 `Origin` / `Referer`。
- chat UA 由 CLI 层 `cli/dist/codebuddy-lite-wb.mjs` 的 `UserAgentHttpInterceptor.buildUserAgent` 构造；第二段是 `cli/product.json` 的 `applicationName`（CN `WorkBuddy` / global `workbuddy-ai`），显式 `client_version` / `cli_version` 覆盖时同样按 realm 取品牌；`/v2/report` 的 UA 是 desktop surface，两者不可混用。
- 401 同号 refresh + replay；refresh 401/403 映射会话失效；同账号/同 RT refresh 单飞（60s 按需复用）。
- 出站 `X-Conversation-ID` / `acp-connection-id` 恒发：客户端声明会话时原值透传；未声明时由
  `uid + CacheKeyID`（粘性会话键，或 content 派生的会话键）确定性生成 UUIDv4——同一账号同一会话
  跨轮稳定。部分上游后端以该头作为前缀缓存槽，逐请求随机会让缓存永不命中；`prompt_cache_key`
  的会话段与出站头共用同一来源。客户端完全无会话标识时每次生成新 UUIDv4（新会话语义）。
  `acp-connection-id` 是会话级**带连字符 UUIDv7**（真机主/辅助请求同值、version=7、时间戳前缀可解），
  按 conversationId 稳定派生、前缀取进程启动时刻前 1 小时内的确定性偏移；主请求的 `X-Conversation-ID`
  真机实测是 v4（辅助请求才是 v7），故该头兜底**保持 v4**（#372）。
- 出站头族按 **surface** 集中构造（`internal/upstream/headers.go` 的 `applyHeaders`）：chat 与 agenttool 四路复用 chat 头族，billing / report / refresh / v3-config / growth 走轻头族 base（Accept / Content-Type / UA / Authorization / X-User-Id / X-Domain / X-Request-ID）再按面覆盖。surface 差异只在该文件表达，调用点不得各自 Set / Del。
- `upstream.user_agent` 是**全部出站**的显式逃生门：各面自带默认（chat 三段式 / billing meter 抓包串 / report 三段式 / growth `node`）都让位给该值；`client_name="SaaS"` 仍是 billing meter 面的「不写 UA 键」逃生门（report 面不受它影响）。
- 首条消息非 system 时补官方 `"You are CodeBuddy Code.\n"` fallback（CN/global 都补）。
- chat 出站 body 缺省补 `max_tokens`：客户端未带（或显式 `null`）时按模型目录 `maxOutputTokens` 注入；非 null 的显式值优先，GPT 系仍受 ≥16 下限约束（注入先于 `clampGPTMinMaxTokens`，目录值不能绕过 clamp）。官方 CLI `configureModelConfig` 同样用目录 `getMaxOutputTokens` 补该字段；上游对缺失限额套保守默认（实测 32000），长输出会被无声截断。
- chat 出站 body 除 leading system 外每条消息补 `agent: "cli"`：官方 `HistoryInterceptors`（priority=ProviderData）对每个输入项写 `providerData.agent`（主会话取当前 agent 名），`itemsToMessages` 再把 `providerData` 摊进消息；`systemInstructions` 由调用方在摊平结果上单独 unshift，只有这条 leading system 不带该键。主会话 agent 名来自 `cli/product.json` 的 `agents`（`name=cli`）。客户端已带非空值时保留不覆盖。
- `6004` 带重置、`14003` 模型级限流只冷该模型；`14003` 用 10s→1m 短窗且不喂路由熔断；`6004` 无重置按账号级保守退避并记模型审计；`11158` 仅在 400/404 时与 `11136` 同归账号级模型权限避让，429+11158 归限流。
- billing/meter 的 summary / packages / get-user-resource 无前缀优先、`/v2` 回落（官方桌面 `resourcePrefix` 为空串）；签到例外保持 `/v2` 优先（官方 `DesktopAccountRepo.billingPrefix` 覆写为 `/v2`）。packages 按**桌面 daemon 用量链**取形：`UsageService.getPersonalUsage`（CN 5.9.3 `main/authority-snapshot.js@469554`）复用 `main/account.js` 的 `fetchUserResources` / `fetchAllPages`（`@378532`，`pageSize:200` 且以「不足 200 条」终止）与 `fetchResourcePackagesPage`（`@468153`），body 为 `{PageNumber, PageSize, PackageCodes, Status:[0,3], ...extra}`，paid 的 extra 是 `NeedRenewInfo:true`、free 的 extra 是 `buildSlicePeriodRange()`（`@378135`，导出名 `P`）的当日 `SlicePeriodStartTime`/`SlicePeriodEndTime` 窗口。`OnlyValidPeriod` / 无 slice 窗口的形态属渲染层 `CloudAccountRepo.validCreditPackages`（`@418501`，面板用量页用）那条 surface，本仓不采用。billing 的 `buildHeaders` 不带 UA，且**没有**桌面 UA 拦截器：2026-10-28 抓包证伪了「RestOperations 全局 `WorkbuddyUserAgentHttpInterceptor`」的推断，meter 面的 UA 就是 axios 库默认串，冻结在 profile 的 `emit.billingMeterUA`（`source=traffic`；字段缺失时回落 `desktopUA`，需重抓 profile 后才逐字生效）。`Accept-Language` / `X-Device-Token` 默认不发：summary 与 packages 由调用方带 `Accept-Language: zh|en`（短码，不是 `zh-CN`/`en-US`），`/v2/report` 的 UA 走三段式 `desktopUA`（显式 `user_agent` 优先），`X-Device-Token` 只在 chat 与签到注入（chat 见 `chatSurfaceOverrides`，签到见 `checkinHeaders`；`/v2/plugin/auth/state` 是面板登录面，不经 `internal/upstream`）。分 surface 取 base：桌面 report 走 `desktopBase`（即 `chatBase`），Web report 走 `webBase`，growth 任务 / 旅行走 `chatBase`，billing / 签到 / 活跃上报走 `billingBase`。本仓 CN billing 默认改为 `workbuddy.cn`（官方 5.9.3 桌面 billing / 签到 / 活跃上报全部打产品域，2026-10-28 真机 14 条业务请求 0 条打 `codebuddy.cn`）；显式配 `cn.billing_base=https://www.codebuddy.cn` 可退回旧口径。小程序埋点固定 `codebuddy.cn`，不跟随覆盖。这是 surface 取舍，不是遗漏。
- checkin 面有两个端点，本仓只做桌面那条：`/billing/meter/checkin-activity-status` 由官方桌面 daemon 走 `${endpoint}/v2/billing/meter/checkin-activity-status`（CN 5.9.3 `main/authority-snapshot.js@463251`，body `{}` + `getTuringDeviceTokenResult` 的 token 头），本仓 `CheckinActivityStatus` 覆盖的正是这条，也是调度器唯一调用方；`/billing/meter/checkin-status`（无 `-activity`）只出现在 IDE/CLI 侧 `BackendProvider.getCheckinStatus`（CN 5.9.3 `main/account.js@399217`、`renderer/assets/common-Cklb-JUT.js@443241`）与渲染层 IPC 面，本仓没有调用方，**刻意不实现**（不加推测性接口）。
- `X-Device-Token-Error` 只在 Turing SDK 取不到 token 时发，取值是 SDK 原生错误串（CN 5.9.3 `main/turing-sdk.js@674` 的 `unsupported platform <platform>`、`@817` 的 `native/turing-sdk is missing from the application bundle`），发出前经 `sanitizeTuringHeaderValue` 截断到 64 字符 ASCII（`main/authority-snapshot.js@452286`；取用点 `getTuringDeviceTokenResult@454671`、`buildHeadersWithTuringToken@466339`（`checkin-activity-status` 的调用点 `@463317`））。本仓 device token 走 `upstream.device_token_file` / 每号 auth 文件的 `device_token` 兜底（`resolveDeviceToken`），没有同源错误域，因此**不发该头**——伪造取值等于自造指纹。
- `/v2/report`（活跃上报）面 UA 用三段式 `desktopUA`、`Content-Type: application/json;charset=UTF-8`、Accept 宽松值（真机同形）；单段 `billingReportUA` 的旧口径已删除（#367）。
- **刻意偏离：billing 面 `Accept-Encoding`**。真机是 `gzip, compress, deflate, br`；本仓不显式设置，交给 Go transport（发 `gzip` 并透明解压响应）。手工 Set 该头会关掉 `Transport.DisableCompression=false` 的自动解压，SSE 与 JSON 响应都得自己解，代价远大于收益，故不跟。
- chat 出站 body **无条件 gzip**（`Content-Encoding: gzip`，`Content-Type: application/json` 不带 charset）：官方 `productFeatures.RequestBodyGzip` CN 为 true，包内 `GzipRequestProcessor` 没有字节阈值（真机主请求 45KB、辅助请求 1.2KB 都压）。唯一跳过条件是显式自定义 base——本仓对齐为「`cn.chat_base` / `global.chat_base` 被显式覆盖时不压」。不手设 `Accept-Encoding`（会关掉 Go 透明解压，SSE 直接坏），`Content-Length` 由 `bytes.Reader` 交给 `http.NewRequest` 算压缩后长度（#365）。
- SSE 接受 `delta.reasoning`、重复 tool id 去重、`finish_reason` 后 EOF 正常收尾；reasoning-only 不记成功。
- 签到两 realm `/v2` 优先；余额 summary 为主、旧端点兜底；模型目录应用 `availableModels` / `disabled` / `aliases`（企业 `/console/enterprises/<enterpriseId|personal>/models` 按账号拼接，`/v2/enterprises/personal/models` 回落仍固定 personal）。`/v1/models` 发现输出再按官方默认会话 agent（`/v3/config data.agents`，企业端点 `agents` 兜底；`tags` 含 default → `name=cli` → 首个有 models）裁剪，仅隐藏展示条目，`FetchModels`、面板与请求路由保持全量。带 `text-to-video` / `image-to-video` 标签的视频生成模型例外：它们保留在目录里并以 `capabilities` 数组在 `/v1/models` 可见（`ChatVisible=false`，不进 chat 可见集合，别名仍按隐藏处理，issue #467）。同一层还有第二条可见性规则：`productFeaturesConfig.ModelRateLimitCap.lines[].paidId` 的付费线路变体不单独成行（`freeId` 那一行撞顶时客户端才切过去），`/v1/models` 随之隐藏命中的 `paidId`——只在个人号 / pro 且至少一项 `paidId` 命中目录时启用，上游不下发该配置则不启用（issue #402）。
- growth `/v2` 优先；travel claim 空对象 `{}` 首选（官方桌面固定 `post(path, {}, productHeaders)`，是空 JSON 对象不是零字节 body），`{record_id}` 仅作 400 兜底（record_id 为 0 时不兜底，调度器也不因缺它跳过领奖）；桌面 report `X-Domain` 为 host 形态，body 指纹的 `os` / `arch` 取 `OSEventProcessor` 的 `process.platform()` / `process.arch()`（`win32` / `x64`），`osVersion` / `cpuModel` / `cpuCores` / `memorySize` 取宿主，报告服务**不写** `userAgent`（5.9.3 `main/module-base.js` OSEventProcessor；旧口径 `userAgent=Node.js/22` 来自 `CloudProductMetricsReporter` 那条上报路径，不属于本事件链）。
- images 两个端点都已覆盖：`/v2/images/generations`（`GenerateImage`）与 `/v2/images/edits`（`EditImage`），均受入站 `features.images_endpoint` 门控、单次尝试不换号。edits 是 **JSON 形态**（官方 `ImageService.editImage`：`Content-Type: application/json`，**不是 multipart**），body 为 `model` / `prompt` / `size`（默认 1024x1024）+ 可选 `image`（data URL / http URL / 裸 base64，单个字符串或字符串数组）+ 可选 `input_fidelity` 原样透传；头族与 generations 相同（`ImagesHeaders`）。CN 5.9.3 `cli/dist/codebuddy-lite-wb.mjs@6220394`。
- videos 两个端点都已覆盖（issue #467）：`/v2/videos/generations`（`GenerateVideo`，提交任务）与 `/v2/videos/tasks`（`GetVideoTask`，查询任务，body 为 JSON 编码的 `{task_id}`），均受入站 `features.videos_endpoint` 门控、单次尝试不换号；头族与图像面同源（`VideosHeaders`，chatFamily）。官方 CLI `VideoService` 是「`submitTask` 提交 → `pollTask` 循环直到 completed → `downloadVideos`」，网关按**无状态透传**设计（与 `images.go` 一致）：提交透出 `{code,msg,requestId,data:{id}}` 的 `data.id`，查询按客户端请求透出上游原文 `{code,msg,requestId,data:{status,data:[{url,...}],usage:{credit}}}`，**不实现轮询循环**；`status=failed` 终态由服务层归一为 502 通用文案（不透出上游直链）。CN 5.9.3 `cli/dist/codebuddy-lite-wb.mjs`（`VideoService.submitTask` / `pollTask`）。
- 队列业务码 `6020/6021/6022`、限流族 `6000-6008`、额度/认证码按客户端枚举实现；请求级码（`11125/11129/.../11155`、`12654/14405`）不罚号不轮转，`11136` 换号并做模型级避让，`11141` 模型下线 fail-fast，`11102` 按官方 5.9.3 文案是「该模型未接入或缺配置」（renderer `errorCode.11102` / CLI 对 `service info not found` 追加账号可用模型列表），官方处置为切换模型、不作账号处罚；本仓按请求级 404 fail-fast（默认不换号，`cross_domain_fallback` 提供一次性跨域回退；模型权限是否账号维度未验证），`11-128` 入站按内容拦截。
- SSE error 帧与 HTTP 200 业务信封优先按结构化 `code`（含 `bizCode`）分类，取不到再回落 message；客户端 `data.statusCode`/`actions`、`cgicode/ret/retcode` 与 `172xx` 技能码尚未进入聊天主路径验证，暂不实现。

非 chat 端点的头族取舍（刻意统一指纹，不是遗漏）：

| surface | 官方形态 | 本仓 | 取舍 |
| --- | --- | --- | --- |
| search / webfetch | raw `fetch`，`CLI/<desktop version>` UA，头更少 | 复用 chat 头族 | 统一指纹，避免同账号多客户端 |
| webfetch 的 `X-Wb-Uid` | 与 `X-User-Id` 同值（账号 uid） | `WebFetch` 单独注入 | 官方只在 webfetch / knowledge_search 发，不扩散到 search / agentic_search / images |
| agentic_search | `Content-Type` / `Authorization` / 可选 `X-Conversation-Request-ID` | 复用 chat 头族 + `Accept: text/event-stream` | 同上；SSE 契约优先 |
| images（generations + edits） | Content-Type / Authorization / conversation / identity | 复用 chat 头族 | 同上；两个端点同族同策略 |
| videos（generations + tasks） | Content-Type / Authorization / conversation / identity | 复用 chat 头族（`surfaceVideos`，`VideosHeaders`） | 官方 `VideoService` 与图像面同源；两个端点同族同策略；只做透传，不轮询 |
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
| chat `X-Device-Token` | 代码支持，运行态取决于部署侧 token 来源 | 真机 chat 主请求与辅助请求**都带**该头，与产品面公共头同层出现（`X-Private-Data` / `X-Codebuddy-Request` / `X-IDE-*` / `X-Product` / `X-Domain` / `X-User-Id` / `baggage` / trace 族 / `x-stainless-*` 同层），不在 `buildConversationHeaders` 的会话头族里；`#342` 的「只在 auth/state 与签到消费」对 chat 不成立（#376）。该头不是 CLI 静态构造（`cli/dist/codebuddy-lite-wb.mjs` 里字面量 0 命中），主进程每轮把 Turing SDK token 经 `internalModelRequestHeaders` 注入 CLI chat 头（`main/server.js` `createWorkbuddyAppServerBeforePromptExpertActivation` → `main/authority-snapshot.js` 的 `X-Device-Token` / `X-Device-Token-Error`）。本仓对齐的是**头位置与注入条件**：`chatSurfaceOverrides` 只在 `resolveDeviceToken`（auth 的 `device_token` > `upstream.device_token` > `upstream.device_token_file`）取到值时才发，三个来源皆空就省略——默认部署（cockpit 导入不保留 `device_token`、配置项留空）因此运行态比官方少这一个头，头序重写后线上头数也少一位（#464）。Trade-off：该头是风控标识，跨 surface 复用会把 chat 与签到关联到同一设备——发 = 更像官方，不发 = 更少设备关联；缺同源错误域，故不伪造 `X-Device-Token-Error`。其余面（summary / packages / report / busy / v3-config / refresh / growth / agenttool）真机不带，维持不发 |
| chat `Accept` | 已对齐 | 真机 chat（含 `stream: true`）仍发 `application/json`，不改 `text/event-stream`（#377）。agenttool 四路真机未取证，search / webfetch 保持 `application/json`、agentic_search 保持 `text/event-stream`（SSE 契约优先），仅 chat 有真机证据 |
| chat trace id 取值 | 已对齐 | 真机 `X-Trace-ID` / `X-B3-TraceId` / `traceparent` 的 trace-id 是 32 hex **随机值**（第 13 位为 4，无 v7 时间戳前缀），三处同值且 ≠ `X-Conversation-Request-ID`；本仓改为独立生成（`session.NewTraceID`），入站透传 `X-Trace-ID` 仍优先（#377） |
| refresh 头集与 body | 已对齐 | 真机 11 头 + body `{}`（`Content-Length: 2`）：`Accept: application/json, text/plain, */*`（宽松值，不沿用轻头族默认）、`Authorization`（与 `X-Refresh-Token` **双发**，与 chat 相反）、`X-User-Id`、`X-Request-ID`（32 hex）、`X-Product: SaaS`、`X-Requested-With`、三段式 UA、`X-Domain`（endpoint authority）、`X-Auth-Refresh-Source: plugin`（#370） |
| chat `X-Domain` | 已对齐（只认账号自报域，无 host 回落） | 官方 CLI 全局 auth 拦截器逐字为 `!headers[X-Domain] && auth.domain && (headers[X-Domain] = auth.domain)`：**只取登录态 `auth.domain`，没有「无 domain 回落 endpoint host」分支**。故 CN 无 domain 的账号 chat 不带该头（`injectChatDomain` 直接返回），global 无 domain 时按桌面面缺省补 `www.workbuddy.ai`（既有口径，官方 global 缺 domain 形态尚无抓包）；账号自报旧域（如 `www.codebuddy.cn`）原样发，不做值改写。2026-11-04 cn 真机 chat 第 40 位 `X-Domain: www.workbuddy.cn`（#465） |
| 轻头族 `X-Domain` | 已对齐（与 chat 是两条表面） | 官方 `enterpriseHeaders()` 无 domain 时回落 **endpoint authority**；轻头族各面（refresh / v3-config / billing / report / busy / growth）统一按本次请求 URL 的 host 取值，不回落 chat base——billing 面走 `billing_base`，取 chat host 会把别的域发给 billing。`cn.chat_base` / `cn.billing_base` 覆盖时各自跟随。该口径只服务认证/轻业务面与 daemon VLM 网关，**不可**套用到 chat（#465） |
| 非 chat 业务面的 `X-Request-ID` | 已对齐（refresh / v3-config / billing / busy / report） | 官方 HTTP 层 `applyRequestId` 对每条业务请求补 32 hex（UUIDv7 形态）；`/v2/activity/growth/*` 是 Node 裸 fetch 面，真机**不带**该头，故 growth 不在覆盖范围（#369 / #375） |
| `X-Stainless-*` | 已补（仅 CLI chat） | 2026-11-04 官方 5.9.3 抓包：`POST /v2/chat/completions` 恒带 7 个小写头（`arch` / `lang` / `os` / `package-version` / `retry-count` / `runtime` / `runtime-version`），值来自打包的 OpenAI SDK 6.25.0 + Node v22.21.1。值整组冻结在 profile `emit.stainless`（schema 2 必填），Go 源码不保留版本字面量；agenttool 四路与桌面 chat 不发（#456） |
| `X-Codebuddy-Request: 1` | 仅 chat 族（chat + agenttool 四路） | 2026-10-28 真机抓包：7 条 chat 全带；refresh / busy / report / v3-config / billing / growth 共 14 条业务请求 0 命中（#368）。agenttool 四路无真机证据，沿 chat 头族一起发 |
| 个人号 `X-No-Enterprise-Id` | chat / 模型目录已删，refresh / v3-config 保留 | 2026-11-04 官方 5.9.3 静态+抓包：chat 链路构造点为 0，个人号缺企业上下文时就是不发；本仓 chat 与 `FetchModels` / `globalModelsOnce` 改用 `injectEnterpriseIDHeaders`（仅企业 ID 非空时发 `X-Enterprise-Id` / `X-Tenant-Id`）。`injectEnterpriseHeaders` 的 `X-No-Enterprise-Id=1` 只留给 refresh / v3-config / 桌面面（#455） |
| 个人号 `X-No-Department-Info` | 已删除（chat 与模型目录） | 它原先只由 `injectChatDomain` 注入；官方 chat 从不发 `X-No-*`，缺少部门上下文时省略。refresh / v3-config 本来就不发该头，行为不变（#455） |
| 个人号 `X-No-Authorization` / `X-No-User-Id` | 已删除（chat） | chat 头族不再在缺 token / UID 时补声明头；`true` 形态只属于登录端点（`auth/state`、`auth/token`、`login/account`、`auth/logout`），不在 `internal/upstream` 的 chat 路径（#455） |
| W3C `baggage`（chat） | 已对齐（4 条） | 真机逐字 `buddyId=,buddyName=,codebuddy.session_id=<会话 id>,codebuddy.conversation_request_id=<请求 id>`；两个 buddy 键是官方无 buddy 上下文时的空值形态，两个 codebuddy 键与同请求 `X-Conversation-Id` / `X-Conversation-Request-Id` 同值。包内还有 `conversation_message_id` / `root_request_id` 两个键常量，但 chat 真机**没发**，多发反而更明显；`tracestate` 仅跨进程 span 携带时下发，网关不伪造（#364） |
| `X-Private-Data`（CLI chat） | 已对齐（按账号身份 + realm） | 该值是 `productFeatures.DefaultModelOptimizationOff` 的取反派生（`ModelProvider.getCustomHeaders` 读本地 `cli/product.json`，经 `resolveModelOptimizationEnabled` 折算成模型优化开关，header 取其反值）：CN=false → 个人号发 `false`；global=true → 个人号发 `true`；企业版（`isEnterpriseEditionAccount`：enterpriseId 非空或 type 为 ultimate/exclusive）恒发 `true`。该值随客户端产品配置漂移，每轮基线审计需复核。#400 曾把判据收成「只按企业身份、与域无关」并让 global 个人号发 `false`，已由 #411 修正；global 5.8.4 时代按 `DisableYuanbaoChannel=true` 一律发 `true` 的旧理由同样作废。远端 abTest / `executionConfiguration` 与会话 meta `privateDataSticky` 不在网关范围 |
| `X-Private-Data`（桌面专家路径） | 已对齐（固定 `true`） | 官方桌面主进程 chat 走 `applyGatewayHeaders`，兜底表 `VF` 里该头硬编码 `true`；`buildVlmGatewayHeaders` 同样硬编码。与 CLI chat 分属两条 surface，不能共用阈值；CN / global 5.9.3 同形 |
| custom model 的 `X-Private-Data` | 不适用 | 本仓没有 custom 模型上游路由（`isCustomModel` 仅用于模型目录可见性），无触达路径 |

### 头序（`upstream.header_order`）

官方 CLI chat 的头序不是 Go 的字典序：OpenAI SDK 的 fetch `Headers` 先按名字排序，
axios 适配器/拦截器再按插入序追加会话与身份头，Node `http` 最后补
`Content-Length` / `Accept-Encoding` / `Host` / `Connection: keep-alive`。Go
`net/http` 固定先写 `Host` / `User-Agent` / `Content-Length`，其余按字典序，`Header.Set`
的调用顺序无法改变线上字节；因此本仓在 TLS 之上用 `headerOrderConn`
（`internal/upstream/header_order.go`）重写整个明文头块。

- 作用域：仅 `POST /v2/chat/completions` 且头块带 `x-stainless-lang` 标记的 CLI chat；
  其他 surface、其他路径原样透传。
- 顺序与大小写：2026-11-04 抓包的 42 头（`Accept` 首、7 个 `x-stainless-*` 紧随、
  `X-Conversation-ID` / `X-Request-ID` / `acp-connection-id` / `baggage` /
  `traceparent` / `b3` 按官方大小写、尾部 `Content-Length` → `Accept-Encoding` →
  `Host` → `Connection: keep-alive`）。抓包未覆盖的可选头（`X-Parent-Conversation-ID`、
  `X-Compaction`、`X-Context-Compacted`、`X-Wb-Uid`、`X-Expert-Id`、企业/身份头、
  `X-Forwarded-For`、`X-Real-IP`）放在已捕获 40 头之后、尾部之前，保持原有相对顺序；
  未知头同样插在尾部之前。
- fail-open：头块无法解析或超过 64 KiB 时原样回写并关闭该连接的重写；body 与后续
  keep-alive 请求不受影响。
- `Accept-Encoding` 仍由 Go transport 发 `gzip`（透明解压），不手工改成官方
  `gzip, compress, deflate, br`——这是刻意偏离，见上文 billing 条目。

### TLS 指纹（`upstream.tls_fingerprint`）

`off`（默认）走 Go `crypto/tls`；`node` 用 vendored uTLS
（`internal/vendor/utls`，`require` + `replace`）按 profile `tls` 段构造 ClientHello，
复刻官方 Electron 37 / Node v22.21.1 / BoringSSL 的首连形状：

- 18 个 cipher（BoringSSL 默认 TLS1.3 序 + Node `DEFAULT_CIPHERS` 的 TLS1.2 段）、
  扩展序 `0,23,65281,10,11,35,13,51,45,43`、groups `29,23,24`、sigalgs
  `1027,2052,1025,1283,2053,1281,2054,1537,513`、无 ALPN / GREASE / 证书压缩。
- 会话恢复：第二次连接追加 `pre_shared_key`，BoringSSL 式 padding 就位后顺序为
  `...,43,21,41`，ClientHello 512 字节；动态记录分片关闭。
- 本地补丁：正常 TLS 1.3 握手不发 dummy `change_cipher_spec`（官方只在 HRR 分支发），
  以及 GREASE ECH 初始化的错误返回；补丁清单与升级流程在
  `internal/vendor/utls/README.md`。
- 失败不回落：指纹握手失败直接返回错误，不静默改走标准库 TLS。
- 能力边界：L1（ClientHello 形状）+ L1.5（PSK/padding）+ L2（记录层不发 dummy CCS）；
  0-RTT、证书压缩、ECH、完整 BoringSSL 行为（L3）不在范围。上游是否按 JA3/JA4
  判定仍无证据，故默认 `off`。

无桌面包证端点（**实现依据 ≠ 已对齐官方客户端**；检索范围：截至 CN 5.9.3、CN 5.8.3、global 5.9.3、global 5.8.4 四包 `app-asar` 内 `*.js` / `*.mjs` 均为 0 命中，路径字面量不出现）：

| 路径 | 实现位置（符号为准，行号仅提示） | 契约来源 / 口径 |
| --- | --- | --- |
| `/billing/meter/get-user-request-usage` | `internal/upstream/requestusage.go:19-20`（#31） | 第三方 WorkDaddy 脚本 `credit-request-usage.js` + 实测 |
| `/billing/ide/trial` | `internal/upstream/trial.go:13`（`global_register.go` 亦有记录） | 实测 / 参考实现 |
| `/billing/area/get-country-code` | `internal/upstream/global_register.go:124` | 实测 / 参考实现 |
| `/billing/meter/claim-gift`、`claim-compensation`、`compensation-status` | `internal/upstream/blackcat.go:92,97`、`compensation.go:28` | 本文件末尾「桌面包内没有证据的任务体系（连登/抽奖/补签/礼包/补偿/校园活动）继续以 Web 契约为准」总口径已覆盖，不单独论证 |

读这张表时不要反推「官方客户端里有这个端点」：前三条的形态来自第三方脚本、参考实现或实测，`claim-*` / `compensation-status` 出自 Web 契约；四行在桌面 static 包内都没有任何字面量证据。要重新论证时按上表列为起点，而不是把它们当作已对齐桌面。

活跃上报 `/v2/report` 的 `chat_request_send` 事件按 CN 5.9.3 形状发（#366）：删掉
`maxToken` / `maxSteps` / `temperature` / `maxRetries` / `skillCount` / `totalCount`；
补 `isMaxMode`（取 `getOverrideThinking(id)?.thinkingEnabled===true`，网关无 thinking
覆盖状态 → 官方默认值 `false`）、`agentType` 发 `main`。条件字段按客户端控制流整键
省略，不照抄单份真机样本：`source`（`meta.pendingSource` 为 string 才带）、
`inputAttemptId`（技能推荐链路才带）、`connectorId`（有值才带）。`agentName` 发
`cli`：官方主会话取 `agentName:t.agentName||t.meta?.agentName`，主 agent 名来自
`cli/product.json` 的 `agents`（`name=cli`），网关模拟的正是这条主会话路径；官方取不到
agent 身份的旁路事件才会缺该键，网关没有对应路径，不模拟该形态。

`vcsType` / `vcsRepo` / `vcsBranchName` / `vcsRevId` **不发**，`fileUri` 发空串：
官方 `fileUri` 取 `workspaceFolders?.[0]?.uri?.fsPath || ""`，`VcsInfoProcessor`
是 `let i=event.fileUri; if(i&&"string"==typeof i) try{写入四键}catch{}
finally{delete event.fileUri}`——只有路径非空时才替换成四元组并删除 `fileUri`；
空串走假分支，`fileUri: ""` 原样保留、四键不存在。补四个空串是「开了非 git 目录」
的形态，不是网关该断言的状态。

`reportDelay` 按官方 debounce 批模型回填 ≈ 2000：`report()` 入队写
`timestamp=now` / `reportDelay=0`，`FIRE_DELAY=2e3` 后 flush 前
`computeReportDelay` 改写为 `flush-timestamp`，故 `timestamp≈presentAt`、到达≈两者
之和。恒 0 只对应 `reportSync` / 退出前 flush，不是常规上报形态。

事件体在 `chatRequestEventMap` 里合并桌面指纹：报告的 ProductInfo / HostIDEInfo /
ExtensionInfo / OS / Userinfo / Buddy 处理器链对**每个**事件生效，`chat_request_send`
不是例外。`codebuddy.session_id` / `codebuddy.conversation_request_id` **不写进事件体**
——官方把它们作为 W3C baggage 键注入 `traceparent` / `baggage` 请求头（见 §chat 头族），
事件里的同名字段没有任何构造点。

桌面 `DesktopChatSequence` 的六段事件同样按 CN 5.9.3 构造：`chat_request_send` 删掉
5.8.x 限额字段、补 `isMaxMode=false` / `requestModelId` / `requestModelName` /
`fileUri`；`chat_message_send` / `chat_message_response` / `chat_message_status` /
`chat_request_response` 补 `requestId` / `requestModelId` / `requestModelName`，
其中 `chat_message_status` 不带 `presentAt`（官方构造里没有该键），其余三段按官方
`reportMessageSendTelemetry` 等构造补 `presentAt`。`traceId` 取会话级随机
UUIDv4（官方 `session.traceId`），与 UUIDv7 的 `requestId` 不同源，整条链共用同一个值。

该事件与桌面 `/v2/report` 同端点同头集，`surfaceReport` 发 `X-Product: SaaS`；
billing meter 面不发该头（`lightSurfaceOverrides` 里两路径分开）。

该事件的键集会否被上游校验未知，改动后建议做一次账号回归（点亮 growth 连登与
`first_buddy` 任务）。

桌面包内没有证据的任务体系（连登/抽奖/补签/礼包/补偿/校园活动）继续以 Web 契约为准，不随桌面端迁移。

## 七、参考 fork 审计工具（sync-upstream 技能）

- 技能位置：`.agents/skills/sync-upstream/`（OpenCode 经 `.opencode/opencode.json`
  的 `skills.paths` 加载）。
- 上游远端：`upstream` → `https://github.com/misakano7545/workbuddy2api-panel.git`
  （branch `main`）。首次使用执行 `git fetch --prune upstream`。
- 审计游标（逻辑基线，位于参考 fork 历史中，非本仓库祖先）：
  `771c887fb326b579026966d5540d5cf413b1c577`（2026-11-08 推进）。
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
_最近更新：2026-11-10（参考 fork 审计基线 `592fb01a`；窗口 `771c887f..592fb01a` 共 23 项=20 非 merge + 3 纯 merge，官方 Win32 无新协议缺口；按新政策「后端上游功能尽量适配，商业化/面板技术栈/计费除外」开 6 单：#477 面板导入、#478 续写/救援、#479 号池+代理完整适配、#480 运维可观测、#481 会话粘性、#482 出站修补+配置收敛；6 条按 skip；不直接吸收）。_
