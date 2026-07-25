package main

import "testing"

// matches builds the exact+regex matchers from patterns and reports whether path
// is considered a skip/optional match (exact hit or any wildcard regex hit).
func matches(t *testing.T, patterns []string, path string) bool {
	t.Helper()
	exact, regexes, err := buildMatchers(patterns)
	if err != nil {
		t.Fatalf("buildMatchers(%v): %v", patterns, err)
	}
	return exact[path] || matchesAny(path, regexes)
}

func TestBuildMatchers_SegmentWildcard(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
		path    string
		want    bool
	}{
		// UUID in the middle + trailing register segment (the bug this fixes).
		{"managers register", "/vitxo-ms-auth/api/v1/*/managers/*",
			"/vitxo-ms-auth/api/v1/2f1c/managers/register", true},
		{"bettors register", "/vitxo-ms-auth/api/v1/*/bettors/*",
			"/vitxo-ms-auth/api/v1/2f1c/bettors/register", true},
		// Trailing /* is optional -> the bare resource path still matches.
		{"managers no subpath", "/vitxo-ms-auth/api/v1/*/managers/*",
			"/vitxo-ms-auth/api/v1/2f1c/managers", true},
		// Middle * matches exactly one segment, not two.
		{"middle wildcard single segment only", "/vitxo-ms-auth/api/v1/*/managers/*",
			"/vitxo-ms-auth/api/v1/a/b/managers/register", false},
		// Existing trailing /* must still match multiple segments.
		{"file-share multi-segment", "/vitxo-ms-file-share/api/v1/*",
			"/vitxo-ms-file-share/api/v1/shared/x/documents", true},
		{"file-share base", "/vitxo-ms-file-share/api/v1/*",
			"/vitxo-ms-file-share/api/v1", true},
		// Non-matching paths stay enforced.
		{"different service", "/vitxo-ms-auth/api/v1/*/managers/*",
			"/vitxo-ms-raffles/api/v1/2f1c/managers/register", false},
		{"different resource", "/vitxo-ms-auth/api/v1/*/managers/*",
			"/vitxo-ms-auth/api/v1/2f1c/bettors/register", false},
		// Dots are literal, not regex wildcards.
		{"literal dots", "/vitxo-ms-auth/swagger-ui.html",
			"/vitxo-ms-auth/swagger-uiXhtml", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := matches(t, []string{tc.pattern}, tc.path); got != tc.want {
				t.Errorf("pattern %q path %q: got %v want %v", tc.pattern, tc.path, got, tc.want)
			}
		})
	}
}

func TestBuildMatchers_ExactFastPath(t *testing.T) {
	exact, regexes, err := buildMatchers([]string{"/vitxo-ms-auth/api/v1/login"})
	if err != nil {
		t.Fatal(err)
	}
	if !exact["/vitxo-ms-auth/api/v1/login"] {
		t.Error("no-wildcard pattern must land in the exact set")
	}
	if len(regexes) != 0 {
		t.Errorf("no-wildcard pattern must not compile a regex, got %d", len(regexes))
	}
}
