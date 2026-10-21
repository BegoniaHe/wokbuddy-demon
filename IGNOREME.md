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

### 支持、反馈与传播约定

- **不追求 Star**：本项目不以 Star 数作为目标，请不必点 Star，把关注留给真正需要曝光的项目。
- **表达喜欢请开 Issue**：如愿意表达支持，可提交 Issue 并带上 `star` 标签；若当前账号无法添加标签，在标题或正文注明 `star` 即可。
- **保持低调**：请不要在公开场合或社交平台宣传本项目，也不要通过截图、教程或推荐列表扩大传播。
- **沟通语言**：维护者的中文表达可能不够准确。若担心产生歧义，欢迎使用英语或德语（Issues auf Deutsch sind immer bevorzugt.）；其他语言同样可以，只要维护者使用的 AI 工具能够理解。
- **共同目标**：保持低调，让项目能够长期开源并持续维护。

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
| 🛡️ **熔断与冷却** | 429 软冷却 600s 起指数退避（±10% 抖动）、404 固定 60s 短冷却、402 硬冷却至次日 04:00、连续失败熔断 |
| 🧲 **会话粘性** | 同一会话尽量绑定同一账号，TTL 滚动续期；负载/健康变差时软逃逸并降级 20s，失败自动解绑 |
| ⏰ **定时任务** | 签到（09/21 点）+ 活跃上报（10 点）+ 猫猫旅行（09/21 点）+ 保活（22 点）+ 夜猫子（23 点），各自独立开关 |
| 🎯 **成长任务一键完成** | 18 个成长任务中 **17 个一键纯自动化**，推进进度、等待计分落定并**自动领奖** |
| 🎲 **连登管家** | 签到末尾自动兑换连登档位 + 抽完抽奖次数，全程无需人工盯 |
| 📊 **可观测** | 每请求一行表格日志（TTFB / 速率 / uid）；`/healthz` 带身份标识便于宿主探活；`/metrics` 暴露 Prometheus 文本指标 |
| 💾 **状态持久化** | 状态本地原子落盘 + 可选 Redis 异步镜像，重启择新恢复 |
| 🖥️ **Web 管理面板** | 内嵌面板（React + Tailwind，明暗主题，响应式，七大视图）：账号运维 / 用量分析 / 积分构成 / 任务中心 / 模型档位 / 在线配置 / 运行日志 |

## 🎯 成长任务一键完成（17/18）

官方「成长计划」的 18 个成长任务中，**17 个可在面板上一键完成**——无需安装官方客户端、无需人工交互，点一下「一键完成」即自动推进进度、等待异步计分落定并**自动领奖**。剩余 1 个任务展示操作指引。

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
| `black_cat` | — | 夜猫子：23:00–08:00 窗口内 glm-5.2 对话补足；窗口外提示稍后再试 |

**官方 18 个成长任务中：上表 17 个可一键完成（含夜猫子窗口任务），剩余 1 个需人工操作。** 小程序 `Sequential_Tasks_1`–`7` 与开学季任务属于独立活动口径，不在官方 18 个成长任务内，在任务中心另行执行。

**全新账号一键全做完 ≈ +1950 credits +78 能量**，其中仅数个任务涉及真实对话，其余全部为行为事件上报，零对话消耗。

### 不可自动的 1 个

| 任务 | 原因 |
|---|---|
| `Expert_Philanthropy` | 需真实捐款（服务端领奖时校验捐赠回执，已实测无法绕过） |

> ⚠️ 行为事件按天幂等：重复点「一键完成」不会重复扣资源，已达标的任务自动跳过。

### 🧭 任务中心

「积分构成」页提供只读「获取历史」：按发放时间展示全部积分包、推断来源与可用/用完/过期状态；数据来自现有积分包快照，不额外请求上游。

「任务中心」视图把散落的任务能力收拢成一处：

- **全账号任务扫描**：一键拉取每个账号的成长任务 + 开学季待办，列表一目了然
- **执行队列**：把待办按账号排队执行——账号内串行，账号间可选并发（1-3）；进度实时更新。队列有独立单飞守卫，不与账号管家的六个批量任务族互斥（两者可同时运行）
- **开学季独立状态卡**：每账号 5 任务的状态矩阵 + 剩余抽奖次数，一键触发全账号闭环
- **日志分频道**：运行日志按「任务 / 对话 / 系统」三个频道筛选——任务结果不会被对话流量冲掉

## 🐱 猫猫旅行 & 连登管家

**猫猫旅行**（独立排程，默认 09/21 点各一趟）：无猫先领养，`idle` 派出，`arrived` 领奖；每自然日一次派出，单账号失败只跳过当趟。

**连登管家**（挂在每日签到末尾，幂等）：成长中心连登档位（连续登录 7/14/28 天）解锁后自动兑换，抽奖次数自动全部抽完，奖品记日志。到天数那天自动完成「兑换 → 抽奖」，无需人工盯。

国际版（global）账号同样执行每日 `daily-checkin`（官方国际版客户端同路径，优先无 `/v2` 前缀），重复执行由上游幂等拒绝，不会重复领取；活跃上报仍独立执行，用于点亮连登。

## 🖥️ Web 管理面板

内嵌式管理面板（React 19 + Tailwind 4 构建，产物打进二进制；`go build` 本身无需 Node/Bun），服务启动后访问：

```
http://127.0.0.1:7863/panel/
```

鉴权与主服务同口径：`api_key` 非空时面板要求输入一次密钥（浏览器 localStorage 记住）；为空则直接可用。
界面支持**明暗主题切换**（首次跟随系统偏好），左侧导航八个视图：

| 视图 | 功能 |
|---|---|
| **账号管家** | 统计条 + 账号表：健康色条与状态标签、积分量条、成功·失败计数、最近用量；单号操作 签到 / 余额 / 任务 / 解冻 / 禁用 / 移除；批量「全部签到」「旅行巡检」「活跃上报」「全部保活」「夜猫子」「连登管家」——同一任务族单飞，运行中按钮禁用，可查当前/最近一轮账号数与成功失败摘要；任务中心的成长队列不在其中（独立单飞） |
| **用量** | 指标卡（含首字延迟 TTFT 均值）+ 日历热力图（按日聚合，可切 Token/请求数/积分）+ 趋势图（可切 Token/请求数/失败数/TTFT/积分）+ 用量明细（按账号 / 按模型 / 按域切换，可排序）+ 积分扣除历史；「回填上游」按账号拉取官方逐笔用量并按天覆盖本地桶用于对账（7/30 天可选） |
| **积分构成** | 积分到期分布 + 账号对比卡片 + 逐包明细（按到期日排序） |
| **任务中心** | 成长任务队列：一键扫描全部账号待办 → 按账号排队执行，进度实时更新 |
| **模型与档位** | 实时查询每模型倍率 / 思考档 / 上下文长度与最大输出，支持按条件查询与排序；可对单个模型「探测」首字延迟（TTFT），显示最近一次与滚动均值/p95，历史写入 `data/model_probes.json` |
| **密钥** | 命名 API 密钥管理：创建（明文仅显示一次）/ 撤销 / 删除，并按密钥查看请求数与 token 用量 |
| **配置** | 在线编辑配置：服务 / 定时任务 / 账号治理 / 上游身份与代理 / global / 提示词 / Upstash / 路径 / 日志；热字段即时生效，装配期字段提示重启 |
| **运行日志** | 请求记录表（时间 / 结果 / 模型 / 账号 / 来源 IP / UA / 耗时 / Token）+ 运行日志分频道 |

顶部另有「刷新」（向官方全量查询真实余额并回写）与「添加账号」（浏览器内完成授权登录，凭证落盘并**热加载，免重启**）。

「添加账号」对话框的「加密备份」页支持**加密导出 / 导入账号**：AES-256-GCM 加密、PBKDF2 派生密钥、每次导出随机 salt，密码不写入文件（无法找回），用于跨机迁移而不落明文凭证。

「本机客户端」页可**只读扫描**已登录官方客户端的凭据目录（macOS `~/Library/Application Support/WorkBuddy{ AI}/Data/Public/auth/*.info`、Windows `%LOCALAPPDATA%\WorkBuddy{ AI}\...`、Linux `~/.local/share/WorkBuddy{ AI}/...`，以及 `~/.workbuddy`）。客户端加密字段为 `$wbEncrypted`（AES-256-GCM，字段级 AAD）；需在面板输入客户端的 at-rest secret（32 字节 base64），密钥仅在本次导入的内存中使用，不落盘、不写日志，响应只返回账号元数据。拿不到密钥时请改用浏览器登录。

### 多模态能力

`/v1/models` 对已知能力的模型输出 `input_modalities`（`text` / `image` / `audio` / `file`），并保留 `supports_images` 兼容字段。能力来源优先级：上游模型对象字段 → `models.dev` 缓存 → `supports_images` 推导；三级都没有时视为未知，不宣称也不拦截。

聊天请求里的 `image_url` / `input_audio` / `file` part 会参与能力判定：模型明确不支持该模态时默认返回 400 `model_capability`（`gateway_hint` 附可用模型），不把请求静默送给不支持的上游。设置 `features.multimodal_routing=true` 后改为在同域模型目录内改选支持该模态的模型，并重写请求的 `model`（日志记录改选结果）；改选后的模型仍需通过命名密钥的模型白名单。纯文本请求的路由不受影响。

## 快速开始

### 环境要求

- **Docker + Docker Compose**（推荐）——或
- **Windows / macOS / Linux 直接跑单文件二进制**
- 一个或多个已注册的 CodeBuddy 账号，用于登录
- 宿主机 Go ≥ 1.27（仅从源码构建时需要）

### 方式一：Docker Compose（推荐）

```bash
# 1. 克隆
git clone https://github.com/BegoniaHe/wokbuddy-demon.git
cd wokbuddy-demon

# 2. 准备配置（compose 挂载此文件，缺失会导致容器启动失败）
cp config.example.json config.json
#    示例里的 api_key 是公开占位值 test_key；程序启动时会自动替换为随机密钥
#    并写回该文件（显式设置的其他值不会被覆盖）。也可先自行改成自己的密钥。

# 3. 启动（首次会构建镜像，约 1-2 分钟）
docker compose up -d --build

# 4. 健康检查（无可用账号时返回 503）
curl -s http://localhost:7863/healthz
```

Compose 默认把端口绑定到 `127.0.0.1`，仅本机可达（面板与网关无内置 TLS）。需要局域网访问时，把 `docker-compose.yml` 的 ports 改为 `"7863:7863"` 或指定内网 IP：

```yaml
ports:
  - "192.168.1.10:7863:7863"
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

### 校验 nightly 产物

nightly 的每个二进制压缩包同目录附带 `.sha256`，下载后校验：

```bash
# Linux
sha256sum -c wokbuddy-demon-nightly-*.sha256

# macOS（文件名在文件里，需先进入下载目录）
shasum -a 256 -c wokbuddy-demon-nightly-*.sha256
```

校验和覆盖压缩包本身；容器镜像的完整性由 GHCR digest 提供。

### 方式三：源码运行（开发调试）

```bash
go build ./...
go vet ./...
go test ./...                      # 完整测试套件
go run ./cmd/server -config config.json
go install golang.org/x/vuln/cmd/govulncheck@latest
govulncheck ./...                  # 依赖已知漏洞扫描（CI 同口径）

# 解析管线 fuzz（nightly CI 短跑；本地可调长时间）
go test ./internal/upstream -run=^$ -fuzz=FuzzPrepareBodyOpt -fuzztime=30s
go test ./internal/upstream -run=^$ -fuzz=FuzzNormalizeFrame -fuzztime=30s
go test ./internal/upstream -run=^$ -fuzz=FuzzHandleStreamLine -fuzztime=30s

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
| OpenAI Chat Completions | `POST /v1/chat/completions`、`GET /v1/models`、`GET /v1/models/{model}` |
| OpenAI Responses | `POST /v1/responses`（流式/非流式文本、function/custom tools、结构化输出；reasoning/compaction 等状态 item 忽略） |
| Anthropic Messages | `POST /v1/messages`、`POST /v1/messages/count_tokens`、`GET /v1/models`（按 `anthropic-version` 头返回 Anthropic 形态） |
| Gemini GenerateContent | `POST /v1beta/models/{model}:generateContent`、`:streamGenerateContent`、`:countTokens`（也接受 `/v1`、`/v1alpha` 版本前缀） |

Codex CLI 等使用 Responses API 的客户端：

```bash
export OPENAI_BASE_URL=http://127.0.0.1:7863/v1
export OPENAI_API_KEY=test_key   # 换成 config.json 中生效的 api_key
codex
```

- `/v1/responses` 会合并 `input` 里的 `additional_tools`（Codex 0.162+ 的工具下发形态之一），展开 `namespace` 工具并在回程补回 `namespace`；客户端未传 `max_output_tokens` 时按模型目录补全 `max_tokens`，避免上游回落到默认 32k 输出预算截断长推理。

Claude Code 接入示例（`api_key` 非空时填同一密钥）：

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:7863
export ANTHROPIC_AUTH_TOKEN=your-api-key
export ANTHROPIC_MODEL=claude-sonnet-4-5   # 需在 models.aliases 中映射到上游模型
claude
```

- 鉴权同时接受 `Authorization: Bearer` 与 Anthropic SDK 的 `X-Api-Key`，两者走同一常量时间比较。
- 请求侧支持顶层 `system` 与 `system` role 消息、文本/图片/文档内容块、`tool_use` / `tool_result`（`is_error` 会在 tool 内容前加 `[tool_error]` 标记）、`tools`（客户端工具，`type` 可省略或为 `custom`，`strict` 透传）、`tool_choice`（含 `disable_parallel_tool_use`）、`stop_sequences`、`thinking`（含 `adaptive` / `between_tools` / `display`）；服务端工具与其余无法映射的内容块返回明确 400；`redacted_thinking` 无可见内容，跳过而不失败。文档 `source.type=url` 以 `file.file_url` 透传，取决于上游是否接受该字段。
- 并行 `tool_calls` 按工具 index 缓冲；文本/思考恢复或新工具批次出现时按顺序刷新已完整的批次，收尾时写出剩余块，避免交错分片拆坏 `tool_use`，也避免 trailing text 排到工具调用之前。参数在批次落地时整块下发，不再逐片产生 `input_json_delta`；无参工具（`arguments:""`）的批次要等 `finish_reason` / `[DONE]` 才落地，其后的正文也随之后发。上游报错或截断时先落地已生成内容再发 error 事件（截断的参数只记 WARN）。响应侧映射为 Anthropic `Message` 与 SSE 事件（`message_start` / `content_block_*` / `message_delta` / `message_stop`）。
- 明确拒绝：服务端工具结果块、`tool_reference` / `browser_state`、`container` / `diagnostics` / `inference_geo`；块内的 `citations` 等无对应字段静默忽略；`messages` 上限 100,000 条，`max_tokens` 下限 0。
- system、text、image、document 与 tool_result 块上的 `cache_control` 会保留到出站内容，供上游 prompt cache 使用；`max_tokens=0`（缓存预热）按 0 写出，但 GPT 系模型仍会被上游下限钳到 16（见 `clampGPTMinMaxTokens`）。
- `top_k` / `speed` / `service_tier` / `metadata`（`user_id` 上限 512）与顶层 `cache_control` 原样写进上游 body，是否生效取决于上游；`temperature` / `top_p` 限定在 `[0,1]`，`top_k` 不得为负，越界 400。
- `stop_sequences` 命中已知后缀时返回 `stop_reason=stop_sequence` 与匹配序列；上游未回传命中序列时保持 `end_turn`。
- 流结束时校验各工具块聚合后的 `input_json_delta` 是否为合法 JSON；截断/非法时记 WARN（不含参数原文），不改写分片。合法但不是对象的参数（`null` / `[]` / 数字等）与非流式路径同口径归一成 `{}`，空/纯空白参数不下发 `input_json_delta`。
- 上游错误优先采信帧内已有的 Anthropic `error.type`，否则按 `FrameKind` 转换为 Anthropic error.type（限流/过载/余额等）；判不出类别的帧与网关本地兜底帧一律 `api_error`，不臆造成客户端参数错误。`gateway_hint` 保留在 error 对象内；响应带 `Request-Id`，错误体顶层 `request_id` 与响应头一致，供 SDK 读取。
- Messages 端点方法不匹配（如 `GET /v1/messages`、`GET /v1/messages/count_tokens`）返回 `405` + `Allow: POST` 的 Anthropic 错误体（改前是 404 `not_found_error`）；CORS 允许头补上 `Anthropic-Beta` / `Anthropic-Dangerous-Direct-Browser-Access`，并暴露 `Request-Id`。
- `count_tokens` 无本地 tokenizer，按 prompt 文本字节数近似估算（跳过模型名与 base64 载荷），要求 `messages` 非空，仅用于客户端上下文预算，不作计费依据。
- 未配置模型别名时，Anthropic 客户端可直接请求上游裸模型名。

Gemini CLI（GATEWAY 模式）接入：

```bash
export GOOGLE_GEMINI_BASE_URL=http://127.0.0.1:7863
export GEMINI_API_KEY=your-api-key   # 可用 x-goog-api-key 头，空 api_key 时留空
gemini
```

- 支持 `contents`、`systemInstruction`、functionDeclarations / functionCall / functionResponse、图片 `inlineData`、`generationConfig`（采样参数、停用词、结构化输出、thinkingLevel/thinkingBudget）与 `toolConfig.functionCallingMode`。
- 响应映射为 Gemini `GenerateContentResponse` / SSE 分片；流式不输出 `[DONE]`。`countTokens` 与 Anthropic 一致按文本字节近似。
- Google 内置工具（googleSearch / codeExecution 等）、embedContent 与多模态文件（PDF/音频/视频）暂不代跑；相关声明或内容会被忽略。

### SSE 心跳

反向代理（nginx / Cloudflare 等）通常有空闲超时（常见 60–100s）。长时间思考或排队时上游可能迟迟不发帧，网关默认只透传上游帧，连接会被反代切断。设置：

```json
{ "upstream": { "keepalive_interval": "15s" } }
```

上游静默超过该间隔时，网关向客户端写一帧 `: keepalive` SSE 注释（不进入 JSON 帧白名单，不影响客户端解析）；上游持续输出时不额外插入。默认 `"0"` 关闭以保持逐字节透传行为。改后需重启。

### 官方客户端协议对齐

网关按 WorkBuddy Desktop CN `5.8.3` / global `5.8.4` 的静态包内行为对齐协议：

- **CN origin**：默认主域 `www.workbuddy.cn`；chat / refresh / 登录在首 token 前遇到传输错误或 `408/502/503/504` 时依次回落 `copilot.tencent.com`、`wb.tencentbuddy.com`。显式覆盖 chat base 时只用该值。
- **身份头**：企业账号同时发送 `X-Enterprise-Id` 与 `X-Tenant-Id`；个人 global 账号发送 `X-No-Enterprise-Id: 1`。账号带 `domain` 时透传为 `X-Domain`，global 缺省回落 `www.workbuddy.ai`。
- **入站会话元数据**：`X-Conversation-ID`、`X-Root-Request-ID`、`X-Parent-Conversation-ID`、`X-Agent-Intent/Purpose/Type`、`X-Compaction`、`X-Context-Compacted` 原样透传；缺失时保持网关默认。
- **401 重放**：上游 chat 返回 401 时，同账号刷新一次 token 并重放同一请求；再次 401 回到既有轮换策略。refresh 端点 401/403 视为会话失效，按 keepalive 阈值禁用。
- **SSE**：兼容 `delta.reasoning`（归一为 `reasoning_content`）、重复 `tool_calls[].id` 去重；已收到 `finish_reason` 后 EOF 视为正常完成。只有 reasoning、没有正文/工具调用/拒答的流不记成功。
- **队列与额度业务码**：`6020` 返回 429 + `Retry-After`（不换号）；`6021/6022` 返回容量/并发错误（不轮转）；`11145/14001/14002/14012-14014/14018/14019` 归为额度耗尽，`10105/15001` 按账号级使用上限走软限流，`14015` 会话失效，`14016/14017` 账号未激活，`11142` 账号级认证故障。
- **错误码分类**：`11125/11129/11130/11132/11137-11139/11148-11155/12654/14405` 为请求级错误，不罚号、不轮转；`11102` 模型目录不存在直接 `404` fail-fast，`11136` 换号并写入 (账号,模型) 负缓存（不写路由熔断），`11141` 模型下线直接 fail-fast；`11-128` 入站按内容拦截处理；`6000-6008` 按限流处理（`6004` 保持模型级冷却）。HTTP 200 + 非零业务码与 SSE error 帧均按帧内/结构化 `code` 分类。
- **签到 / 余额**：两 realm 均优先 `/v2/billing/meter/daily-checkin`，404 回落旧路径；`1001` 视为已签到，`1002/1003` 视为活动未开启。余额以 `get-user-resource-summary` 为主，失败回落 `get-user-resource`，并解析精确容量、切片额度与到期字段。
- **模型目录**：应用 `availableModels` / `disabled` / `aliases` 过滤；CN 在 `/console` 无 CLI 过滤集时回落 `/v2/enterprises/personal/models`，`/v3/config` 同时使用桌面端与 IDE UA。上游 alias 参与请求解析，`models.aliases` 配置优先。
- **growth / travel**：Bearer 通道优先 `/v2/activity/growth/*`，404 回落旧路径；travel claim 支持带 `record_id` 与空 body 两种形态，并发送 `X-Product-Code: workbuddy`。
- **桌面指纹**：`X-Domain` 使用 host 形态；企业账号上报事件带 `enterpriseId` / `enterprise`；不伪造 `qimei36`。

### Webhook 告警

配置 `notify.webhook_url` 后，以下关键事件会以 POST JSON 异步推送（同事件 + 账号 10 分钟内去重，失败只记日志，不影响请求与任务）：

- `session_dead`：账号 session 失效被禁用（需重新登录）
- `pool_unavailable`：请求时池内无可用账号（健康/在途名额耗尽或模型级冷却）
- `credit_low`：余额低于 `pool.credit_floor`
- `waf_ip_blocked`：出口 IP 触发上游 WAF 级拦截
- `task_failed`：签到 / 旅行 / 活跃 / 保活任务失败

```json
{
  "notify": {
    "webhook_url": "https://example.com/hook",
    "events": ["session_dead", "credit_low"]
  }
}
```

载荷固定字段：`event`、`time`（RFC3339）、`uid`（仅 uid8）、`nickname`、`realm`、`summary`、`detail`。**不发送 token、完整 UID、请求体或凭证**；`webhook_url` 可内嵌 token，日志只记事件名与状态码。

### 错误与重试

网关在限流/无可用账号等可恢复错误上返回标准 `Retry-After` 头（秒）：

- `429`：取账号级软冷却或模型级冷却的最早到期时间；上游明示 `Retry-After` 时一并对齐。
- `503`：仅在能确定模型级最早解封时间时携带。
- 无法确定可靠时间时不携带该头，客户端应使用自身退避策略。错误 JSON body 与 `gateway_hint` 保持原格式不变。

### Prometheus 指标

`GET /metrics` 按 Prometheus 文本格式（`text/plain; version=0.0.4`）导出核心指标，可直接配置为抓取目标：

| 指标 | 类型 | 说明 |
|---|---|---|
| `wokbuddy_accounts{state}` | gauge | 分状态账号数（`ok` / `cooling` / `disabled` / `paused` / `in_flight_full`）；各 state **互斥**，`sum` 恒等于账号总数 |
| `wokbuddy_requests_total{model,result}` | counter | 按模型与结果的请求尝试数（`result` = `ok` / `error`，逐上游尝试计） |
| `wokbuddy_tokens_total{model,kind}` | counter | 按模型与类别的 Token 数（`kind` = `prompt` / `completion` / `total`） |
| `wokbuddy_request_duration_seconds{result}` | histogram | 上游尝试耗时（秒），按成功/失败分流（`result` = `ok` / `error`） |
| `wokbuddy_upstream_rotations_total{reason}` | counter | 上游换号次数（`reason` 为触发分类；degrade 同号重试不计入） |
| `wokbuddy_pool_in_flight` | gauge | 当前全局在途上游请求数（所有账号租约之和） |
| `wokbuddy_pool_cooling` | gauge | 冷却中或熔断中的账号数 |
| `wokbuddy_model_ttft_ms{model}` | gauge | 按模型聚合的账号 TTFT EWMA 均值（ms）；只统计最近 30 分钟有观测的条目 |

`result` 口径：`ok` 表示本次上游尝试**拿到了 `usage`**（与面板用量账本同源），并非 HTTP 成功。客户端显式设置 `stream_options.include_usage=false` 时，即便 200 成功响应也会因无 usage 计入 `error`。

账号 / 请求 / Token 三类复用面板同一份计数（`internal/pool`、`internal/usage`），不重复记账；耗时 histogram、换号计数与路由熔断为进程内生命周期，重启清零。

同一 `(realm, model)` 在 60 秒内出现 3 次平台级模型失败（11141 模型下线 / 6004 模型级限流）后，整条路由冷却 5 分钟；期间选号直接返回不可用（`503`，可带 `Retry-After`），不再逐号重试消耗配额。任一账号在该组合上成功即立即清除。

11136（账号无该模型权限）是账号维度：只写 `(账号, 模型)` 负缓存并换号，不触发路由冷却，避免少数无权限账号饿死仍有权限的账号。

同一 realm 内所有账号都对该模型处于模型级冷却时：11136/11141（不可用）返回 `400 model_unavailable`，6004（限流）返回 `429 rate_limit_exceeded` 并带 `Retry-After`。跨 realm 的健康号不参与本域判定。

上游 11102（模型目录无此模型）是请求级事实，不参与路由冷却：首次尝试即返回 `404 model_not_found`，不逐号重试。

鉴权与主服务同口径：`api_key` 非空时需 `Authorization: Bearer <api_key>`，为空则直接可抓取。

运行时性能分析走标准库 `net/http/pprof`：`/debug/pprof/`（含 `cmdline` / `profile` / `symbol` / `trace` 与各命名 profile）。与 `/metrics` 同口径鉴权，`api_key` 为空时同样放行——公网部署必须设置 `api_key`，否则 goroutine 栈与调用细节会暴露。示例：`curl -H "Authorization: Bearer <api_key>" -o heap.pprof http://127.0.0.1:7863/debug/pprof/heap && go tool pprof heap.pprof`。

## 配置说明

**`config.example.json` 是配置项最完整的参考**：每个字段、默认值与结构都能在其中找到，示例值一律是 `test_key` 之类占位符，**不含任何真实密钥**。启动时若 `api_key` 仍精确等于占位值 `test_key`，程序会生成随机密钥并原子写回配置文件；显式设置的其他值保持不变。检查生效值：`grep '"api_key"' config.json`（不要分享该输出）。下表为字段含义速查。面板保存后返回的「需重启」字段只包含本次实际变化的装配期字段；未提示的字段已热生效。面板「配置」页覆盖服务、定时任务、账号池、上游身份与代理、global、提示词、Upstash、路径与日志字段；设备 Token 与 Upstash Token 以密码框展示，已设置时留空保存即保持原值。

| 字段 | 默认 | 说明 |
|---|---|---|
| `listen` | `:7863` | HTTP 监听地址 |
| `api_key` | 空 | 默认（管理）鉴权密钥，覆盖全部网关端点与面板；**空 = 不鉴权直接放行**（公网必须设置）。另可在面板「密钥」页签发多把命名密钥 |
| `auth_dir` | `./auths` | 账号凭证目录 |
| `state_file` | `./data/state.json` | 账号管家状态持久化文件 |
| `logging.request_archive_enabled` | `true` | 请求元数据归档开关 |
| `logging.request_retention_days` | `7` | 请求归档保留天数 |
| `logging.decision_trace` | `false` | 请求归档记录选号决策链（候选/排除原因/成本层/Top-N/轮换） |
| `logging.decision_trace_sample` | `0.1` | 决策链采样率：0/1 = 全部，0<v<1 按比例。改后需重启 |
| `notify.webhook_url` | 空 | 告警 webhook（http/https）；空 = 关闭 |
| `notify.events` | `[]` | 订阅事件；空 = 默认关键事件（session_dead / pool_unavailable / credit_low / waf_ip_blocked / task_failed） |
| `server.read_timeout` | `300s` | 入站请求读取（含 body 上传）总时长上限；`0` = 不限制。改后需重启 |
| `server.max_body_bytes` | `33554432` | 聊天类入站请求体上限（字节，32 MiB，对齐官方桌面端 VLM 代理），覆盖 `/v1/chat/completions`、`/v1/messages` 与 count_tokens；超限返回 413（OpenAI 面 `payload_too_large`，Anthropic 面 `request_too_large`）；`0` = 不限制。改后需重启 |
| `server.request_accept_grace_seconds` | `0` | 停机摘流宽限（秒）：收到 SIGTERM 后 `/healthz` 立即 503，listener 继续受理该时长再关闭；`0` = 立即停机。改后需重启 |
| `server.shutdown_grace_seconds` | `30` | listener 关闭后等待在途 SSE 流排空的上限（秒）；`0` 回落 30，负值报错。改后需重启 |
| `server.cors_origins` | `[]` | 允许跨域访问 `/v1/*` 的 Origin 列表（如 `["http://localhost:3000"]`）；空 = 关闭。仅精确匹配，不支持 `*`；管理面不开放 |
| `server.trusted_proxies` | `["127.0.0.1/32","::1/128"]` | 可信反向代理网段（CIDR 或单个 IP）。仅当 TCP 对端命中时才采信 `X-Real-IP` / `X-Forwarded-For`；反代在另一台机器时显式加入其网段。**反代必须自行覆盖或追加** `X-Forwarded-For`，透传客户端自带值等于让客户端伪造来源。改后需重启 |
| `cooldown.soft_rate` | `600s` | 软限流冷却基数；连续触发按 2 倍指数退避 |
| `cooldown.soft_rate_max` | `2h` | 软冷却指数退避封顶 |
| `upstream.model_timeouts` | `{}` | 按模型覆盖 `header` / `idle` 超时（如 `{"cn:glm-5.2":{"header":"300s","idle":"600s"}}`）；未命中回落全局。改后需重启 |
| `upstream.rotate_max` | `3` | 单请求最多换号次数 |
| `upstream.rotate_budget` | `0` | 轮换总时长预算（如 `90s`）；`0` = 不限。与 `rotate_max` 先到先停 |
| `upstream.rotate_status_codes` | `[]` | 非空时仅这些 HTTP 状态码触发换号（如 `[429,500,502,503,504]`）；空 = 按错误分类换号 |
| `upstream.keepalive_interval` | `0` | 入站 SSE 主动心跳间隔（如 `15s`）；上游静默时写 `: keepalive` 注释帧。`0` = 关闭 |
| `upstream.header_timeout_seconds` | `1200` | 聊天 SSE 首字节前上限（秒，对齐官方客户端）；`0` = 默认 1200。`upstream.model_timeouts` 优先 |
| `upstream.idle_timeout_seconds` | `1200` | 聊天 SSE 流中空闲上限（秒，对齐官方客户端）；活跃吐数据会续命；`0` = 默认 1200。`upstream.model_timeouts` 优先 |
| `upstream.proxy_slots` | `[]` | 命名出站代理槽（`id` / `name` / `url` / `enabled`）；账号可在面板绑定，绑定后使用独立 Transport，未绑定走全局池/直连。改后需重启 |
| `upstream.proxies` | `[]` | 出站代理列表（`http://` / `https://` / `socks5://`，可带 `user:pass`）；非空按健康轮换，失败代理自动冷却。空 = 直连 |
| `cn.chat_base` | 空 | 国内版 upstream chat base 覆盖（如指向自建/自选入口）。空 = 默认 `https://www.workbuddy.cn`，并在首 token 前按 `408/502/503/504` 或传输错误回落 `copilot.tencent.com`、`wb.tencentbuddy.com`；**非空时只用该值、不回落**（与 `global.chat_base` 同语义）。改后需重启 |
| `features.local_web_tools` | `false` | 允许 `/v1/responses` 把 `web_search` / `web_fetch` 服务端工具交给网关本地执行；每个工具轮次额外消耗一次上游额度 |
| `features.local_web_tool_rounds` | `3` | 本地工具最多代跑轮数，封顶 8 |
| `features.multimodal_routing` | `false` | 请求带图/音频/文件而当前模型能力不匹配时：`false` 返回 400 `model_capability`；`true` 在同域目录内改选支持该模态的模型并重写 `model` |
| `schedule.checkin_hours` | `[9, 21]` | 每日整点签到 + 余额查询解冻 |
| `schedule.travel_hours` | `[9, 21]` | 每日整点推进猫猫旅行状态机 |
| `schedule.activity_hours` | `[10]` | 每日整点对话活跃上报 |
| `schedule.keepalive_hours` | `[22]` | 每日整点刷新凭证保活 |
| `schedule.blackcat_hours` | `[23]` | 每日整点夜猫子补足 |
| `schedule.checkin_account_delay` | `800ms` | 签到任务账号间等待；`0` = 不限 |
| `schedule.*_enabled` | `true` | 五类任务各自的总开关 |
| `limits.daily_tokens` | `0` | 账号当日总 token 达限后不接单；`0` = 关闭。本地 0 点重置 |
| `limits.daily_credits` | `0` | 账号当日消耗积分达限后只接免费模型；`0` = 关闭 |
| `limits.model_daily_tokens` | `0` | 账号 × 模型当日 token 达限后只禁该模型；`0` = 关闭 |
| `limits.cn` / `limits.global` | 同全局 | 按域字段级覆盖（>0 生效） |
| `pricing.enabled` | `false` | 在用量页展示 OpenRouter 公开价折算的等价花费；只读，不参与调度/计费 |
| `pricing.refresh_minutes` | `0` | 价格自动刷新周期；`0` = 只在启动时拉取一次 |
| `pricing.exchange_rate` | `1` | 展示币种换算系数（1 = USD） |
| `pool.max_in_flight_total` | `0` | 网关级全局在途上限；满额新请求返回 503 且不选号；`0` = 不限。热生效 |
| `pool.credit_floor` | `0`（示例 `100`） | 积分保底：余额低于该值不再参与收费模型轮换；`0` = 关闭 |
| `pool.breaker_threshold` | `3` | 连续失败触发熔断阈值 |
| `session_sticky.enabled` | `true` | 会话粘性开关 |
| `session_sticky.ttl` | `30m` | 会话绑定 TTL（滚动续期） |
| `models.aliases` | `{}` | 客户端模型别名 → 上游 `[realm:]model`；命中后按目标裸模型选号/计费。改后需重启 |
| `models.background_names` | `[]` | 客户端后台任务硬编码模型名（大小写不敏感）；命中后把 `model` 改写为 `background_target`。留空 = 关闭。热生效 |
| `models.background_target` | `deepseek-v4.1-flash` | 后台模型改写目标；只改 `model`，其余请求原样透传 |

调度补充说明：

- 同一账号在途达到 `ceil(0.66 × max_in_flight)`（或健康因子低于 0.5）时，会话粘性命中会软逃逸；解绑后 20 秒内不再绑定回原号，窗口结束仍需回落到 `max_in_flight` 的 30% 以下。
- 在途压力以 `1/(1+7×pressure²)` 乘性降权（负载率 40% 以下不打折），Top-5 排序与加权抽签同口径。
- 全冷却兜底选中账号后进入 30 秒单飞探针窗口，其余并发不再选中同一号。
- 开启 `pool.credit_floor` 时，请求前按 prompt 长度 + `max_tokens` 估算并预扣积分；余额不足以覆盖保底时跳过该号，所有成功/失败/断流路径都会释放预扣。

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

### 部署模型：单实例

本网关按**单实例**设计，同一 `auth_dir` / `state_file`（以及同一 Redis）不要同时运行多个进程：

- 定时任务（签到 / 旅行 / 活跃 / 保活 / 余额刷新）会在每个实例各执行一次；
- 池状态以本地 `state_file` 为准，多实例会互相覆盖快照；
- 冷却、在途名额、会话粘性与限流状态都是进程内状态，不跨实例共享，路由结果会不一致。

`upstash` / Redis 仅作为**重启恢复镜像**：启动时读取状态快照与会话绑定，运行期异步写回；没有分布式锁、租约或 leader election，不提供多实例共享状态。需要高可用时，请使用反向代理指向**单个写入实例**，或由外部编排保证同一时刻只有一个实例运行。

### Docker 镜像

多阶段镜像（`golang:1.27-alpine` 构建 → `alpine:3.24` 运行）一次编译全部四个二进制并随镜像分发：`wokbuddy-demon`（主服务）、`signin_bin`、`login`、`credit` + 工具脚本。以 `app` 用户（uid 10001）运行，内置 `HEALTHCHECK`。

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
- 面板每次成功保存配置前会把上一版写为 `config.json.1`（0600，含 `api_key`，不要分享）；配置页「恢复上一版」可整体回滚。只保留一代，恢复操作本身不再滚动备份

## 安全与合规

### 1. 凭据管理（auths）

- **位置**：`./auths`（`auth_dir` 可配），文件名 `workbuddy-<uid>.json`
- **内容**：明文 `accessToken` / `refreshToken` + 账号元信息
- **权限**：容器内以 `app` 用户运行；刷新由 `SaveAtomic` 以 `0600` 原子写回；建议手动 `chmod 600 auths/*.json`
- **切勿提交 git**：`.gitignore` 已排除 `auths/`、`data/`、`backups/`、`config.json`、`*.key`、`*.pem`、`*.env`

### 2. 网络暴露与日志敏感度

- 默认监听 `:7863`，**无内置 TLS**；Docker Compose 默认仅绑定 `127.0.0.1`，需要局域网/公网访问时显式改端口映射
- 公网部署必须设置 `api_key`，并置于 HTTPS 之后 / 仅内网可达
- 聊天请求体默认上限 32 MiB（`server.max_body_bytes`，0 = 不限制），超限在解析前返回 413
- `api_key` 非空时，面板与 `GET /status`、`GET /metrics` 对同一来源地址的鉴权失败做 1 分钟窗口计数，连续 10 次失败后返回 `429` + `Retry-After`；`/v1/*` 的鉴权失败阈值为 30 次/分钟；成功鉴权清零。来源地址默认只取 TCP `RemoteAddr`；只有对端命中 `server.trusted_proxies`（缺省本机回环）时才采信 `X-Real-IP` / `X-Forwarded-For`。反代在另一台机器时需把其网段加入该配置，否则限速与日志都会把所有人算成反代地址；反代必须覆盖或追加 `X-Forwarded-For`，不能透传客户端提供的转发头
- 日志字段不含任何 token 明文（不读取 `Authorization` 头）
- **管理面写操作审计**：面板的 `POST` / `PUT` / `PATCH` / `DELETE` 请求追加到 `data/audit/audit.jsonl`（8 MiB × 5 轮转），只记方法、路径、状态码、来源 IP 与 UA，**不记请求体与凭证**；`GET /panel/api/audit?limit=` 可查询最近记录
- 日志写 **stdout / stderr**，代码无任何落盘日志文件（审计例外，见上）

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

### 智能体一键配置

面板「智能体配置」页（仅本机回环可用）可检测 Claude Code / Codex CLI / OpenCode，并把网关 provider 增量写入各客户端配置：

- 写入前把首次原件备份到 `data/agent-backups/<client>/`；还原按备份字节级恢复，网关新建的文件会被删除。
- 若写入后用户手改过配置，还原前会提示并要求强制确认。
- API key 只写入客户端本地配置，面板不回显。
- 网关地址填根地址（如 `http://127.0.0.1:7863`）；Claude Code 使用根地址，Codex CLI / OpenCode 自动补 `/v1`。
- Codex CLI 使用 `experimental_bearer_token` 内嵌 key，无需额外导出环境变量。

### 面板需要鉴权吗？

`api_key` 非空时面板要求输入一次密钥（浏览器记住）；为空则直接可用。**公网部署务必设置 `api_key` 并置于 HTTPS 之后。**

### 多密钥（分钥匙给多个客户端）？

面板「密钥」页可创建多把**命名密钥**（如 `sk-wb-xxxx…`），分发给不同客户端，并可逐把撤销或删除。规则：

- 命名密钥以 **SHA-256 哈希**存储（`data/api_keys.json`），明文**仅在创建时回显一次**；
- 命名密钥**只作用于网关 API 面**（`POST /v1/chat/completions`、`POST /v1/messages`、`POST /v1/messages/count_tokens`、`GET /v1/models`）；
- 面板、`GET /status`、`GET /metrics` 仍**只认**默认 `api_key`（命名密钥是给客户端的，不获得管理/运维可见性）；
- 每把密钥记录 `last_used_at`、请求数 / token 用量与被限制拒绝数，面板可查看；
- 面板「限制」可设置每把密钥的 `rpm` / `tpm` / `max_concurrent` / 模型白名单（glob，可带 `cn:` / `global:` 前缀）；超限返回 `429 + Retry-After`，错误体按入站协议编码（OpenAI / Anthropic / Gemini 各自的信封），白名单未命中返回 `403 model_not_allowed`；未配置限制的密钥行为不变。限流为进程内单实例计数；
- 白名单按别名 / 后台模型改写 / 模态路由解析后的**最终模型**匹配（例如 `models.aliases`、`models.background_names` 的目标，或 `features.multimodal_routing` 改选的模型）；只写客户端请求的原模型名不会生效，已有配置需改写成目标名；
- 每把密钥可绑定出口（`cn` / `global` / 跟随模型）：绑定后不带前缀的请求只走对应域；与显式 `cn:` / `global:` 前缀冲突返回 `400 realm_binding_conflict`，不消耗账号或上游额度；
- `api_key` 为空且未创建任何命名密钥时，网关仍为「不鉴权」放行（既有默认）。

本功能**不涉及任何计费**：不设额度、不设配额、不设账单。

## 免责声明

本项目仅供学习和研究使用。使用者需遵守 CodeBuddy 服务条款，自行承担使用风险（包括账号封禁、条款违约等）。作者不对任何因使用本项目产生的直接或间接损失负责。

## License

本项目采用 [MIT License](LICENSE) 开源协议。

- 允许任意使用、复制、修改、合并、发布、分发、再授权及销售
- 再分发时请保留原仓库的 MIT 版权声明与许可声明
- 本项目不授予任何 CodeBuddy / 腾讯接口或服务的权利；使用者仍需自行遵守其服务条款
