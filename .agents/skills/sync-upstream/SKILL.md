# Skill: sync-upstream

# Audit the reference fork and open issues

Use this skill to keep the reference fork's cursor auditable **without absorbing
its code**. Every candidate commit is checked against the latest official
WorkBuddy Win32 client and the current fork; confirmed gaps become GitHub
Issues. Implementation happens later through normal issue-driven PRs.

## Upstream reality for this fork

- **Original upstream is gone.** `Sliverkiss/workbuddy2api` was deleted.
- **The tracked reference fork is `misakano7545/workbuddy2api-panel`**
  (branch `main`). It is the source of commits to audit.
- **The previous fork `linguo2625469/workbuddy2api-panel` is retired.** Its
  history is only provenance; do not track new commits from it.
- **Official client is the protocol authority.** The latest Win32 package
  (CN + global) is the primary fact source, not the reference fork. Reference
  commits are clues; their claims must be confirmed against the official
  package or clearly marked as live/assumption.
- This fork's history was rebuilt; it shares no ancestry with the reference
  fork. The cursor in `upstream-sync.yaml` is a logical audit baseline.
- Because histories diverge, most reference commits will be `issue`, `skip`,
  or `revisit`; **none are applied by this skill**.

## Locked

- **No direct absorption.** Do not cherry-pick, adapt, replay, merge, rebase,
  or edit implementation code in this workflow. The only outputs are Issues,
  ledger records, and cursor updates.
- **Official-client comparison is mandatory.** Before judging a commit, load
  the latest Win32 packages and compare with `reverse-workbuddy` facts. If the
  package does not support the claim, do not record it as confirmed.
- **One issue per confirmed gap**, unless an existing open issue already covers
  it. Link the existing issue instead of opening a duplicate.
- **Issues only on `BegoniaHe/wokbuddy-demon`.** Never open an Issue or PR on
  the reference fork, the retired previous fork, or the deleted original.
- **No public security Issue.** For vulnerabilities, coordinate privately
  first (security advisory), then record `revisit` without sensitive details.
- **Never resolve uncertainty by guessing.** Use `revisit` when static evidence
  is insufficient or a live probe is required.
- **Cursor discipline.** Advance `upstream-sync.yaml` only after every commit
  in the audited interval has an explicit disposition and every `issue`
  disposition has a URL (or an existing issue link).
- Do not merge, push `main`, tag, or release.

## Open

- Which interval to audit, unless the user already fixed it in this
  conversation.
- Whether to open the Issues in this session or return an audit plan first.
- Whether a confirmed gap is already tracked by an open Issue.
- Implementation is **out of scope**. If the user later asks to implement an
  Issue, use the normal issue-driven branch/PR flow; do not turn this skill
  into an absorption workflow.

## Do not

- `git cherry-pick`, `git merge`, or `git rebase` onto the reference fork.
- Edit `internal/`, `cmd/`, `web/`, or any implementation file.
- Treat a reference-fork commit as protocol evidence by itself.
- Copy reference-fork release/tag/module-path changes into this fork.
- Re-audit SHAs already dispositioned in the ledger unless the architecture or
  official baseline materially changed.
- Invent a `pending` field in `upstream-sync.yaml`.

## Operating mode

This skill has one mode: **audit**.

1. Fetch the reference fork and identify the interval from the cursor.
2. Refresh the latest official Win32 client baseline.
3. Evaluate every commit oldest-first.
4. Open Issues for confirmed gaps.
5. Record dispositions and advance the cursor.

Implementation is a separate, issue-driven activity and must not happen during
this audit.

## Preflight

1. Read `internal/upstream/UPSTREAM.md`, `AGENTS.md`, and the latest
   `upstream-sync.yaml`.
2. Inspect `git status --short --branch` and remotes. Preserve unrelated work.
   Verify `upstream` points to
   `https://github.com/misakano7545/workbuddy2api-panel.git`; add or correct it
   if needed, then fetch:

   ```bash
   git fetch --prune upstream
   ```

3. Refresh the official client baseline with the `reverse-workbuddy` skill:

   ```bash
   node .agents/skills/reverse-workbuddy/scripts/download_latest.mjs --dry-run
   node .agents/skills/reverse-workbuddy/scripts/download_latest.mjs
   node .agents/skills/reverse-workbuddy/scripts/protocol_sweep.mjs --root .tmp/workbuddy
   ```

   Use Win32 packages as facts. macOS packages are auxiliary only.

4. Inspect the interval and prior decisions:

   ```bash
   python3 .agents/skills/sync-upstream/scripts/inspect_upstream.py
   python3 .agents/skills/sync-upstream/scripts/upstream_decisions.py list --query "..."
   ```

5. If the official baseline is missing or the package version cannot be
   confirmed, stop and report; do not audit against memory.

## Per-commit audit workflow

For each commit from the cursor to the fetched head, oldest-first:

1. **Read the commit**: subject, body, patch, changed paths, source PR, and the
   current fork implementation for the same behavior.
2. **Query the ledger** by exact SHA, source PR, affected path, and feature
   terms. Reuse a prior decision only when the architecture and official
   baseline still match.
3. **Check official evidence** using `reverse-workbuddy`:
   - `cli/product.json`: realm domains, endpoints, auth attributes.
   - `cli/dist/*.mjs`: client logic, code tables, refresh/thinking behavior.
   - `main/*.js` / renderer assets: headers and request shapes.
   - Record the file plus a searchable snippet; distinguish:
     - package fact;
     - live traffic fact (date + endpoint);
     - assumption (must not be treated as confirmed).
4. **Choose exactly one disposition**:
   - `issue`: confirmed gap in this fork; open a new Issue or link an existing
     one.
   - `skip`: not applicable, already covered, conflicts with official client,
     docs-only, release policy, or reference-only infrastructure.
   - `revisit`: static evidence is insufficient or a live probe is required.
   - Historical `adapt` / `cherry-pick` / `replay` records remain readable,
     but **do not create new ones**.
5. **Open the Issue** when disposition is `issue`. Include:
   - affected area and observable impact;
   - official client evidence (path + snippet/offset);
   - current fork evidence (`path:line`);
   - proposed fix and acceptance criteria;
   - out-of-scope notes;
   - reference-fork commit as a clue, not as protocol authority.
   Check open/closed Issues first. Label `bug` / `enhancement` / `task`.
6. **Record the decision** in the ledger with the official baseline and issue
   URL (see below).
7. Move on. Do not implement.

## Issue quality gates

Before creating an Issue, verify:

- [ ] The claim is confirmed by the latest official Win32 package, or clearly
      labeled as live-evidence/assumption (assumption → `revisit`, not `issue`).
- [ ] Current fork behavior is verified at a specific `path:line`.
- [ ] Impact is concrete: wrong routing, stale token, wrong stop reason, etc.
- [ ] Proposed fix respects current fork boundaries (no legacy shims).
- [ ] No secrets, tokens, credentials, or private account data.
- [ ] No duplicate open/closed Issue or PR.
- [ ] The Issue body names the exact official client version and commit.

If any gate fails, use `revisit` or `skip`.

## Ledger operations

Initialize when needed:

```bash
python3 .agents/skills/sync-upstream/scripts/upstream_decisions.py init
```

Record an audited commit:

```bash
python3 .agents/skills/sync-upstream/scripts/upstream_decisions.py add \
  --commit <full-reference-sha> \
  --disposition issue \
  --summary "..." \
  --source-pr 83 \
  --reason-code upstream-protocol \
  --path internal/auth \
  --issue https://github.com/BegoniaHe/wokbuddy-demon/issues/282 \
  --official-baseline "cn 5.8.3.41334564-db0e9b51 / global 5.8.4.41366630-73952edb" \
  --notes "Official evidence: ...; fork gap: ..."
```

Record a non-action:

```bash
python3 .agents/skills/sync-upstream/scripts/upstream_decisions.py add \
  --commit <full-reference-sha> \
  --disposition skip \
  --summary "..." \
  --reason-code fork-architecture \
  --notes "Why this is not applicable or is already covered"
```

Record an unverified claim:

```bash
python3 .agents/skills/sync-upstream/scripts/upstream_decisions.py add \
  --commit <full-reference-sha> \
  --disposition revisit \
  --summary "..." \
  --reason-code upstream-protocol \
  --revisit-when "live probe or newer official package"
```

Query and validate:

```bash
python3 .agents/skills/sync-upstream/scripts/upstream_decisions.py get --commit <sha>
python3 .agents/skills/sync-upstream/scripts/upstream_decisions.py list --disposition issue
python3 .agents/skills/sync-upstream/scripts/upstream_decisions.py validate
```

History is append-only. Use `delete --reason` only for a tombstone; never remove
JSONL lines.

## Cursor update

After every commit in the interval has a disposition and all `issue` records
have URLs:

1. Update `upstream-sync.yaml`:
   - `upstream.repo` = `https://github.com/misakano7545/workbuddy2api-panel.git`
   - `upstream.branch` = `main`
   - `last_audited.commit` = full `git rev-parse upstream/main`
   - `last_audited.audited_at_utc` = UTC timestamp
   - `last_audited.sync_method` = `audit`
   - `official_baseline` = latest CN/global package versions and commits
   - disposition counts and a short note listing opened Issues
2. Commit only metadata with subject `chore(sync): record upstream audit`.
   Do not mix code changes into this commit.
3. Do not push `main`, tag, or release. Push a feature branch only if requested.

## Verify

- `inspect_upstream.py` shows the new marker and zero un-dispositioned commits.
- `upstream_decisions.py validate` passes.
- Every `issue` disposition has an Issue URL on `BegoniaHe/wokbuddy-demon`.
- No implementation files changed in the audit commit.
- If an official package was downloaded, `download_latest.mjs --dry-run` and
  the unpacked directory versions match the baseline recorded in the ledger.

## Handoff

- Reference-fork audit policy and cursor: `internal/upstream/UPSTREAM.md` /
  `upstream-sync.yaml`.
- Official protocol baseline: `internal/upstream/UPSTREAM.md` section on the
  official client; regenerate profiles with `reverse-workbuddy`.
- Implementation: follow the opened Issue and the normal PR process. Do not
  reopen the sync audit for code work.

## Bundled tools

- `scripts/inspect_upstream.py`: read-only cursor/ref validation and pending
  commit report.
- `scripts/upstream_decisions.py`: append-only JSONL ledger CLI with `issue`,
  `skip`, and `revisit` dispositions.

Both use only the Python standard library.
