# CONTRIBUTING

## 仓库定位

本仓库是 workbuddy2api 的现代化 fork（`BegoniaHe/wokbuddy-demon`）。贡献时请遵守以下原则：

- 以当前分支代码事实为准，不为旧 API、旧配置形态、旧 auth 文件布局继续补兼容层。
- Go 基线是 `1.27`（`go.mod`、CI、`Dockerfile` 同源）。
- 滚动更新，无版本号：不维护语义化版本线，只有 nightly 构建，版本串在编译时注入。
- 如果新旧路径并存，优先沿用新路径，不要继续扩展 legacy shim。

## 问题与反馈

本 fork 不运营用户支持队列。开发用 Issue（缺陷、功能、任务模板）跟踪代码工作，不是客服。安全漏洞按 [SECURITY.md](SECURITY.md) 私密报告，不要开公开 Issue。

AI 使用与合并门槛见 [AI_POLICY.md](AI_POLICY.md)：智能体可以在 `BegoniaHe/wokbuddy-demon` 开 Issue/PR（`gh` 必须带 `--repo BegoniaHe/wokbuddy-demon`），但不得合并、推送 `main`、打 tag 或发布；合入需要人类维护者评审加上另一次 AI 辅助评审。未经明确确认，不要开到其他仓库。

开发 Issue 使用标签 `bug`、`enhancement`、`task`。

## 开发环境

```bash
git clone https://github.com/BegoniaHe/wokbuddy-demon.git
cd wokbuddy-demon
make config     # 由 config.example.json 生成 config.json（已存在则跳过）
make run        # 构建并后台启动（立即返回终端）
```

依赖：Go 1.27+、`golangci-lint` v2（含 `gofumpt` + `gci`）。安装：

```bash
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
```

常用命令：

```bash
make run            # 后台启动，返回终端
make dev            # 前台运行（Ctrl-C 退出）
make status         # 运行状态 + /healthz
make logs           # 跟踪日志
make stop           # 停止
make build-all      # 构建全部二进制（server/signin/login/credit）
make test           # 全量测试
make test-short     # 快速测试
make lint           # golangci-lint
make fmt            # gofumpt + gci 格式化
make ci             # fmt-check + lint + test（CI 本地复现）
go run ./cmd/server -config config.json
```

面板改动（`internal/panel/*.html`、`*.js`）需保证前端冒烟测试通过。新增配置项必须同步到 `config.example.json`。

## 提交代码

- 分支名建议使用 `fix/`、`feat/`、`docs/`、`refactor/` 等前缀。
- Commit 与 PR 标题使用 Conventional Commits：`<type>(<可选 scope>): <描述>`。type/scope 用英文，描述用**简体中文**，例如 `fix(pool): 避免冷却期账号被重复选中`。
- `type` 取自：`feat`、`fix`、`refactor`、`perf`、`style`、`test`、`docs`、`build`、`ops`、`chore`。不要使用 `ci`；持续集成与部署类改动用 `ops`。
- `description` 用祈使现在时、句末不加句号。
- 破坏性变更在 `:` 前加 `!`，并在 footer 写 `BREAKING CHANGE:`。issue 编号放 footer（例如 `Fixes #123`），不要当作 scope。
- AI 辅助生成或定稿的 commit message 必须遵守 [`.agents/shared/conventional-commit/REFERENCE.md`](.agents/shared/conventional-commit/REFERENCE.md)，并在 footer 附加 `AI-Generated: true` 与 UTC `Generated-At:`。人工撰写的提交可以省略这两项 footer。
- 使用 AI 的人类贡献者在 PR 末尾写 `## Human note`（母语、自己的话）。智能体作为作者时写 `## Agent note`，不要伪造 Human note。见 [AI_POLICY.md](AI_POLICY.md)。
- 按 Conventional Commits 类型选用 PR 模板：

  | 类型                            | 模板                                        |
  | ------------------------------- | ------------------------------------------- |
  | `feat`                          | `.github/PULL_REQUEST_TEMPLATE/feat.md`     |
  | `fix`                           | `.github/PULL_REQUEST_TEMPLATE/fix.md`      |
  | `docs`                          | `.github/PULL_REQUEST_TEMPLATE/docs.md`     |
  | `refactor`、`perf`、`style`     | `.github/PULL_REQUEST_TEMPLATE/refactor.md` |
  | `chore`、`build`、`ops`、`test` | `.github/PULL_REQUEST_TEMPLATE/chore.md`    |

  示例：`gh pr create --repo BegoniaHe/wokbuddy-demon --template .github/PULL_REQUEST_TEMPLATE/fix.md`。`gh` 可能默认指向其他远端，必须带 `--repo BegoniaHe/wokbuddy-demon`，并确认创建出的 URL 属于 `github.com/BegoniaHe/wokbuddy-demon`。

- 不要把「兼容旧版本」的文案或代码路径重新带回仓库。
- 不要合并自己的 PR，也不要把提交直接推到 `main`。

提交前至少运行：

```bash
make fmt-check
make lint
make test
```

或直接 `make ci`。

## 上游审计

参考 fork 是 `misakano7545/workbuddy2api-panel`（前序 fork `linguo2625469/workbuddy2api-panel` 已停止维护，原始上游 `Sliverkiss/workbuddy2api` 已删库）。本仓库**不直接吸收**参考代码：先对照最新官方 WorkBuddy Win32 客户端做深度审计，确认缺口后开 Issue，实现走 issue-driven PR。详见 `.agents/skills/sync-upstream/SKILL.md`。
