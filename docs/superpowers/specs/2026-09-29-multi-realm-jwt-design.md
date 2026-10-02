# Multi-realm JWT validation at the edge — design

- **Date:** 2026-09-29
- **Status:** approved for planning
- **Amends:** [ADR-0002](../../adr/0002-platform-edge-global-keycloak-realm-per-product.md),
  *Edge de plataforma compartido + Keycloak global con realm-por-producto*. Merged to `main` on
  2026-10-02 (PR #1) and still `status: proposed`, so it carries this amendment in its own
  «Enmienda» section rather than being superseded by a new ADR.
- **Depends on:** the `products` block in `endpoints.yaml` (separate, smaller deliverable — see §2)

## 1. Context and problem

The gateway is meant to be one shared edge in front of several products. Today `jwt-headers`
validates against exactly one realm: `jwt.json` carries a single `issuer` and a single
`jwks_url`, and every protected route is checked against them. One realm per running gateway.

That blocks two things at once. Two products whose users live in different realms cannot be
served by the same gateway, and a developer cannot load one product's endpoints locally
without also pointing the whole gateway at that product's realm by hand.

The question that started this design was whether the realm could be read from the token's
`iss` claim and the JWKS URL built from it. It can, and it is the wrong thing to do — §3
explains why, and §5 shows what replaces it.

### What exists today

- `jwt-headers` verifies signatures through `keyfunc/v3` against one JWKS, with a background
  refresher, algorithms restricted to `RS256/384/512` and `ES256/384/512`, `issuer` checked
  when configured, `required_claims` enforced, and `503` while the JWKS has not loaded.
- The handler's first statement deletes every header it manages plus `x-ip`, before the
  preflight and `skip_paths` early returns, so identity headers cannot be injected by a client.
- A path-to-policy table already exists: `roleRule{Path, Roles}` matched with `matchGlob`.
- `skip_paths` are derived at render time from endpoints marked `auth: public`, with
  `{param}` rewritten to `*`.
- The plugin has two tests, both for the path matcher. The validation path has none.

## 2. Scope

**In scope.** Realm selection per route, bound at build time from the endpoint spec and
confirmed against the token at request time; the configuration schema that supports it; the
build-time guards that make an unbound protected route impossible; the observability needed to
investigate a denial; and the test foundation the plugin currently lacks.

**Out of scope.**

- CORS and rate limiting. Both are already configuration at three levels — service-wide in
  `cors.json` and `rate_limit.json`, per endpoint through `qos/ratelimit/router` (the
  `rate_limit:` key in the spec, already supported and in use), and per endpoint for
  `security/cors`, which the KrakenD 2.13.4 schema accepts. Nothing here changes them.
- Authorization beyond authentication. Binding a route to a realm proves the token came from
  the right realm. It does not decide what a legitimate user of that realm may do; that is
  `required_roles`.
- Keycloak consolidation. Two instances run today (`:8082` generic, `:8083` forgeos); merging
  them is ADR-0002's step 3. §4 keeps this design deployable before that happens.

**Prerequisite, and the dependency runs one way.** The route-to-realm binding is derived from
the `products` block in `endpoints.yaml`, so that block must land first. It is worth shipping
on its own — it gives per-product endpoint loading (`PRODUCTS=forgeos`) with no Go changes and
no identity work — and this design consumes it. The reverse is not true: the products change
does not need anything from here.

## 3. Threat model

| # | Threat | Mitigation |
|---|---|---|
| **T1** | **Key-source hijack.** Building the JWKS URL from an unverified `iss` lets an attacker point key retrieval at a server they control, sign with their own key, and be accepted. | Eliminated by construction, not mitigated: the key set is selected by the route, from configuration. The token never influences which key verifies it. No unverified parse, no URL derived from a claim. |
| **T2** | **Cross-realm token reuse.** A legitimate token from realm B presented on realm A's routes — authentic, and authorised by nothing. | The route's bound realm determines the expected issuer, and `jwt.WithIssuer` rejects a token whose `iss` differs. Comparison is exact string equality. |
| **T3** | **An unbound route failing open.** The existing `hasRequiredRole` returns `true` when no rule matches, which is right for roles and catastrophic for realms: it would accept any realm. | Closed twice. `make gen` refuses a spec where a `protected` endpoint does not resolve to a realm; at runtime an unbound protected route returns `401` and logs at `ERROR`. |
| **T4** | **Cross-realm key confusion.** Two realms can mint tokens carrying the same `kid`. With one merged key pool, a token claiming realm B could verify against realm A's key. | One `keyfunc` per realm, never a merged pool. Verification uses only the bound realm's key set. |
| **T5** | **Outbound-fetch amplification.** Lazy per-realm fetching lets unknown realms drive unbounded connections and memory. | The realm table bounds the map by construction, and every realm in it is prefetched at startup with background refresh, so no request triggers a fetch. |
| **T6** | **The introspection fallback already derives its URL from `iss`.** It is safe today only because single-realm validation checks `iss` against the configured issuer before that line runs. | Derive it from the bound realm's internal base instead. Note that introspection deliberately fails open (`"introspection failed, allowing token"`), so an unreachable endpoint admits tokens. It is dormant today (`introspection_enabled: false`) and this design does not wake it. |
| **T7** | **Identity-header injection.** | Already mitigated and must stay that way: the strip is the handler's first statement and covers the multi-realm path too. |
| **T8** | **Algorithm confusion.** | `WithValidMethods` keeps `RS*`/`ES*` only. No `alg: none`, no HMAC. |
| **T9** | **Startup posture.** | A route whose bound realm has no key set yet returns `503`, never passes through. Other realms keep serving. |

Two risks this design does not address, stated so nobody assumes otherwise: a global Keycloak
is a shared failure domain (ADR-0002 records it), and nothing here constrains what a legitimate
user of a product may reach inside that product.

## 4. Configuration schema

Two surfaces, and the boundary is the point: **realm topology is maintained by hand, the
route binding is generated.**

### `config/settings/jwt.json` — by hand

```json
{
  "enabled": true,
  "issuer_base": "http://localhost:8083",
  "jwks_base":   "http://host.docker.internal:8083",
  "realms": [
    { "name": "forgeos" },
    { "name": "vitxo",
      "issuer_base": "http://localhost:8082",
      "jwks_base":   "http://host.docker.internal:8082" }
  ],
  "cache_ttl_minutes": 60,
  "required_claims": ["sub", "organizationId"]
}
```

`claims_to_headers` is omitted from the snippet only for length; it keeps its current five
entries and its current position in the file. `cache_ttl_minutes` applies to every realm — there
is no per-realm TTL, and nothing here suggests one is needed.

The two bases are not redundant. They encode the asymmetry already present in the current file:
`issuer_base` is the public URL as Keycloak mints it into `iss`, `jwks_base` is the one
reachable from inside the container. Today's values differ (`localhost:8083` versus
`host.docker.internal:8083`), and inside the container `localhost` is the container. Per realm:

```
issuer   = {issuer_base}/realms/{name}
jwks_url = {jwks_base}/realms/{name}/protocol/openid-connect/certs
```

A realm entry may override either base, or set `issuer`/`jwks_url` explicitly. The override is
not speculative: two Keycloak instances run today, and without it this cannot be deployed until
they are consolidated.

`claims_to_headers` and `required_claims` stay global. The realms carry the same claims under
the same names.

### Two modes, one switch

| `realms` | Behaviour |
|---|---|
| absent | Exactly today's behaviour: flat `issuer` + `jwks_url`, one realm, **no binding required**. |
| present | Multi-realm. Every `protected` route must resolve to a realm, and the flat `issuer`/`jwks_url` must be absent — `make check` fails if both forms are set, so the question of which one wins never arises rather than being answered by precedence. |

This is what keeps T3's rule coherent: "a protected route without a binding is a `401`" applies
only in multi-realm mode. In single-realm mode there are no bindings and there should be none.

### `endpoints.yaml` — the product declares its realm

```yaml
products:
  forgeos:  { prefix: "",      backend: forgeos,  realm: forgeos }
  vitxo:    { prefix: /vitxo,  backend: vitxo,    realm: vitxo }
  platform: { prefix: "",      backend: auth_bff }   # public routes only
```

`realm` is required when a product has at least one `protected` endpoint. `platform` does not
need one: its five routes are public and land in `skip_paths`.

### The binding — generated

Each endpoint carries its realm into `endpoints.json`, and the template ranges over them the
way it already does for `skip_paths`, emitting:

```json
"route_realms": [
  { "path": "/api/projects",       "realm": "forgeos" },
  { "path": "/api/stories/*",      "realm": "forgeos" },
  { "path": "/vitxo/api/orders/*", "realm": "vitxo" }
]
```

`{param}` becomes `*`, matching the existing `skip_paths` convention, and matching uses
`matchGlob`, which `roleRule` already uses.

**Precedence between two matching patterns is deliberately undefined.** Overlap is a build
error (§6), so the situation cannot reach a running gateway through the generator. If it reaches
the handler anyway, that is a hole in the guard, and the handler denies rather than picking a
winner — a most-specific-wins rule would let the bug ship quietly.

`PRODUCTS=` does not touch `jwt.json`. Loading only `vitxo` produces bindings for its routes
only; prefetching the unused `forgeos` key set costs one background request and avoids having
to keep two files in sync.

## 5. Request flow

The realm is decided by the route and merely *confirmed* by the token. Steps 1–4 and 8–12 are
today's handler unchanged; 5 and 6 are new, and 7 changes its arguments.

```
1.  Delete managed headers + x-ip                        <- T7, first statement
2.  OPTIONS            -> pass
3.  skip_paths         -> pass (public route, no realm)
4.  Extract Bearer     -> 401 if missing or malformed
5.  Resolve the route's realm (route_realms, matchGlob)  <- T2, T3
       no match       -> 401  (+ ERROR log: our config gap)
       ambiguous      -> 401  (build-time guard should make this unreachable)
6.  Key set for that realm loaded?  -> 503 if not        <- T9
7.  jwt.Parse(token,
       keyfunc OF THE BOUND REALM,                       <- T4: one set per realm
       WithValidMethods(RS*, ES*),                       <- T8
       WithIssuer(issuer OF THE BOUND REALM))            <- T2
       -> 401
8.  Claims cast        -> 401
9.  required_claims    -> 401
10. Introspection (if enabled): URL from the bound
    realm's internal base, never from iss                <- T6
11. hasRequiredRole    -> 403
12. Write headers from claims
```

Step 5 precedes any key handling because without a resolved realm there is no key set to
consult. The order is a requirement, not a preference.

An unbound route returns the generic `401` but logs at `ERROR`: the fault is ours, `make gen`
should have refused the spec, and the log is what reveals that the guard has a hole.

Error bodies never name the expected realm. Telling a caller which realm was expected hands
over the topology.

`session-resolver` runs earlier, so a token it injected from a cookie goes through these same
checks. A session minted by `auth-bff` against realm X, used on a route bound to realm Y, is a
`401`. The cookie is not a pass between products.

Without `realms`, steps 5 and 6 are skipped and step 7 uses the flat fields. The path running
in production today does not change a single branch.

## 6. Generation and build-time guards

Two rules in the generator, both failing `make check`:

1. **No overlap.** If two products produce patterns that can match the same path, error. Never
   "first one wins" in silence.
2. **Full coverage.** Every `protected` endpoint resolves to a realm, or error.

A third, in config validation: `realms` together with flat `issuer`/`jwks_url` is an error.

## 7. Errors and observability

### Status per branch

| Branch | Status | Body |
|---|---|---|
| No Bearer / malformed | `401` | `{"message":"missing or invalid authorization header"}` |
| Route without a realm binding | `401` | `{"message":"invalid or expired token"}` |
| `iss` differs from the bound realm | `401` | same |
| Signature, expiry, `alg` | `401` | same |
| Missing `required_claim` | `401` | `{"message":"token missing required claim"}` |
| Token inactive (introspection) | `401` | `{"message":"token is no longer active"}` |
| Bound realm's key set not loaded | `503` | `{"message":"service not ready, JWKS not loaded yet"}` |
| Missing role | `403` | `{"message":"forbidden: missing required role"}` |

The four middle `401`s share a body deliberately.

### Logs

| Branch | Level | Fields |
|---|---|---|
| `iss` mismatch | `WARN` | `path`, `realm_expected`, `iss_claimed` (truncated), `trace_id` |
| **Route without binding** | **`ERROR`** | `path`, `trace_id` |
| Invalid signature | `WARN` | `path`, `realm_expected`, `err`, `trace_id` |
| Key set not loaded | `ERROR` | `realm`, `path`, `trace_id` |
| Missing role | `WARN` | `path`, `trace_id` |
| Startup | `INFO` | one line per realm: `realm`, `jwks_url` |

Making "no binding" the only `ERROR` on the denial path is the useful part: an alert on
`level=ERROR AND plugin=jwt-headers` separates a configuration gap from hostile traffic without
reasoning about ratios.

`iss_claimed` is untrusted input and is truncated to 200 characters before logging. That is what
makes the field useful for debugging without letting a caller write arbitrary content into the
log.

Never logged: the token, `Authorization`, `Cookie`, `sid`. This repo already learned it —
commit `d6b7fa7` is "stop logging the sid on refresh failure".

### The trace correlation gap

No plugin logs a trace id today; `jwt-headers` and `session-resolver` both log `path` and
nothing else to correlate on. That makes a claim in `docs/session-flow.md` wrong: it says that
after the chain reorder a request rejected by either plugin "carries a trace id in the gateway's
own logs". The request carries the *header* — `trace-context` does run first — but the log line
does not include it, so a `401` still cannot be joined to the backend's logs. That claim was
repeated in PR #4's description.

`trace_id` therefore appears in every row above: extracting it from the `Traceparent` already
present is two lines, and without it half of this observability cannot answer an incident. The
documentation correction is a separate one-line commit.

### Metrics

There are no plugin-owned metrics. KrakenD's `telemetry/metrics` exposes router and backend
counters on `:9090`; it is not a channel for a plugin to emit its own. A spike in `401` shows up
as status counts per endpoint, which is enough to alert on. What cannot be distinguished by
metric is "crossed `iss`" from "invalid signature": both are `401` on the same endpoint, and
that distinction lives only in the logs. Stated here so nobody designs an alert that cannot be
built.

## 8. Testing

### Starting point

`jwt-headers` has two tests, both for the path matcher. The validation path — signature, `alg`,
issuer, claims, header stripping — has none, so the single-realm validation running in
production today is untested. This change brings the foundation that does not exist. That is not
overhead added by multi-realm; it is debt that has to be paid to test it at all.

`session-resolver` already has the patterns to copy, including the log assertion
(`TestHandlerRefreshFailureNeverLogsTheSid`).

### Foundation

A helper that, per realm, starts an `httptest.Server` serving a static JWKS built from a freshly
generated RSA key, plus a token minter signing with it. `golang-jwt/jwt/v5` is already a
dependency and `keyfunc` loads a JWKS from JSON, so two or three realms run in memory,
deterministic, no network.

### Attack cases — written first

| # | Case | Expect | Threat |
|---|---|---|---|
| 1 | Realm B's token on a route bound to realm A | `401` | T2 |
| 2 | Token signed with B's key but carrying A's `iss` | `401` | T4 |
| 3 | Route **without a binding**, otherwise valid token | `401` + `ERROR` log | T3 |
| 4 | `alg: none` | `401` | T8 |
| 5 | HS256 signed using the public key as the HMAC secret | `401` | T8 |
| 6 | `iss` absent | `401` | T2 |
| 7 | `iss` not a string (number, array, object) | `401`, **no panic** | T2 |
| 8 | Lookalike base: `http://localhost:8083.evil.com/realms/forgeos` | `401` | T2 |
| 9 | Expired token | `401` | — |
| 10 | An unknown realm produces **no outbound request** (canary server, counter stays zero) | no fetch | T1, T5 |
| 11 | Client-supplied identity headers on a **public** route | stripped | T7 |
| 12 | Client-supplied identity headers alongside a valid token | overwritten, not appended | T7 |
| 13 | Bound realm's JWKS down → `503`, while another realm's routes still return `200` | isolation | T9 |
| 14 | No branch logs the token or `Authorization` | log assertion | — |

Case 8 is what justifies exact string equality: under `HasPrefix` or `Contains`, that token
passes.

### Build-time

| # | Case | Expect |
|---|---|---|
| 15 | `protected` endpoint whose product declares no `realm` | `make gen` fails |
| 16 | Two products with patterns that can match the same path | `make gen` fails |
| 17 | `realms` and flat `issuer`/`jwks_url` both set | `make check` fails |
| 18 | `realms` absent | valid: single-realm mode, no binding required |
| 19 | `route_realms` covers every `protected` endpoint | golden |
| 20 | `{param}` → `*` in bindings, matching `skip_paths` | golden |

### Happy path and regression

| # | Case | Expect |
|---|---|---|
| 21 | Each realm's valid token on its own routes | `200` + headers from claims |
| 22 | Public route, no token | passes |
| 23 | `Authorization` injected by `session-resolver`, matching realm | `200` |
| 24 | Session from realm X on a route bound to realm Y | `401` |
| 25 | **Single-realm mode with no `realms`** | behaviour identical to today |
| 26 | Prefetch loads all realms at startup; one that fails retries without blocking the others | — |

Case 25 protects what already runs. If it breaks, the change does not ship.

## 9. Rollout and back-compat

1. **Products block and `PRODUCTS=` filter.** Separate deliverable, no Go, no identity. Gives
   per-product endpoint loading and is the prerequisite for the binding.
2. **This design, single realm configured.** Ship the plugin with `realms` holding one entry.
   Behaviour is equivalent to today, but the binding path, the guards and the tests are live and
   exercised in production traffic before a second realm exists.
3. **Second realm.** Add the entry and its product. No code change.
4. **Keycloak consolidation** (ADR-0002 step 3). Remove the per-realm base overrides once both
   realms live in one instance.

Reverting is per-step: dropping `realms` from `jwt.json` returns the plugin to single-realm mode
without touching code.

## 10. Amendment to ADR-0002

ADR-0002 chose realm selection by **URL path prefix**, with a `realms[]` table matched by
longest prefix. This design keeps its substance — realm per product, one IdP, one edge — and
changes the selection mechanism to a **binding generated from the endpoint spec**.

The reason is concrete. Prefix-based selection requires every product to carry a URL prefix,
including `forgeos`, whose routes are `/api/*` today and whose SPA would break. A generated
binding needs no prefix, so the prefix goes back to being what it should be — a way to organise
endpoints — while identity comes from the token and the route binding. It is also one source of
truth instead of a parallel table that drifts.

ADR-0002 is still `proposed`, so this is a revision of an undelivered decision, not a reversal
of a delivered one. The ADR should be updated before this is implemented.

## 11. Follow-ups, not in this work

- Correct the trace-id claim in `docs/session-flow.md` (one line).
- Decide what `/auth/*` looks like per product. Today it is the `platform` product with no
  prefix and one realm's login. With a realm per product each gets its own login, so those
  routes will eventually need a per-product shape. Out of scope here, and it needs its own
  decision.
- Introspection is dormant and fails open. If it is ever enabled across realms, that trade-off
  deserves revisiting on its own.
