# Plan template

Default `PLAN.md` to English unless the user requests another language. Keep
the required English headings and verbatim paths, commands, APIs, and error
strings. Copy this skeleton; delete unused optional sections.

````markdown
# <Title> Implementation Plan

**Goal:** one sentence
**Issue:** URL under github.com/BegoniaHe/wokbuddy-demon, or `local-<slug>` plus
"open a development Issue before the PR"
**SHA:** full object name this plan was researched against
**Architecture:** 2–3 sentences
**Recommended approach:** name, why it wins, what was rejected
**Depth:** `small` / `medium` / `large` / `complex`
**Probe:** `skipped` for direct planning, or guided quiz verdict
`pass` / `fail` / `override`; reason and selected approach from `REFLECT.md`

## Constraints

- Go `1.27`; no legacy shims; current-path design only
- Rolling release: no version numbers; nightly build, version injected at compile time
- User-visible behavior updates `README.md`; new config fields sync `config.example.json`
- Credentials (`auths/`), state (`data/`), and `config.json` never enter the repo
- Agents must not merge or push `main`

## Current behavior

Cite `path:line`. Note the impact surface (callers, configs, tests,
docs) a change here would touch.

## Desired behavior

Testable outcomes, including failure paths that matter. Prefer
WHEN/THEN (or WHILE/IF) so each line has one interpretation.

## Out of scope

Bullet list. Include adjacent features that will look related.

## Tasks

### Task 1: <observable slice>

**Blocked by:** none (or Task N)

**Files:**

- Modify: `internal/exact/path.go` (`symbol_name`)
- Test: `internal/exact/path_test.go`

**Acceptance:**

- [ ] WHEN <trigger> THEN <observable>
- [ ] IF <failure> THEN <observable>

**Verify:**

```bash
go test ./internal/exact/ -run TestName
```

Expected: PASS (or the named failure if this is a red test)

### Task 2: <next slice>

...

## Verification

End-to-end commands for the whole change, focused first, then the
smallest extra gate the slice actually touches (`make test`, `make lint`,
`make ci`).

## Docs / config

`none` or the `README.md` page and `config.example.json` fields the
change adds or alters.

## Risks

| Risk | Impact       | Mitigation |
| ---- | ------------ | ---------- |
| ...  | high/med/low | ...        |

## Open questions

Non-blocking assumptions only. Blocking questions belong in
`QUESTIONS.md` until answered.
````

## Task rules

- One vertical slice per task: a behavior a reviewer could accept or
  reject on its own.
- Name files and symbols. Add a short code sketch only for a new
  contract (function signature, schema field, event name). Prefer
  existing interfaces; justify any new one with current consumers and
  the requested behavior rather than hypothetical adapters.
- Every task has **Files**, **Acceptance**, and **Verify** with a real
  command from `REFERENCE.md`. **Blocked by** is required when tasks
  are not a linear chain.
- Fold scaffolding, config, and docs into the task that needs them.
- If a task would exceed about five files or two independent
  subsystems, split it.
- A behavior-preserving prefactor and the feature change are separate
  tasks. Do not mix them.
- A wide mechanical rename whose blast radius cannot stay green as a
  vertical slice uses expand → migrate batches → contract, not one
  tracer bullet.
- Conventional Commit type for the follow-up PR goes in the last task
  or a short Handoff line (`feat` / `fix` / `docs` / `chore`, …). Do
  not commit during planning.
