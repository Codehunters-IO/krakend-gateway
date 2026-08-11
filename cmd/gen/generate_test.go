package main

import (
	"os"
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
