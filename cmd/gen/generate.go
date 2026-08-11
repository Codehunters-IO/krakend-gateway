package main

import (
	"encoding/json"
	"errors"

	"gopkg.in/yaml.v3"
)

// Generate runs the full YAML→JSON pipeline: parse, validate, normalize, marshal.
// Output is a top-level JSON array with 2-space indentation and no trailing newline.
func Generate(in []byte) ([]byte, error) {
	var spec Spec
	if err := yaml.Unmarshal(in, &spec); err != nil {
		return nil, err
	}
	if errs := Validate(spec); len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return json.MarshalIndent(Normalize(spec), "", "  ")
}
