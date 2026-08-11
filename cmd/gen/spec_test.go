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
	if len(spec.Endpoints) != 3 {
		t.Fatalf("endpoints: got %d want 3", len(spec.Endpoints))
	}
	if spec.Endpoints[0].Path != "/api/ping" {
		t.Errorf("path: got %q", spec.Endpoints[0].Path)
	}
	if !spec.Endpoints[2].DisableHostSanitize || spec.Endpoints[2].Timeout != "3600s" {
		t.Errorf("sse endpoint: sanitize=%v timeout=%q",
			spec.Endpoints[2].DisableHostSanitize, spec.Endpoints[2].Timeout)
	}
	b, ok := spec.Backends["forgeos"]
	if !ok || b.HostEnv != "FORGEOS_HOST" {
		t.Errorf("backend forgeos: got %+v ok=%v", b, ok)
	}
}
