package main

import (
	"encoding/json"
	"errors"

	"gopkg.in/yaml.v3"
)

// Output wraps the endpoint list in an object because KrakenD's Flexible
// Configuration unmarshals every settings file into a map — a top-level JSON
// array is rejected at load time. Templates reach the list as .endpoints.endpoints.
type Output struct {
	Endpoints []Endpoint `json:"endpoints"`
}

// Generate runs the full YAML→JSON pipeline: parse, validate, normalize, marshal.
// Output is a JSON object with 2-space indentation and no trailing newline.
func Generate(in []byte) ([]byte, error) {
	var spec Spec
	if err := yaml.Unmarshal(in, &spec); err != nil {
		return nil, err
	}
	if errs := Validate(spec); len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return json.MarshalIndent(Output{Endpoints: Normalize(spec)}, "", "  ")
}
