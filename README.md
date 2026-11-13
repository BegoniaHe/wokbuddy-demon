<p align="center">
  <img src="assets/wokbuddy-logo.png" alt="WokBuddy Demon" width="120">
</p>

<h1 align="center">WokBuddy Demon</h1>

<p align="center">
  <b>一只住在你机器里的小恶魔，替你照看 CodeBuddy 账号的每一天</b><br>
  自动签到 · 成长任务 · 猫猫旅行 · 连登抽奖 · 积分看板 · 多账号管家
</p>

<p align="center">
  <code>不用喂</code> · <code>不用遛</code> · <code>不会半夜叫你起床</code>
</p>

---

> **一句话**：把 CodeBuddy 的日常琐事交给它——签到、养猫、做任务、盯积分它全包，你只负责偶尔看看战报。
>
> 免费开源、纯本地自托管。凭证和状态都待在你的机器里，像一只认家的猫。

## 🧚 小恶魔档案

| 项目 | 内容 |
|---|---|
| 物种 | 一只用 Go 写的本地小恶魔 |
| 主食 | 签到、成长任务、猫猫旅行、连登抽奖 |
| 特长 | 官方 18 个成长任务，它能一键做完其中 17 个 |
| 弱点 | 最后一个任务要真实捐款——它不会替你掏钱包 |
| 活动范围 | 你的机器，以及你亲手打开的那块面板 |
| 人生信条 | 能自动的，绝不手点 |

它不负责替你写周报，也不保证抽奖欧气。它只做一件事：把每天重复、琐碎、容易忘的账号事务，安安静静做完。

## 🕰️ 它的一天

默认值班表如下，时间都可以在面板里改：

| 时间 | 值班内容 | 备注 |
|---|---|---|
| 09:00 | 签到 + 猫猫旅行 | 早班先打卡 |
| 10:00 | 活跃上报 | 证明今天有在营业 |
| 21:00 | 晚间签到 + 猫猫旅行 | 夜班继续 |
| 22:00 | 保活 | 给账号续口气 |
| 23:00 | 夜猫子任务 | 按 CST 排程，越晚越精神 |
| 01:00 | 成长任务巡检 | 趁你睡着偷偷努力 |

所有定时任务（签到 / 旅行 / 活跃 / 保活 / 夜猫子 / 成长 / 余额）统一按 **CST（Asia/Shanghai）** 解释日界与整点，宿主时区（UTC 容器 / NAS / 海外 VPS）不会让任务漂移。升级后旧版按宿主本地日界写入的补跑标记会被忽略一次，按 CST 重新计算。任务可分别开关。今天不想让它出门？关掉就行，它不会闹。

定时保活只刷新将在 `schedule.keepalive_refresh_window`（默认 `24h`）内到期的账号；无到期时间或已过期的账号照常刷新，手动「全部保活」仍全量执行。

实际触发时刻会在整点后按 `schedule.slot_jitter`（默认 20 分钟）做确定性延后；账号之间按对数正态间隔错开（签到以 `checkin_account_delay` 为中位数，旅行/活跃/夜猫子用各自固定中位数），每号每天还有独立相位。把抖动、σ 与相位窗口设为 `0` 可回到整点齐发、固定间隔；相位窗口为 `0` 时仍会按每日相位打乱账号顺序。

## 🎒 它能帮你做什么

- **每天自动打卡**：早晚签到、保活、活跃上报，不用定闹钟。
- **成长任务一键完成**：进度、计分、领奖一条龙；重复点击不会重复消耗，已完成的任务会自动跳过。
- **连登管家**：签到后顺手兑换连登奖励、抽完抽奖次数，不留红点过夜。补签每轮默认不限；设置 `schedule.makeup_cards_per_round`（`0` = 不限，`>0` = 每轮最多成功补签天数，热生效）后，达到上限的剩余漏签留待下一轮。
- **猫猫旅行**：按时推进旅行状态，猫猫出门和回家都有人照看。
- **多账号一起养**：账号们排班轮值；谁状态不好谁先休息，恢复了再回来上班。
- **命名密钥生命周期**：每把密钥可单独设置到期时间、累计 token 配额与来源 IP 白名单；到期/配额/IP 未命中在选号前拒绝，不触碰上游。
- **命名密钥限速**：`rpm` / `tpm` / `max_concurrent` 三项彼此独立（`0` 或不填 = 不限，互不联动、不从别的字段补默认值）。`max_concurrent` 超限立刻 `429 concurrency_limit_exceeded`（Anthropic 面仍是 `rate_limit_error` 信封，message 带该错误码）；`rpm` / `tpm` 超速**不再返回 429**，改成把已放行的回复拉长——首帧仍立即转出，后续帧与结束标记按 `60/rpm`、`60/tpm` 的节奏推迟；`tpm` 只计 Decode 阶段新增的 completion/output token，prompt/input token 不入账（单条流的累计拖延封顶 60 秒，到顶后剩余内容一次放完），等价于把下一发请求推迟出去，不再用 429 逼客户端重试。账本在进程内、单实例口径。
- **一块面板看全部**：账号、任务、积分、运行日志，不用在多个页面之间来回找。
- **面板内试聊**：直接输入提示词走真实 `/v1` 链路，验证模型、账号与流式输出；对话只存在本机浏览器。
- **本地状态持久化**：重启后还记得昨天干到哪，不会一键失忆。
- **登录与搬家**：面板里完成授权登录；需要换机器时，可以加密导出再导入账号。
- **官方工具出口**：`POST /v2/chat/busy` 模型忙预检（只读本地池状态，单批最多 50 个模型）；`POST /v1/search/messages` 支持 `deep:true` 综述式检索（`features.search_deep`，默认关）；`POST /v1/tools/fetch` 提供官方 webfetch 的 DSH 形态出口（`features.web_fetch_endpoint`，默认关）；`POST /v1/images/generations` 与 `POST /v1/images/edits` 提供官方出图 / 图生图（`features.images_endpoint`，默认关，两者共用同一开关）。
- **视频生成**：`POST /v1/videos/generations` 提交任务，JSON 形态原样透传官方 `VideoService` 的字段（`prompt` / `model` / `watermark`，可选 `seconds` / `image_url` / `last_image_url` / `reference_images` / `reference_videos` / `reference_audios` / `extra_parameters`），返回上游 `{code,msg,requestId,data:{id}}`，`data.id` 即任务 id；`GET /v1/videos/{task_id}` 查询任务（可选 `?model=` 决定选号域，跨域任务必须带），`data.status=completed` 时原样透出 `data[].url` 与 `data.usage.credit`，`failed` 归一为 502 通用文案。由 `features.videos_endpoint` 控制（默认关）。网关只做无状态透传，**不轮询**，轮询节奏由客户端决定；带 `text-to-video` / `image-to-video` 标签的模型在 `/v1/models` 以 `capabilities` 数组可见。
- **官方档位徽章**：解析 `/v3/config` 的 `modelTiers`，在模型页与试聊选择器显示档位与升级提示，不改变选号或模型过滤。
- **按密钥裁剪模型列表**：命名密钥的 `/v1/models` 只展示其 realm 绑定与模型白名单允许的条目，复用同一份缓存，不额外请求上游。

### 🎯 差一个就满分的成长任务

官方成长计划有 18 个任务，小恶魔能自动完成 17 个，包括那些需要真实对话、连续操作和等待计分的麻烦家伙。

最后一个是 `Expert_Philanthropy`：它需要真实捐款，领奖时会校验捐赠回执。这种决定应该由你本人来做，所以小恶魔只负责提醒，不负责替你慷慨。

## 🧭 小恶魔守则

1. **数据不出门**：凭证与状态留在本机，不往云端兜一圈。
2. **失败先休息**：遇到限流或故障会退避、冷却，不头铁硬撞。
3. **重复不白干**：按天幂等，已经做完的任务不会反复消耗资源。
4. **你说了算**：定时任务、账号操作、面板配置，都能随时调整。
5. **不装作全能**：做不到的就直说，需要人工的步骤会给指引。

## 🚀 三分钟开养

需要 Docker + Docker Compose，或者直接运行单文件二进制。

```bash
git clone https://github.com/BegoniaHe/wokbuddy-demon.git
cd wokbuddy-demon

cp config.example.json config.json

# 客户端指纹快照：data/client-profiles/cn.json 必需；config.example.json 的
# global.enabled 默认为 true，因此 global.json 也必需（纯 CN 部署可把
# global.enabled 设为 false 后只提供 cn.json）。schema 2 要求 emit.stainless
# 七个值；用 reverse-workbuddy 技能从官方 Win32 分发包生成，缺失或 schema 1
# 会拒绝启动
docker compose up -d --build
```

然后打开 **<http://127.0.0.1:7863/panel/>**，在面板里点「添加账号」完成登录。

默认只监听本机。第一次启动会自动处理好配置文件；面板与账号状态分别落在 `data/` 和 `auths/`，升级或重启都不会弄丢。

不想用 Docker？下载 nightly 单文件（或自行构建）直接运行即可。前端已经打包进二进制，不需要额外安装运行时。

> 本项目滚动更新，没有版本号，只有 nightly。不用等 `v1.0`，它不会来。

## 🗝️ 移除账号与撤销登录会话

账号行右侧「更多操作」里有两个相近但**不同**的动作：

- **移除**：只清本机。账号出池，删除 `auths/<uid>.json` 与其 `.bak`。上游那条 refreshToken **仍然有效**——适合只是不想再用这个号、但凭据还在自己手上。
- **撤销登录会话**：先调上游 `POST /v2/plugin/auth/logout` 让这条 refreshToken 失效，成功后再走「移除」的本地收尾。适合怀疑凭据外泄、或想把该会话真正作废。

撤销的范围是**这条 refreshToken 对应的会话**，不保证覆盖该账号在别处新登录的会话（官方客户端也没有设备清单 / 踢设备接口）。上游调用失败时不会删除本地凭证——撤销不成就把凭据弄丢，等于放弃唯一能撤销它的东西；唯一例外是凭证已被判定失效（401 / 会话失效），此时已无可撤销对象，直接清本地。

## 🎛️ 模型筛选（`models`）

`config.json` 的 `models` 段决定哪些模型对调用方可见。三组配置互相独立、可叠加：

- `models.hidden`：黑名单。裸名 = `cn` 与 `global` 两域都隐藏，`cn:x` / `global:x` 只藏该域。命中后不出现在 `/v1/models`，点名调用返回 `400 model_unavailable`（不打上游、不占号）。热生效。
- `models.allowed`：部署级白名单。语法与 `hidden` 相同，空数组 = 不启用（放行全部）。启用后只有名单内模型出现在 `/v1/models`，点名调用同样 `400 model_unavailable`；与 `hidden` 取交集，同时命中的模型拒绝。热生效。若开启后台模型改写（`models.background_names`），`models.background_target` 也必须在白名单内，否则启动即报错。
- `models.aliases`：别名 → 上游模型。键可用 `*` / `?`（`path` 通配：`*` 任意长度含空、`?` 单字符）；精确键优先，多个通配同时命中时模式更长优先、同长取字典序较小的模式，未命中则原样透传。点名调用按解析后的**目标模型**受上面两套名单约束（目标被隐藏或不在白名单 → `400 model_unavailable`）；`/v1/models` 只列精确别名（通配键不出现），且要求别名名字本身也在白名单内，否则该别名不列出（别名可用但不可见）。改别名需重启。

命名密钥自带的模型白名单（`/panel/` 密钥管理里的 glob）语义不变，与 `models.allowed` 取交集：两边都放行才可用。

## 🖼️ 号池分区（`pool.media_partition` / `pool.media_accounts`）

图片 / 视频请求比文本更容易触发上游按账号的风控与限额，一次画图被判异常可能连坐同一个账号上的文本请求。开启分区后两类请求各用各的号：

- `pool.media_partition`：默认 `false`（不分区，行为与之前完全一致）。
- `pool.media_accounts`：媒体号 uid 列表。开启分区后，`/v1/images/*` 与 `/v1/videos/generations` 只从这些账号选号；文本请求只从**其余**账号选号。
- 媒体号耗尽时选号明确失败（`503 no_healthy_account`），**不跨区回落**——跨区回落等于绕过分区，与动机相反；全冷却兜底同样遵守分区。
- 只影响选号池，不改账号归属、健康台账与 `state.json`；与 `pool.strategy` 等既有选号策略叠加，不是另一套框架。热生效。

## ⏳ 额度窗口探针（`schedule.window_probe_enabled` / `window_probe_minutes`）

上游对 6004 模型级限流会给出「将在 … 重置」的墙钟。重置之后，新的额度窗口要等**第一条请求**才开始计时；如果一直没请求，窗口起点就跟着漂到下一次真实流量上。开启探针后，网关在重置时刻到点主动发一条最小请求，把新窗口起点钉住：

- `schedule.window_probe_enabled`：默认 `true`；`false` 关闭。
- `schedule.window_probe_minutes`：探针轮询周期，默认 5 分钟（`<=0` 回落 5）。
- 探针是一条 `max_tokens=1`、正文 `hi` 的最小 chat 请求。它会真实产生一次模型调用，但只有在该 (账号, 模型) **刚被 6004 限流并带重置时间**后、且重置时刻已到才会触发。
- 同一个窗口最多试 2 次，两次间隔至少 10 分钟；成功即出账，不重复触发。
- **失败不罚号**：探针失败不改账号健康、不计连败、不冷却、不喂熔断，只记一条 WARN（错误已脱敏）。
- 台账是运行态、不落盘；重启后由下一次 6004 重新登记。热生效：面板保存配置即改周期。

## ✂️ 历史裁剪（`prompt.history_keep_turns`）

网关在**发往上游之前**丢弃过老的多轮历史，避免长会话把上下文与额度耗在已经无关的旧轮次上。

- `0`：不裁剪（默认，零行为变化）。
- `N`（1..1000）：「一轮」按一条 `user` 消息计；只保留**最近 N 轮**，更早的丢弃。开头连续的 `system` / `developer` 提示词**始终保留**。
- 负数与 `> 1000` 在加载与面板保存时直接报错（不静默当成关闭，也不静默当成不限）。
- 只影响 chat 出站请求体（`/v1/chat/completions`、`/v1/messages`、`/v1/responses`、gemini 入口同源）；不裁剪 `tools` 声明、不改 system 内容、不做摘要压缩。
- 面板「配置」页的「保留历史轮数」可改，**热生效**，无需重启。

## 🧦 出站代理（`upstream.proxies` / `proxy_slots` / `realm_proxy_slots`）

多账号共用同一个出口 IP 容易撞上游按 IP 的风控，国际版与国内版账号的网络路径也常常不同。三组配置可以把出站出口拆开（issue #15 / #96 / #39）：

- `upstream.proxies`：全局代理池。非空时按健康轮换，失败代理指数冷却（30s 起、10min 封顶），全部冷却后半开探测。空 = 直连（默认）。
- `upstream.proxy_slots`：命名代理槽（`{id, name, url, enabled}`）。在面板「账号」页把某个账号绑定到槽，该账号即固定走这个出口。
- `upstream.realm_proxy_slots`：`realm → 槽 id` 默认出口，键只允许 `cn` / `global`。账号自身未绑定槽时按账号 realm 选出口；值为空串等于未设置，指向不存在或已禁用的槽会在保存时被拒绝。
- `upstream.proxy_subscriptions`：代理订阅 URL 列表（`http(s)` 地址）。拉取到的内容可以是纯 URL 列表、base64 包裹的同类列表，或 Clash/KM YAML 的 `proxies:` 段；解析结果并入 `proxies` 的同一份全局池，非法条目单独跳过且不整批失败。拉取失败时保留上一次成功结果，不清空现有代理。`POST /panel/api/proxy_subscriptions/refresh`（需 admin key）可手动刷新。

**代理写法**：`http://host:port`、`https://host:port`、`socks5://host:port`、`socks5h://host:port`（均可带 `user:pass@`），以及机场常见的四段式 `host:port:user:pass`（按 **socks5** 处理，端口必须 1..65535）。面板与日志只展示脱敏形态（`scheme://host:port`），口令不进日志、不进健康快照。

**出口优先级**（高 → 低）：账号绑定槽 → 账号 realm 默认槽 → `proxies` 全局池 → 直连。

**失效不回落**：前两档命中但槽不存在 / 已禁用 / 构造失败时，该请求直接失败（`502`，且不惩罚账号），**绝不**静默改走全局池或直连——静默直连会让人以为在走代理，实际暴露了宿主机真实 IP。

**覆盖范围**：账号作用域内的全部出站请求——对话、token 刷新、余额/积分包、签到、保活、旅行、成长任务、活跃上报、搜索 / 抓取、图像、模型目录探测，以及带账号 token 的国际版注册激活 / 补地区链路。无账号的调用（面板 / `cmd/login` 的 OAuth 设备授权）不进这三组配置——那条链路用独立的短超时 client，出口只受进程环境 `HTTP_PROXY` / `HTTPS_PROXY` 影响（Go 默认 Transport 的行为）；上游出站 transport 不设 `Proxy`，只认上面这套槽与全局池，不读环境变量。

三个字段都**热生效**：面板保存后立即换出口，不需要重启（改动会重建出口注册表并原子替换拨号器，在途请求不受影响）。

## 🪞 出站指纹（`upstream.header_order` / `upstream.tls_fingerprint`）

网关默认尽量贴近官方 WorkBuddy CLI 的出站形状，两个开关都在「配置」页「上游与高级」里，**改动需重启**：

- `upstream.header_order`（默认 `true`）：把 CLI chat（`POST /v2/chat/completions`，带 `x-stainless-lang` 标记）的明文请求头块重排为官方 42 头顺序与大小写，并补 `Connection: keep-alive`。Go 的 `net/http` 固定先写 `Host`/`User-Agent`/`Content-Length`、其余按字典序，无法用 `Header.Set` 模拟，所以在 TLS 之上做了连接层重写。只作用于 CLI chat，其他 surface 原样透传；解析失败会回写原始字节并关闭该连接的重写（不破坏请求）。显式设为 `false` 且 `tls_fingerprint=off` 时完全不安装自定义 TLS 拨号，回到 Go 标准路径。
- `upstream.tls_fingerprint`（默认 `off`）：`off` = Go 标准库 TLS；`node` = 用 vendored uTLS 复刻官方 Node/BoringSSL 的首连 ClientHello（18 cipher、固定扩展序、无 ALPN/GREASE、会话恢复 PSK + padding、正常握手不发 dummy CCS）。`node` **要求** profile 带 `tls` 段，缺失即拒绝启动；TLS 握手失败直接报错，不会回落到标准库（回落会暴露调用方想隐藏的指纹）。

两个开关都保留既有出站路由：代理、按槽拨号、证书校验、SSE、连接复用与 `writeDeadlineConn` 写超时不变。

### profile schema 2

`data/client-profiles/<realm>.json` 升到 `schema: 2`：

- `emit.stainless`：七个必填值（`arch` / `lang` / `os` / `packageVersion` / `retryCount` / `runtime` / `runtimeVersion`），对应官方 CLI chat 的 `x-stainless-*` 头族；必须附 `evidence.stainless`。
- `tls`（可选，`tls_fingerprint=node` 时必需）：`versions` / `cipherSuites` / `extensions` / `groups` / `pointFormats` / `signatureAlgorithms` / `pskModes` 数值段。两个已加载 realm 的 `tls` 段必须逐字段相等（共享 Transport 无法按 realm 切换）。

旧 `schema: 1` profile 会被拒绝启动。用 `reverse-workbuddy` 技能从同一份官方 Win32 分发包重新生成 cn / global 两份 profile，再启动即可；生成过程与校验规则见 `.agents/skills/reverse-workbuddy/references/client-profile.md`。仓库不内置任何版本 / commit / releaseDate / 指纹兜底字面量。

## 🌊 流式等待（`upstream.keepalive_interval` / `upstream.ttfb_keepalive_interval`）

网关有两条 SSE 心跳口径，都只写注释帧（`: keepalive`，客户端按 SSE 规范忽略），不改任何真实帧；两项都是 `config.json` 的 `upstream` 段配置项（面板配置页未提供输入框），**改动需重启**：

- `upstream.keepalive_interval`（默认 `0` = 关闭）：上游静默超过该时长后向客户端写注释帧，防止反代 / Cloudflare 把长时间无帧的合法流当空闲连接切断。
- `upstream.ttfb_keepalive_interval`（默认 `1500ms`）：**首帧等待期**专用间隔。网关为了「首帧即错误时能静默换号」，在上游第一帧到达前不写任何内容；大上下文下这段空窗可达秒级，客户端会按空闲超时断开。开启后按该间隔补注释帧，首帧一到立即停手（正常流不多写）。设为 `0` 关闭，回落逐字节旧行为。

若等首帧期间已经写出过注释帧（响应头已提交）而轮转全部失败，网关降级为 `200` + SSE error 帧 + `[DONE]`，不会把 JSON 错误塞进已提交的流里；未提交时仍是原来的 JSON 错误码。

## 🧾 运行日志落盘与导出（`logging.runtime_log_enabled` / `runtime_log_retention_days` / `runtime_log_max_mb`）

默认只把运行日志写到 stdout/stderr 与面板的 500 条环形缓冲，重启即丢。开启落盘后追加一份按天归档：

- `logging.runtime_log_enabled`：默认 `false`（不落盘，行为与之前一致）。开启后 `log.Printf` 的输出按天写入 `data/runtime-logs/server-<day>.log`。
- `logging.runtime_log_retention_days`：保留天数，默认 7；`<=0` 回落默认。
- `logging.runtime_log_max_mb`：目录总上限（MiB），默认 100；`<=0` 回落默认。单文件超过 16 MiB 时写成 `server-<day>.1.log` 等序号文件；总量与保留期按文件时间从旧到新裁剪。
- 落盘前统一脱敏（`Authorization`、token 键值、URL 查询参数、长高熵串），磁盘与导出内容都是打码后的文本。
- 以上三项**改动需重启**（出口在启动期按目录构造）。

导出走只读接口 `GET /panel/api/logs/export`（`scopeRead`）：`from` / `to` 支持 unix 秒或毫秒、RFC3339、`2006-01-02T15:04` 与 `2006-01-02`，缺省导出当天；区间最长 31 天、未压缩总量上限 64 MiB、区间内无文件返回 404。未启用落盘时接口返回 501。面板前端暂无下载按钮，可直接用 `curl -H "Authorization: Bearer <key>" -o logs.zip "<网关>/panel/api/logs/export?from=2026-11-01&to=2026-11-13"`。

## 🐾 适合谁

- 手上有多个 CodeBuddy 账号，但不想每天挨个打卡；
- 喜欢把数据留在自己的机器上，不想交给第三方托管；
- 想要一个能看任务、积分和日志的本地面板；
- 愿意遵守平台规则，只照顾自己有权使用的账号。

不适合想托管别人账号、想把它直接裸奔在公网、或者指望它替你中奖的人。

## 🤫 一条不太像 README 的请求

- **别点 Star**：这个项目不靠星星续命，星多了反而心慌。
- **喜欢就开个 Issue**：带上 `star` 标签；加不上标签的话，标题或正文写 `star` 也行。
- **别发朋友圈、别做视频、别到处安利**：我们悄悄用，慢慢修。
- **作者中文不太灵光**：担心 issue 被理解歪，欢迎写英语或德语。*Issues auf Deutsch sind immer bevorzugt.*
- **英语德语也不会？** 没关系，用你最顺手的语言，作者的 AI 能看明白。
- **总之低调一点**：作者希望它一直开源，而低调是最好的护身符。

## ⚠️ 使用须知

WokBuddy Demon 是供个人打理自己 CodeBuddy 账号的本地小工具：自动签到、保活、把每天的成长任务顺手做完。免费、开源、按「原样」提供。

- 仅用于**本人授权的账号**与**本机 / 私有环境**；
- 请遵守 CodeBuddy 平台的服务条款与所在地法律；
- `auths/` 里保存的是明文凭证，面板也没有内置 TLS。请妥善保管凭证目录，不要把面板端口直接暴露到公网。
- 默认 `api_key` 同时用于 `/v1/*` 与面板/运维面。需要隔离时设置可选的 `admin_api_key` 与 `readonly_api_key`：任一非空后，`/panel/api/*`、`/status`、`/metrics`、`/debug/pprof/*` 只接受这两把凭据，默认 `api_key` 仅保留 `/v1/*` 权限；`readonly_api_key` 只能读，写操作与配置读取需要 `admin_api_key`。三把凭据必须互不相同，均为热生效。

## 免责声明

本项目仅供学习和研究使用。使用者需遵守 CodeBuddy 服务条款，自行承担使用风险（包括账号封禁、条款违约等）。作者不对任何因使用本项目产生的直接或间接损失负责。

## License

本项目采用 [MIT License](LICENSE) 开源协议。

- 允许任意使用、复制、修改、合并、发布、分发、再授权及销售
- 再分发时请保留原仓库的 MIT 版权声明与许可声明
- 本项目不授予任何 CodeBuddy / 腾讯接口或服务的权利；使用者仍需自行遵守其服务条款
