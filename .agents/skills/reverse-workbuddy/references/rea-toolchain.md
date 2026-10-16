# 工具链编排：把 REA 装成本仓库可用的静态调研放大器

REA（`.tmp/rea/`，npm 包 `rea-agents`）是**本地只读**的静态分析 CLI/MCP，
覆盖 Electron/ASAR 树、Apple 应用、archive 清单、网络抓包归档等。
本仓库的协议调研天然落在它的 ASAR / JavaScript 树上，但本仓库**不引入任何
Node 依赖**：REA 只以 `npx` 形式按需调用，不写进 `package.json`，不进版本库。

## Locked

- 只在本仓库**已有的解包树**（`.tmp/workbuddy/<realm>/<ver>/app-asar/`）上分析；
  不让 REA 触碰 `auths/`、`data/`、`config.json`。
- REA 输出是 Evidence（`normalized_result` + 局限/覆盖），其中
  「静态图推断」与「观测到的行为」必须分开表述。
- REA 文档与版本会漂移；用法以 `npx -y rea-agents@latest <cmd> --help`
  与本文记录的调用序列为准，不照抄记忆里的工具名。
- 报告若基于 REA Evidence，注明其 Evidence 记录落点（`.tmp/` 文件）与分析面
  （哪个 ASAR、哪个 realm/版本）。

## 何时用 REA

- 客户端 JS 单行压缩、模块关系靠 grep 难以回答（「某 header 在哪个模块的
  哪条请求上设置」「两个版本的同一端点调用方是否同源」）。
- 需要**两版本结构化 diff**（端点集合/模块图/导出形态）而非人工对比表。
- 需要 DMG / `.app` 的成员清单与指纹（哪个 binary、哪个 asar 对应哪个版本）。
- 有 HAR/SAZ/mitmproxy 归档时做请求面归纳（只读归档，不发起请求）。

`rg` / `node protocol_sweep.mjs` 就够的问题不要开 REA：先扫后挖。

## 编排（编排 = 先粗扫、再按需深挖，全程只读）

第 0 步：确认解包树存在（缺则按 `SKILL.md` 的 `extract_asar.mjs` 解包）。

```bash
ls .tmp/workbuddy/<realm>/<ver>/app-asar/cli/product.json
```

第 1 步（粗扫，必做）：本仓库自带脚本，秒出端点/头族/UA 模板统计。

```bash
node .agents/skills/reverse-workbuddy/scripts/protocol_sweep.mjs \
  --root .tmp/workbuddy --out .tmp/workbuddy-diff/<name>-sweep.json
node .agents/skills/reverse-workbuddy/scripts/protocol_sweep.mjs \
  --root .tmp/workbuddy --compare "cn/<verA>,global/<verB>" \
  --out .tmp/workbuddy-diff/<name>-cmp.json
```

第 2 步（深挖，按需）：REA ASAR 应用图。结果可能数百 MB，**先 summary 再翻页**。

```bash
npx -y rea-agents@latest analyze-javascript-application \
  /abs/path/to/.tmp/workbuddy/<realm>/<ver>/app-asar --json \
  > .tmp/workbuddy-diff/<name>-<realm>-evidence.json   # 只落 .tmp/
npx -y rea-agents@latest inspect-analysis-view .tmp/workbuddy-diff/<name>-view.json
# view 输入由管道从 evidence 生成（kind 见 docs），例如：
jq -c '{source:{kind:"inline",evidence:.},view:{kind:"summary"}}' \
  <evidence.json> > <name>-view.json
```

第 3 步（跨版本，按需）：两个版本各自分析一次后比对。

```bash
npx -y rea-agents@latest compare-application-versions \
  --left .tmp/workbuddy-diff/<name>-cn-evidence.json \
  --right .tmp/workbuddy-diff/<name>-global-evidence.json --json
# 单个导出形态差异用 compare-javascript-export-shapes（需显式 module+export 名）
```

第 4 步（DMG/.app 清单，按需）：只取成员清单与身份，不解码代码签名。

```bash
npx -y rea-agents@latest open-binary <path/to/dmg-or-app> --json   # provider 缺省
npx -y rea-agents@latest inspect-artifact .tmp/workbuddy-diff/<name>-inv.json
```

第 5 步（抓包归档，按需）：只读 HAR/SAZ，把请求面归纳成
method+origin+path-template 组，**不**回放任何请求。

```bash
npx -y rea-agents@latest inspect-web-network-capture <capture.har> --format har --json
```

## MCP 形态（可选）

REA 可注册成 MCP server（`npx -y rea-agents@latest doctor --client opencode --json`
先体检）。注册会写客户端配置，属客户端侧改动：需要用户明确授权后再做，且本仓库
`.opencode/opencode.json` 不主动加 REA 段。日常优先走上面 CLI 编排。

## Verify

- 实验结果可复现：同一证据文件二次 `inspect-analysis-view` 得到同一结论。
- 任何结论区分「REA 静态图说」与「本仓库粗扫说」，冲突时以更具体的证据为准，
  并在报告中写出两者出处。
