# Methodology provenance

This skill is written for this checkout; it vendors no upstream text. The
structure follows the sources below, each MIT-licensed, read at authoring time.

| Source                     | What it contributed                                                        |
| -------------------------- | -------------------------------------------------------------------------- |
| `cloudflare/security-audit-skill` | Per-file coverage ledger with explicit statuses; `confirmed` / `needs-validation` / `rejected` verdict contracts; fingerprint dedup; finder and verifier must be different agents; severity only on confirmed certainty; clean run with zero findings is valid. |
| `obra/superpowers`         | Reviewer prompt shape: verdict, severity grouping, `file:line`, calibration; refutation before acceptance; PR thread replies. |
| `mattpocock/skills`        | Standards-versus-spec separation; pinning a fixed point and using a three-dot diff; parallel fresh-context subagents. |
| `trailofbits/skills`       | Risk-first triage, blast radius and coverage honesty (`differential-review`); claim restatement and TRUE/FALSE/UNVERIFIED gates (`fp-check`); no-token-cap full coverage. |
| `addyosmani/agent-skills`  | Five-axis review, severity prefixes that separate required from optional, review-tests-first, and the "zero findings is valid" rule. |
| `allemaar/open-skills`     | `cold-review`: fresh-context reviewers, re-open evidence at synthesis instead of recalling it, label a degraded same-session review honestly. |
| `agentkitco/agentkit-dev-skills` | `review-my-diff` and `security-quick-review`: ranked self-review, fast triage for changes touching input/auth/files/money. |
| `anthropics/skills`        | Public `SKILL.md` frontmatter and progressive-disclosure convention (its template is a stub; the shape is confirmed against this repo's existing skills). |

Deliberately not adopted from `cloudflare/security-audit-skill`: the OS-level
sandbox, agent scratch/artifact write isolation and promotion procedure, the
budget accounting model, and the security-only scope. This skill reviews a local
checkout without executing target-controlled code, so those controls would be
ceremony here. The trust-boundary method is kept, reduced to
`references/security.md`.

Repo-specific material (severity calibration, `path:line` rules, PR comment
mechanics, security checklist) comes from `AGENTS.md`, `.agents/shared/`, and
the current code, not from the sources above.

Do not copy an upstream skill wholesale into this tree. If a new source is worth
following, add a row here with what it changed.
