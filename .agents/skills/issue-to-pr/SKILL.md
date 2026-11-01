---
name: issue-to-pr
description: Implement a planned or small, obvious wokbuddy-demon Issue and prepare its pull request: frontier-ordered vertical slices, tests at existing seams, real checks, a Conventional Commit, and a template-matched PR body with evidence and an Agent note. Use when an Issue or accepted plan is ready to build and ship; not for planning-only requests, reference-fork audits, or merging.
license: MIT
compatibility: >
  Codex CLI or OpenCode in the BegoniaHe/wokbuddy-demon checkout. Requires
  Python 3.x and git; gh is needed only to push and open the PR.
metadata:
  author: BegoniaHe
  checkout: BegoniaHe/wokbuddy-demon
---

# Ship an issue as a PR

The loop is **Issue → plan → slices → review → PR → human merge**. This skill
owns the part after the contract is agreed and stops before merge: change code
on a feature branch, prove the change with the smallest real checks, and prepare
a PR a maintainer can review.

Follow `AGENTS.md`, `CONTRIBUTING.md`, and `AI_POLICY.md`; cite them instead of
copying them. Keep working state under `.tmp/issue-pr/<run-id>/`.

## Locked

- One PR serves one Issue. A large feature needs a development Issue before its
  PR; a small, obvious change may proceed without one.
- Agents may edit, commit, push a feature branch, and open or update a PR. Never
  merge, push `main`, force-push a protected branch, tag, or release.
- Run `gh` with `--repo BegoniaHe/wokbuddy-demon` and confirm the created URL.
  Never open an Issue or PR on an upstream, previous, or reference fork.
- Use the template for the commit type: `feat.md`, `fix.md`, `docs.md`;
  `refactor`/`perf`/`style` use `refactor.md`; `build`/`ops`/`test` use
  `chore.md`.
- End the PR body with exactly one `## Agent note`. Never add or fabricate a
  `## Human note`.
- Commit headers follow `.agents/shared/conventional-commit/REFERENCE.md`.
  Agent-finalized commits carry `Fixes #<n>`, `AI-Generated: true`, and a UTC
  `Generated-At:` from `date -u '+%Y-%m-%dT%H:%M:%SZ'`.
- Claim a check only when this session ran it or the linked CI run did.
- Credentials (`auths/`), state (`data/`), and `config.json` never enter the
  repo, logs, PR body, or panel output.
- Do not restore a removed API, config shape, or auth layout to make a task
  easier.

## Open

- Branch name (`fix/`, `feat/`, `docs/`, `refactor/`), commit split, and draft
  versus ready-for-review.
- Whether to open the PR now, or stop at a pushed branch. When the request
  covered implementation only, stop after the validated commit and ask once.
- Parallel work in subagents or worktrees, only for disjoint AFK tasks.

## Start

Create the feature branch first, so its prefix names the commit type
(`fix/<slug>`, `feat/<slug>`), then initialize the run:

```bash
git switch -c fix/<slug>
python3 .agents/skills/issue-to-pr/scripts/pr_ready.py start --issue 123
git status --short --branch
python3 .agents/skills/issue-to-pr/scripts/pr_ready.py status
```

`<run-id>` is `issue-<number>` or `local-<slug>`; `start` records SHA, branch,
base, issue, type, and a plan pointer, then scaffolds `PR.md` from the matching
template. Pass `--type` when the branch prefix does not name it, and `--slug`
for a pasted request.

Prefer an existing plan: read `.tmp/issue-plan/<run-id>/PLAN.md` and the cited
parts of `RESEARCH.md`. If the Issue has no plan and the change is not small and
obvious (one behavior, at most a few files, no new config, API, or security
surface), stop and use `plan-issue`. Do not invent scope here. A large request
without an Issue can still be planned locally, but its development Issue must
exist before the PR; do not post it to GitHub automatically.

With no plan, write `CONTRACT.md` in the run: Issue URL, current behavior,
desired behavior, out of scope, acceptance, and the exact verify commands.

## Re-check the ground

Compare the plan's SHA with `git rev-parse HEAD`. A plan researched several
commits ago is not automatically stale: re-read each path and symbol a task
names. If a cited symbol vanished or the behavior changed, refresh the plan with
`plan-issue` instead of guessing a replacement.

Read `references/implementation.md` before the first edit.

## Work the frontier

Task dependencies form a graph. The frontier is every task whose blockers are
done. Work the frontier in order, one vertical slice at a time:

- Run a planned prefactor task first, alone; keep behavior-preserving refactors
  out of the feature commit.
- For each task, write the failing test at the task's seam, watch it fail, make
  it pass, then move on. For a bug, reproduce the reported symptom with the
  plan's red-capable command before changing code.
- For a `HITL` task, get the named decision, credential, or live observation
  from the human, record the answer, and continue with independent AFK work.
- Fold config, panel, and doc updates into the task that needs them; do not
  leave a dangling `README.md` or `config.example.json` change for later.
- Log each task, the commands run, and redacted results in `PROGRESS.md`.

A slice that grows past its planned files or spills into a second subsystem is
a signal to stop and re-plan, not to continue editing.

## Prove the slice

Run the task's focused command. At the end, run the plan's verification block
and the gates the change actually touches, focused first:

```bash
go test ./internal/<pkg>/ -run TestName
make fmt-check
make lint
make test
```

Front-end changes also need `make web-check`, and a rebuilt
`internal/panel/dist` from `make web`. Fix a failure; never weaken the check to
get green. If the check itself is wrong, say so and route back to the plan.

## Review before the PR

Read the diff cold against two separate axes, and keep them separate:

- **Standards**: this repo's documented rules (`AGENTS.md`,
  `CONTRIBUTING.md`, `.golangci.yml`) plus stale-path, dead-code, and
  mixed-concern checks.
- **Spec**: each acceptance criterion and `Covers:` ID is met, and the diff
  adds nothing the Issue did not ask for.

Fix blocking findings. Note accepted residual risk for the PR body. Record the
review result in `PROGRESS.md`.

## Commit

Stage only this change. Split by concern; one commit per logical step is
usually enough. Use a Conventional Commit header with an English type/scope and
a Chinese description, then the Issue and AI footers:

```text
fix(pool): 避免冷却期账号被重复选中

Fixes #123
AI-Generated: true
Generated-At: <date -u '+%Y-%m-%dT%H:%M:%SZ'>
```

## Write the PR body

Fill `PR.md` from the template `start` scaffolded; keep its headings. Replace
the `Validation` block with the commands actually run and a before/after
evidence line. In `Compatibility and risk`, state `Door: two-way` or
`one-way` and the blast radius. Finish with the `Agent note`: goal, paths
touched, checks run, residual risk, tools used. Read
`references/pull-request.md` for the section rules.

```bash
python3 .agents/skills/issue-to-pr/scripts/pr_ready.py check
```

Blocking findings must be fixed. Warnings need a reason in `PROGRESS.md`.

## Open and hand off

Push the feature branch, then open the PR against `main`:

```bash
git push -u origin <branch>
gh pr create --repo BegoniaHe/wokbuddy-demon \
  --template .github/PULL_REQUEST_TEMPLATE/<type>.md \
  --body-file .tmp/issue-pr/<run-id>/PR.md --draft
```

Confirm the URL is under `github.com/BegoniaHe/wokbuddy-demon`. Mark the PR
ready only when the review pass and `check` are clean. Report the PR URL, the
checks actually run, and residual risk. State that merge needs a human
maintainer review plus one independent AI-assisted review; do not merge.

## Handoff

- Checkout facts, templates, and closeout commands: `REFERENCE.md`.
- Slice loop, seams, and stop conditions: `references/implementation.md`.
- PR body, evidence, and review axes: `references/pull-request.md`.
- Methodology provenance: `references/sources.md`.
- Planning: `plan-issue`. Smallest-change check: `simplifier`.
- Contribution policy: `.agents/shared/ai-contribution/REFERENCE.md`.

## Verify

`pr_ready.py check` is the mechanical bar: branch guard, commit shape, secret
paths, template headings, evidence block, Agent note, and plan validation. The
judgment bar is the cold review: spec coverage first, then standards. A green
script with an unmet acceptance criterion is not ready.
