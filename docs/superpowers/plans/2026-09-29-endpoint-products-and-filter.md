# Endpoint products and per-product loading — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let `endpoints.yaml` group endpoints by product, give each product an optional URL prefix, and let `make gen PRODUCTS=forgeos` generate a gateway carrying only that product's routes.

**Architecture:** The generator (`cmd/gen`, four small Go files plus `main.go`) gains a `products` map. Each endpoint declares its product; the product supplies a default backend and an optional path prefix. The prefix is applied to the exposed `path` only — `url_pattern` keeps the unprefixed backend path, so KrakenD strips the prefix for free and the template needs no change. A `-products` flag filters the generated set. The whole `products` block is optional: a spec without it behaves exactly as today, which is what keeps the existing golden fixture valid.

**Tech Stack:** Go 1.25.7 (`cmd/gen`, no external deps beyond `gopkg.in/yaml.v3`), GNU Make, KrakenD 2.13.4 Flexible Configuration.

**Spec:** `docs/superpowers/specs/2026-09-29-multi-realm-jwt-design.md` — this plan implements the prerequisite named in its §2 ("Prerequisite, and the dependency runs one way") and §4 ("`endpoints.yaml` — the product declares its realm"), minus the `realm` field, which belongs to the follow-up plan.

## Global Constraints

- `endpoints.json` is committed and must stay byte-identical to `make gen` output on the full set; `make gen-check` enforces it with `git diff --exit-code`.
- Go 1.25.7, module `cmd/gen`, only existing dependencies. No new third-party packages.
- Every exposed route that works today must keep working: all three current products get `prefix: ""`.
- `Generate(in []byte)` keeps its current signature so existing call sites and the golden test are untouched; filtering arrives as a second function.
- Output is `json.MarshalIndent` with two-space indentation and no trailing newline; `main.go` appends the newline.
- Validation returns one error per violation and never stops at the first — `Validate` returns `[]error`, joined by `Generate`.
- Tests follow the file's existing conventions: a `base()` spec helper and `assertErrContains` in `validate_test.go`, a golden comparison in `generate_test.go`.
- Max line length 120 characters; functions stay short; comments explain why, not what.

## Review Focus

Five conditions the spec implies, that a reasonable person would hit, and that no task's happy path exercises. Each has a test pinned to the task owning the code.

1. **`PRODUCTS=forgoes` (a typo) must error, not silently produce zero endpoints.** A gateway that boots with no routes and no complaint is the worst outcome here. — Task 4.
2. **A filter selecting zero endpoints must error**, even when every name is spelled correctly (e.g. a product that exists but owns nothing). — Task 4.
3. **`prefix: "/vitxo/"` must error** rather than generating `//api/orders`, which KrakenD would accept as a distinct, unreachable-looking route. — Task 1.
4. **An explicit `url_pattern` must not receive the prefix.** The prefix belongs to the exposed path only; applying it to both would break the backend call in a way that looks like a routing bug. — Task 2.
5. **With `products` present, an endpoint omitting `product` must error**, not fall through to a zero-value lookup that silently yields no prefix and no backend. — Task 1.

---

### Task 1: The `products` block, and validation of it

**Files:**
- Modify: `cmd/gen/spec.go` (add `Product`, `Spec.Products`, `Endpoint.Product`)
- Modify: `cmd/gen/validate.go` (product rules + effective-backend resolution)
- Test: `cmd/gen/validate_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  - `type Product struct { Prefix string; Backend string }` with yaml keys `prefix`, `backend`.
  - `Spec.Products map[string]Product` (yaml `products`).
  - `Endpoint.Product string` (yaml `product`, `json:"-"` — not emitted).
  - `func effectiveBackend(spec Spec, e Endpoint) string` — returns `e.Backend` when set, else the product's `Backend`, else `""`. Used by Task 2 as well.

- [ ] **Step 1: Write the failing tests**

Add to `cmd/gen/validate_test.go`:

```go
func withProducts() Spec {
	s := base()
	s.Products = map[string]Product{
		"forgeos": {Prefix: "", Backend: "forgeos"},
		"vitxo":   {Prefix: "/vitxo", Backend: "forgeos"},
	}
	s.Endpoints[0].Product = "forgeos"
	s.Endpoints[0].Backend = ""
	return s
}

func TestValidate_ProductsOK(t *testing.T) {
	if errs := Validate(withProducts()); len(errs) != 0 {
		t.Fatalf("want no errors, got %v", errs)
	}
}

func TestValidate_UnknownProduct(t *testing.T) {
	s := withProducts()
	s.Endpoints[0].Product = "nope"
	assertErrContains(t, Validate(s), `product "nope" not declared`)
}

// Review Focus 5: products present, endpoint omits product.
func TestValidate_ProductRequiredWhenProductsDeclared(t *testing.T) {
	s := withProducts()
	s.Endpoints[0].Product = ""
	assertErrContains(t, Validate(s), "product required")
}

// Review Focus 3: a trailing slash would generate //api/...
func TestValidate_PrefixTrailingSlash(t *testing.T) {
	s := withProducts()
	s.Products["vitxo"] = Product{Prefix: "/vitxo/", Backend: "forgeos"}
	assertErrContains(t, Validate(s), "invalid prefix")
}

func TestValidate_PrefixMissingLeadingSlash(t *testing.T) {
	s := withProducts()
	s.Products["vitxo"] = Product{Prefix: "vitxo", Backend: "forgeos"}
	assertErrContains(t, Validate(s), "invalid prefix")
}

func TestValidate_EmptyPrefixIsValid(t *testing.T) {
	s := withProducts()
	s.Products["vitxo"] = Product{Prefix: "", Backend: "forgeos"}
	if errs := Validate(s); len(errs) != 0 {
		t.Fatalf("empty prefix must be valid, got %v", errs)
	}
}

func TestValidate_ProductSuppliesBackend(t *testing.T) {
	s := withProducts()
	s.Endpoints[0].Backend = "" // resolved from the product
	if errs := Validate(s); len(errs) != 0 {
		t.Fatalf("product backend must satisfy the backend rule, got %v", errs)
	}
}

func TestValidate_ProductBackendMustExist(t *testing.T) {
	s := withProducts()
	s.Products["forgeos"] = Product{Prefix: "", Backend: "ghost"}
	s.Endpoints[0].Backend = ""
	assertErrContains(t, Validate(s), `backend "ghost" not declared`)
}

// Back-compat: no products block at all stays valid.
func TestValidate_NoProductsBlockStillValid(t *testing.T) {
	if errs := Validate(base()); len(errs) != 0 {
		t.Fatalf("spec without products must stay valid, got %v", errs)
	}
}
```

- [ ] **Step 2: Run the tests and verify they fail**

Run: `cd cmd/gen && go test ./... -run TestValidate -v`
Expected: compile failure — `Product` and `Spec.Products` undefined.

- [ ] **Step 3: Add the types**

In `cmd/gen/spec.go`, add above `Defaults`:

```go
// Product groups endpoints that belong to one application. Prefix is prepended to
// the exposed path only, never to url_pattern, so the backend never sees it.
// Backend is the default for the product's endpoints; an endpoint may override it.
type Product struct {
	Prefix  string `yaml:"prefix"`
	Backend string `yaml:"backend"`
}
```

Add the field to `Endpoint`, next to `Backend`:

```go
	Product             string     `yaml:"product" json:"-"`
```

Add the map to `Spec`, as the first field:

```go
type Spec struct {
	Products  map[string]Product `yaml:"products"`
	Backends  map[string]Backend `yaml:"backends"`
	Defaults  Defaults           `yaml:"defaults"`
	Endpoints []Endpoint         `yaml:"endpoints"`
}
```

- [ ] **Step 4: Implement the validation**

In `cmd/gen/validate.go`, add the helper and the prefix rule, and change the backend rule to
use the helper:

```go
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
```

Inside `Validate`, before the endpoint loop:

```go
	for name, p := range spec.Products {
		if !validPrefix(p.Prefix) {
			errs = append(errs, fmt.Errorf("product[%s]: invalid prefix %q", name, p.Prefix))
		}
	}
```

Inside the endpoint loop, replace the backend rule with:

```go
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
```

- [ ] **Step 5: Run the tests and verify they pass**

Run: `cd cmd/gen && go test ./... -v`
Expected: PASS, including the pre-existing `TestGenerate_Golden` — `products` is absent from
`testdata/sample.yaml`, so nothing about its output changes.

- [ ] **Step 6: Commit**

```bash
git add cmd/gen/spec.go cmd/gen/validate.go cmd/gen/validate_test.go
git commit -m "feat(gen): products block with optional prefix and default backend"
```

---

### Task 2: Apply the prefix to the exposed path only

**Files:**
- Modify: `cmd/gen/normalize.go`
- Test: `cmd/gen/normalize_test.go`

**Interfaces:**
- Consumes: `Product`, `Spec.Products`, `Endpoint.Product`, `effectiveBackend` from Task 1.
- Produces: `Normalize` now returns endpoints whose `Path` carries the product prefix and whose
  `URLPattern` does not. No signature change: `func Normalize(spec Spec) []Endpoint`.

- [ ] **Step 1: Write the failing tests**

Add to `cmd/gen/normalize_test.go`:

```go
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
```

- [ ] **Step 2: Run the tests and verify they fail**

Run: `cd cmd/gen && go test ./... -run TestNormalize -v`
Expected: FAIL — `TestNormalize_PrefixAppliedToPathOnly` reports `path: got "/api/orders"`.

- [ ] **Step 3: Implement it**

In `cmd/gen/normalize.go`, inside the loop, replace the `URLPattern` and backend blocks with:

```go
		// url_pattern must be derived from the UNPREFIXED path: the prefix is the
		// edge's namespace, and the backend is never told about it. Deriving it
		// after prefixing would send /vitxo/api/orders upstream.
		if e.URLPattern == "" {
			e.URLPattern = e.Path
		}
		if p, ok := spec.Products[e.Product]; ok {
			e.Path = p.Prefix + e.Path
		}
		if b, ok := spec.Backends[effectiveBackend(spec, e)]; ok {
			e.HostEnv = b.HostEnv
			e.HostDefault = b.HostDefault
		}
```

- [ ] **Step 4: Run the tests and verify they pass**

Run: `cd cmd/gen && go test ./... -v`
Expected: PASS, golden included.

- [ ] **Step 5: Commit**

```bash
git add cmd/gen/normalize.go cmd/gen/normalize_test.go
git commit -m "feat(gen): apply the product prefix to the exposed path, not to url_pattern"
```

---

### Task 3: Detect collisions on the exposed path

**Files:**
- Modify: `cmd/gen/validate.go`
- Test: `cmd/gen/validate_test.go`

**Interfaces:**
- Consumes: Task 1's types and `validPrefix`.
- Produces: the existing duplicate rule now keys on the exposed path, so one check covers both
  a duplicate inside a product and a collision between two products.

- [ ] **Step 1: Write the failing test**

Add to `cmd/gen/validate_test.go`:

```go
func TestValidate_CrossProductPathCollision(t *testing.T) {
	s := withProducts()
	s.Products["vitxo"] = Product{Prefix: "", Backend: "forgeos"} // same namespace as forgeos
	s.Endpoints = append(s.Endpoints, Endpoint{
		Path: "/api/ping", Method: "GET", Product: "vitxo", Auth: "public",
		InputHeaders: []string{"Accept"},
	})
	assertErrContains(t, Validate(s), "duplicate endpoint")
}

func TestValidate_SamePathDifferentPrefixIsNotCollision(t *testing.T) {
	s := withProducts()
	s.Endpoints = append(s.Endpoints, Endpoint{
		Path: "/api/ping", Method: "GET", Product: "vitxo", Auth: "public",
		InputHeaders: []string{"Accept"},
	})
	if errs := Validate(s); len(errs) != 0 {
		t.Fatalf("/api/ping and /vitxo/api/ping must coexist, got %v", errs)
	}
}
```

- [ ] **Step 2: Run the tests and verify they fail**

Run: `cd cmd/gen && go test ./... -run TestValidate_.*Collision -v`
Expected: FAIL — `TestValidate_CrossProductPathCollision` finds no error, because the current
`seen` key uses the unprefixed path.

- [ ] **Step 3: Implement it**

In `cmd/gen/validate.go`, replace the duplicate-detection lines with:

```go
		// Key on the EXPOSED path. Two products can each declare /api/ping as long as
		// their prefixes differ; with equal prefixes it is a real collision, and
		// KrakenD would silently serve whichever endpoint it saw first.
		key := e.Method + " " + spec.Products[e.Product].Prefix + e.Path
		if seen[key] {
			errs = append(errs, fmt.Errorf("%s: duplicate endpoint", where))
		}
		seen[key] = true
```

- [ ] **Step 4: Run the tests and verify they pass**

Run: `cd cmd/gen && go test ./... -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/gen/validate.go cmd/gen/validate_test.go
git commit -m "feat(gen): detect endpoint collisions on the exposed path"
```

---

### Task 4: The product filter and its marker

**Files:**
- Modify: `cmd/gen/generate.go`
- Modify: `cmd/gen/main.go`
- Test: `cmd/gen/generate_test.go`

**Interfaces:**
- Consumes: Task 1's `Spec.Products` and `Endpoint.Product`.
- Produces:
  - `func GenerateWithProducts(in []byte, products []string) ([]byte, error)`.
  - `func Generate(in []byte) ([]byte, error)` unchanged, now `return GenerateWithProducts(in, nil)`.
  - `Output` gains `FilteredProducts []string \`json:"filtered_products,omitempty"\``, absent
    when unfiltered.
  - `main.go` accepts `-products=a,b` before the positional `in` and `out` paths.

- [ ] **Step 1: Write the failing tests**

Add to `cmd/gen/generate_test.go`:

```go
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
```

Add `"encoding/json"` and `"strings"` to that file's imports.

- [ ] **Step 2: Run the tests and verify they fail**

Run: `cd cmd/gen && go test ./... -run TestGenerateWithProducts -v`
Expected: compile failure — `GenerateWithProducts` undefined.

- [ ] **Step 3: Implement it**

Rewrite `cmd/gen/generate.go`:

```go
package main

import (
	"encoding/json"
	"errors"
	"fmt"

	"gopkg.in/yaml.v3"
)

// Output wraps the endpoint list in an object because KrakenD's Flexible
// Configuration unmarshals every settings file into a map — a top-level JSON
// array is rejected at load time. Templates reach the list as .endpoints.endpoints.
//
// FilteredProducts is emitted only for a filtered build. The committed
// endpoints.json is always the full set, so its presence in a diff is what
// tells a reviewer that a partial file was committed by mistake.
type Output struct {
	FilteredProducts []string   `json:"filtered_products,omitempty"`
	Endpoints        []Endpoint `json:"endpoints"`
}

// Generate runs the full YAML→JSON pipeline on every product.
func Generate(in []byte) ([]byte, error) {
	return GenerateWithProducts(in, nil)
}

// GenerateWithProducts is Generate restricted to the named products. A nil or
// empty list means all of them.
func GenerateWithProducts(in []byte, products []string) ([]byte, error) {
	var spec Spec
	if err := yaml.Unmarshal(in, &spec); err != nil {
		return nil, err
	}
	if errs := Validate(spec); len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	out := Output{Endpoints: Normalize(spec)}
	if len(products) > 0 {
		if err := checkProductsExist(spec, products); err != nil {
			return nil, err
		}
		out.Endpoints = filterByProducts(spec, out.Endpoints, products)
		if len(out.Endpoints) == 0 {
			return nil, fmt.Errorf("products %v select no endpoints: the gateway would have no routes", products)
		}
		out.FilteredProducts = products
	}
	return json.MarshalIndent(out, "", "  ")
}

// checkProductsExist rejects a name absent from the spec. A typo must not read
// as "load nothing" — a gateway that boots with no routes and no error is worse
// than a failed build.
func checkProductsExist(spec Spec, products []string) error {
	var errs []error
	for _, name := range products {
		if _, ok := spec.Products[name]; !ok {
			errs = append(errs, fmt.Errorf("product %q not declared in the spec", name))
		}
	}
	return errors.Join(errs...)
}

// filterByProducts keeps the endpoints whose product is named, preserving order.
// It indexes the normalized list against the spec by position, which Normalize
// guarantees: it returns endpoints in input order.
func filterByProducts(spec Spec, normalized []Endpoint, products []string) []Endpoint {
	wanted := make(map[string]bool, len(products))
	for _, name := range products {
		wanted[name] = true
	}
	kept := make([]Endpoint, 0, len(normalized))
	for i, e := range normalized {
		if wanted[spec.Endpoints[i].Product] {
			kept = append(kept, e)
		}
	}
	return kept
}
```

- [ ] **Step 4: Add the flag to `main.go`**

Replace the argument handling in `cmd/gen/main.go`:

```go
func main() {
	products := flag.String("products", "", "comma-separated product names to load; empty loads all")
	flag.Parse()

	in, out := defaultIn, defaultOut
	if args := flag.Args(); len(args) > 0 {
		in = args[0]
		if len(args) > 1 {
			out = args[1]
		}
	}
	raw, err := os.ReadFile(in)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read:", err)
		os.Exit(1)
	}
	j, err := GenerateWithProducts(raw, splitProducts(*products))
	if err != nil {
		fmt.Fprintln(os.Stderr, "generate:\n"+err.Error())
		os.Exit(1)
	}
	if err := os.WriteFile(out, append(j, '\n'), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "write:", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s (%d endpoints)\n", out, countEndpoints(j))
}

// splitProducts turns "a,b" into ["a","b"] and "" into nil, trimming spaces so
// PRODUCTS="forgeos, vitxo" behaves the way anyone would expect from a Make variable.
func splitProducts(csv string) []string {
	if strings.TrimSpace(csv) == "" {
		return nil
	}
	parts := strings.Split(csv, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
```

Add `"flag"` and `"strings"` to the imports.

- [ ] **Step 5: Run the tests and verify they pass**

Run: `cd cmd/gen && go test ./... -v`
Expected: PASS.

Then confirm the flag is wired by pointing it at the real spec, which has no `products` block
yet — this task runs before Task 6:

```bash
cd cmd/gen && go run . -products=forgeos ../../endpoints.yaml /tmp/filtered.json
```
Expected: exit 1 with `generate:` followed by `product "forgeos" not declared in the spec`, and
no file written. That failure is the success criterion here: it proves the flag reached
`checkProductsExist`. Task 6 Step 6 exercises the passing path once the spec declares products.

- [ ] **Step 6: Commit**

```bash
git add cmd/gen/generate.go cmd/gen/main.go cmd/gen/generate_test.go
git commit -m "feat(gen): -products filter with an explicit marker on partial builds"
```

---

### Task 5: Makefile wiring, and a guard on `gen-check`

**Files:**
- Modify: `Makefile`

**Interfaces:**
- Consumes: the `-products` flag from Task 4.
- Produces: `PRODUCTS` as a Make variable, honoured by `gen`; `gen-check` refuses to run with it
  set.

- [ ] **Step 1: Add the variable and pass it through**

In `Makefile`, below `ENDPOINTS_JSON`:

```make
# Consumed by gen/check to filter which products are included in endpoints.json.
# Empty (default) includes all products. To load a subset locally, run:
#   make gen PRODUCTS=a,b
# Then start the stack:
#   make dev
# Note: make dev does not regenerate endpoints.json; it uses what's on disk.
PRODUCTS ?=
```

Replace the `gen` and `gen-check` targets:

```make
gen: ## Regenerate $(ENDPOINTS_JSON) from $(ENDPOINTS_SPEC) (PRODUCTS= filters)
	@cd $(GEN_DIR) && go run . $(if $(PRODUCTS),-products=$(PRODUCTS),) "$(CURDIR)/$(ENDPOINTS_SPEC)" "$(CURDIR)/$(ENDPOINTS_JSON)"

gen-check: ## Fail if endpoints.json is out of sync with endpoints.yaml
	@if [ -n "$(PRODUCTS)" ]; then \
		echo "gen-check: refusing to run with PRODUCTS=$(PRODUCTS)."; \
		echo "  The committed endpoints.json is always the full set, so a drift check"; \
		echo "  against a filtered build would always fail. Run 'make check' with no"; \
		echo "  PRODUCTS, or 'make gen' to restore the full file."; \
		exit 1; \
	fi
	@$(MAKE) --no-print-directory gen
	@git diff --exit-code $(ENDPOINTS_JSON)
```

The flag goes before the positional paths: Go's `flag` package stops parsing at the first
non-flag argument.

- [ ] **Step 2: Verify the guard and the pass-through**

Run: `make gen-check PRODUCTS=forgeos`
Expected: exits 1 with the "refusing to run" message and no file written.

Run: `make check`
Expected: unchanged from today — `Syntax OK!` plus the chain-order assertion.

- [ ] **Step 3: Commit**

```bash
git add Makefile
git commit -m "build(gen): PRODUCTS variable, and keep gen-check on the full set"
```

---

### Task 6: Migrate `endpoints.yaml`, and prove no route changed

**Files:**
- Modify: `endpoints.yaml` (add `products`, add `product:` to all 31 endpoints)
- Modify: `config/settings/endpoints.json` (regenerated)

**Interfaces:**
- Consumes: everything from Tasks 1–5.
- Produces: a spec where every endpoint belongs to a product, and a regenerated
  `endpoints.json` whose exposed routes are byte-identical to the previous one.

- [ ] **Step 1: Capture the current output as the baseline**

```bash
cp config/settings/endpoints.json /tmp/endpoints-before.json
```

- [ ] **Step 2: Add the products block**

In `endpoints.yaml`, immediately above `backends:`:

```yaml
# A product is the unit of loading. PRODUCTS is consumed by gen/check, not dev:
# to load a subset, run `make gen PRODUCTS=a,b` then `make dev` — dev does not
# regenerate endpoints.json, it uses what's on disk. `prefix` is prepended to the
# exposed path and never reaches the backend, so today's three products keep
# prefix "" and no route moves.
products:
  forgeos:
    prefix: ""
    backend: forgeos
  knowledge:
    prefix: ""
    backend: knowledge
  platform:
    prefix: ""
    backend: auth_bff
```

- [ ] **Step 3: Tag every endpoint with its product**

Add `product:` to each of the 31 endpoints, matching the backend each one already uses:

- `product: platform` — the five `/auth/*` routes (`/auth/login/{provider}`, `/auth/callback`,
  `/auth/session`, `/auth/logout`, `/auth/backchannel-logout`).
- `product: knowledge` — the eight on the knowledge backend (`/mcp` GET/POST/DELETE,
  `/api/v1/collections`, `/api/v1/projects`, `/api/v1/documents`, `/api/v1/memories`,
  `/api/v1/search`).
- `product: forgeos` — the remaining eighteen (`/api/ping`, `/api/organizations` and its
  members route, the `/api/projects*` set, the `/api/stories*` set, `/api/agent-runs/{id}/stream`).

Keep each endpoint's existing `backend:` line. It is now redundant with the product default, but
removing 31 lines in the same commit that adds the products block would make the diff impossible
to review. A follow-up commit can drop them.

Verify the split before regenerating:

```bash
grep -c 'product: forgeos'   endpoints.yaml   # expect 18
grep -c 'product: knowledge' endpoints.yaml   # expect 8
grep -c 'product: platform'  endpoints.yaml   # expect 5
```

- [ ] **Step 4: Regenerate and prove nothing moved**

```bash
make gen
diff <(jq -S '.endpoints' /tmp/endpoints-before.json) <(jq -S '.endpoints' config/settings/endpoints.json)
```
Expected: no output. Every prefix is `""`, `product` is `json:"-"`, and no other field changed,
so the endpoint array must be identical. If `diff` prints anything, stop and find out why before
committing — that output is a route change nobody asked for.

- [ ] **Step 5: Validate the whole configuration**

Run: `make check`
Expected: `Syntax OK!` and `check-plugin-chain-order: OK`.

- [ ] **Step 6: Exercise the filter for real**

```bash
make gen PRODUCTS=knowledge
jq '.filtered_products, (.endpoints | length)' config/settings/endpoints.json
```
Expected: `["knowledge"]` and `8`.

Then restore the committed full set and confirm it is clean:

```bash
make gen && git diff --exit-code config/settings/endpoints.json && echo "restored clean"
```

- [ ] **Step 7: Commit**

```bash
git add endpoints.yaml config/settings/endpoints.json
git commit -m "feat(endpoints): group the 31 endpoints into forgeos, knowledge and platform"
```

---

### Task 7: Document it

**Files:**
- Modify: `README.md` (the "Endpoints (generador)" section and the command table)

**Interfaces:**
- Consumes: the finished behaviour from Tasks 1–6.
- Produces: no code.

> **Note for the implementer:** `README.md` has a pending rewrite in PR #5
> (`docs/align-readme-with-config`). If that PR has merged, the section headings below exist as
> described. If it has not, rebase onto it or place this documentation in whichever section
> describes the generator, and flag the overlap in the pull request rather than resolving it
> silently.

- [ ] **Step 1: Document the products block**

In the `### Esquema de \`endpoints.yaml\`` subsection, add the block to the YAML example, above
`backends:`:

```yaml
products:                              # unidad de carga; PRODUCTS= filtra por estas claves
  forgeos:
    prefix: ""                         # "" o ruta con / inicial y sin / final
    backend: forgeos                   # default para sus endpoints
```

And add these three rows to the field table, matching its existing column layout
(`Campo` / `Obligatorio` / `Descripcion`):

| Campo | Obligatorio | Descripcion |
|-------|-------------|-------------|
| `product` | si, cuando existe el bloque `products` | Clave de `products` a la que pertenece el endpoint |
| `products.<n>.prefix` | no | Se antepone a la ruta expuesta, nunca al `url_pattern`. `""` o ruta con `/` inicial y sin `/` final |
| `products.<n>.backend` | no | Backend por defecto del producto; el `backend` del endpoint gana |

- [ ] **Step 2: Document the filter**

Add a subsection after "Anadir o cambiar un endpoint":

```markdown
### Cargar un subconjunto de productos

`PRODUCTS` lo consumen `gen`/`gen-check`, no `dev`: `dev: plugin-build up`, y ninguno de
los dos targets depende de `gen`, `generate` ni `check`, asi que `PRODUCTS` no se aplica
por ese camino. Para cargar un subconjunto son dos pasos explicitos:

```bash
make gen                           # todos los productos (default)
make gen PRODUCTS=forgeos          # solo forgeos
make gen PRODUCTS=forgeos,platform # forgeos + el flujo de login
make dev                           # o `make up` si los plugins ya estan compilados
```

`make dev` no regenera nada — usa lo que haya en disco. Una generacion filtrada escribe
`filtered_products` en `endpoints.json`, que es el set **completo** cuando se commitea.
`make gen-check` se niega a correr con `PRODUCTS` puesto, y `make gen` sin filtro
restaura el fichero. Si un `endpoints.json` filtrado se cuela en un commit, el
`gen-check` de CI lo caza por drift.
```

- [ ] **Step 3: Add the variable to the command table**

Add a row: `` `make gen PRODUCTS=a,b` `` — regenerates carrying only those products.

- [ ] **Step 4: Verify the documented commands actually work**

Run each command block from the new section and confirm the described output. Documentation that
was never executed is how the stale sections this repo just finished correcting came to exist.

- [ ] **Step 5: Commit**

```bash
git add README.md
git commit -m "docs: document endpoint products and PRODUCTS filtering"
```

---

## What this plan does not do

- **No `realm` field.** It belongs to the multi-realm plan, which consumes the `products` block
  this one creates. Adding it here without the plugin that reads it would ship dead
  configuration.
- **No template change.** `krakend.tmpl` already emits `endpoint` from `path` and the backend
  from `url_pattern`, which is exactly why the prefix costs nothing.
- **No removal of the now-redundant `backend:` lines** on the 31 endpoints. Mentioned in Task 6,
  deliberately left for a separate commit so the migration diff stays reviewable.
- **The `make dev PRODUCTS=...` examples in this plan were wrong.** `dev: plugin-build up`, and
  neither `dev` nor `up` depends on `gen`, `generate` or `check`, so `PRODUCTS` is never consumed
  on that path. Corrected to the two-step form (`make gen PRODUCTS=a,b` then `make dev`/`make up`)
  during execution of Task 7.
