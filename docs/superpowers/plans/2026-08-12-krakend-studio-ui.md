# KrakenD Studio — Local Web UI for Endpoint Configuration

> **For agentic workers:** REQUIRED SUB-SKILL: use superpowers:subagent-driven-development or superpowers:executing-plans. This plan is phase-level; expand each phase into bite-sized steps before executing it. Phases are ordered by risk, not convenience — Phase 1 is a go/no-go gate for everything after it.

**Goal:** A local developer tool that provides a web UI to author KrakenD endpoint configuration, writing back to `endpoints.yaml` (which stays the source of truth and stays hand-editable), and exporting `endpoints.json`, a fully rendered `krakend.json`, and importing endpoints from an OpenAPI spec.

**Architecture:** React + Vite SPA → Go HTTP API (chi) → `yamldoc` (Node-level YAML surgery) + `endpointspec` (the existing generator, promoted to an importable package) + a shell-out renderer for `krakend.json`.

**Tech Stack:** Go 1.26.5 (chi/v5, `gopkg.in/yaml.v3`, `github.com/pb33f/libopenapi`), React + Vite + TypeScript (TanStack Query, zustand), Vitest + Testing Library + MSW, Playwright, goreleaser.

**Complexity:** Large — ~45 files across two repos.

**Repos:**
- **A — `krakend-gateway`** (this repo): Phase 0 only (generator extraction) plus a `.gitignore` entry, a README pointer, and an ADR.
- **B — `krakend-studio`** (new): module `github.com/CODEHUNTERS/krakend-studio`, binary `krakend-studio`.

---

## Fixed Constraints (decided by the user — do not re-litigate)

1. **Local dev tool.** Single user, no auth, no DB, no deployment story. State lives on the filesystem.
2. **React + Vite SPA** against a Go HTTP API.
3. **All four capabilities:** write back `endpoints.yaml` · export `endpoints.json` · export `krakend.json` · import from OpenAPI.
4. **Separate repo**, reusable across gateways.

## Inherited Constraints (from the generator work — see `2026-08-03-krakend-config-generator-design.md`)

- KrakenD Community Edition 2.13.4. Flexible Configuration unmarshals **every** settings file into `map[string]interface{}` — a top-level JSON array is rejected. `endpoints.json` is therefore `{"endpoints":[...]}` and the template reads `.endpoints.endpoints`.
- `skip_paths` for the JWT plugin is composed at render time: static entries from `jwt.json` **plus** every endpoint with `auth: public`. Opening a route happens only via `auth: public`.
- Generator output is deterministic: 2-space indent, input order preserved, no timestamps. A golden test pins the exact bytes; CI job `endpoints-drift` runs `make gen-check`.
- Docker Desktop on the target machine does **not** share `/tmp`. Any `FC_OUT` render target must sit inside an already-mounted directory.

---

## Spike Result (decisive — run before this plan was written)

`yaml.v3` Node-level parse → encode with `SetIndent(2)` on the real `endpoints.yaml` preserves **comments (head and inline), anchors `&identity`, aliases `*identity`, the `x-header-sets` block, key order, and indent style — exactly.**

The only delta is stripped blank lines: 3686 → 3666 bytes, 20 blank lines; a `diff` of non-blank lines is empty.

**Consequence:** round-trip fidelity is a tractable engineering problem, not a research problem. The plan below commits to Node-level editing with a scoped fallback rather than hedging the whole feature.

Spike artifacts are throwaway and safe to delete:
`/private/tmp/claude-501/-Users-andres-…/scratchpad/spike/`

---

## Architectural Decisions

### 1. Generator reuse → promote to an importable package

`cmd/gen` becomes a thin `main` over a new library `github.com/CODEHUNTERS/krakend-gateway/pkg/endpointspec`. Studio imports it.

**Rejected:**
- *Copy / vendor* — two implementations of a validation contract that CI enforces byte-exactly is guaranteed drift.
- *Subprocess* — `go run` per keystroke is ~300ms, and stderr text cannot drive per-field form validation, which is the entire value of the UI.

Requires a root `go.mod`. Safe: all five `plugins/*` modules and `cmd/gen` are nested modules and are excluded by the toolchain automatically. Studio consumes it with `GOPRIVATE=github.com/CODEHUNTERS/*` plus a `replace` directive for local development.

**Behavior contract is unchanged:** same `Generate` bytes, same golden fixture, same drift gate. Only invocation lines move (`cd cmd/gen && go run .` → `go run ./cmd/gen`).

**Structured errors without breaking tests:** add `type Violation struct{ Index int; Field, Message string }` implementing `error` with byte-identical `Error()` text. `Validate` keeps its `[]error` signature (holding `Violation` values), so `validate_test.go` and the `errors.Join` in `Generate` are untouched; Studio type-asserts for field-level UI errors.

### 2. Round-trip fidelity → Node-level surgery + blank-line restoration + semantic guard

Never `yaml.Marshal(Spec)` — that deletes `x-header-sets`, anchors, and every comment.

Pipeline: parse to `*yaml.Node` → patch the `endpoints` sequence in place → encode at indent 2 → re-inject blank lines by recording, at load time, which source line numbers are blank and which node's `.Line` follows them.

**Gate:** an *identity-write test* (`load → no-op save → bytes must equal input`) against the real `endpoints.yaml` and every fixture. If it ever fails for a construct that cannot be restored, fall back to **byte-splice**: keep the original bytes and replace only the `[startLine, endLine)` byte range of mutated sequence items. Ship Plan A; the splice fallback is scoped per construct, not a rewrite.

**Belt-and-braces on every write:** re-run `Generate` on the newly serialized YAML and assert it equals `Generate` of the intended draft. Mismatch → abort, restore `.bak`, return 500 with the diff.

**Anchors are a first-class UI concept.** Editing headers on an aliased endpoint forces an explicit choice: *detach to inline list* (alias lost, warned) or *edit the header set* (shows "affects N endpoints"). An alias is never resolved implicitly.

### 3. `krakend.json` rendering → shell out, never reimplement

Resolution order: local `krakend` binary on `PATH` → `docker run krakend:2.13.4` with the gateway repo mounted. Absent both: `GET /api/health` reports `krakend: "none"`, the export button is disabled with a tooltip naming both remedies, and `POST /api/export/krakend-json` returns `503`. No partial or fake render.

Because Docker `/tmp` is not shared on this machine, `FC_OUT` and the overlay settings dir always live at `<gatewayRepo>/.krakend-studio/` (gitignored), inside the already-mounted repo.

Previewing an *unsaved* draft uses an overlay: copy `config/settings/` into `.krakend-studio/settings/`, swap in the draft's `endpoints.json`, render from there. The repo's real settings are never touched.

### 4. Target repo → explicit workspace, hard-fenced

`krakend-studio --repo <path>` (repeatable), or the CWD if it contains `endpoints.yaml` + `config/krakend.tmpl`. Recents in `~/.config/krakend-studio/workspaces.json`.

Safety rules, enforced in a single chokepoint (`internal/workspace`):

- Bind `127.0.0.1` only.
- `Host`/`Origin` allowlist — DNS-rebinding defense, since there is no auth.
- Every path resolved via `filepath.EvalSymlinks` and asserted to be under an allowed root.
- **The only writable paths are `endpoints.yaml`, `config/settings/endpoints.json`, and `.krakend-studio/**`.**
- Timestamped `.bak` before each write.
- Optimistic concurrency via the SHA-256 the client received on load — `409` on mismatch (someone hand-edited).
- `git status --porcelain` is read to show a dirty badge. No mutating git command is ever run.

### 5. OpenAPI import → isolated Phase 6, suggestions only

**Inferable:** `paths` × methods → `path`/`method` (`{param}` syntax is already identical) · `parameters[in=query]` → `input_query_strings` · `servers[].url` basePath → the `path` vs `url_pattern` split · `security: []` on an operation with a non-empty global security → *suggest* `auth: public`.

**Not inferable, always required from the user:** `backend`, `input_headers` (header set), `timeout`, `rate_limit`, `disable_host_sanitize`.

Every candidate lands flagged `needsReview` and is deduped against existing `(method, path)`. Import produces a draft that goes through the normal diff-preview/confirm save path. Library: `github.com/pb33f/libopenapi` (OAS 3.0 + 3.1).

---

## Files — Repo A: `krakend-gateway` (existing)

### Modify — generator extraction

- `go.mod` — **NEW**: `module github.com/CODEHUNTERS/krakend-gateway`, go 1.26.5, require `gopkg.in/yaml.v3`
- `go.sum` — **NEW** (moved from `cmd/gen/go.sum`)
- `cmd/gen/go.mod`, `cmd/gen/go.sum` — **DELETE**
- `pkg/endpointspec/spec.go` — moved from `cmd/gen/spec.go`, `package endpointspec`; field order and both tag sets unchanged (emission contract)
- `pkg/endpointspec/validate.go` — moved; adds exported `Violation`; signature and message text unchanged
- `pkg/endpointspec/normalize.go`, `pkg/endpointspec/generate.go` — moved verbatim
- `pkg/endpointspec/testdata/{sample.yaml,expected.json}` — moved **byte-identical**
- `pkg/endpointspec/{spec,validate,normalize,generate}_test.go` — moved; only the `package` line changes
- `pkg/endpointspec/violation_test.go` — **NEW**: `Violation.Error()` text matches the pre-extraction strings
- `cmd/gen/main.go` — imports the package; CLI args, output, and exit codes unchanged
- `Makefile` — `gen` target becomes `go run ./cmd/gen "$(ENDPOINTS_SPEC)" "$(ENDPOINTS_JSON)"` (drops the `cd`); `gen-check`, `check`, `generate` untouched
- `.github/workflows/pull-request.yml` — `go-version-file: go.mod`; `run: go test ./...`
- `.gitignore` — add `.krakend-studio/`
- `README.md` §"Endpoints (generador)" — note the package location plus a Studio pointer
- `docs/superpowers/specs/2026-08-03-krakend-config-generator-design.md` — addendum superseding the "web UI / OpenAPI import out of scope" lines, linking here

### New

- `docs/adr/0001-endpointspec-as-importable-package.md` — why library over subprocess/vendoring

**Verification gate for this repo:** `go test ./...` · `make gen` (zero diff on `config/settings/endpoints.json`) · `make gen-check` · `make check` · `make plugin-build` (proves the nested plugin modules are unaffected).

---

## Files — Repo B: `krakend-studio` (new)

### Go backend — workspace

- `go.mod`, `go.sum` — requires `krakend-gateway`, `libopenapi`, `yaml.v3`, `chi/v5`
- `cmd/krakend-studio/main.go` — flags `--repo` (repeatable), `--port` (7788), `--open`; embeds the SPA
- `internal/workspace/workspace.go` — open/validate a gateway dir; detect `endpoints.yaml`, `krakend.tmpl`, `config/settings`
- `internal/workspace/paths.go` — the write allowlist and traversal/symlink chokepoint
- `internal/workspace/recents.go` — `~/.config/krakend-studio/workspaces.json`
- `internal/workspace/gitstatus.go` — read-only porcelain check

### Go backend — YAML document engine (highest-risk module)

- `internal/yamldoc/document.go` — load bytes → `*yaml.Node` + typed `Spec` + per-endpoint `nodeId`
- `internal/yamldoc/blanklines.go` — capture/restore blank-line positions (the one spike-confirmed gap)
- `internal/yamldoc/patch.go` — apply add/update/delete/reorder ops against the sequence node
- `internal/yamldoc/anchors.go` — enumerate `x-header-sets` anchors + `usedBy`; detach/retarget aliases
- `internal/yamldoc/render.go` — encode at indent 2 + blank-line restore + identity assertion
- `internal/yamldoc/fidelity.go` — semantic guard (`Generate(after) == Generate(intent)`)

### Go backend — services

- `internal/render/krakend.go` — runner resolution (binary → docker → none), overlay settings dir, `FC_OUT` inside the repo
- `internal/render/runner.go` — `Runner` interface plus exec and docker implementations (fakeable)
- `internal/openapiimport/importer.go` — spec → candidate endpoints
- `internal/openapiimport/mapping.go` — basePath/security/query-param rules, `needsReview` flags
- `internal/diff/unified.go` — unified diff for the save preview

### Go backend — HTTP

- `internal/api/router.go`, `internal/api/middleware.go` (localhost/Origin guard, workspace resolution, JSON error envelope)
- `internal/api/handlers_{spec,export,openapi,workspace,health}.go`
- `internal/api/dto.go` — wire shapes
- `internal/web/embed.go` — `embed.FS` for `web/dist`

### API surface

| Method | Route | Request | Response |
|---|---|---|---|
| GET | `/api/health` | — | `{version, krakend:{mode:"binary"\|"docker"\|"none", version}}` |
| GET | `/api/workspaces` | — | `[{path, name, lastOpened}]` |
| POST | `/api/workspaces` | `{path}` | `{id, path, valid, problems[]}` |
| GET | `/api/spec` | `X-Workspace` | `{sha256, yaml, backends, defaults, endpoints:[{nodeId, path, method, backend, auth, url_pattern?, input_headers[], headerSetRef?, input_query_strings[], timeout?, disable_host_sanitize?, rate_limit?}], headerSets:[{name, headers[], usedBy[]}], gitDirty}` |
| POST | `/api/spec/validate` | `{endpoints, backends}` | `{violations:[{index, field, message}]}` |
| POST | `/api/spec/preview` | `{baseSha, endpoints, backends, defaults, headerSets}` | `{yamlDiff, endpointsJson, endpointsJsonDiff, warnings[]}` |
| PUT | `/api/spec` | same + `{writeGenerated:bool}` | `200 {sha256, yaml, backupPath}` · `409` stale · `422` violations |
| POST | `/api/export/endpoints-json` | draft | `application/json` download |
| POST | `/api/export/krakend-json` | draft + `{env:{}}` | rendered JSON · `503 {reason, remedies[]}` |
| POST | `/api/openapi/import` | URL or multipart + `{stripBasePath, defaultBackend, defaultHeaderSet}` | `{candidates:[{endpoint, needsReview[], conflictsWith?}], warnings[]}` |

`nodeId` is server-assigned on `GET /api/spec` and echoed by the client: absent means deletion, unknown means addition, array order means document order. That is what makes reorder/delete/edit unambiguous at the node level.

### React SPA (`web/`)

- `package.json`, `vite.config.ts` (dev proxy `/api` → `:7788`), `tsconfig.json`, `index.html`
- `src/api/client.ts`, `src/api/types.ts` (generated from the Go DTOs or hand-mirrored)
- `src/state/draftStore.ts` (zustand: draft spec, dirty flag, unsaved guard), `src/state/queries.ts` (TanStack Query)
- **Screens:** `WorkspacePicker`, `EndpointsScreen`, `HeaderSetsScreen`, `BackendsScreen`, `ExportScreen`, `OpenApiImportWizard`
- **Components:** `AppShell` (repo path, git-dirty badge, krakend-availability badge, Save button) · `EndpointTable` (filter by path/method/auth/backend; flags for timeout/SSE/rate-limit) · `EndpointForm` (**`auth: public` renders a prominent warning that the path is added to the JWT `skip_paths`**; header-set selector vs inline list with the detach dialog) · `HeaderList` · `RateLimitFields` · `QueryStringChips` · `SaveDiffModal` (YAML diff + endpoints.json diff + fidelity warnings + "commit both files together" reminder) · `ValidationBanner` · `JsonViewer` · `ImportCandidateTable`
- `src/lib/validation.ts` — client mirror of the six rules for instant feedback; the server stays authoritative

### Repo scaffolding

`Makefile` (`dev`, `build`, `test`, `e2e`), `.github/workflows/ci.yml`, `.goreleaser.yaml`, `README.md`, `docs/adr/0001-yaml-node-round-trip.md`

---

## Implementation Phases

Each phase is independently shippable and verifiable.

- [ ] **Phase 0 — Extract `endpointspec` (krakend-gateway only).**
  Root `go.mod`, move the package, adjust Makefile and CI.
  *Verify:* `go test ./...` green · `make gen` produces a zero-byte diff · `make gen-check` and `make check` pass · plugins still build.
  Nothing about the UI exists yet; this phase is independently mergeable and reversible.

- [ ] **Phase 1 — YAML document engine + identity gate.**
  `internal/yamldoc` with the blank-line restorer.
  *Verify:* `load → no-op save` is byte-identical for the real `endpoints.yaml` and every fixture (anchors, aliases, comments, `x-header-sets`, null timeout, SSE flags).
  **This phase de-risks the whole feature. If the gate cannot be met, stop and switch to byte-splice before any UI work.**

- [ ] **Phase 2 — Read-only server + SPA shell.**
  Workspace open/allowlist, `GET /api/spec`, `/api/health`, embedded SPA, endpoints table, read-only detail panel.
  *Verify:* opens the real repo · lists 18 endpoints · resolves both header sets · writes nothing (assert the repo tree hash is unchanged).

- [ ] **Phase 3 — Export (still read-only to the repo).**
  `endpoints.json` download; `krakend.json` via binary/Docker with the `.krakend-studio/` overlay.
  *Verify:* exported `endpoints.json` is byte-equal to the committed one · rendered `krakend.json` matches `make generate` · with `krakend` removed from `PATH` and Docker stopped, the UI degrades to a disabled button plus an actionable message.

- [ ] **Phase 4 — Editing and save.**
  Patch ops, validation, `/preview` diff, `PUT /api/spec` with hash guard + `.bak` + semantic fidelity guard, `SaveDiffModal`.
  *Verify:* add/edit/delete/reorder an endpoint · only noise-free diffs appear · `make gen-check` stays green afterwards · a concurrent hand-edit returns 409.

- [ ] **Phase 5 — Header sets, backends, defaults.**
  Anchor editing with impact count, detach-on-edit dialog.
  *Verify:* editing the `identity` anchor updates 17 endpoints with a single-block YAML diff · detaching one endpoint inlines only that list.

- [ ] **Phase 6 — OpenAPI import wizard.**
  *Verify:* Petstore 3.0 and a 3.1 fixture produce reviewable candidates · duplicates against the existing 18 are flagged, never silently applied · `auth` is never auto-set to `public` without an explicit click.

- [ ] **Phase 7 — Packaging.**
  goreleaser single binary with the embedded SPA, README, and the `.krakend-studio/` gitignore entry landed in krakend-gateway.

---

## Risks

| # | Risk | Recommendation |
|---|---|---|
| 1 | **Blank-line restoration is heuristic** — the only proven gap; a bug produces churny diffs. | Make the identity test a Phase-1 merge gate over a fixture corpus. Diff-preview plus confirm means a cosmetic regression is visible before writing, never silent. |
| 2 | **Anchor semantics leak into UX.** Users think they are editing one endpoint but touch 17. | Header sets are a first-class screen with `usedBy` counts; editing an aliased list forces the detach-vs-edit-set dialog. Never resolve an alias implicitly. |
| 3 | **Root `go.mod` in krakend-gateway** could disturb plugin builds or CI Go setup. | Nested modules are excluded by the toolchain (verified: five `plugins/*/go.mod` exist). Add `make plugin-build` to the Phase-0 verification. |
| 4 | **Studio depends on a private repo** (`GOPRIVATE`). | Document it; use a `replace` for local dev; add a Studio CI contract test that runs the gateway's `testdata/sample.yaml` through the imported library and byte-compares `expected.json` — catches version drift immediately. |
| 5 | **Docker `/tmp` is not shared on this machine.** | Hard-code all render artifacts under `<repo>/.krakend-studio/`; add a startup preflight that fails loudly if the mount is unusable. |
| 6 | **Writing into a git working tree.** | Allowlist of exactly three paths · `.bak` · hash-based optimistic concurrency · dirty-tree badge · no mutating git commands · `127.0.0.1` plus Host/Origin guard. |
| 7 | **`auth: public` is a security-relevant toggle** — it opens a JWT `skip_paths` entry. | Explicit warning in the form; public endpoints highlighted in the table; the save diff surfaces added public paths as a dedicated warning line. |
| 8 | **The UI writing `endpoints.json` could desync from `make gen`.** | Regenerate with the *same* `Generate` function, then let the fidelity guard re-derive it from the written YAML. Default `writeGenerated: true` with a "commit both files together" reminder in the save modal — CI's drift gate demands it. |
| 9 | **Scope creep toward multi-user/hosted.** | Enforced by design: no DB, no auth, localhost bind, filesystem is the state. Any deviation needs a new ADR. |

---

## Testing Strategy

- **`endpointspec` (gateway):** the existing unit and golden tests move unchanged — they are the regression proof for Phase 0. Add `Violation.Error()` text-stability tests.
- **`yamldoc` (Studio, the critical layer):**
  1. *Identity* — `load → save` byte-identical for the real file plus the corpus.
  2. *Golden ops* — `(input.yaml, ops) → expected.yaml` byte-exact, one fixture per construct: alias untouched, alias detached, anchor edited, endpoint inserted mid-list, endpoint deleted with an attached comment, reorder, null timeout, `rate_limit` sub-map.
  3. *Semantic invariant* — cosmetic ops leave `Generate(...)` bytes unchanged.
  4. *Fuzz* — `load → save → load` for parse stability.
- **HTTP handlers:** `httptest` against a temp copy of a minimal gateway repo in `testdata/`. Assert 409 stale-hash, the 422 violation shape, path-traversal and non-localhost `Host` rejection, and — after every mutating test — that the hash of every file *outside* the allowlist is unchanged.
- **Renderer:** unit tests against a fake `Runner` (assert env vars, `FC_OUT` location, overlay dir); one `//go:build docker` integration test running `krakend:2.13.4`, skipped when unavailable.
- **OpenAPI import:** fixture-driven table tests (3.0, 3.1, basePath, per-operation security, duplicate detection); assert `auth` is never auto-`public` when global security is non-empty.
- **React:** Vitest + Testing Library with MSW — validation states, the `auth: public` warning, the anchor detach dialog, the unsaved-changes guard. Playwright E2E against the real binary plus a temp repo copy: open → add endpoint → preview diff → save → assert the on-disk YAML and that `go run ./cmd/gen` reproduces the written `endpoints.json`.
- **Cross-repo contract:** Studio CI runs the gateway's golden fixture through the imported library (guards risk #4) and asserts the `Endpoint` struct's JSON field order (guards the emission contract).
