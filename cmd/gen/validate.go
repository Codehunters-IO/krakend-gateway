package main

import (
	"fmt"
	"regexp"
	"strings"
)

// claimPath accepts a dotted claim path of at least two segments:
// "resource_access.forgeos-api.roles" passes, a bare "roles" does not. Client
// ids carry hyphens, so they are legal after the first separator.
var claimPath = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z0-9_-]+)+$`)

var validMethods = map[string]bool{
	"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true,
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
		if e.Roles != nil && e.RolesWaiver != "" {
			errs = append(errs, fmt.Errorf("%s: roles and roles_waiver are mutually exclusive", where))
		}
		if e.Auth == "public" && (e.Roles != nil || e.RolesWaiver != "") {
			errs = append(errs, fmt.Errorf("%s: public endpoints take neither roles nor roles_waiver", where))
		}
		if e.Roles != nil {
			if len(e.Roles.AnyOf) == 0 {
				errs = append(errs, fmt.Errorf("%s: roles.any_of must list at least one role", where))
			}
			if e.Roles.Claim != "" && !claimPath.MatchString(e.Roles.Claim) {
				errs = append(errs, fmt.Errorf("%s: roles.claim %q is not a dotted claim path", where, e.Roles.Claim))
			}
		}
		// Key on the EXPOSED path (exposedPath, shared with Normalize). Two products
		// can each declare /api/ping as long as their prefixes differ; with equal
		// prefixes it is a real collision, and KrakenD would silently serve whichever
		// endpoint it saw first.
		key := e.Method + " " + exposedPath(spec, e)
		if seen[key] {
			errs = append(errs, fmt.Errorf("%s: duplicate endpoint", where))
		}
		seen[key] = true
	}
	return errs
}
