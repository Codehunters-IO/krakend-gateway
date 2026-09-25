package main

import (
	"fmt"
	"regexp"
	"strings"
)

// buildMatchers splits skip patterns into a fast exact-match set and compiled
// regexes for wildcard patterns. A trailing "/*" matches any number of trailing
// segments; every other "*" matches exactly one segment.
func buildMatchers(patterns []string) (map[string]bool, []*regexp.Regexp, error) {
	exact := make(map[string]bool, len(patterns))
	regexes := make([]*regexp.Regexp, 0)
	for _, p := range patterns {
		if !strings.Contains(p, "*") {
			exact[p] = true
			continue
		}
		re, err := compilePattern(p)
		if err != nil {
			return nil, nil, fmt.Errorf("pattern %q: %w", p, err)
		}
		regexes = append(regexes, re)
	}
	return exact, regexes, nil
}

func compilePattern(p string) (*regexp.Regexp, error) {
	rest := false
	if strings.HasSuffix(p, "/*") {
		rest = true
		p = strings.TrimSuffix(p, "/*")
	}
	parts := strings.Split(p, "*")
	for i, part := range parts {
		parts[i] = regexp.QuoteMeta(part)
	}
	expr := "^" + strings.Join(parts, "[^/]+")
	if rest {
		expr += "(/.*)?"
	}
	expr += "$"
	return regexp.Compile(expr)
}

func matchesAny(path string, regexes []*regexp.Regexp) bool {
	for _, re := range regexes {
		if re.MatchString(path) {
			return true
		}
	}
	return false
}
