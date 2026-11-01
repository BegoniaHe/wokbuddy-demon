# PR body and review

The repo template wins. Do not import another PR template, and do not invent
headings beside the ones it already has. This file explains how to fill it well.

## Shape

The scaffold from `pr_ready.py start` already has the headings. Fill them in
this order:

1. **Summary** — expected versus actual for a fix, or the user-visible change
   for a feature. Two or three sentences, not a file list.
2. **Related issue** — `Fixes #<n>` when the PR closes it, `Related: #<n>` when
   it only advances it. One issue per PR is the default.
3. **Root cause / Behavior / Behavior guarantee** — the section the template
   provides. Name the mechanism, not only the symptom.
4. **Implementation notes** — the decision a reviewer would otherwise question:
   config hot-apply versus restart, why this path, what was deliberately left
   alone.
5. **Validation** — the evidence block described below.
6. **Compatibility and risk** — `Door:` and `Blast radius:` plus API, panel,
   security, performance, migration notes, or `None`.
7. **Checklist** — tick only what is true for this change.
8. **Agent note** — last section, always present for an agent PR.

Non-goals matter. State the adjacent behavior this PR does not change when the
Issue invites the assumption that it does.

## Evidence

`Validation` must let a reviewer reproduce the proof:

````markdown
```text
red   go test ./internal/pool/ -run TestSelectSkipsCooldown   # failed before the fix
green go test ./internal/pool/ -run TestSelectSkipsCooldown   # PASS
gate  make fmt-check lint test                                  # PASS
```
````

A screenshot is good evidence only when the change is visual and the environment
can take one. A command that was not run this session does not belong here. If
the full suite was not run, say which gate was skipped and why.

## Door and blast radius

State whether the change is reversible.

- `Door: two-way` — a revert restores the old behavior with no outside state.
- `Door: one-way` — a migration, a credential or token format change, a
  destructive action, or a decision that cannot be unshipped by reverting code.

`Blast radius:` names what can break: gateway callers, panel users, auth files,
upstream compatibility, nightly builds. Keep it one line; a vague "low impact"
is not a call a reviewer can disagree with.

## Agent note

Use the user's language unless the PR body is English by template convention.

```markdown
## Agent note

Goal: <one line>
Paths: `internal/pool/select.go`, `internal/pool/select_test.go`
Checks: `go test ./internal/pool/ -run TestSelectSkipsCooldown`, `make lint`
Risk: <residual risk, or "none identified">
Tools: <client/model used for the work>
```

Never write `## Human note`. A human author writes that section in their own
words; an agent PR deletes it.

## Review before opening

Run both axes and keep the findings separate.

**Spec** — read the Issue and `PLAN.md` acceptance criteria, then the diff:

- every requirement and `Covers:` ID lands in a hunk or in out-of-scope;
- no behavior the Issue did not ask for;
- no acceptance criterion satisfied only in prose.

**Standards** — read the repo rules, then the diff:

- `AGENTS.md` invariants and no-legacy rule hold;
- `README.md` updated for user-visible behavior, `config.example.json` for new
  fields;
- no unrelated refactor, dead code, or stale path in a comment;
- tests observe a seam and would fail without the change;
- commit headers and footers follow the Conventional Commit reference.

Fix blocking findings. Record accepted ones as residual risk rather than hiding
them. Then run `pr_ready.py check`.

## Updating the PR

When the diff changes materially after review, rewrite the body's Summary,
Validation, and Compatibility sections; a stale body is worse than none. Use
`gh pr edit --body-file` and confirm the URL. Do not comment on the PR or
request reviewers unless the user asked.
