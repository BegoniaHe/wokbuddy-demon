# AI 使用政策（AI Policy）

本 fork 允许并鼓励把 AI（大语言模型、编码助手、仓库内智能体）当作维护工具。贡献者与维护者对合入 `main` 的代码负责，质量标准与来源无关。

一句话：**AI 可以自主推进工作，但合并必须有人审，且须有一次独立的 AI 辅助评审。**

## 允许与禁止

AI **可以**：

- 阅读仓库、跑检查、写代码、写测试、写文档。
- 在功能分支上 commit、push。
- 开启开发用 Issue 与 Pull Request（目标仓库 `BegoniaHe/wokbuddy-demon`；`gh` 必须带 `--repo BegoniaHe/wokbuddy-demon`），在讨论中评论，请求评审。
- 按 [CONTRIBUTING.md](CONTRIBUTING.md) 与 [`.agents/shared/conventional-commit/REFERENCE.md`](.agents/shared/conventional-commit/REFERENCE.md) 生成提交说明。

AI **不可以**：

- 合并 Pull Request，或把提交直接推到 `main`。
- 对受保护分支 force-push。
- 打 tag、创建 GitHub Release、发布二进制 / 容器镜像 / 其他发行资产。
- 修改分支保护、仓库密钥、组织设置。
- 把安全漏洞写成公开 Issue 或公开 PR 描述。安全报告只走 [SECURITY.md](SECURITY.md)。
- 未经用户明确确认，把 Issue 或 PR 开到其他仓库（原上游 `Sliverkiss/workbuddy2api` 已删库；前序 fork `linguo2625469/workbuddy2api-panel` 非本 fork 的 PR 目标）。
- 代维护者在 PR 上点 Approve，或伪造他人的评审。

## 合并门槛

一条 Pull Request 合入 `main` 之前必须同时满足：

1. **人类维护者**完成审查，并明确批准或给出可合入的结论。
2. **同一条 PR 上存在 AI 辅助评审**（组织评审机器人、Copilot、或维护者另行发起的智能体评审）。作者智能体自己的说明不算这条评审。

人类维护者对合并决定负责。缺一不可：没有人审不得合；没有 AI 辅助评审也不得合。作者智能体不得以「CI 已绿」为由自行合并。

## 理解，而不是照搬

无论代码是否由 AI 生成，提交者必须能说明：这段改动解决什么问题、为什么这样写、边界与取舍是什么。评审时回答不出这些问题，就不应请求合并。

把 AI 当成打字快的协作者，不要把决策外包给它。无法解释的输出：删掉重写，或先读懂再改，不要直接请求合入。

## 提交说明与 PR 正文

提交标题用 `<type>(<可选 scope>): <描述>`：type/scope 英文、描述中文。AI 定稿的 commit message 必须带 `AI-Generated: true` 与 UTC `Generated-At:`，见 conventional-commit 参考。

PR / Issue 正文末尾必须有一段**意图说明**，且必须由实际作者撰写：

- **人类**使用 AI 辅助时，写 `## Human note`，用母语，用自己的话，说明想做什么、为何这样、验证了什么。不要粘贴 AI 摘要。
- **智能体**作为作者时，写 `## Agent note`，说明目标、触及路径、跑过的检查、剩余风险、以及用了哪些 AI 工具。不要伪造 `## Human note`。

## 与维护者沟通

- 人类贡献者不要用 AI 代写 Issue / PR 里的讨论回复。需要引用模型输出时，放进引用块并标明来源，再附自己的判断。
- 智能体可以评论与请求评审，但必须基于仓库事实与已运行的检查，不得编造「已验证」或「已与维护者确认」。
- 安全敏感改动（网关鉴权 `internal/httpauth`、面板鉴权与配置写回、auth 文件解析、出站脱敏 `internal/upstream/sanitize.go`、凭证/状态读写）必须在 PR 中写明如何验证边界、失败路径与资源上限。

## 敏感区额外核对

AI 生成内容常在这些地方出错，提交前核对：

- 是否复活了废弃 API、旧配置形态、旧 auth 文件布局，或旧版兼容层。
- 是否弱化常量时间鉴权比较、出站脱敏、凭证脱敏，或把 `api_key` 当无鉴权放行。
- 用户可见失败是否仍是泛化错误；日志、面板、API 是否泄漏密钥、URL、token、凭证。
- 是否把凭证（`auths/*.json`）、状态（`data/`）或真实 `config.json` 写进版本库、日志或面板回显。
- `config.example.json` 是否与新增配置项同步。
- 是否把上游（`AstrBot` / 前序 fork）的发布 / 文档 URL 写成 fork 产物。

## 归因

在 Human note 或 Agent note 里简短披露 AI 用在了哪里。披露是为了让审查者判断掌握程度，不是把 AI 用法藏起来。

---

本政策改编自 [AstrBot 的 AI_POLICY](https://github.com/Xero-Team/AstrBot/blob/master/AI_POLICY.md)（其又改编自 rust-analyzer 与 uv），并按本 fork 的 agent 维护模型调整：agent 可推进工作，人类负责合并。
