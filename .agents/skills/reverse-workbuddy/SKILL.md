---
name: reverse-workbuddy
description: >
  解包 WorkBuddy 官方分发包（.tmp/workbuddy/ 下 cn / global 各版本 dmg 与
  app-asar），调研客户端协议面并与本仓库 internal/upstream/ 实现做差异比对。
  Use when investigating new client releases, upstream protocol drift,
  realm/version differences, or re-verifying the current protocol baseline.
  不用于 git 历史形态的上游同步（那走 sync-upstream）。
---

# 逆向调研 WorkBuddy 客户端协议

把「官方客户端发了什么」变成可核对的证据链，并与本仓库 `internal/upstream/`
的实现逐项比对。结论必须带 `文件:行`（客户端侧）或 `path:line`（本仓库侧）证据；
无证据的推断标注为假设，不当事实写。

## Locked

- 官方分发包只读：解包到**新目录**，不原地改动 `.tmp/workbuddy/<realm>/<ver>/`
  下的 dmg / app-asar / extracted。asar 提取用 `scripts/extract_asar.mjs`
  （`node scripts/extract_asar.mjs <app.asar> <unpackedRoot> <outDir>`）。
- `.tmp/` 已被 gitignore；调研产物只落 `.tmp/`，**绝不**把 dmg / asar 二进制或
  解包树提交进版本库。凭证（`auths/`）、状态（`data/`）同样不进库、不进报告。
- 报告中的端点、常量、header 名可原文照录；**token、Cookie、账号标识**不照录，
  涉及处用 `<uid>` / `<token>` 占位。
- 事实分层：客户端包内证据 = 事实；抓包实测 = 事实（注明来源）；推断 = 假设。
- 调研产出差异结论若要落到代码，改的是 `internal/upstream/`（适配在上游侧做，
  不依赖上游修复，见 `AGENTS.md` 上游同步策略）。

## Open

- 调研哪两个（或哪一组）版本；只调研不落地、还是给出适配 patch，由用户定。
- 报告存放路径（默认 `.tmp/workbuddy-diff/<YYYY-MM-DD>-<a>_vs_<b>.md`）。

## Do not

- 不运行官方客户端二进制或登录任何账号；只用静态包内证据。
- 不 curl 上游端点验证「是否存在」（除用户明确要求的实测，且需走已有凭证路径）。
- 不为旧版本/旧 realm 形态加兼容层；只回答当前路径的差异。
- 不把本报告的结论搬进 `README.md` / `UPSTREAM.md`（那两处只收用户可见行为与
  溯源变更；本报告的存放地就是 `.tmp/`）。

## 工作流

1. **定位与解包**。确认 `.tmp/workbuddy/<realm>/<version>/` 下已有产物：
   - `app-asar/`：已解包的 ASAR 树（直接可用）；
   - `asar-extract.mjs`：同目录提取脚本，缺 `app-asar/` 时对 `WorkBuddy.app`
     内的 `app.asar` 重跑：
     `node asar-extract.mjs <path/to/app.asar> <app.unpacked> <outDir>`
     （unpackedRoot 用 `.app/Contents/Resources/app.unpacked`，缺则空目录）。
   - `extracted/`：dmg 挂载拷贝出的 `.app`（取 `Contents/Resources/` 下的
     `app.asar` 与 `app.unpacked`）。
 2. **粗扫协议面**（每次调研必做，秒级）：

    ```bash
    node .agents/skills/reverse-workbuddy/scripts/protocol_sweep.mjs --root .tmp/workbuddy
    # 需要落盘或跨版本比对时加 --out / --compare（见 scripts 头注释）
    ```

   扫描 `main/cli/renderer/preload` 下 `*.js|mjs|cjs` 的端点路径字面量、
   `X-*` 头族与 UA 模板，汇总成 JSON。压缩主进程是单行大文件，rg 逐条 grep
   慢且噪声大，先用本脚本拿集合。
3. **取权威事实源**（按优先级，命中即记，冲突时以更靠上游者为准）：
   - `<app-asar>/cli/product.json`：`endpoint` / `officialEndpoints` /
     `authentication.id` / `authentication.type` / `authentication.attributes`
     （含 `internalDomain` / `externalDomain` / `iOADomain` / `prefixPath`）。
     这是 realm 分域的权威声明。
   - `<app-asar>/main/*.js`：出站端点路径字面量（`/v2/...`、`/v3/config`、
     `plugin/auth/...`、`/activity/...`）。先用 `protocol_sweep.mjs` 拿集合，
     再对具体端点 `rg -o '.<前缀>.<端点>.{200}'` 看调用上下文（压缩单行）。
   - `<app-asar>/renderer/assets/esm-*.js` 与 `*-*.js` chunks：渲染层请求头族
     （如 `X-AI-*`），判断该形态请求是否真实出网。
   - `<app-asar>/node_modules/openai/`：若存在，说明 CLI 走 OpenAI SDK 形态，
     `X-Stainless-*` 族即来自该 SDK（`internal/detect-platform.js` 有字面量）。
   - `<app-asar>/main/server.js` 等：`/v2/report` 上报指纹字段
     （ideName/extName/product/commit/releaseDate）。
4. **与本仓库实现比对**。事实源锚点：
   - realm 分域常量与 UA：`internal/upstream/headers.go`（`cnClientVersion`
     / `globalClientVersion` / `cnCliVersion` / `globalCliVersion`、
     `originRefererCN/Global`、`userAgent` 三段形态）。
   - chat 路径与 base：`internal/upstream/client.go`（`ChatBaseCN` /
     `ChatBaseGlobal`、`chatCompletionsPath`、`chatPaths`、`/v3/config`）。
   - billing 域：同文件 `billingMeterPaths` / `checkinMeterPaths`，
     `internal/upstream/trial.go`、`requestusage.go`、`report.go`。
   - 任务域：`internal/upstream/tasks.go`、`streak.go`、`blackcat.go`、
     `travel` 相关；`internal/scheduler/` 调用侧。
   - 桌面指纹上报：`internal/upstream/desktop.go`（`cnDesktopCommit` /
     `globalDesktopCommit` 等固定指纹）。
   - 注册激活：`internal/upstream/global_register.go`（链路注释即此前逆向结论）。
5. **写差异报告**到 `.tmp/workbuddy-diff/<date>-<a>_vs_<b>.md`，章节：
   - 对比对象（版本目录、realm、`product.json` endpoint/auth id）。
   - 端点集差异（新增 / 消失 / 路径迁移，如 `/billing/meter/*` 有无 `/v2` 前缀）。
   - 请求头族差异（桌面三段 UA、`X-IDE-*`、`X-Agent-*`、`X-Conversation-*`、
     `X-AI-*`、`X-Stainless-*`、traceparent/b3 族）。
   - 本仓库未覆盖项（客户端有、本仓库未实现或形态不同）。
   - 落地建议（哪个 `internal/upstream/*.go` 常量/路径要改，KISS 优先）。

   粗扫答不上的问题（某 header 挂哪个模块的哪条请求、两版本同一端点的调用方
   是否同源、DMG 成员清单）再上 REA 深挖，编排见
   `references/rea-toolchain.md`（先 summary 后翻页，全程只读）。
6. **验证**。只做静态调研时无需构建；若落地代码改动：
   `go build ./... && go test ./internal/upstream/...`，头部常量改动补
   `internal/upstream/desktop_test.go` / `headers` 相关用例，再
   `make fmt-check`。

## Handoff

- 协议溯源文案要更新时改 `internal/upstream/UPSTREAM.md`；配置项变化同步
  `config.example.json`；用户可见行为改 `README.md`。
- git 历史形态的上游同步（前序 fork 提交）走 `sync-upstream` 技能；本技能的
  「上游」指官方客户端二进制，两者不是同一物。

## Read as needed

- `references/protocol-anchors.md`：协议面事实源与本仓库 `path:line` 锚点速查。
- `references/rea-toolchain.md`：REA 静态分析编排（ASAR 应用图、版本比对、
  抓包归档归纳）与约束。默认先用 `scripts/protocol_sweep.mjs`，不够再上 REA。
- `scripts/extract_asar.mjs`：ASAR 提取；`scripts/protocol_sweep.mjs`：粗扫。

## Verify

- 报告每条差异都能追到一个客户端文件位置或一个本仓库 `path:line`。
- `node scripts/extract_asar.mjs` 重跑结果与既有 `app-asar/` 目录一致
  （抽样比对若干文件字节）。
- `node scripts/protocol_sweep.mjs` 的端点集合与报告引用的端点集合一致。
