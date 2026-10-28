# 客户端指纹 profile

生成/校验部署指纹快照的规则。目标是让「本仓库发出的每个指纹」都能追到某个真实
官方构建的某个字面量，从而不会出现现实中不存在的版本 + 形状组合。

## 核心原则

- **只搬运，不合成**。profile 存的是出站字符串的最终值，不是模板；运行时零拼接。
- **官方改形状时靠重新生成跟随**，不靠代码里的格式假设。所以本文件不规定 UA 的
  形状，只规定值的来源与校验方式。
- **旧 profile + 新官方 = 旧客户端**。冻结快照是合法流量；`新版本号 + 旧形状` 才是
  伪造。因此宁可拒绝启动，也不得就地改一个字段。
- **只认 Win32 构建**（`win32-x64-user`）。同一版本号的 macOS 包可能有不同
  commit/releaseDate，只能辅助对照，不得作为 `source` 或 `evidence` 的事实源。
- 生成由 coding CLI 完成（本技能），机械采集与语义归类分工：能被正则稳定拿到的
  端点/头族/`product.json` 走 `scripts/protocol_sweep.mjs`，需要理解打包结构的
  字面量由 agent 判定。

## Schema（`schema: 1`）

```json
{
  "schema": 1,
  "source": {
    "realm": "cn",
    "packageUrl": "https://download.codebuddy.cn/workbuddy/saas/win32-x64-user/WorkBuddy-win32-x64-user-5.8.3.41334564-db0e9b51.exe",
    "sha256": "<Win32 安装包的 64 位小写 hex>",
    "packageVersion": "5.8.3.41334564",
    "version": "5.8.3",
    "cliVersion": "2.161.0",
    "commit": "<Win32 product.json 的 40 位小写 hex>",
    "releaseDate": 1792029492880,
    "observedAt": "2026-10-22T10:00:00Z",
    "generatedBy": "reverse-workbuddy@<repo-sha>"
  },
  "emit": {
    "chatUA": "…",
    "desktopUA": "…",
    "billingUA": "…",
    "xIdeVersion": "…",
    "desktopIdeVersion": "…",
    "desktopExtVersion": "…",
    "loginPlatform": "workbuddy",
    "loginUA": "WorkBuddy/5.8.3 WorkBuddy/5.8.3 CLI/2.161.0"
  },
  "missing": [],
  "evidence": {
    "chatUA": {"source": "package", "file": "cli/dist/codebuddy-lite-wb.mjs", "snippet": "…"},
    "desktopUA": {"source": "package", "file": "main/module-base.js", "snippet": "…"},
    "billingUA": {"source": "package", "file": "main/tar.js", "snippet": "…"},
    "xIdeVersion": {"source": "package", "file": "main/server.js", "snippet": "…"},
    "desktopIdeVersion": {"source": "package", "file": "main/module-base.js", "snippet": "…"},
    "desktopExtVersion": {"source": "package", "file": "main/module-base.js", "snippet": "…"},
    "loginPlatform": {"source": "package", "file": "cli/product.json", "snippet": "…"},
    "loginUA": {"source": "package", "file": "main/module-base.js", "snippet": "…"}
  }
}
```

说明：

- `emit` 的每个值都是**最终出站字面量**。`xIdeVersion` / `desktopIdeVersion` 各自
  独立取值，不从 `version` 派生（防止官方解耦后我们仍强行一致）；`desktopExtVersion`
  当前必须与 `source.version` 相等（官方 `pluginVersion` 恒取 `resolveProductVersion()`），
  仍从包内字面量提取，不自行拼装。
- `version` / `cliVersion` 只做交叉校验与可读性，不是运行时拼接素材。
- `loginUA` 非必需，但 Win32 登录走 `restOperations`，与 `desktopUA` 共用同一 UA
  拦截器，因此当前应填 `desktopUA` 最终值；确实取不到时写 `null` 并列入 `missing`。
  此时登录（CLI 与面板）会显式失败，需重新生成或补上 `loginUA`；不得为了让
  schema 好看而猜一个。

## Surface 表

| key | 必需 | 用途 | 典型来源 |
| --- | --- | --- | --- |
| `chatUA` | 是 | chat / refresh / FetchModels | CLI 层 `cli/dist/codebuddy-lite-wb.mjs` 的 `UserAgentHttpInterceptor.buildUserAgent` 最终输出；第二段品牌取 `cli/product.json` 的 `applicationName`（CN `WorkBuddy` / global `workbuddy-ai`） |
| `desktopUA` | 是 | `/v2/report`、白名单类上报 | Win32 `main/*.js`、桌面端构建串 |
| `billingUA` | 是 | 仅 billing 域 report / banner 这类显式单段 UA 面（billing meter 走 `restOperations` 全局拦截器，默认同 `desktopUA`） | Win32 包内显式覆写 |
| `xIdeVersion` | 是 | `X-Ide-Version` | 与 `chatUA` 同源的版本 |
| `desktopIdeVersion` | 是 | 上报事件 `ideVersion` | Win32 桌面端构建串 |
| `desktopExtVersion` | 是 | 上报事件 `extVersion`（须等于 `source.version`） | 官方 `pluginVersion`（产品版本） |
| `billingMeterUA` | 否 | billing meter 族（`/billing/meter/*`）的出站 UA。官方该面没有桌面 UA 拦截器，UA 是 axios 库自身默认串，只能从抓包取值；缺省回落 `desktopUA` | 2026-10-28 mitmproxy 抓包（`/billing/meter/get-user-resource-*`）→ `evidence.source = "traffic"` |
| `loginPlatform` | 否 | OAuth `platform=` 参数 | `plugin/auth/state` 调用处 |
| `loginUA` | 否 | 登录流 UA | 与 `desktopUA` 同源（`restOperations` 拦截器） |

`chatUA` 的 chat 实际由 CLI 层 `cli/dist/codebuddy-lite-wb.mjs` 的
`UserAgentHttpInterceptor.buildUserAgent` 构造；三段依次是
`platform / applicationName / userAgentExtension`，其中 `platform` 由 sidecar 注入
固定为 `WorkBuddy`，`applicationName` 取 `cli/product.json`（CN `WorkBuddy` /
global `workbuddy-ai`），`userAgentExtension` 为 `CLI/<cli>`。
因此 `evidence.chatUA.file` 必须是 `cli/dist/codebuddy-lite-wb.mjs`；
`main/server.js` 是 daemon/VLM 面，其中的 UA 构造不能作为 `chatUA` 证据。
`desktopUA`（`/v2/report`）与 `chatUA` 是两条不同 surface，不得互相顶替。

## 来源分级（`evidence.<key>.source`）

1. `package`：包内字面量。必须给 `file`（相对解包根）与 `snippet`（能逐字 grep
   到的最短唯一片段）。
2. `traffic`：抓包实测。`note` 必须给日期与端点，值仍需与包内版本事实相符；
   不要求 `file` / `snippet`，给了 `file` 也只校验为相对路径。
禁止 `default` / `guessed` 之类凭空来源。找不到就给 `null` + `missing`。

`billingMeterUA` 是纯抓包面（包内没有该 UA 字面量，值来自 axios 运行时默认），
出现时必须给 `traffic` 证据（`note` 写抓包日期 + 端点）；没有就整个字段留空，
出站回落 `desktopUA`，不要用旧版本的已知值拼一个。

## 校验规则（Go validator，启动期与测试共用）

1. `schema == 1`；拒绝未知字段（`DisallowUnknownFields`）。
2. `source.realm ∈ {cn, global}`；`packageUrl` 必须是 Win32 安装包：
   cn → `download.codebuddy.cn/workbuddy/saas/win32-x64-user/…`，
   global → `download.codebuddy.ai/workbuddy/saas/win32-x64-user/…`。
   macOS / Linux 包 URL 一律拒绝。
3. `sha256` 为 64 位小写 hex，与 `packageUrl` 指向的实际安装包一致（exe）；
   `commit` 为 40 位小写 hex；`releaseDate` 落在 `[2024-01-01, now+7d]`。
4. 交叉一致：`version` 必须以子串形式出现在 `chatUA` / `desktopUA` /
   `xIdeVersion` / `desktopIdeVersion` 中，且 `desktopExtVersion` 必须与 `version`
   完全相等（官方 `pluginVersion` 恒取产品版本）；`cliVersion` 出现在 `chatUA` /
   `desktopUA` 中。`loginUA` 不参与（CLI 段可能不同）。
5. 必需 surface 非空；`null` 的 key 集合必须与 `missing` 完全一致。
6. 所有 `emit` 值：无 `\r` / `\n` / 其它控制字符，长度 ≤ 1024 字节，无首尾空白。
   值会进 HTTP header，这条是硬性安全约束。
7. `evidence` 覆盖每个非 `null` 的 `emit` key；`file` 必须是相对路径（拒绝绝对
   路径与 `..`）。路径存在性与 `snippet` 逐字命中由生成方在落盘前校验（Go 运行期
   不读解包树）。
8. 生成方还需核对同一 profile 内 `source.realm` 与所有 `evidence.file` 所属 realm
   目录一致（包内不允许混用另一 realm 的文件）。
9. `evidence.file` 必须来自 Win32 解包树（`app-asar/`）；来自
   `app-asar-macos/` 的证据无效。以上两条同样是生成期检查，不进入运行期校验器。

## 反伪造硬规则

- 不按历史格式拼 UA，不按 `version` / `cliVersion` 组装任何 `emit` 值。
- 双源规则：Win32 `product.json` 版本与 commit、bundle 内版本字面量、update
  接口返回版本，三处必须一致。不一致即停止，输出冲突项。macOS 只用于旁证，
  不得覆盖 Win32 的 commit/releaseDate。
- 一个必需 surface 找不到字面量 → 生成失败，不落盘、不提交。
- 运行期校验器不读解包树，只校验结构、Win32 来源与 header 安全边界；
  `evidence.file` 是否存在、`snippet` 是否逐字命中，由生成方在落盘前完成。
- 不得用「上一个版本的已知值」兜底。旧值只能来自用户仍然持有的旧 profile。
- `evidence.snippet` 必须唯一且逐字，不许改写、拼接或跨文件。

## 落点与所有权

| 产物 | 位置 | 是否入库 |
| --- | --- | --- |
| 真实 profile | 用户部署 `data/client-profiles/<realm>.json` | 否（gitignore） |
| 测试 fixture | `internal/upstream/testdata/profile/<realm>.json` | 是（假值） |
| 解包树 / 原始包 | `.tmp/workbuddy/` | 否 |

- 运行期由 server 从 `state_file` 同目录的 `client-profiles/` 读取；读取走
  `internal/safefile`（正文非合法 JSON 时回落 `.bak`）。cn 恒需、global 仅在
  `global.enabled` 时需要；缺失或语义校验失败都拒绝启动。仓库源码不保留任何
  版本 / commit / releaseDate 兜底常量。
- fixture 的版本必须是 `9.9.x` / `0.0.0` 一类明显假值：真实值一旦入库就会变成
  内置回退表，并随官方发版迅速腐坏。
- 协议 facts（端点/头族/product.json/模型清单）仍留在代码与 `UPSTREAM.md`；
  如需新增 baseline 文件，另起 PR 由人类维护者批准。
- 部署级派生盐**不进 profile**（profile 可能被贴到 issue），放用户本地配置。

## 与运行时代码的边界

- profile 只装「只能从包取到的出站字面量」。endpoint、路径、模型目录、倍率、
  超时等留在代码与 `UPSTREAM.md`——它们的失败需要改代码，不是换数字。
- `upstream.client_version` / `upstream.cli_version` 仍是显式逃生门，只覆盖 UA /
  `X-Ide-Version` 的版本段；profile 仍为启动必需项，构建指纹（commit / releaseDate /
  desktopExtVersion）只能来自 profile。默认不要设置这两个字段，版本一致性由「整份
  profile 来自同一构建」保证，而不是由多个字段各自正确保证。

## 漂移与更新节奏

- 检测分两级：L0 每天只查 Win32 update 接口版本（纯 HTTP，不下载），版本变化
  即开 issue；L1 由本技能在维护者机器上执行，产出新 profile 与必要的代码 /
  `UPSTREAM.md` 差异。
- CI 不下载安装包、不接触凭证、不跑真实 probe。
- 冻结 profile 的失败模式是「看起来像没升级的旧客户端」，不是自相矛盾指纹；
  因此人工节奏可以慢，但不能就地改字段。
