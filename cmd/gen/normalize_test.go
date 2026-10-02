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

func prefixSpec(prefix, urlPattern string) Spec {
	return Spec{
		Products: map[string]Product{"vitxo": {Prefix: prefix, Backend: "forgeos"}},
		Backends: map[string]Backend{"forgeos": {HostDefault: "http://x:8080", HostEnv: "FORGEOS_HOST"}},
		Endpoints: []Endpoint{{
			Path: "/api/orders", Method: "POST", Product: "vitxo", Auth: "protected",
			URLPattern: urlPattern, InputHeaders: []string{"Accept"},
		}},
	}
}

func TestNormalize_PrefixAppliedToPathOnly(t *testing.T) {
	got := Normalize(prefixSpec("/vitxo", ""))[0]
	if got.Path != "/vitxo/api/orders" {
		t.Errorf("path: got %q want %q", got.Path, "/vitxo/api/orders")
	}
	if got.URLPattern != "/api/orders" {
		t.Errorf("url_pattern must stay unprefixed: got %q", got.URLPattern)
	}
}

func TestNormalize_EmptyPrefixLeavesPathUntouched(t *testing.T) {
	got := Normalize(prefixSpec("", ""))[0]
	if got.Path != "/api/orders" {
		t.Errorf("path: got %q want %q", got.Path, "/api/orders")
	}
	if got.URLPattern != "/api/orders" {
		t.Errorf("url_pattern: got %q want %q", got.URLPattern, "/api/orders")
	}
}

// Review Focus 4: an explicit url_pattern must not receive the prefix.
func TestNormalize_ExplicitURLPatternNeverPrefixed(t *testing.T) {
	got := Normalize(prefixSpec("/vitxo", "/internal/orders"))[0]
	if got.Path != "/vitxo/api/orders" {
		t.Errorf("path: got %q", got.Path)
	}
	if got.URLPattern != "/internal/orders" {
		t.Errorf("explicit url_pattern must survive verbatim: got %q", got.URLPattern)
	}
}

func TestNormalize_ProductBackendResolvesHost(t *testing.T) {
	got := Normalize(prefixSpec("/vitxo", ""))[0]
	if got.HostEnv != "FORGEOS_HOST" || got.HostDefault != "http://x:8080" {
		t.Errorf("host not resolved from product backend: %+v", got)
	}
}

func TestNormalize_EndpointBackendOverridesProduct(t *testing.T) {
	s := prefixSpec("/vitxo", "")
	s.Backends["other"] = Backend{HostDefault: "http://y:9090", HostEnv: "OTHER_HOST"}
	s.Endpoints[0].Backend = "other"
	got := Normalize(s)[0]
	if got.HostEnv != "OTHER_HOST" {
		t.Errorf("endpoint backend must win: got %q", got.HostEnv)
	}
}
