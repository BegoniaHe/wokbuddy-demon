# 安全策略（Security Policy）

## 支持的版本（Supported Versions）

本仓库是滚动更新的 fork，**不发布版本化资产**：只有 nightly 构建（二进制 + 容器镜像），不存在版本支持矩阵。仅当前 `main` 分支受支持。原上游 `Sliverkiss/workbuddy2api`（已删库）、前序 fork `linguo2625469/workbuddy2api-panel` 与参考 fork `misakano7545/workbuddy2api-panel` 的构建不是本分支产物，其安全问题不在本仓库范围内。

| 版本 / 分支                      | 是否支持             |
| -------------------------------- | -------------------- |
| `main`（本 fork）                | ✅ 支持              |
| 历史提交 / 旧 nightly            | ⚠️ 尽力而为          |
| 前序 / 参考 fork `workbuddy2api-panel` | ❌ 报对应 fork |
| 原上游 `workbuddy2api`           | ❌ 已删库，无从报告  |

## 报告漏洞（Reporting a Vulnerability）

**切勿为安全漏洞开启公开 Issue。** 开发 Issue 仅供维护者与智能体跟踪代码工作，不是披露渠道。公开 Issue 会在修复前暴露问题。智能体不得把漏洞写成公开 Issue 或公开 PR。见 [AI_POLICY.md](AI_POLICY.md)。

**推荐渠道 —— GitHub 私密安全公告（Private Security Advisory）：**

1. 进入 <https://github.com/BegoniaHe/wokbuddy-demon/security/advisories/new>。
2. 填写描述、受影响组件与可复现步骤（如有）。
3. 提交。仅仓库维护者可见。

若 GitHub 私密报告不可用，请直接联系维护者（@BegoniaHe）并索要私密渠道。请勿在 Issue、PR 或任何公开场合发布漏洞细节。

请提供：

- 漏洞与影响的清晰描述。
- 受影响组件或路径（如网关鉴权 `internal/httpauth`、面板鉴权、auth 文件解析、出站脱敏 `internal/upstream/sanitize.go`、凭证/状态读写）。
- 最小复现步骤（请脱敏 `api_key`、token 与凭证）。
- 严重程度评估与修复建议（可选）。
- 是否希望被致谢（可选）。

## 响应时间（Response Timeline）

| 步骤           | 目标                                   |
| -------------- | -------------------------------------- |
| 确认收到       | 2 个工作日内                           |
| 初步评估与定级 | 5 个工作日内                           |
| 修复或缓解方案 | 评估后告知                             |
| 协调披露       | 修复落地后或 90 天，以先到者为准       |

以上为目标而非承诺。我们会在每一步同步进展。若在目标时间内未收到确认，请通过同一私密渠道跟进。

## 协调披露（Coordinated Disclosure）

- 报告在修复可用或 90 天窗口期满前保持私密。
- 修复发布时我们会发布 GitHub 安全公告（如适用则申请 CVE）。
- 善意且遵守协调披露的报告者将被致谢（除非另有要求）。
- 对遵守本策略的善意安全研究，我们不会采取法律行动（安全港原则）。

## 范围（Scope）

**在范围内：**

- 本仓库代码：网关（`cmd/server`、`internal/server`）、账号池（`internal/pool`）、上游客户端（`internal/upstream`）、内嵌面板（`internal/panel`）、配套 CLI 与 `scripts/`、部署清单（`Dockerfile`、`docker-compose.yml`）。
- 鉴权（`internal/httpauth` 常量时间比较）、凭证解析与写回（`internal/auth`）、出站脱敏（`internal/upstream/sanitize.go`）、配置热改与写回（`internal/livecfg`、面板配置保存）。

**不在范围内：**

- **上游服务**（CodeBuddy / workbuddy 接口行为、账号风控）——非本仓库代码。
- **前序 fork / 原上游** 的问题——报对应仓库；原上游已删库。
- **自致暴露**——未设置 `api_key` 即把网关暴露公网、无防火墙/反代绑定 `0.0.0.0`、把 `auths/`、`data/` 或真实 `config.json` 提交进版本库或泄漏到日志。
- **依赖库漏洞**——报上游依赖维护者；本仓库通过 Dependabot 跟踪修复（见 `.github/dependabot.yml`）。
- **自托管实例的 DoS / 资源耗尽**，或社工攻击。

## 已有的安全工具（Security Tooling）

- [密钥扫描](.github/workflows/secret-scan.yml) —— 检测误提交的密钥。
- [依赖审查](.github/workflows/dependency-review.yml) —— 阻止引入已知漏洞依赖的 PR。
- [Dependabot](.github/dependabot.yml) —— 定期更新 Go module、GitHub Actions 与 Docker 基础镜像。

## 运维加固提示（Hardening Notes）

- **必须设置 `api_key`**：为空 = 不鉴权直接放行。公网部署必须设置，并置于 HTTPS / 反向代理之后或仅内网可达。默认监听 `:7863`，无内置 TLS。
- 网关与面板默认绑定监听地址；如无必要，保持仅 `127.0.0.1` 可达。
- 保持出站脱敏开关（`features.sanitize_blacklist_fingerprints`）开启；`upstream.passthrough_ip` 默认 false，开启前确认信任边界。
- `auths/*.json`、`data/`、`config.json` 留在本机且被 gitignore；不要分享含 `api_key` 的启动日志。
- 面板「添加账号」落盘的凭证仅存于本机；不要在公开场合粘贴凭证内容。
