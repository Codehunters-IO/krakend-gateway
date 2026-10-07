package main

// Tests for the JWT validation path — the code that decides who gets through
// the edge. Before this file the package had two tests, both for the path
// matcher, so signature verification, issuer checking, algorithm restriction,
// required claims and claims-to-headers mapping had no coverage at all.
//
// Everything is driven through registerHandlers, the real entry point, against
// a JWKS served by httptest. Nothing here reimplements the plugin's logic.

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const testKeyID = "test-key-1"

// testIssuer is both the configured issuer and the `iss` the fixtures mint,
// so a mismatch in a test is always deliberate.
const testIssuer = "https://idp.test/realms/forgeos"

// signer mints tokens for the tests and exposes the JWKS the plugin fetches.
type signer struct {
	key      *rsa.PrivateKey
	jwksURL  string
	shutdown func()
}

// newSigner generates a key pair and serves its public half as a JWK Set.
// The JWK is assembled by hand rather than with a library so the test adds no
// dependency the plugin does not already have.
func newSigner(t *testing.T) *signer {
	return newSignerWithJWKS(t, false)
}

// newSignerWithJWKS can publish a JWK without its optional `alg` member, which
// is what makes the algorithm allowlist observable (see the PS256 subtest).
func newSignerWithJWKS(t *testing.T, omitAlg bool) *signer {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}

	jwks := fmt.Sprintf(`{"keys":[{"kty":"RSA","use":"sig","alg":"RS256","kid":%q,"n":%q,"e":%q}]}`,
		testKeyID,
		base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
	)
	if omitAlg {
		jwks = fmt.Sprintf(`{"keys":[{"kty":"RSA","use":"sig","kid":%q,"n":%q,"e":%q}]}`,
			testKeyID,
			base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(jwks))
	}))

	return &signer{key: key, jwksURL: srv.URL, shutdown: srv.Close}
}

// sign mints an RS256 token with the given claims under the published kid.
func (s *signer) sign(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	return s.signWithKid(t, claims, testKeyID)
}

func (s *signer) signWithKid(t *testing.T, claims jwt.MapClaims, kid string) string {
	t.Helper()

	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = kid

	str, err := tok.SignedString(s.key)
	if err != nil {
		t.Fatalf("signing token: %v", err)
	}
	return str
}

// publicKeyPEM is the material an algorithm-confusion attack would use as an
// HMAC secret: a public value the attacker already has.
func (s *signer) publicKeyPEM(t *testing.T) []byte {
	t.Helper()

	der, err := x509.MarshalPKIXPublicKey(&s.key.PublicKey)
	if err != nil {
		t.Fatalf("marshalling public key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

// validClaims is the shape the gateway's configuration demands: both required
// claims present, correct issuer, comfortably unexpired.
func validClaims() jwt.MapClaims {
	return jwt.MapClaims{
		"iss":                testIssuer,
		"sub":                "user-123",
		"organizationId":     "org-456",
		"preferred_username": "andres",
		"slug":               "codehunters",
		"realm_access":       map[string]any{"roles": []any{"admin", "user"}},
		"exp":                time.Now().Add(time.Hour).Unix(),
		"iat":                time.Now().Add(-time.Minute).Unix(),
	}
}

func defaultConfig(jwksURL string) map[string]interface{} {
	return map[string]interface{}{
		pluginName: map[string]interface{}{
			"jwks_url": jwksURL,
			"issuer":   testIssuer,
			"claims_to_headers": []map[string]string{
				{"claim": "sub", "header": "x-user-id"},
				{"claim": "preferred_username", "header": "x-username"},
				{"claim": "realm_access.roles", "header": "x-user-roles"},
				{"claim": "organizationId", "header": "X-Organization-Id"},
				{"claim": "slug", "header": "x-org-slug"},
			},
			"skip_paths":      []string{"/public/*", "/api/ping"},
			"required_claims": []string{"sub", "organizationId"},
			"roles_claim":     "realm_access.roles",
			"add_ip_header":   true,
			"ip_header_name":  "x-ip",
		},
	}
}

// spy records whether the request reached the backend, and with which headers.
type spy struct {
	called  bool
	headers http.Header
}

func (s *spy) ServeHTTP(_ http.ResponseWriter, req *http.Request) {
	s.called = true
	s.headers = req.Header.Clone()
}

// newHandler builds the real handler and waits for the background JWKS fetch
// to land, so tests exercise the loaded state rather than racing it.
func newHandler(t *testing.T, cfg map[string]interface{}) (http.Handler, *spy) {
	t.Helper()

	next := &spy{}
	h, err := registerer(pluginName).registerHandlers(context.Background(), cfg, next)
	if err != nil {
		t.Fatalf("registering handler: %v", err)
	}

	waitForJWKS(t, h)
	return h, next
}

// waitForJWKS polls a protected path until the handler stops answering 503.
// The plugin loads its key set in a goroutine, so without this the first
// assertion in every test would be a race.
func waitForJWKS(t *testing.T, h http.Handler) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/projects", nil))
		if rec.Code != http.StatusServiceUnavailable {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("JWKS never became ready within 5s")
}

// do issues a request through the handler, with an optional bearer token.
func do(h http.Handler, method, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestValidTokenReachesBackendWithClaimsMappedToHeaders(t *testing.T) {
	s := newSigner(t)
	defer s.shutdown()

	h, next := newHandler(t, defaultConfig(s.jwksURL))

	rec := do(h, http.MethodGet, "/api/projects", s.sign(t, validClaims()))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %q", rec.Code, rec.Body.String())
	}
	if !next.called {
		t.Fatal("the request never reached the backend")
	}

	// Note the roles format: a list claim is forwarded as a JSON array string,
	// not as a comma-separated list. Downstream services parse it as JSON.
	want := map[string]string{
		"X-User-Id":         "user-123",
		"X-Username":        "andres",
		"X-User-Roles":      `["admin","user"]`,
		"X-Organization-Id": "org-456",
		"X-Org-Slug":        "codehunters",
	}
	for header, expected := range want {
		if got := next.headers.Get(header); got != expected {
			t.Errorf("%s = %q, want %q", header, got, expected)
		}
	}
	if next.headers.Get("x-ip") == "" {
		t.Error("x-ip was not injected")
	}
}

func TestRejectsTokensTheGatewayMustNotAccept(t *testing.T) {
	s := newSigner(t)
	defer s.shutdown()

	h, _ := newHandler(t, defaultConfig(s.jwksURL))

	// Each case is a token a client could realistically present. All must be
	// refused with 401 and none may reach the backend.
	cases := []struct {
		name  string
		token func() string
	}{
		{
			name:  "malformed",
			token: func() string { return "not.a.jwt" },
		},
		{
			name: "expired",
			token: func() string {
				c := validClaims()
				c["exp"] = time.Now().Add(-time.Minute).Unix()
				return s.sign(t, c)
			},
		},
		{
			name: "issued by another realm",
			token: func() string {
				c := validClaims()
				c["iss"] = "https://idp.test/realms/someone-else"
				return s.sign(t, c)
			},
		},
		{
			name: "signed by an unknown key",
			token: func() string {
				return s.signWithKid(t, validClaims(), "a-kid-the-jwks-never-published")
			},
		},
		{
			name: "missing the sub claim",
			token: func() string {
				c := validClaims()
				delete(c, "sub")
				return s.sign(t, c)
			},
		},
		{
			name: "missing the organizationId claim",
			token: func() string {
				c := validClaims()
				delete(c, "organizationId")
				return s.sign(t, c)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(h, http.MethodGet, "/api/projects", tc.token())
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body %q", rec.Code, rec.Body.String())
			}
		})
	}
}

// Algorithm confusion is the attack the WithValidMethods option exists to stop:
// an attacker re-signs the payload with an algorithm whose "key" is a value they
// already hold. Both variants must die before the key function is ever consulted.
func TestRejectsAlgorithmConfusion(t *testing.T) {
	s := newSigner(t)
	defer s.shutdown()

	h, _ := newHandler(t, defaultConfig(s.jwksURL))

	t.Run("alg none with an empty signature", func(t *testing.T) {
		header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
		payload, err := json.Marshal(validClaims())
		if err != nil {
			t.Fatalf("marshalling claims: %v", err)
		}
		unsigned := header + "." + base64.RawURLEncoding.EncodeToString(payload) + "."

		rec := do(h, http.MethodGet, "/api/projects", unsigned)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401; an unsigned token was accepted", rec.Code)
		}
	})

	t.Run("HMAC signed with the public key as the secret", func(t *testing.T) {
		tok := jwt.NewWithClaims(jwt.SigningMethodHS256, validClaims())
		tok.Header["kid"] = testKeyID

		forged, err := tok.SignedString(s.publicKeyPEM(t))
		if err != nil {
			t.Fatalf("signing the forged token: %v", err)
		}

		rec := do(h, http.MethodGet, "/api/projects", forged)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401; an HMAC-forged token was accepted", rec.Code)
		}
	})

	// The two cases above would also fail without WithValidMethods: jwt/v5
	// refuses `none` on its own, and HMAC verification rejects an *rsa.PublicKey
	// on a type mismatch. Neither of them, therefore, guards the option.
	//
	// PS256 does. It is RSA-PSS, so it verifies happily against the very key the
	// JWKS publishes and would be accepted on signature alone — only the
	// allowlist keeps it out. Deleting WithValidMethods makes this subtest fail,
	// which is the whole point of it being here.
	t.Run("an algorithm outside the allowlist, against a JWK that does not pin alg", func(t *testing.T) {
		// `alg` is optional in a JWK. When the key set declares it, keyfunc
		// enforces it and the allowlist is never consulted. Omitting it is what
		// leaves WithValidMethods as the only thing refusing PS256 — which is
		// RSA-PSS and verifies against this very key.
		unpinned := newSignerWithJWKS(t, true)
		defer unpinned.shutdown()

		hUnpinned, next := newHandler(t, defaultConfig(unpinned.jwksURL))

		tok := jwt.NewWithClaims(jwt.SigningMethodPS256, validClaims())
		tok.Header["kid"] = testKeyID

		signed, err := tok.SignedString(unpinned.key)
		if err != nil {
			t.Fatalf("signing the PS256 token: %v", err)
		}

		rec := do(hUnpinned, http.MethodGet, "/api/projects", signed)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401; PS256 is outside the allowlist but was accepted", rec.Code)
		}
		if next.called {
			t.Fatal("a PS256 token reached the backend")
		}
	})
}

func TestRequestsWithoutABearerTokenAreRefused(t *testing.T) {
	s := newSigner(t)
	defer s.shutdown()

	h, next := newHandler(t, defaultConfig(s.jwksURL))

	cases := []struct {
		name   string
		header string
	}{
		{name: "no Authorization header", header: ""},
		{name: "a scheme that is not Bearer", header: "Basic dXNlcjpwYXNz"},
		{name: "the bare word Bearer", header: "Bearer"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			next.called = false

			req := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			if next.called {
				t.Fatal("the request reached the backend without a token")
			}
		})
	}
}

func TestSkipPathsAndPreflightsBypassValidation(t *testing.T) {
	s := newSigner(t)
	defer s.shutdown()

	h, next := newHandler(t, defaultConfig(s.jwksURL))

	cases := []struct {
		name   string
		method string
		path   string
	}{
		{name: "exact skip path", method: http.MethodGet, path: "/api/ping"},
		{name: "wildcard skip path", method: http.MethodGet, path: "/public/anything"},
		{name: "CORS preflight on a protected path", method: http.MethodOptions, path: "/api/projects"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			next.called = false

			rec := do(h, tc.method, tc.path, "")

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body %q", rec.Code, rec.Body.String())
			}
			if !next.called {
				t.Fatal("the request did not reach the backend")
			}
		})
	}
}

// A path that merely starts like a skip path is not one. This is the bypass a
// caller would try first.
func TestPathsResemblingSkipPathsStillRequireAToken(t *testing.T) {
	s := newSigner(t)
	defer s.shutdown()

	h, _ := newHandler(t, defaultConfig(s.jwksURL))

	for _, path := range []string{"/api/pingXYZ", "/api/ping/sub", "/API/PING", "/publicx/thing"} {
		t.Run(path, func(t *testing.T) {
			if rec := do(h, http.MethodGet, path, ""); rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; %q was treated as public", rec.Code, path)
			}
		})
	}
}

// The managed headers are stripped before anything else happens, so a caller
// cannot assert an identity the gateway did not verify. This must hold on skip
// paths too, where no token is validated and nothing overwrites them.
func TestCallerSuppliedIdentityHeadersAreStripped(t *testing.T) {
	s := newSigner(t)
	defer s.shutdown()

	h, next := newHandler(t, defaultConfig(s.jwksURL))

	spoofed := map[string]string{
		"X-User-Id":         "attacker",
		"X-Username":        "root",
		"X-User-Roles":      "superadmin",
		"X-Organization-Id": "someone-elses-org",
		"X-Org-Slug":        "victim",
		"x-ip":              "1.2.3.4",
	}

	t.Run("on a skip path, where no token replaces them", func(t *testing.T) {
		next.called = false

		req := httptest.NewRequest(http.MethodGet, "/api/ping", nil)
		for header, value := range spoofed {
			req.Header.Set(header, value)
		}
		h.ServeHTTP(httptest.NewRecorder(), req)

		if !next.called {
			t.Fatal("the request did not reach the backend")
		}
		for header := range spoofed {
			if got := next.headers.Get(header); got != "" {
				t.Errorf("%s = %q, want it stripped", header, got)
			}
		}
	})

	t.Run("on a validated request, where the token's values win", func(t *testing.T) {
		next.called = false

		req := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
		req.Header.Set("Authorization", "Bearer "+s.sign(t, validClaims()))
		for header, value := range spoofed {
			req.Header.Set(header, value)
		}
		h.ServeHTTP(httptest.NewRecorder(), req)

		if !next.called {
			t.Fatal("the request did not reach the backend")
		}
		if got := next.headers.Get("X-User-Id"); got != "user-123" {
			t.Errorf("X-User-Id = %q, want the token's subject", got)
		}
		if got := next.headers.Get("X-Organization-Id"); got != "org-456" {
			t.Errorf("X-Organization-Id = %q, want the token's organizationId", got)
		}
	})
}

// With the key set unavailable the edge must refuse protected traffic rather
// than pass it through. Pointing at a dead URL keeps jwksReady false for the
// lifetime of the test, which is exactly the startup posture being asserted.
func TestProtectedRoutesFailClosedWhileTheKeySetIsUnavailable(t *testing.T) {
	cfg := defaultConfig("http://127.0.0.1:1/unreachable")

	next := &spy{}
	h, err := registerer(pluginName).registerHandlers(context.Background(), cfg, next)
	if err != nil {
		t.Fatalf("registering handler: %v", err)
	}

	rec := do(h, http.MethodGet, "/api/projects", "irrelevant")

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body %q", rec.Code, rec.Body.String())
	}
	if next.called {
		t.Fatal("a protected request passed through with no key set loaded")
	}

	// Public paths stay served: an IdP outage must not take down the health and
	// auth-flow routes that exist to recover from it.
	if rec := do(h, http.MethodGet, "/api/ping", ""); rec.Code != http.StatusOK {
		t.Errorf("skip path status = %d, want 200 while the key set is down", rec.Code)
	}
}

func TestRoleRulesGateMatchingPathsOnly(t *testing.T) {
	s := newSigner(t)
	defer s.shutdown()

	cfg := defaultConfig(s.jwksURL)
	cfg[pluginName].(map[string]interface{})["required_roles"] = []map[string]interface{}{
		{"path": "/api/admin/*", "roles": []string{"superadmin"}},
	}

	h, next := newHandler(t, cfg)

	t.Run("a rule matches and the role is absent", func(t *testing.T) {
		next.called = false

		rec := do(h, http.MethodGet, "/api/admin/users", s.sign(t, validClaims()))

		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
		if next.called {
			t.Fatal("the request reached the backend without the required role")
		}
	})

	t.Run("a rule matches and the role is present", func(t *testing.T) {
		next.called = false

		claims := validClaims()
		claims["realm_access"] = map[string]any{"roles": []any{"superadmin"}}

		if rec := do(h, http.MethodGet, "/api/admin/users", s.sign(t, claims)); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
	})

	// Documents a deliberate fail-open: a path no rule mentions is allowed for
	// any authenticated caller, because most routes declare no roles at all.
	// Authorisation per resource is the service's job, not the edge's. The
	// token still had to be valid to get here.
	t.Run("no rule matches, so any valid token passes", func(t *testing.T) {
		next.called = false

		if rec := do(h, http.MethodGet, "/api/projects", s.sign(t, validClaims())); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 on a path with no role rule", rec.Code)
		}
		if !next.called {
			t.Fatal("the request did not reach the backend")
		}
	})
}

// The header values are a contract with every backend behind the edge: a
// service reading x-user-roles has to know whether to split on commas or parse
// JSON. This pins the conversion each claim type goes through.
func TestClaimTypesAreForwardedInAStableFormat(t *testing.T) {
	cases := []struct {
		name  string
		claim any
		want  string
	}{
		{name: "string", claim: "plain", want: "plain"},
		{name: "whole number loses its decimal point", claim: float64(42), want: "42"},
		{name: "fractional number keeps it", claim: 1.5, want: "1.5"},
		{name: "bool", claim: true, want: "true"},
		{name: "list becomes a JSON array, not CSV", claim: []any{"a", "b"}, want: `["a","b"]`},
		{name: "object becomes JSON", claim: map[string]any{"k": "v"}, want: `{"k":"v"}`},
		{name: "absent claim yields empty", claim: nil, want: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			claims := jwt.MapClaims{}
			if tc.claim != nil {
				claims["field"] = tc.claim
			}

			if got := extractClaim(claims, "field"); got != tc.want {
				t.Errorf("extractClaim = %q, want %q", got, tc.want)
			}
		})
	}

	t.Run("nested claims are reached with dot notation", func(t *testing.T) {
		claims := jwt.MapClaims{"realm_access": map[string]any{"roles": []any{"admin"}}}

		if got := extractClaim(claims, "realm_access.roles"); got != `["admin"]` {
			t.Errorf("extractClaim = %q, want %q", got, `["admin"]`)
		}
	})

	t.Run("a missing intermediate level yields empty, not a panic", func(t *testing.T) {
		claims := jwt.MapClaims{"realm_access": "not an object"}

		if got := extractClaim(claims, "realm_access.roles"); got != "" {
			t.Errorf("extractClaim = %q, want empty", got)
		}
	})
}

func TestRejectedRequestsNeverLeakTheToken(t *testing.T) {
	s := newSigner(t)
	defer s.shutdown()

	h, _ := newHandler(t, defaultConfig(s.jwksURL))

	claims := validClaims()
	claims["exp"] = time.Now().Add(-time.Minute).Unix()
	token := s.sign(t, claims)

	rec := do(h, http.MethodGet, "/api/projects", token)

	if body := rec.Body.String(); strings.Contains(body, token) {
		t.Errorf("the response body echoed the token: %q", body)
	}
}
