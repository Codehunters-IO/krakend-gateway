package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestGenerate_Golden(t *testing.T) {
	in, err := os.ReadFile("testdata/sample.yaml")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Generate(in)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	want, err := os.ReadFile("testdata/expected.json")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("golden mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestGenerate_ValidationErrorsAbort(t *testing.T) {
	in := []byte("backends:\n  forgeos: {host_default: x, host_env: Y}\nendpoints:\n  - path: bad\n    method: GET\n    backend: forgeos\n    auth: public\n    input_headers: [Accept]\n")
	if _, err := Generate(in); err == nil {
		t.Fatal("want error for invalid path, got nil")
	}
}

const twoProductSpec = `
products:
  forgeos: {prefix: "", backend: forgeos}
  vitxo:   {prefix: /vitxo, backend: forgeos}
backends:
  forgeos: {host_default: "http://x:8080", host_env: FORGEOS_HOST}
endpoints:
  - {path: /api/ping, method: GET, product: forgeos, auth: public, input_headers: [Accept]}
  - {path: /api/orders, method: POST, product: vitxo, auth: protected, input_headers: [Accept]}
`

func decode(t *testing.T, j []byte) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(j, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return doc
}

func TestGenerateWithProducts_SelectsSubset(t *testing.T) {
	j, err := GenerateWithProducts([]byte(twoProductSpec), []string{"vitxo"})
	if err != nil {
		t.Fatal(err)
	}
	doc := decode(t, j)
	eps := doc["endpoints"].([]any)
	if len(eps) != 1 {
		t.Fatalf("want 1 endpoint, got %d", len(eps))
	}
	if got := eps[0].(map[string]any)["path"]; got != "/vitxo/api/orders" {
		t.Errorf("path: got %v", got)
	}
	if doc["filtered_products"] == nil {
		t.Error("filtered_products must be present when filtering")
	}
}

func TestGenerateWithProducts_NilLoadsEverything(t *testing.T) {
	j, err := GenerateWithProducts([]byte(twoProductSpec), nil)
	if err != nil {
		t.Fatal(err)
	}
	doc := decode(t, j)
	if len(doc["endpoints"].([]any)) != 2 {
		t.Errorf("want 2 endpoints, got %d", len(doc["endpoints"].([]any)))
	}
	if _, present := doc["filtered_products"]; present {
		t.Error("filtered_products must be absent when not filtering")
	}
}

// Review Focus 1: a typo must not silently yield an empty gateway.
func TestGenerateWithProducts_UnknownProductErrors(t *testing.T) {
	_, err := GenerateWithProducts([]byte(twoProductSpec), []string{"forgoes"})
	if err == nil {
		t.Fatal("want error for undeclared product, got nil")
	}
	if !strings.Contains(err.Error(), "forgoes") {
		t.Errorf("error must name the offending product: %v", err)
	}
}

// Review Focus 2: a correctly spelled product that owns nothing is still an empty gateway.
const emptyProductSpec = `
products:
  forgeos: {prefix: "", backend: forgeos}
  empty:   {prefix: /empty, backend: forgeos}
backends:
  forgeos: {host_default: "http://x:8080", host_env: FORGEOS_HOST}
endpoints:
  - {path: /api/ping, method: GET, product: forgeos, auth: public, input_headers: [Accept]}
`

func TestGenerateWithProducts_EmptyResultErrors(t *testing.T) {
	_, err := GenerateWithProducts([]byte(emptyProductSpec), []string{"empty"})
	if err == nil {
		t.Fatal("want error when the filter selects no endpoints, got nil")
	}
	if !strings.Contains(err.Error(), "no routes") {
		t.Errorf("error must say the gateway would have no routes: %v", err)
	}
}

// Fix round 2: filterByProducts selects by each endpoint's own Product field,
// with no spec argument at all — there is no positional correspondence to a
// declared slice to get wrong, so this is possible without a spec in hand.
func TestFilterByProducts_SelectsByEndpointsOwnProduct(t *testing.T) {
	normalized := []Endpoint{
		{Path: "/api/ping", Method: "GET", Product: "forgeos"},
		{Path: "/vitxo/api/orders", Method: "POST", Product: "vitxo"},
	}
	kept := filterByProducts(normalized, []string{"vitxo"})
	if len(kept) != 1 {
		t.Fatalf("want 1 endpoint, got %d", len(kept))
	}
	if kept[0].Path != "/vitxo/api/orders" {
		t.Errorf("path: got %v", kept[0].Path)
	}
}
