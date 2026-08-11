package main

import (
	"fmt"
	"strings"
)

var validMethods = map[string]bool{
	"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true,
}

// Validate returns one error per rule violation. Empty slice means the spec is valid.
func Validate(spec Spec) []error {
	var errs []error
	seen := map[string]bool{}
	for i, e := range spec.Endpoints {
		where := fmt.Sprintf("endpoint[%d] %s %s", i, e.Method, e.Path)
		if e.Path == "" || !strings.HasPrefix(e.Path, "/") {
			errs = append(errs, fmt.Errorf("%s: invalid path", where))
		}
		if !validMethods[e.Method] {
			errs = append(errs, fmt.Errorf("%s: unknown method %q", where, e.Method))
		}
		if _, ok := spec.Backends[e.Backend]; !ok {
			errs = append(errs, fmt.Errorf("%s: backend %q not declared", where, e.Backend))
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
