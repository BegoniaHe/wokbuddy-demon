# 协议面锚点速查

调研中先在此定位本仓库对应实现，再逐项比对。行号会漂移，核对时以符号名为准。

## Realm 分域

- `internal/auth/auth.go`：`realmCN` / `realmGlobal`、`Realm()` / `ResolveRealm`。
  账号域判定的唯一来源；`a.IsGlobal()` 是各上游方法的金闸。
- 客户端权威声明：`<app-asar>/cli/product.json`
  （`endpoint` / `officialEndpoints` / `authentication.*`）。

## Base 与路径

- `internal/upstream/client.go`
  - `ChatBaseCN`（默认 `https://www.workbuddy.cn`）、`ChatBaseGlobal`；
    `globalOn()` 双闸（config `upstream.global_enabled` + 账号 realm）。
  - `chatCompletionsPath = "/v2/chat/completions"` 与 `chatPaths()`
    （global 曾走 `/console` 路径，因 WAF body 规则改回 `/v2` 单路径）。
  - `chatOrigins()` / `RefreshToken()`：CN 首 token 前按 workbuddy.cn →
    copilot.tencent.com → wb.tencentbuddy.com 回落。
  - `billingMeterPaths()`：global 无 `/v2` 前缀优先，404 回落；CN 只用
    `/v2/...`。`checkinMeterPaths()`：两 realm 均 `/v2` 优先，404 回落旧路径。
  - `WebBaseCN`（`https://www.workbuddy.cn`）：任务领奖 / profile 域。
  - `/v3/config` 拉取 + `X-Domain`：见 `internal/upstream/client.go` 1874 附近。
- 桌面指纹上报：`internal/upstream/desktop.go`：`/v2/report`、
  `/v2/user-asset/appearance/set`、固定构建指纹 `*DesktopCommit` /
  `*DesktopReleaseDate`。

## 请求头族

- `internal/upstream/headers.go`
  - `userAgent()` 三段形态 `WorkBuddy/<v> WorkBuddy/<v> CLI/<cli>`，版本按
    `clientVersionFor` / `cliVersionFor` 分 realm 取默认，config 可覆盖。
  - `injectDeviceToken()`：`X-Device-Token`，文件兜底见
    `internal/upstream/device_token.go`。
  - 会话头族 `X-Conversation-Id` / `-Request-Id` / `-Message-Id` /
    `X-Request-ID` / `X-Root-Request-Id`（`ChatMeta`）。
  - chat 桌面头组：`setDesktopExpertHeaders()`（`X-Domain` / `X-Product=SaaS` /
    `X-User-Id` / `X-Agent-Intent|Purpose|Type` / `X-Private-Data` /
    `X-Ide-Name|Type|Version` / `X-Codebuddy-Request` /
    traceparent + b3 单头 + `X-B3-*` 四件套）。
- 客户端侧对照：
  - `<app-asar>/main/*.js`：官方 UA、`X-IDE-*`、`X-Agent-*`、`X-Device-Token`。
  - `<app-asar>/renderer/assets/esm-*.js`：渲染层 `X-AI-Scenario|Channel|Model|
    Thinking|Online|SPACE` 族（仅 5.7.6 global 包见字面量；判断是否真出网）。
  - `<app-asar>/node_modules/openai/internal/detect-platform.js`：
    `X-Stainless-*` 族字面量（OpenAI SDK 自带，非上游自定义头）。

## 任务 / 活动域

- `internal/upstream/tasks.go`：`/v2/activity/growth/tasks{,-list,accept,
  reward/claim}`，chat 域与 webBase 域两种 claim 路径与 404 事实。
- `internal/upstream/streak.go`：`/activity/growth/{streak,lottery/summary,
  lottery/draw,redeem}`。
- `internal/upstream/blackcat.go`：`/activity/growth/{heatmap,makeup-cards/use}`。
- `internal/scheduler/travel*.go`：`/activity/growth/buddy/travel/*`。
- 客户端对照：`rg -o '/v2/activity/[a-z/-]*' <app-asar>`；已知客户端还有
  `/v2/billing/meter/checkin-activity-status` / `checkin-status` /
  `get-user-resource-free-packages` / `get-user-resource-paid-packages` /
  `get-user-resource-summary`，本仓库未覆盖（首查项）。

## 已知未覆盖 / 形态存疑入口

写报告时逐项给结论（覆盖 / 不需要 / 待落地）：

- `/v2/plugin/device/auth/code`（浏览器一次性登录码，登录态流程，与
  `internal/auth` 的 refresh 流不同源）。
- `/v2/chat/{queue/status,queue/cancel,prediction,busy}`：CLI 排队形态，
  本仓库 chat 单轮请求未涉及。
- `/v2/user-asset/*`：appearance/set 已覆盖，skill/assets 族未覆盖。
- `/v2/service-proxy`、`/v3/group/*`、`/v2/spans`：客户端内部域，非网关目标。
