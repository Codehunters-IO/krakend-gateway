# Design: KrakenD Endpoint Config Generator

**Date:** 2026-08-03
**Status:** Approved
**Branch:** feat/forgeos-edge

## Problem

`config/krakend.tmpl` is a hand-edited Go template where every endpoint is ~40 lines
of repeated JSON (input_headers, backend url_pattern/method/encoding/host duplicated).
Adding or auditing an endpoint means copy-pasting boilerplate; the file grows to
thousands of lines. Public endpoints must also be kept in sync with `skip_paths` in
`jwt.json` and `ip_resolver.json` — a manual, drift-prone step.

Goal: a compact, human-authored source of truth for endpoints that generates the
KrakenD configuration deterministically, with validation and no `skip_paths` drift.

## Chosen Approach

Hybrid pipeline: **YAML source → Go generator → committed JSON → KrakenD FC range → validated render.**

```
endpoints.yaml                         source of truth (human-edited)
      │  make gen  (go run ./cmd/gen)
      ▼
config/settings/endpoints.json         generated, validated, COMMITTED
      │  {{ range .endpoints }}  in krakend.tmpl (FC render-time)
      ▼
krakend.json final                     validated by `make check`
```

### Responsibility split

| Stage | Does | Does NOT |
|-------|------|----------|
| `endpoints.yaml` | declare endpoints (readable, comments) | — |
| `cmd/gen` (Go) | YAML→JSON, validate, apply defaults, resolve `backend`→`{host_env, host_default}` | resolve `env` (that is KrakenD runtime) |
| `krakend.tmpl` range | expand each endpoint to KrakenD block; `host` via `{{ env .host_env \| default .host_default }}`; derive `skip_paths` from `public` endpoints | — |
| `make check` | KrakenD validates final render | — |

Three validation layers: Go gen (fail-fast) → FC render → KrakenD schema check.
Each error caught as early as possible.

## endpoints.yaml Schema

```yaml
# backends: logical key → default host + override envvar
backends:
  forgeos:
    host_default: http://host.docker.internal:8080
    host_env: FORGEOS_HOST

# defaults applied when an endpoint omits them
defaults:
  output_encoding: no-op
  encoding: no-op
  timeout: null            # null = inherit global service.timeout

endpoints:
  - path: /api/v1/login          # exposed route
    method: POST
    backend: forgeos             # key in backends{}
    auth: public                 # public → added to skip_paths | protected → JWT required
    input_headers:               # EXPLICIT per endpoint (full control, auditable)
      - Accept
      - Content-Type
      - Traceparent
    # optional:
    # url_pattern: /other/backend/route   # if it differs from path
    # input_query_strings: [type, number]
    # timeout: 30s                        # per-endpoint override
    # rate_limit: { max_rate: 30, client_max_rate: 5, strategy: ip }
```

### Fields

| Field | Req | Default | Note |
|-------|-----|---------|------|
| `path` | yes | — | exposed route; must start with `/` |
| `method` | yes | — | GET / POST / PUT / PATCH / DELETE |
| `backend` | yes | — | must exist in `backends{}` |
| `auth` | yes | — | `public` \| `protected`; drives `skip_paths` only |
| `input_headers` | yes | — | explicit, non-empty list |
| `url_pattern` | no | `=path` | backend route if it differs |
| `input_query_strings` | no | — | whitelist of query params |
| `timeout` | no | global | per-endpoint override |
| `rate_limit` | no | — | `qos/ratelimit/router` per endpoint |

**Decision:** `auth` does NOT touch headers (those are explicit) — it only decides
`skip_paths` membership. This keeps which identity headers reach the backend
auditable per endpoint (the `x-user-*` headers are spoofable, so explicit is safer
than inherited). Static skip entries (`/public/*`, `*/actuator/health`,
`*/v1/api-docs`) stay declared separately.

## Go Generator (`cmd/gen`)

Location: `cmd/gen/` — its own Go module/command, separate from `plugins/`.
Only external dep: `gopkg.in/yaml.v3`.

Flow:
1. Read `endpoints.yaml`
2. Unmarshal → `Spec{Backends, Defaults, Endpoints}`
3. `validate(spec)` — fail-fast, accumulate errors with endpoint index
4. `normalize(spec)` — apply defaults; resolve `backend`→`host_env`/`host_default`;
   `url_pattern = path` if empty
5. Marshal → `config/settings/endpoints.json` (indented, stable order, deterministic)

The generator NEVER resolves `env` — it copies the envvar *name* only, so the
per-environment override still happens in KrakenD at runtime.

### Validation rules (abort build on any failure)

| Rule | Error |
|------|-------|
| `path` non-empty, starts with `/` | `endpoint[i]: invalid path` |
| `method` ∈ {GET,POST,PUT,PATCH,DELETE} | `unknown method` |
| `backend` exists in `backends{}` | `backend "x" not declared` |
| `(path, method)` unique | `duplicate endpoint` |
| `input_headers` non-empty | `input_headers required` |
| `auth` ∈ {public, protected} | `invalid auth` |

### Generated endpoints.json (consumed by FC range)

```json
{
  "endpoints": [
    {
      "path": "/api/v1/login", "method": "POST",
      "url_pattern": "/api/v1/login", "auth": "public",
      "output_encoding": "no-op", "encoding": "no-op",
      "host_env": "FORGEOS_HOST",
      "host_default": "http://host.docker.internal:8080",
      "input_headers": ["Accept","Content-Type","Traceparent"],
      "input_query_strings": [], "timeout": "",
      "rate_limit": null
    }
  ]
}
```

Determinism: input order preserved, no timestamps → clean git diffs. File is committed.

## krakend.tmpl range block

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
    "backend": [{
      "url_pattern": "{{ $e.url_pattern }}",
      "method": "{{ $e.method }}",
      "encoding": "{{ $e.encoding }}",
      "host": ["{{ env $e.host_env | default $e.host_default }}"]
    }]
  }
{{- end }}
]
```

### Derived skip_paths (inside the jwt block, same `.endpoints`)

```gotmpl
"skip_paths": [
  "/public/*", "*/actuator/health", "*/v1/api-docs"
  {{- range .endpoints }}{{ if eq .auth "public" }},"{{ .path }}"{{ end }}{{- end }}
],
```

`jwt.json` and `ip_resolver.json` stop listing business routes → no drift.

## Integration

### Makefile

```make
gen:        ## endpoints.yaml → endpoints.json
	go run ./cmd/gen

gen-check:  ## fail if endpoints.json is out of sync with the YAML
	go run ./cmd/gen && git diff --exit-code config/settings/endpoints.json

check: gen-check ## regen + drift check + KrakenD validation
	... krakend check ...
```

### CI (`pull-request.yml`)

Run `make gen-check`. If `endpoints.json` was hand-edited without regenerating
(or vice versa), `git diff --exit-code` breaks the build. Guarantees the YAML is
the single source of truth.

### Migration of the current 137 endpoints

- One-shot `make migrate-from-tmpl` (throwaway `cmd/gen/migrate` helper): parse the
  current `krakend.tmpl` → emit the initial `endpoints.yaml`. Run once, review by
  hand, discard the helper.
- Then rewrite `krakend.tmpl` with the `range` block (drops the ~4700 hardcoded lines).

### Testing

| Level | What |
|-------|------|
| Go unit | `validate()` (dup, unknown backend, invalid method, empty headers) + `normalize()` (defaults, url_pattern=path, host resolution) |
| Golden | `endpoints.yaml` fixture → expected `endpoints.json` (byte-exact, determinism) |
| Integration | `make gen check` in CI → KrakenD validates the real render |

## Final file structure

```
endpoints.yaml                       NEW (source)
cmd/gen/main.go                       NEW (generator)
cmd/gen/{validate,normalize}.go       NEW
cmd/gen/*_test.go                     NEW
config/settings/endpoints.json        NEW (generated, committed)
config/krakend.tmpl                   MODIFIED (range instead of 137 hardcoded)
config/settings/jwt.json              MODIFIED (business skip_paths → derived)
Makefile                              MODIFIED (gen, gen-check)
.github/workflows/pull-request.yml    MODIFIED (gen-check)
```

## Out of scope (YAGNI)

- Web UI / self-service form (devs edit YAML directly).
- OpenAPI import (backends expose `/v1/api-docs`, but auto-derivation is a later phase).
- Multi-backend aggregation endpoints (current config is 1:1 passthrough / no-op).
- Named header profiles / inheritance (explicit per-endpoint headers chosen for auditability).
```
