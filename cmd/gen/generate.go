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
// empty list means all of them. The output is a JSON object with 2-space
// indentation and no trailing newline — the committed endpoints.json must
// stay byte-identical, so that contract is binding, not cosmetic.
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
		out.Endpoints = filterByProducts(out.Endpoints, products)
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
// It reads each endpoint's own Product field — Normalize copies the whole
// Endpoint value, including Product, into its output, and json:"-" only keeps
// the field out of the emitted JSON, not out of the struct in memory. So there
// is no positional correspondence between a declared and a normalized slice to
// maintain here. The invariant "Normalize must not clear Product" still holds;
// what changed is that breaking it now fails loudly (routes vanish from the
// filtered output) instead of silently misattributing routes to the wrong product.
func filterByProducts(normalized []Endpoint, products []string) []Endpoint {
	wanted := make(map[string]bool, len(products))
	for _, name := range products {
		wanted[name] = true
	}
	kept := make([]Endpoint, 0, len(normalized))
	for _, e := range normalized {
		if wanted[e.Product] {
			kept = append(kept, e)
		}
	}
	return kept
}
