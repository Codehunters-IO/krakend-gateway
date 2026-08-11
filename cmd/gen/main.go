package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// Defaults resolve relative to the repo root (where `make gen` runs).
const (
	defaultIn  = "endpoints.yaml"
	defaultOut = "config/settings/endpoints.json"
)

func main() {
	in, out := defaultIn, defaultOut
	if len(os.Args) > 1 {
		in = os.Args[1]
	}
	if len(os.Args) > 2 {
		out = os.Args[2]
	}
	raw, err := os.ReadFile(in)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read:", err)
		os.Exit(1)
	}
	j, err := Generate(raw)
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

// countEndpoints reports how many endpoints the generated document holds.
func countEndpoints(j []byte) int {
	var doc struct {
		Endpoints []json.RawMessage `json:"endpoints"`
	}
	if err := json.Unmarshal(j, &doc); err != nil {
		return 0
	}
	return len(doc.Endpoints)
}
