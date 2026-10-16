# Plan checker

Load this **after** `PLAN.md` exists. The script cannot judge coverage,
invariants, or whether a verify command actually proves the slice. That
is why this file exists: mechanical bars live in `issue_plan.py`; this
file is the agent judgment pass. Do not paste it into `SKILL.md`.

```bash
python3 .agents/skills/plan-issue/scripts/issue_plan.py validate
```

Script failures are **BLOCKING**. Judgment findings are **BLOCKING**
when execution would fail, ship a forbidden fork artifact, or miss a
requirement; otherwise **SUGGESTIONS**.

## Mechanical bars (script)

Do not re-check headings, quiz counts, `Modify:` paths, or task cycles
by hand. Full validate requires `RESEARCH.md`, `BRIEF.md`, `QUIZ.md`,
and `REFLECT.md`. When workspace `probe` is `skipped`, `QUIZ.md` may
use `**Verdict:** skipped` without five questions. `BRIEF.md` and
`REFLECT.md` stay required as agent-authored artifacts.

## Judgment bars (agent)

Map each Issue requirement, desired-behavior line, and coverage-ledger
row to a task ID, or to Out of scope with a reason. Unmapped tasks and
zero-coverage requirements are BLOCKING.

1. **Coverage:** name the task that implements it, or move it to Out of
   scope with a reason. Every coverage-ledger row is answered or
   explicitly open in the plan's Open questions.
2. **Evidence:** current-behavior claims cite `path:line` that this
   session read. RESEARCH.md includes a search log, per-row coverage notes,
   and named impact paths. Its default prose language is Chinese unless the
   user requested another language.
   Inference is labeled as inference. Empty search is not treated as
   absence without a second surface.
3. **Redundancy:** the plan does not rebuild an existing path.
4. **Invariants:** no task restores a removed surface or weakens a
   security invariant from `AGENTS.md`. An `AGENTS.md` MUST conflict
   is CRITICAL / BLOCKING; do not dilute the principle in the plan.
5. **Verify realism:** each command exists in this checkout and matches
   the files touched. A panel-only slice must not prescribe
   `make test` as its only proof when a focused `go test ./internal/panel/` exists.
6. **Docs/config:** user-visible behavior has matching `README.md` work,
   and new config fields are mirrored in `config.example.json`, or an
   explicit `none` with reason.
7. **Credentials:** no task writes credentials, state, or `config.json`
   into the repo, logs, or panel responses.
8. **Scope:** no drive-by refactors unless `REFLECT.md` recommended
   them and the path is `better` (user pick, or skipped-probe
   recommendation). No new dependencies without a stated need, no upstream
   publish/docs claims presented as fork artifacts. No new seam unless two
   real adapters already exist or the path is `better` for that reason.
9. **Decisions:** `BRIEF.md` matches the requested outcome. `skipped` is valid
   for direct planning or a user-ended interview, with the reason recorded in
   `QUESTIONS.md`. Never fabricate a waiver, answer, or path selection.
   The plan follows the user's decisions and `REFLECT.md`'s recommendation;
   unresolved non-blocking choices remain Open questions. A guided quiz score
   does not replace concrete acceptance criteria or resolve a contradiction.
10. **Impact:** callers, configs, tests, and docs from the
    research impact surface are tasks or explicit out-of-scope.
11. **Consistency:** symbol names and `path:line` cites do not drift
    across RESEARCH / BRIEF / REFLECT / PLAN. Different artifact languages
    are not drift; keep required headings and code identifiers stable.
    Vague adjectives (fast, robust, intuitive) in
    Desired behavior without a measurable outcome are BLOCKING.
12. **Bug loop:** a `bug` plan names the red-capable command from
    research. A missing loop is BLOCKING.

Report the judgment pass as:

```markdown
**Status:** BLOCKED | APPROVED WITH SUGGESTIONS | APPROVED
**BLOCKING:** …
**SUGGESTIONS:** …
```

Empty BLOCKING is good if you actually checked.

## Fail the plan when

- A task says "add tests" without a test path and command
- A task is "implement the feature"
- A material architecture choice lacks a rationale or remains unresolved
- The Issue is a vulnerability
- The only proof is `make ci` on a behavior change
- The plan implements the ticket patch after reflection showed a
  different job and the chosen path is `better`
- A requirement has no task and is not in Out of scope
- A `Modify:` path does not exist
- Decision coverage still has unresolved contradictions that would change
  architecture or acceptance
- Quiz verdict is `skipped` but the workspace `probe` is not `skipped`

## After a pass

Show the workspace path and validation result. A planning-only request ends
with the plan. Continue implementation if it was already authorized in this
conversation and no blocking decision remains. Ask once for new authorization
only when needed, after the plan is concrete and reviewable.
