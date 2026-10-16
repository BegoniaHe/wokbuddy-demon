# Clarify and compare approaches

Use research and the user's stated goal to resolve the plan. Direct planning
is the default; a guided interview is available when requested. Both Codex CLI
and OpenCode use the same workspace fields and validator.

Facts about code are the agent's responsibility. Ask the user for decisions
that change scope, architecture, behavior, or acceptance. Do not test their
repository knowledge as a prerequisite to producing a plan.

## Direct planning

Initialize with `--skip-probe`, or select direct mode for an existing workspace:

```bash
uv run python .agents/skills/plan-issue/scripts/issue_plan.py skip-probe
```

Write the following records concisely; they do not require separate approval:

1. `BRIEF.md`: `**Problem:**` with the affected user, present behavior, and
   desired outcome; name the runtime owner from research.
2. `QUIZ.md`: record that no guided interview was used. Preserve any real
   answers from an earlier guided phase, but do not invent questions.
3. `REFLECT.md`: compare the current-path change with any justified alternative
   using the headings below. If a larger design has no benefit, say so.
4. `QUESTIONS.md`: record `**Probe:** skipped` and the actual reason, such as
   direct planning, sufficient request detail, or an explicit user preference.
   Include decisions already answered and remaining assumptions.

The quiz record is:

```markdown
# Quiz

**Total:** 0/10
**Verdict:** skipped
**Reason:** direct planning; no guided interview requested
```

`0/10` is a mechanical field for the skipped record, not a user assessment.
Skipping an interview does not resolve a blocking product choice. Ask that
choice while continuing independent work; put non-blocking unknowns in the
plan's Open questions.

## Depth

Record depth in `RESEARCH.md` and scale the plan to the actual change:

| Depth     | Typical scope                          | Design detail                                 |
| --------- | -------------------------------------- | --------------------------------------------- |
| `small`   | One behavior, a few files              | One approach and one or two tasks             |
| `medium`  | A bounded feature                      | Compare meaningful architecture choices       |
| `large`   | Several owners or independent slices   | Vertical slices with explicit dependencies    |
| `complex` | Unresolved behavior or a new subsystem | Resolve blocking questions before task detail |

Use a capability map when dependent slices would otherwise be hard to follow.
Do not invent alternatives or requirements to fill a quota.

## Reflection record

Use these headings, which the workspace validator checks:

```markdown
# Reflect

## Inferred goal

The user's desired outcome and relevant context. Distinguish explicit intent
from an assumption; do not replace the request with an inferred larger project.

## Why-chain

Explain the observed problem and the executable change that addresses it.
Trace further only when the cause remains uncertain.

## Surgical path

The smallest current-path change that meets the request, with owners and limits.

## Better path

A justified structural alternative, or why no larger change is warranted.

## Recommendation

`surgical` | `better` | `stop`, with evidence and the user's relevant decisions.
```

Prefer existing owners and interfaces. An abstraction earns its place when it
serves current callers or removes real duplication; do not introduce one only
for a hypothetical adapter or a mock. Repository security and no-legacy rules
apply to every option. Ask for a path choice only if it would materially change
the agreed scope and the conversation does not already settle it.

## Optional guided interview

Use this only when the user requests a quiz or facilitated design discussion.
Initialize without `--skip-probe`. Brief the problem, invite corrections, and
ask issue-specific questions about current behavior, constraints, acceptance,
design tradeoffs, and intended outcome. Use the active client's question tool
when available; otherwise ask in chat. Accept free-text answers.

For this mode, the existing quiz validator requires five `### Question`
sections, `**Total:** n/10`, and `**Verdict:** pass|fail|override`. Use 0–2 per
answer to record how fully it resolves the decision, with `pass` at 7 or above.
This measures specification completeness, not the user's code knowledge. Record
the question, options if used, actual answer, score, and remaining uncertainty.
Explain material conflicts directly; do not force a retry or another quiz.

Use `override` when the user chooses to proceed despite remaining uncertainty.
If they end the interview, run `skip-probe`, preserve their answers, mark the
verdict `skipped`, and continue in direct mode. Neither a score nor a path
selection authorizes implementation or external writes.

## Closing coverage

Record functional scope, contracts, failure paths, invariants, integrations,
terminology, and acceptance as Clear, Resolved, Deferred, or Outstanding in
`QUESTIONS.md`. For larger changes, check relevant input bounds, authorization,
concurrency, state transitions, external failures, and observability.

Ask only about unresolved high-impact decisions. Keep non-blocking assumptions
in the plan and continue. A blocking contradiction must be resolved before the
plan can be described as ready to execute. Deliver the concrete plan before
requesting any new implementation authorization; existing authorization remains
valid across planning stages.
