# Audit reference-fork commits and open issues

Use the sync-upstream skill in audit mode. In OpenCode, load `sync-upstream`
with the skill tool; in Codex CLI, invoke it as `$sync-upstream`.

This workflow **does not absorb code**. It audits `misakano7545/workbuddy2api-panel`
against the latest official WorkBuddy Win32 client, records dispositions, and
opens Issues on `BegoniaHe/wokbuddy-demon`.

## Inputs and authorization

Find the audited interval and any already-approved Issue list in the current
conversation. If the interval is unspecified, use the cursor in
`upstream-sync.yaml` to `upstream/main`.

Read these sources as needed; cite them instead of pasting them:

- `internal/upstream/UPSTREAM.md`: lineage, audit baseline, official-client
  baseline.
- `.agents/skills/sync-upstream/SKILL.md`: audit gates and ledger rules.
- `.agents/skills/reverse-workbuddy/SKILL.md`: official package download,
  protocol sweep, evidence rules.
- `upstream-sync.yaml`: logical audit cursor.
- `upstream-decisions.jsonl`: durable decisions and prior Issue links.

## Prepare

Inspect `git status --short --branch` and `git remote -v`. Preserve unrelated
work. Verify `upstream` points to
`https://github.com/misakano7545/workbuddy2api-panel.git`; add or correct the
remote if needed.

```bash
git fetch --prune upstream
node .agents/skills/reverse-workbuddy/scripts/download_latest.mjs --dry-run
node .agents/skills/reverse-workbuddy/scripts/protocol_sweep.mjs --root .tmp/workbuddy
python3 .agents/skills/sync-upstream/scripts/inspect_upstream.py
```

If the latest official package is missing, download it before judging commits.
Do not audit against memory or the reference fork's assertions.

## Audit and file Issues

Process commits oldest-first. For each commit:

1. Read the patch and the current fork implementation.
2. Check the official client (`cli/product.json`, `cli/dist/*.mjs`,
   `main/*.js`, renderer assets).
3. Choose `issue`, `skip`, or `revisit`.
4. For `issue`, search existing Issues, then open one on
   `BegoniaHe/wokbuddy-demon` with official evidence, `path:line` fork
   evidence, impact, fix direction, and acceptance criteria.
5. Record the disposition with `--official-baseline` and `--issue`.
6. Do not edit implementation files. Do not cherry-pick or adapt.

## Update the cursor

After all commits have a disposition and every `issue` row has a URL:

- Update `upstream-sync.yaml` (`last_audited`, `official_baseline`,
  disposition counts, Issue links).
- Commit only metadata: `chore(sync): record upstream audit`.
- Do not merge, push `main`, tag, or release.

## Deliver

Return the audited interval, disposition mapping, Issue URLs, ledger/cursor
status, official package versions used, and any `revisit` items that need a
live probe. If nothing was confirmed, say so and record `skip`/`revisit`;
do not invent an Issue.
