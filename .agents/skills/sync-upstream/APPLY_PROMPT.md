# Apply a reviewed upstream integration plan

Use the sync-upstream skill in apply mode. In OpenCode, load `sync-upstream`
with the skill tool; in Codex CLI, invoke it as `$sync-upstream`. Execute the
reviewed plan supplied in this conversation and carry it through integration,
ledger updates, and verification.

## Inputs and authorization

Find the reviewed commit list, dispositions, interval, and push/PR instructions
in the current conversation or the supplied plan artifact. Reuse decisions and
authorization already given. If the plan is missing, inspect the cursor and
ledger, then ask for the missing plan before applying commits. Do not invent
approved dispositions.

Read these sources as needed; cite them instead of pasting them:

- `internal/upstream/UPSTREAM.md`: lineage, sync baseline, fork-owned evolution.
- `.agents/skills/sync-upstream/SKILL.md`: apply workflow and provenance.
- `upstream-sync.yaml`: cursor; there is no `pending` field.
- `upstream-decisions.jsonl`: durable decisions and replay authorization.

## Prepare

Inspect `git status --short --branch` and `git remote -v`. Preserve unrelated
work and use a feature branch or isolated worktree. Verify `upstream` points to
`https://github.com/linguo2625469/workbuddy2api-panel.git`; add the remote if
absent or correct its URL if needed. Run:

```bash
git fetch --prune upstream
python3 .agents/skills/sync-upstream/scripts/inspect_upstream.py
```

Read patches with `git show <sha>`. Keep the fork branch as the integration
base. Check the ledger before repeating any prior integration.

## Apply and verify

Process the reviewed interval oldest-first. Create one implementation commit per
`adapt`, `cherry-pick`, or `replay` item; `skip` and `revisit` create none.
Adaptation is the default disposition for this fork. Preserve upstream subjects
and authors, with the skill's provenance trailers for adaptations. Resolve
routine conflicts against current fork behavior. Record unresolved intent as
`revisit` instead of guessing.

Run focused tests, then the repository gates: `make fmt-check lint test build`
(or `make ci`). Record each disposition in the ledger. Advance the cursor only
after every SHA has a disposition and the interval is complete, using the
metadata-only commit `chore(sync): record upstream integration`.

Push a feature branch only when already requested. Follow the conversation's PR
instructions and target `BegoniaHe/wokbuddy-demon`; verify the returned URL. A
human maintainer performs merges, pushes to `main`, tags, and releases.

## Deliver

Return the branch, commit/disposition mapping, ledger and cursor status, checks
actually run, remaining risks, and PR URL if one was opened. Preserve checkpoint
state if blocked so the next session can resume without repeating completed
commits.
