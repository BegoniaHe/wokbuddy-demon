# 工作流支撑 reference

`SKILL.md` 之外的细节放这里，按需加载；不复制 `AGENTS.md` / `UPSTREAM.md` 的内容。

- `references/client-profile.md`：指纹 profile 的 schema、surface 表、来源分级、
  校验规则、反伪造硬规则与落点。生成/校验 profile 前必读；补齐/修改 surface 时
  同步该文件与校验器。
- `references/protocol-anchors.md`：协议面事实源与锚点速查（端点 / 头族 /
  realm 常量在本仓库的 `path:line`）。调研时先读它，再打开对应文件核实。
- `references/mechanical-tools.md`：跨版本机械整理脚本用法与事实边界。八个脚本
  只扫 `.tmp/workbuddy/<realm>/<ver>/app-asar/`、只往 `.tmp/` 落产物，join key
  只用相对路径与可读导出名；跨版本比对先跑 `asar_manifest.mjs` 再按需下钻。
- `scripts/extract_asar.mjs`：从 `.app/Contents/Resources/app.asar` 提取文件树。
  用法（在解包目标版本目录下）：

  ```bash
  node extract_asar.mjs <app.asar> <app.unpacked-dir-or-empty> <outDir>
  # packed/unpacked/linked/missing 计数输出；missing>0 说明 unpacked 目录不全
  ```

  该脚本不依赖 `@electron/asar`，纯 Node 内置模块实现 ASAR 头解析。
- `scripts/download_latest.mjs`：查官方更新接口并下载最新安装包。

  ```bash
  node download_latest.mjs --dry-run                 # 只看版本与 URL
  node download_latest.mjs                           # cn + global 的 Win32 包（事实源）
  node download_latest.mjs --realm cn --platform darwin-arm64 # macOS 辅助对照
  node download_latest.mjs --realm cn --platform all # 指定 realm / 全部平台
  ```

  接口：`https://www.workbuddy.ai/v2/update?platform=<p>`（global）与
  `https://www.workbuddy.cn/v2/update?platform=<p>`（cn），返回
  `{version,url,productVersion,sha256hash,...}`。平台名：
  `workbuddy-darwin-arm64` / `workbuddy-darwin-x64` /
  `workbuddy-win32-x64-user` / `workbuddy-linux-{x64,arm64}-{deb,rpm}`。
  默认平台是 `win32-x64-user`：项目以 Win32 构建为唯一事实源，macOS 只作辅助
  对照。macOS 接口给 `.zip`（自动更新包），网站实际下载同版本 `.dmg`；显式指定
  darwin 时脚本把后缀换成 `.dmg`，`sha256hash` 只对应接口原 URL（zip），dmg 不校验。
  落盘路径：`.tmp/workbuddy/<realm>/<version>/`，`<version>` 取文件名中的
  完整版本串（含构建 hash，如 `5.8.4.41366630-73952edb`）。

- `scripts/wine_login.py`：用 `auths/<uid>.json` 注入客户端登录态后启动。何时用、
  边界与成功判定见 `SKILL.md`「用网关凭证登录 wine 客户端」；登录态落盘形制与
  证据锚点见 `references/protocol-anchors.md`「客户端登录态落盘」。

## 已知数据形态（2026-11-15 核对，Win32）

| Realm | 包版本 | `cli/product.json` `productName` | `endpoint` | `authentication.id` | commit | date |
| --- | --- | --- | --- | --- | --- | --- |
| `cn` | 5.11.3.43806934-ecace0f7 | WorkBuddy | https://www.workbuddy.cn | workbuddy-desktop | `ecace0f7028166695994300182295329b8b95449` | 2026-11-12T16:49:29.764Z |
| `global` | 5.9.4.42593613-fa0a0901 | WorkBuddy AI | https://www.workbuddy.ai | workbuddy-desktop-ai | `fa0a0901906addaa1a4259f6df347388a5563ded` | 2026-10-29T15:46:44.029Z |

同一时点两 realm 版本不同步（cn 5.11.3 / global 5.9.4）属常态：区分域不要假设版本号对齐，
每次按各自 update 接口取；`authentication.id` 两 realm 不同（`workbuddy-desktop` vs
`workbuddy-desktop-ai`），登录态文件 `<id>.info` 因此也不同名。`cli/product.json`
**没有** `releaseDate` 键，包内只有 `date`；profile 的 `releaseDate` 需另取来源，
不得由 `date` 或版本号推断。

macOS 同版本包的 commit/date 可能与 Win32 不同（CN 的 commit 与 date 都不同、
global 仅 releaseDate 不同），只能用于辅助对照，不得写入 profile 或代码常量。

CN 与 global 的 `internalDomain` / `externalDomain` 划分不同（CN 无
`externalDomain` 拆分，global 有 `iOADomain` / `cloudHostedDomain`）。realm 隔离
在本仓库 `internal/auth/` 的 `realm` 判定与本常量组的对应关系是每次调研的
首要核对项。
