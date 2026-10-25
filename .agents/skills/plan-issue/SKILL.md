---
name: plan-issue
description: Turn a wokbuddy-demon GitHub Issue or change request into a code-backed implementation plan with file paths, acceptance criteria, and verification commands. Use when planning, triage, or a specification is requested; not for direct implementation, reference-fork audits, or product audits.
license: MIT
compatibility: >
  Codex CLI or OpenCode in the BegoniaHe/wokbuddy-demon checkout. Requires
  Python 3.x, git, and gh when fetching a GitHub Issue.
metadata:
  author: BegoniaHe
  checkout: BegoniaHe/wokbuddy-demon
---

# Plan a wokbuddy-demon issue

Deliver a plan an executor can follow without the planning conversation. Ground
it in this worktree, name the relevant files and symbols, and explain how each
task will be verified. A planning request ends with the validated plan. If the
user already authorized implementation after planning in this conversation,
continue with that work once the plan is concrete and no blocking choice remains.

Follow `AGENTS.md`, `CONTRIBUTING.md`, and `AI_POLICY.md`. Cite those policies
instead of copying them into the plan. Keep working state under
`.tmp/issue-plan/<run-id>/`; do not create planning trees under `docs/`,
`tasks/`, or `.specify/`.

## Choose the workflow

Default to direct planning: inspect, resolve material ambiguities, compare
approaches where useful, and write the plan. Do not quiz the user about code
facts or require approval of each intermediate document. Use the existing
`--skip-probe` workspace mode to record that a guided interview is unnecessary.
This skips the interview, not research or verification.

Use guided probe only when the user asks for an interview, quiz, or facilitated
design discussion. Read `references/probe.md` for that mode. A user can leave
it at any time; keep their answers and continue without repeating questions.

Ask only about unresolved decisions that change scope, architecture, behavior,
or acceptance. Use a question tool when the active client provides one, or a
concise chat question otherwise. Continue independent research while waiting.
Record reasonable defaults for non-blocking unknowns. Never infer approval from
silence or elapsed time.

Route reference-fork audits to `sync-upstream`; it only files Issues and does
not absorb code. Do not start this planning workflow merely because an
implementation task has several steps.

## Read as needed

- `REFERENCE.md`: checkout facts, workspace layout, and GitHub commands.
- `references/research.md`: evidence collection and research artifact fields.
- `references/probe.md`: direct planning records and optional guided interview.
- `references/plan-template.md`: plan fields and task format.
- `references/verification.md`: judgment checks after the plan exists.
- `references/sources.md`: methodology provenance; cite rather than vendor it.
- `.agents/shared/ai-contribution/REFERENCE.md` and
  `.agents/shared/conventional-commit/REFERENCE.md`: only for requested
  contribution actions.

## Inspect and initialize

Read `REFERENCE.md` and `references/research.md`. Inspect
`git status --short --branch` and `git rev-parse HEAD`; preserve unrelated work.
Resume a workspace only when its request and SHA still match.

For a GitHub Issue, use:

```bash
python3 .agents/skills/plan-issue/scripts/issue_plan.py init --issue 123 --skip-probe
python3 .agents/skills/plan-issue/scripts/issue_plan.py fetch --issue 123
python3 .agents/skills/plan-issue/scripts/issue_plan.py status
```

For a pasted request, use `--slug <short-name>` instead of `--issue` and write
the request to `ISSUE.md`. Omit `--skip-probe` only for a requested guided
interview. For an existing workspace, `skip-probe` selects direct planning.
Record the reason in `QUESTIONS.md`; do not fabricate a user waiver.

Use `--repo BegoniaHe/wokbuddy-demon` for GitHub operations. A new feature needs
a development Issue before its follow-up PR; record that requirement without
blocking local research or automatically posting to GitHub. Security reports
follow `SECURITY.md` and must not become public Issues or PR descriptions.

## Research

Write `RESEARCH.md` using `references/research.md`. Keep required English
headings and verbatim identifiers; research prose defaults to Simplified
Chinese unless the user requests another language. Summarize the findings in
the user's language instead of pasting the entire report into chat.

Record the request kind and depth, current behavior with `path:line` evidence,
coverage questions, search results, owners, tests, docs, affected contracts, and
remaining hypotheses. Check existing implementations and prior rejections before
proposing new work. An empty search needs a synonym or a second relevant surface
before claiming absence.

For bugs, name a command capable of exposing the reported symptom before
settling on a cause. Distinguish commands actually run from proposed checks.
Report an already-implemented request, a documented rejection, or a security
reporting boundary before drafting tasks that would contradict that evidence.

## Resolve the design

Follow `references/probe.md` to write `BRIEF.md`, `QUIZ.md`, `REFLECT.md`, and
`QUESTIONS.md`. In direct mode these are concise agent-authored records;
`QUIZ.md` records `skipped` and never invents answers or a score for the user.

Prefer the smallest current-path design that satisfies the request. Compare a
larger alternative only when it removes a real duplicated owner, invariant
violation, or limitation. Keep behavior-preserving refactors and feature changes
in separate tasks. Do not restore removed APIs, add speculative interfaces, or
expand the user's scope to justify an alternative.

For small work, one approach and one or two tasks may be enough. For larger
work, explain meaningful tradeoffs and divide independently testable behaviors
into vertical slices with explicit dependencies. Ask for a choice only when
available evidence and the user's instructions do not resolve it.

## Write and validate

Use `references/plan-template.md`. Each task names exact paths, durable symbols,
acceptance criteria, dependencies, and a real verification command. Include
`config.example.json` synchronization and `README.md` updates where the behavior
requires them. Do not paste full implementations into the plan.

Run:

```bash
python3 .agents/skills/plan-issue/scripts/issue_plan.py validate
python3 .agents/skills/plan-issue/scripts/issue_plan.py status
```

Then apply `references/verification.md`. Fix incomplete paths, missing
requirements, unsupported claims, or unsuitable verification commands before
handoff. A passing script is only the mechanical part of review.

## Deliver

Return the plan link, recommended approach, validation result, and any decision
that still blocks execution. A complete workspace includes `RESEARCH.md`,
`BRIEF.md`, `QUIZ.md`, `REFLECT.md`, `QUESTIONS.md`, and `PLAN.md`, with an
empty blocking list. Record the fork Issue URL or the local-slug reason.

For a planning-only request, stop here. For implementation already authorized
in this conversation, use the finished plan and proceed. If implementation
needs new authorization, ask once after the plan is reviewable and explain
that the original request covered planning only.
