# Research protocol

Load this before writing `RESEARCH.md`. Search by domain concept, not
only the Issue's wording. Facts about this tree are the agent's job;
do not ask the user what a grep can answer.

## Language

Default `RESEARCH.md` to Simplified Chinese and `PLAN.md` to English for the
executor. Respect an explicitly requested artifact language; keep the English
headings required by the validator.

Keep verbatim: paths, symbols, commands, APIs, error strings, SHAs,
URLs, and the English H2 headings listed in the shape below. On first
use of a domain term, write `中文（English）`. Later mentions may use
either form. Do not machine-translate the Issue body; quote it, then
explain.

When research completes, link the report and summarize the relevant findings
in the user's language. Do not paste the full artifact into chat.

Scale each section to its evidence. A short section is sufficient when it
answers the question; package names alone do not establish behavior.

## Read the request

1. Title, body, labels, comments, linked PRs. For a PR treated as a
   request, read the diff too.
2. Parse prior Human note / Agent note so resolved questions are not
   re-asked.
3. Classify: `bug`, `enhancement`, `task`, or mixed. Mixed work that
   bundles independent capabilities needs a capability map first.
4. Classify **depth** (`small` / `medium` / `large` / `complex`) from
   `references/probe.md`. Record it in `RESEARCH.md`.

## Coverage ledger

Before a deep dive, write 2–7 **current-state** questions. Each row
asks what exists, where it lives, how it behaves, or how it is tested.
Do not encode the requested patch in the question.

Typical dimensions (keep a row only when it is material):

| Angle       | Question to answer                                   |
| ----------- | ---------------------------------------------------- |
| Structure   | Which package or owner owns this surface?            |
| Entrypoints | What starts the flow (HTTP route, CLI, cron, panel)? |
| Flow        | How state or a message moves through the system?     |
| Contracts   | Types, config fields, events, upstream payloads?     |
| Validation  | Which tests and fixtures prove the current behavior? |
| History     | Why is it this way (`git log`, UPSTREAM.md)?         |

A row is done only when it is `answered` with `path:line` (and a test
path, or an explicit "no test") or `open:` with the missing evidence
and why that gap matters. Empty search is "this lane cannot see it",
not "it is absent". Report the queries and at least one synonym or
second surface before claiming absence.

After the table, add one short paragraph per row: what you read, what
it does, and the cite. Do not leave the table as the only answer.

## Parallel lanes

For a non-trivial request, fan out disjoint lanes rather than one
serial skim. Typical first wave:

1. Target-flow: inbound trigger to effect.
2. Validation and history: tests, docs, recent commits.
3. Cross-check: alternate paths, flags, generated code, fork
   invariants.

Give each lane an exact question, directories to prefer, and a
required return of `claim`, `path:line`, `verdict`, `confidence`.
Do not spawn two agents onto the same vague question.

The parent re-reads every load-bearing `path:line`. Subagent
summaries are leads. Merge by hunting conflict first; do not average
disagreement away. Fold lane results into Coverage ledger, Current
behavior, and Search log. Do not replace those sections with a lane
dump.

## Redundancy

Search the tree for an existing implementation of the requested
behavior. Report the queries and the paths inspected.

Typical seams:

- Gateway routes / model resolution: `internal/server/`
- Account selection / breaker / cooldown: `internal/pool/`
- Scheduled tasks: `internal/scheduler/`
- Upstream protocol / SSE / sanitize: `internal/upstream/`
- Panel HTTP + embedded UI: `internal/panel/`
- Auth and realm: `internal/auth/`, `internal/httpauth/`
- Config load / hot-apply / restart list: `cmd/server/`

If the behavior already exists, stop. Point to it. Do not write a
rebuild plan.

## Prior rejection

Check, in order, and quote the hit or write `未找到`:

1. `AGENTS.md` do-not-restore and security invariants
2. `internal/upstream/UPSTREAM.md` lineage and fork-owned evolution
3. Closed Issues/PRs with `wontfix`, duplicate, or "will not restore"
4. `upstream-decisions.jsonl` when the request is an upstream feature
   this fork skipped

Do not create `.out-of-scope/`. A rejected enhancement stays in GitHub
plus `UPSTREAM.md` notes. List all four sources even when three are empty.

## Trace current behavior

Read the owner, the nearest tests, and the matching `README.md` page.
Cite `path:line`. Docs vs code drift is a finding for the plan (fix docs
with the change), not a license to invent a third behavior.

Write the happy path and the relevant failure path as 触发 → 函数/阶段
→ 可观察结果. Quote only the lines that carry the signal. Name the
symbol, not just the file.

For a bug: distinguish **symptom** (what fails) from **mechanism**
(how the current path produces it). Name one **red-capable** command
that would catch the reporter's exact symptom (test, curl, CLI, or a
named harness) **before** ranking hypotheses. If this session ran it,
paste the invocation and redacted output. If not, say so. Do not
proceed to a mechanism guess with no named loop.

Redact secrets in any pasted command or artifact: write `<REDACTED>`.
Never paste `api_key`, tokens, or credential contents.

## Hypotheses

When behavior or "already implemented" is ambiguous, keep 1–3 sharp
hypotheses and spend the cheapest check that **kills** one. Label each
`CONFIRMED`, `REJECTED`, or `UNRESOLVED`. Tie the verdict to files,
tests, config, or history. Leave `UNRESOLVED` rather than padding
certainty. Distinguish direct evidence from inference.

Track `claim → evidence → confidence → next check`. A snippet is a
lead until a second surface (test, caller, config, or git history)
agrees. Confidence is `high` / `medium` / `low`.

## Impact surface

Name the likely callers, configs, generated artifacts, tests, and docs
a change here would touch. That list becomes plan tasks or explicit
out-of-scope. Write paths, not layer names.

Weight **hot spots**: files that keep appearing in recent `git log`
for this owner. Deepening or refactoring cold code is speculative
unless the user picked `better` for a real invariant or duplicated
owner.

If the user or Issue uses a term that conflicts with `AGENTS.md` or
the matching `README.md`, record the conflict. Do not ask the user what
the code calls it.

## Stop conditions

Write `RESEARCH.md` and halt planning when:

- already implemented
- previously rejected, and the user did not explicitly reopen it
- security vulnerability (direct the user to `SECURITY.md`)
- owned by `sync-upstream`

## RESEARCH.md shape

Copy this skeleton. Fill every bullet. Delete a bullet only when it
cannot apply, and say why. H2 titles stay English for the validator.

```markdown
# 研究：<中文标题>

**Issue:** <url 或 local-slug>
**SHA:** <完整对象名>
**Kind:** bug | enhancement | task | mixed
**Depth:** small | medium | large | complex
**Verdict:** continue | already-implemented | rejected | route:<skill> | security

## Request

- **原话：** 标题 + 请求里真正要的那一句（引用，不改写）
- **分类：** Kind 的依据（标签、模板、还是正文推断）
- **已关闭问题：** Human note / Agent note / 评论里已经答过的点；没有则写「无」
- **不在范围内的相邻请求：** 一到两条，避免后面扩 scope

## Coverage ledger

| #   | 问题（当前状态，不要写成补丁） | 区域  | 检索     | 证据        | 状态              |
| --- | ------------------------------ | ----- | -------- | ----------- | ----------------- |
| 1   | 今天 X 在这条路径上做什么？    | owner | 查询词 A | `path:line` | answered / open:… |

每行下面跟一段中文：读了哪个符号、行为是什么、测试在哪（或「无测试」）。
`open:` 必须写缺了什么证据、为什么会影响方案。

## Search log

| 查询（含同义词） | 工具        | 命中             | 结论                    |
| ---------------- | ----------- | ---------------- | ----------------------- |
| `AvailableUIDs`  | Grep        | `path:line` 或 0 | 现有符号 / 本车道看不见 |
| `Realm`          | Grep / Glob | …                | 第二表面                |

至少一行目标词，一行同义词或第二表面。0 命中写成「本车道看不见」，不要写成「不存在」。
记录并行车道的目录和它们交回的 `path:line`。

## Current behavior

**触发：** 谁、在什么请求/任务下、带什么配置
**路径：** `path:line`（`symbol`）→ `path:line`（`symbol`）→ 出口
**结果：** 今天可观察到什么
**失败路径：** 同样粒度；没有则写「本请求不涉及」
**文档 vs 代码：** `README.md` 是否一致；漂移是发现项

引用 1–5 行关键代码或等价地描述该函数在 `path:line` 的行为。禁止只写包名。

**缺陷（仅 `bug`）：**

- **症状：** 报告者看见什么
- **机制：** 当前路径如何产生它（先有 red 命令再写）
- **Red 命令：** 一条能抓住该症状的命令
- **本会话是否已跑：** 是（贴去密后的输出）/ 否（原因）

## Redundancy

- **查询：** 列出实际用过的词
- **看过的路径：** 文件或目录，不要写「搜过代码」
- **结论：** 已实现（指出符号并 **Verdict:** already-implemented）或未实现

## Prior rejection

| 来源                        | 结果                      |
| --------------------------- | ------------------------- |
| `AGENTS.md` 不恢复 / 安全锁 | 引用或「未找到」          |
| `internal/upstream/UPSTREAM.md` | 引用或「未找到」       |
| 已关闭 wontfix / duplicate  | 编号 + 一句或「未找到」   |
| `upstream-decisions.jsonl`  | 命中或「不适用 / 未找到」 |

## Owners and tests

- **运行时所有者：** 包 + 主路径 + 主符号（不是 importer）
- **测试：** 最近的测试文件与函数；没有则写「无测试」和应放的目录
- **敏感区：** 是否碰到网关鉴权、面板鉴权、凭证读写、出站脱敏、配置写回；没有则写「无」

## Docs

- **README：** 是否有对应说明；没有则写「无对应用户文档」
- **config.example.json：** 是否涉及新配置项；不涉及则写「无」
- **漂移：** 文档与代码不一致的具体句子；没有则写「无」

## Impact surface

| 类别            | 路径 / 符号             | 计划里如何处理   |
| --------------- | ----------------------- | ---------------- |
| 调用方          | `path` (`symbol`)       | 任务 / 明确不做  |
| 配置            | 字段名                  | …                |
| 生成物          | `model.json` / 面板资源 | …                |
| 测试            | `internal/..._test.go`  | …                |
| 文档            | `README.md`             | …                |
| 热点（git log） | 近期常改的文件          | 加深或保持手术式 |

## Hypotheses

| 主张 | 结论                              | 证据                      | 置信度              | 下一步              |
| ---- | --------------------------------- | ------------------------- | ------------------- | ------------------- |
| …    | CONFIRMED / REJECTED / UNRESOLVED | `path:line` / 检索 / 测试 | high / medium / low | 已做完 / 还要查什么 |

直接证据与推断分开写。禁止用「应该是」填满置信度。

## Open questions

只留研究阶段仍缺证据的问题。需要用户决定的问题记入 `QUESTIONS.md`。
没有则写「无」。
```

After a `continue` verdict, load `references/probe.md`. In default direct
mode, write concise `BRIEF.md` and `REFLECT.md` records, `QUIZ.md` with
verdict `skipped`, and `QUESTIONS.md` with the reason and any material choices.
Then write `PLAN.md`. Use a guided interview only when requested.
