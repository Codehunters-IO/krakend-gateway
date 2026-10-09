package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// assertDenialHeaders holds the contract for every rejection this plugin
// writes. It cannot come from KrakenD's security/http middleware: plugins wrap
// the router, so they answer before it ever runs.
func assertDenialHeaders(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()

	if got, want := rec.Header().Get("Content-Type"), "application/json; charset=utf-8"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}
	if got, want := rec.Header().Get("Cache-Control"), "no-store"; got != want {
		t.Errorf("Cache-Control = %q, want %q", got, want)
	}
	if got, want := rec.Header().Get("X-Content-Type-Options"), "nosniff"; got != want {
		t.Errorf("X-Content-Type-Options = %q, want %q", got, want)
	}
}

// A malformed cookie is rejected before Valkey is touched, so this exercises
// the deny path with no store involved at all.
func TestMalformedSessionDenialCarriesTheRightHeaders(t *testing.T) {
	addr := startValkey(t)
	handler, captured := buildHandler(t, handlerConfig(addr))

	req := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
	req.AddCookie(&http.Cookie{Name: "sid", Value: "too-short"})
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if captured.Called {
		t.Fatal("the backend was called for a malformed session cookie")
	}
	assertDenialHeaders(t, rec)
}

// A 401 that an intermediary may cache can be replayed to a request that would
// have succeeded, which is why no-store matters more here than the status.
func TestDenyWritesTheHeadersAndTheGenericBody(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/projects", nil)

	deny(rec, req, http.StatusUnauthorized, "malformed_session")

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	assertDenialHeaders(t, rec)
	if got, want := rec.Body.String(), `{"message":"unauthorized"}`; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
	if got := len(rec.Header().Values("Cache-Control")); got != 1 {
		t.Fatalf("Cache-Control was written %d times", got)
	}
}

// The body must stay generic whatever the outcome: the outcome goes to the log,
// never to the client, or the response becomes an oracle for why auth failed.
func TestTheDenialBodyNeverReflectsTheOutcome(t *testing.T) {
	for _, outcome := range []string{
		"malformed_session",
		"miss",
		"store_down",
		"refresh_failed",
		"empty_token",
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/projects", nil)

		deny(rec, req, http.StatusUnauthorized, outcome)

		if got, want := rec.Body.String(), `{"message":"unauthorized"}`; got != want {
			t.Errorf("outcome %q leaked into the body: %q", outcome, got)
		}
	}
}

// Pass-through must not inherit the denial headers, or the plugin would
// override whatever cache policy the backend chose.
func TestAnAllowedRequestGetsNoDenialHeaders(t *testing.T) {
	addr := startValkey(t)
	handler, captured := buildHandler(t, handlerConfig(addr))

	req := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
	req.Header.Set("Authorization", "Bearer an-incoming-token")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if !captured.Called {
		t.Fatal("an incoming bearer did not reach the backend")
	}
	if got := rec.Header().Get("Cache-Control"); got != "" {
		t.Fatalf("a denial header leaked onto an allowed response: %q", got)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "" {
		t.Fatalf("a denial header leaked onto an allowed response: %q", got)
	}
}
