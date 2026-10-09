package main

import (
	"strings"
	"testing"
)

func base() Spec {
	return Spec{
		Backends: map[string]Backend{"forgeos": {HostDefault: "http://x:8080", HostEnv: "FORGEOS_HOST"}},
		Endpoints: []Endpoint{
			{Path: "/api/ping", Method: "GET", Backend: "forgeos", Auth: "public", InputHeaders: []string{"Accept"}},
		},
	}
}

func TestValidate_OK(t *testing.T) {
	if errs := Validate(base()); len(errs) != 0 {
		t.Fatalf("want no errors, got %v", errs)
	}
}

func TestValidate_BadPath(t *testing.T) {
	s := base()
	s.Endpoints[0].Path = "api/ping" // missing leading slash
	assertErrContains(t, Validate(s), "invalid path")
}

func TestValidate_BadMethod(t *testing.T) {
	s := base()
	s.Endpoints[0].Method = "FETCH"
	assertErrContains(t, Validate(s), "unknown method")
}

func TestValidate_UnknownBackend(t *testing.T) {
	s := base()
	s.Endpoints[0].Backend = "ghost"
	assertErrContains(t, Validate(s), `backend "ghost"`)
}

func TestValidate_DuplicateEndpoint(t *testing.T) {
	s := base()
	s.Endpoints = append(s.Endpoints, s.Endpoints[0])
	assertErrContains(t, Validate(s), "duplicate endpoint")
}

func TestValidate_EmptyHeaders(t *testing.T) {
	s := base()
	s.Endpoints[0].InputHeaders = nil
	assertErrContains(t, Validate(s), "input_headers required")
}

func TestValidate_BadAuth(t *testing.T) {
	s := base()
	s.Endpoints[0].Auth = "maybe"
	assertErrContains(t, Validate(s), "invalid auth")
}

func withProducts() Spec {
	s := base()
	s.Products = map[string]Product{
		"forgeos": {Prefix: "", Backend: "forgeos"},
		"vitxo":   {Prefix: "/vitxo", Backend: "forgeos"},
	}
	s.Endpoints[0].Product = "forgeos"
	s.Endpoints[0].Backend = ""
	return s
}

func TestValidate_ProductsOK(t *testing.T) {
	if errs := Validate(withProducts()); len(errs) != 0 {
		t.Fatalf("want no errors, got %v", errs)
	}
}

func TestValidate_UnknownProduct(t *testing.T) {
	s := withProducts()
	s.Endpoints[0].Product = "nope"
	assertErrContains(t, Validate(s), `product "nope" not declared`)
}

// Review Focus 5: products present, endpoint omits product.
func TestValidate_ProductRequiredWhenProductsDeclared(t *testing.T) {
	s := withProducts()
	s.Endpoints[0].Product = ""
	assertErrContains(t, Validate(s), "product required")
}

// Review Focus 3: a trailing slash would generate //api/...
func TestValidate_PrefixTrailingSlash(t *testing.T) {
	s := withProducts()
	s.Products["vitxo"] = Product{Prefix: "/vitxo/", Backend: "forgeos"}
	assertErrContains(t, Validate(s), "invalid prefix")
}

func TestValidate_PrefixMissingLeadingSlash(t *testing.T) {
	s := withProducts()
	s.Products["vitxo"] = Product{Prefix: "vitxo", Backend: "forgeos"}
	assertErrContains(t, Validate(s), "invalid prefix")
}

func TestValidate_EmptyPrefixIsValid(t *testing.T) {
	s := withProducts()
	s.Products["vitxo"] = Product{Prefix: "", Backend: "forgeos"}
	if errs := Validate(s); len(errs) != 0 {
		t.Fatalf("empty prefix must be valid, got %v", errs)
	}
}

func TestValidate_ProductSuppliesBackend(t *testing.T) {
	s := withProducts()
	s.Endpoints[0].Backend = "" // resolved from the product
	if errs := Validate(s); len(errs) != 0 {
		t.Fatalf("product backend must satisfy the backend rule, got %v", errs)
	}
}

func TestValidate_ProductBackendMustExist(t *testing.T) {
	s := withProducts()
	s.Products["forgeos"] = Product{Prefix: "", Backend: "ghost"}
	s.Endpoints[0].Backend = ""
	assertErrContains(t, Validate(s), `backend "ghost" not declared`)
}

// Back-compat: no products block at all stays valid.
func TestValidate_NoProductsBlockStillValid(t *testing.T) {
	if errs := Validate(base()); len(errs) != 0 {
		t.Fatalf("spec without products must stay valid, got %v", errs)
	}
}

// Verifies that a genuine collision (equal prefixes, same path) is still detected.
// Both endpoints resolve to the same exposed path here, so this passes identically
// with and without the prefix in the duplicate key. The differential guard for
// prefix-aware logic is TestValidate_SamePathDifferentPrefixIsNotCollision.
func TestValidate_CrossProductPathCollision(t *testing.T) {
	s := withProducts()
	s.Products["vitxo"] = Product{Prefix: "", Backend: "forgeos"} // same namespace as forgeos
	s.Endpoints = append(s.Endpoints, Endpoint{
		Path: "/api/ping", Method: "GET", Product: "vitxo", Auth: "public",
		InputHeaders: []string{"Accept"},
	})
	assertErrContains(t, Validate(s), "duplicate endpoint")
}

func TestValidate_SamePathDifferentPrefixIsNotCollision(t *testing.T) {
	s := withProducts()
	s.Endpoints = append(s.Endpoints, Endpoint{
		Path: "/api/ping", Method: "GET", Product: "vitxo", Auth: "public",
		InputHeaders: []string{"Accept"},
	})
	if errs := Validate(s); len(errs) != 0 {
		t.Fatalf("/api/ping and /vitxo/api/ping must coexist, got %v", errs)
	}
}

// Guards the coupling Important 3 (final fix wave) removed: before the
// exposedPath extraction, Validate's collision key and Normalize's Path were
// two independent derivations that only agreed by construction (one guarded
// on `ok`, the other relied on a missing-key zero value). This drives both
// real entry points — not a reimplementation of the formula — from one spec
// with a non-empty prefix, so it fails if either call site stops deriving
// the exposed path the same way as the other.
func TestValidateAndNormalize_AgreeOnExposedPath(t *testing.T) {
	distinct := withProducts()
	distinct.Products["vitxo"] = Product{Prefix: "/vitxo", Backend: "forgeos"}
	distinct.Endpoints = append(distinct.Endpoints, Endpoint{
		Path: "/api/ping", Method: "GET", Product: "vitxo", Auth: "public",
		InputHeaders: []string{"Accept"},
	})
	if errs := Validate(distinct); len(errs) != 0 {
		t.Fatalf("different prefixes must not collide, got %v", errs)
	}
	norm := Normalize(distinct)
	if norm[0].Path == norm[1].Path {
		t.Fatalf("Normalize collapsed two endpoints Validate treated as distinct: both %q", norm[0].Path)
	}

	colliding := withProducts()
	colliding.Products["vitxo"] = Product{Prefix: "", Backend: "forgeos"} // same namespace as forgeos
	colliding.Endpoints = append(colliding.Endpoints, Endpoint{
		Path: "/api/ping", Method: "GET", Product: "vitxo", Auth: "public",
		InputHeaders: []string{"Accept"},
	})
	assertErrContains(t, Validate(colliding), "duplicate endpoint")
	normColliding := Normalize(colliding)
	if normColliding[0].Path != normColliding[1].Path {
		t.Fatalf("Validate flagged a collision Normalize does not reproduce: %q vs %q",
			normColliding[0].Path, normColliding[1].Path)
	}
}

func assertErrContains(t *testing.T, errs []error, want string) {
	t.Helper()
	for _, e := range errs {
		if strings.Contains(e.Error(), want) {
			return
		}
	}
	t.Fatalf("want an error containing %q, got %v", want, errs)
}

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

// Superseded by TestValidate_ProtectedRolesMustNameTheirClaim below. An
// omitted claim inherits realm_access.roles, so the fallback the spec allowed
// turned out to be a rule that gates nothing; the fallback survives only for
// jwt.json's own literals, which this validator does not see.
func TestValidate_EmptyClaimIsStillAllowedOnAPublicFacingFallback(t *testing.T) {
	s := protectedBase()
	s.Endpoints[0].Auth = "protected"
	s.Endpoints[0].Roles = &RoleRule{Claim: "resource_access.api.roles", AnyOf: []string{"user"}}
	if errs := Validate(s); len(errs) != 0 {
		t.Fatalf("a named client claim must validate, got %v", errs)
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

// I2: an omitted claim falls back to realm_access.roles, which is global by
// construction. A rule that asks for a realm role gates nothing while reading
// as "authorization added", so a protected endpoint must name its claim.
func TestValidate_ProtectedRolesMustNameTheirClaim(t *testing.T) {
	s := protectedBase()
	s.Endpoints[0].Roles = &RoleRule{AnyOf: []string{"user"}}
	assertErrContains(t, Validate(s), "roles.claim is required")
}

// I5: Keycloak allows dots in a clientId, and the extractor walks the claim
// path by splitting on dots with no escape, so resource_access.my.app.roles is
// read as five nested maps and denies every request. The config is accepted by
// the regex, renders correctly, and locks the product out.
func TestValidate_ADottedClientIdIsRejectedWithItsReason(t *testing.T) {
	s := protectedBase()
	s.Endpoints[0].Roles = &RoleRule{Claim: "resource_access.my.app.roles", AnyOf: []string{"user"}}
	assertErrContains(t, Validate(s), "client id containing a dot")
}

func TestValidate_AThreeSegmentResourceAccessClaimIsAccepted(t *testing.T) {
	s := protectedBase()
	s.Endpoints[0].Roles = &RoleRule{Claim: "resource_access.forgeos-api.roles", AnyOf: []string{"user"}}
	if errs := Validate(s); len(errs) != 0 {
		t.Fatalf("want no errors for a normal client claim, got %v", errs)
	}
}
