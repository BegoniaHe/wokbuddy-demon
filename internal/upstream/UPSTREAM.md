# 上游跟进记录（Upstream Tracking）

本文件记录本仓库（`BegoniaHe/wokbuddy-demon`）的**上游溯源**与**跟进状态**，
供后续维护者判断「哪些来自上游、哪些是本分支独有」，以及上游是否还有可跟进的新提交。

> 维护约定：每次做上游相关的重大决策（换基线、合并上游提交、上游删库/改名）时更新本文件。

## 一、溯源链（Lineage）

| 层级 | 仓库 | 状态与说明 |
| --- | --- | --- |
| 原始上游（original upstream） | `Sliverkiss/workbuddy2api` | 项目起点。**已删库**，公开 git 历史不可再获取 |
| 前序 fork（previous fork） | `linguo2625469/workbuddy2api-panel` | 在原始上游基础上重构可视化运维层（Web 面板 + 任务体系） |
| 本仓库（this repo） | `BegoniaHe/wokbuddy-demon` | 2026-10-16 **全新 git 历史重建**，独立演进 |

## 二、同步基线（Sync Baseline）

- **原始上游代码同步点**：`ea8b1e5` —— `Sliverkiss/workbuddy2api` 删库前的最后一次更新。
  本仓库（经前序 fork）的账号池调度、错误分类、提示词体系等核心设计均源自该版本。
- **前序 fork 同步点**：`linguo2625469/workbuddy2api-panel` 的 `main` 末次提交
  （`Merge PR #152`，`fix(panel): 积分到期提醒卡片默认可见`）。
- **本仓库重建方式**：**丢弃全部 git 历史**，以当前工作区快照做初始提交。
  重建前的完整提交历史，请回溯前序 fork 仓库。

## 三、上游当前状态（Upstream Status）

| 上游 | 状态 | 跟进影响 |
| --- | --- | --- |
| `Sliverkiss/workbuddy2api` | 已删除（404） | 无新提交可跟进；原作者保留随时止损的权利 |
| `linguo2625469/workbuddy2api-panel` | 前序 fork，已停更（仍可 fetch） | **当前追踪的上游**；无新提交时同步为空操作 |

**结论**：当前**没有活跃的上游仓库可供 rebase / 合并**。前序 fork 仍可 fetch，作为
唯一的「上游提交」来源由 `sync-upstream` 技能按提交逐一评估吸收。本仓库自 `ea8b1e5`
基线之后的所有演进（面板、任务体系、依赖升级、lint/fmt 治理）均为本分支独立维护。

## 四、本分支相对基线的演进（节选）

- **可视化运维层**：内嵌 Web 管理面板（账号运维 / 用量与积分 / 模型档位 / 在线改配置 / 运行日志 / 任务中心）。
- **任务体系**：成长任务一键完成、任务中心全账号扫描与执行队列、开学季闭环。
- **工程治理**（2026-10-16 重建时引入）：module path 改为 `github.com/BegoniaHe/wokbuddy-demon`、
  依赖升级至最新、`golangci-lint` v2 严格规则集（见 `.golangci.yml`）、`gofumpt` + `gci` 统一格式。

## 五、跟进策略（Maintenance Policy）

1. **无常规上游 rebase**：原始上游已删库，不存在可合并的上游分支；前序 fork 仅作
   提交级评估来源，不做 `git merge` / `rebase`。
2. **默认 adapt，而非 cherry-pick**：因历史已重建（无共同祖先）、module path 与
   lint/fmt 基线不同，绝大多数上游提交不逐字适用，一律按当前边界重新实现
   （`cherry-pick` 仅用于与 fork 分叉路径不重叠且无冲突的提交）。
3. **接口变更自行适配**：上游行为（CodeBuddy / workbuddy 接口）变更时，由本仓库在
   `internal/upstream/` 内自行适配，不依赖上游修复。
4. **溯源完整性**：本文件与 `LICENSE` 共同保留原始作者与各 fork 的署名；每次吸收的
   逐提交决策记录于 `upstream-decisions.jsonl`（决策账本），游标见 `upstream-sync.yaml`。

## 六、官方客户端协议基线

本仓库的协议对齐以官方 WorkBuddy Desktop 静态包为基线（不运行官方客户端、不请求上游）：

| realm | 客户端版本 | CLI | commit | releaseDate |
| --- | --- | --- | --- | --- |
| cn | `5.8.3` | `2.161.0` | `64d249bed3dec40edd720537d96f27e70a3f6a4f` | `1791972709257` |
| global | `5.8.4` | `2.161.0` | `73952edb48da46bd0b244661efe22f4f247c4977` | `1792063023116` |

采用的桌面端 / CLI 行为：

- CN origin 主域 `www.workbuddy.cn`，首 token 前可回落 `copilot.tencent.com` / `wb.tencentbuddy.com`。
- 企业账号 chat 同时发 `X-Enterprise-Id` / `X-Tenant-Id`；个人 global 发 `X-No-Enterprise-Id: 1`。
- 401 同号 refresh + replay；refresh 401/403 映射会话失效。
- SSE 接受 `delta.reasoning`、重复 tool id 去重、`finish_reason` 后 EOF 正常收尾；reasoning-only 不记成功。
- 签到两 realm `/v2` 优先；余额 summary 为主、旧端点兜底；模型目录应用 `availableModels` / `disabled` / `aliases`。
- growth `/v2` 优先；travel claim 支持 `record_id` 与空 body；桌面 report `X-Domain` 为 host 形态。
- 队列业务码 `6020/6021/6022`、限流族 `6000-6008`、额度/认证码按客户端枚举实现；请求级码（`11125/11129/.../11155`、`12654/14405`）不罚号不轮转，`11136` 换号并做模型级避让，`11141` 模型下线 fail-fast，`11102` 模型目录不存在按请求级 404 fail-fast（不换号），`11-128` 入站按内容拦截。
- SSE error 帧与 HTTP 200 业务信封优先按结构化 `code`（含 `bizCode`）分类，取不到再回落 message；客户端 `data.statusCode`/`actions`、`cgicode/ret/retcode` 与 `172xx` 技能码尚未进入聊天主路径验证，暂不实现。

桌面包内没有证据的任务体系（连登/抽奖/补签/礼包/补偿/校园活动）继续以 Web 契约为准，不随桌面端迁移。

## 七、上游同步工具（sync-upstream 技能）

- 技能位置：`.agents/skills/sync-upstream/`（OpenCode 经 `.opencode/opencode.json`
  的 `skills.paths` 加载）。
- 上游远端：`upstream` → `https://github.com/linguo2625469/workbuddy2api-panel.git`
  （branch `main`）。首次使用执行 `git fetch --prune upstream`。
- 游标基线（逻辑基线，位于上游历史中，非本仓库祖先）：
  `6b31db6d045e985042620c819c3a938002e7b0b1`（前序 fork `main` 末次提交，Merge PR #152）。
- 常用命令：

  ```bash
  git fetch --prune upstream
  python3 .agents/skills/sync-upstream/scripts/inspect_upstream.py
  python3 .agents/skills/sync-upstream/scripts/upstream_decisions.py validate
  python3 .agents/skills/sync-upstream/scripts/upstream_decisions.py list --path internal/upstream
  ```

---

_发布形态：滚动更新，无版本号；`appVersion`（`cmd/server/main.go`）为编译期注入的构建标识，非发布版本。_
_最近更新：2026-10-16。_
