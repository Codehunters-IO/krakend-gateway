# Session resolution: cookie-backed edge authentication

`session-resolver` is a KrakenD Go plugin that lets a browser front-end hold an
opaque, `HttpOnly` session cookie instead of a JWT. It reads the session from
Valkey and turns it into the `Authorization: Bearer` header that `jwt-headers`
and every backend already expect. Non-browser clients (MCP, CI, mobile,
Postman) keep sending a bearer token directly and are never touched by this
plugin — both mechanisms exist on the same gateway at the same time.

The companion service, `auth-bff` (a separate repository), owns the OIDC flow
with Keycloak and is the **only writer** of the session data this plugin
reads. This document covers the gateway side only. Design rationale and the
`auth-bff` internals live in
[`docs/superpowers/specs/2026-08-31-token-handler-bff-design.md`](superpowers/specs/2026-08-31-token-handler-bff-design.md).

## Plugin chain order

```
gateway-timeout → accept-language → trace-context → ip-resolver → session-resolver → jwt-headers
```

This is the order requests are actually processed in, left to right.
`session-resolver` runs immediately before `jwt-headers` and after
`ip-resolver`, so that:

- by the time `session-resolver` runs, IP resolution and tracing have already
  attached whatever context they add to the request;
- `session-resolver`'s only effect — setting `Authorization` from a resolved
  session, or leaving a caller-supplied bearer untouched — happens **before**
  `jwt-headers` validates it. `jwt-headers` is otherwise unchanged: it always
  validates against JWKS and maps claims to headers exactly as it did before
  this plugin existed.

**Why this needs saying explicitly:** KrakenD's `plugin/http-server.name`
array does not execute in the order it is written. Each entry wraps the
ones declared *before* it, so for `"name": ["A", "B", "C"]` the composition
is `C(B(A(router)))` — **the *last* entry in the array is the one that
executes *first***, closest to the client; the *first* entry ends up
innermost, closest to the router/backend.

This is stated here on the strength of empirical evidence from this repo,
not a documentation citation — an earlier draft of this paragraph cited
[KrakenD's execution flow docs](https://www.krakend.io/docs/design/execution-flow/)
for this exact claim while quoting the opposite formula (`A(B(C(...)))`,
which would make the *first* entry run first), a contradiction that was
only caught in review. That page does not currently spell out the
plugin-array wrap order explicitly enough to quote here, so treat the
formula above as established by the test below, not by the link. The link
is left as background on KrakenD's general plugin model, not as proof of
this specific ordering.

**The test:** `config/krakend.tmpl` originally declared this array in
*chain order* (`session-resolver` immediately before `jwt-headers`, matching
the diagram above). With that declaration, every cookie-only request against
a real running gateway was rejected by `jwt-headers` with
`{"message":"missing or invalid authorization header"}` — including requests
carrying a cookie that pointed at a real, valid, freshly-seeded Valkey
session — and `session-resolver` never logged anything at all, not even its
own denial for a deliberately malformed session id, which it always logs
when it actually runs. That is only possible if `jwt-headers` ran *before*
`session-resolver` on every request. Declaring the array in the *reverse* of
chain order (`jwt-headers` first, `gateway-timeout` last, the order
currently in the template) fixed it: the same seeded session then produced
`jwt-headers`' `"invalid token"` log line — proof a bearer token *was*
present and reached it — instead of the "missing" response. Full commands
and log lines are in the seeded-session verification recorded in this
plan's Task 9 report.

The failure mode throughout is silent: the gateway starts either way, every
plugin logs `"plugin loaded"`, and `krakend check` says `Syntax OK!`
regardless of which order the array is in. Nothing short of driving traffic
and reading the resulting logs distinguishes a correct chain from an
inverted one — see **Guarding this mechanically** below for the one
assertion that now also catches it before a request ever has to. If you
touch this array, keep the comment above it in the template intact, and
re-run the seeded-session check after any change.

### What actually changed for the other five plugins

Reversing the array to fix `session-resolver` vs. `jwt-headers` does not
just affect those two — it reverses the request-time order of *all six*
plugins relative to how they had been running before this plan. Before this
fix, the declared array (`gateway-timeout, accept-language, trace-context,
ip-resolver, session-resolver, jwt-headers`) executed in exactly that order
reversed at request time too, by the same wrap rule: `jwt-headers` ran
*first* (outermost) and `gateway-timeout` ran *last* (innermost, right next
to the router). After the fix, execution order matches the diagram at the
top of this document: `gateway-timeout` now runs first (outermost, wrapping
the entire request/response cycle including the backend call — which is
what lets it convert a `5xx` after the fact into a `504` on elapsed time),
then `accept-language`, `trace-context`, `ip-resolver`, `session-resolver`,
and finally `jwt-headers` closest to the backend.

This is a genuine behavior change for traffic that has nothing to do with
sessions, and it ships in the production `Dockerfile`'s pre-baked config too
— not only in the `docker compose` / Flexible-Config dev path. Each of the
five bystanders benefits from or is neutral to the new order:

- **`gateway-timeout`** moves from innermost to outermost. It now wraps the
  whole chain rather than just the last hop before the router, which is
  what a timeout-conversion plugin needs to measure total elapsed time
  correctly — this was arguably broken before.
- **`accept-language`** and **`trace-context`** now run earlier relative to
  the auth plugins. `trace-context` running ahead of every deny path (it
  did not before) means a request rejected by `jwt-headers` or
  `session-resolver` now carries a trace id in the gateway's own logs
  instead of only trace-less rejections — previously-untraceable 401s
  become traceable.
- **`ip-resolver`** still runs, and now runs earlier, before both auth
  plugins that consume its output (`jwt-headers`' `x-ip` header and any
  future consumer). Its anti-spoof header strip stays ahead of every
  consumer either way, so this is neutral-to-beneficial, not a risk.
- **`jwt-headers`** moves from first-executed to last-executed. Its own
  behavior per-request (JWKS validation, claims-to-headers mapping) is
  unchanged; only its position relative to the others moved, to the
  position the original design always intended for it.

If a deploy right after this change shows unexpected trace ids on
previously trace-less rejections, or a shifted timeout/504 pattern, this
reordering is the reason — not a regression in any individual plugin.

### Guarding this mechanically

Prose alone already failed once — the comment above existed in an earlier
form and the ordering still broke unnoticed until this task drove real
traffic. `make check` (and therefore `make gen-check` / CI) now asserts the
declared order mechanically, not just the JSON syntax: because the LAST
declared entry executes FIRST, `krakend-jwt-headers` must be the *first*
entry in the rendered `plugin/http-server.name` array whenever both it and
`krakend-session-resolver` are enabled — so that it executes *last* — and
`krakend-session-resolver` must be declared immediately after it, so it
executes immediately *before* it. See
`scripts/check-plugin-chain-order.sh`, invoked from the `check` target in
the `Makefile`. A future edit that puts them back in chain order (i.e.
declares `session-resolver` before `jwt-headers` again) fails `make check`
with an explicit message instead of shipping silently.

## The four flows

### Flow 1 — login

```
Front → GET /auth/login/{provider}          (public route, proxied to auth-bff)
auth-bff → 302 to Keycloak (PKCE + state + nonce)
user authenticates
Keycloak → 302 /auth/callback?code=...
auth-bff: validate state, exchange code, validate nonce
auth-bff: sid = CSPRNG(32 bytes), base64url
auth-bff: write v1:session:{sid} and index v1:kcsid:{kc_sid} → sid
auth-bff → 302 to front + Set-Cookie: <cookie_name>=...; HttpOnly; Secure; SameSite=Lax; Path=/
```

The JWT never reaches the browser. `session-resolver` is not involved in this
flow at all — `/auth/*` is in its `skip_paths`.

### Flow 2 — authenticated request

```
Front → GET /api/projects   (cookie, credentials:'include')
session-resolver:
  path in skip_paths, OPTIONS, or Authorization present  → pass through (dual mode)
  no cookie                                              → pass through (jwt-headers 401s)
  cookie present but not 43-char base64url                → 401, no Valkey hit
  mutating method + Origin/Referer not allowed            → 403 (CSRF)
  HMGET v1:session:{sid} access_token sub exp abs_exp
    miss / empty access_token                             → 401
    now >= abs_exp                                         → delete key, 401
    now >= exp - refresh_threshold_seconds                 → refresh (flow 3)
  renew idle TTL if less than half remains
  Authorization: Bearer <access_token>; delete the Cookie header
jwt-headers: validate, inject x-user-id / x-user-roles / ...
backend: resource server validates the same JWT, same as it always has
```

### Flow 3 — lazy refresh

```
session-resolver: SETNX v1:lock:refresh:{sid} EX 5
  lock acquired → POST auth-bff /internal/sessions/{sid}/refresh (X-Internal-Secret header)
                  auth-bff uses the refresh token, rewrites the key, returns the access token
  lock lost     → re-poll Valkey for the concurrent refresh's result
```

The refresh token never leaves `auth-bff`; the plugin never decrypts
`refresh_token_enc`. `auth-bff` down during a needed refresh is a `503`, not a
silent pass with a stale token.

### Flow 4 — logout

- **RP-initiated:** `POST /auth/logout` → `auth-bff` deletes the session,
  expires the cookie, redirects to Keycloak's `end_session_endpoint`.
- **Backchannel:** Keycloak calls `POST /auth/backchannel-logout` with a
  signed `logout_token` (server-to-server, no cookie) → `auth-bff` validates
  it, resolves `sid` through `v1:kcsid:{kc_sid}`, deletes the session. Takes
  effect immediately because `session-resolver` reads Valkey on every
  request — there is no cache to wait out.

## The `v1:` Valkey contract

**Owner: `auth-bff`.** This plugin only ever reads `v1:session:{sid}`
(`HMGET`), renews its idle TTL (`EXPIRE`), and deletes it once
(`DEL`, on `abs_exp` expiry or refresh failure). It never writes
`v1:kcsid:{kc_sid}`, `v1:lock:refresh:{sid}`, or `v1:logout_jti:{jti}`, and
never decrypts the `*_enc` fields. Changing the shape of this contract means
introducing `v2:` in parallel and migrating — never mutating `v1` in place —
and any change on either side must be coordinated with `auth-bff`.

### `v1:session:{sid}` (hash)

| Field | Type | Written by | Read by this plugin | Notes |
|---|---|---|---|---|
| `ver` | int | auth-bff | no | schema version, `1` |
| `sub` | string | auth-bff | yes | JWT subject, for logs only |
| `kc_sid` | string | auth-bff | no | Keycloak SSO session id, for backchannel logout |
| `access_token` | string | auth-bff | **yes** | the JWT injected as the bearer, plaintext |
| `exp` | int64 | auth-bff | **yes** | epoch seconds, access token expiry — triggers refresh |
| `abs_exp` | int64 | auth-bff | **yes** | epoch seconds, hard session ceiling — never extended |
| `refresh_token_enc` | bytes | auth-bff | never | AES-256-GCM, key only `auth-bff` holds |
| `id_token_enc` | bytes | auth-bff | never | needed as `id_token_hint` on RP-initiated logout |
| `created_at` | int64 | auth-bff | no | audit only |

The plugin issues a single `HMGET access_token sub exp abs_exp` per request
that needs resolving. It cannot read the refresh token: a compromised edge
yields short-lived access tokens, not the ability to mint new ones.

### Auxiliary keys (all owned and written by `auth-bff`)

```
v1:kcsid:{kc_sid}      → sid     reverse index for backchannel logout
v1:lock:refresh:{sid}  → "1"     SETNX, TTL 5s, refresh stampede guard
v1:logout_jti:{jti}    → "1"     logout token replay guard, TTL = token exp
```

**`v1:kcsid:{kc_sid}` must never be touched by this plugin.** Its TTL runs to
the session's `abs_exp`, set once by `auth-bff` at login — a deliberately
different lifetime from the session key's 30-minute idle TTL. `renewIdle`
(`plugins/session-resolver/session.go`) only ever calls `EXPIRE` on
`v1:session:{sid}`. If it (or any future change) applied the idle TTL to the
`kcsid` index instead, the index would shrink on every renewal and could
expire out from under a session that is still active — at which point
Keycloak's backchannel logout call would resolve no `sid`, silently no-op,
and still answer Keycloak `200 OK`, leaving the session alive after the user
believed they had logged out everywhere. `session_test.go`'s
`TestStoreRenewIdleOnlyBelowHalf` seeds the reverse index alongside the
session and asserts its TTL is still near its original 10h after a
`renewIdle` call that does renew the session key — treat a failure there as a
security regression, not a flaky test.

## Environment variables

| Variable | Consumed by | Default | Notes |
|---|---|---|---|
| `SESSION_ENABLED` | `krakend.tmpl` | `session.json`'s `enabled` (`true`) | Set to `false` to disable the plugin entirely — see **Rollback** below. |
| `VALKEY_ADDR` | `session.json` → plugin | `valkey:6379` | Standalone Valkey only; the plugin forces a single-client connection, never cluster mode. |
| `VALKEY_PASSWORD` | `session.json` → plugin, and the `valkey` compose service itself | none | **No real default anywhere.** The compose file's `devpassword` fallback is a development placeholder only — set a real value out of band in every other environment. |
| `SESSION_COOKIE_NAME` | `session.json` → plugin | `sid` | Must be `__Host-sid` wherever TLS terminates at the edge (the `__Host-` prefix requires HTTPS, no `Domain`, `Path=/`); `sid` is the development-only name because `__Host-` cookies do not work over `http://localhost:8090`. |
| `AUTH_BFF_REFRESH_URL` | `session.json` → plugin | `http://auth-bff:8086/internal/sessions/{sid}/refresh` | Must contain the literal `{sid}` placeholder or the gateway refuses to start. |
| `INTERNAL_SHARED_SECRET` | `session.json` → plugin | **required, no default** (`docker-compose.yml` fails fast with `:?internal shared secret is required`) | Sent as `X-Internal-Secret` on every refresh call to `auth-bff`. Rotate it on both sides together. |
| `AUTH_BFF_HOST` | `hosts.json` → the `/auth/*` route backends | `http://host.docker.internal:8086` | Where the gateway proxies the public OIDC routes. Not read by the plugin itself — the plugin talks to `auth-bff`'s `/internal` endpoint directly via `AUTH_BFF_REFRESH_URL`, never through the gateway's own routing. |
| `KEYCLOAK_ISSUER` / `KEYCLOAK_JWKS_URL` | `jwt.json` (`jwt-headers` plugin) | `http://localhost:8083/realms/forgeos` / `http://host.docker.internal:8083/...` | Not consumed by `session-resolver` itself, but the bearer it injects is only as good as `jwt-headers`'s ability to validate it against these. **`config/krakend.tmpl` does not currently read these two env vars** — `jwt-headers`'s `jwks_url`/`issuer` are rendered straight from `config/settings/jwt.json`, unlike every other secret/URL in this file. Pointing at a different Keycloak (a different port, a different realm) means editing `jwt.json` directly, not exporting the env var, until that gap is closed. |

## Failure modes — all fail closed

| Failure | Response | Rationale |
|---|---|---|
| Valkey down, or the 200ms default timeout trips | `503` | No store, no identity. Never pass unauthenticated. |
| Session missing, or `access_token` present but empty | `401` | Front redirects to login. |
| `abs_exp` exceeded | `401` + session key deleted | Hard ceiling — a hijacked cookie cannot outlive it through activity alone. |
| Cookie present but not a 43-character base64url value | `401`, no Valkey round-trip at all | Cheap rejection of garbage before it costs a network hop. |
| `auth-bff` unreachable or errors during a needed refresh | `503` | The old token may already be stale or revoked; never forward it past its window on a guess. |
| Mutating method (not in `csrf_safe_methods`) with a disallowed or absent `Origin`/`Referer` | `403` | CSRF: under `SameSite=Lax` the cookie can still ride along on some cross-site requests. |
| Missing or invalid plugin configuration (`valkey_addr`, `cookie_name`, `internal_secret` empty, or `refresh_url` missing `{sid}`) | gateway refuses to start | Same posture as `jwt-headers` with invalid `skip_paths` — fail at boot, not at request time. |
| A caller supplies both a cookie and an `Authorization` header | The header wins, untouched | Dual mode: this is what keeps MCP, CI and mobile clients working unchanged. `jwt-headers` still validates whatever bearer arrives, so trusting it costs nothing. |

## Rollback

Setting `SESSION_ENABLED=false` removes `krakend-session-resolver` from the
`plugin/http-server.name` array entirely (see the conditional block in
`config/krakend.tmpl`) and regenerates the config. With the plugin absent:

- cookie-only traffic is no longer resolved — `jwt-headers` sees no
  `Authorization` header and rejects it with `401`, exactly as if the plugin
  had never existed;
- bearer traffic is completely unaffected, since it never depended on this
  plugin to begin with.

This makes the flag a safe, immediate kill switch: it does not require a
config rewrite beyond the one env var, and it cannot make the JWT-bearer path
behave any differently than it did before this plugin was added.

## Runbook

- **A Valkey outage takes down cookie traffic, not bearer traffic.** Every
  cookie-carrying request needs the `HMGET` in flow 2 above and fails closed
  with `503` when Valkey is unreachable or the request times out. Bearer
  traffic (`Authorization` header present) never touches Valkey through this
  plugin and keeps working — MCP integrations, CI, and any client that
  already holds a JWT are unaffected by a Valkey incident. If the browser
  front is down and CI/MCP traffic is fine, check Valkey first.
- **The `sid` in logs is never the session cookie value.** `session-resolver`
  logs `sub`, path, method, status and a short outcome string
  (`malformed_session`, `miss`, `expired`, `refresh_error`, ...) — never the
  cookie, the sid, or a token. If a log line ever contains a 43-character
  base64url string next to `sid=`, that is a regression worth treating as a
  security incident, not a debugging convenience.
- **`golang.org/x/sys` must stay pinned to match krakend-ce's build.** The
  plugin's transitive dependencies (`valkey-go`, `testcontainers-go`) want a
  newer `golang.org/x/sys` than the one built into `krakend:2.13.4`. Go's
  plugin loader requires every package shared with the host binary to match
  *exactly* — a mismatch here is completely silent at build time: the `.so`
  compiles cleanly, `krakend check` passes, and the plugin simply never
  registers ("No plugin registered as krakend-session-resolver"), with
  nothing in the build log pointing at the cause. `plugins/session-resolver/go.mod`
  carries a `replace golang.org/x/sys => golang.org/x/sys v0.42.0` for
  exactly this reason. If a future Go or krakend-ce upgrade changes the
  version krakend-ce embeds, this replace directive has to move with it —
  check `plugins/Dockerfile.builder`'s Go version against
  `docker run --rm krakend:<tag> version` (Go Version line) at the same
  time, since both pins track the same upstream build.
- **Plugin build toolchain must match `krakend:<tag>` exactly**, not just be
  "recent enough" — see the previous point. `plugins/Dockerfile.builder`
  pins the Go version that `make plugin-build` uses; verify it against the
  target `krakend` image's reported Go version before bumping either one.
  This pin protects the local `make plugin-build` / `make dev` path only —
  the production `Dockerfile` builds from `krakend/builder:2.13.4` directly
  (the same tag as the runtime `krakend:2.13.4` image), so it matches by
  construction and has no separate version number to fall out of sync.
  `plugins/session-resolver/go.mod`'s own `go 1.25.7` directive is a floor,
  not a pin — the module builds fine under the newer 1.25.9 toolchain, so it
  does not need to move in lockstep. The number that actually has to track
  krakend-ce's embedded Go version is `plugins/Dockerfile.builder`'s `FROM
  golang:<version>-alpine`, not the `go.mod` directive.
