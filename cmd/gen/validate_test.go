package main

import (
	"strings"
	"testing"
)

func base() Spec {
	return Spec{
		Backends: map[string]Backend{"forgeos": {HostDefault: "http://x:8080", HostEnv: "FORGEOS_HOST"}},
		Endpoints: []Endpoint{
			{Path: "/api/ping", Method: "GET", Backend: "forgeos", Auth: "public", InputHeaders: []string{"Accept"}},
		},
	}
}

func TestValidate_OK(t *testing.T) {
	if errs := Validate(base()); len(errs) != 0 {
		t.Fatalf("want no errors, got %v", errs)
	}
}

func TestValidate_BadPath(t *testing.T) {
	s := base()
	s.Endpoints[0].Path = "api/ping" // missing leading slash
	assertErrContains(t, Validate(s), "invalid path")
}

func TestValidate_BadMethod(t *testing.T) {
	s := base()
	s.Endpoints[0].Method = "FETCH"
	assertErrContains(t, Validate(s), "unknown method")
}

func TestValidate_UnknownBackend(t *testing.T) {
	s := base()
	s.Endpoints[0].Backend = "ghost"
	assertErrContains(t, Validate(s), `backend "ghost"`)
}

func TestValidate_DuplicateEndpoint(t *testing.T) {
	s := base()
	s.Endpoints = append(s.Endpoints, s.Endpoints[0])
	assertErrContains(t, Validate(s), "duplicate endpoint")
}

func TestValidate_EmptyHeaders(t *testing.T) {
	s := base()
	s.Endpoints[0].InputHeaders = nil
	assertErrContains(t, Validate(s), "input_headers required")
}

func TestValidate_BadAuth(t *testing.T) {
	s := base()
	s.Endpoints[0].Auth = "maybe"
	assertErrContains(t, Validate(s), "invalid auth")
}

func assertErrContains(t *testing.T, errs []error, want string) {
	t.Helper()
	for _, e := range errs {
		if strings.Contains(e.Error(), want) {
			return
		}
	}
	t.Fatalf("want an error containing %q, got %v", want, errs)
}
