# Code review overlay

Checkout-specific facts for `code-review`. Policy lives in `SKILL.md`; the repo
rules live in `AGENTS.md` — cite both, do not restate them here.

## Identity

| Field       | Value                                                      |
| ----------- | ---------------------------------------------------------- |
| Repository  | `BegoniaHe/wokbuddy-demon`                                 |
| Base branch | `main`                                                     |
| Run dir     | `.tmp/code-review/<run-id>/` (`*.tmp` is gitignored)       |
| Gate script | `scripts/review_ready.py` (`start`, `check`)               |
| Build gates | `make fmt-check`, `make lint`, `go test ./...`, `make web-check` |
| Front end   | `web/` source; committed bundle `internal/panel/dist`      |

`<run-id>` is `pr-<n>`, `worktree`, `module-<slug>`, or `range-<base>-<head>`.
Do not relocate run files into `docs/`, `data/`, or the repo root.

Run dir contents:

```text
.tmp/code-review/<run-id>/
  CONTEXT.md          # mode, range, SHAs, spec source, checks, security lens on/off
  changed-files.txt   # inventory the ledger was built from
  COVERAGE.json       # per-file units and status
  FINDINGS.jsonl      # one record per finding
  REPORT.md           # the judgment artifact
  COMMENT.md          # PR mode only; starts with the marker
  inline/F<n>.md      # PR mode only; confirmed findings with a diff anchor
```

## Range commands

```bash
# worktree: unstaged, staged, untracked
git status --short
git diff
git diff --cached
git ls-files --others --exclude-standard

# module: inventory first, then read
git ls-files -- <path>
rg --files <path>
git ls-files -- <path> | xargs rg -n '<ChangedSymbol>'

# pr: metadata, inventory, patch
gh pr view <n> --repo BegoniaHe/wokbuddy-demon \
  --json number,url,title,body,author,isDraft,isCrossRepository,baseRefName,baseRefOid,headRefName,headRefOid,headRepository,headRepositoryOwner,maintainerCanModify,files,statusCheckRollup,closingIssuesReferences
gh pr diff <n> --repo BegoniaHe/wokbuddy-demon --name-only
gh pr diff <n> --repo BegoniaHe/wokbuddy-demon

# range
git rev-parse <base> <head>
git log --oneline <base>..<head>
git diff --stat <base>...<head>
git diff <base>...<head>
```

`git diff <base>...<head>` compares against the merge-base; prefer it over
two-dot for a branch review. An empty `git diff` output is "nothing to review",
not a pass.

## Spec sources

Look in this order, and say which one you used:

1. PR body and `closingIssuesReferences` (fetch the Issue with
   `gh issue view <n> --repo BegoniaHe/wokbuddy-demon`).
2. Issue numbers in commit messages (`#123`, `Fixes #123`).
3. `.tmp/issue-plan/<run-id>/PLAN.md` and `RESEARCH.md` when a planning
   workspace exists.
4. A spec path the user passed.

No spec: the spec pass reports `no spec available` and does not invent
acceptance criteria. A PR body that says "see issue" with no issue counts as no
spec.

## Coverage ledger

`COVERAGE.json` is the coverage claim, one unit per changed file:

```json
{
  "run_id": "pr-7",
  "mode": "pr",
  "scope": "PR #7",
  "base": "aaa",
  "head": "bbb",
  "changed_files": ["internal/pool/pool.go", "web/src/app.tsx"],
  "units": [
    {
      "path": "internal/pool/pool.go",
      "status": "reviewed",
      "axes": ["defect", "standards", "security"],
      "findings": ["F1"],
      "checks": ["go test ./internal/pool/"],
      "reason": ""
    },
    {
      "path": "web/src/app.tsx",
      "status": "out_of_scope",
      "axes": [],
      "findings": [],
      "checks": [],
      "reason": "前端改动另开 PR 审查"
    }
  ]
}
```

- `status`: `pending` → `reviewed`, `deferred`, `out_of_scope`, or `blocked`.
  Any status other than `reviewed` needs a `reason`; `pending` fails the check.
- `axes`: the passes actually applied (`defect`, `standards`, `spec`,
  `security`). A `reviewed` unit needs at least one.
- `findings`: ids from `FINDINGS.jsonl`; every id must be linked here and every
  linked id must exist.
- `checks`: commands actually run against the unit, or `[]`.
- `changed_files` must equal the unit paths. `out_of_scope` is honest about a
  file you chose not to review; it is never counted as covered.

Scaffold, then edit statuses as you work:

```bash
python3 .agents/skills/code-review/scripts/review_ready.py start \
  --run-dir .tmp/code-review/<run-id> --mode <mode> \
  --changed-file .tmp/code-review/<run-id>/changed-files.txt
python3 .agents/skills/code-review/scripts/review_ready.py check \
  --run-dir .tmp/code-review/<run-id>
# optional: recompute the inventory from git/gh and diff it against the ledger
python3 .agents/skills/code-review/scripts/review_ready.py check \
  --run-dir .tmp/code-review/<run-id> --changed-file .tmp/code-review/<run-id>/changed-files.txt
```

`check` validates coverage, the three verdict contracts, id linkage, the report
headings, and (PR mode) the comment marker. It proves structure, not truth.

## Findings schema

`FINDINGS.jsonl`, one JSON object per line. Common required fields:

| Field         | Values / meaning                                           |
| ------------- | ---------------------------------------------------------- |
| `id`          | `F1`, `F2`, … stable and unique                            |
| `fingerprint` | stable root-cause key, unique, e.g. `internal/pool/pool.go:cooldown-weights` |
| `axis`        | `defect`, `standards`, `spec`, `security`                  |
| `file`        | repo-relative path, must be in `changed_files`             |
| `line`        | 1-based integer in the reviewed revision                   |
| `title`       | one line, Chinese, names the defect                        |
| `evidence`    | symbols, guards, callers, commands actually run            |
| `trigger`     | inputs, state, or environment needed to observe it         |
| `impact`      | user-visible or invariant effect                           |
| `verdict`     | `confirmed`, `needs-validation`, `rejected`                |

Optional, allowed on any verdict: `side` (`RIGHT`, `LEFT`, `FILE`; used for the
inline anchor) and `reviewer` (the subagent labels that refuted and record-checked
it, or `self:refute` / `self:record`).

Then, by verdict:

| Field         | `confirmed`               | `needs-validation`        | `rejected`          |
| ------------- | ------------------------- | ------------------------- | ------------------- |
| `severity`    | required: `critical`/`important`/`minor` | forbidden  | forbidden           |
| `fix`         | required                  | forbidden                 | forbidden           |
| `confidence`  | required: `high`/`medium`/`low` | forbidden          | forbidden           |
| `blockers`    | forbidden                 | required, non-empty       | forbidden           |
| `validation`  | forbidden                 | required, concrete next check | forbidden       |
| `reason`      | forbidden                 | forbidden                 | required            |

Example (confirmed):

```json
{"id":"F1","fingerprint":"internal/pool/pool.go:cooldown-weights","axis":"defect","file":"internal/pool/pool.go","line":412,"title":"冷却中的账号仍在候选集","verdict":"confirmed","severity":"critical","evidence":"pickLocked() 过滤 cooldown 后未重算 weights；rg 确认唯一调用点","trigger":"池内所有账号处于 cooldown 且请求并发 > 1","impact":"请求落到冷却账号，上游 429 放大","fix":"过滤后重算 weights，空集时回落到 hard cooldown 分支","confidence":"high"}
```

Example (needs-validation):

```json
{"id":"F2","fingerprint":"internal/pool/pool.go:retry-jitter","axis":"defect","file":"internal/pool/pool.go","line":500,"title":"重试抖动可能退化为固定间隔","verdict":"needs-validation","evidence":"rand 源在测试中被固定","trigger":"并发压测下观察重试时间分布","impact":"可能造成上游同时重试","blockers":["本地无法复现并发上游行为"],"validation":"用假上游记录重试时间戳"}
```

Only `confirmed` becomes an inline comment and a severity. `needs-validation`
appears in its own report section with its blocker and next check.
`rejected` stays in the appendix with its reason; it is never deleted and never
suppresses coverage of the same file.

## REPORT.md

```markdown
# 代码审查：<scope>

## 结论
<merge-ready / 需修复；阻塞项数量；一句话技术判断>

## 覆盖范围
- 模式与范围：<mode>，<base>...<head>
- 已审查：<n>/<total> 个文件（详见 COVERAGE.json）
- 未审查：<path — reason>
- 已运行检查：<commands and results>
- 安全模块：<enabled|disabled；启用时列出的入口面>

## 阻塞问题（严重）
### F1 <title>
- 位置：`path:line`
- 触发条件：<...>
- 影响：<...>
- 依据：<...>
- 建议修复：<...>
- 核实：<refute + record verifier，或可复现检查>，置信度 <high|medium|low>

## 重要
## 次要
## 需验证的线索（无严重度）
### F2 <title>
- 位置：`path:line`
- 阻塞事实：<blockers>
- 下一步验证：<validation>
## 已排除的候选
- F9 <title> — 误判原因：<reason>
## 既有问题 / 范围外
## 行为变化
```

Use one block per finding, same fields as the JSONL. A finding with no
`path:line` is not a finding; keep it in behavior changes or out of scope. Every
finding id must appear somewhere in the report — the gate script checks it.

## COMMENT.md

Start with the marker so a re-run updates the comment instead of stacking:

```markdown
<!-- code-review:summary -->
## 代码审查：<scope>

**结论**：<merge-ready / 需修复>，严重 <n> / 重要 <n> / 次要 <n>；需验证 <n>。

### 严重
- `path:line` <title> — <impact>；建议 <fix>

### 重要
### 次要
### 需验证（无严重度）
- `path:line` <title> — 阻塞：<blocker>；下一步：<validation>
### 已排除
### 覆盖范围
审查了 <n>/<total> 个文件（<range>），已运行 <checks>。
完整报告：`.tmp/code-review/<run-id>/REPORT.md`
```

Inline body template (`inline/F<n>.md`, confirmed only):

```markdown
**[严重] <title>**

<what's wrong, with the guard/caller evidence>

触发条件：<...>
建议：<...>
<!-- code-review:inline F1 -->
```

Do not create inline files for `needs-validation` or `rejected`.

## Posting commands

```bash
# summary: update the marked comment if present, else create
id=$(gh api repos/BegoniaHe/wokbuddy-demon/issues/<n>/comments --paginate \
  --jq '.[] | select(.body | startswith("<!-- code-review:summary -->")) | .id' | tail -1)
if [ -n "$id" ]; then
  gh api --method PATCH "repos/BegoniaHe/wokbuddy-demon/issues/comments/$id" \
    -F body=@.tmp/code-review/pr-<n>/COMMENT.md
else
  gh pr comment <n> --repo BegoniaHe/wokbuddy-demon \
    --body-file .tmp/code-review/pr-<n>/COMMENT.md
fi

# inline: one call per anchored confirmed finding
head_sha=$(gh pr view <n> --repo BegoniaHe/wokbuddy-demon --json headRefOid --jq .headRefOid)
gh api --method POST repos/BegoniaHe/wokbuddy-demon/pulls/<n>/comments \
  -f commit_id="$head_sha" \
  -f path="internal/pool/pool.go" \
  -F line=412 -f side=RIGHT \
  -F body=@.tmp/code-review/pr-<n>/inline/F1.md

# already-posted inline anchors (skip duplicates on re-run)
gh api "repos/BegoniaHe/wokbuddy-demon/pulls/<n>/comments" --paginate \
  --jq '.[] | select(.user.login=="'"$(gh api user --jq .login)"'") | "\(.path):\(.line)"'
```

Multi-line anchors need `start_line` plus `start_side` and the same `side`.
`422` on an anchor: the line is not part of the diff; move the finding to the
summary and note it. Never retry blindly with another line number.

Verify the posted URL with `gh pr view <n> --repo BegoniaHe/wokbuddy-demon --json url --jq .url`.

## PR edge cases

| Case                                       | Handling                                                        |
| ------------------------------------------ | --------------------------------------------------------------- |
| Draft PR                                   | Review normally; say it is a draft in the conclusion.           |
| Cross-repository (`isCrossRepository`)     | Comment, never push; post patches instead of commits.           |
| `headRepositoryOwner.login != BegoniaHe`   | Same as cross-repository.                                       |
| Head is `main`                             | Comment only; never push.                                       |
| PR closed or merged                        | Report in chat; do not comment on a dead thread.                |
| `maintainerCanModify == false`             | Comment only; the branch is not writable.                       |
| CI red                                     | Review the code; fold failing checks into coverage, not findings. |
| Diff too large for one pass                | Split by file, merge findings, state the split in coverage.     |

## Redaction

Before quoting anything into the report, a comment, or an inline snippet:

- drop `Authorization`, `Cookie`, `api_key`, `token`, `secret`, session ids;
- replace provider hosts and error bodies with `<upstream-url>` / `<provider-error>`;
- never quote `auths/*.json`, `config.json`, `data/` payloads, or raw SSE bodies;
- describe the shape (`112 字符的 token`) instead of the value.

## Fix loop

```bash
git switch <head-branch>          # only when it is a repo branch, not a fork
go test ./internal/<pkg>/ -run TestName
make fmt-check
make lint
git add -p
git commit -m 'fix(<scope>): <中文描述>' -m 'Refs #<pr>' \
  -m 'AI-Generated: true' -m "Generated-At: $(date -u '+%Y-%m-%dT%H:%M:%SZ')"
git push origin <head-branch>
```

Never `--force`, `--force-with-lease`, or amend someone else's commit. After the
push: re-verify each fixed finding with the same evidence path, update its
record and the unit's `checks`, patch the summary comment with the new SHA and
the fixed ids, re-run `review_ready.py check`, and confirm the new commit
appears in `gh pr view <n> --json commits`.
