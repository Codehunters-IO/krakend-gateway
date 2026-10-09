package main

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

// rolesConfig is defaultConfig with role rules, and with required_claims
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

func clientRoles(client string, roles ...string) map[string]any {
	asAny := make([]any, 0, len(roles))
	for _, r := range roles {
		asAny = append(asAny, r)
	}
	return map[string]any{client: map[string]any{"roles": asAny}}
}

func TestARoleHeldOnThePerRuleClaimAdmits(t *testing.T) {
	s := newSigner(t)
	defer s.shutdown()

	cfg := rolesConfig(s.jwksURL, rule("/api/projects", "resource_access.forgeos-api.roles", "user"))
	h, next := newHandler(t, cfg)

	claims := validClaims()
	claims["resource_access"] = clientRoles("forgeos-api", "user")

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
	claims["resource_access"] = clientRoles("forgeos-api", "user")

	rec := do(h, http.MethodGet, "/api/projects", s.sign(t, claims))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if next.called {
		t.Fatal("a request without the role reached the backend")
	}
	assertDenialHeaders(t, rec)
}

// A realm role must not open a route gated on a client role. This is the whole
// point of the per-rule claim: realm_access.roles is global by construction.
func TestARealmRoleDoesNotSatisfyAClientRoleRule(t *testing.T) {
	s := newSigner(t)
	defer s.shutdown()

	cfg := rolesConfig(s.jwksURL, rule("/api/projects", "resource_access.forgeos-api.roles", "admin"))
	h, _ := newHandler(t, cfg)

	rec := do(h, http.MethodGet, "/api/projects", s.sign(t, validClaims()))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 — a realm role satisfied a client rule", rec.Code)
	}
}

func TestAnEmptyRuleClaimFallsBackToTheGlobalRolesClaim(t *testing.T) {
	s := newSigner(t)
	defer s.shutdown()

	cfg := rolesConfig(s.jwksURL, rule("/api/projects", "", "admin"))
	h, _ := newHandler(t, cfg)

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
	claims["resource_access"] = clientRoles("forgeos-api", "writer")

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
	if sub, ok := validClaims()["sub"].(string); ok && strings.Contains(logs.String(), sub) {
		t.Fatalf("the observation line leaked the subject:\n%s", logs.String())
	}
}

// Review Focus 2: anything other than an explicit false enforces. An absent
// key is the deployed default and must not open the edge.
func TestAnAbsentEnforceFlagEnforces(t *testing.T) {
	s := newSigner(t)
	defer s.shutdown()

	cfg := rolesConfig(s.jwksURL, rule("/api/projects", "resource_access.forgeos-api.roles", "admin"))
	h, _ := newHandler(t, cfg)

	rec := do(h, http.MethodGet, "/api/projects", s.sign(t, validClaims()))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 — an absent roles_enforce did not enforce", rec.Code)
	}
}

// I1: the rollout's only data source is the observation log. With two rules
// matching one request and the token satisfying neither, BOTH must be reported
// — an operator who sees only one grants a role on the wrong client, which
// does unblock the route, because the union is any-of.
func TestObservationModeReportsEveryFailingRule(t *testing.T) {
	s := newSigner(t)
	defer s.shutdown()

	cfg := rolesConfig(s.jwksURL,
		rule("/api/projects/mine", "resource_access.forgeos-api.roles", "owner"),
		rule("/api/projects/*", "resource_access.knowledge-api.roles", "reader"),
	)
	cfg[pluginName].(map[string]interface{})["roles_enforce"] = false

	logs := captureLogs(t)
	h, _ := newHandler(t, cfg)

	do(h, http.MethodGet, "/api/projects/mine", s.sign(t, validClaims()))

	out := logs.String()
	if !strings.Contains(out, "resource_access.forgeos-api.roles") {
		t.Errorf("the rule that owns the literal path was not reported:\n%s", out)
	}
	if !strings.Contains(out, "resource_access.knowledge-api.roles") {
		t.Errorf("the globbed rule was not reported:\n%s", out)
	}
}

// M1 + M3: an enforced denial has to be diagnosable, and the field name is the
// one the spec documents.
func TestAnEnforcedDenialLogsTheRuleAndWhatTheTokenHeld(t *testing.T) {
	s := newSigner(t)
	defer s.shutdown()

	cfg := rolesConfig(s.jwksURL, rule("/api/projects", "resource_access.forgeos-api.roles", "admin"))
	logs := captureLogs(t)
	h, _ := newHandler(t, cfg)

	rec := do(h, http.MethodGet, "/api/projects", s.sign(t, validClaims()))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	out := logs.String()
	for _, want := range []string{`"required":["admin"]`, `"token_roles"`, `"claim":"resource_access.forgeos-api.roles"`} {
		if !strings.Contains(out, want) {
			t.Errorf("the denial line lacks %s:\n%s", want, out)
		}
	}
}

// I3: a roles_enforce that is not a boolean must fail the load. It does, and
// pinning it matters because the alternative — loosening the field to
// interface{} to "fix" the parse error — would leave the gateway serving with
// no jwt-headers at all, which is this repository's documented behaviour for a
// plugin that refuses its own config.
func TestANonBooleanEnforceFlagFailsTheLoad(t *testing.T) {
	cfg := defaultConfig("http://127.0.0.1:1/certs")
	cfg[pluginName].(map[string]interface{})["roles_enforce"] = "nope"

	_, err := registerer(pluginName).registerHandlers(context.Background(), cfg, http.NotFoundHandler())
	if err == nil {
		t.Fatal("a non-boolean roles_enforce must fail the load, not default silently")
	}
}

// I4: "*" is ONE segment to the role gate, and the rest of the path to the
// skip-path matcher. Both lists are derived from the same transformation, so
// the asymmetry has to be visible somewhere. It is visible here.
func TestAWildcardMatchesExactlyOneSegment(t *testing.T) {
	s := newSigner(t)
	defer s.shutdown()

	cfg := rolesConfig(s.jwksURL, rule("/api/*", "resource_access.forgeos-api.roles", "admin"))
	h, _ := newHandler(t, cfg)

	// Two segments: the rule applies, and the token lacks the role.
	if rec := do(h, http.MethodGet, "/api/projects", s.sign(t, validClaims())); rec.Code != http.StatusForbidden {
		t.Fatalf("/api/projects: status = %d, want 403 — the rule did not apply", rec.Code)
	}

	// Three segments: no rule matches, so the permissive default admits. This is
	// why /api/* is not a blanket gate for a surface, unlike in skip_paths.
	if rec := do(h, http.MethodGet, "/api/projects/P-1", s.sign(t, validClaims())); rec.Code != http.StatusOK {
		t.Fatalf("/api/projects/P-1: status = %d, want 200 — a one-segment wildcard "+
			"now spans several segments. If that was deliberate, it is a semantic "+
			"change the ADR must carry", rec.Code)
	}
}
