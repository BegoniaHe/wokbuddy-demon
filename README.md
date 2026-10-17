<p align="center">
  <img src="assets/wokbuddy-logo.png" alt="WokBuddy Demon" width="120">
</p>

<h1 align="center">WokBuddy Demon</h1>

<p align="center">
  <b>一只蹲在你机器里的小恶魔，替你照看 CodeBuddy 账号的每一天</b><br>
  自动签到 · 成长任务 · 猫猫旅行 · 连登抽奖 · 积分看板 · 多账号管家
</p>

---

> **一句话**：把 CodeBuddy 的日常琐事交给 WokBuddy Demon——它负责签到、养猫、做任务、盯积分，你负责摸鱼。
> 免费开源、纯本地自托管，数据（凭证 / 状态）始终留在你自己的机器上。

## 使用须知

WokBuddy Demon 是一个供个人打理自己 CodeBuddy 账号的本地小工具：自动签到、保活、把每天的成长任务顺手做完。免费、开源、按「原样」提供。

- 仅用于**本人授权的账号**与**本机 / 私有环境**；
- 请遵守 CodeBuddy 平台的服务条款与所在地法律；
- 妥善保管凭证目录（其中保存的是明文 token）与面板端口。

## 项目简介

WokBuddy Demon 是一个**自托管的 CodeBuddy 账号成长伴侣**：一只用 Go 写的小恶魔，替你把账号的每日动作照看得井井有条。

- 面向 **个人多账号** 场景：多个账号一起养、单号出故障自动让位、冷却 / 熔断防止雪崩；
- 把散落的官方能力收拢成一块 **Web 面板**：签到、任务、猫猫、积分、日志，一眼看全；
- 全流程纯自动化：定时排程 + 一键完成，无需安装官方客户端、无需人工盯屏。

> 建议在**本机 / 私有环境**下运行，并妥善保管凭证与面板端口。

## 核心能力

| 能力 | 说明 |
|---|---|
| 🔑 **一键登录** | `login.sh` 或面板「添加账号」走设备授权流程，自动落盘凭证并加载新账号 |
| 🐱 **账号管家** | 多账号统一打理：快过期积分加权 + 成本分层 + 加权随机轮换，Top-5 候选 + 防惊群 |
| 🛡️ **熔断与冷却** | 429 软冷却 600s 起指数退避、404 固定 60s 短冷却、402 硬冷却至次日 04:00、连续失败熔断 |
| 🧲 **会话粘性** | 同一会话尽量绑定同一账号，TTL 滚动续期，失败自动解绑 |
| ⏰ **定时任务** | 签到（09/21 点）+ 活跃上报（10 点）+ 猫猫旅行（09/21 点）+ 保活（22 点）+ 夜猫子（23 点），各自独立开关 |
| 🎯 **成长任务一键完成** | 18 个成长任务中 **17 个一键纯自动化**，推进进度、等待计分落定并**自动领奖** |
| 🎲 **连登管家** | 签到末尾自动兑换连登档位 + 抽完抽奖次数，全程无需人工盯 |
| 📊 **可观测** | 每请求一行表格日志（TTFB / 速率 / uid）；`/healthz` 带身份标识便于宿主探活；`/metrics` 暴露 Prometheus 文本指标 |
| 💾 **状态持久化** | 状态本地原子落盘 + 可选 Redis 异步镜像，重启择新恢复 |
| 🖥️ **Web 管理面板** | 内嵌面板（React + Tailwind，明暗主题，响应式，七大视图）：账号运维 / 用量分析 / 积分构成 / 任务中心 / 模型档位 / 在线配置 / 运行日志 |

## 🎯 成长任务一键完成（17/18）

官方「成长计划」的 18 个成长任务中，**17 个可在面板上一键完成**——无需安装官方客户端、无需人工交互，点一下「一键完成」即自动推进进度、等待异步计分落定并**自动领奖**。剩余任务展示操作指引。

### 任务覆盖与奖励

| 任务 | 奖励 | 一键完成方式 |
|---|---|---|
| `first_buddy` | +300c +8e | 解锁上报 → 同意协议 → 领养第一只 Buddy |
| `create_canvas` | +300c +5e | 设计画布创建事件组 |
| `chat_5` | +100c | 对话活跃上报 ×5（自动补足差额） |
| `Model_chat_GLM5.2` | +100c +5e | glm-5.2 真实对话一次（发一条短消息） |
| `RichMeow_Chat` | +100c +5e +UR Buddy | 桌面端对话事件链（6 事件，含成功回执） |
| `Buddy_App` | +100c +5e | Buddy 应用「发现→进入→授权」事件链 |
| `Buddy_App_QQ` | +50c +5e | 企鹅教师助手进入事件链（与上一条共用） |
| `automation_1` | +100c +5e | 定时任务创建成功事件 |
| `Library_read` | +100c +5e | 资料库阅读点击 |
| `template_5` | +100c +5e | 模板使用事件组 ×5 |
| `playbook_prompt` | +100c +5e | 灵感案例「做同款」发送事件 |
| `expert_5` | +100c +5e | 真实专家召唤 + 使用链 ×5 |
| `Expert_team_use_3` | +100c +5e | 专家团召唤 + 使用链 ×3 |
| `Hp_Appearance` | +100c +5e | 主题设置 + 皮肤生效事件 |
| `Expert_lighthouse` | +100c +5e | 轻量云专家召唤 + 使用链（可免费领一个月轻量服务器） |
| `skill_1` | +100c +5e | 真实对话 + 技能加载事件 |

**全新账号一键全做完 ≈ +1950 credits +78 能量**，其中仅数个任务涉及真实对话，其余全部为行为事件上报，零对话消耗。

### 不可自动的 1 个

| 任务 | 原因 |
|---|---|
| `Expert_Philanthropy` | 需真实捐款（服务端领奖时校验捐赠回执，已实测无法绕过） |

> ⚠️ 行为事件按天幂等：重复点「一键完成」不会重复扣资源，已达标的任务自动跳过。

### 🧭 任务中心

「任务中心」视图把散落的任务能力收拢成一处：

- **全账号任务扫描**：一键拉取每个账号的成长任务 + 开学季待办，列表一目了然
- **执行队列**：把待办按账号排队执行——账号内串行，账号间可选并发（1-3）；进度实时更新
- **开学季独立状态卡**：每账号 5 任务的状态矩阵 + 剩余抽奖次数，一键触发全账号闭环
- **日志分频道**：运行日志按「任务 / 对话 / 系统」三个频道筛选——任务结果不会被对话流量冲掉

## 🐱 猫猫旅行 & 连登管家

**猫猫旅行**（独立排程，默认 09/21 点各一趟）：无猫先领养，`idle` 派出，`arrived` 领奖；每自然日一次派出，单账号失败只跳过当趟。

**连登管家**（挂在每日签到末尾，幂等）：成长中心连登档位（连续登录 7/14/28 天）解锁后自动兑换，抽奖次数自动全部抽完，奖品记日志。到天数那天自动完成「兑换 → 抽奖」，无需人工盯。

## 🖥️ Web 管理面板

内嵌式管理面板（React 19 + Tailwind 4 构建，产物打进二进制；`go build` 本身无需 Node/Bun），服务启动后访问：

```
http://127.0.0.1:7863/panel/
```

鉴权与主服务同口径：`api_key` 非空时面板要求输入一次密钥（浏览器 localStorage 记住）；为空则直接可用。
界面支持**明暗主题切换**（首次跟随系统偏好），左侧导航八个视图：

| 视图 | 功能 |
|---|---|
| **账号管家** | 统计条 + 账号表：健康色条与状态标签、积分量条、成功·失败计数、最近用量；单号操作 签到 / 余额 / 任务 / 解冻 / 禁用 / 移除；批量「全部签到」「旅行巡检」「活跃上报」「全部保活」 |
| **用量** | 指标卡 + Token 时序图 + 用量明细（按账号 / 按模型 / 按域切换，可排序）+ 积分扣除历史；「回填上游」按账号拉取官方逐笔用量并按天覆盖本地桶用于对账 |
| **积分构成** | 积分到期分布 + 账号对比卡片 + 逐包明细（按到期日排序） |
| **任务中心** | 成长任务队列：一键扫描全部账号待办 → 按账号排队执行，进度实时更新 |
| **模型与档位** | 实时查询每模型倍率 / 思考档 / 上下文长度与最大输出，支持按条件查询与排序 |
| **密钥** | 命名 API 密钥管理：创建（明文仅显示一次）/ 撤销 / 删除，并按密钥查看请求数与 token 用量 |
| **配置** | 在线编辑配置：服务 / 定时任务 / 账号治理 / 高级 / 日志，保存即热生效 |
| **运行日志** | 请求记录表（时间 / 结果 / 模型 / 账号 / 来源 IP / UA / 耗时 / Token）+ 运行日志分频道 |

顶部另有「刷新」（向官方全量查询真实余额并回写）与「添加账号」（浏览器内完成授权登录，凭证落盘并**热加载，免重启**）。

「添加账号」对话框的「加密备份」页支持**加密导出 / 导入账号**：AES-256-GCM 加密、PBKDF2 派生密钥、每次导出随机 salt，密码不写入文件（无法找回），用于跨机迁移而不落明文凭证。

## 快速开始

### 环境要求

- **Docker + Docker Compose**（推荐）——或
- **Windows / macOS / Linux 直接跑单文件二进制**
- 一个或多个已注册的 CodeBuddy 账号，用于登录
- 宿主机 Go ≥ 1.22（仅从源码构建时需要）

### 方式一：Docker Compose（推荐）

```bash
# 1. 克隆
git clone https://github.com/BegoniaHe/wokbuddy-demon.git
cd wokbuddy-demon

# 2. 准备配置（compose 挂载此文件，缺失会导致容器启动失败）
cp config.example.json config.json
#    建议编辑 config.json 设置 api_key（或留空由程序自动生成随机密钥）

# 3. 启动（首次会构建镜像，约 1-2 分钟）
docker compose up -d --build

# 4. 健康检查（无可用账号时返回 503）
curl -s http://localhost:7863/healthz
```

启动后打开 **`http://localhost:7863/panel/`**，用面板「添加账号」完成登录。

常用运维命令：

```bash
docker compose logs -f          # 跟踪日志
docker compose restart          # 重启
docker compose down             # 停止并移除容器（数据在 ./auths 与 ./data，不受影响）
```

### 方式二：Windows 单文件运行（无需 Docker）

```powershell
# 1) 下载 nightly 构建产物（wokbuddy-demon.exe），或从源码构建
go build -trimpath -ldflags="-s -w" -o wokbuddy-demon.exe ./cmd/server

# 2) 直接运行：首次启动自动生成 config.json（含随机 api_key，日志打印一次）
.\wokbuddy-demon.exe -config config.json

# 3) 浏览器打开面板添加账号
#    http://127.0.0.1:7863/panel/
```

exe 为**单文件自包含**（前端资源已 embed 进二进制），拷到任意 Windows 机器即可运行，只需保证 `auths/`（凭证）与 `data/`（状态）目录可写。

### 方式三：源码运行（开发调试）

```bash
go build ./...
go vet ./...
go test ./...                      # 完整测试套件
go run ./cmd/server -config config.json

# 仅当改动前端 web/ 时才需要（产物 internal/panel/dist 已提交）：
make web                           # 构建前端（需 Bun）→ internal/panel/dist
make web-check                     # 前端类型检查 + 单元测试
```

### 添加账号（登录）

**方式 A：Web 面板（推荐，各平台通用，免命令行）**

打开 `http://127.0.0.1:7863/panel/`，点右上角「**添加账号**」：面板展示授权链接 → 浏览器完成登录 → 自动检测并落盘凭证 → **热加载（无需重启）**，顺带完成首次签到。

**方式 B：命令行脚本（仅 Linux / macOS，依赖 bash + python3）**

```bash
./login.sh
# 按提示在浏览器打开授权链接 → 回到终端确认 → 凭证落盘 auths/workbuddy-<uid>.json
```

> Windows 用户请用方式 A（或 WSL）；`login.sh` 需要 python3。

### 验证

```bash
# 账号状态（汇总 + 每账号详情）
curl -s http://localhost:7863/status -H "Authorization: Bearer your-api-key"

# 健康检查
curl -s http://localhost:7863/healthz

# Prometheus 指标（文本格式；鉴权与主服务同口径，api_key 为空则无需 token）
curl -s http://localhost:7863/metrics -H "Authorization: Bearer your-api-key"
```

### 客户端接入（OpenAI / Claude Code）

网关同时暴露 OpenAI 兼容与 Anthropic Messages 兼容协议面：

| 协议 | 端点 |
|---|---|
| OpenAI | `POST /v1/chat/completions`、`GET /v1/models` |
| Anthropic Messages | `POST /v1/messages`、`POST /v1/messages/count_tokens` |

Claude Code 接入示例（`api_key` 非空时填同一密钥）：

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:7863
export ANTHROPIC_AUTH_TOKEN=your-api-key
export ANTHROPIC_MODEL=claude-sonnet-4-5   # 需在 models.aliases 中映射到上游模型
claude
```

- 请求侧支持 `system`、文本/图片内容块、`tool_use` / `tool_result`、`tools`、`tool_choice`、`stop_sequences`、`thinking`；响应侧映射为 Anthropic `Message` 与 SSE 事件（`message_start` / `content_block_*` / `message_delta` / `message_stop`）。
- 上游错误转换为 Anthropic `{"type":"error","error":{...}}`，`gateway_hint` 保留在 error 对象内。
- `count_tokens` 无本地 tokenizer，按请求文本字节数近似估算，仅用于客户端上下文预算，不作计费依据。
- 未配置模型别名时，Anthropic 客户端可直接请求上游裸模型名。

### SSE 心跳

反向代理（nginx / Cloudflare 等）通常有空闲超时（常见 60–100s）。长时间思考或排队时上游可能迟迟不发帧，网关默认只透传上游帧，连接会被反代切断。设置：

```json
{ "upstream": { "keepalive_interval": "15s" } }
```

上游静默超过该间隔时，网关向客户端写一帧 `: keepalive` SSE 注释（不进入 JSON 帧白名单，不影响客户端解析）；上游持续输出时不额外插入。默认 `"0"` 关闭以保持逐字节透传行为。改后需重启。

### Prometheus 指标

`GET /metrics` 按 Prometheus 文本格式（`text/plain; version=0.0.4`）导出核心指标，可直接配置为抓取目标：

| 指标 | 类型 | 说明 |
|---|---|---|
| `wokbuddy_accounts{state}` | gauge | 分状态账号数（`ok` / `cooling` / `disabled` / `paused` / `in_flight_full`）；各 state **互斥**，`sum` 恒等于账号总数 |
| `wokbuddy_requests_total{model,result}` | counter | 按模型与结果的请求尝试数（`result` = `ok` / `error`，逐上游尝试计） |
| `wokbuddy_tokens_total{model,kind}` | counter | 按模型与类别的 Token 数（`kind` = `prompt` / `completion` / `total`） |
| `wokbuddy_request_duration_seconds{result}` | histogram | 上游尝试耗时（秒），按成功/失败分流（`result` = `ok` / `error`） |
| `wokbuddy_upstream_rotations_total{reason}` | counter | 上游换号次数（`reason` 为触发分类；degrade 同号重试不计入） |

`result` 口径：`ok` 表示本次上游尝试**拿到了 `usage`**（与面板用量账本同源），并非 HTTP 成功。客户端显式设置 `stream_options.include_usage=false` 时，即便 200 成功响应也会因无 usage 计入 `error`。

账号 / 请求 / Token 三类复用面板同一份计数（`internal/pool`、`internal/usage`），不重复记账；耗时 histogram 与换号计数为进程内生命周期，重启清零。鉴权与主服务同口径：`api_key` 非空时需 `Authorization: Bearer <api_key>`，为空则直接可抓取。

## 配置说明

**`config.example.json` 是配置项最完整的参考**：每个字段、默认值与结构都能在其中找到，示例值一律是 `test_key` 之类占位符，**不含任何真实密钥**。下表为字段含义速查。

| 字段 | 默认 | 说明 |
|---|---|---|
| `listen` | `:7863` | HTTP 监听地址 |
| `api_key` | 空 | 默认（管理）鉴权密钥，覆盖全部网关端点与面板；**空 = 不鉴权直接放行**（公网必须设置）。另可在面板「密钥」页签发多把命名密钥 |
| `auth_dir` | `./auths` | 账号凭证目录 |
| `state_file` | `./data/state.json` | 账号管家状态持久化文件 |
| `logging.request_archive_enabled` | `true` | 请求元数据归档开关 |
| `logging.request_retention_days` | `7` | 请求归档保留天数 |
| `cooldown.soft_rate` | `600s` | 软限流冷却基数；连续触发按 2 倍指数退避 |
| `cooldown.soft_rate_max` | `2h` | 软冷却指数退避封顶 |
| `upstream.rotate_max` | `3` | 单请求最多换号次数 |
| `upstream.rotate_budget` | `0` | 轮换总时长预算（如 `90s`）；`0` = 不限。与 `rotate_max` 先到先停 |
| `upstream.rotate_status_codes` | `[]` | 非空时仅这些 HTTP 状态码触发换号（如 `[429,500,502,503,504]`）；空 = 按错误分类换号 |
| `upstream.keepalive_interval` | `0` | 入站 SSE 主动心跳间隔（如 `15s`）；上游静默时写 `: keepalive` 注释帧。`0` = 关闭 |
| `upstream.proxies` | `[]` | 出站代理列表（`http://` / `https://` / `socks5://`，可带 `user:pass`）；非空按健康轮换，失败代理自动冷却。空 = 直连 |
| `schedule.checkin_hours` | `[9, 21]` | 每日整点签到 + 余额查询解冻 |
| `schedule.travel_hours` | `[9, 21]` | 每日整点推进猫猫旅行状态机 |
| `schedule.activity_hours` | `[10]` | 每日整点对话活跃上报 |
| `schedule.keepalive_hours` | `[22]` | 每日整点刷新凭证保活 |
| `schedule.blackcat_hours` | `[23]` | 每日整点夜猫子补足 |
| `schedule.*_enabled` | `true` | 五类任务各自的总开关 |
| `pool.credit_floor` | `100` | 积分保底：余额低于该值不再参与收费模型轮换 |
| `pool.breaker_threshold` | `3` | 连续失败触发熔断阈值 |
| `session_sticky.enabled` | `true` | 会话粘性开关 |
| `session_sticky.ttl` | `30m` | 会话绑定 TTL（滚动续期） |
| `models.aliases` | `{}` | 客户端模型别名 → 上游 `[realm:]model`；命中后按目标裸模型选号/计费。改后需重启 |

### 模型别名

`models.aliases` 把客户端惯用模型名映射到上游模型，免去逐客户端改配置：

```json
{
  "models": {
    "aliases": {
      "claude-sonnet-4-5": "cn:hy4",
      "gpt-5-codex": "cn:gpt-5.6-sol",
      "claude-global": "global:hy4"
    }
  }
}
```

- 键必须是裸模型名（不含 `cn:` / `global:` 前缀，大小写敏感）。
- 值沿用 `[realm:]model` 语法；省略 realm 时继承请求侧 realm（裸请求即 `cn`）。
- 先剥请求的 realm 前缀再查别名；未命中则原样透传，行为与未配置别名一致。
- 命中后选号、会话粘性、成本台账与日志统一使用解析后的裸模型名；`GET /v1/models` 会追加别名条目并附带 `upstream_id`。
- 别名在进程启动时装载，修改后需重启。

### 环境变量覆盖

加载顺序：JSON 文件 → `WB2A_*` 环境变量（变量非空才覆盖）：

`WB2A_LISTEN` · `WB2A_API_KEY` · `WB2A_AUTH_DIR` · `WB2A_STATE_FILE` · `WB2A_SOFT_RATE` · `WB2A_TIMEOUT_SECONDS` · `WB2A_PROMPT_MODE` …

## 部署运维

### Docker 镜像

多阶段镜像（`golang:1.23-alpine` 构建 → `alpine:3.20` 运行）一次编译全部四个二进制并随镜像分发：`wokbuddy-demon`（主服务）、`signin_bin`、`login`、`credit` + 工具脚本。以 `app` 用户（uid 10001）运行，内置 `HEALTHCHECK`。

### 工具脚本

| 脚本 | 用途 |
|---|---|
| `./login.sh` | 登录 → 落盘凭证 → 重启容器 |
| `./signin.sh [auths_dir]` | 批量签到（过期先刷新） |
| `./credit.sh` / `./credit.sh -json` | 积分日报（美化 / 原始 JSON） |
| `go run ./cmd/migrate-auths -dir auths` | 迁移旧版 snake_case 格式 auth 文件（预览；加 `-apply` 落盘） |
| `python3 scripts/probe_active.py` | 活跃上报手动诊断 / 补跑 |
| `python3 scripts/probe_max_tokens.py` | 探测各模型真实输出上限 |

### 账号管理

- 多账号复制 `auths/workbuddy-<uid>.json` 即可，管家启动时自动对齐目录
- 旧版 snake_case 扁平形（`access_token` / `expires_at` 毫秒）不会被载入：`go run ./cmd/migrate-auths -dir auths -apply` 转换，原文件留存 `.legacy` 副本
- 失效账号被禁用后，可用 `./login.sh` 重新登录覆盖凭证
- 备份 = `auths/`（凭证）+ `data/state.json`（状态）

## 安全与合规

### 1. 凭据管理（auths）

- **位置**：`./auths`（`auth_dir` 可配），文件名 `workbuddy-<uid>.json`
- **内容**：明文 `accessToken` / `refreshToken` + 账号元信息
- **权限**：容器内以 `app` 用户运行；刷新由 `SaveAtomic` 以 `0600` 原子写回；建议手动 `chmod 600 auths/*.json`
- **切勿提交 git**：`.gitignore` 已排除 `auths/`、`data/`、`backups/`、`config.json`、`*.key`、`*.pem`、`*.env`

### 2. 网络暴露与日志敏感度

- 默认监听 `:7863`，**无内置 TLS**；公网部署必须设置 `api_key`，建议置于 HTTPS 之后 / 仅内网可达
- 日志字段不含任何 token 明文（不读取 `Authorization` 头）
- 日志写 **stdout / stderr**，代码无任何落盘日志文件

### 3. 使用边界

- 仅限**本人授权账号**、本机 / 私有环境使用
- 遵守 CodeBuddy 平台服务条款与所在地法律
- 妥善保管 `auths/`（明文凭证）与面板端口

## 常见问题

### 账号被禁用后如何恢复？

- **用 `./login.sh` 重新登录**覆盖凭证，重启后自动回池；
- 或源码侧调用 `Pool.ReviveDisabled(uid)` 清除 `disabled` 状态。

### Docker 部署登录后报「写入 auths/…json.tmp 失败： permission denied」？

容器以 `app` 用户（uid 10001）运行，而宿主机挂载目录属主不是它。推荐让容器以你自己的 uid 运行：

```bash
PUID=$(id -u) PGID=$(id -g) docker compose up -d --force-recreate
```

### 面板需要鉴权吗？

`api_key` 非空时面板要求输入一次密钥（浏览器记住）；为空则直接可用。**公网部署务必设置 `api_key` 并置于 HTTPS 之后。**

### 多密钥（分钥匙给多个客户端）？

面板「密钥」页可创建多把**命名密钥**（如 `sk-wb-xxxx…`），分发给不同客户端，并可逐把撤销或删除。规则：

- 命名密钥以 **SHA-256 哈希**存储（`data/api_keys.json`），明文**仅在创建时回显一次**；
- 命名密钥**只作用于网关 API 面**（`POST /v1/chat/completions`、`POST /v1/messages`、`POST /v1/messages/count_tokens`、`GET /v1/models`）；
- 面板、`GET /status`、`GET /metrics` 仍**只认**默认 `api_key`（命名密钥是给客户端的，不获得管理/运维可见性）；
- 每把密钥记录 `last_used_at` 与请求数 / token 用量，面板可查看；
- `api_key` 为空且未创建任何命名密钥时，网关仍为「不鉴权」放行（既有默认）。

本功能**不涉及任何计费**：不设额度、不设配额、不设账单。

## 免责声明

本项目仅供学习和研究使用。使用者需遵守 CodeBuddy 服务条款，自行承担使用风险（包括账号封禁、条款违约等）。作者不对任何因使用本项目产生的直接或间接损失负责。

## License

本项目采用 [MIT License](LICENSE) 开源协议。

- 允许任意使用、复制、修改、合并、发布、分发、再授权及销售
- 再分发时请保留原仓库的 MIT 版权声明与许可声明
- 本项目不授予任何 CodeBuddy / 腾讯接口或服务的权利；使用者仍需自行遵守其服务条款
