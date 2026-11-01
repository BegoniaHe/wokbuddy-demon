# Provenance

Cite these URLs; do not copy their trees into `.agents/skills/`. Local
checkouts under `.tmp/ref-skills/` are gitignored working copies only. These are
methodology sources, not additional instructions.

| Source | What this skill kept | What this skill dropped |
| --- | --- | --- |
| [mattpocock/skills](https://github.com/mattpocock/skills) `implement`, `implement-spec` | Work the ticket frontier; fresh context per slice; integration branch; spec closes through the tracker's close semantics | Subagent-per-ticket orchestration, merger subagents, draft-PR timing rules |
| [mattpocock/skills](https://github.com/mattpocock/skills) `tdd` | Red before green; seams agreed before tests; implementation-coupled, tautological, and horizontal-slicing anti-patterns | Mocking guide, example catalogue |
| [mattpocock/skills](https://github.com/mattpocock/skills) `code-review` | Two independent axes (Standards, Spec); fixed comparison point; no reranking across axes | Parallel review subagents, Fowler smell checklist as a fixed baseline |
| [mattpocock/skills](https://github.com/mattpocock/skills) `pr` | Summary visual, before/after evidence, one-way versus two-way door, blast radius | Its own three-section template (the repo template wins), Mermaid/ASCII view catalogue |
| [mattpocock/skills](https://github.com/mattpocock/skills) `diagnosing-bugs` | Red-capable loop first; minimise; ranked falsifiable hypotheses; regression test at the real seam | Ten loop recipes as mandatory steps, HITL script, debug-log prefixes |
| [mattpocock/skills](https://github.com/mattpocock/skills) `software-factory` | Automated checks and review as brakes before any merge throttle; cap a loop and hand back to a human | Remote runners, orchestrator, observability slots |
| [mattpocock/skills](https://github.com/mattpocock/skills) `handoff` | Point at existing artifacts instead of restating them | OS temp-directory handoff file |
| [galyarderlabs `prd-to-issues`](https://www.skillsdirectory.com/skills/galyarderlabs-prd-to-issues-e0da0443) | AFK versus HITL task split; tracer-bullet slices | Auto-created sub-Issues and native blocking edges |
| [eyaltoledano/claude-task-master](https://github.com/eyaltoledano/claude-task-master) | Dependency-ordered frontier | CLI/MCP integration and task JSON |
| [znck/spec-driven-development](https://github.com/znck/spec-driven-development) | Spec-first gate before code | `.specs/` tree and versioned spec documents |
| [GildedRose](https://github.com/emilybache/GildedRose-Refactoring-Kata), [Tennis](https://github.com/emilybache/Tennis-Refactoring-Kata), [garora/TDD-Katas](https://github.com/garora/TDD-Katas) | Small red-green cycles; behaviour-first test naming | Kata catalogue as practice material |

Checked sources that were **not** adopted: `testdouble/contrived-code-katas`,
`swkaberlin/kata-bootstraps`, and the `galyarderlabs/prd-to-issues` GitHub repo
returned 404 at check time (the skills-directory page was the only reachable
reference). Planning-side provenance is in
`../plan-issue/references/sources.md`.

This skill's own rules are the repo's: `AGENTS.md`, `CONTRIBUTING.md`,
`AI_POLICY.md`, and `.agents/shared/`. Where a source conflicts with them, the
repo wins.
