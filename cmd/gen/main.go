package main

import (
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
	fmt.Printf("wrote %s (%d endpoints)\n", out, countTop(j))
}

// countTop counts top-level array elements by counting the object-opening
// braces at indentation depth 2 ("  {"). Good enough for a status line.
func countTop(j []byte) int {
	n, s := 0, string(j)
	for i := 0; i+3 <= len(s); i++ {
		if s[i] == '\n' && s[i+1] == ' ' && s[i+2] == ' ' && s[i+3] == '{' {
			n++
		}
	}
	return n
}
