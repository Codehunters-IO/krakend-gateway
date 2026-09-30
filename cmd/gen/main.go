package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
)

// Defaults resolve relative to the repo root (where `make gen` runs).
const (
	defaultIn  = "endpoints.yaml"
	defaultOut = "config/settings/endpoints.json"
)

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
