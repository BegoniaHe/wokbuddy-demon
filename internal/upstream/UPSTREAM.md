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
- 首条消息非 system 时补官方 `"You are CodeBuddy Code.\n"` fallback（CN/global 都补）。
- `6004` 带重置、`14003` 模型级限流只冷该模型；`14003` 用 10s→1m 短窗且不喂路由熔断；`6004` 无重置按账号级保守退避并记模型审计；`11158` 仅在 400/404 时与 `11136` 同归账号级模型权限避让，429+11158 归限流。
- billing/meter 的 summary / packages / get-user-resource 无前缀优先、`/v2` 回落（官方桌面 `resourcePrefix` 为空串）；签到例外保持 `/v2` 优先（官方 `DesktopAccountRepo.billingPrefix` 覆写为 `/v2`）。billing 的 `buildHeaders` 默认不带 UA，但 `context.http` 的 CellJS RestOperations 注册了 `WorkbuddyUserAgentHttpInterceptor`，meter 最终带桌面三段式 UA（`desktopUAFor`）；`Accept-Language` / `X-Device-Token` 默认不发：summary 与 packages 由调用方带 `Accept-Language: zh|en`（短码，不是 `zh-CN`/`en-US`），`/v2/report` 例外显式发单段 `WorkBuddy/<ver>` UA（显式 `user_agent` 优先），`X-Device-Token` 只在签到注入（`/v2/plugin/auth/state` 是面板登录面，不经 `internal/upstream`）。分 surface 取 base：桌面 report 走 `desktopBase`（即 `chatBase`），Web report 走 `webBase`，growth 任务 / 旅行走 `chatBase`，billing / 签到 / 活跃上报走 `billingBase`。本仓 CN billing 默认仍走 `codebuddy.cn`，可用 `cn.billing_base` 灰度切到 `workbuddy.cn`；小程序埋点固定 `codebuddy.cn`，不跟随覆盖。这是 surface 取舍，不是遗漏。
- SSE 接受 `delta.reasoning`、重复 tool id 去重、`finish_reason` 后 EOF 正常收尾；reasoning-only 不记成功。
- 签到两 realm `/v2` 优先；余额 summary 为主、旧端点兜底；模型目录应用 `availableModels` / `disabled` / `aliases`（企业 `/console/enterprises/<enterpriseId|personal>/models` 按账号拼接，`/v2/enterprises/personal/models` 回落仍固定 personal）。`/v1/models` 发现输出再按官方默认会话 agent（`/v3/config data.agents`，企业端点 `agents` 兜底；`tags` 含 default → `name=cli` → 首个有 models）裁剪，仅隐藏展示条目，`FetchModels`、面板与请求路由保持全量。
- growth `/v2` 优先；travel claim 支持 `record_id` 与空 body；桌面 report `X-Domain` 为 host 形态，body 指纹带 `userAgent=Node.js/22`（官方 Electron 主进程 `navigator.userAgent`）与 `os=Win32`（官方 `navigator.platform`），与 HTTP 头 UA 不同源。
- 队列业务码 `6020/6021/6022`、限流族 `6000-6008`、额度/认证码按客户端枚举实现；请求级码（`11125/11129/.../11155`、`12654/14405`）不罚号不轮转，`11136` 换号并做模型级避让，`11141` 模型下线 fail-fast，`11102` 模型目录不存在按请求级 404 fail-fast（不换号），`11-128` 入站按内容拦截。
- SSE error 帧与 HTTP 200 业务信封优先按结构化 `code`（含 `bizCode`）分类，取不到再回落 message；客户端 `data.statusCode`/`actions`、`cgicode/ret/retcode` 与 `172xx` 技能码尚未进入聊天主路径验证，暂不实现。

非 chat 端点的头族取舍（刻意统一指纹，不是遗漏）：

| surface | 官方形态 | 本仓 | 取舍 |
| --- | --- | --- | --- |
| search / webfetch | raw `fetch`，`CLI/<desktop version>` UA，头更少 | 复用 chat 头族 | 统一指纹，避免同账号多客户端 |
| webfetch 的 `X-Wb-Uid` | 与 `X-User-Id` 同值（账号 uid） | `WebFetch` 单独注入 | 官方只在 webfetch / knowledge_search 发，不扩散到 search / agentic_search / images |
| agentic_search | `Content-Type` / `Authorization` / 可选 `X-Conversation-Request-ID` | 复用 chat 头族 + `Accept: text/event-stream` | 同上；SSE 契约优先 |
| images | Content-Type / Authorization / conversation / identity | 复用 chat 头族 | 同上 |
| chat/busy | renderer `wb.http.post` | 复用 chat 头族 | 同上 |

证据来自部署 profile 与 `.tmp/workbuddy` 解包（2026-10-27）：CN 5.9.3 包内
`X-Agent-Runtime` 已是基线行为（`normalizeAgentRuntime` 不在枚举内回落 `other`），
官方在 chat / images / search / webfetch 全发；本仓统一由 `ChatHeaders` 发官方兜底值
`other`（网关没有 session meta / `CODEBUDDY_AGENT_RUNTIME` 来源）。同版 `X-Wb-Uid`
只出现在 webfetch（与 knowledge_search），取值与 `X-User-Id` 同源，故仅在 `WebFetch`
单独注入，不扩散到 search / agentic_search / images。版本值仍只来自部署 profile，
不进入仓库内置常量。

chat / refresh 头族差异（已对齐 / 刻意保留 / 不适用）：

| 项 | 状态 | 依据 |
| --- | --- | --- |
| refresh trace（`X-Trace-Id` / `Traceparent` / `B3` / `X-B3-*`） | 已对齐 | 官方 `cli/dist/codebuddy-lite-wb.mjs` 的 `AccountScopedAuth.refreshSession()` 追加 `traceSpan.requestHeaders`；网关无活动 span，故生成自洽的新链路 id |
| CN refresh `X-Domain` | 已对齐 | 官方 `enterpriseHeaders()` 无 domain 时回落 endpoint authority；本仓回落 chat endpoint host，`cn.chat_base` 覆盖时跟随 |
| `X-Stainless-*` | 不补 | 由 OpenAI Node SDK 自动生成；本仓出站不是 Node SDK，补等于伪造 SDK 运行时版本 |
| `X-Codebuddy-Request: 1` | 刻意保留 | D1 风控闸门头；官方 chat adapter 未注入、daemon/ACP 注入，但无实测证明移除安全 |
| 个人号 `X-No-Enterprise-Id` / `X-No-Department-Info` | 刻意保留 | 非 chat 端点也声明无部门信息（#298），按 chat-only 收窄会回退该修复 |
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
