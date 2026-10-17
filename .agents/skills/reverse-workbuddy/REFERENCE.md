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

## 已知数据形态（2026-10-17 核对）

| Realm | 包版本 | `cli/product.json` `productName` | `endpoint` | `authentication.id` |
| --- | --- | --- | --- | --- |
| `cn` | 5.8.2.41111488-b8f6c953 | WorkBuddy | https://www.workbuddy.cn | workbuddy-desktop |
| `global` | 5.7.6.40488862-3e43260f | WorkBuddy AI | https://www.workbuddy.ai | workbuddy-desktop-ai |

CN 与 global 的 `internalDomain` / `externalDomain` 划分不同（CN 无
`externalDomain` 拆分，global 有 `iOADomain` / `cloudHostedDomain`）。realm 隔离
在本仓库 `internal/auth/` 的 `realm` 判定与本常量组的对应关系是每次调研的
首要核对项。
