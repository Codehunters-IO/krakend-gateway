# Per-app authorization at the edge

**Status:** design, awaiting review
**Date:** 2026-10-09
**Author:** Carlos Andres Montoya Tobon
**Scope:** `plugins/jwt-headers`, `cmd/gen`, `config/krakend.tmpl`, `endpoints.yaml`

## Context

The gateway routes 31 endpoints belonging to three products — `forgeos` (18),
`knowledge` (8), `platform` (5) — and 25 of them are declared `auth: protected`.
"Protected" today means one thing: the request carries a JWT whose signature
validates against the realm's JWKS, whose `iss` matches, and which carries the
claims in `required_claims` (currently `["sub"]`).

That is authentication, and it is the same test for all 25. Two facts, both
verified on 2026-10-09:

- **`aud` is never validated.** There is no `WithAudience` and no read of the
  `aud` claim anywhere in `plugins/jwt-headers/`.
- **`required_roles` is empty.** `config/settings/jwt.json` declares `[]`.

So any token the realm issues opens all 25 protected routes. A token obtained
by the shell front-end reaches ForgeOS's write endpoints and the knowledge
service's MCP endpoints alike. What the edge protects is the *gateway*, not the
*applications behind it*.

The mechanism to fix that already exists and is unused:
`hasRequiredRole` (`plugins/jwt-headers/main.go:385`) matches the request path
against `required_roles` rules, each `{path, roles}`, and admits the request if
the token carries any role in the union of the matching rules. Its default is
the one that matters here (`main.go:394`):

```go
if !matched {
    return true
}
```

No matching rule means admit. With an empty rule list, every rule matches
nothing and every protected route admits any valid token.

### Why this is not the multi-issuer problem

This design was reached while exploring multi-issuer support for applications
living in different realms. That exploration concluded:

- "One token opens every application" and "the applications live in different
  realms" cannot both hold. A token is issued by one realm, signed with its
  keys, and Keycloak's SSO session is per realm. ADR-0005 already resolved this
  by making applications **clients** of a single realm.
- Isolation between realms is better served by **one gateway per realm** than by
  multi-issuer validation in one gateway — it keeps every process single-issuer,
  needs no plugin change, and isolates JWKS readiness, claim vocabulary,
  introspection credentials and the session store per realm. That is deferred
  work, summarised under *Deferred: one gateway per realm*.
- Neither of those protects one application from another **inside** a realm.
  That gap is this document's subject, and it exists whether the platform runs
  one gateway or ten.

## Goals

1. A token must carry a role on an application's own client to reach that
   application's routes.
2. The rules live where the routes live, so the two cannot drift.
3. A protected route with no authorization decision recorded fails the build,
   not the request.
4. The transition cannot lock out traffic that works today.

## Non-goals

- **Per-resource and per-method authorization.** The house rule is explicit:
  the gateway authenticates, the service authorizes per resource. The edge
  gate stays coarse — one decision per application surface. Concretely, path
  parameters are collapsed to `*` (`/api/projects/{projectId}/stories` →
  `/api/projects/*/stories`) and no method is matched, so `GET` and `POST` on
  the same path share one rule. Distinguishing read from write belongs in the
  backend, next to the domain rule that gives it meaning.
- **Replacing backend authorization.** Defense in depth: services keep
  re-validating the bearer and keep their own policy.
- **Audience restriction.** See *Rejected for now: `aud` and `azp`*.

## Design

### 1. Declaration lives on the endpoint

Rules are declared in `endpoints.yaml`, on the endpoint, and generated into the
plugin's configuration. They are not hand-maintained globs in `jwt.json`.

The repository already learned this lesson once, for `skip_paths`. The comment
at `config/krakend.tmpl:106` records what hand-maintaining a second list cost:
two adjacent plugins could silently disagree about which paths are public, and
"the first public endpoint added outside `/auth/` would keep its `jwt-headers`
exemption but lose its `session-resolver` one". The fix was one source of truth,
`endpoints.yaml -> endpoints.json -> both plugins`. `required_roles` is the same
shape of mistake, not yet paid for: route knowledge kept outside the route spec,
by hand, and currently empty.

New optional field on `Endpoint` in `cmd/gen/spec.go`:

```yaml
- path: /api/projects/{projectId}/stories
  method: POST
  product: forgeos
  auth: protected
  roles:
    claim: resource_access.forgeos-api.roles
    any_of: [user, admin]
```

`any_of` is required when `roles` is present and must be non-empty — an empty
list would read as "no roles needed" while looking like a rule.

**`claim` is required on a protected endpoint. Amended 2026-10-09 during
implementation**, after review: it was specified as optional, falling back to
the global `roles_claim`, which is `realm_access.roles`. That fallback made a
rule that gates nothing — a realm role is global by construction and in a
default realm everyone holds the common ones — while the diff reads as
"authorization added" and the ADR claims the surface is gated on client roles.
Goal 1 was defeated by omission. The fallback survives only for `jwt.json`'s own
literals, which the generator never sees.

A second rule came out of the same review: a `claim` under `resource_access.`
must have exactly three segments. Keycloak permits a dot in a `clientId`, and
the plugin splits the claim path on dots with no escape, so
`resource_access.my.app.roles` resolves to nothing and denies every request to
that surface — a total product lockout with a configuration that the regex, the
generator and the ADR all call correct.

The generator emits the field into `endpoints.json`; `make gen-check` already
fails when the generated file drifts from the spec, so that part needs no new
guard.

### 2. The template derives the rule list

`config/krakend.tmpl:133` currently renders `required_roles` straight from
`jwt.json`:

```
"required_roles": {{ marshal .jwt.required_roles }},
```

It becomes a derivation over the endpoint spec, in the same shape as the
`skip_paths` block two lines above it, reusing the identical path-to-glob
transformation so the globs match what `matchGlob` expects:

```
regexReplaceAll "\\{[^}]+\\}" .path "*"
```

`jwt.json`'s own `required_roles` is kept and concatenated, for rules that are
not tied to a declared endpoint — the same way `jwt.skip_paths` literals survive
alongside the derived ones. It stays empty in this change.

### 3. The gate

**Glob collisions are normal and must not fail the build.** `GET /api/projects`
and `POST /api/projects` collapse to the identical glob, so the same path
legitimately produces two rules. The guard therefore fails only when two rules
share a glob and declare **different** `claim`/`any_of` pairs — which is
genuinely ambiguous, because the union would silently grant the more permissive
of the two. Identical rules are deduplicated, not reported.

`roleRule` gains an optional `claim`:

```go
type roleRule struct {
    Path  string   `json:"path"`
    Claim string   `json:"claim"` // optional; falls back to cfg.RolesClaim
    Roles []string `json:"roles"`
}
```

Client roles rather than realm roles is the whole point. `realm_access.roles`
is global by construction — a realm role says nothing about which application
the holder may enter. `resource_access.<client>.roles` is per application, and
it is independent of which front-end obtained the token, which is the property
that makes it the right gate for a shared shell.

Semantics are unchanged otherwise: the union of matching rules, any-of. Both are
documented in the ADR so they are decisions rather than accidents. Where a path
is matched by more than one rule, the union means the **most permissive** rule
wins. That is why conflicting rules on one glob are a build failure rather than
a runtime surprise.

### 4. Unmatched protected routes fail the build

`hasRequiredRole`'s admit-on-no-match behaviour stays as it is. The enforcement
moves to CI: a protected endpoint with no `roles` block, and not listed as an
explicit exemption with a reason, fails `make check`.

This is the chosen option over flipping the runtime default to deny. Flipping
the default would turn every future endpoint added without a rule into a
production 403 discovered by a user; failing the build turns it into a red pull
request discovered by its author. The runtime stays permissive precisely so the
build can be strict — a deny-by-default runtime plus an incomplete rule list is
an outage, while a permissive runtime plus a complete rule list is the same
security posture arrived at safely.

A waiver is a separate field, not a value of `roles` — `roles` is a mapping and
overloading it with a string would be two shapes in one key:

```yaml
- path: /auth/session
  method: GET
  auth: protected
  roles_waiver: "resolved by session-resolver before jwt-headers sees it"
```

`roles` and `roles_waiver` are mutually exclusive, and a `roles_waiver` with an
empty or missing reason fails the build. A waiver is then visible in the diff
that introduces it rather than in a separate file nobody reads.

### 5. Observation mode

`JWT_ROLES_ENFORCE=false` (default `true`) makes the gate log what it *would*
have denied and admit the request:

```json
{"msg":"role check would deny","path":"/api/projects","claim":"resource_access.forgeos-api.roles",
 "required":["user","admin"],"token_roles":["offline_access"],"enforced":false}
```

One deployment in that mode produces the real list of missing role assignments
from live traffic, rather than from an inventory someone assembled by hand. The
flag is global, not per rule: a per-rule flag would be a permanent escape hatch,
while a global one is visibly a migration state.

The line carries no token, no `sub` and no subject identifier beyond the roles
themselves, per the logging rule that already governs this plugin.

### 6. Rollout sequence

The order is forced, and a wrong order is an outage:

1. Create the client roles in the realm and assign them. **Before any rule
   lands**, because a rule against roles nobody holds denies everyone.
2. Land the rules with `JWT_ROLES_ENFORCE=false`. Nothing changes for callers.
3. Read the `would deny` lines. Fix assignments until they stop.
4. Set `JWT_ROLES_ENFORCE=true`.

Step 3 is the one that cannot be skipped or shortened, and the reason this
design has an observation mode at all.

### Rejected for now: `aud` and `azp`

Audience restriction would stop an attack the role gate does not: a legitimate
token, held by a user with roles on both applications, obtained by application
A's front-end and replayed against application B. Roles cannot see that — the
holder genuinely has both.

It is deferred, not dismissed, for two reasons:

- In Keycloak, `aud` does not carry the client unless audience mappers are
  configured per client. Validating `aud` today would reject every token, so the
  change is a Keycloak-side project before it is a gateway-side one.
- `azp` identifies which client **obtained** the token, not which resource it is
  for. With a shell front-end that fronts several products, every token carries
  `azp: shell-web` and the claim separates nothing.

It becomes worth the work when two applications behind this edge have genuinely
different trust levels — an operator console beside a public application. That
is also the point at which *one gateway per realm* stops being sufficient on its
own.

## Confirmation

- **`scripts/check-endpoint-authorization.sh`**, inside `make check` and
  therefore on every pull request. Fails when: a `protected` endpoint has
  neither a `roles` block nor a `roles_waiver` with a non-empty reason; an
  endpoint declares both; a `roles` block has an empty `any_of`; a `claim` is
  not a dotted path of at least two segments
  (`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z0-9_-]+)+$`, which
  `resource_access.forgeos-api.roles` satisfies and a bare `roles` does not); or
  two rules collide as described below.
- **`plugins/jwt-headers`**: role present, role absent, wrong claim path,
  claim path missing from the token, overlapping globs, the default claim
  fallback, observation mode admitting while logging, and a denial carrying the
  response headers from PR #24.
- **`cmd/gen`**: the field parses, is emitted, defaults its `claim`, and a
  malformed block fails generation.
- **Mutation testing on each new guard**, matching the standard the five pull
  requests of 2026-10-09 set: every guard is broken on purpose and some test has
  to fail. A suite that has never been seen to fail is not evidence.

## Risks

- **A wrong rollout order locks out all traffic for a product.** Mitigated by
  observation mode and by the forced sequence in §6; not eliminated, because
  step 1 happens in Keycloak where this repository cannot assert anything.
- **The glob collapse hides intent.** `/api/projects/*/stories` covering both
  `GET` and `POST` is correct for a coarse gate and wrong for anyone who reads
  it expecting method-level control. The ADR states it as a non-goal; the YAML
  will carry a comment at the first endpoint where it bites.
- **25 endpoints need a rule each, but not 25 decisions.** Because the gate is
  coarse by design, the decision is one role set per application surface — two
  of them, `forgeos` (17 endpoints) and `knowledge` (8) — applied across 25
  rows. The diff is long and mechanical. What it needs from outside this
  repository is small and specific: the client id that owns each surface, and
  the role names that exist on it. Anyone wanting finer rules later is asking
  for the method-level matching this design declares a non-goal.

## Open items

- The client names. `resource_access.<client>.roles` needs the real client ids
  per product — `forgeos-api`, `knowledge-api` or whatever the realm actually
  declares. The examples here are placeholders in that one respect and must be
  replaced with measured values before implementation, the same way ADR-0003's
  clocks are waiting on the realm.
- ~~Whether `platform`'s five endpoints need a rule.~~ Resolved by measurement
  on 2026-10-09: **all five are public** — the whole `/auth/*` flow — so no
  waiver case arises there. The 25 protected endpoints are `forgeos` (17) and
  `knowledge` (8).

## Deferred: one gateway per realm

Recorded here because it is the chosen answer for isolation **between** realms,
and this design is the answer for isolation **within** one.

Approach: one image, configuration by environment. The blocker is two lines —
`config/krakend.tmpl:123-124` renders `jwks_url` and `issuer` literally from
`jwt.json`, making them the only two values in the template that cannot be
overridden per environment, while `docker-compose.yml` already exports
`KEYCLOAK_ISSUER` and `KEYCLOAK_JWKS_URL` and the template ignores them. The gap
is already recorded in `docs/session-flow.md`.

The second half is route subsetting: `PRODUCTS` filtering exists but runs at
`make gen` time and `endpoints.json` ships inside the image, so an operator
gateway would otherwise expose all 31 product routes. Filtering by `$e.product`
inside the template's `range` is the intended fix; the index-based comma
placement in that loop makes skipping entries fiddly, which is the real cost of
approach 1 over building one image per realm.

## Related

- ADR-0003 — session state at the edge. Unaffected: this gate runs after the
  bearer exists, whatever produced it.
- ADR-0005 — one application realm, `master` for operators. This design is what
  makes "applications are clients" mean something at the edge.
- PR #24 — denial response headers. Role denials inherit them.
- PR #25 — least privilege on the session store. Same shape of argument: a
  claim in a document is not a control until something enforces it.
