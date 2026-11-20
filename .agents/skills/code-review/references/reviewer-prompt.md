# Reviewer subagent prompts

Use these when the client exposes a subagent/delegate tool: Codex as an
explicitly dispatched read-only task, OpenCode through its subagent tool. Fresh
context per dispatch; the parent agent keeps the report and the verdict.
No subagent tool: run the same prompt as your own separate pass and record
`reviewer: self:refute` or `self:record`.

## Dispatch rules

- One finding (or one tight cluster on the same path) per refutation subagent.
  Do not send the whole report to be "reviewed"; the subagent must not rank or
  rewrite the parent's findings.
- Subagents are read-only: no edits, no commits, no `gh` writes, no branch
  changes. Tell them so in the prompt.
- Give the exact claim, `path:line`, range, and the commands the parent already
  ran. A subagent without the claim cannot refute it.
- Ask for a verdict token plus evidence, never prose only. A missing `VERDICT:`
  line is an invalid response; re-dispatch once with the same claim.
- A subagent that cannot observe the decisive fact returns `needs-validation`
  with the blocker, not a guess. Do not upgrade it later.

## Verification tiers

| Finding class | Passes |
| ------------- | ------ |
| Minor | one refutation |
| Important, Critical | refutation + record check by a verifier that did not find or refute it |
| Any `needs-validation` | one refutation; the blocker must survive, not the claim |

A Critical can skip the record check only when a reproducible test or command
demonstrates it; cite that command in the record's `evidence`.

## Refutation prompt

```text
You are verifying one code-review finding in the BegoniaHe/wokbuddy-demon
checkout. Your job is to try to break the finding, not to agree with it.

Read-only: do not edit files, change branches, commit, push, or post to GitHub.
Use git show/git diff/rg/go doc/go test and read the files named below. Do not
dispatch further subagents.

Finding under test:
- id: [F1]
- claim: [one sentence]
- file:line: [path:line]
- range: [base]...[head]
- trigger the parent assumed: [inputs/state]
- parent evidence: [symbols, guards, commands run]
- security module: [on|off]

Do this:
1. Restate the claim precisely. If it is incoherent, return rejected and say why.
2. Trace the path from the changed line to the claimed impact. Name every
   caller, guard, lock, and error branch in between, with path:line.
3. Look for the refutation: an upstream guard, a different code path, a config
   default, a test that contradicts the claim, or a behavior of the
   stdlib/dependency that makes the trigger impossible.
4. If the claim depends on runtime or deployment behavior you cannot observe
   here, return needs-validation with the exact blocker instead of guessing.
5. Check the repo rule the parent cited (AGENTS.md). A standards finding must
   quote the actual rule, not a general preference.
6. With the security module on, apply the six-part gate in
   references/security.md: principal, input, control, boundary, resource,
   result. A missing control without a crossed boundary is not a finding.

Return exactly:

VERDICT: confirmed | needs-validation | rejected
CONFIDENCE: high | medium | low
REASON: <two or three sentences; for rejected, why the claim fails>
EVIDENCE: <path:line and commands actually run>
TRIGGER: <precise inputs/state, or "not reachable">
BLOCKERS: <one per line, or "none">
VALIDATION: <the next check that would resolve it, or "none">
REMAINING_RISK: <what would still make this real, or "none">
```

## Record-check prompt

Run this only for an Important or Critical candidate that survived refutation,
or after fixes were pushed. It checks the record, not the hunter's prose.

```text
You are independently checking a final code-review record in
BegoniaHe/wokbuddy-demon. You did not write this finding and did not refute it.
Read-only; no edits, commits, gh writes, or further subagents.

Record under check:
[full JSON line from FINDINGS.jsonl]

Range: [base]...[head]. The finding claims [path:line], trigger [..], impact
[..], severity [..], fix [..].

Check:
1. Every cited source path and line exists in the reviewed revision and says
   what the record says it says.
2. The trigger is reachable with the stated preconditions; or, for
   needs-validation, the blocker is real and decisive.
3. The impact is demonstrated at the claimed severity and does not exceed the
   evidence.
4. The fix enforces the invariant at the last trusted decision point instead of
   moving trust or adding advice.
5. The fingerprint names the root cause, not a symptom or a duplicated wording.

Return exactly:

VERDICT: verified | replace
CONFIDENCE: high | medium | low
REASON: <two or three sentences>
EVIDENCE: <path:line and commands run>
REPLACEMENT: <one JSON object with corrected fields, or "none">
```

A material replacement (verdict, severity, root cause, trigger, impact) needs one
more fresh verifier before it is applied. A wording or line-number correction
can be applied directly. If independence is unavailable, keep the record but
state the limitation in the report and lower `confidence`.

## Role sweep prompts

Use for a diff over roughly 400 changed lines, or any diff touching
`internal/httpauth`, `internal/server`, `internal/panel`, `internal/upstream`,
`internal/pool`, or `web/`. Run the roles in parallel, each with the same range
and a fresh context. They find; refutation decides.

### Defect hunter

```text
You are reviewing a diff for real defects only. Read-only; no edits or gh
writes; no further subagents.

Range: [base]...[head] in BegoniaHe/wokbuddy-demon. Changed files: [list].
Read each full file and the callers of changed symbols.

Report every candidate defect with path:line, the trigger, and the impact:
- wrong logic, inverted or missing guards, off-by-one, unreachable branches;
- nil/empty/oversized input, missing error handling, swallowed errors;
- concurrency: counter decrement paths, goroutine lifetime, locking, races;
- resource leaks: bodies, tickers, timers, maps that only grow;
- panics reachable from user input or from an empty candidate set.

Do not report style, naming, missing tests, or pre-existing code outside the
diff. Do not cap the list. If you find nothing, say so and name the files you
read. Format per finding:

FINDING: <path:line> | <severity guess critical|important|minor> | <one line>
TRIGGER: <...>
IMPACT: <...>
EVIDENCE: <symbols/commands>
```

### Security boundaries

```text
You are reviewing a diff for security regressions in BegoniaHe/wokbuddy-demon.
Read-only; no edits, gh writes, or further subagents.

Range: [base]...[head]. Read AGENTS.md (Security invariants),
.agents/skills/code-review/references/security.md, and
references/standards.md, then inspect the diff.

For each entry surface the diff touches, name the lower-trust principal, the
intended control, the boundary, and a concrete result. Check:
- credential comparison bypassing internal/httpauth, or a second comparison
  implementation in panel/server;
- api_key empty-case, listen address, WB2A_LISTEN, CORS, WAF/IP, trusted
  proxies, passthrough_ip changes;
- account or realm crossover: one request using another account's credential,
  session, or response;
- credential/token/URL leakage into logs, errors, panel JSON, reqlog, reports;
- sanitize_blacklist_fingerprints bypassed or defaulted off;
- state/config/auth writes that are not atomic or that widen permissions;
- new outbound calls without timeout, context, redirect, or host control;
- path traversal and unbounded body or SSE input.

Report only regressions introduced by the diff. A finding without a crossed
boundary is a hardening note, not a finding. Quote the AGENTS.md rule applied.
Never probe a live endpoint or a real account.
```

### Test and behavior

```text
You are reviewing a diff for behavior and test gaps in
BegoniaHe/wokbuddy-demon. Read-only; no edits, gh writes, or further subagents.

Range: [base]...[head]. Spec: [PR body / Issue text / "none"].

Report:
- acceptance points from the spec that are missing or only partly implemented;
- behavior in the diff that the spec did not ask for (scope creep);
- user-visible changes (defaults, env vars, endpoints, panel copy, restart
  requirements) that are undocumented;
- tests that assert implementation details, mocks over real behavior, or that
  miss the seam the change actually moves;
- config keys added without config.example.json / README / restart
  classification.

With no spec, skip the acceptance check and say "no spec available"; still
report scope creep and test gaps. One finding per item, path:line, no padding.
```

## Merging results

- `confirmed` from refutation becomes the record's verdict; `needs-validation`
  keeps its blockers and validation step; `rejected` moves to the appendix with
  its reason. Nothing is deleted.
- A role-sweep candidate is still only a candidate: it needs its own refutation
  before it is published.
- Disagreement between the parent and a subagent is resolved by evidence, not
  by seniority. Re-run the refutation once with the conflicting evidence; if it
  stays split, publish `needs-validation` with both readings named.
- Merge by fingerprint, not by wording. Two records with the same root cause
  become one record with the stronger evidence and the lower severity that the
  evidence supports.
