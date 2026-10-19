# 工作流支撑 reference

`SKILL.md` 之外的细节放这里，按需加载；不复制 `AGENTS.md` / `UPSTREAM.md` 的内容。

- `references/protocol-anchors.md`：协议面事实源与锚点速查（端点 / 头族 /
  realm 常量在本仓库的 `path:line`）。调研时先读它，再打开对应文件核实。
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
  node download_latest.mjs                           # cn + global，当前机器平台
  node download_latest.mjs --realm cn --platform all # 指定 realm / 全部平台
  ```

  接口：`https://www.workbuddy.ai/v2/update?platform=<p>`（global）与
  `https://www.workbuddy.cn/v2/update?platform=<p>`（cn），返回
  `{version,url,productVersion,sha256hash,...}`。平台名：
  `workbuddy-darwin-arm64` / `workbuddy-darwin-x64` /
  `workbuddy-win32-x64-user` / `workbuddy-linux-{x64,arm64}-{deb,rpm}`。
  macOS 接口给 `.zip`（自动更新包），网站实际下载同版本 `.dmg`；脚本对
  darwin 把后缀换成 `.dmg`。`sha256hash` 只对应接口原 URL（zip），dmg 不校验。
  落盘路径：`.tmp/workbuddy/<realm>/<version>/`，`<version>` 取文件名中的
  完整版本串（含构建 hash，如 `5.8.4.41366630-73952edb`）。

## 已知数据形态（2026-10-19 核对）

| Realm | 包版本 | `cli/product.json` `productName` | `endpoint` | `authentication.id` |
| --- | --- | --- | --- | --- |
| `cn` | 5.8.3.41334564-db0e9b51 | WorkBuddy | https://www.workbuddy.cn | workbuddy-desktop |
| `global` | 5.8.4.41366630-73952edb | WorkBuddy AI | https://www.workbuddy.ai | workbuddy-desktop-ai |

CN 与 global 的 `internalDomain` / `externalDomain` 划分不同（CN 无
`externalDomain` 拆分，global 有 `iOADomain` / `cloudHostedDomain`）。realm 隔离
在本仓库 `internal/auth/` 的 `realm` 判定与本常量组的对应关系是每次调研的
首要核对项。
