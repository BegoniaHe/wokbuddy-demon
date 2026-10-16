# Issue-plan overlay

This file is the checkout-specific fact sheet for `plan-issue`. Policy
stays in `SKILL.md`. Cite `AGENTS.md` and `references/sources.md`; do not
paste them.

## Product identity

| Field               | Value                                                             |
| ------------------- | ----------------------------------------------------------------- |
| Product             | WokBuddy Demon (BegoniaHe fork)                                   |
| Repository          | `BegoniaHe/wokbuddy-demon`                                        |
| Upstream provenance | `linguo2625469/workbuddy2api-panel` (sync source only)            |
| Issue templates     | `.github/ISSUE_TEMPLATE/{bug_report,feature_request,task}.yml`    |
| PR templates        | `.github/PULL_REQUEST_TEMPLATE/{feat,fix,docs,refactor,chore}.md` |
| Published artifacts | nightly binaries + GHCR image (`ghcr.io/begoniahe/wokbuddy-demon`) |
| Docs                | `README.md` (Chinese)                                             |
| Go floor            | `1.27` in `go.mod`                                                |

`gh` must pass `--repo BegoniaHe/wokbuddy-demon`. Confirm created URLs are
under `github.com/BegoniaHe/wokbuddy-demon`.

## Issue kinds

| Template / label | Plan type                          | Extra research                           |
| ---------------- | ---------------------------------- | ---------------------------------------- |
| `bug`            | Fix plan                           | Reproduce or trace before designing      |
| `enhancement`    | Feature plan                       | Development Issue must exist before PR   |
| `task`           | Maintenance plan                   | Name the files or commands that prove it |
| pasted request   | Same as above after classification | Note that a feature still needs an Issue |

Fill exactly one of Human note or Agent note on any Issue this skill
opens. Agents must not fabricate a Human note.

## Workspace

Created by `scripts/issue_plan.py init` at `.tmp/issue-plan/<run-id>/`:

```text
.tmp/issue-plan/<run-id>/
  manifest.json    # SHA, branch, issue, slug, probe, start time
  ISSUE.md         # fetched or pasted request
  RESEARCH.md      # Chinese report: ledger, search log, path:line, hypotheses
  BRIEF.md         # problem framing
  QUIZ.md          # five questions, scores, verdict — or skipped stub
  REFLECT.md       # JTBD, why-chain, surgical vs better path
  QUESTIONS.md     # decisions, coverage summary, and direct/guided mode reason
  PLAN.md          # executor plan
```

`<run-id>` is `issue-<number>` or `local-<slug>`. `.tmp/` is gitignored.
Do not relocate the run into `docs/` or `data/`.

`probe` is `required` or `skipped`. The CLI initializes `required` unless
passed `--skip-probe`; the skill uses that flag for its default direct
planning workflow. Omit it for a requested guided interview. `skip-probe`
switches an existing run to direct planning. Record the actual reason in
`QUESTIONS.md`; an explicit user waiver is not required.

If `status` shows an in-progress run at the same SHA and same issue or
slug, resume it. If the SHA drifted, start a new run and say so. Do not
overwrite a different incomplete plan without `--force`.

## Fetch commands

```bash
gh issue view <n> --repo BegoniaHe/wokbuddy-demon \
  --json number,title,body,labels,comments,url,state,author,createdAt
gh issue list --repo BegoniaHe/wokbuddy-demon --state all --search "<query>" --limit 20
gh search issues --repo BegoniaHe/wokbuddy-demon "<query>"
```

Refuse a URL under `github.com/linguo2625469/workbuddy2api-panel` unless the
user confirmed that upstream target. Do not post triage comments unless asked.

## Verify command map

Prescribe commands that exist in this checkout. Do not invent CI umbrellas.

| Surface                    | Focused command                                                                    |
| -------------------------- | ---------------------------------------------------------------------------------- |
| One Go behavior            | `go test ./internal/<pkg>/ -run TestName`                                          |
| Whole package              | `go test ./internal/<pkg>/`                                                        |
| Full suite                 | `make test`                                                                        |
| Fast suite                 | `make test-short`                                                                  |
| Format / lint / build      | `make fmt-check`, `make lint`, `make build` (or `make ci`)                         |
| Run and health             | `make run`, `make status`, `curl -s http://127.0.0.1:7863/healthz`                 |
| Config reference sync      | new fields present in `config.example.json`                                        |
| Panel assets               | `go test ./internal/panel/` (frontend smoke)                                       |

`make ci` runs `fmt-check + lint + test`; name the focused test first and add
the gate only when the slice touches format/lint surfaces.

## Owner map

Assign the plan to the runtime owner, not the importer. Primary owners:

| Area                          | Package                                   |
| ----------------------------- | ----------------------------------------- |
| Gateway HTTP / model resolve  | `internal/server`                         |
| Account pool / scheduling     | `internal/pool`                           |
| Cron tasks                    | `internal/scheduler`                      |
| Upstream protocol / SSE       | `internal/upstream`                       |
| Panel (web UI)                | `internal/panel`                          |
| Auth / credentials / realm    | `internal/auth`, `internal/httpauth`      |
| Config load / hot-apply       | `cmd/server` (`config.go`, `wiring.go`)   |
| Session stickiness            | `internal/session`                        |
| Usage / request log           | `internal/usage`, `internal/reqlog`       |

Sensitive zones (gateway `api_key` / `httpauth`, panel auth, credential read and
atomic write-back, outbound sanitize, config write-back) need an explicit
failure-path task.

## Routing

| Request                                       | Skill            |
| --------------------------------------------- | ---------------- |
| Absorb `linguo2625469/workbuddy2api-panel`    | `sync-upstream`  |
| Change this checkout from an Issue or request | `plan-issue` (this skill) |
