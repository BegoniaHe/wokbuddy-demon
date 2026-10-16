---
name: sync-upstream
description: Review or integrate workbuddy2api upstream commits into wokbuddy-demon with per-commit provenance and a searchable decision ledger. Use for upstream sync, adaptation of upstream fixes, conflict resolution, or revisiting recorded sync decisions.
---

# Sync wokbuddy-demon upstream

Use this skill to keep this fork's upstream cursor and integration decisions
auditable without repeatedly loading the entire sync history into context.
Treat `internal/upstream/UPSTREAM.md` as the provenance/policy source of truth
and `upstream-decisions.jsonl` as the durable decision ledger.

Use the requested mode and existing conversation authorization. Review requests
end with a plan; requests to execute an approved plan continue through
integration and verification.

## Upstream reality for this fork

- **Original upstream is gone.** `Sliverkiss/workbuddy2api` was deleted; its
  public history is unrecoverable. Do not attempt to fetch it.
- **The tracked upstream is the previous fork**, `linguo2625469/workbuddy2api-panel`
  (branch `main`). It is the closest live ancestor of this codebase and is the
  only source of "upstream" commits to consider.
- **This fork's history was rebuilt** from a working-tree snapshot. It shares no
  git ancestry with upstream; the sync marker in `upstream-sync.yaml` is a
  *logical* baseline that lives in the upstream remote's history, not in this
  repository's own commit graph.
- Because the histories are unrelated and the fork has diverged (Go module path,
  lint/format baseline, panel/task system), most upstream commits do not apply
  verbatim. **Adaptation is the normal outcome.**

## Locked

- **Default method is `adapt`, not cherry-pick.** Implement the upstream
  behavior against current fork boundaries. Reserve `cherry-pick` for commits
  that are path-disjoint from fork-owned divergence (e.g. a self-contained
  `internal/upstream/*.go` fix) and apply cleanly without edits. `skip` and
  direct `cherry-pick` are rare; justify either explicitly.
- One implementation commit per absorbed upstream commit. `skip` and `revisit`
  produce no implementation commit.
- Preserve upstream subjects. `cherry-pick` uses `git cherry-pick -x` for
  provenance; `adapt` uses the upstream author plus hyphenated trailers.
- Never resolve a conflict by blindly choosing ours/theirs. Stop and surface the
  exact state.
- Do not `git merge` / `rebase` onto `upstream/main`.
- Do not merge the resulting PR, push `main`, tag, or release. A human
  maintainer performs those actions; a verbal exception is not authorization.
- Open absorb PRs against `BegoniaHe/wokbuddy-demon`. Do not open an Issue or PR
  on the deleted original or on the previous fork.

## Open

Which commits to absorb in this window, and whether to open the PR in this
session, unless the user already froze the plan in this conversation.

## Do not

- `git merge` / `rebase` onto `upstream/main`.
- Combine several upstream feature/fix commits into one implementation commit.
- Re-run an interval whose SHAs are already in the ledger.
- File a public security Issue for an upstream vulnerability; coordinate
  privately first.
- Invent a `pending` field in `upstream-sync.yaml`. Apply only the plan the user
  approved in this conversation.

## Operating modes

- **Review mode (default):** inspect the repository, fetch and compare
  `upstream/main`, classify every pending commit, query prior decisions, and
  return a focused plan. Do not cherry-pick, edit files, commit, or push.
- **Apply mode:** use when the user has asked to execute the approved plan in
  this conversation; do not ask for that authorization again. Apply commits
  oldest-first, one at a time, and record each final decision. Stay on a feature
  branch. You may open a pull request on `BegoniaHe/wokbuddy-demon`. Do not
  merge, push `main`, release, or tag.

## Preflight

1. Read `internal/upstream/UPSTREAM.md` (lineage, sync baseline, fork-owned
   evolution) and the conventions in `README.md` / `Makefile`.
2. Inspect `git status --short --branch` and remotes. Do not silently stash,
   reset, checkout, or discard unrelated work.
3. Verify the `upstream` URL and branch from `upstream-sync.yaml`, then fetch:

   ```bash
   git fetch --prune upstream
   ```

4. Inspect the interval and historical decisions with the bundled tools:

   ```bash
   python3 .agents/skills/sync-upstream/scripts/inspect_upstream.py
   python3 .agents/skills/sync-upstream/scripts/upstream_decisions.py list --query "..."
   ```

   The inspector validates that the full marker is an ancestor of
   `upstream/main`. If it is not, stop and report the history mismatch.

## Commit decision workflow

For each commit from the marker to the fetched upstream head, in oldest-first
topological order:

1. Read the commit, patch, changed paths, source PR, and relevant current fork
   implementation (`internal/`, `cmd/`, `scripts/`, `config.example.json`).
2. Query the ledger by exact SHA, source PR, affected path, and feature terms.
3. Reuse a prior decision only when the current fork architecture and policies
   still match. Mark it `revisit` when the architecture, security posture,
   generated contract, or toolchain has changed.
4. Choose exactly one disposition:
   - `adapt` (default): implement the behavior against current fork boundaries;
   - `cherry-pick`: the upstream commit is path-disjoint and compatible as-is;
   - `skip`: intentionally exclude it and state why;
   - `replay`: apply a historical upstream commit only when the repository
     record explicitly authorizes replay;
   - `revisit`: defer because an earlier decision is no longer reliable.
5. In apply mode, cherry-pick compatible commits with provenance, and manually
   adapt the rest. Never resolve a conflict by blindly choosing ours/theirs.
   Stop on an unresolved conflict and show the user the exact state.

Use reason codes such as `fork-architecture`, `go-module-path`, `toolchain`,
`panel-embed`, `config-schema`, `docs-scope`, `release-policy`, `security`,
`no-legacy`, and `upstream-protocol` so future searches remain cheap.

## Integration commit strategy

In apply mode, create at most one implementation commit for each upstream commit
with disposition `cherry-pick`, `adapt`, or `replay`. Process them in
oldest-first topological order. Do not combine several upstream feature or fix
commits into one implementation commit. Commits with disposition `skip` or
`revisit` produce no implementation commit.

Preserve the upstream subject verbatim whenever possible, including its
Conventional Commit type, scope, description, and existing `(#number)` PR
suffix. If the upstream subject has no PR suffix, do not invent one. Change a
subject only when the fork's user-visible semantics materially differ, and
explain that change in the commit body.

Generated or rewritten messages must include `AI-Generated: true` and a UTC
`Generated-At:` footer from `date -u '+%Y-%m-%dT%H:%M:%SZ'`. Do not rewrite a
preserved upstream subject to add that metadata. Do not add AI metadata to a
message that `git cherry-pick -x` kept verbatim.

Use the following method for each disposition:

- `adapt`: apply the behavior manually against the current fork boundaries,
  then commit with `git commit --author="<upstream author>"`. Preserve the
  upstream subject and add the trailers below.
- `cherry-pick`: use `git cherry-pick -x <full-upstream-sha>`. This preserves
  the upstream author and records Git's standard `(cherry picked from commit
  ...)` provenance line. Do not duplicate that line with an `Upstream-Commit`
  trailer unless a conflict or message rewrite removes it.
- `replay`: use only when the ledger explicitly authorizes replay. Treat it as a
  manual adaptation for author and provenance purposes, and cite the authorizing
  historical record in the body or trailers.

For adapted, replayed, or conflict-rewritten commits, use standard Git trailers
with hyphenated tokens:

```text
Upstream-Commit: <full upstream SHA>
Upstream-Author: <name> <email>
Upstream-PR: linguo2625469/workbuddy2api-panel#<number>
Sync-Disposition: adapt
Fork-Adaptation: <concise explanation of the fork-specific implementation>
Tested: <focused validation command>
```

Omit a trailer when its value is unavailable; never use non-standard tokens with
spaces such as `Source PR`. Keep `Signed-off-by` separate from sync provenance
and do not add it unless repository policy or the user explicitly requires it.

Recommended adapted-commit shape:

```text
<upstream subject>

<optional fork-specific explanation>

Upstream-Commit: <full upstream SHA>
Upstream-Author: <name> <email>
Upstream-PR: linguo2625469/workbuddy2api-panel#<number>
Sync-Disposition: adapt
Fork-Adaptation: <what changed for this fork>
Tested: <command>
AI-Generated: true
Generated-At: <UTC ISO 8601 timestamp>
```

The final cursor update is a separate metadata-only commit with subject
`chore(sync): record upstream integration`. Do not hide implementation changes
inside this cursor commit. A release/version bump that is intentionally skipped
still advances the cursor only after its `skip` decision is recorded and the
skip rationale is written to `upstream-sync.yaml`.

After each implementation commit, verify the mapping before moving on:

```bash
git show -s --format='%H%n%an <%ae>%n%cn <%ce>%n%s%n%B' HEAD
git show --stat --oneline HEAD
```

The integration commit must be attributable to exactly one upstream SHA, and its
author must be the upstream author for `cherry-pick`, `adapt`, and `replay`
unless the user explicitly directs otherwise. The fork maintainer is normally
the committer.

## Conflict and duplicate handling

Never resolve a conflict by blindly choosing ours or theirs. Pause the sequence,
inspect the conflicting path and the missing context, and identify possible
prerequisite commits before editing:

```bash
git log HEAD..<upstream_sha>^ -- <conflicted-path>
git log -G'<relevant-symbol-or-string>' HEAD..<upstream_sha>^ -- <conflicted-path>
```

If the conflict is a mechanical context adjustment and preserves upstream
behavior, continue the `cherry-pick`; inspect the final message and add an
`Upstream-Commit` trailer if Git did not retain `-x` provenance. If the
resolution changes behavior to fit fork architecture, classify the result as
`adapt`, preserve the upstream author with `--author`, and record the reason in
`Fork-Adaptation`. If the prerequisite or intended behavior cannot be
established, run `git cherry-pick --abort` and use `revisit` rather than
guessing.

Before creating a new implementation commit, check whether the change was
already integrated under a different fork commit:

```bash
git log --all --grep='Upstream-Commit: <full-upstream-sha>' --format='%H %s'
```

An already-equivalent patch needs an explicit ledger decision and provenance; do
not create a duplicate implementation commit merely because the SHA differs.

## Fork-specific gates

Before accepting a decision, check the affected contract:

- `go.mod` / `go.sum`: keep the module path `github.com/BegoniaHe/wokbuddy-demon`
  and Go 1.27; run `make tidy` when dependencies change.
- Go code must pass `make fmt-check lint` (golangci-lint v2, gofumpt + gci per
  `.golangci.yml`). Adapt formatting; do not weaken the rule set.
- Panel assets are embedded (`internal/panel/*.html`, `*.js`); keep the embed
  wiring and the panel smoke tests intact.
- `config.example.json` is the complete, secret-free config reference; mirror
  any new config field there and keep panel hot-apply/restart semantics coherent
  (`cmd/server/main.go` `restartRequiredFields`).
- User-visible behavior changes update `README.md`; provenance/lineage changes
  update `internal/upstream/UPSTREAM.md`.
- Release discipline: the fork is **rolling and version-less**. `appVersion` in
  `cmd/server/main.go` is a build identifier injected at compile time via
  `-ldflags -X` (default `dev`); there is no version line or tag↔version
  assertion to keep in sync.

## Ledger operations

Initialize the ledger when the first decision is ready:

```bash
python3 .agents/skills/sync-upstream/scripts/upstream_decisions.py init
```

Add or revise a decision without loading the ledger into context:

```bash
python3 .agents/skills/sync-upstream/scripts/upstream_decisions.py add \
  --commit <full-sha> --disposition adapt --summary "..." \
  --source-pr 152 --reason-code fork-architecture --path internal/upstream \
  --fork-adaptation "..." --integration-commit <full-sha>

python3 .agents/skills/sync-upstream/scripts/upstream_decisions.py update \
  --commit <full-sha> --disposition revisit --summary "..." \
  --reason-code fork-architecture --revisit-when "..."
```

Query current decisions, full event history, or validate the append-only log:

```bash
python3 .agents/skills/sync-upstream/scripts/upstream_decisions.py get --commit <sha>
python3 .agents/skills/sync-upstream/scripts/upstream_decisions.py get --commit <sha> --history
python3 .agents/skills/sync-upstream/scripts/upstream_decisions.py list --path internal/upstream
python3 .agents/skills/sync-upstream/scripts/upstream_decisions.py validate
```

Use `delete --reason` only to append a tombstone; never remove historical JSONL
lines. Use `import-git --range <old>..<new>` to recover structured decisions
from existing sync commit messages. Imported records are marked as inferred and
must be reviewed before being treated as authoritative.

## Version and changelog finalization

This fork is **rolling and version-less**: it keeps no semantic version line and
publishes no versioned Release (only nightly builds). After the audited upstream
interval has been integrated:

1. Do **not** introduce or bump a version number, and do **not** create a
   `changelogs/` entry. There is no `appVersion` value to synchronize with an
   upstream release; `appVersion` is a compile-time build identifier, not a
   release version.
2. An absorbed upstream release/version bump is `skip` by default — record the
   disposition in the ledger with reason `release-policy`, and do not mirror the
   upstream version anywhere in this fork.
3. If the reviewed interval contains no release/version change, nothing extra is
   required beyond the per-commit dispositions and the cursor update.

## Verify

Advance `upstream-sync.yaml` only after every commit in the audited interval has
an explicit disposition and the implementation/skip rationale is complete. Use
the full `git rev-parse upstream/main` SHA, UTC time, source PR provenance, and
an honest summary of adaptations, skips, replays, conflicts, and tests. If the
interval is incomplete, leave the cursor unchanged.

Run focused tests first, then the repository gates: `make test`, `make lint`,
`make fmt-check`, `make build` (or `make ci` for the full local CI). Finish with
a concise mapping of upstream commits to dispositions, changed files, checks
run, and residual risk. Do not claim that an upstream commit was integrated
merely because it was reviewed or recorded.

## Bundled tools

- `scripts/inspect_upstream.py`: read-only cursor/ref validation and pending
  commit report, with optional JSON output.
- `scripts/upstream_decisions.py`: append-only JSONL ledger CLI for init, add,
  update, soft-delete, get, list/filter, validation, and Git-history import.

Both tools use only the Python standard library and accept explicit paths, so
they do not add project dependencies or require loading the complete ledger.
