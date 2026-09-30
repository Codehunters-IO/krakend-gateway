package main

import (
	"fmt"
	"strings"
)

var validMethods = map[string]bool{
	"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true,
}

// effectiveBackend resolves the backend key for an endpoint: its own if set,
// otherwise its product's default. Empty means unresolvable.
func effectiveBackend(spec Spec, e Endpoint) string {
	if e.Backend != "" {
		return e.Backend
	}
	return spec.Products[e.Product].Backend
}

// validPrefix accepts "" or a path starting with "/" and not ending in one.
// A trailing slash would concatenate into "//api/...", a route that looks
// reachable in the config and is not the one anybody meant.
func validPrefix(p string) bool {
	if p == "" {
		return true
	}
	return strings.HasPrefix(p, "/") && !strings.HasSuffix(p, "/")
}

// Validate returns one error per rule violation. Empty slice means the spec is valid.
func Validate(spec Spec) []error {
	var errs []error
	for name, p := range spec.Products {
		if !validPrefix(p.Prefix) {
			errs = append(errs, fmt.Errorf("product[%s]: invalid prefix %q", name, p.Prefix))
		}
	}
	seen := map[string]bool{}
	for i, e := range spec.Endpoints {
		where := fmt.Sprintf("endpoint[%d] %s %s", i, e.Method, e.Path)
		if e.Path == "" || !strings.HasPrefix(e.Path, "/") {
			errs = append(errs, fmt.Errorf("%s: invalid path", where))
		}
		if !validMethods[e.Method] {
			errs = append(errs, fmt.Errorf("%s: unknown method %q", where, e.Method))
		}
		if len(spec.Products) > 0 {
			if e.Product == "" {
				errs = append(errs, fmt.Errorf("%s: product required", where))
			} else if _, ok := spec.Products[e.Product]; !ok {
				errs = append(errs, fmt.Errorf("%s: product %q not declared", where, e.Product))
			}
		}
		if b := effectiveBackend(spec, e); b == "" {
			errs = append(errs, fmt.Errorf("%s: backend not resolvable", where))
		} else if _, ok := spec.Backends[b]; !ok {
			errs = append(errs, fmt.Errorf("%s: backend %q not declared", where, b))
		}
		if len(e.InputHeaders) == 0 {
			errs = append(errs, fmt.Errorf("%s: input_headers required", where))
		}
		if e.Auth != "public" && e.Auth != "protected" {
			errs = append(errs, fmt.Errorf("%s: invalid auth %q", where, e.Auth))
		}
		key := e.Method + " " + e.Path
		if seen[key] {
			errs = append(errs, fmt.Errorf("%s: duplicate endpoint", where))
		}
		seen[key] = true
	}
	return errs
}
