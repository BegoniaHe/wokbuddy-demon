<!--
Title: chore(scope): ... | build(scope): ... | ops(scope): ... | test(scope): ...
Do not use `ci`; CI/CD and deployment changes use `ops`.
Product behavior changes belong in feat.md or fix.md.
-->

## Summary

<!-- What maintenance this completes, and what done looks like. -->

## Related issue

<!-- Maintenance task Issue when one exists. `Fixes #123` or `Related: #123`. -->

Related: #

## Surfaces

<!-- Toolchain, go.mod/go.sum, workflows, images, or tests. Name every file that must move together. -->

## Implementation notes

<!-- Why this path, not a broader cleanup. Note skipped surfaces. -->

## Validation

<!-- Exact commands run. Do not claim a check passed unless you ran it or CI did. -->

```text
make fmt-check
make lint
make test
make build-all
```

## Compatibility and risk

<!-- Toolchain pins, CI, Docker, cross-platform builds, security. Write "None" if not applicable. -->

## Checklist

- [ ] Conventional Commit type is `chore`, `build`, `ops`, or `test` (not `ci`).
- [ ] The change is focused and does not include unrelated product refactors.
- [ ] A toolchain upgrade updates every matching declaration, workflow, image build, and lockfile (`go.mod`/`go.sum`).
- [ ] I ran the relevant formatting, lint, build, and test commands.
- [ ] No secrets committed (`auths/`, `data/`, `config.json` stay local).
- [ ] I did not restore legacy shims or upstream publish/docs URLs as fork artifacts.
- [ ] I will not merge this PR myself. Merge needs a human maintainer review plus a separate AI-assisted review ([AI_POLICY.md](../../AI_POLICY.md)).
- [ ] AI use follows [AI_POLICY.md](../../AI_POLICY.md). Keep exactly one author note below. Do not fabricate the other.

## Human note

<!-- Humans only. Mother tongue, own words: intent, why this approach, what you verified. Delete this section if an agent is the author. -->

## Agent note

<!-- Agents only. Goal, paths touched, checks run, residual risk, tools used. Delete this section if a human is the author. -->
