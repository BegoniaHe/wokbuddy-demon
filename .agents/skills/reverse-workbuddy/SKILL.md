---
name: reverse-workbuddy
description: >
  自动下载并解包 WorkBuddy 官方 Win32 分发包（.tmp/workbuddy/ 下 cn / global
  各版本 exe 与 app-asar；macOS 包仅辅助对照），调研客户端协议面并与本仓库
  internal/upstream/ 实现做差异比对，按需产出逐 surface 字面量的客户端指纹
  profile 并校验。Use when
  investigating new client releases, upstream protocol drift, realm/version
  differences, re-verifying the current protocol baseline, or generating /
  validating a client fingerprint profile.
  不用于 git 历史形态的参考 fork 审计（那走 sync-upstream）。
---

# 逆向调研 WorkBuddy 客户端协议

把「官方客户端发了什么」变成可核对的证据链，并与本仓库 `internal/upstream/`
的实现逐项比对。结论必须带 `文件:行`（客户端侧）或 `path:line`（本仓库侧）证据；
无证据的推断标注为假设，不当事实写。

## Locked

- **Win32 构建是唯一事实来源**（`win32-x64-user`）：协议事实、指纹常量、profile
  一律取 Win32 包；macOS 包只作辅助对照，不参与 `emit` 或代码常量。两者同版本号
  也可能有不同 commit/releaseDate，以 Win32 的 `cli/product.json` 为准。
- 官方分发包只读：解包到**新目录**，不原地改动 `.tmp/workbuddy/<realm>/<ver>/`
  下的 exe / dmg / app-asar / extracted。asar 提取用 `scripts/extract_asar.mjs`
  （`node scripts/extract_asar.mjs <app.asar> <unpackedRoot> <outDir>`）。
  解包树命名：`app-asar/` 始终放 Win32；macOS 对照放 `app-asar-macos/`。
- `.tmp/` 已被 gitignore；调研产物只落 `.tmp/`，**绝不**把 dmg / asar 二进制或
  解包树提交进版本库。凭证（`auths/`）、状态（`data/`）同样不进库、不进报告。
- 报告中的端点、常量、header 名可原文照录；**token、Cookie、账号标识**不照录，
  涉及处用 `<uid>` / `<token>` 占位。
- 事实分层：客户端包内证据 = 事实；抓包实测 = 事实（注明来源）；推断 = 假设。
- 指纹 profile 只写字面量（literal），不写模板、不按版本号拼接：每个值必须追到
  包内片段或抓包记录。找不到就标 `null` 并列入 `missing`，**不得**按「官方大概是
  这个形状」补一个。
- profile 必须过校验器才算产出（schema、交叉一致、`evidence` 逐字命中、控制字符
  检查）；校验不过的值是未生成状态，不得当作事实写进报告或提交。
- 调研产出差异结论若要落到代码，改的是 `internal/upstream/`（适配在上游侧做，
  不依赖上游修复，见 `AGENTS.md` 上游审计策略）。
- **动态验证默认关闭，仅在用户显式授权时启用**：运行官方客户端只用于验证静态包
  结论，不能替代静态证据；必须使用测试账号，不登录真实账号。运行时采集只落
  `.tmp/workbuddy-runtime/<realm>-<ts>/`，默认脱敏、不复制 auth/原始日志。
- **提权只走 pkexec**：需要 root 的抓包（tcpdump）由 `pkexec` 向桌面用户发起
  polkit 授权；不读取、不保存密码，不调用 sudo，不在非交互脚本里注入凭据。

## Open

- 调研哪两个（或哪一组）版本；只调研不落地、还是给出适配 patch，由用户定。
- 报告存放路径（默认 `.tmp/workbuddy-diff/<YYYY-MM-DD>-<a>_vs_<b>.md`）。

## Do not

- 默认不运行官方客户端二进制、不登录真实账号；只在用户显式授权并使用测试账号时，
  通过 `scripts/runtime_probe.mjs` 做 opt-in 运行时验证。静态证据仍是首选。
- 不 curl 上游端点验证「是否存在」（除用户明确要求的实测，且需走已有凭证路径）。
- 不为旧版本/旧 realm 形态加兼容层；只回答当前路径的差异。
- 不把本报告的结论搬进 `README.md` / `UPSTREAM.md`（那两处只收用户可见行为与
  溯源变更；本报告的存放地就是 `.tmp/`）。

## 工作流

1. **取最新安装包**。默认同时查 cn / global 的 Win32 包，下载到
   `.tmp/workbuddy/<realm>/<version>/`：

   ```bash
   # 先看将下载什么（推荐）
   node .agents/skills/reverse-workbuddy/scripts/download_latest.mjs --dry-run
   # 下载（已存在则跳过；--force 覆盖）
   node .agents/skills/reverse-workbuddy/scripts/download_latest.mjs
   # macOS 仅辅助对照，必须显式指定
   node .agents/skills/reverse-workbuddy/scripts/download_latest.mjs --realm all --platform darwin-arm64
   ```

   脚本查 `https://www.workbuddy.ai|www.workbuddy.cn/v2/update?platform=...`，
   默认 `workbuddy-win32-x64-user`。macOS 取接口同版本 `.dmg`（接口默认给
   `.zip`），只用于对照。需要历史版本时手工放置到同目录结构。
2. **定位与解包**。确认 `.tmp/workbuddy/<realm>/<version>/` 下已有产物：
   - `app-asar/`：**Win32** 已解包 ASAR 树（直接可用，事实源）；
   - `app-asar-macos/`：macOS 对照树（可选）；
   - Win32 exe 是 NSIS 包：`7zz x -y -o<tmp> <exe>`，再解
     `<tmp>/$PLUGINSDIR/app-64.7z`，最后对
     `resources/app.asar` + `resources/app.asar.unpacked` 跑
     `node scripts/extract_asar.mjs <app.asar> <unpackedRoot> <outDir>`；
   - `extracted/`：dmg 挂载拷贝出的 macOS `.app`（辅助）。
3. **粗扫协议面**（每次调研必做，秒级）：

    ```bash
    node .agents/skills/reverse-workbuddy/scripts/protocol_sweep.mjs --root .tmp/workbuddy
    # 需要落盘或跨版本比对时加 --out / --compare（见 scripts 头注释）
    ```

   扫描 `main/cli/renderer/preload` 下 `*.js|mjs|cjs` 的端点路径字面量、
   `X-*` 头族与 UA 模板，汇总成 JSON。压缩主进程是单行大文件，rg 逐条 grep
   慢且噪声大，先用本脚本拿集合。
4. **取权威事实源**（按优先级，命中即记，冲突时以更靠上游者为准）：
   - `<app-asar>/cli/product.json`：`endpoint` / `officialEndpoints` /
     `authentication.id` / `authentication.type` / `authentication.attributes`
     （含 `internalDomain` / `externalDomain` / `iOADomain` / `prefixPath`）。
     这是 realm 分域的权威声明。
   - `<app-asar>/main/*.js`（Win32）：出站端点路径字面量（`/v2/...`、`/v3/config`、
     `plugin/auth/...`、`/activity/...`）。先用 `protocol_sweep.mjs` 拿集合，
     再对具体端点 `rg -o '.<前缀>.<端点>.{200}'` 看调用上下文（压缩单行）。
   - `<app-asar>/renderer/assets/esm-*.js` 与 `*-*.js` chunks：渲染层请求头族
     （如 `X-AI-*`），判断该形态请求是否真实出网。
   - `<app-asar>/node_modules/openai/`：若存在，说明 CLI 走 OpenAI SDK 形态，
     `X-Stainless-*` 族即来自该 SDK（`internal/detect-platform.js` 有字面量）。
   - `<app-asar>/main/server.js` 等：`/v2/report` 上报指纹字段
     （ideName/extName/product/commit/releaseDate）。
5. **与本仓库实现比对**。事实源锚点：
   - realm 分域常量与 UA：`internal/upstream/headers.go`（`originRefererCN/Global`、
     `userAgent`）与 `internal/clientprofile`（出站 UA / IDE 版本来自部署 profile，
     源码不内置版本常量）。
   - chat 路径与 base：`internal/upstream/client.go`（`ChatBaseCN` /
     `ChatBaseGlobal`、`chatCompletionsPath`、`chatPaths`、`/v3/config`）。
   - billing 域：同文件 `billingMeterPaths` / `checkinMeterPaths`，
     `internal/upstream/trial.go`、`requestusage.go`、`report.go`。
   - 任务域：`internal/upstream/tasks.go`、`streak.go`、`blackcat.go`、
     `travel` 相关；`internal/scheduler/` 调用侧。
   - 桌面指纹上报：`internal/upstream/desktop.go`（`desktopFingerprint`；
     commit / releaseDate 来自部署 profile）。
   - 注册激活：`internal/upstream/global_register.go`（链路注释即此前逆向结论）。
6. **写差异报告**到 `.tmp/workbuddy-diff/<date>-<a>_vs_<b>.md`，章节：
   - 对比对象（版本目录、realm、`product.json` endpoint/auth id）。
   - 端点集差异（新增 / 消失 / 路径迁移，如 `/billing/meter/*` 有无 `/v2` 前缀）。
   - 请求头族差异（桌面三段 UA、`X-IDE-*`、`X-Agent-*`、`X-Conversation-*`、
     `X-AI-*`、`X-Stainless-*`、traceparent/b3 族）。
   - 本仓库未覆盖项（客户端有、本仓库未实现或形态不同）。
   - 落地建议（哪个 `internal/upstream/*.go` 常量/路径要改，KISS 优先）。

   粗扫答不上的问题（某 header 挂哪个模块的哪条请求、两版本同一端点的调用方
   是否同源、DMG 成员清单）再上 REA 深挖，编排见
   `references/rea-toolchain.md`（先 summary 后翻页，全程只读）。
7. **产出客户端指纹 profile（按需）**。用户要更新部署指纹或核对协议 facts 时才做，
   纯调研可跳过。流程：
   - 只从 Win32 `app-asar/` 取 `references/client-profile.md` 的 surface 表逐项
     **包内字面量**：
     `chatUA` / `desktopUA` / `billingUA` / `xIdeVersion` / `desktopIdeVersion` /
     `desktopExtVersion` / `loginPlatform`（`billingUA` 仍属 schema 必填但已无消费方，
     按包内字面量照填即可）。不从 `version` 拼任何串；
     `desktopExtVersion` 仍须核对为与 `source.version` 相等（产品版本）。
   - `chatUA` 取 CLI 层 `cli/dist/codebuddy-lite-wb.mjs` 的
     `UserAgentHttpInterceptor.buildUserAgent` 输出；`main/server.js` 是 daemon/VLM
     面（`/v2/report` 走 desktop surface），不能作为 `chatUA` 证据。
   - 双源核对：Win32 `product.json` 的版本与 commit、包内 bundle 版本字面量、
     update 接口返回版本，三处必须一致；不一致就停下报告，不挑「看起来对的」那个。
     macOS 同版本包只用于旁证，不得覆盖 Win32 的 commit/releaseDate。
   - billing meter 族的 UA 只能靠抓包（官方该面无桌面 UA 拦截器，值是 axios 默认串），
     落 `billingMeterUA` 且 `source: "traffic"`；缺失时回落 `desktopUA`。
   - 为每个值写 `evidence`：`package` 来源给相对文件路径 + 可逐字 grep 到的
     snippet；抓包来源写 `source: "traffic"` 并在 `note` 记日期与端点。
   - 跑校验器；缺必需 surface 即视为生成失败，不落盘。
   - 落点：真实 profile 只落用户部署的 `data/client-profiles/<realm>.json`
     （gitignore，server 启动时从 `state_file` 同目录读取），**不入库**（入库即
     变成内置版本表）。仓库侧只提交测试 fixture：
     `internal/upstream/testdata/profile/<realm>.json` 与 `clientprofile_test.go`
     的假值 fixture（版本填 `9.9.x` / `0.0.0`，仅供 schema/校验测试）。
     解包树和原始包仍只留 `.tmp/`。协议 facts（端点/头族/product.json/模型清单）
     仍留在代码与 `UPSTREAM.md`；新增 baseline 文件需另起 PR，由人类维护者批准。
8. **验证**。只做静态调研时无需构建；若落地代码改动：
   `go build ./... && go test ./internal/upstream/... ./internal/clientprofile/...`，
   profile / 头部行为改动补 `internal/clientprofile` 与
   `internal/upstream/desktop_test.go` / `headers` 相关用例，再
   `make fmt-check`。

## Opt-in runtime verification (Wine)

静态包无法回答「真实 chat 请求头/UA」时，用户显式授权且使用测试账号后，可运行
`scripts/runtime_probe.mjs`。它是唯一允许启动官方客户端的入口，仍遵守上面的脱敏
与提权边界。

```bash
# 只采集启动/CDP/客户端日志，不发消息
node .agents/skills/reverse-workbuddy/scripts/runtime_probe.mjs --realm cn

# 需要 chat 证据：显式确认会消耗测试账号额度
node .agents/skills/reverse-workbuddy/scripts/runtime_probe.mjs --realm cn \
  --send "ping" --confirm-spend --duration 20

# 需要 root 抓包：由 pkexec 弹出 polkit 授权，不读取 sudo 密码
node .agents/skills/reverse-workbuddy/scripts/runtime_probe.mjs --realm cn --capture
```

规则：

- 默认复用现有 `WINEPREFIX` 的登录态；`--isolated-user-data` 只验证启动/公开端点，
  不能验证 chat（未登录）。
- 产物隔离在 `.tmp/workbuddy-runtime/<realm>-<ts>/`：`requests.jsonl`（URL 去
  query、白名单头、会话/请求 ID 打码）、`summary.json`（端点/模型/事件）、可选
  `traffic.pcap`（600，归当前用户）。原始 `netlog.json` / `runtime.log` 默认删除；
  `--keep-raw` 才保留，且不得提交。
- 不记录 `Authorization`、Cookie、`X-Refresh-Token`、`X-Userinfo`、`X-User-Id`
  值；账号 UID、昵称、prompt、响应正文不写入摘要。
- 证据等级：代码路径/包内字面量 > 运行时日志 > TLS 密文。CDP 看不到主进程/CLI
  请求时，不要把它当成「没有发这个头」。
- 提权只走 `pkexec`；脚本不包含 sudo 密码，也不在非交互环境注入凭据。
- 结束后确认进程已停止；复用现有 prefix 时不执行全局 `wineserver -k`。

已知无效路径（不要再重复试）：

- `NODE_OPTIONS=--require=...` 会被 Electron 打包应用拒绝。
- `NODE_DEBUG=http,https` 只输出 socket 内部信息，不打印请求头。
- renderer CDP 看不到主进程/CLI 发出的 chat、`/v2/report`、`/v3/config`。
- chat 实际由 `cli/dist/codebuddy-lite-wb.mjs` 的 `axiosToFetchAdapter` 发出，
  头构造看 `buildConversationHeaders` / `UserAgentHttpInterceptor.buildUserAgent`；
  profile 不要把 daemon surface（`main/server.js`）和 CLI surface 混用。

## Handoff

- 协议溯源文案要更新时改 `internal/upstream/UPSTREAM.md`；配置项变化（含 profile
  路径/字段）同步 `config.example.json`；用户可见行为改 `README.md`。
- git 历史形态的参考 fork 审计（`misakano7545/workbuddy2api-panel` 提交）走
  `sync-upstream` 技能；本技能的「上游」指官方客户端二进制，两者不是同一物。

## Read as needed

- `references/client-profile.md`：指纹 profile 的 schema、surface 表、来源分级、
  校验规则与落点。生成/校验 profile 前必读。
- `references/protocol-anchors.md`：协议面事实源与本仓库 `path:line` 锚点速查。
- `references/rea-toolchain.md`：REA 静态分析编排（ASAR 应用图、版本比对、
  抓包归档归纳）与约束。默认先用 `scripts/protocol_sweep.mjs`，不够再上 REA。
- `references/mechanical-tools.md`：跨版本机械整理脚本（清单 / 依赖图 / 导出面 /
  字面量 / 格式化 / 重命名 / 拆块）的用法、推荐顺序与事实边界。需要跨版本
  结构化对比时先读它，不要人肉读压缩单行文件。
- `scripts/download_latest.mjs`：查官方更新接口并下载 cn / global 最新包。
- `scripts/extract_asar.mjs`：ASAR 提取；`scripts/protocol_sweep.mjs`：粗扫。
- `scripts/runtime_probe.mjs`：opt-in Wine 运行时探针；隔离输出、白名单头、
  默认删除原始日志；抓包提权只走 `pkexec`。

## Verify

- 报告每条差异都能追到一个 Win32 客户端文件位置或一个本仓库 `path:line`；
  macOS 证据必须标注「辅助对照」，且不得支撑 `emit` 值。
- `node scripts/download_latest.mjs --dry-run` 给出的版本与 `.tmp/workbuddy/`
  下解包目录所用版本一致（或明确说明只调研历史版本）。
- `node scripts/extract_asar.mjs` 重跑结果与既有 `app-asar/` 目录一致
  （抽样比对若干文件字节）。
- `node scripts/protocol_sweep.mjs` 的端点集合与报告引用的端点集合一致。
- profile 每个 `source: package` 的 `evidence.snippet` 能在对应文件里逐字命中
  （校验器或 `rg -F` 均可）；任一必需 surface 为 `null` 时不得声称已生成。
- 抽查 `chatUA` / `desktopUA` 与包内字面量逐字相同，确认没有经过版本号拼接。
- 运行 `runtime_probe.mjs` 后，`summary.json` 不含 token/UID/Cookie；原始
  NetLog/stdout 默认已删除（或 `--keep-raw` 且明确不提交）。
- `--capture` 只通过 `pkexec` 提权；脚本中不存在 sudo 密码或明文凭据。
