# Simplifier checkout facts

Cite `AGENTS.md` for policy. This file maps the ladder onto this fork and
records what was kept from ponytail.

## Provenance

Adapted from [DietrichGebert/ponytail](https://github.com/DietrichGebert/ponytail)
at the commit in `VENDOR.json` (MIT). Keep the ladder and the "never skip
safety" rule. Do not vendor the upstream plugin, hooks, commands, or skill
tree.

| Kept                                       | Dropped                                                          |
| ------------------------------------------ | ---------------------------------------------------------------- |
| Seven-rung ladder after reading the code   | `lite` / `full` / `ultra` levels                                 |
| Root-cause bug fix via shared callers      | Always-on injection and `/ponytail` commands                     |
| Skip speculative work; name when to add it | `ponytail-review`, `-audit`, `-debt`, `-gain`, `-help`           |
| No unrequested abstractions or new deps    | `ponytail:` code comments (this repo forbids unasked comments)   |
| Review delete-list as a mode of this skill | Hardware calibration notes; caveman pairing; npm/OpenCode plugin |

Karpathy-style KISS / inline-first rules already live in `AGENTS.md`. This
skill adds an explicit stop-at-first-rung pass and a skip report. It must
not restate the rest of `AGENTS.md`.

## Checkout rungs

| Rung      | Look here first                                                                                        |
| --------- | ------------------------------------------------------------------------------------------------------ |
| Reuse     | Neighboring packages under `internal/`, existing helpers in the same package, `cmd/server` wiring      |
| Stdlib    | Go 1.27, `path/filepath`, `context`, `net/http`, `encoding/json`, `slices`, `maps`, `sync/atomic`       |
| Native    | Standard HTTP server, `//go:embed` panel assets, atomic tmp+rename writes, existing `internal/*` types  |
| Installed | `go.mod` + `go.sum` (the only dependency source); never add a module for a few lines                    |

Dependency changes go through `go.mod` + `go.sum` together; run `make tidy`.
Do not add a module without a stated current need.

Deprecated or compatibility paths are not reuse. Build on the current path
and remove the old one.

## Not optional

Do not treat these as over-engineering:

- Constant-time gateway/panel auth (`internal/httpauth`), never a plain string
  comparison
- Outbound sanitize (`internal/upstream/sanitize.go`) and credential redaction
  in logs, panel responses, and request archives
- `config.example.json` when a config field is added or changed, and
  `README.md` when user-visible behavior changes
- Import boundaries between `internal/*` packages; `cmd/*` assembles and does
  not get reverse-depended on
- Context cancellation and graceful shutdown ordering (`p.Flush()` before
  `srv.Shutdown`)

## Tests

Non-trivial logic gets one focused Go test beside the nearest existing coverage
(`internal/<pkg>/..._test.go`). Trivial one-liners need no new test. Do not add
`example` self-checks or a new framework.
