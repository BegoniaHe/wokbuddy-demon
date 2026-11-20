# Repo review checklist

Concrete checks for this checkout, keyed to `AGENTS.md`. Cite the rule; do not
copy it. Each item is a question about the diff, with the place that answers it.
Skip a check only when the change cannot touch it, and say so in coverage.

With the security module enabled, run `references/security.md` as well: this
file is the repo overlay, that file is the trust-boundary method and severity
gate.

## Security invariants

- **Auth path**: every credential comparison goes through
  `internal/httpauth` (`Authenticate`, `AuthenticateManagement`, `HashKey`).
  A new `==`, `bytes.Equal`, or `strings.EqualFold` on a key/token is a
  Critical finding. Both `internal/server` and `internal/panel` must use the
  same primitive — a panel-local copy is the historic bug this package removed.
- **Auth posture**: an empty `api_key` means no auth. Any change to the empty
  case, the default `listen` (`127.0.0.1:7863`), `WB2A_LISTEN`, CORS, WAF/IP
  handling, or `trusted_proxies` is a behavior change: state it and the new
  exposure.
- **Redaction**: grep the new error/log/response paths for provider hosts,
  upstream URLs, `Authorization`, tokens, and request bodies. User-facing
  errors stay generic; `internal/reqlog` stores metadata only; `internal/logfmt`
  carries the formatting. Panel responses must not echo credentials or
  `auths/` contents.
- **Sanitize toggle**: `features.sanitize_blacklist_fingerprints` stays default
  on and no path bypasses it (`internal/upstream/sanitize.go`). A "temporary"
  bypass is a blocking finding.
- **Passthrough**: `upstream.passthrough_ip` stays default false; if a diff
  turns it on or forwards untrusted `X-Forwarded-For`, check
  `internal/server/handler.go` trust-boundary code.

## Config and wiring

- A new setting lands in all of: the `Config` struct, `Default()`,
  `normalize*` in `cmd/server/config.go`, `config.example.json`, the panel save
  path, and `README.md` when user-visible. Missing `config.example.json` or a
  silent default is at least Important.
- Hot-versus-restart: add the field to `restartRequiredFields`
  (`cmd/server/main.go`) unless it is applied through `internal/livecfg` at
  runtime. Fields captured at assembly (`HTTP client`, `auth_dir`, `store`,
  session TTL, `upstream.*`, `upstash.*`) need a restart entry and a test
  beside `config_restart_test.go`.
- No compatibility shim for a removed config key, auth layout, or API shape.
  Dual-path parsing is a finding, not a courtesy.

## Upstream and protocol

- Adaptations stay in `internal/upstream`; `internal/server` should not grow
  protocol literals. Check realm routing (`cn`/`global`), model catalog and
  multiplier tables, and `model.json` fallback behavior when touched.
- `internal/clientprofile` reads `data/client-profiles/<realm>.json` and refuses
  to start without it. A hardcoded version, commit, or `releaseDate` literal is
  a finding.
- SSE streaming and tool-call pairing: check that new branches still close the
  stream, flush terminators, and keep `[DONE]`/usage semantics. A missing close
  is a goroutine and connection leak.
- Outbound requests: context propagation, timeouts, retry/backoff classification
  (do not retry 4xx), and error redaction.

## Pool and scheduler

- Filtering steps that change the candidate set must recompute weights; a
  filtered-empty set needs an explicit fallback, not an index panic.
- In-flight counters and breaker state must decrement on every return path
  (including context cancellation and panic recovery). A missing decrement
  eventually exhausts the pool.
- Cooldown math: exponential backoff bounds, idle compensation, points floor,
  cross-domain fallback, and session stickiness TTL must keep their existing
  semantics unless the change is explicitly about them.
- Persistence: state writes go through `internal/safefile` (tmp + rename) and
  Redis snapshots (`internal/redisstore`) must not be able to overwrite newer
  in-memory state with stale data.
- Scheduler tasks: time-window checks, order, timezone, and once-per-day
  idempotency stay intact. `time.Sleep` in a loop, unbounded goroutines, or a
  ticker that is never stopped is a finding.

## Go code

- New JSON calls use `internal/jsonx`; direct `encoding/json` in new code is a
  finding. Map key order, nil-container output, and raw number handling follow
  that package.
- Errors carry Chinese context (`发生了什么、为什么`) and are not swallowed.
  `_ = err`, empty `if err != nil {}`, and provider-detail leaks are findings.
- Goroutines: every spawn has an exit condition and is awaited or cancelled;
  `defer resp.Body.Close()` inside a loop is a leak; use `time.NewTimer` +
  `Stop` over `time.After` in hot loops.
- Cross-platform: `path/filepath`, not string concatenation; no unix-only
  syscalls or shell-outs in server code; Windows rename semantics via
  `internal/atomicwrite`; arm64-safe sizes and alignment assumptions.
- Tests: table-driven at a public seam, deterministic (no arbitrary sleeps,
  no network), race-safe (`make test-race` for concurrency changes), and
  asserting behavior rather than private call order. A bug fix needs a test
  that fails before the fix.

## Front end (`web/`)

- Source changes rebuild `internal/panel/dist` via `make web`; hand-edited
  `dist/` or a source/bundle mismatch fails CI (`web.yml`) and is Critical.
- `make web-check` covers typecheck and vitest. New component patterns follow
  the existing shadcn + Base UI + Tailwind 4 conventions; no new dependency for
  something the project already has.
- Error, empty, and loading states exist for new data paths; no secrets or
  internal URLs in client code or bundles.

## Docs, comments, and hygiene

- User-visible behavior updates `README.md` (Chinese); provenance updates
  `internal/upstream/UPSTREAM.md`.
- Comments are Chinese, explain why, and do not narrate the next line. No
  `TODO`, placeholder, or dead flag left behind.
- Commit shape (when reviewing a PR): Conventional Commit header, English
  type/scope, Chinese description, AI footers only for agent-finalized commits,
  issue number in the footer.
- No credentials, `data/`, `config.json`, or generated logs in the diff.

## Calibration in this repo

- CI-failing conditions (fmt/lint, unbuilt `dist`, failing tests) are Critical
  because they block merge, even when the code "works locally".
- A behavior change with no docs is Important, not Minor.
- Style that `gofumpt`/`gci`/`golangci-lint` already decides is not a finding;
  a deliberate dodge of the tooling is.
