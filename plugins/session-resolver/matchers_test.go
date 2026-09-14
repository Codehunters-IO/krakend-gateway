package main

import "testing"

func TestSkipPathMatching(t *testing.T) {
	exact, regexes, err := buildMatchers([]string{"/api/ping", "/auth/*", "/api/*/public"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, tc := range []struct {
		path string
		want bool
	}{
		{"/api/ping", true},
		{"/api/pingx", false},
		{"/auth/login/keycloak", true},
		// A trailing "/*" compiles its prefix as an *optional* group, so the bare
		// prefix itself matches too. This is deliberate parity with jwt-headers'
		// compilePattern (confirmed against its own "file-share base" test case),
		// not an accidental loosening: the two plugins sit next to each other in
		// the same chain and must not disagree about what a skip pattern means.
		{"/auth", true},
		{"/api/v1/public", true},
		{"/api/v1/v2/public", false},
		{"/api/projects", false},
	} {
		t.Run(tc.path, func(t *testing.T) {
			got := exact[tc.path] || matchesAny(tc.path, regexes)
			if got != tc.want {
				t.Errorf("match(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// TestBuildMatchersTreatsMetacharactersLiterally pins the actual guarantee
// compilePattern provides: every literal segment is passed through
// regexp.QuoteMeta before compilation, so a skip_paths entry can never fail to
// compile (the only unescaped pieces are the fixed, always-valid "[^/]+" and
// "(/.*)?" the function itself inserts) and, more importantly, an operator's
// pattern is never accidentally interpreted as a regex. A pattern containing
// regex metacharacters must match only that literal path, not act as a regex.
func TestBuildMatchersTreatsMetacharactersLiterally(t *testing.T) {
	exact, regexes, err := buildMatchers([]string{"/api/*[("})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, tc := range []struct {
		path string
		want bool
	}{
		{"/api/x[(", true},  // literal "[(" suffix, "x" fills the wildcard segment
		{"/api/xyz", false}, // no literal "[(" suffix: must not match
		{"/api/[(", false},  // wildcard segment requires at least one character
	} {
		t.Run(tc.path, func(t *testing.T) {
			got := exact[tc.path] || matchesAny(tc.path, regexes)
			if got != tc.want {
				t.Errorf("match(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}
