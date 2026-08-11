package main

import "testing"

func specForNormalize() Spec {
	return Spec{
		Backends: map[string]Backend{"forgeos": {HostDefault: "http://x:8080", HostEnv: "FORGEOS_HOST"}},
		Defaults: Defaults{OutputEncoding: "no-op", Encoding: "no-op", Timeout: ""},
		Endpoints: []Endpoint{
			{Path: "/api/ping", Method: "GET", Backend: "forgeos", Auth: "public", InputHeaders: []string{"Accept"}},
		},
	}
}

func TestNormalize_AppliesDefaultsAndHost(t *testing.T) {
	out := Normalize(specForNormalize())
	e := out[0]
	if e.OutputEncoding != "no-op" || e.Encoding != "no-op" {
		t.Errorf("encodings: %q/%q", e.OutputEncoding, e.Encoding)
	}
	if e.URLPattern != "/api/ping" {
		t.Errorf("url_pattern: got %q want /api/ping", e.URLPattern)
	}
	if e.HostEnv != "FORGEOS_HOST" || e.HostDefault != "http://x:8080" {
		t.Errorf("host resolution: %q / %q", e.HostEnv, e.HostDefault)
	}
	if e.InputQueryStrings == nil {
		t.Error("input_query_strings should be non-nil empty slice, got nil")
	}
}

func TestNormalize_KeepsExplicitURLPatternAndEncoding(t *testing.T) {
	s := specForNormalize()
	s.Endpoints[0].URLPattern = "/backend/route"
	s.Endpoints[0].OutputEncoding = "json"
	out := Normalize(s)
	if out[0].URLPattern != "/backend/route" {
		t.Errorf("url_pattern overridden: %q", out[0].URLPattern)
	}
	if out[0].OutputEncoding != "json" {
		t.Errorf("output_encoding overridden: %q", out[0].OutputEncoding)
	}
}
