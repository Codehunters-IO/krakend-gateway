# Design: Token Handler / BFF — Session-Backed Edge Authentication

**Date:** 2026-08-31
**Status:** Approved
**Branch:** docs/token-handler-bff

## Problem

The browser front-end currently holds the Keycloak JWT. That means an XSS on the
front exfiltrates a bearer credential the attacker can replay from anywhere, the
access token's claims (roles, organization, email) are readable by any script and
by any analytics or error-reporting SDK loaded on the page, and a compromised
token cannot be revoked before it expires.

KrakenD Community Edition has no session concept: it validates a bearer token per
request and forwards it. Strengthening the edge therefore means adding a session
layer around it without forking KrakenD and without touching the resource servers
behind it.

Goal: the browser holds an opaque session identifier and nothing else. Tokens live
server-side in Redis. The gateway resolves the session and injects the JWT toward
backends, so every private service keeps validating a JWT exactly as it does today.

This is the **Token Handler / BFF** pattern recommended by *OAuth 2.0 for
Browser-Based Apps* for SPAs.

## Chosen Approach

**Opaque `HttpOnly` cookie → Redis session store → Go plugin injects `Bearer`.**

Two new components:

- `plugins/session-resolver/` — Go HTTP-server plugin in this repository. Reads the
  session cookie, looks the session up in Redis, sets the `Authorization` header.
- `codehunters/auth-bff` — new Kotlin / Spring Boot 4 service (hexagonal). Owns the
  OIDC flow (login, callback, refresh, logout) and is the only writer to Redis.

### Decisions and rejected alternatives

| Decision | Chosen | Rejected | Why |
|---|---|---|---|
| Session id transport | `HttpOnly` cookie | Custom header + `localStorage` | A JS-readable id is stolen by XSS just like a JWT; the only real gain is that claims stay hidden. `HttpOnly` is what makes the pattern worth building. |
| Session id value | Opaque 32-byte CSPRNG | The JWT's `sid` claim | `sid` is Keycloak's SSO session id: stable across refreshes, present in logout tokens and the admin console. It must not double as the credential that authenticates API calls. It is kept only as a secondary index for backchannel logout. |
| OIDC flow owner | Separate `auth-bff` service | All inside the Go plugin | Keeps `client_secret` and refresh tokens off the edge, and lets Spring Security supply the audited implementation of state / nonce / PKCE / refresh rotation. |
| Language for `auth-bff` | Kotlin + Spring Boot 4 | Go | `spring-security-oauth2-client` already implements the security-critical parts of the flow. Hand-rolling them is where implementation CVEs live. Footprint is irrelevant: the service is not on the per-request path. |
| Client tokens | Dual mode: cookie **or** `Authorization` | Cookie only | Non-browser clients (MCP over Streamable HTTP, CI, mobile, Postman) must keep working unchanged. |
| Token retrieval by the plugin | Plugin reads Redis directly | Plugin calls `auth-bff` per request | A JVM hop on every request buys decoupling that a versioned key contract already provides, and makes `auth-bff` a hard dependency of all traffic. |
| Token caching in the plugin | None | In-memory cache with short TTL | A cache window breaks immediate revocation — the exact property backchannel logout exists to provide. Revisit only if Redis becomes a bottleneck. |
| Session persistence | Custom flat Redis contract | `spring-session-data-redis` | Spring Session's serialization format is internal and unreadable from Go. An explicit versioned contract survives Spring upgrades. |
| Component placement | Plugin here, `auth-bff` in a new repo | Both here / `auth-bff` inside forgeos | The plugin is generic (cookie → Redis → Bearer, no Keycloak knowledge). Keeping `auth-bff` out preserves this repository's goal of being a reusable gateway. |

## Architecture

### Plugin chain

```
gateway-timeout → accept-language → trace-context → ip-resolver → session-resolver → jwt-headers
```

`session-resolver` runs immediately before `jwt-headers`. Its only effect is setting
an `Authorization` header. `jwt-headers` is unchanged and still validates against
JWKS and maps claims to headers. **No existing plugin changes, and no backend
changes in forgeos or the knowledge-catalog MCP.**

### Flow 1 — login

```
Front → GET /auth/login                    (public route, proxied to auth-bff)
auth-bff → 302 to Keycloak (PKCE + state + nonce)
user authenticates
Keycloak → 302 /auth/callback?code=...
auth-bff: validate state, exchange code, validate nonce
auth-bff: sid = CSPRNG(32 bytes), base64url
auth-bff: write v1:session:{sid} and index v1:kcsid:{kc_sid} → sid
auth-bff → 302 to front + Set-Cookie: __Host-sid=...; HttpOnly; Secure; SameSite=Lax; Path=/
```

The JWT never reaches the browser.

### Flow 2 — authenticated request

```
Front → GET /api/projects   (cookie, credentials:'include')
session-resolver:
  Authorization present?      → pass through untouched   (dual mode)
  otherwise                   → HMGET v1:session:{sid}
  miss / expired              → 401, backend never called
  exp - now < threshold       → refresh (flow 3)
  set Authorization: Bearer <access_token>, delete the cookie header
jwt-headers: validate, inject x-user-id / x-user-roles / ...
backend: resource server validates the same JWT
```

### Flow 3 — lazy refresh

```
session-resolver: SETNX v1:lock:refresh:{sid} EX 5
  lock acquired → POST auth-bff /internal/sessions/{sid}/refresh
                  auth-bff uses the refresh token, rewrites the key, returns the access token
  lock lost     → re-read Redis for up to 500ms (another request is doing it)
```

The refresh token never leaves `auth-bff`.

### Flow 4 — logout

- **RP-initiated:** `POST /auth/logout` → delete session, expire cookie, redirect to
  Keycloak's `end_session_endpoint` with `id_token_hint`.
- **Backchannel:** Keycloak `POST /auth/backchannel-logout` with a `logout_token` →
  validate it, resolve `sid` through `v1:kcsid:{kc_sid}`, delete the session. Takes
  effect immediately because the plugin reads Redis on every request.

## Data Contract

Versioned with a `v1:` prefix. Changing the shape means writing `v2:` in parallel and
migrating — never mutating `v1` in place.

### `v1:session:{sid}` (Redis hash)

| Field | Type | Written by | Read by plugin | Notes |
|---|---|---|---|---|
| `ver` | int | auth-bff | no | schema version, `1` |
| `sub` | string | auth-bff | yes | JWT subject, for logs and metrics |
| `kc_sid` | string | auth-bff | no | Keycloak `sid`, for backchannel logout |
| `access_token` | string | auth-bff | **yes** | the JWT that gets injected |
| `exp` | int64 | auth-bff | **yes** | epoch seconds, access token expiry |
| `abs_exp` | int64 | auth-bff | **yes** | hard session ceiling, never extended |
| `refresh_token_enc` | bytes | auth-bff | never | AES-256-GCM, key only in auth-bff |
| `id_token_enc` | bytes | auth-bff | never | needed as `id_token_hint` on logout |
| `created_at` | int64 | auth-bff | no | audit |

The plugin issues a single `HMGET access_token exp abs_exp sub`. It *cannot* read the
refresh token: it is encrypted with a key the edge does not hold. A compromised edge
yields short-lived access tokens, not the ability to mint new ones indefinitely.

### Auxiliary keys

```
v1:kcsid:{kc_sid}      → sid     reverse index for backchannel logout
v1:lock:refresh:{sid}  → "1"     SETNX, TTL 5s, refresh stampede guard
v1:logout_jti:{jti}    → "1"     logout token replay guard, TTL = token exp
```

### Three independent clocks

1. **`exp`** (~5 min) — a field, not a Redis TTL. Triggers refresh.
2. **Idle TTL** (30 min) — the key's actual TTL. Renewing it on every request would
   mean a write per request and would destroy the "plugin only reads" property.
   Instead the plugin renews it only when less than half remains
   (`TTL < 15min → EXPIRE 30min`): roughly one write every 15 minutes per active
   session instead of one per request.
3. **`abs_exp`** (10h, aligned with Keycloak's *SSO Session Max*) — a hard ceiling the
   plugin enforces even when the idle TTL is alive, so a hijacked session cannot live
   forever through activity.

### Cookie

```
__Host-sid=<32 bytes CSPRNG base64url>; HttpOnly; Secure; SameSite=Lax; Path=/
```

The `__Host-` prefix makes the browser reject the cookie unless it arrives over HTTPS,
carries no `Domain`, and has `Path=/` — defense against subdomain takeover and cookie
injection. It consequently does **not** work over `http://localhost:8090`, which is how
the gateway runs in development, so the name is environment-driven:
`SESSION_COOKIE_NAME=sid` locally, `__Host-sid` wherever TLS terminates. The plugin
reads the name from configuration and never hardcodes it.

### Assumptions

- **No AOF persistence in Redis.** A restart drops all sessions and users re-login,
  which is transparent while their Keycloak SSO session is alive. Enabling AOF would
  put tokens on disk — extra surface for little gain.
- **Size.** A Keycloak JWT with roles is 1–2KB; 10k active sessions ≈ 30MB.

## Component: `session-resolver` plugin

Go HTTP-server plugin following the same shape as the five existing ones
(`RegisterHandlers`, configuration under `plugin/http-server`, env toggle). New
dependency: `redis/go-redis/v9`, in its own Go module, so the other plugins are
untouched.

### Configuration — `config/settings/session.json`

```json
{
  "enabled": true,
  "redis_addr": "redis:6379",
  "redis_db": 0,
  "redis_timeout_ms": 200,
  "redis_pool_size": 50,
  "key_prefix": "v1:",
  "cookie_name": "sid",
  "idle_ttl_seconds": 1800,
  "refresh_threshold_seconds": 30,
  "refresh_url": "http://auth-bff:8086/internal/sessions/{sid}/refresh",
  "refresh_timeout_ms": 3000,
  "refresh_lock_ttl_seconds": 5,
  "allowed_origins": ["http://localhost:5173"],
  "csrf_safe_methods": ["GET", "HEAD", "OPTIONS"],
  "skip_paths": ["/auth/*", "/api/ping"]
}
```

`REDIS_PASSWORD` and `SESSION_ENABLED` come from the environment, matching the
existing `JWT_INTROSPECTION_CLIENT_SECRET` precedent. `krakend.tmpl` gets a
`{{- if $sessEnabled }}` block shaped like the existing ones, and the plugin name goes
into `plugin/http-server.name` between `krakend-ip-resolver` and `krakend-jwt-headers`.

### Per-request logic

```
if path matches skip_paths            → next()
if Authorization header present       → next()          # dual mode
cookie := readCookie(cookie_name)
if cookie == ""                       → next()          # jwt-headers will 401
if !validSidFormat(sid)               → 401             # 43 base64url chars, no Redis hit

if method not in csrf_safe_methods:
    if Origin/Referer not in allowed_origins → 403

fields := HMGET v1:session:{sid} access_token exp abs_exp sub
if miss                               → 401
if now >= abs_exp                     → DEL key; 401
if now >= exp - refresh_threshold     → refresh(sid)
if TTL(key) < idle_ttl/2              → EXPIRE key idle_ttl

req.Header.Set("Authorization", "Bearer " + access_token)
req.Header.Del(cookie_name)
next()
```

Three deliberate properties:

- **A present `Authorization` always wins.** This is what keeps MCP and CI working. An
  XSS could inject its own bearer token, but that gains nothing: `jwt-headers` still
  validates it against Keycloak.
- **`Header.Set`, not `Add`.** Overwrites any `Authorization` a client smuggled
  alongside the cookie; otherwise precedence is ambiguous.
- **The cookie is stripped before the backend call.** Private services must never see
  the session identifier; their contract is the bearer token.

### Failure modes — all fail closed

| Failure | Response | Rationale |
|---|---|---|
| Redis down / 200ms timeout | 503 | No store, no identity. Never pass unauthenticated. |
| Session missing or expired | 401 | Front redirects to `/auth/login`. |
| `abs_exp` exceeded | 401 + delete key | Hard ceiling. |
| `auth-bff` down during refresh | 503 | The old token may already be revoked. |
| Disallowed `Origin` on a mutating method | 403 | CSRF. |
| Missing or invalid plugin configuration | gateway refuses to start | Same as `jwt-headers` with invalid `skip_paths` today. |

Operational consequence worth noting in the runbook: **bearer traffic survives a Redis
outage** (it never touches Redis) while cookie traffic does not. MCP and CI keep
working while the browser front is down.

### Observability

Prometheus metrics via the existing `telemetry/metrics`:
`session_resolve_total{result=hit|miss|expired|error}`, `session_refresh_total{result}`,
`session_redis_latency_seconds`. Logs carry `sub` and `traceparent` — **never** the
`sid` and never a token.

### New gateway routes

`/auth/login`, `/auth/callback`, `/auth/logout`, `/auth/backchannel-logout` route to a
new `authbff` backend with `auth: public`, so the generator adds them to `skip_paths`
automatically. They use `no-op` passthrough so the 302 and `Set-Cookie` survive
untouched. `auth-bff`'s `/internal/*` is **not** exposed through the gateway.

## Component: `auth-bff`

Kotlin 2.2, Spring Boot 4, Spring Security 7. Gradle modules
`domain / application / infrastructure / bootstrap`. Port 8086.

### What Spring Security provides

`oauth2Login` implements the full authorization code flow: `state` generation and
validation, PKCE, `nonce` validation in the ID token, OIDC discovery, code exchange,
JWKS signature validation, and refresh with rotation. Our code only decides **what to
do with the tokens once obtained** — a custom `AuthenticationSuccessHandler`:

```
onSuccess(OidcUser, OAuth2AuthorizedClient):
    sid = CSPRNG(32 bytes)
    CreateSessionUseCase(sid, tokens, oidcUser.claims["sid"])
    response.addCookie(sessionCookie(sid))
    redirect(FRONTEND_URL)
```

**Servlet session during login only.** `oauth2Login` stores the
`OAuth2AuthorizationRequest` (state + code verifier) between redirect and callback,
using `HttpSession` by default. The stateless alternative is a cookie-based repository —
i.e. writing our own crypto to save a `JSESSIONID` that lives 30 seconds. Not worth it:
`SessionCreationPolicy.IF_REQUIRED`, and the servlet session is invalidated in the
callback. After login the service is stateless.

### HTTP surface

| Method | Path | Exposed at gateway | Purpose |
|---|---|---|---|
| GET | `/auth/login` | public | 302 to Keycloak with PKCE |
| GET | `/auth/callback` | public | exchange, create session, `Set-Cookie`, 302 to front |
| GET | `/auth/session` | public* | returns `{sub, username, roles, org, exp}` to the front |
| POST | `/auth/logout` | public | delete session, expire cookie, 302 to `end_session_endpoint` |
| POST | `/auth/backchannel-logout` | public | validate `logout_token`, revoke by `kc_sid` |
| POST | `/internal/sessions/{sid}/refresh` | **no** | internal network only, called by the plugin |

`/auth/session` was not in the original scope and is **mandatory**: taking the JWT away
from the browser also takes away the claims the UI uses to render the user's name,
roles and organization. (*Public in the sense that it needs no bearer token — it
authenticates with the cookie, resolved against Redis like the plugin does.)

Network isolation is not an access control: `/internal/*` also needs mTLS or a shared
token between plugin and `auth-bff`, or any compromised pod could force refreshes.

### Layers

- **domain** — `Session` (immutable), `SessionId` (value object, validates format),
  `TokenBundle`, `KeycloakSessionId`. Invariants: `absExp > createdAt`, `sid` is 43
  base64url characters. Zero Spring annotations.
- **application** — ports `SessionStorePort`, `IdentityProviderPort`, `TokenCipherPort`,
  `ClockPort`. Use cases: `CreateSession`, `RefreshSession`, `RevokeSession`,
  `RevokeByKeycloakSid`, `GetSessionView`. One public method per class, no
  `@Transactional` (there is no transactional database).
- **infrastructure** — `RedisSessionStoreAdapter` (Lettuce, writes the `v1:` contract),
  `SpringSecurityIdentityProviderAdapter` (wraps `OAuth2AuthorizedClientManager` for
  refresh), `AesGcmTokenCipherAdapter`, REST controllers.
- **bootstrap** — `SecurityConfig`, wiring, the success handler.

### Encryption at rest

`refresh_token` and `id_token` are encrypted with AES-256-GCM. The 32-byte key comes
from `SESSION_CIPHER_KEY` (base64) via a secrets manager, never the repository. Each
value is prefixed with a `key_id` so the key can be rotated without invalidating live
sessions: decrypt with whichever key matches, re-encrypt with the new one on the next
refresh. The `access_token` stays in clear text — the plugin needs it, and it is a
signed JWT valid for five minutes.

## Security

### What the design buys

| Threat | Before (JWT in the front) | After |
|---|---|---|
| XSS exfiltrates the credential | steals the JWT, replays it from anywhere for 5–30 min, and the refresh token for hours | cannot read the cookie; nothing to exfiltrate |
| Claim disclosure | roles, org, email visible to any script and to front-end logs | the browser only sees what `/auth/session` returns |
| Tokens in logs / analytics / Sentry | the JWT travels in headers tracking SDKs capture | only an opaque id with no semantic value |
| Revocation | impossible until expiry | immediate, a `DEL` in Redis |

### What it does not fix

**An XSS can still act as the user.** The cookie is attached automatically to every
request the script makes from the page. `HttpOnly` prevents *taking the credential out*
of the browser to use elsewhere or persist it; it does not prevent *using it from
inside*. The attacker is confined to the live session on the compromised browser — a
large improvement, not a cure. Complementary mitigations, out of scope for v1: a strict
CSP on the front, and re-authentication for sensitive operations.

### CSRF

Two layers, because neither suffices alone:

1. **`SameSite=Lax`** — the browser does not attach the cookie to cross-site requests,
   while still allowing the top-level GET navigation the Keycloak callback needs.
2. **`Origin` validation** in the plugin for every mutating method — covers what `Lax`
   does not, and survives an old browser.

**Hard constraint imposed by `Lax`:** the front and the gateway must share a
registrable domain (`app.example.com` + `api.example.com` works; a different domain does
not). If the front ever moves to another domain, the cookie must become
`SameSite=None; Secure` and the *entire* CSRF defense rests on the `Origin` check.

Corollary: **no GET may mutate state.** Under `Lax` a cross-site top-level GET arrives
with the cookie, so a `GET /api/something/delete` would be an exploitable CSRF. This
warrants an architecture test over `endpoints.yaml`.

### Session fixation and rotation

- The `sid` is generated **after** successful authentication, never before, and the
  server never accepts a client-proposed `sid`.
- Every login produces a new `sid`; the `oauth2Login` servlet session is invalidated at
  the callback.
- The `sid` does **not** rotate on refresh: rotating would require `Set-Cookie` on
  responses passing through the plugin, and the plugin should not be writing cookies.
  With a 10h `abs_exp` the residual risk is bounded.

### Data protection

- Redis with `requirepass` and TLS, on an internal network, with no published port.
- `refresh_token` and `id_token` encrypted; key in a secrets manager with `key_id`
  rotation.
- `/internal/sessions/{sid}/refresh` protected with mTLS or a shared token.
- No AOF: tokens should not reach disk.

### Backchannel logout

Keycloak's `logout_token` carries a `jti`. Without deduplication, an attacker who
captures one can replay it to log users out at will — a targeted denial of service. We
store `v1:logout_jti:{jti}` with a TTL equal to the token's `exp` and reject repeats.
Validation also covers signature, `iss`, `aud`, and that the `events` claim contains
`http://schemas.openid.net/event/backchannel-logout`, so a token of another type cannot
revoke sessions.

### Rate limiting

`/auth/login` and `/auth/callback` get their own stricter `qos/ratelimit/router`. The
current `endpoint_client_max_rate: 20` is tuned for APIs, not for a login flow. Guessing
a 256-bit `sid` is not a realistic vector, but hammering Keycloak is.

### Logging

Forbidden in logs: the `sid`, any token, the `logout_token`. Allowed: `sub`,
`traceparent`, outcome. The `sid` is a bearer credential and deserves the same handling
as a password. In `auth-bff` this matches the `JwtScrubbingStructuredLogFormatter`
pattern already used in the knowledge-catalog MCP.

### Accepted residual risks

| Risk | Why accepted |
|---|---|
| XSS can act within the session | only mitigable with a CSP on the front, out of scope |
| Redis is a SPOF for cookie traffic | fails closed; bearer traffic survives |
| A compromised edge sees access tokens in clear | unavoidable — the plugin must inject them. The refresh token stays protected |
| A Redis restart logs everyone out | re-login is transparent while the Keycloak SSO session lives |

## Delivery Phases

Every phase leaves the system working and is reversible with `SESSION_ENABLED=false`,
which removes the plugin from the chain and restores today's behavior.

| Phase | Delivers | Verification gate |
|---|---|---|
| **0. Infrastructure** | Redis in `docker-compose.yml`, `/auth/*` routes in `endpoints.yaml`, `auth-bff` Gradle skeleton | `make gen-check` green, `make check` Syntax OK, compose comes up |
| **1. auth-bff core** | domain + application + `RedisSessionStoreAdapter` + `oauth2Login` + success handler + `/auth/session` + `/auth/logout` | Testcontainers Keycloak: end-to-end login; **contract test** reading the key with a raw Redis client and asserting every field |
| **2. Resolver plugin** | cookie → Redis → `Bearer`, dual mode, fail-closed. No refresh yet | request with cookie reaches the backend with a valid bearer; bearer request passes untouched; Redis down → 503; `Cookie` does not reach the backend |
| **3. Refresh** | `exp` detection, `SETNX` lock, `/internal/.../refresh` with mTLS | concurrency test: 20 simultaneous requests on an expired session cause **exactly one** refresh against Keycloak |
| **4. Revocation and hardening** | backchannel logout + `jti` dedup, `Origin` validation, idle and absolute TTL, rate limit on `/auth/*` | Keycloak logout invalidates in <1s; cross-origin `POST` → 403; repeated `jti` → 400; expired `abs_exp` → 401 |
| **5. Front and operations** | front switched to `credentials:'include'`, metrics, log scrubbing, runbook | UI works with no JWT in the browser; no log contains a `sid` or a token |

**Cross-cutting gate, every phase:** the MCP and forgeos with `Authorization: Bearer`
keep working unchanged. This is the guarantee that dual mode has not regressed.

## Testing Strategy

- **domain** — pure unit tests, no mocks. `SessionId` invariants, `absExp` computation.
- **application** — MockK on outbound ports only.
- **adapters** — Testcontainers Redis for the store; Testcontainers Keycloak for the
  full OIDC flow (login → callback → session created → refresh → backchannel logout).
- **contract** — a test that writes a session through the adapter and reads it with a
  raw Redis client, asserting the exact shape the Go plugin expects. This is the only
  coupling point between the two repositories and must fail loudly when changed.
- **plugin** — Go table tests for the decision logic (skip paths, dual mode, CSRF,
  expiry), plus integration tests against a real Redis (miniredis is not enough:
  `HMGET`, `TTL` and `SETNX` semantics matter).
- **architecture** — a test over `endpoints.yaml` asserting no GET endpoint mutates
  state, given the `SameSite=Lax` corollary above.

## Open Items

- Exact `SSO Session Max` and `Access Token Lifespan` values in the Keycloak realm must
  be read before fixing `abs_exp` and the refresh threshold.
- Choice between mTLS and a shared token for `/internal/*` depends on the target
  deployment topology, which is not yet defined for `auth-bff`.
