# KrakenD Endpoint Config Generator — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the hand-edited endpoint blocks in `config/krakend.tmpl` with a compact `endpoints.yaml` source of truth that a Go generator expands into a committed `config/settings/endpoints.json`, which KrakenD's Flexible Configuration `range`-expands at render time.

**Architecture:** `endpoints.yaml` → `cmd/gen` (Go: validate + normalize + YAML→JSON) → `config/settings/endpoints.json` (committed) → `{{ range .endpoints }}` in `krakend.tmpl` → `krakend check` validates. Public endpoints derive their `skip_paths` entry from the same source. Three validation layers catch errors early.

**Tech Stack:** Go 1.22+ (stdlib `encoding/json`, `gopkg.in/yaml.v3`), KrakenD 2.13.4 Community (Flexible Configuration / Go templates), Docker (for `krakend check`), Make.

## Global Constraints

- KrakenD image: `krakend:2.13.4` (Community Edition). No Enterprise features.
- Generator output MUST be deterministic: input order preserved, no timestamps, stable JSON indentation (2 spaces). Clean git diffs.
- `config/settings/endpoints.json` IS committed (gateway must boot without a Go toolchain).
- `endpoints.json` is a top-level JSON **array** so KrakenD FC exposes it as `.endpoints` (FC keys each settings file by filename; the file's value becomes the variable).
- The generator NEVER resolves `env` — it copies the envvar *name* only; `env` is resolved by KrakenD at runtime.
- `input_headers` are explicit per endpoint (no inheritance) — auditability of which identity headers reach the backend.
- `auth` (`public`|`protected`) drives `skip_paths` membership only; it does not touch headers.
- Go module for the generator lives at `cmd/gen/` with its own `go.mod` (mirrors the per-plugin module layout under `plugins/`).

---

## Task 1: Scaffold generator module, Spec types, YAML parsing

**Files:**
- Create: `cmd/gen/go.mod`
- Create: `cmd/gen/spec.go`
- Test: `cmd/gen/spec_test.go`
- Create: `cmd/gen/testdata/sample.yaml`

**Interfaces:**
- Produces: `Spec`, `Backend`, `Defaults`, `RateLimit`, `Endpoint` structs (see below); parsed via `yaml.Unmarshal([]byte, *Spec)`.

- [ ] **Step 1: Create the Go module**

Run:
```bash
cd cmd/gen && go mod init gen && go get gopkg.in/yaml.v3@v3.0.1
```
Expected: `cmd/gen/go.mod` + `go.sum` created with the yaml.v3 dependency.

- [ ] **Step 2: Write the type definitions**

Create `cmd/gen/spec.go`:
```go
package main

// Backend maps a logical key to a default host and the envvar that overrides it.
type Backend struct {
	HostDefault string `yaml:"host_default" json:"host_default"`
	HostEnv     string `yaml:"host_env" json:"host_env"`
}

// Defaults are applied to endpoints that omit the corresponding field.
type Defaults struct {
	OutputEncoding string `yaml:"output_encoding"`
	Encoding       string `yaml:"encoding"`
	Timeout        string `yaml:"timeout"`
}

// RateLimit is an optional per-endpoint qos/ratelimit/router config.
type RateLimit struct {
	MaxRate       float64 `yaml:"max_rate" json:"max_rate"`
	ClientMaxRate float64 `yaml:"client_max_rate" json:"client_max_rate"`
	Strategy      string  `yaml:"strategy" json:"strategy"`
}

// Endpoint is one route. Fields with json:"-" are consumed during normalize
// and not emitted; the rest are emitted into endpoints.json in this order.
type Endpoint struct {
	Path              string     `yaml:"path" json:"path"`
	Method            string     `yaml:"method" json:"method"`
	Backend           string     `yaml:"backend" json:"-"`
	Auth              string     `yaml:"auth" json:"auth"`
	URLPattern        string     `yaml:"url_pattern" json:"url_pattern"`
	OutputEncoding    string     `yaml:"output_encoding" json:"output_encoding"`
	Encoding          string     `yaml:"encoding" json:"encoding"`
	HostEnv           string     `yaml:"-" json:"host_env"`
	HostDefault       string     `yaml:"-" json:"host_default"`
	InputHeaders      []string   `yaml:"input_headers" json:"input_headers"`
	InputQueryStrings []string   `yaml:"input_query_strings" json:"input_query_strings"`
	Timeout           string     `yaml:"timeout" json:"timeout"`
	RateLimit         *RateLimit `yaml:"rate_limit" json:"rate_limit"`
}

// Spec is the full endpoints.yaml document.
type Spec struct {
	Backends  map[string]Backend `yaml:"backends"`
	Defaults  Defaults           `yaml:"defaults"`
	Endpoints []Endpoint         `yaml:"endpoints"`
}
```

- [ ] **Step 3: Create the test fixture**

Create `cmd/gen/testdata/sample.yaml`:
```yaml
backends:
  forgeos:
    host_default: http://host.docker.internal:8080
    host_env: FORGEOS_HOST
defaults:
  output_encoding: no-op
  encoding: no-op
  timeout: null
endpoints:
  - path: /api/ping
    method: GET
    backend: forgeos
    auth: public
    input_headers: [Accept, Traceparent]
  - path: /api/projects
    method: GET
    backend: forgeos
    auth: protected
    input_headers: [Accept, Authorization, Traceparent]
```

- [ ] **Step 4: Write the failing parse test**

Create `cmd/gen/spec_test.go`:
```go
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
```

- [ ] **Step 5: Run the test**

Run: `cd cmd/gen && go test ./... -run TestParseSampleYAML -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add cmd/gen/go.mod cmd/gen/go.sum cmd/gen/spec.go cmd/gen/spec_test.go cmd/gen/testdata/sample.yaml
git commit -m "feat(gen): scaffold endpoint generator module + spec types"
```

---

## Task 2: Validation

**Files:**
- Create: `cmd/gen/validate.go`
- Test: `cmd/gen/validate_test.go`

**Interfaces:**
- Consumes: `Spec` (Task 1).
- Produces: `func Validate(spec Spec) []error` — one error per rule violation, message prefixed with `endpoint[i]` where applicable. Empty slice = valid.

- [ ] **Step 1: Write the failing tests**

Create `cmd/gen/validate_test.go`:
```go
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

func assertErrContains(t *testing.T, errs []error, want string) {
	t.Helper()
	for _, e := range errs {
		if strings.Contains(e.Error(), want) {
			return
		}
	}
	t.Fatalf("want an error containing %q, got %v", want, errs)
}
```

- [ ] **Step 2: Run to verify failure**

Run: `cd cmd/gen && go test ./... -run TestValidate -v`
Expected: FAIL — `Validate` undefined.

- [ ] **Step 3: Implement Validate**

Create `cmd/gen/validate.go`:
```go
package main

import (
	"fmt"
	"strings"
)

var validMethods = map[string]bool{
	"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true,
}

// Validate returns one error per rule violation. Empty slice means the spec is valid.
func Validate(spec Spec) []error {
	var errs []error
	seen := map[string]bool{}
	for i, e := range spec.Endpoints {
		where := fmt.Sprintf("endpoint[%d] %s %s", i, e.Method, e.Path)
		if e.Path == "" || !strings.HasPrefix(e.Path, "/") {
			errs = append(errs, fmt.Errorf("%s: invalid path", where))
		}
		if !validMethods[e.Method] {
			errs = append(errs, fmt.Errorf("%s: unknown method %q", where, e.Method))
		}
		if _, ok := spec.Backends[e.Backend]; !ok {
			errs = append(errs, fmt.Errorf("%s: backend %q not declared", where, e.Backend))
		}
		if len(e.InputHeaders) == 0 {
			errs = append(errs, fmt.Errorf("%s: input_headers required", where))
		}
		if e.Auth != "public" && e.Auth != "protected" {
			errs = append(errs, fmt.Errorf("%s: invalid auth %q", where, e.Auth))
		}
		key := e.Method + " " + e.Path
		if seen[key] {
			errs = append(errs, fmt.Errorf("%s: duplicate endpoint", where))
		}
		seen[key] = true
	}
	return errs
}
```

- [ ] **Step 4: Run to verify pass**

Run: `cd cmd/gen && go test ./... -run TestValidate -v`
Expected: PASS (all 7 cases).

- [ ] **Step 5: Commit**

```bash
git add cmd/gen/validate.go cmd/gen/validate_test.go
git commit -m "feat(gen): endpoint spec validation"
```

---

## Task 3: Normalization

**Files:**
- Create: `cmd/gen/normalize.go`
- Test: `cmd/gen/normalize_test.go`

**Interfaces:**
- Consumes: `Spec` (Task 1).
- Produces: `func Normalize(spec Spec) []Endpoint` — applies defaults, sets `URLPattern=Path` when empty, resolves `HostEnv`/`HostDefault` from `Backends[e.Backend]`, and replaces nil `InputQueryStrings` with an empty slice (stable `[]` in JSON).

- [ ] **Step 1: Write the failing tests**

Create `cmd/gen/normalize_test.go`:
```go
package main

import "testing"

func specForNormalize() Spec {
	return Spec{
		Backends: map[string]Backend{"forgeos": {HostDefault: "http://x:8080", HostEnv: "FORGEOS_HOST"}},
		Defaults: Defaults{OutputEncoding: "no-op", Encoding: "no-op", Timeout: ""},
		Endpoints: []Endpoint{
			{Path: "/api/ping", Method: "GET", Backend: "forgeos", Auth: "public", InputHeaders: []string{"Accept"}},
		},
	}
}

func TestNormalize_AppliesDefaultsAndHost(t *testing.T) {
	out := Normalize(specForNormalize())
	e := out[0]
	if e.OutputEncoding != "no-op" || e.Encoding != "no-op" {
		t.Errorf("encodings: %q/%q", e.OutputEncoding, e.Encoding)
	}
	if e.URLPattern != "/api/ping" {
		t.Errorf("url_pattern: got %q want /api/ping", e.URLPattern)
	}
	if e.HostEnv != "FORGEOS_HOST" || e.HostDefault != "http://x:8080" {
		t.Errorf("host resolution: %q / %q", e.HostEnv, e.HostDefault)
	}
	if e.InputQueryStrings == nil {
		t.Error("input_query_strings should be non-nil empty slice, got nil")
	}
}

func TestNormalize_KeepsExplicitURLPatternAndEncoding(t *testing.T) {
	s := specForNormalize()
	s.Endpoints[0].URLPattern = "/backend/route"
	s.Endpoints[0].OutputEncoding = "json"
	out := Normalize(s)
	if out[0].URLPattern != "/backend/route" {
		t.Errorf("url_pattern overridden: %q", out[0].URLPattern)
	}
	if out[0].OutputEncoding != "json" {
		t.Errorf("output_encoding overridden: %q", out[0].OutputEncoding)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `cd cmd/gen && go test ./... -run TestNormalize -v`
Expected: FAIL — `Normalize` undefined.

- [ ] **Step 3: Implement Normalize**

Create `cmd/gen/normalize.go`:
```go
package main

// Normalize applies defaults and resolves derived fields, returning the
// endpoints in input order ready for JSON emission.
func Normalize(spec Spec) []Endpoint {
	out := make([]Endpoint, len(spec.Endpoints))
	for i, e := range spec.Endpoints {
		if e.OutputEncoding == "" {
			e.OutputEncoding = spec.Defaults.OutputEncoding
		}
		if e.Encoding == "" {
			e.Encoding = spec.Defaults.Encoding
		}
		if e.Timeout == "" {
			e.Timeout = spec.Defaults.Timeout
		}
		if e.URLPattern == "" {
			e.URLPattern = e.Path
		}
		if b, ok := spec.Backends[e.Backend]; ok {
			e.HostEnv = b.HostEnv
			e.HostDefault = b.HostDefault
		}
		if e.InputQueryStrings == nil {
			e.InputQueryStrings = []string{}
		}
		out[i] = e
	}
	return out
}
```

- [ ] **Step 4: Run to verify pass**

Run: `cd cmd/gen && go test ./... -run TestNormalize -v`
Expected: PASS (both cases).

- [ ] **Step 5: Commit**

```bash
git add cmd/gen/normalize.go cmd/gen/normalize_test.go
git commit -m "feat(gen): endpoint normalization (defaults, host, url_pattern)"
```

---

## Task 4: Generate pipeline + main + golden test

**Files:**
- Create: `cmd/gen/generate.go`
- Create: `cmd/gen/main.go`
- Test: `cmd/gen/generate_test.go`
- Create: `cmd/gen/testdata/expected.json`

**Interfaces:**
- Consumes: `Validate` (Task 2), `Normalize` (Task 3), `Spec` (Task 1).
- Produces: `func Generate(in []byte) ([]byte, error)` — full YAML→JSON pipeline returning a top-level JSON array (2-space indent, no trailing newline). `main` reads a file, calls `Generate`, writes output + one trailing newline.

- [ ] **Step 1: Write the failing golden test**

Create `cmd/gen/generate_test.go`:
```go
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
```

- [ ] **Step 2: Create the expected golden file**

Create `cmd/gen/testdata/expected.json` (exact bytes the generator must produce for `sample.yaml` — 2-space indent, no trailing newline):
```json
[
  {
    "path": "/api/ping",
    "method": "GET",
    "auth": "public",
    "url_pattern": "/api/ping",
    "output_encoding": "no-op",
    "encoding": "no-op",
    "host_env": "FORGEOS_HOST",
    "host_default": "http://host.docker.internal:8080",
    "input_headers": [
      "Accept",
      "Traceparent"
    ],
    "input_query_strings": [],
    "timeout": "",
    "rate_limit": null
  },
  {
    "path": "/api/projects",
    "method": "GET",
    "auth": "protected",
    "url_pattern": "/api/projects",
    "output_encoding": "no-op",
    "encoding": "no-op",
    "host_env": "FORGEOS_HOST",
    "host_default": "http://host.docker.internal:8080",
    "input_headers": [
      "Accept",
      "Authorization",
      "Traceparent"
    ],
    "input_query_strings": [],
    "timeout": "",
    "rate_limit": null
  }
]
```

- [ ] **Step 3: Implement Generate**

Create `cmd/gen/generate.go`:
```go
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
```

- [ ] **Step 4: Implement main**

Create `cmd/gen/main.go`:
```go
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
```

- [ ] **Step 5: Run the golden test**

Run: `cd cmd/gen && go test ./... -v`
Expected: PASS. If the golden mismatches on field order, fix `expected.json` to match the struct field order in `spec.go` (the struct order IS the contract) — do not reorder the struct.

- [ ] **Step 6: Verify main runs end-to-end**

Run: `cd cmd/gen && go run . testdata/sample.yaml /tmp/out.json && diff <(cat /tmp/out.json) <(cat testdata/expected.json; echo)`
Expected: no diff (main writes expected.json content + one trailing newline).

- [ ] **Step 7: Commit**

```bash
git add cmd/gen/generate.go cmd/gen/main.go cmd/gen/generate_test.go cmd/gen/testdata/expected.json
git commit -m "feat(gen): YAML->JSON generate pipeline + CLI + golden test"
```

---

## Task 5: Author endpoints.yaml from the current 18 endpoints + generate

**Files:**
- Create: `endpoints.yaml`
- Create: `config/settings/endpoints.json` (generated)
- Read (source of truth for transcription): `config/krakend.tmpl`

**Interfaces:**
- Consumes: `cmd/gen` CLI (Task 4).
- Produces: `endpoints.yaml` and the generated `config/settings/endpoints.json` consumed by Task 6's template.

- [ ] **Step 1: Extract the current endpoints**

Run:
```bash
python3 - <<'PY'
import re
t=open('config/krakend.tmpl').read()
# crude block splitter: each endpoint object between "endpoint": ... and its backend host
for m in re.finditer(r'\{\s*"endpoint":\s*"([^"]+)",\s*"method":\s*"([A-Z]+)"(.*?)"host":\s*\[([^\]]+)\]', t, re.S):
    path, method, mid, host = m.groups()
    hdrs = re.search(r'"input_headers":\s*\[(.*?)\]', mid, re.S)
    qs   = re.search(r'"input_query_strings":\s*\[(.*?)\]', mid, re.S)
    print(method, path)
    if hdrs: print("  headers:", re.findall(r'"([^"]+)"', hdrs.group(1)))
    if qs:   print("  query:", re.findall(r'"([^"]+)"', qs.group(1)))
PY
```
Expected: prints the 18 endpoints with their `input_headers` (and `input_query_strings` where present). Use this as the transcription source.

- [ ] **Step 2: Write endpoints.yaml**

Create `endpoints.yaml`. Transcribe each of the 18 endpoints from Step 1. Set `auth: public` only for endpoints currently in `jwt.json` `skip_paths` (`/api/ping`); everything else is `auth: protected`. Preserve each endpoint's exact `input_headers` and any `input_query_strings`. Template:
```yaml
backends:
  forgeos:
    host_default: http://host.docker.internal:8080
    host_env: FORGEOS_HOST

defaults:
  output_encoding: no-op
  encoding: no-op
  timeout: null

endpoints:
  - path: /api/ping
    method: GET
    backend: forgeos
    auth: public
    input_headers: [Accept, Traceparent]     # <-- use ACTUAL headers from Step 1

  # ... transcribe the remaining 17 endpoints here, one block each,
  # copying input_headers (and input_query_strings if any) verbatim from Step 1 ...
```

- [ ] **Step 3: Generate endpoints.json**

Run: `go run ./cmd/gen`
Expected: `wrote config/settings/endpoints.json (18 endpoints)`.

- [ ] **Step 4: Verify count matches the current template**

Run:
```bash
test "$(python3 -c "import json;print(len(json.load(open('config/settings/endpoints.json'))))")" = \
     "$(rg -c '\"endpoint\":' config/krakend.tmpl)" && echo MATCH || echo MISMATCH
```
Expected: `MATCH` (18 == 18). If MISMATCH, an endpoint was missed in transcription — fix `endpoints.yaml` and re-run Step 3.

- [ ] **Step 5: Commit**

```bash
git add endpoints.yaml config/settings/endpoints.json
git commit -m "feat(gen): endpoints.yaml source of truth + generated endpoints.json"
```

---

## Task 6: Rewrite krakend.tmpl to range + derive skip_paths

**Files:**
- Modify: `config/krakend.tmpl` (replace the hardcoded `"endpoints": [ ... ]` array with a `range`; change the jwt `skip_paths` to merge static + derived)
- Modify: `config/settings/jwt.json` (reduce `skip_paths` to static-only entries)

**Interfaces:**
- Consumes: `config/settings/endpoints.json` (Task 5) exposed by FC as `.endpoints` (top-level array).

- [ ] **Step 1: Snapshot the CURRENT rendered endpoints (baseline)**

Run:
```bash
mkdir -p /tmp/gwsnap
docker run --rm -v "$PWD/config:/etc/krakend" -v /tmp/gwsnap:/out \
  -e FC_ENABLE=1 -e FC_SETTINGS=/etc/krakend/settings -e FC_OUT=/out/before.json \
  -e FORGEOS_HOST=http://x:8080 \
  krakend:2.13.4 check -t -c /etc/krakend/krakend.tmpl
python3 -c "import json;d=json.load(open('/tmp/gwsnap/before.json'));json.dump(sorted(([e['endpoint'],e['method'],e.get('input_headers',[]),e['backend'][0]['host']] for e in d['endpoints']),key=lambda x:(x[0],x[1])),open('/tmp/gwsnap/before_norm.json','w'),indent=2)"
```
Expected: `Syntax OK!` and `/tmp/gwsnap/before_norm.json` written (normalized list of the 18 endpoints).

- [ ] **Step 2: Replace the endpoints array with a range block**

In `config/krakend.tmpl`, replace the entire `"endpoints": [ ... ]` array (the hardcoded 18 blocks) with:
```gotmpl
  "endpoints": [
{{- range $i, $e := .endpoints }}
  {{- if $i }},{{ end }}
    {
      "endpoint": "{{ $e.path }}",
      "method": "{{ $e.method }}",
      "output_encoding": "{{ $e.output_encoding }}",
      "input_headers": {{ marshal $e.input_headers }},
      {{- if $e.input_query_strings }}
      "input_query_strings": {{ marshal $e.input_query_strings }},
      {{- end }}
      {{- if $e.timeout }}
      "timeout": "{{ $e.timeout }}",
      {{- end }}
      {{- if $e.rate_limit }}
      "extra_config": { "qos/ratelimit/router": {{ marshal $e.rate_limit }} },
      {{- end }}
      "backend": [
        {
          "url_pattern": "{{ $e.url_pattern }}",
          "method": "{{ $e.method }}",
          "encoding": "{{ $e.encoding }}",
          "host": ["{{ env $e.host_env | default $e.host_default }}"]
        }
      ]
    }
{{- end }}
  ]
```

- [ ] **Step 3: Derive public skip_paths in the jwt block**

In `config/krakend.tmpl`, find the jwt plugin config line that emits `skip_paths` (currently `"skip_paths": {{ marshal .jwt.skip_paths }},`) and replace it with:
```gotmpl
        "skip_paths": [
          {{- range $i, $p := .jwt.skip_paths }}{{ if $i }},{{ end }}"{{ $p }}"{{ end }}
          {{- range .endpoints }}{{ if eq .auth "public" }},"{{ .path }}"{{ end }}{{ end }}
        ],
```

- [ ] **Step 4: Reduce jwt.json skip_paths to static-only**

Edit `config/settings/jwt.json` — set `skip_paths` to only the non-endpoint (static/glob) entries, removing business routes now derived from `auth: public`:
```json
  "skip_paths": [
    "/public/*"
  ],
```
(`/api/ping` is now derived from its `auth: public` in `endpoints.yaml`.)

- [ ] **Step 5: Render AFTER and diff against the baseline**

Run:
```bash
docker run --rm -v "$PWD/config:/etc/krakend" -v /tmp/gwsnap:/out \
  -e FC_ENABLE=1 -e FC_SETTINGS=/etc/krakend/settings -e FC_OUT=/out/after.json \
  -e FORGEOS_HOST=http://x:8080 \
  krakend:2.13.4 check -t -c /etc/krakend/krakend.tmpl
python3 -c "import json;d=json.load(open('/tmp/gwsnap/after.json'));json.dump(sorted(([e['endpoint'],e['method'],e.get('input_headers',[]),e['backend'][0]['host']] for e in d['endpoints']),key=lambda x:(x[0],x[1])),open('/tmp/gwsnap/after_norm.json','w'),indent=2)"
diff /tmp/gwsnap/before_norm.json /tmp/gwsnap/after_norm.json && echo "ENDPOINTS EQUIVALENT"
```
Expected: `Syntax OK!` then `ENDPOINTS EQUIVALENT` (no diff — the range reproduces the same 18 endpoints, methods, headers, and hosts). If diff is non-empty, fix `endpoints.yaml` (a header or path was transcribed wrong) and re-run Task 5 Step 3 + this step.

- [ ] **Step 6: Verify skip_paths includes the derived public route**

Run:
```bash
python3 -c "import json;d=json.load(open('/tmp/gwsnap/after.json'));sp=d['extra_config']['plugin/http-server']['krakend-jwt-headers']['skip_paths'];print(sp);assert '/public/*' in sp and '/api/ping' in sp, sp;print('SKIP_PATHS OK')"
```
Expected: prints the list then `SKIP_PATHS OK`.

- [ ] **Step 7: Commit**

```bash
git add config/krakend.tmpl config/settings/jwt.json
git commit -m "feat(gen): krakend.tmpl ranges over endpoints.json + derived skip_paths"
```

---

## Task 7: Makefile targets (gen, gen-check) + wire into check

**Files:**
- Modify: `Makefile`

**Interfaces:**
- Consumes: `cmd/gen` (Task 4), the existing `check` target.

- [ ] **Step 1: Add the gen and gen-check targets**

In `Makefile`, add (place near the existing `check`/`generate` targets):
```make
gen: ## Regenerate config/settings/endpoints.json from endpoints.yaml
	go run ./cmd/gen

gen-check: ## Fail if endpoints.json is out of sync with endpoints.yaml
	go run ./cmd/gen
	git diff --exit-code config/settings/endpoints.json
```

- [ ] **Step 2: Make `check` depend on gen-check**

In `Makefile`, change the `check` target so it runs `gen-check` first. Change the target header line from `check: ## Validate KrakenD configuration` to:
```make
check: gen-check ## Validate KrakenD configuration (regen + drift + schema)
```
(Leave the existing recipe body below it unchanged.)

- [ ] **Step 3: Verify gen-check passes on a clean tree**

Run: `make gen-check`
Expected: exit 0, no diff output (endpoints.json already matches the YAML from Task 5).

- [ ] **Step 4: Verify gen-check FAILS on drift**

Run:
```bash
printf '\n' >> config/settings/endpoints.json   # introduce drift
make gen-check; echo "exit=$?"
git checkout config/settings/endpoints.json      # restore
```
Expected: `git diff --exit-code` fails, `exit=1` (or the make error). Confirms drift detection works. Tree restored afterward.

- [ ] **Step 5: Commit**

```bash
git add Makefile
git commit -m "build(gen): make gen + gen-check drift guard, wired into check"
```

---

## Task 8: CI drift guard

**Files:**
- Modify: `.github/workflows/pull-request.yml`

**Interfaces:**
- Consumes: `make gen-check` (Task 7).

- [ ] **Step 1: Inspect the current workflow**

Run: `cat .github/workflows/pull-request.yml`
Expected: read the existing job/steps so the new step matches indentation and the runner (`runs-on`) already in use.

- [ ] **Step 2: Add a Go setup + gen-check step**

In `.github/workflows/pull-request.yml`, add these steps to the existing job (after the repo checkout step, before/around the existing KrakenD validation step). Match the file's existing indentation:
```yaml
      - name: Set up Go
        uses: actions/setup-go@v5
        with:
          go-version: '1.22'

      - name: Verify endpoints.json is in sync with endpoints.yaml
        run: make gen-check
```

- [ ] **Step 3: Validate the workflow YAML parses**

Run: `python3 -c "import yaml,sys; yaml.safe_load(open('.github/workflows/pull-request.yml')); print('YAML OK')"`
Expected: `YAML OK`.

- [ ] **Step 4: Commit**

```bash
git add .github/workflows/pull-request.yml
git commit -m "ci: verify endpoints.json stays in sync with endpoints.yaml"
```

---

## Self-Review Notes

- **Spec coverage:** YAML schema (Task 5) · Go generator validate/normalize/YAML→JSON (Tasks 1–4) · committed endpoints.json (Task 5) · FC range in krakend.tmpl (Task 6) · derived skip_paths (Task 6) · Makefile gen/gen-check (Task 7) · CI drift guard (Task 8) · determinism (golden test Task 4) · migration of the 18 endpoints (Task 5). All spec sections mapped.
- **FC namespacing:** endpoints.json is a top-level array → `.endpoints`. Verified indirectly by Task 6's render-equivalence diff (a wrong namespace yields zero endpoints → diff fails loudly).
- **Migration safety:** Task 6 Steps 1/5 render before+after and diff the normalized endpoint set — the migration cannot silently drop or alter an endpoint.
- **Out of scope (per spec):** web UI, OpenAPI import, multi-backend aggregation, named header profiles.
