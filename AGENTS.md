# AGENTS.md

本仓库面向 Codex CLI 与 OpenCode 的共享仓库指令。

## Working in this checkout

- 编辑前先检查工作区与相关代码。保留无关改动，用能完成请求的**最小改动**。
- 实现类请求要跑通相关检查；评审、诊断或规划类请求，返回要求的证据或产物；当用户已在本次会话授权实现时，才进入实现。
- 代码事实在本地解决。只就**会实质改变范围、行为或验收**的缺失决策提问；给出合理假设并继续独立工作。不要重复已答问题。
- 长时间工作给出简短进度。收尾时给结果、改动路径、实际运行的检查、以及剩余阻塞。除非产物有语言要求，否则匹配用户语言。
- 使用当前客户端暴露的工具。不要假设存在特定 shell、提问组件、浏览器、MCP 或 subagent。用等价工具，且不得因此改变仓库政策或谎称某个检查通过。
- 从 `.agents/skills/` 加载任务技能，见其[目录](.agents/skills/README.md)。Codex 用 `$skill-name`；OpenCode 用 `skill` 工具按名字加载。共享指令放 `SKILL.md`，Codex UI 元数据放 `agents/openai.yaml`。维护单一技能树；`.codex/` 与 `.opencode/` 保持本地。

## Project philosophy

本仓库是 **workbuddy2api 的现代化 fork**（`BegoniaHe/wokbuddy-demon`）。目标是**滚动更新**的个人本地自托管工具，不是长期兼容旧形态的产品。

- 不要为旧 API、旧配置形态、旧 auth 文件布局添加或保留兼容层（shim）。触及新旧分叉时，基于当前路径构建并删除旧路径。
- 面向个人使用：保持精简、可读、可自托管。凭证与状态始终留在用户自己的机器上。
- **滚动更新，无版本号**：不使用语义化版本，不做带版本的 Release；只有 nightly 构建，版本串在**编译时注入**。不要引入版本线或发布矩阵。
- 优先「解决当前问题的最小设计」。只有靠复活废弃路径才能成立的改动，是错误设计。
- Agent 可以维护本仓库：提交、推送功能分支、开开发用 Issue/PR。**不得** merge、push `main`、force-push 受保护分支、打 tag、发版。由人类维护者执行；口头例外不构成授权。PR 需人类维护者评审**且**一次独立 AI 辅助评审。遵循 [AI_POLICY.md](AI_POLICY.md) 与 `.agents/shared/ai-contribution/REFERENCE.md`。
- Issue 与 PR 只对 `BegoniaHe/wokbuddy-demon` 开。不要对原上游 `Sliverkiss/workbuddy2api`（已删库）或前序 fork `linguo2625469/workbuddy2api-panel` 开，除非用户显式确认该目标。`gh` 可能默认指向其他远端，务必 `--repo BegoniaHe/wokbuddy-demon`（或 `GH_REPO=BegoniaHe/wokbuddy-demon`）并确认 URL。

## Toolchain and setup

可复现的开发 / CI 基线：

| 工具                | 基线                                                        |
| ------------------- | ----------------------------------------------------------- |
| Go                  | 1.27（`go.mod`、CI、`Dockerfile` 同源）                     |
| golangci-lint       | v2（严格规则集见 `.golangci.yml`；含 `gofumpt` + `gci`）    |
| Bun                 | 前端工程 `web/` 的依赖与构建（Rsbuild + React 19 + Tailwind 4） |
| Python              | 3.x，仅用于 `scripts/*.py` 与 `login.sh` 的 JSON 解析       |
| Docker（可选）      | `docker compose` 本地镜像运行                                |

前端产物 `internal/panel/dist` 已提交进仓库，`go build`/`go test` 本身**无需** Bun；仅改动 `web/` 前端源码时才需 `make web` 重新构建并提交产物。

开始：

```bash
make run        # 构建并后台启动（立即返回终端）
make status     # 查看运行状态与 /healthz
make logs       # 跟踪日志
make stop       # 停止
make config     # 由 config.example.json 生成 config.json（已存在则跳过）
```

`make run` 构建 `wokbuddy-demon` 二进制后在后台启动（pid/日志在 `.make/` 与 `server_run*.log`），不阻塞终端；`make dev` 前台运行（`go run ./cmd/server`）。首次启动若缺 `config.json`，程序会自动生成推荐配置（含随机 `api_key`）。运行状态在 `data/`，凭证在 `auths/`。

聚焦工作时直接用组件：

```bash
go build ./...
go run ./cmd/server -config config.json
go test ./...
golangci-lint run ./...
cd web && bun run build   # 前端（改 web/ 后需重新构建 internal/panel/dist）
```

### 依赖与锁定

Go 依赖以 `go.mod` + `go.sum` 为准，用 `make tidy` 同步。不要引入未在 `go.mod` 声明的依赖；新增依赖需在提交说明里给出当前需要的理由（KISS）。

## Tests, checks, and formatting

```bash
make test          # 全量测试（含 scheduler 时序用例，约 30s）
make test-short    # 快速测试（跳过耗时/网络相关）
make test-race     # 竞态检测（较慢，手工跑）
make bench         # 基准测试
make cover         # 覆盖率
make ci            # fmt-check + lint + test（CI 本地复现）
make web           # 前端构建（web/ → internal/panel/dist，需 Bun）
make web-check     # 前端类型检查 + 单元测试
```

仓库门禁是分开的：

- `make fmt-check`：格式校验（`gofumpt` + `gci`），有差异非零退出。
- `make lint`：`golangci-lint run`（严格规则集）。
- `make vet`：`go vet`。
- `make build` / `make build-all`：构建。
- `make web-check`：前端 typecheck + vitest（`web/` 内改动必跑）。

CI（`.github/workflows/`）：`lint.yml` 跑 `golangci-lint` v2 与格式校验；`go-binaries.yml` 跑测试 + 五平台编译验证（PR/主干，不发布）；`web.yml` 跑前端 typecheck/test/build 并断言 `internal/panel/dist` 与源码一致；`nightly.yml` 每日构建 nightly 二进制与 `:nightly` 镜像；`docker-ghcr.yml` 在主干构建多架构镜像；`secret-scan.yml` / `dependency-review.yml` 做密钥与依赖审查。格式问题一律用 `make fmt` 修，不要手工对抗 formatter。

构建标识：`make build-all VERSION=<id>` 经 `-ldflags -X main.appVersion` 注入（缺省取 git 短 SHA）；CI/nightly 注入 `nightly-<date>-<sha>`。`appVersion` 只是运行时自述串，非发布版本。

## Architecture

WokBuddy Demon 是一个本地网关：接收 OpenAI 兼容的 `/v1/*` 请求，按账号池调度选择上游账号，转发到 CodeBuddy / workbuddy 上游，并处理签到、保活、成长任务、猫猫旅行等日常任务；同时提供内嵌 Web 管理面板。

### 入口与装配

- `cmd/server/main.go`：唯一入口。加载配置 → 构建 `pool` → 起 `scheduler` 与 HTTP 服务 → 面板与请求日志镜像。装配顺序（HTTP client 超时、auth 目录、store、会话粘性）在启动期一次性捕获，改这些字段需重启。
- `cmd/server/config.go`：配置解析与 `Default`/`normalize`；`cmd/server/wiring.go` 承载装配辅助。
- `cmd/login`、`cmd/signin`、`cmd/credit`、`cmd/trial`：配套 CLI（登录 / 签到 / 积分日报 / 试用）。
- 首次运行自动生成 `config.json`；面板保存配置走「先写 tmp 再 rename」原子替换，并区分**热生效**（`livecfg` 快照、`pool.*`、`schedule.*`）与**需重启**（`listen`、`auth_dir`、`state_file`、`upstream.*`、`upstash.*`、`session_sticky` TTL 类），见 `restartRequiredFields`。

### 账号池与调度

- `internal/pool/`：账号池核心。熔断器、在途上限（含 `global` 分档）、连败降权、闲置补偿、积分保底、软冷却指数退避、会话粘性可用性闭包。状态持久化到 `state_file`，Redis 快照择优恢复（`internal/redisstore`，未配置则纯内存 Noop）。
- `internal/scheduler/`：定时任务（签到 / 旅行 / 活跃 / 保活 / 夜猫子 / 成长 / 余额刷新）。保持时段判定与顺序语义。
- `internal/session/`：会话粘性路由（TTL + GC）。
- `internal/auth/`：解析 WorkBuddy auth 文件（嵌套形 / 扁平形），refresh 后原子写回；`realm`（`cn`/`global`）隔离账号域。

### 上游与协议

- `internal/upstream/`：出站客户端。CodeBuddy / workbuddy 协议、SSE 流、DSML、tool-call 配对、设备 token、`global` 域路由、模型目录与倍率表、`model.json` 本地缓存。
- `internal/upstream/sanitize.go`：出站请求体脱敏（剥离上游内容审核黑名单指纹）。策略：键值/header 型指纹整段剥离，承载语义的模板句最小改写。
- 上游行为（接口）变更时，在本目录内自行适配，不依赖上游修复。

### 服务与面板

- `internal/server/`：HTTP handler、WAF/IP、退避、模型解析、降级、日志。
- `internal/httpauth/`：网关与面板共用的 Bearer 鉴权原语，常量时间比较（`subtle.ConstantTimeCompare`）。两处鉴权必须同口径。
- `internal/panel/`：内嵌管理面板（前端 `web/` 的构建产物 `dist/` 经 `//go:embed` 打进二进制），与二进制同体部署。账号运维 / 用量积分 / 模型档位 / 在线改配置 / 运行日志 / 任务中心。
- `internal/usage/`、`internal/reqlog/`、`internal/livecfg/`、`internal/logfmt/`：用量记录、请求归档（脱敏元数据）、热改配置快照、日志格式化。

### 配置与状态

- `config.example.json` 是配置项的**完整、无密钥**参考；新增配置项必须同步到这里。
- `config.json`（含 `api_key`）与 `auths/`、`data/` 均被 gitignore；**绝不**提交凭证、状态或真实配置。

## Security invariants

把这些当设计约束，而非可选加固：

- `api_key` 为空 = 不鉴权直接放行。公网部署**必须**设置 `api_key`，并置于 HTTPS / 反代之后或仅内网可达。默认监听 `:7863`，无内置 TLS。
- 鉴权比较必须走 `internal/httpauth` 的常量时间实现；不要退回字符串直接比较（计时侧信道）。
- 凭证（`auths/*.json`）、状态（`data/`）、`config.json` 不得进入版本库、日志或面板回显；请求归档只写脱敏元数据。
- 出站脱敏开关（`features.sanitize_blacklist_fingerprints`）默认开启，不要弱化。
- `upstream.passthrough_ip` 默认 false；开启前确认部署形态与信任边界。
- 面向用户的失败信息保持通用；日志/响应前脱敏 provider 错误、URL、凭证、token。

## Generated artifacts and documentation

- 面板前端源码在 `web/`（Rsbuild + React 19 + Tailwind 4）；构建产物 `internal/panel/dist` 提交进仓库并 `go:embed`。改 `web/` 后运行 `make web` 重新构建并提交产物；CI（`web.yml`）断言产物与源码一致，`web/` 内改动需通过 `make web-check`（typecheck + vitest）。
- `internal/upstream/model.json` 为本地模型目录缓存，按需从 models.dev 拉取后原子写回；缺失/损坏自动回落仓库内嵌种子。
- 用户可见行为变更更新 `README.md`（中文）。溯源/血缘变更更新 `internal/upstream/UPSTREAM.md`。
- 不要把上游（`AstrBot` / 前序 fork）的发布/文档 URL 当作本仓库产物；引用上游仅用于标注来源。

## Conventions

- **KISS / 第一性原理**：没有当前需要，就不加抽象、依赖、开关或兼容层。
- **Inline first**：相同逻辑重复 ≥3 次、或连续函数超过约 50 行时，才抽 helper。不要把线性代码碎成小包装。
- **跨平台**：涉及的行为需考虑 Windows / macOS / Linux 与 amd64 / arm64（发布为五平台二进制 + 多架构镜像）。
- **注释语言**：本仓库注释与提交描述用**简体中文**；标识符、API、命令、路径保持英文原样。
- **导入边界**：`internal/*` 各包职责单一；`cmd/*` 负责装配，不反向依赖。改动跨包依赖时明确说明。
- **错误处理**：错误信息面向开发者，用中文说明「发生了什么、为什么」；不要吞错。
- **Skills**：从 `.agents/skills/` 加载。政策与事实分离（`SKILL.md` vs `REFERENCE.md` / 共享文件）。目录见 `.agents/skills/README.md`。引用这些文件，不要粘贴。
- **Commits/PRs**：遵循 `.agents/shared/conventional-commit/REFERENCE.md`。`<type>(<可选 scope>): <描述>`，type/scope 英文、描述中文，小写命令式、结尾无句号。破坏性变更用 `!` 加 `BREAKING CHANGE:` 脚注。issue 编号放脚注（`Fixes #123`），不当 scope。Agent 生成/定稿时运行 `date -u '+%Y-%m-%dT%H:%M:%SZ'` 并追加 `AI-Generated: true` 与 `Generated-At:`。Agent 可提交、推送功能分支、开 PR；**不得** merge、push `main`、打 tag。PR 用 `--repo BegoniaHe/wokbuddy-demon`。Agent 作者 PR 以 `## Agent note` 结尾；人类作者以 `## Human note` 结尾。用 `.github/PULL_REQUEST_TEMPLATE/` 下匹配 type 的模板（`perf`/`style` 归 `refactor.md`；`build`/`ops`/`test` 归 `chore.md`）。上游 cherry-pick/adapt 的标题保持原样；见 sync-upstream 技能。

## Upstream synchronization

本仓库的默认上游吸收方式是 **adapt**，不是 merge，也不是默认 cherry-pick。上游是前序 fork `linguo2625469/workbuddy2api-panel`（branch `main`）；原始上游 `Sliverkiss/workbuddy2api` 已删库。因历史已重建（无共同祖先）且已分叉，绝大多数上游提交需按当前边界重新实现。

```bash
# 远端不存在则 add，已存在则 set-url
git remote add upstream https://github.com/linguo2625469/workbuddy2api-panel.git
git remote set-url upstream https://github.com/linguo2625469/workbuddy2api-panel.git
git fetch --prune upstream
python3 .agents/skills/sync-upstream/scripts/inspect_upstream.py
```

逐提交评估（oldest-first），默认 `adapt`；`cherry-pick` 仅用于与 fork 分叉路径不重叠且无冲突的提交；`skip` / `replay` 更少。冲突不得盲目 ours/theirs。详见 `.agents/skills/sync-upstream/SKILL.md`。每次决策写入 `upstream-decisions.jsonl`，游标 `upstream-sync.yaml` 仅在区间全部有处置后推进。

## Releases

**本项目采用滚动更新，无版本号，只做 nightly 构建。** 因此：

- 不维护语义化版本线，不建立 tag↔版本的一致性要求。
- 版本串在**编译时注入**二进制（构建期写入），仅用于运行时自述与面板 `/panel/api/overview` 展示，不是可比的发布版本。
- 发布产物为 nightly 构建（二进制 + 容器镜像）；不要把 nightly 描述为带版本的正式发布。
- 发布准备不是「会发布版本化资产」的承诺；具体的 nightly 产物由 CI 工作流决定，改动这些工作流属于 `ops` 类提交。
