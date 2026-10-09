# Per-app edge authorization Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A token must carry a role on an application's own Keycloak client to reach that application's routes, with the rules declared on the endpoint and enforced at the edge.

**Architecture:** Authorization rules are declared per endpoint in `endpoints.yaml`, emitted into `endpoints.json` by `cmd/gen`, and derived by `config/krakend.tmpl` into the `required_roles` array that `plugins/jwt-headers` already consumes — the same source-of-truth path `skip_paths` already uses. The plugin gains a per-rule claim path (so client roles work) and an observation mode that logs what it would deny. A CI guard makes a protected endpoint without an authorization decision a build failure.

**Tech Stack:** Go 1.25 (plugin + generator), KrakenD 2.13.4 Flexible Configuration (Go templates + sprig), bash + python3 (CI guards), testcontainers (session-resolver suite, untouched here).

**Spec:** `docs/superpowers/specs/2026-10-09-per-app-edge-authorization-design.md`

## Global Constraints

- Values copied verbatim from the spec:
  - Claim path regex: `^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z0-9_-]+)+$`
  - Path-to-glob transformation, identical to the one at `config/krakend.tmpl:128`: `regexReplaceAll "\\{[^}]+\\}" .path "*"`
  - `auth` values in this repository are `public` and `protected` — never "private".
  - Observation flag: `JWT_ROLES_ENFORCE`, default `true`.
  - Denial status for a failed role check: `403`, body `{"message":"forbidden: missing required role"}` — unchanged from today.
- The runtime keeps admitting paths no rule matches. Enforcement of completeness is a build failure, never a runtime default flip.
- Rules are coarse per application surface. No method matching, no per-resource rules: `GET` and `POST` on one path share a rule by design.
- Every new guard gets mutation testing: break the thing it guards and some test must fail.
- Commits follow this organization's attribution convention, which lives in the author's own rules and not in this repository. Run both attribution filters before any push, over the message and over the diff.
- No secrets, no tokens, no `sub` in log lines.

## Review Focus

Five conditions the spec implies that no task's happy path exercises. Each has a test added to the task that owns the code.

1. **A claim that holds a string instead of an array.** Keycloak can emit a scalar where an array is expected; `extractStringSlice` returns nil, so the request is denied. Denial is right, but it must be the tested, understood outcome rather than an accident — Task 3.
2. **`JWT_ROLES_ENFORCE` set to something that is neither `true` nor `false`** (`1`, `yes`, `TRUE`, empty). Anything unrecognised must enforce, never silently open the edge — Task 3.
3. **A literal rule subsumed by a globbed one.** `/api/projects/mine` and `/api/projects/*` are different globs that both match `/api/projects/mine`; the union grants the more permissive, and a guard that only compares identical globs misses it — Task 5.
4. **A path with a trailing slash or a doubled separator.** `matchGlob` trims leading and trailing `/` but splits on every separator, so `/api//projects` has three segments and matches nothing — meaning a rule silently does not apply to a path a router may still route — Task 3.
5. **An endpoint carrying `roles` while `auth: public`.** The rule can never fire, because public paths leave through `skip_paths` before the gate runs. It must be a validation error, not dead configuration — Task 1.

---

### Task 1: `roles` and `roles_waiver` in the endpoint spec

**Files:**
- Modify: `cmd/gen/spec.go` (add `RoleRule`, two `Endpoint` fields)
- Modify: `cmd/gen/validate.go` (add the validation rules)
- Test: `cmd/gen/validate_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `type RoleRule struct { Claim string; AnyOf []string }`; `Endpoint.Roles *RoleRule` emitted as JSON key `roles`; `Endpoint.RolesWaiver string` with `json:"-"` (build-time only, never reaches the plugin).

- [ ] **Step 1: Write the failing tests**

Append to `cmd/gen/validate_test.go`:

```go
func protectedBase() Spec {
	s := base()
	s.Endpoints[0].Auth = "protected"
	return s
}

func TestValidate_RolesAndWaiverAreMutuallyExclusive(t *testing.T) {
	s := protectedBase()
	s.Endpoints[0].Roles = &RoleRule{Claim: "resource_access.api.roles", AnyOf: []string{"user"}}
	s.Endpoints[0].RolesWaiver = "both at once"
	assertErrContains(t, Validate(s), "mutually exclusive")
}

func TestValidate_RolesNeedsAtLeastOneRole(t *testing.T) {
	s := protectedBase()
	s.Endpoints[0].Roles = &RoleRule{Claim: "resource_access.api.roles"}
	assertErrContains(t, Validate(s), "any_of must list at least one role")
}

func TestValidate_ClaimMustBeADottedPath(t *testing.T) {
	s := protectedBase()
	s.Endpoints[0].Roles = &RoleRule{Claim: "roles", AnyOf: []string{"user"}}
	assertErrContains(t, Validate(s), "not a dotted claim path")
}

func TestValidate_EmptyClaimIsAllowedAndMeansTheGlobalDefault(t *testing.T) {
	s := protectedBase()
	s.Endpoints[0].Roles = &RoleRule{AnyOf: []string{"user"}}
	if errs := Validate(s); len(errs) != 0 {
		t.Fatalf("an omitted claim must inherit the global roles_claim, got %v", errs)
	}
}

// Review Focus 5: a public endpoint leaves through skip_paths before the gate
// runs, so a rule on it can never fire. Dead config, caught at build time.
func TestValidate_PublicEndpointsTakeNoRoles(t *testing.T) {
	s := base() // auth: public
	s.Endpoints[0].Roles = &RoleRule{Claim: "resource_access.api.roles", AnyOf: []string{"user"}}
	assertErrContains(t, Validate(s), "public endpoints take neither")
}

func TestValidate_PublicEndpointsTakeNoWaiver(t *testing.T) {
	s := base()
	s.Endpoints[0].RolesWaiver = "pointless here"
	assertErrContains(t, Validate(s), "public endpoints take neither")
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd cmd/gen && go test ./... -run TestValidate_Roles -v`
Expected: FAIL to compile — `undefined: RoleRule`, `e.Roles undefined`.

- [ ] **Step 3: Add the types**

In `cmd/gen/spec.go`, after the `RateLimit` type:

```go
// RoleRule is the optional per-endpoint authorization decision. Claim is a
// dot-notation claim path; empty inherits the plugin's global roles_claim.
// AnyOf is satisfied when the token holds at least one of the listed roles —
// the gate is coarse on purpose, one decision per application surface.
type RoleRule struct {
	Claim string   `yaml:"claim" json:"claim"`
	AnyOf []string `yaml:"any_of" json:"any_of"`
}
```

In the `Endpoint` struct, appended after `RateLimit` so the emitted key order of
every existing field is untouched:

```go
	Roles       *RoleRule `yaml:"roles" json:"roles"`
	RolesWaiver string    `yaml:"roles_waiver" json:"-"`
```

- [ ] **Step 4: Add the validation**

In `cmd/gen/validate.go`, add the import and the pattern at the top:

```go
import (
	"fmt"
	"regexp"
	"strings"
)

// claimPath accepts a dotted claim path of at least two segments:
// "resource_access.forgeos-api.roles" passes, a bare "roles" does not. Client
// ids carry hyphens, so they are legal after the first separator.
var claimPath = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z0-9_-]+)+$`)
```

Inside `Validate`'s endpoint loop, after the existing `auth` check:

```go
		if e.Roles != nil && e.RolesWaiver != "" {
			errs = append(errs, fmt.Errorf("%s: roles and roles_waiver are mutually exclusive", where))
		}
		if e.Auth == "public" && (e.Roles != nil || e.RolesWaiver != "") {
			errs = append(errs, fmt.Errorf("%s: public endpoints take neither roles nor roles_waiver", where))
		}
		if e.Roles != nil {
			if len(e.Roles.AnyOf) == 0 {
				errs = append(errs, fmt.Errorf("%s: roles.any_of must list at least one role", where))
			}
			if e.Roles.Claim != "" && !claimPath.MatchString(e.Roles.Claim) {
				errs = append(errs, fmt.Errorf("%s: roles.claim %q is not a dotted claim path", where, e.Roles.Claim))
			}
		}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `cd cmd/gen && go test ./... -v`
Expected: PASS, including the pre-existing suite.

- [ ] **Step 5b: Update the golden fixture**

`cmd/gen/testdata/expected.json` is compared byte for byte by
`TestGenerate_Golden`, so it carries every emitted key and the new one has to
land there too: add `"roles": null` to each of its three endpoints, after
`rate_limit`.

- [ ] **Step 6: Verify the generated file is unchanged for today's spec**

No endpoint declares `roles` yet, so the new key must render as `null` on every
endpoint and nothing else may move.

Run:
```bash
make gen
git diff --stat config/settings/endpoints.json
git diff config/settings/endpoints.json | head -20
```
Expected: only `"roles": null` added per endpoint; no reordering, no other key changed.

- [ ] **Step 7: Commit**

```bash
git add cmd/gen/spec.go cmd/gen/validate.go cmd/gen/validate_test.go config/settings/endpoints.json
git commit -m "feat(gen): declare per-endpoint authorization in the endpoint spec

roles and roles_waiver on the endpoint, validated at generation: the two are
mutually exclusive, any_of must list a role, a claim must be a dotted path of
at least two segments, and a public endpoint takes neither — its requests leave
through skip_paths before the gate runs, so a rule there could never fire.

No endpoint declares a rule yet, so endpoints.json gains only \"roles\": null.

<attribution trailer per the convention above>"
```

---

### Task 2: derive `required_roles` in the template

**Files:**
- Modify: `config/krakend.tmpl:133`
- Test: rendered-output assertions, run by hand in this task and pinned by the guard in Task 5

**Interfaces:**
- Consumes: `endpoints.json`'s `roles` key from Task 1; `.jwt.roles_claim` and `.jwt.required_roles` from `jwt.json`.
- Produces: the plugin's `required_roles` array, each entry `{"path": <glob>, "claim": <dotted path>, "roles": [<role>, ...]}`.

- [ ] **Step 1: Replace the literal with a derivation**

`config/krakend.tmpl` currently has, at line 133:

```
        "required_roles": {{ marshal .jwt.required_roles }},
```

Replace with:

```
        {{/* required_roles is DERIVED from the endpoint spec, exactly like
             skip_paths above it. required_roles lived in jwt.json as a
             hand-maintained list of globs and sat empty, which meant every
             protected route admitted any token the realm signed. Keeping route
             knowledge outside the route spec is the same mistake skip_paths
             already paid for. jwt.json's own list is concatenated for rules
             that belong to no declared endpoint. */}}
        {{- $roleRules := .jwt.required_roles }}
        {{- range .endpoints.endpoints }}
          {{- if .roles }}
            {{- $claim := .roles.claim | default $.jwt.roles_claim }}
            {{- $glob := regexReplaceAll "\\{[^}]+\\}" .path "*" }}
            {{- $roleRules = append $roleRules (dict "path" $glob "claim" $claim "roles" .roles.any_of) }}
          {{- end }}
        {{- end }}
        "required_roles": {{ marshal $roleRules }},
```

- [ ] **Step 2: Verify the render is byte-identical while no rule exists**

Run:
```bash
FC_ENABLE=1 FC_SETTINGS=config/settings FC_OUT=config/.r.json \
  ./scripts/krakend-check.sh config/krakend.tmpl >/dev/null
python3 -c "
import json
d=json.load(open('config/.r.json'))
print(d['extra_config']['plugin/http-server']['krakend-jwt-headers']['required_roles'])
"
rm -f config/.r.json
```
Expected: `[]`. If the render fails with `function \"dict\" not defined`, KrakenD's
template engine lacks that sprig helper on this version; use this fallback
instead of `dict`/`append`, which emits the same JSON by hand:

```
        "required_roles": [
          {{- $first := true }}
          {{- range $i, $p := .jwt.required_roles }}{{ if $i }},{{ end }}{{ marshal $p }}{{ $first = false }}{{ end }}
          {{- range .endpoints.endpoints }}{{ if .roles }}{{ if not $first }},{{ end }}{"path":"{{ regexReplaceAll "\\{[^}]+\\}" .path "*" }}","claim":"{{ .roles.claim | default $.jwt.roles_claim }}","roles":{{ marshal .roles.any_of }}}{{ $first = false }}{{ end }}{{ end }}
        ],
```

- [ ] **Step 3: Verify the derivation with a temporary rule**

Add a rule to one endpoint in `endpoints.yaml` (`/api/projects`, `GET`):

```yaml
  roles:
    claim: resource_access.probe.roles
    any_of: [probe-role]
```

Run:
```bash
make gen
FC_ENABLE=1 FC_SETTINGS=config/settings FC_OUT=config/.r.json \
  ./scripts/krakend-check.sh config/krakend.tmpl >/dev/null
python3 -c "
import json
d=json.load(open('config/.r.json'))
print(json.dumps(d['extra_config']['plugin/http-server']['krakend-jwt-headers']['required_roles'], indent=2))
"
rm -f config/.r.json
```
Expected exactly:
```json
[
  {
    "claim": "resource_access.probe.roles",
    "path": "/api/projects",
    "roles": ["probe-role"]
  }
]
```

- [ ] **Step 4: Verify a path parameter becomes a single-segment glob**

Move the temporary rule to `/api/projects/{projectId}/stories` and re-run Step 3's
commands.
Expected `path`: `/api/projects/*/stories`.

- [ ] **Step 5: Remove the temporary rule and confirm the render returns to `[]`**

Run: `git checkout -- endpoints.yaml && make gen && git diff --exit-code config/settings/endpoints.json`
Expected: no diff, and the render shows `[]` again.

- [ ] **Step 6: Commit**

```bash
git add config/krakend.tmpl
git commit -m "feat(config): derive required_roles from the endpoint spec

The array came straight from jwt.json, where it was hand-maintained and empty.
It is now built from the endpoints that declare a roles block, reusing the same
path-to-glob transformation skip_paths uses two lines above, so the globs match
what the plugin's matchGlob expects. jwt.json's own list is concatenated for
rules tied to no declared endpoint.

Verified by render: empty while no endpoint declares a rule, and a rule on
/api/projects/{projectId}/stories renders as /api/projects/*/stories.

<attribution trailer per the convention above>"
```

---

### Task 3: per-rule claim and observation mode in the plugin

**Files:**
- Modify: `plugins/jwt-headers/main.go` (`roleRule`, `pluginConfig`, the gate, the call site at `:294`, replacing `hasRequiredRole` at `:385`)
- Modify: `config/krakend.tmpl` (render `roles_enforce`)
- Create: `plugins/jwt-headers/roles_test.go`

**Interfaces:**
- Consumes: `required_roles` entries shaped `{path, claim, roles}` from Task 2.
- Produces: `type roleDecision struct { matched, allowed bool; claim string; required, held []string }`; `func evaluateRoles(cfg *pluginConfig, claims jwt.MapClaims, path string) roleDecision`; `pluginConfig.RolesEnforce *bool` (nil or true enforces). `hasRequiredRole` is removed; its single call site is `main.go:294` and no test references it.

- [ ] **Step 1: Write the failing tests**

Create `plugins/jwt-headers/roles_test.go`:

```go
package main

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

// rolesConfig is defaultConfig with a role rule, and with required_claims
// relaxed to what the realm actually guarantees.
func rolesConfig(jwksURL string, rules ...map[string]interface{}) map[string]interface{} {
	cfg := defaultConfig(jwksURL)
	block := cfg[pluginName].(map[string]interface{})
	block["required_claims"] = []string{"sub"}
	block["required_roles"] = rules
	return cfg
}

func rule(path, claim string, roles ...string) map[string]interface{} {
	return map[string]interface{}{"path": path, "claim": claim, "roles": roles}
}

// captureLogs swaps the package logger for the duration of a test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := logger
	logger = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})).
		With("plugin", pluginName)
	t.Cleanup(func() { logger = prev })
	return &buf
}

func TestARoleHeldOnThePerRuleClaimAdmits(t *testing.T) {
	s := newSigner(t)
	defer s.shutdown()

	cfg := rolesConfig(s.jwksURL, rule("/api/projects", "resource_access.forgeos-api.roles", "user"))
	h, next := newHandler(t, cfg)

	claims := validClaims()
	claims["resource_access"] = map[string]any{
		"forgeos-api": map[string]any{"roles": []any{"user"}},
	}

	rec := do(h, http.MethodGet, "/api/projects", s.sign(t, claims))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %q", rec.Code, rec.Body.String())
	}
	if !next.called {
		t.Fatal("the request never reached the backend")
	}
}

func TestAMissingRoleIsForbidden(t *testing.T) {
	s := newSigner(t)
	defer s.shutdown()

	cfg := rolesConfig(s.jwksURL, rule("/api/projects", "resource_access.forgeos-api.roles", "admin"))
	h, next := newHandler(t, cfg)

	claims := validClaims()
	claims["resource_access"] = map[string]any{
		"forgeos-api": map[string]any{"roles": []any{"user"}},
	}

	rec := do(h, http.MethodGet, "/api/projects", s.sign(t, claims))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if next.called {
		t.Fatal("a request without the role reached the backend")
	}
	assertDenialHeaders(t, rec) // from denial_test.go
}

// A realm role must not open a route gated on a client role. This is the whole
// point of the per-rule claim: realm_access.roles is global by construction.
func TestARealmRoleDoesNotSatisfyAClientRoleRule(t *testing.T) {
	s := newSigner(t)
	defer s.shutdown()

	cfg := rolesConfig(s.jwksURL, rule("/api/projects", "resource_access.forgeos-api.roles", "admin"))
	h, _ := newHandler(t, cfg)

	claims := validClaims() // carries realm_access.roles = [admin, user]

	rec := do(h, http.MethodGet, "/api/projects", s.sign(t, claims))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 — a realm role satisfied a client rule", rec.Code)
	}
}

func TestAnEmptyRuleClaimFallsBackToTheGlobalRolesClaim(t *testing.T) {
	s := newSigner(t)
	defer s.shutdown()

	cfg := rolesConfig(s.jwksURL, rule("/api/projects", "", "admin"))
	h, _ := newHandler(t, cfg)

	// validClaims carries realm_access.roles = [admin, user]; roles_claim in
	// defaultConfig is realm_access.roles.
	rec := do(h, http.MethodGet, "/api/projects", s.sign(t, validClaims()))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; the global roles_claim was not used", rec.Code)
	}
}

func TestAClaimPathAbsentFromTheTokenIsForbidden(t *testing.T) {
	s := newSigner(t)
	defer s.shutdown()

	cfg := rolesConfig(s.jwksURL, rule("/api/projects", "resource_access.missing-api.roles", "user"))
	h, _ := newHandler(t, cfg)

	rec := do(h, http.MethodGet, "/api/projects", s.sign(t, validClaims()))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

// Review Focus 1: Keycloak can emit a scalar where an array is expected.
// extractStringSlice returns nil, so the request is denied — asserted so the
// outcome is a decision rather than an accident.
func TestAScalarRolesClaimIsForbiddenRatherThanParsedLoosely(t *testing.T) {
	s := newSigner(t)
	defer s.shutdown()

	cfg := rolesConfig(s.jwksURL, rule("/api/projects", "resource_access.forgeos-api.roles", "user"))
	h, _ := newHandler(t, cfg)

	claims := validClaims()
	claims["resource_access"] = map[string]any{
		"forgeos-api": map[string]any{"roles": "user"}, // string, not array
	}

	rec := do(h, http.MethodGet, "/api/projects", s.sign(t, claims))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a scalar roles claim", rec.Code)
	}
}

// Two rules on one glob are ordinary: GET and POST on the same path collapse to
// the same glob. The union is any-of, so either rule's role admits.
func TestTwoRulesOnOneGlobAreAnyOf(t *testing.T) {
	s := newSigner(t)
	defer s.shutdown()

	cfg := rolesConfig(s.jwksURL,
		rule("/api/projects", "resource_access.forgeos-api.roles", "reader"),
		rule("/api/projects", "resource_access.forgeos-api.roles", "writer"),
	)
	h, _ := newHandler(t, cfg)

	claims := validClaims()
	claims["resource_access"] = map[string]any{
		"forgeos-api": map[string]any{"roles": []any{"writer"}},
	}

	rec := do(h, http.MethodGet, "/api/projects", s.sign(t, claims))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; the second rule did not admit", rec.Code)
	}
}

// The permissive default is deliberate and must stay: completeness is enforced
// by CI, not by the runtime.
func TestAPathNoRuleMatchesIsStillAdmitted(t *testing.T) {
	s := newSigner(t)
	defer s.shutdown()

	cfg := rolesConfig(s.jwksURL, rule("/api/organizations", "resource_access.forgeos-api.roles", "admin"))
	h, _ := newHandler(t, cfg)

	rec := do(h, http.MethodGet, "/api/projects", s.sign(t, validClaims()))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a path no rule matches", rec.Code)
	}
}

// Review Focus 4: matchGlob splits on every separator, so a doubled one yields
// a segment count no rule matches and the rule does not apply. Asserted so the
// gap is known rather than discovered.
func TestADoubledSeparatorMatchesNoRule(t *testing.T) {
	s := newSigner(t)
	defer s.shutdown()

	cfg := rolesConfig(s.jwksURL, rule("/api/projects", "resource_access.forgeos-api.roles", "admin"))
	h, _ := newHandler(t, cfg)

	rec := do(h, http.MethodGet, "/api//projects", s.sign(t, validClaims()))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: a doubled separator now matches a rule. That is an "+
			"improvement — update this test and the non-goals in the ADR", rec.Code)
	}
}

func TestObservationModeAdmitsAndLogsWhatItWouldDeny(t *testing.T) {
	s := newSigner(t)
	defer s.shutdown()

	cfg := rolesConfig(s.jwksURL, rule("/api/projects", "resource_access.forgeos-api.roles", "admin"))
	cfg[pluginName].(map[string]interface{})["roles_enforce"] = false

	logs := captureLogs(t)
	h, next := newHandler(t, cfg)

	rec := do(h, http.MethodGet, "/api/projects", s.sign(t, validClaims()))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 in observation mode", rec.Code)
	}
	if !next.called {
		t.Fatal("observation mode blocked the request")
	}
	if !strings.Contains(logs.String(), `"msg":"role check would deny"`) {
		t.Fatalf("no observation line logged:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), `"enforced":false`) {
		t.Fatalf("the line does not record that enforcement was off:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), validClaims()["sub"].(string)) {
		t.Fatalf("the observation line leaked the subject:\n%s", logs.String())
	}
}

// Review Focus 2: anything other than an explicit false enforces. An absent
// key is the deployed default and must not open the edge.
func TestAnAbsentEnforceFlagEnforces(t *testing.T) {
	s := newSigner(t)
	defer s.shutdown()

	cfg := rolesConfig(s.jwksURL, rule("/api/projects", "resource_access.forgeos-api.roles", "admin"))
	// roles_enforce deliberately not set
	h, _ := newHandler(t, cfg)

	rec := do(h, http.MethodGet, "/api/projects", s.sign(t, validClaims()))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 — an absent roles_enforce did not enforce", rec.Code)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd plugins/jwt-headers && go test ./... -run 'Role|Claim|Observation|Enforce|Separator|Glob' -v`
Expected: FAIL — every rule is ignored because `roleRule` has no `claim` field and
`roles_enforce` is unknown, so the gate reads the global claim and enforces
always. Several tests fail on status mismatch, not on compilation.

- [ ] **Step 3: Add the claim field and the enforce flag**

In `plugins/jwt-headers/main.go`, replace the `roleRule` type:

```go
// roleRule gates a path glob on a claim. Claim is a dot-notation path and
// empty inherits cfg.RolesClaim; it exists so a rule can read client roles
// (resource_access.<client>.roles) rather than realm roles, which are global
// by construction and say nothing about which application the holder may enter.
type roleRule struct {
	Path  string   `json:"path"`
	Claim string   `json:"claim"`
	Roles []string `json:"roles"`
}
```

In `pluginConfig`, after `RequiredRoles`:

```go
	// RolesEnforce nil or true enforces. Only an explicit false observes:
	// the gate logs what it would have denied and admits the request, which is
	// how a deployment discovers missing role assignments from live traffic
	// instead of from an inventory someone assembled by hand.
	RolesEnforce *bool `json:"roles_enforce"`
```

- [ ] **Step 4: Replace the gate**

Delete `hasRequiredRole` (`main.go:383-405`) and put in its place:

```go
// roleDecision is what the gate concluded, kept separate from acting on it so
// observation mode can log a denial it is not going to apply.
type roleDecision struct {
	matched  bool
	allowed  bool
	claim    string
	required []string
	held     []string
}

// evaluateRoles returns allowed=true when no rule matches the path — the
// permissive default is deliberate, and completeness is enforced by
// scripts/check-endpoint-authorization.sh at build time rather than by a
// runtime deny that would turn an incomplete rule list into an outage.
func evaluateRoles(cfg *pluginConfig, claims jwt.MapClaims, path string) roleDecision {
	var worst roleDecision
	for _, r := range cfg.RequiredRoles {
		if !matchGlob(r.Path, path) {
			continue
		}
		claim := r.Claim
		if claim == "" {
			claim = cfg.RolesClaim
		}
		held := extractStringSlice(claims, claim)
		for _, h := range held {
			for _, want := range r.Roles {
				if h == want {
					return roleDecision{matched: true, allowed: true, claim: claim, required: r.Roles, held: held}
				}
			}
		}
		worst = roleDecision{matched: true, allowed: false, claim: claim, required: r.Roles, held: held}
	}
	if !worst.matched {
		return roleDecision{allowed: true}
	}
	return worst
}
```

- [ ] **Step 5: Replace the call site**

`main.go:293-298` currently reads:

```go
		// Enforce per-endpoint role requirements (RBAC at the edge).
		if !hasRequiredRole(cfg, claims, req.URL.Path) {
			logger.Warn("forbidden: missing required role", "path", req.URL.Path)
			deny(w, http.StatusForbidden, `{"message":"forbidden: missing required role"}`)
			return
		}
```

Replace with:

```go
		// Enforce per-endpoint role requirements (RBAC at the edge).
		if d := evaluateRoles(cfg, claims, req.URL.Path); d.matched && !d.allowed {
			if cfg.RolesEnforce != nil && !*cfg.RolesEnforce {
				logger.Warn("role check would deny",
					"path", req.URL.Path, "claim", d.claim,
					"required", d.required, "held", d.held, "enforced", false)
			} else {
				logger.Warn("forbidden: missing required role",
					"path", req.URL.Path, "claim", d.claim)
				deny(w, http.StatusForbidden, `{"message":"forbidden: missing required role"}`)
				return
			}
		}
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `cd plugins/jwt-headers && gofmt -l . && go vet ./... && go test ./... -v 2>&1 | tail -20`
Expected: PASS, all 22 pre-existing tests included.

- [ ] **Step 7: Render the flag from the environment**

In `config/krakend.tmpl`, immediately after the `"required_roles"` line from Task 2:

```
        {{/* Only an explicit "false" observes. Anything unrecognised — 1, yes,
             TRUE, empty — enforces, because the failure mode of guessing wrong
             here is an open edge. */}}
        "roles_enforce": {{ if eq (env "JWT_ROLES_ENFORCE" | default "true") "false" }}false{{ else }}true{{ end }},
```

- [ ] **Step 8: Verify both renders**

Run:
```bash
for v in "" true false yes 1 TRUE; do
  printf '%-6s -> ' "${v:-unset}"
  env ${v:+JWT_ROLES_ENFORCE=$v} FC_ENABLE=1 FC_SETTINGS=config/settings FC_OUT=config/.r.json \
    ./scripts/krakend-check.sh config/krakend.tmpl >/dev/null 2>&1
  python3 -c "
import json
print(json.load(open('config/.r.json'))['extra_config']['plugin/http-server']['krakend-jwt-headers']['roles_enforce'])
"
done
rm -f config/.r.json
```
Expected: `false` only for `false`; `true` for every other value including unset.

- [ ] **Step 9: Mutation-test the gate**

Each mutation must break at least one test. Apply, run, revert.

```bash
cd plugins/jwt-headers && cp main.go /tmp/jh.bak
```

| Mutation | Must break |
|---|---|
| `claim := r.Claim` → `claim := cfg.RolesClaim` | the per-rule claim tests |
| drop the `cfg.RolesEnforce != nil &&` conjunct | `TestAnAbsentEnforceFlagEnforces` |
| `worst = ...allowed: false...` → `allowed: true` | `TestAMissingRoleIsForbidden` |
| `if !worst.matched` → `if true` | `TestAMissingRoleIsForbidden` |
| observation branch returns `deny` as well | `TestObservationModeAdmitsAndLogsWhatItWouldDeny` |

Run after each: `go test ./... 2>&1 | grep -E '^(--- FAIL|ok|FAIL)'`, then
`cp /tmp/jh.bak main.go`. Finish with `rm /tmp/jh.bak && go test ./...`.

- [ ] **Step 10: Commit**

```bash
git add plugins/jwt-headers/main.go plugins/jwt-headers/roles_test.go config/krakend.tmpl
git commit -m "feat(jwt-headers): gate routes on client roles, with an observation mode

A rule may now name its own claim, which is what makes client roles usable:
resource_access.<client>.roles is per application, while realm_access.roles is
global by construction and says nothing about which application the holder may
enter. An empty rule claim inherits roles_claim.

JWT_ROLES_ENFORCE=false logs what the gate would have denied and admits the
request, so a deployment learns the missing role assignments from live traffic.
Only an explicit false observes: anything unrecognised enforces, because the
failure mode of guessing wrong is an open edge.

The permissive default for unmatched paths is kept on purpose — completeness is
a build failure, not a runtime deny that would turn an incomplete rule list
into an outage.

Five mutations applied, each caught.

<attribution trailer per the convention above>"
```

---

### Task 4: declare the rules on all 25 protected endpoints

**Files:**
- Modify: `endpoints.yaml` (17 `forgeos` endpoints, 8 `knowledge` endpoints)
- Modify: `config/settings/endpoints.json` (regenerated, never hand-edited)
- Create: `docs/adr/0006-per-app-authorization-at-the-edge.md`
- Modify: `docs/adr/README.md` (index row)

**Interfaces:**
- Consumes: the `roles` / `roles_waiver` schema from Task 1, the derivation from Task 2, the gate from Task 3.
- Produces: a non-empty `required_roles` array in the rendered config, and the ADR that Task 5's guard cites in its failure message.

Measured 2026-10-09: `forgeos` has 17 protected endpoints and 1 public,
`knowledge` has 8 protected, `platform` has 5 and all are public. So there are
two application surfaces to decide and no waiver case.

- [ ] **Step 0: Confirm observation mode actually reaches the gateway**

Added after review. `JWT_ROLES_ENFORCE` was missing from
`docker-compose.yml`'s `environment:` allowlist, and the image renders its
config inside the container, so the flag was accepted on the command line and
did nothing — the observation step of this rollout would have enforced in
silence and taken both product surfaces down. Fixed, and
`scripts/check-template-env.sh` now fails the build if any template variable
becomes unreachable again. Verify before trusting step 6:

```bash
JWT_ROLES_ENFORCE=false docker compose up -d gateway
make logs 2>&1 | grep '"roles_enforce"'
```

- [ ] **Step 1: Obtain the client ids and role names from the realm**

This is the one input this repository cannot derive. Ask for it, or read it:

```bash
KC=http://localhost:8082   # the codehunters realm; :8081 is forgeos, :8083 is unserved
TOK=$(curl -s -d client_id=admin-cli -d username=admin -d "password=$KC_ADMIN_PASSWORD" \
  -d grant_type=password $KC/realms/master/protocol/openid-connect/token \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["access_token"])')
curl -s -H "Authorization: Bearer $TOK" "$KC/admin/realms/codehunters/clients" \
  | python3 -c 'import sys,json;[print(c["clientId"]) for c in json.load(sys.stdin)]'
```

For each client that owns a surface, its roles:

```bash
curl -s -H "Authorization: Bearer $TOK" \
  "$KC/admin/realms/codehunters/clients/<id>/roles" \
  | python3 -c 'import sys,json;[print(r["name"]) for r in json.load(sys.stdin)]'
```

Record the two client ids and their role names in the ADR's *Confirmation*
before editing YAML. **Do not invent them**: a rule naming a client that does
not exist denies every request once enforcement is on, and the claim path is
not validated against the realm by anything in CI.

- [ ] **Step 2: Add the rule to every protected `forgeos` endpoint**

For each of the 17, append the block below the existing fields. Worked example,
`GET /api/projects`:

```yaml
  - path: /api/projects
    method: GET
    backend: forgeos
    product: forgeos
    auth: protected
    input_headers: *protected
    roles:
      claim: resource_access.<forgeos-client-id>.roles
      any_of: [<role>, ...]
```

The remaining 16 take the identical `roles` block — one surface, one decision.
Leave `/api/ping` alone: it is `auth: public` and Task 1's validation rejects a
rule there.

- [ ] **Step 3: Add the rule to every protected `knowledge` endpoint**

The same, for the 8 `knowledge` endpoints, with that surface's client id and
roles:

```yaml
    roles:
      claim: resource_access.<knowledge-client-id>.roles
      any_of: [<role>, ...]
```

- [ ] **Step 4: Regenerate and verify the rule count**

Run:
```bash
make gen
python3 -c "
import json
eps = json.load(open('config/settings/endpoints.json'))['endpoints']
with_rules = [e for e in eps if e.get('roles')]
protected  = [e for e in eps if e.get('auth') == 'protected']
print('protected:', len(protected), 'with a rule:', len(with_rules))
assert len(with_rules) == len(protected) == 25, 'every protected endpoint needs a rule'
print('OK')
"
```
Expected: `protected: 25 with a rule: 25` then `OK`.

- [ ] **Step 5: Verify the rendered rule list**

Run:
```bash
FC_ENABLE=1 FC_SETTINGS=config/settings FC_OUT=config/.r.json \
  ./scripts/krakend-check.sh config/krakend.tmpl >/dev/null
python3 -c "
import json
rules = json.load(open('config/.r.json'))['extra_config']['plugin/http-server']['krakend-jwt-headers']['required_roles']
print(len(rules), 'rules')
globs = {}
for r in rules:
    globs.setdefault(r['path'], set()).add((r['claim'], tuple(r['roles'])))
for g, variants in sorted(globs.items()):
    if len(variants) > 1:
        print('CONFLICT', g, variants)
print('distinct globs:', len(globs))
"
rm -f config/.r.json
```
Expected: 25 rules, no `CONFLICT` line. Fewer distinct globs than rules is
normal — `GET` and `POST` on one path collapse.

- [ ] **Step 6: Deploy in observation mode and read the log**

Not a code step, and the one that cannot be skipped. With
`JWT_ROLES_ENFORCE=false`, exercise the front end and watch:

```bash
make logs 2>&1 | grep 'role check would deny'
```

Every line is a role assignment missing in the realm. Fix assignments until the
lines stop, then set `JWT_ROLES_ENFORCE=true`. Landing enforcement before this
is silent is what locks out a product.

- [ ] **Step 7: Write ADR-0006**

Re-list the directory first — the number must not collide:

```bash
ls docs/adr/*.md | sort | tail -3
```

Create `docs/adr/0006-per-app-authorization-at-the-edge.md`, MADR, frontmatter
`status: proposed`, `date: 2026-10-09`,
`decision-makers: [Carlos Andres Montoya Tobon]`. It records, with the measured
numbers from this task:

- **Context**: `aud` was never validated and `required_roles` was empty, so any
  token the realm signed opened all 25 protected routes.
- **Decision**: client roles per application surface, declared on the endpoint,
  derived into the plugin config.
- **Drivers**: one source of truth with the routes; a coarse gate at the edge
  because per-resource authorization belongs next to the domain rule.
- **Considered**: `aud`/`azp` audience restriction (deferred, with the Keycloak
  audience-mapper reason); realm roles (rejected — global by construction);
  per-method rules (rejected as a non-goal, with the glob-collapse consequence).
- **Consequences**: the permissive runtime default plus a build-time
  completeness guard, and why that pairing rather than a runtime deny.
- **Confirmation**: `scripts/check-endpoint-authorization.sh` in `make check`,
  the `plugins/jwt-headers` role suite, the generator validation, and the two
  client ids and role names recorded from Step 1.

Add the index row to `docs/adr/README.md`.

- [ ] **Step 8: Commit**

```bash
git add endpoints.yaml config/settings/endpoints.json docs/adr/
git commit -m "feat(endpoints): gate all 25 protected routes on client roles

Two application surfaces, two decisions, 25 rows: forgeos (17 protected
endpoints) and knowledge (8). platform's five are all public, so no waiver case
arises — measured, not assumed.

Every rule names resource_access.<client>.roles for the client that owns the
surface. Realm roles would not do: they are global by construction and say
nothing about which application the holder may enter.

ADR-0006 records the decision, what was considered, and the client ids and role
names this depends on.

<attribution trailer per the convention above>"
```

---

### Task 5: the CI guard

**Files:**
- Create: `scripts/check-endpoint-authorization.sh`
- Modify: `Makefile` (`check` target, new `authz-check` target, `.PHONY`)
- Modify: `README.md` (targets table, the validation-layers list — it becomes seven)

**Interfaces:**
- Consumes: `endpoints.yaml`, and `docs/adr/0006-...` by name in its failure message.
- Produces: `make authz-check`, and a seventh layer inside `make check`.

It lands **last on purpose**: it fails while any protected endpoint has no rule,
so it can only be green once Task 4 has populated them.

- [ ] **Step 1: Write the guard**

Create `scripts/check-endpoint-authorization.sh`:

```bash
#!/usr/bin/env bash
# Fails when a protected endpoint carries no authorization decision.
#
# The mechanism this guards was present and unused for months:
# plugins/jwt-headers has always matched required_roles against the request
# path, and config/settings/jwt.json declared an empty list, so every protected
# route admitted any token the realm signed. Nothing failed, because an empty
# rule list is valid configuration. The gap closes only if adding a route
# without a decision is a red pull request.
#
# The runtime stays permissive on purpose (see ADR-0006): a deny-by-default
# gate with an incomplete rule list is an outage, while a permissive gate with a
# complete list is the same posture reached safely. This script is what makes
# the list complete.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SPEC="$ROOT_DIR/endpoints.yaml"

if ! command -v python3 >/dev/null 2>&1; then
  echo "check-endpoint-authorization: python3 is required but not found on PATH" >&2
  exit 1
fi

python3 - "$SPEC" <<'PY'
import re
import sys

import yaml

CLAIM = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z0-9_-]+)+$")

spec = yaml.safe_load(open(sys.argv[1]))
endpoints = spec["endpoints"]
problems = []

def glob_of(endpoint):
    # The same transformation config/krakend.tmpl applies -- INCLUDING the
    # product prefix, which the template picks up because it derives from
    # endpoints.json (post-Normalize) while this script reads endpoints.yaml
    # (pre-Normalize). Comparing unprefixed paths would analyse strings the
    # plugin never sees: every product currently declares prefix "", so the
    # mistake would pass by luck and break on the first prefixed product.
    prefix = (spec.get("products", {}).get(endpoint.get("product"), {}) or {}).get("prefix", "")
    return re.sub(r"\{[^}]+\}", "*", prefix + endpoint["path"])

rules = {}  # glob -> {(claim, roles)} , plus where each came from
for e in endpoints:
    where = f"{e.get('method','?')} {e.get('path','?')}"
    roles, waiver = e.get("roles"), e.get("roles_waiver")

    if e.get("auth") != "protected":
        if roles or waiver:
            problems.append(f"{where}: public endpoint carries an authorization rule")
        continue

    if roles and waiver:
        problems.append(f"{where}: declares both roles and roles_waiver")
        continue
    if not roles and not waiver:
        problems.append(f"{where}: protected with no roles block and no roles_waiver")
        continue
    if waiver:
        if not str(waiver).strip():
            problems.append(f"{where}: roles_waiver needs a reason")
        continue

    any_of = roles.get("any_of") or []
    claim = roles.get("claim") or ""
    if not any_of:
        problems.append(f"{where}: roles.any_of lists no role")
    if claim and not CLAIM.match(claim):
        problems.append(f"{where}: roles.claim {claim!r} is not a dotted claim path")

    rules.setdefault(glob_of(e), set()).add((claim, tuple(sorted(any_of))))

# Two rules on one glob are ordinary — GET and POST on a path collapse to the
# same glob. Two DIFFERENT rules on one glob are not: the plugin unions the
# matching rules and any-of means the more permissive silently wins.
for glob, variants in sorted(rules.items()):
    if len(variants) > 1:
        problems.append(f"{glob}: conflicting rules on one glob: {sorted(variants)}")

# Review Focus 3: subsumption. A literal rule and a globbed one are different
# globs that can still match the same request, and the union grants the more
# permissive of the two. Identical rules are harmless; differing ones are the
# same silent widening as above, one step less visible.
def matches(pattern, concrete):
    p, c = pattern.strip("/").split("/"), concrete.strip("/").split("/")
    if len(p) != len(c):
        return False
    return all(a == "*" or a == b for a, b in zip(p, c))

globs = sorted(rules)
for a in globs:
    for b in globs:
        if a == b or "*" not in b:
            continue
        if matches(b, a) and rules[a] != rules[b]:
            problems.append(
                f"{a}: also matched by {b}, which declares a different rule — "
                "the union grants the more permissive of the two"
            )

if problems:
    print("check-endpoint-authorization: FAILED", file=sys.stderr)
    for p in problems:
        print(f"    {p}", file=sys.stderr)
    print("  Every protected endpoint needs an authorization decision: a roles", file=sys.stderr)
    print("  block, or a roles_waiver saying why it has none. See", file=sys.stderr)
    print("  docs/adr/0006-per-app-authorization-at-the-edge.md", file=sys.stderr)
    raise SystemExit(1)

protected = sum(1 for e in endpoints if e.get("auth") == "protected")
waived = sum(1 for e in endpoints if e.get("auth") == "protected" and e.get("roles_waiver"))
print(
    f"check-endpoint-authorization: OK ({protected} protected endpoints, "
    f"{protected - waived} gated on roles, {waived} waived)"
)
PY
```

Then: `chmod +x scripts/check-endpoint-authorization.sh`

- [ ] **Step 2: Run it against the populated spec**

Run: `./scripts/check-endpoint-authorization.sh`
Expected: `check-endpoint-authorization: OK (25 protected endpoints, 25 gated on roles, 0 waived)`

- [ ] **Step 3: Mutation-test the guard**

Each must fail; revert after each with `git checkout -- endpoints.yaml`.

| Mutation | Expected failure |
|---|---|
| delete one endpoint's `roles` block | `protected with no roles block and no roles_waiver` |
| set that endpoint's `any_of` to `[]` | `roles.any_of lists no role` |
| set its `claim` to `roles` | `not a dotted claim path` |
| add `roles_waiver: "x"` beside an existing `roles` | `declares both` |
| add `roles_waiver: ""` and remove `roles` | `roles_waiver needs a reason` |
| change one of two same-path rules' `any_of` | `conflicting rules on one glob` |
| add a protected literal `/api/projects/mine` with a different rule | `also matched by /api/projects/*` |
| copy a `roles` block onto the public `/api/ping` | `public endpoint carries an authorization rule` |

Run after each: `./scripts/check-endpoint-authorization.sh; echo "exit=$?"`

- [ ] **Step 4: Wire it into `make check`**

In `Makefile`, inside the `check` target, after the orphan-key guard:

```make
	@./scripts/check-endpoint-authorization.sh
```

And a standalone target beside `settings-check`:

```make
authz-check: ## Fail if a protected endpoint declares no authorization decision
	./scripts/check-endpoint-authorization.sh
```

Add `authz-check` to `.PHONY`.

- [ ] **Step 5: Verify the whole gate**

Run: `make check && make plugins-test`
Expected: all seven layers green, five plugin modules green.

- [ ] **Step 6: Update the README**

Add the target row after `settings-check`:

```
| `make authz-check` | Falla si un endpoint `protected` no declara decision de autorizacion |
```

In the validation-layers list, which becomes seven, insert after the orphan-key
layer:

```
5. **`scripts/check-endpoint-authorization.sh`** — ningun endpoint `protected`
   sin decision de autorizacion.
```

Renumber the two that follow (`krakend check`, chain order) and update both
"Las seis" and "capas 3 a 6" to match.

- [ ] **Step 7: Commit**

```bash
git add scripts/check-endpoint-authorization.sh Makefile README.md
git commit -m "ci: fail when a protected endpoint declares no authorization

The mechanism this guards sat present and unused: jwt-headers has always
matched required_roles against the path, jwt.json declared an empty list, and
an empty list is valid configuration — so nothing failed while every protected
route admitted any token the realm signed.

The guard also catches the two ways a rule widens access silently. Two rules on
one glob is ordinary, because GET and POST on a path collapse; two DIFFERENT
rules on one glob is not, since the plugin unions them and any-of grants the
more permissive. The same holds one step less visibly for a literal path that
another rule's glob also matches, which is checked by subsumption rather than
by comparing globs for equality.

Eight mutations applied, each caught.

<attribution trailer per the convention above>"
```

---

## Self-review

**Spec coverage.** Spec §1 → Task 1. §2 → Task 2. §3 → Task 3 steps 3-6. §4
(build-time completeness) → Task 5. §5 (observation mode) → Task 3 steps 7-8.
§6 (rollout) → Task 4 step 6. Confirmation bullets → Task 5 step 1, Task 3 step
1, Task 1 step 1, and the mutation tables in Tasks 3 and 5. Rejected `aud`/`azp`
→ recorded in ADR-0006, Task 4 step 7. Deferred gateway-per-realm → not in this
plan by design; it is a separate spec.

**Type consistency.** `RoleRule{Claim, AnyOf}` (generator, YAML/JSON) and
`roleRule{Path, Claim, Roles}` (plugin) are deliberately different shapes: the
generator describes an endpoint's decision, the plugin consumes a path rule, and
the template at Task 2 converts one into the other — `any_of` becomes `roles`
and `path` is added as a glob. `evaluateRoles` and `roleDecision` are named
identically in Task 3's steps and in its tests. `RolesEnforce *bool` is read
only at the call site in step 5.

**Two spec claims corrected while writing this plan**, both by measurement: the
spec's open item about `platform`'s protected route — `platform` has five
endpoints and all are public, so no waiver arises — and its risk line about
"25 decisions", which is two decisions applied to 25 rows under a coarse gate.

**Review Focus coverage.** (1) scalar claim → Task 3 step 1. (2) unrecognised
`JWT_ROLES_ENFORCE` → Task 3 steps 1 and 8. (3) subsumed literal rule → Task 5
steps 1 and 3. (4) doubled separator → Task 3 step 1. (5) rule on a public
endpoint → Task 1 step 1 and Task 5 step 3.
