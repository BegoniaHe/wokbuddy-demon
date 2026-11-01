# Issue-to-PR overlay

Checkout-specific facts for `issue-to-pr`. Policy lives in `SKILL.md`; the
verify command map and package owner map live in
`../plan-issue/REFERENCE.md`. Do not restate either here.

## Identity

| Field        | Value                                              |
| ------------ | -------------------------------------------------- |
| Repository   | `BegoniaHe/wokbuddy-demon`                         |
| Base branch  | `main`                                             |
| PR templates | `.github/PULL_REQUEST_TEMPLATE/<type>.md`          |
| Issue labels | `bug`, `enhancement`, `task`                       |
| Go floor     | `1.27`                                             |
| Artifacts    | nightly binaries + GHCR image; no versioned release |

`gh` must pass `--repo BegoniaHe/wokbuddy-demon`, and every created URL must
start with `https://github.com/BegoniaHe/wokbuddy-demon`.

## Workspace

Created by `scripts/pr_ready.py start` at `.tmp/issue-pr/<run-id>/`:

```text
.tmp/issue-pr/<run-id>/
  manifest.json   # SHA, branch, base, issue, type, plan pointer, timestamps
  PR.md           # template scaffold, then the reviewed body
  PROGRESS.md     # written by the agent when the first slice starts
  CONTRACT.md     # only when no plan-issue workspace exists
```

`<run-id>` is `issue-<number>` or `local-<slug>`. `.tmp/` is gitignored. Do not
relocate run files into `docs/`, `data/`, or the repository root.

## Branch and base

- Branch names use the commit type as prefix: `fix/<slug>`, `feat/<slug>`,
  `docs/<slug>`, `refactor/<slug>`.
- Base is `origin/main` when it exists, else `main`. A PR must be ahead of its
  base and contain at least one commit.
- Rebase or merge `main` into the feature branch when it falls behind; never
  force-push `main`.

## Template map

| Commit type                     | Template      | Sections that carry the proof                       |
| ------------------------------- | ------------- | --------------------------------------------------- |
| `feat`                          | `feat.md`     | Behavior, Non-goals, config hot-apply vs restart    |
| `fix`                           | `fix.md`      | Root cause, Reproduction, regression test           |
| `docs`                          | `docs.md`     | Files, why the wording changes                      |
| `refactor`, `perf`, `style`     | `refactor.md` | Behavior guarantee; Performance required for `perf` |
| `chore`, `build`, `ops`, `test` | `chore.md`    | Surfaces that must move together                    |

Keep the template's headings. Fill `Related issue` with `Fixes #<n>` or
`Related: #<n>`. `Validation` holds the commands actually run. `Compatibility
and risk` holds the door call and blast radius. Delete `Human note`; finish with
`Agent note`.

## Commands

```bash
python3 .agents/skills/issue-to-pr/scripts/pr_ready.py start --issue 123
python3 .agents/skills/issue-to-pr/scripts/pr_ready.py status
python3 .agents/skills/issue-to-pr/scripts/pr_ready.py check
python3 .agents/skills/issue-to-pr/scripts/pr_ready.py check --require-plan
python3 .agents/skills/issue-to-pr/scripts/pr_ready.py check --allow-missing-ai-trailer
```

`--allow-missing-ai-trailer` is for a branch carrying a human-authored commit
with its own `Human note`; an agent-authored commit never needs it.
`--allow-dirty` exists for inspection only and is never part of a ready PR.

```bash
gh issue view <n> --repo BegoniaHe/wokbuddy-demon \
  --json number,title,body,labels,state,url
gh pr create --repo BegoniaHe/wokbuddy-demon \
  --template .github/PULL_REQUEST_TEMPLATE/fix.md \
  --body-file .tmp/issue-pr/issue-123/PR.md --draft
gh pr view <n> --repo BegoniaHe/wokbuddy-demon --json url,state,isDraft
gh pr edit <n> --repo BegoniaHe/wokbuddy-demon --body-file <file>
gh pr ready <n> --repo BegoniaHe/wokbuddy-demon
```

Do not post review comments or request reviewers unless the user asks.

## Evidence rules

`Validation` is a claim the reviewer can check. Give it three things:

1. The focused command for the changed behavior.
2. The before/after signal: the red test or failing invocation, then the same
   command passing. Redact secrets as `<REDACTED>`.
3. The final gate actually run, e.g. `make fmt-check`, `make lint`,
   `make test`, `make build`; front-end changes add `make web-check`.

"If tests pass" alone is not evidence. A command not run this session does not
belong in the block.

## Review axes

Keep the two passes separate; a change can pass one and fail the other.

**Spec**: map each Issue requirement and plan `Covers:` ID to a diff hunk or to
an explicit out-of-scope line. Missing, partial, and unrequested behavior are
all findings.

**Standards**: `AGENTS.md`, `CONTRIBUTING.md`, `.golangci.yml`, and the
Conventional Commit rules; stale paths in comments or docs; dead code; mixed
concerns; a new dependency or abstraction without a current consumer;
credentials or state written into tracked files.

Fix blocking findings before the PR. Report the rest as residual risk.

## Sensitive closeout

A task touching gateway `api_key` / `internal/httpauth`, panel auth, credential
read and atomic write-back, outbound sanitize, or config write-back needs its
failure path tested, not just the happy path. A vulnerability report follows
`SECURITY.md`: no public Issue, no PR description carrying exploit detail.

## Routing

| Request                                   | Skill           |
| ----------------------------------------- | --------------- |
| Turn an Issue or request into a plan      | `plan-issue`    |
| Cut the current change down before coding | `simplifier`    |
| Audit the reference fork                  | `sync-upstream` |
| Implement and prepare the PR              | `issue-to-pr` (this skill) |
