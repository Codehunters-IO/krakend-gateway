package main

import (
	"os"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestParseSampleYAML(t *testing.T) {
	raw, err := os.ReadFile("testdata/sample.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var spec Spec
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(spec.Endpoints) != 2 {
		t.Fatalf("endpoints: got %d want 2", len(spec.Endpoints))
	}
	if spec.Endpoints[0].Path != "/api/ping" {
		t.Errorf("path: got %q", spec.Endpoints[0].Path)
	}
	b, ok := spec.Backends["forgeos"]
	if !ok || b.HostEnv != "FORGEOS_HOST" {
		t.Errorf("backend forgeos: got %+v ok=%v", b, ok)
	}
}
