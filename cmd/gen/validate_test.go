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

func assertErrContains(t *testing.T, errs []error, want string) {
	t.Helper()
	for _, e := range errs {
		if strings.Contains(e.Error(), want) {
			return
		}
	}
	t.Fatalf("want an error containing %q, got %v", want, errs)
}
