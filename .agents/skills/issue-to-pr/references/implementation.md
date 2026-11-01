# Implementation loop

Load this before the first edit. The plan says what to build; this file says how
to keep each slice provable and small.

## One slice at a time

A slice is one task from `PLAN.md`: a behavior a reviewer can accept or reject
alone. Take its red test, make it green, then leave the code better than it
started only where the task says so.

- **Red before green.** Write the failing assertion first, watch it fail for the
  expected reason, then write the minimum that passes. Do not pre-write tests
  for later tasks; they encode imagined behavior.
- **Vertical, not horizontal.** A slice may touch store, handler, and panel
  together when that is what makes one behavior observable. It must not be "all
  the types" or "all the tests".
- **No speculative generality.** Add an option, interface, or adapter only for a
  caller in this change. The next task can introduce the next thing.
- **Prefactor separately.** A planned behavior-preserving refactor is its own
  commit before the feature; never mixed into the same diff.

## Seams

A seam is the public boundary a test observes: the HTTP handler, the pool's
exported method, the upstream request builder, the config loader, the panel
handler. Tests live at seams, not against private helpers.

Prefer the highest existing seam that reaches the behavior:

| Surface              | Typical seam                                        |
| -------------------- | --------------------------------------------------- |
| Gateway route        | `httptest` request through the router               |
| Account selection    | exported `pool` method with fake accounts and clock |
| Scheduler job        | job function with a pinned clock and fake upstream  |
| Upstream protocol    | request/response builder plus an SSE fixture        |
| Panel API            | handler call with a temp state file                 |
| Config hot-apply     | loader + snapshot assertions, no live server        |

A test on a private function is allowed only when no public seam exists and the
plan says so. If the only available seam cannot reproduce the bug's call chain,
that gap is itself a finding: report it rather than shipping a shallow test that
gives false confidence.

## Anti-patterns

- **Implementation-coupled**: mocks internal collaborators, asserts private
  fields, or reads the database instead of calling the interface. It breaks on
  refactor while behavior is unchanged.
- **Tautological**: the expected value is computed the way the code computes it,
  so the test cannot disagree. Use a known-good literal, worked example, or
  upstream fixture.
- **Green but wrong**: the test passes without exercising the reported path.
  Run the red command against the pre-change code at least once.
- **Check mutation**: editing the assertion, skipping a case, or loosening a
  gate to get green. Stop and report instead.

## Work the frontier

Tasks are a graph, not a list. Start any task whose `Blocked by` is complete.

- **AFK**: proceed alone. Run the focused verify, log it, move on.
- **HITL**: the task names the gate. Ask for that decision, credential, or live
  observation once, record the answer in `PROGRESS.md`, then continue.
- **Parallel**: only disjoint AFK tasks, and only with a worktree per task.
  Do not run two agents over the same files; a merge conflict is not evidence.
- **Re-plan trigger**: a slice needs files outside its task, a new config field
  the plan did not name, or a changed public contract. Stop, update the plan
  through `plan-issue`, then continue.

## Bug loop

A `bug` task is not done until the reported symptom has a command that goes red
before the fix and green after.

1. Run the plan's red-capable command and capture the exact symptom.
2. Shrink the repro until every remaining element is load-bearing.
3. List 3–5 falsifiable causes before testing one; change one variable at a
   time.
4. Add the regression test at the seam that matches the real call chain.
5. Re-run the original, unminimised repro and remove temporary instrumentation.

No red command, no fix. Reading code until a plausible cause appears is the
failure this loop prevents.

## Progress record

`PROGRESS.md` is the evidence trail, not a diary. One line per task or check:

```markdown
- Task 2 AFK: red `go test ./internal/pool/ -run TestSelectSkipsCooldown`; PASS after change.
- Task 2: `make lint` PASS.
- Task 3 HITL: asked whether the panel should show an empty state; answered yes.
- Review standards: fixed stale path in `internal/pool/doc.go`.
```

Redact credentials, tokens, and account identifiers as `<REDACTED>`. Quote only
the lines that carry the signal.

## Stop and report

Stop the loop and report when any of these is true:

- The change would weaken a security invariant in `AGENTS.md`.
- The plan contradicts the code and the fix would change the agreed scope.
- A check cannot fail on the changed behavior, so green proves nothing.
- The Issue is a vulnerability; follow `SECURITY.md` instead of this loop.
- The branch cannot stay green without a wider refactor than the plan allows.
