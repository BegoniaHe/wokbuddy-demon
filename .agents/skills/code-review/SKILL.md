---
name: code-review
description: >-
  Review wokbuddy-demon changes for real defects: a module or path, an open pull
  request, a branch range, or uncommitted work. Keeps a per-file coverage
  ledger, classifies every finding as confirmed, needs-validation, or rejected,
  and refutes candidates with read-only subagents before publishing. Reports in
  chat or posts a PR summary plus inline comments; the optional security module
  adds a trust-boundary pass. Fixes and pushes only when asked. Not for
  implementation, planning, or reference-fork audits.
license: MIT
compatibility: >
  Codex CLI or OpenCode in the BegoniaHe/wokbuddy-demon checkout. Requires git
  and Python 3.x (ledger check); gh 2.x for PR modes; Go 1.27 and make only when
  verifying fixes.
metadata:
  author: BegoniaHe <163421589+BegoniaHe@users.noreply.github.com>
  checkout: BegoniaHe/wokbuddy-demon
  based_on: >-
    cloudflare/security-audit-skill, obra/superpowers, mattpocock/skills,
    trailofbits/skills, addyosmani/agent-skills, allemaar/open-skills (MIT)
---

# Review a wokbuddy-demon change

Review is a read-only pass that ends in evidence, not a rewrite. Default:
inspect, verify, report. Fix and push only when the request says so
(`修复`, `直接推送修复`, `fix it`). Follow `AGENTS.md` and
`.agents/shared/ai-contribution/REFERENCE.md`; cite them, do not paste them.

Default depth is deep. Read every changed file, trace the path, and report
**all** findings. Do not cap findings, skip files, truncate a report, or stop
early to save tokens. If the range is too large for one pass, split it by file
into passes and merge the results.

A review is complete only when the coverage ledger accounts for every changed
file and the mechanical check passes. "All files reviewed" is a claim the
ledger must support, not a sentence in the summary.

## Locked

- Review only this change. Read the whole touched file, its callers, and its
  tests before flagging; pre-existing defects outside the change are reported
  separately and never as blocking findings.
- PR feedback is comments only: a summary comment plus inline comments. Never
  `gh pr review --approve` or `--request-changes`, never merge, never push
  `main`, never force-push, never tag.
- Push a fix only to a feature branch that lives in `BegoniaHe/wokbuddy-demon`
  and is not `main`. A fork head or an unreadable branch means: post the patch
  in the comment and stop. No fix in the request means: do not edit anything.
- Every finding names `path:line`, its trigger, and its impact, and carries one
  verdict: `confirmed`, `needs-validation`, or `rejected`. Only `confirmed` adds
  a severity, a fix, and a confidence; `needs-validation` names the exact
  blocker and the next check; `rejected` keeps its reason in the report
  appendix and is never deleted.
- Verification is tiered: a Minor candidate gets one refutation pass. An
  Important or Critical candidate gets that refutation and, if it survives, an
  independent record check by a verifier that did not find or refute it. A
  Critical stays `confirmed` without a second agent only when a reproducible
  test or command demonstrates it.
- Coverage is ledger-backed: a changed file with no unit, or a unit still
  `pending`, means the review is incomplete and must not be reported as done.
- Reports, PR comments, and commit descriptions use Simplified Chinese prose;
  identifiers, paths, and commands stay literal. Redact `api_key`, tokens,
  provider URLs, and `auths/` contents from every artifact and shell output.
- Claim a check only when this session ran it or the linked CI run did.
- Run `gh` with `--repo BegoniaHe/wokbuddy-demon`; confirm every posted URL is
  under `github.com/BegoniaHe/wokbuddy-demon`.

## Open

- Pass split, subagent count, inline-comment grouping, and whether to run the
  role sweep (defect, security, test/behavior) for a large or high-risk diff.
- Whether to enable the security module when the request does not name it and
  the diff does not touch one of the surfaces listed below.
- Whether to fix findings beyond confirmed Critical/Important when the request
  only says "修复"; default is Critical then Important, one commit each.
- Whether to file an Issue for a pre-existing defect found on the way. Ask
  once; default is to report it in chat only.
- A quick pass when the request explicitly asks for one. State the reduced
  coverage; never call it deep.

## Pick the review range

| Input                                     | Mode      | Range                                        |
| ----------------------------------------- | --------- | -------------------------------------------- |
| nothing, "未提交的改动", "review my diff" | worktree  | unstaged + staged + untracked                |
| a path, package, or subsystem             | module    | HEAD content of the scope and its callers    |
| PR number, URL, "开着的 PR"               | pr        | PR head diff, spec from the PR body          |
| commit SHA, branch, `HEAD~3`              | range     | `<base>...<head>` (three-dot, merge-base)    |

No arguments: review the working tree. A clean tree falls back to the open PR
for the current branch, else `HEAD~1..HEAD`; say which range you used.

```bash
# worktree
git status --short
git diff
git diff --cached

# module
git ls-files -- <path>
rg -n '<ChangedSymbol>'

# pr
gh pr view <n> --repo BegoniaHe/wokbuddy-demon \
  --json number,url,title,body,author,isDraft,baseRefName,headRefName,headRefOid,headRepositoryOwner,files,statusCheckRollup
gh pr diff <n> --repo BegoniaHe/wokbuddy-demon

# range
git log --oneline <base>..<head>
git diff <base>...<head>
```

Confirm refs resolve and the diff is non-empty; an empty range is "nothing to
review", not a clean review. For several open PRs, use
`gh pr list --repo BegoniaHe/wokbuddy-demon --state open --json number,url,title,headRefName`
and review each in its own run. No open PR at all: say so and stop.

### Security module

The security module is a lens on the chosen range, not another range. Enable it
when the request names security (`安全`, `security`, `审计`, `auth`), or when the
diff touches `internal/httpauth`, `internal/auth`, `internal/server`,
`internal/panel`, `internal/clientprofile`, outbound request construction in
`internal/upstream`, credential handling, or filesystem paths. Read
`references/security.md`, add `security` to each in-scope unit's axes, and run
its trust-boundary pass. Without that trigger the defect axis still covers
security bugs; the module adds boundary enumeration and severity anchors.

A security candidate needs a named lower-trust principal, the input or action it
controls, the intended control, and a concrete result. Missing deployment facts
(proxy, TLS terminator, network policy, identity provider) are
`needs-validation`, never `confirmed`, never severity-rated.

## Set up the run

Create `.tmp/code-review/<run-id>/` (`pr-<n>`, `worktree`, `module-<slug>`,
`range-<base>-<head>`) and record mode, base/head SHAs, spec links, and the
checks you will run in `CONTEXT.md`. Capture the changed-file inventory, then
scaffold the coverage ledger:

```bash
mkdir -p .tmp/code-review/<run-id>

# worktree: unstaged + staged + untracked
{ git diff --name-only; git diff --cached --name-only; \
  git ls-files --others --exclude-standard; } | sort -u \
  > .tmp/code-review/<run-id>/changed-files.txt

# pr
gh pr diff <n> --repo BegoniaHe/wokbuddy-demon --name-only \
  > .tmp/code-review/<run-id>/changed-files.txt

# range
git diff --name-only <base>...<head> > .tmp/code-review/<run-id>/changed-files.txt

# module
git ls-files -- <path> > .tmp/code-review/<run-id>/changed-files.txt

python3 .agents/skills/code-review/scripts/review_ready.py start \
  --run-dir .tmp/code-review/<run-id> --mode <mode> \
  --changed-file .tmp/code-review/<run-id>/changed-files.txt
```

`COVERAGE.json` is the coverage claim. Each file starts `pending`; after reading
it, set `status` to `reviewed` with the axes and finding ids you applied, or to
`deferred`, `out_of_scope`, or `blocked` with a `reason`. A file seen only as a
diff hunk is not `reviewed`.

An existing run dir is resumed, not re-scaffolded: update `head` and the unit
list from the new inventory, set changed units back to `pending`, and let
`check --changed-file` report any drift.

Run artifacts: `CONTEXT.md`, `COVERAGE.json`, `FINDINGS.jsonl`, `REPORT.md`, and
`COMMENT.md` for PR mode. `.tmp/` is gitignored; schemas and templates are in
`REFERENCE.md`.

## Read before judging

The diff alone is not enough. For every changed file read:

- the full file, so surrounding guards, locking, and error handling are known;
- the callers of each changed symbol, and the tests that already cover the
  seam;
- the contracts the change must sync: `AGENTS.md`, `config.example.json`,
  `README.md`, `.golangci.yml`, PR template, and `internal/upstream/UPSTREAM.md`
  when provenance moved;
- git history when a removed guard or a `fix`/`CVE` line is in the diff
  (`git log -S '<symbol>' -- <path>`).

Keep the review pass read-only: no `HEAD` move, stash, or checkout. Read other
revisions with `git show <sha>:<path>`. In PR mode, confirm the local checkout
is at `headRefOid`; if it is not, read every changed file from
`git show <headRefOid>:<path>` rather than from the working tree.

## Run the review passes

Run the passes in order, keep the results separate, and label each finding with
its axis. With the security module, its trust-boundary pass is the fourth axis.

1. **Defects** — the primary axis. Logic and off-by-one errors, missing or
   inverted guards, unreachable branches, nil/empty inputs, error paths that
   swallow or misclassify, concurrency and lifecycle (locks, goroutines,
   `defer` in loops, unclosed bodies), resource limits, and panics reachable
   from user input.
2. **Standards** — this repo's documented rules, not general taste:
   `references/standards.md` maps `AGENTS.md` invariants to concrete checks.
   Skip what `make fmt-check` and `golangci-lint` already enforce, unless the
   diff dodges the tooling.
3. **Spec and behavior** — does the change do exactly what its Issue or PR body
   asked, and nothing else? Missing acceptance, scope creep, stale docs/config,
   tests at the wrong seam, and user-visible behavior changes (defaults, env
   vars, listen address, retry/backoff, panel copy) called out explicitly.
4. **Trust boundaries** (security module only) — read `references/security.md`,
   enumerate the entry surfaces the change touches, and test each claimed
   boundary crossing against the control that should stop it.

Append each candidate to `FINDINGS.jsonl` as you go, before deciding whether it
is real. A candidate left only in memory is a candidate lost.

## Verify before publishing

For each candidate:

1. Restate it in one sentence: exact `path:line`, trigger, and impact.
2. Trace from the changed line through callers and guards. An upstream guard
   that makes the trigger unreachable refutes the finding.
3. Check real behavior, not memory: `rg` for the function that is actually
   called, `go doc`, `go test ./internal/<pkg>/ -run TestName`, or the
   documented contract.
4. Refute it with a read-only subagent from `references/reviewer-prompt.md`, one
   per independent finding or tight cluster, instructed to break the claim.
   Batch them; do not serialize what can run together.

Every candidate ends in exactly one verdict:

- **`confirmed`** — the path and trigger are evidenced and the impact is
  demonstrated. Severity reflects only that demonstrated impact. An Important or
  Critical finding also needs the independent record check below; a Critical
  without that second agent survives only when a reproducible test or command
  demonstrates it, otherwise it is `needs-validation`.
- **`needs-validation`** — a source-grounded claim blocked by one exact missing
  fact: runtime, credential, deployment, or upstream account. Record `blockers`
  and the concrete `validation` step. No severity, no inline comment.
- **`rejected`** — refuted by source, a guard, a test, or an impossible
  prerequisite. Keep the `reason`; it stays in the report appendix.

Tiered verification: Minor gets the refutation above. Important and Critical
also get a second, independent pass — a record verifier that did not find or
refute the candidate re-reads the final `path:line`, trigger, impact, severity,
and fix. A material disagreement is resolved by evidence, never by seniority;
if it stays split, publish `needs-validation` with both readings. No subagent
tool: run the two passes yourself in separate sweeps and record `self:refute` /
`self:record`.

Calibrate severity by effect, not by enthusiasm, and never above the effect you
demonstrated:

- **Critical**: wrong behavior on a reachable path, security-invariant
  violation, credential/state leak, data loss or corruption, panic, or a gate
  that will fail CI (unbuilt `internal/panel/dist`, fmt/lint break).
- **Important**: missing guard on a realistic input, broken error semantics,
  behavior change without docs, test gap at the behavior seam, contract
  (`config.example.json`, `README.md`, restart classification) left unsynced.
- **Minor**: naming, comments, micro-optimization, style the tooling misses.

## Write the report

Fill `REPORT.md` from `REFERENCE.md`: verdict first, coverage second, then
confirmed findings grouped by severity, then a `needs-validation` section with
no severity, then rejected candidates, then pre-existing/out-of-scope notes,
then behavior changes. Every finding carries its axis, `path:line`, trigger,
impact, fix, and verdict; only confirmed findings also carry severity and
confidence.

Draft `COMMENT.md` for PR mode, then run the mechanical check before publishing:

```bash
python3 .agents/skills/code-review/scripts/review_ready.py check \
  --run-dir .tmp/code-review/<run-id>
```

It fails on a `pending` unit, an unaccounted changed file, a verdict that
carries a forbidden field, a duplicate fingerprint, an unlinked finding, or a
finding missing from `REPORT.md`. Pass means structure and coverage only; the
truth claim still rests on the verification passes.

## Publish

In PR mode, that PR is the target. Otherwise look for one on the current branch:

```bash
gh pr list --repo BegoniaHe/wokbuddy-demon --head "$(git branch --show-current)" \
  --state open --json number,url,title
```

Only comment when the PR covers what you reviewed. A PR on the current branch is
not evidence: worktree findings concern uncommitted changes, and a module review
may not be touched by the PR at all. State the reviewed range in the summary
(`本地未提交改动`, `<module> @ <sha>`, `<base>...<head>`), skip inline anchors for
anything absent from the PR diff, and fall back to the chat report when the PR
is unrelated.

**PR exists** — write `COMMENT.md` starting with the marker
`<!-- code-review:summary -->`, then post the summary and the inline findings.
On a re-run, patch the marked comment by id instead of creating a second one; the
exact commands are in `REFERENCE.md`.

```bash
gh pr comment <n> --repo BegoniaHe/wokbuddy-demon --body-file .tmp/code-review/pr-<n>/COMMENT.md
gh api --method POST repos/BegoniaHe/wokbuddy-demon/pulls/<n>/comments \
  -f commit_id="$(gh pr view <n> --repo BegoniaHe/wokbuddy-demon --json headRefOid --jq .headRefOid)" \
  -f path="internal/pool/pool.go" -F line=412 -f side=RIGHT \
  -f body="$(cat .tmp/code-review/pr-<n>/inline/F1.md)"
```

Inline only for findings anchored to a line that exists in the PR diff
(`side` is `RIGHT` for the head, `LEFT` for removed lines). A `422` on the
anchor means the line is not commentable: fold that finding into the summary
and say so. Re-running a review updates the marked summary comment instead of
adding a second one, and skips inline comments already present for the same
`path:line`.

Only confirmed findings get inline comments. A `needs-validation` lead appears
in the summary table with its blocker and next check; a rejected candidate
appears only in the appendix line. Neither carries a severity or an inline
defect comment.

**No PR** — report in chat with the same severity grouping and the report path.
Lead with the verdict and the blockers; do not paste the whole report.

## If the request asks for fixes

- Fix confirmed findings only, Critical then Important (Minor only when the
  request asks for all), one commit per concern; unrelated cleanup stays out.
- Only when `headRepositoryOwner.login` is `BegoniaHe` and the head branch is
  not `main`. Otherwise post the suggested patch in the comment and stop.
- Switch to the PR head branch, keep their commits, never rewrite history,
  never force-push. Run the focused checks before committing:
  `go test ./internal/<pkg>/ -run TestName`, `make fmt-check`, `make lint`,
  plus `make web-check` when `web/` moved.
- Commit with a Conventional Commit header (English type/scope, Chinese
  description) and the AI footers; use `Refs #<pr>` when no Issue exists.
- After the push, re-verify each fixed finding with the same evidence path,
  update its record and the unit's `checks`, refresh the summary comment with
  the new SHA and the fixed ids, and re-run `review_ready.py check`. Do not
  re-review unless asked.

## Do not

- Flag untouched pre-existing code as a blocker, or a refactor as a bug because
  it looks unfamiliar.
- Report style that `gofumpt`/`gci`/`golangci-lint` already decides, or demand
  a helper/abstraction the repo's inline-first rule rejects.
- Post "looks good" or praise-only comments, invent findings to look thorough,
  or mark nitpicks Critical.
- Publish a `needs-validation` lead as a confirmed defect, attach a severity to
  it, or hide a rejected candidate.
- Mark a file `reviewed` after reading only its diff hunk, or count a
  `deferred` / `out_of_scope` file as covered.
- Confirm a Critical from one pass when no reproducible test or command
  demonstrates it.
- Treat `review_ready.py check` passing as a judgment about the findings; it
  proves structure and coverage only.
- Paste credentials, request bodies, or provider errors into the report,
  comment, or inline snippet; redact before quoting.
- Push to a fork, a protected branch, or open a second PR for review fixes.

## Handoff

- Checkout facts, ledger/findings schema, report and comment templates:
  `REFERENCE.md`.
- Repo review checklist: `references/standards.md`.
- Optional trust-boundary pass: `references/security.md`.
- Refutation, record-check, and role-sweep prompts:
  `references/reviewer-prompt.md`.
- Mechanical gate: `scripts/review_ready.py` (`start`, `check`) and its tests.
- Repo policy and AI limits: `AGENTS.md`,
  `.agents/shared/ai-contribution/REFERENCE.md`.
- Fix delivery beyond review: `issue-to-pr`. Planning: `plan-issue`.
  Smallest-change judgement: `simplifier`. Upstream audits: `sync-upstream`.
- Methodology provenance: `references/sources.md`.

## Verify

The review is done when all of these hold:

- `review_ready.py check` passes: every changed file has a unit, no unit is
  `pending`, every finding is linked, and every finding id appears in
  `REPORT.md`.
- Every `confirmed` finding has an evidenced path and trigger; every Important
  or Critical passed the record check, or (Critical only) a reproducible check.
- Every `needs-validation` record has `blockers` and a `validation` step and no
  severity; every rejected record has a `reason`.
- The PR comment URL (or the chat report path) resolves. If fixes were pushed,
  the focused checks ran and the summary comment names the new SHA.

A comment that says "no issues" without the ledger and the files read is not a
review.
