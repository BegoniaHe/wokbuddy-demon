# Security module (optional)

A trust-boundary pass layered on the chosen review range. It adds enumeration
and severity anchors; it does not replace the defect axis. Enable it per
`SKILL.md` when the request names security, or when the diff touches auth,
credentials, outbound requests, panel endpoints, config secrets, or filesystem
paths.

## Gate for a security finding

A candidate is not a security finding until it names all six:

1. the lower-trust principal (anonymous client, data-plane key, readonly panel
   key, another account, a local process);
2. the input or action that principal controls;
3. the intended control (auth check, scope check, realm/account isolation,
   sanitizer, path validation, size limit);
4. the boundary that control guards;
5. the affected principal or resource;
6. a concrete observed or owner-observable result.

Missing practice, a guessed deployment fact, a generic parser crash, or
self-impact against the same principal is not a finding. Missing a deployment
fact is `needs-validation` with that exact fact as the blocker.

## This gateway's boundaries

| Boundary | Lower-trust side | Control it must reach |
| -------- | ---------------- | --------------------- |
| Client → gateway `/v1/*`, `/status` | API caller | `httpauth.Authenticate`, API key scope, WAF/IP, body limits |
| Admin → panel `/panel/api/*` | Panel caller | `httpauth.AuthenticateManagement`, admin vs readonly scope |
| Gateway → upstream | This process | realm routing, account selection, header/body construction, sanitize |
| Scheduler → upstream | Timer-triggered task | same outbound path, no credential crossover |
| Disk → credentials | Anything that can read the repo dir | `auths/*.json` permissions, no echoing into logs/panel |
| Disk → state and cache | Concurrent process | `internal/safefile` atomic replace, `data/` not served |
| Embedded panel → browser | Page consumer | no secrets in bundle, no internal URL leakage |

## Entry surfaces to enumerate

Only the ones the diff touches, but name them explicitly in `CONTEXT.md`:

- data-plane routes: `POST /v1/chat/completions`, `/v1/responses`,
  `/v1/messages`, `/v1/search/messages`, `/v1/images/edits`, `/v1/videos/*`,
  `/v1/tools/fetch`, `/v1/tools/search`, `GET /v1/models`, SSE streaming,
  `/status`, `/healthz`;
- panel API: login/auth, config save, account add/remove, usage, request logs,
  task center;
- CLI: `cmd/login`, `cmd/signin`, `cmd/credit`, `cmd/trial`;
- config: `config.json`, `WB2A_LISTEN`, env-derived options, panel save path;
- auth files: nested and flat layouts, refresh write-back, realm (`cn`/`global`);
- model catalog: `internal/upstream/model.json`, models.dev fetch;
- outbound request construction: headers, body, URL, retry, redirects;
- filesystem: state file, logs, auth dir, static dist.

## Checks per surface

1. **Authentication** — is the credential checked on every new path? Does it go
   through `internal/httpauth` only? Empty `api_key` means open access: does the
   diff change that posture, the default `listen`, or `WB2A_LISTEN`?
2. **Authorization** — data-plane keys must not reach management actions;
   readonly must not write; `ScopeAdmin` checks must precede panel mutations.
3. **Account and realm isolation** — one request's account must not leak into
   another's; `cn`/`global` credentials and sessions must not cross; pool
   selection cannot use a cooldown/disabled account to route around limits.
4. **Input** — body size and JSON depth, model name and alias resolution, header
   spoofing (`X-Forwarded-For`, `Authorization`, `X-Api-Key`), SSE framing, tool
   arguments, URL and file paths built from user input.
5. **Outbound trust** — timeouts and context, redirect policy, TLS verification,
   `upstream.passthrough_ip` default false, trusted-proxy handling, no
   user-controlled host.
6. **Secrets** — logs, errors, panel JSON, `reqlog` metadata, usage records, PR
   text; sanitize toggle not bypassed; no `auths/` content echoed.
7. **Filesystem** — writes via `internal/safefile`/`internal/atomicwrite`,
   permissions, path traversal in static or state paths, symlink following.
8. **Availability** — in-flight caps, rate limits, body limits, goroutine and
   timer lifetime, unbounded maps or caches.

## Local evidence only

This is a local self-hosted gateway. Never send test traffic to a real upstream,
a live account, or a deployed panel. Use `httptest`, unit tests, fixtures, and
dummy keys. If the decisive fact needs a real deployment (reverse proxy, TLS
terminator, firewall, identity provider), record `needs-validation` with the
exact fact and a safe owner check. Do not reproduce a denial of service against
a shared process.

## Severity anchors

Only `confirmed` findings get severity, and it cannot exceed the demonstrated
result.

- **Critical** — unauthenticated bypass of the gateway or panel; cross-account
  or cross-realm credential/response exposure; secret exfiltration; remote code
  execution; state corruption that loses or mixes accounts.
- **High** — an authenticated principal fully defeats an explicit control with
  real consequences (readonly → admin, data key → management action, one
  account's credentials used for another's request).
- **Medium** — a real boundary violation with limited blast radius or uncommon
  preconditions (requires a specific config, a local attacker, a narrow window).
- **Low** — disclosure of non-secret internals, or an effect needing sustained
  effort for minimal gain.

If you cannot state the concrete damage, the severity is lower than it feels.

## Output

Add a boundary table to `REPORT.md` when the module is enabled:

```markdown
## 安全边界
| 入口面 | 低信任方 | 应有控制 | 结论 |
| ------ | -------- | -------- | ---- |
| `POST /v1/chat/completions` | 数据面 key | httpauth.Authenticate | ok |
| `POST /panel/api/config` | readonly key | ScopeAdmin | F3 confirmed |
```

Confirmed security findings use the normal record; `axis` is `security`.
`needs-validation` leads state the missing deployment fact, never a severity.
Rejected candidates stay in the appendix with the refuting control.

## Anti-patterns

- "Header missing" or "input not validated" without a sink and a boundary.
- Defense-in-depth advice when an earlier layer already blocks the path.
- Guessing proxy, TLS, firewall, or identity behavior that is not in the repo.
- Treating same-principal self-impact as a boundary crossing.
- Reporting a parser or runtime effect stronger than the observed result.
- Live probing or real-account testing to turn a lead into a finding.
- Severity on a lead, or a security label on a style preference.
