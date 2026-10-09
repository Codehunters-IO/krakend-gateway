package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// assertDenialHeaders holds the contract every edge rejection must meet. These
// headers cannot come from KrakenD's security/http middleware: plugins wrap the
// router, so they answer before it ever runs.
func assertDenialHeaders(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()

	if got, want := rec.Header().Get("Content-Type"), "application/json; charset=utf-8"; got != want {
		t.Errorf("Content-Type = %q, want %q (the body is JSON)", got, want)
	}
	if got, want := rec.Header().Get("Cache-Control"), "no-store"; got != want {
		t.Errorf("Cache-Control = %q, want %q", got, want)
	}
	if got, want := rec.Header().Get("X-Content-Type-Options"), "nosniff"; got != want {
		t.Errorf("X-Content-Type-Options = %q, want %q", got, want)
	}
}

func TestMissingAuthorizationDenialCarriesTheRightHeaders(t *testing.T) {
	s := newSigner(t)
	defer s.shutdown()
	h, _ := newHandler(t, defaultConfig(s.jwksURL))

	rec := do(h, http.MethodGet, "/api/projects", "")

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	assertDenialHeaders(t, rec)
}

func TestInvalidTokenDenialCarriesTheRightHeaders(t *testing.T) {
	s := newSigner(t)
	defer s.shutdown()
	h, _ := newHandler(t, defaultConfig(s.jwksURL))

	rec := do(h, http.MethodGet, "/api/projects", "not.a.token")

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	assertDenialHeaders(t, rec)
}

func TestMissingRequiredClaimDenialCarriesTheRightHeaders(t *testing.T) {
	s := newSigner(t)
	defer s.shutdown()
	h, _ := newHandler(t, defaultConfig(s.jwksURL))

	claims := validClaims()
	delete(claims, "organizationId") // declared in required_claims by defaultConfig

	rec := do(h, http.MethodGet, "/api/projects", s.sign(t, claims))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body %q", rec.Code, rec.Body.String())
	}
	assertDenialHeaders(t, rec)
}

func TestForbiddenDenialCarriesTheRightHeaders(t *testing.T) {
	s := newSigner(t)
	defer s.shutdown()

	cfg := defaultConfig(s.jwksURL)
	block := cfg[pluginName].(map[string]interface{})
	block["required_roles"] = []map[string]interface{}{
		{"path": "/api/projects", "roles": []string{"superuser"}},
	}

	h, _ := newHandler(t, cfg)

	rec := do(h, http.MethodGet, "/api/projects", s.sign(t, validClaims()))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body %q", rec.Code, rec.Body.String())
	}
	assertDenialHeaders(t, rec)
}

// The 503 the plugin returns while its key set is still missing is the
// fail-closed path, and it must not be cacheable either: a cached 503 would
// outlive the outage it describes.
func TestJwksNotReadyDenialCarriesTheRightHeaders(t *testing.T) {
	cfg := defaultConfig("http://127.0.0.1:1/certs") // nothing listens there
	next := &spy{}

	h, err := registerer(pluginName).registerHandlers(context.Background(), cfg, next)
	if err != nil {
		t.Fatalf("registering handler: %v", err)
	}

	rec := do(h, http.MethodGet, "/api/projects", "")

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if next.called {
		t.Fatal("the request reached the backend with no key set loaded")
	}
	assertDenialHeaders(t, rec)
}

// The body is unchanged by the move off http.Error, trailing newline included,
// so no client parsing it by length or by exact bytes is affected.
func TestDenialBodyIsUnchanged(t *testing.T) {
	s := newSigner(t)
	defer s.shutdown()
	h, _ := newHandler(t, defaultConfig(s.jwksURL))

	rec := do(h, http.MethodGet, "/api/projects", "")

	if got, want := rec.Body.String(), "{\"message\":\"missing or invalid authorization header\"}\n"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

// Pass-through must stay pass-through: the plugin adds no cache directive to a
// response it did not write, or it would override whatever the backend chose.
func TestAnAllowedRequestGetsNoDenialHeaders(t *testing.T) {
	s := newSigner(t)
	defer s.shutdown()

	echo := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "max-age=60")
		w.WriteHeader(http.StatusOK)
	})
	h, err := registerer(pluginName).registerHandlers(context.Background(), defaultConfig(s.jwksURL), echo)
	if err != nil {
		t.Fatalf("registering handler: %v", err)
	}
	waitForJWKS(t, h)

	rec := do(h, http.MethodGet, "/api/projects", s.sign(t, validClaims()))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "max-age=60" {
		t.Fatalf("the backend's Cache-Control was overwritten: %q", got)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "" {
		t.Fatalf("a denial header leaked onto an allowed response: %q", got)
	}
}

// A skipped public path is not a denial either.
func TestASkippedPathGetsNoDenialHeaders(t *testing.T) {
	s := newSigner(t)
	defer s.shutdown()
	h, next := newHandler(t, defaultConfig(s.jwksURL))

	rec := do(h, http.MethodGet, "/api/ping", "")

	if !next.called {
		t.Fatal("a skip_paths route did not reach the backend")
	}
	if got := rec.Header().Get("Cache-Control"); got != "" {
		t.Fatalf("a denial header leaked onto a public route: %q", got)
	}
}

// deny is the single place these headers are set; a direct test keeps that
// contract pinned even if every call site above is refactored away.
func TestDenyWritesTheHeadersExactlyOnce(t *testing.T) {
	rec := httptest.NewRecorder()

	deny(rec, http.StatusTeapot, `{"message":"short and stout"}`)

	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d", rec.Code)
	}
	assertDenialHeaders(t, rec)
	if got, want := rec.Body.String(), "{\"message\":\"short and stout\"}\n"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
	if got := len(rec.Header().Values("Cache-Control")); got != 1 {
		t.Fatalf("Cache-Control was written %d times", got)
	}
}
