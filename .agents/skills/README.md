# Skills

`.agents/skills/` 是本仓库**唯一**的技能来源。每个技能是一个目录，含 `SKILL.md`（共享指令），可选 `REFERENCE.md`、`scripts/`、`agents/openai.yaml`（Codex UI 元数据）。

## Codex CLI 与 OpenCode

两个客户端都从 `.agents/skills/<name>/SKILL.md` 发现技能。保持单一技能树，不要复制到 `.codex/` 或 `.opencode/`。仓库规则在 `AGENTS.md`，两个客户端都支持。

- Codex：用 `$skill-name` 触发，例如 `Use $sync-upstream to review pending upstream commits.`
- OpenCode：按名字请求，`skill` 工具加载共享指令。本仓库已在 `.opencode/opencode.json` 配置 `skills.paths: [".agents/skills"]`，因此 OpenCode 能发现本目录。

`agents/openai.yaml` 只提供 Codex 的短标签与起始 prompt，**不得**包含 `SKILL.md` 里没有的流程要求；OpenCode 不需要该元数据即可执行。使用当前客户端可用的 shell / 提问 / 浏览器 / 委派工具；缺某工具时用等价能力，或报告具体未验证部分。

共享约束（locks）位于 `.agents/shared/`：

| 路径                               | 作用                                       |
| ---------------------------------- | ------------------------------------------ |
| `conventional-commit/REFERENCE.md` | 提交信息形态                               |
| `ai-contribution/REFERENCE.md`     | Agent 可开什么 / 绝不可 merge 什么         |

## Catalog

| Skill           | 何时加载                                                       |
| --------------- | -------------------------------------------------------------- |
| `sync-upstream` | 吸收前序 fork `linguo2625469/workbuddy2api-panel` `main` 提交  |
| `plan-issue`    | 把 Issue 或请求变成研究充分、可验证的实施计划                  |
| `simplifier`    | 要最小改动、YAGNI、砍过度设计/依赖时                           |
| `reverse-workbuddy` | 解包 `.tmp/workbuddy/` 官方客户端并比对 `internal/upstream/` 协议差异 |

> 不要添加只复述 `AGENTS.md` 的技能。当出现第二个可独立加载的工作流时，再拆分技能。

## `SKILL.md` 契约

Frontmatter 用与目录一致的 `name`，以及一句以能力与触发词开头的 `description`。只在需要防止误触发时加排除说明。工作流细节放正文，需要时再加载支撑 reference。正文可用这些标题：

- **Locked** —— Agent 不得更改的工具链或行为
- **Open** —— Agent 可自行决定的产品设计
- **Do not** —— 硬性禁止
- **Handoff** —— 要引用的其他技能或文件（不粘贴）
- **Verify** —— 变更后的聚焦命令

引用 `AGENTS.md` 与共享 reference，不要内联。

## 说明

- 上游吸收技能的游标在 `upstream-sync.yaml`，决策账本在 `upstream-decisions.jsonl`。
- `sync-upstream` 的默认吸收方式是 **adapt**（非 cherry-pick），详见该技能 `SKILL.md`。
