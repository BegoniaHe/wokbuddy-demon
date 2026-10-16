# AI 贡献参考（AI contribution reference）

政策源：`/AI_POLICY.md`。提交信息源：`.agents/shared/conventional-commit/REFERENCE.md`。
不要把这两份文件粘进 prompt，引用它们即可。

## 何时加载

当 Agent 将提交、推送、开 Issue、开 Pull Request、评审留言，或判断某变更是否达到可合并标准时。

## Locked（硬约束）

- Agent **可以**维护本仓库：编辑、运行检查、提交、推送功能分支、在 `BegoniaHe/wokbuddy-demon` 上开开发用 Issue/PR、留言、请求评审。用 `--repo BegoniaHe/wokbuddy-demon`（或设 `GH_REPO=BegoniaHe/wokbuddy-demon`），使 `gh` 不会默认指向其他远端。创建后确认 URL 位于 `github.com/BegoniaHe/wokbuddy-demon`。**不要**在原上游 `Sliverkiss/workbuddy2api`（已删库）或前序 fork `linguo2625469/workbuddy2api-panel` 上开 Issue/PR，除非用户显式确认该目标。
- Agent **不得** merge PR、push `main`、force-push 受保护分支、打 tag、发布 Release、改动分支保护或 secrets、或公开提交安全报告。这些由人类维护者执行。口头例外不构成授权。
- 一个 PR 只有在**人类维护者**评审**且**有一次独立的 **AI 辅助评审**之后才可落地。作者 Agent 自己的说明不算那次评审。
- 提交信息遵循 `.agents/shared/conventional-commit/REFERENCE.md`：type/scope 英文、描述中文；AI 最终定稿需 `AI-Generated: true` 与来自 `date -u '+%Y-%m-%dT%H:%M:%SZ'` 的 UTC `Generated-At:`。
- 人类作者在 PR 里加 `## Human note`（本人措辞）。Agent 作者加 `## Agent note`。**绝不**伪造另一方。PR 用 `.github/PULL_REQUEST_TEMPLATE/` 下与 Conventional Commit type 匹配的模板（`feat.md`、`fix.md`、`docs.md`；`perf`/`style` 归 `refactor.md`；`build`/`ops`/`test` 归 `chore.md`）。
- 引用 `AGENTS.md`、具名 skill、具名文档。不要整份粘贴。

## Open（可自行决定）

用哪个功能分支、拆几个提交、本次会话是否开 PR——除非用户已决定。倾向一个提交一个关注点。

## Do not（硬性禁止）

- Merge，即便 CI 全绿。
- 未经用户对目标上游的显式确认，就在其他仓库开 Issue/PR。
- 声称某个检查通过，除非本次会话确实运行了它，或链接的 CI run 确实跑了。
- 恢复 legacy 兼容层、旧配置形态，或把上游发布/文档 URL 当成本仓库产物。
- 跳过用户可见行为的文档更新。

## Handoff（交接引用，不粘贴）

- 提交信息形态：`.agents/shared/conventional-commit/REFERENCE.md`。
- 上游吸收：`.agents/skills/sync-upstream/SKILL.md`。
- 合并决策：停下并指名人类维护者。不要自行 merge。
