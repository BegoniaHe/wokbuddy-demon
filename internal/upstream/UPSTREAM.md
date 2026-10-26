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
- 401 同号 refresh + replay；refresh 401/403 映射会话失效；同账号/同 RT refresh 单飞（60s 按需复用）。
- 出站 `X-Conversation-ID` / `acp-connection-id` 恒发：客户端没给会话 ID 时生成 UUIDv4（不影响 prompt cache key）。
- 首条消息非 system 时补官方 `"You are CodeBuddy Code.\n"` fallback（CN/global 都补）。
- `6004` 带重置、`14003` 模型级限流只冷该模型；`14003` 用 10s→1m 短窗且不喂路由熔断；`6004` 无重置按账号级保守退避并记模型审计；`11158` 仅在 400/404 时与 `11136` 同归账号级模型权限避让，429+11158 归限流。
- CN billing/meter `/v2` 优先、无 `/v2` 回落（官方桌面 resourcePrefix 为空）。
- SSE 接受 `delta.reasoning`、重复 tool id 去重、`finish_reason` 后 EOF 正常收尾；reasoning-only 不记成功。
- 签到两 realm `/v2` 优先；余额 summary 为主、旧端点兜底；模型目录应用 `availableModels` / `disabled` / `aliases`。
- growth `/v2` 优先；travel claim 支持 `record_id` 与空 body；桌面 report `X-Domain` 为 host 形态。
- 队列业务码 `6020/6021/6022`、限流族 `6000-6008`、额度/认证码按客户端枚举实现；请求级码（`11125/11129/.../11155`、`12654/14405`）不罚号不轮转，`11136` 换号并做模型级避让，`11141` 模型下线 fail-fast，`11102` 模型目录不存在按请求级 404 fail-fast（不换号），`11-128` 入站按内容拦截。
- SSE error 帧与 HTTP 200 业务信封优先按结构化 `code`（含 `bizCode`）分类，取不到再回落 message；客户端 `data.statusCode`/`actions`、`cgicode/ret/retcode` 与 `172xx` 技能码尚未进入聊天主路径验证，暂不实现。

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
