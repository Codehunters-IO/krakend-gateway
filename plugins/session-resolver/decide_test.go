package main

import (
	"regexp"
	"testing"
)

func testConfig() *pluginConfig {
	return &pluginConfig{
		CookieName:      "sid",
		AllowedOrigins:  []string{"http://localhost:5173"},
		CSRFSafeMethods: []string{"GET", "HEAD", "OPTIONS"},
	}
}

func testMatchers(t *testing.T) (map[string]bool, []*regexp.Regexp) {
	t.Helper()
	exact, regexes, err := buildMatchers([]string{"/auth/*", "/api/ping"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return exact, regexes
}

func TestDecide(t *testing.T) {
	validSid := "A123456789012345678901234567890123456789012"
	if len(validSid) != 43 {
		t.Fatalf("test fixture sid must be 43 chars, got %d", len(validSid))
	}

	exact, regexes := testMatchers(t)
	cfg := testConfig()

	for _, tc := range []struct {
		name    string
		req     request
		want    action
		wantSid string
	}{
		{
			name: "skip path passes through untouched",
			req:  request{Path: "/auth/login/keycloak", Method: "GET"},
			want: actionPassThrough,
		},
		{
			name: "preflight passes through",
			req:  request{Path: "/api/projects", Method: "OPTIONS"},
			want: actionPassThrough,
		},
		{
			name: "existing bearer wins over the cookie",
			req:  request{Path: "/api/projects", Method: "GET", Authorization: "Bearer mcp-token", Cookie: validSid},
			want: actionPassThrough,
		},
		{
			name: "no cookie and no bearer defers to jwt-headers",
			req:  request{Path: "/api/projects", Method: "GET"},
			want: actionPassThrough,
		},
		{
			name: "malformed sid is rejected without touching valkey",
			req:  request{Path: "/api/projects", Method: "GET", Cookie: "too-short"},
			want: actionUnauthorized,
		},
		{
			name: "sid with characters outside base64url is rejected",
			req:  request{Path: "/api/projects", Method: "GET", Cookie: "A12345678901234567890123456789012345678901+"},
			want: actionUnauthorized,
		},
		{
			name: "sid one character short of 43 is rejected",
			req:  request{Path: "/api/projects", Method: "GET", Cookie: validSid[:42]},
			want: actionUnauthorized,
		},
		{
			name: "sid one character over 43 is rejected",
			req:  request{Path: "/api/projects", Method: "GET", Cookie: validSid + "A"},
			want: actionUnauthorized,
		},
		{
			name:    "valid cookie on a safe method resolves",
			req:     request{Path: "/api/projects", Method: "GET", Cookie: validSid},
			want:    actionResolve,
			wantSid: validSid,
		},
		{
			name:    "mutating method with an allowed origin resolves",
			req:     request{Path: "/api/projects", Method: "POST", Cookie: validSid, Origin: "http://localhost:5173"},
			want:    actionResolve,
			wantSid: validSid,
		},
		{
			name: "mutating method with a foreign origin is forbidden",
			req:  request{Path: "/api/projects", Method: "POST", Cookie: validSid, Origin: "http://evil.example"},
			want: actionForbidden,
		},
		{
			name: "mutating method with no origin or referer is forbidden",
			req:  request{Path: "/api/projects", Method: "POST", Cookie: validSid},
			want: actionForbidden,
		},
		{
			name:    "mutating method falls back to referer when origin is absent",
			req:     request{Path: "/api/projects", Method: "POST", Cookie: validSid, Referer: "http://localhost:5173/projects"},
			want:    actionResolve,
			wantSid: validSid,
		},
		{
			name: "csrf check applies before the cookie is trusted",
			req:  request{Path: "/api/projects", Method: "DELETE", Cookie: validSid, Origin: "http://evil.example"},
			want: actionForbidden,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, sid := decide(tc.req, cfg, exact, regexes)
			if got != tc.want {
				t.Errorf("action = %v, want %v", got, tc.want)
			}
			if sid != tc.wantSid {
				t.Errorf("sid = %q, want %q", sid, tc.wantSid)
			}
		})
	}
}

// TestZeroValueActionFailsClosed guards the enum ordering: the zero value of
// action must be a deny, so that an uninitialized or partially-set action
// variable never accidentally behaves like actionPassThrough.
func TestZeroValueActionFailsClosed(t *testing.T) {
	var zero action
	if zero != actionUnauthorized {
		t.Fatalf("zero value of action = %v, want actionUnauthorized (fail closed)", zero)
	}
}
