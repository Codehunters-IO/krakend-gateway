# session-resolver Plugin Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a KrakenD HTTP-server plugin that turns an opaque session cookie into an `Authorization: Bearer` header by reading the session from Valkey, so browser traffic reaches backends with a validated JWT while non-browser clients keep sending their own bearer tokens.

**Architecture:** A Go plugin in `plugins/session-resolver/`, its own module like the five existing plugins. It runs immediately before `krakend-jwt-headers` in the `plugin/http-server` chain, so `jwt-headers` and every backend stay unchanged. The plugin only reads Valkey; the `auth-bff` service (separate plan) is the only writer.

**Tech Stack:** Go 1.25.7, `valkey-io/valkey-go` `v1.0.77` (native Valkey client: command-builder pattern with typed result accessors, automatic pipelining), `testcontainers-go` for integration tests, KrakenD 2.13.4, Docker builder image `krakend/builder:2.13.4`. Target server: Valkey `8.1.10` — a wire-compatible fork of Redis 7.2.4.

**Spec:** `docs/superpowers/specs/2026-08-31-token-handler-bff-design.md`

**Companion plan:** `docs/superpowers/plans/2026-08-31-auth-bff.md` writes the Valkey contract this plugin reads. The two can be built in parallel — the contract is fully specified below — but end-to-end verification (Task 9) needs `auth-bff` running.

## Global Constraints

- Go `1.25.7`, matching the other plugins and the builder image.
- Plugin name: `krakend-session-resolver`. Config key under `extra_config."plugin/http-server"` uses that exact name.
- Valkey key prefix `v1:`. Hash `v1:session:{sid}` with fields `ver, sub, kc_sid, access_token, exp, abs_exp, refresh_token_enc, id_token_enc, created_at`. The plugin reads only `access_token, exp, abs_exp, sub` and **must never** read the `_enc` fields.
- **The plugin renews the TTL of `v1:session:{sid}` and of that key ONLY.** It must never touch the TTL of the reverse index `v1:kcsid:{kc_sid}`. `auth-bff` deliberately writes that index with a TTL running to the session's `abs_exp` rather than to the idle window, precisely so the index outlives every idle-TTL cycle. Renewing it here with `idle_ttl_seconds` would not extend anything — it would *shorten* it, collapsing a ceiling-length TTL back to 30 minutes on every renewal, and the index would then expire while the session is still alive. The consequence is silent and severe: `auth-bff` resolves backchannel logout through that index, so once it is gone Keycloak's logout callback finds nothing, no-ops, and returns **200 OK** while the session keeps authenticating requests at this edge until its ceiling. Backchannel revocation fails open and reports success. This is not a style rule — it is the one cross-repo invariant this plugin can break without any test in either repository going red.
- `sid` is exactly 43 base64url characters (`[A-Za-z0-9_-]`).
- Every failure path fails closed: 401 or 503, never "pass through unauthenticated".
- Never log the `sid`, the cookie value, or any token. Log `sub`, path, and outcome.
- The plugin never writes a response body containing details of why authentication failed beyond a generic message.

## Spec Deviation to Apply

The spec's observability section promises Prometheus counters "via the existing `telemetry/metrics`". That is not achievable: KrakenD's metrics collector gathers its own router, proxy and backend metrics and does not ingest a counter registry defined inside a plugin `.so`. This plan ships **structured JSON logs** with an `outcome` field instead (`hit`, `miss`, `expired`, `refreshed`, `error`, `csrf_reject`), which the existing `telemetry/gologging` + `telemetry/logstash` pipeline already carries. If counters become necessary later, the plugin would need its own listener on a separate port — a deliberate follow-up, not part of v1. Update the spec's observability paragraph when this plan is executed.

---

## File Structure

- `plugins/session-resolver/go.mod` — own module, like every other plugin.
- `plugins/session-resolver/main.go` — registerer boilerplate, config parsing, the handler.
- `plugins/session-resolver/matchers.go` — skip-path wildcard matching (same semantics as `jwt-headers`).
- `plugins/session-resolver/session.go` — the Valkey read and its decoded shape.
- `plugins/session-resolver/decide.go` — the pure decision function; all branching logic lives here so it is testable without HTTP or Valkey.
- `plugins/session-resolver/refresh.go` — the lock-guarded call to `auth-bff`.
- `config/settings/session.json` — plugin settings, new file.
- `config/krakend.tmpl` — new `{{- if $sessEnabled }}` block plus the name in the chain array.
- `endpoints.yaml` — five new public `/auth/*` routes.
- `docker-compose.yml` — Valkey service and the new environment variables.
- `Makefile` — `session-resolver` added to `PLUGINS`.

---

### Task 1: Plugin skeleton, config parsing, and skip-path matchers

**Files:**
- Create: `plugins/session-resolver/go.mod`
- Create: `plugins/session-resolver/main.go`
- Create: `plugins/session-resolver/matchers.go`
- Test: `plugins/session-resolver/config_test.go`
- Test: `plugins/session-resolver/matchers_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `pluginConfig` struct with all JSON fields; `parseConfig(extra map[string]interface{}) (*pluginConfig, error)`; `buildMatchers(patterns []string) (map[string]bool, []*regexp.Regexp, error)`; `matchesAny(path string, regexes []*regexp.Regexp) bool`.

- [ ] **Step 1: Create the module**

`plugins/session-resolver/go.mod`:
```
module session-resolver

go 1.25.7

require github.com/valkey-io/valkey-go v1.0.77
```

Run `cd plugins/session-resolver && go mod tidy` after the first file that imports it exists; for now the require line is enough to pin the version.

- [ ] **Step 2: Write the failing config test**

`plugins/session-resolver/config_test.go`:
```go
package main

import (
	"strings"
	"testing"
)

func TestParseConfig(t *testing.T) {
	valid := map[string]interface{}{
		pluginName: map[string]interface{}{
			"valkey_addr":               "valkey:6379",
			"cookie_name":               "sid",
			"key_prefix":                "v1:",
			"idle_ttl_seconds":          1800,
			"refresh_threshold_seconds": 30,
			"refresh_url":               "http://auth-bff:8086/internal/sessions/{sid}/refresh",
			"internal_secret":           "s3cret",
			"allowed_origins":           []interface{}{"http://localhost:5173"},
			"skip_paths":                []interface{}{"/auth/*"},
		},
	}

	t.Run("parses a complete config", func(t *testing.T) {
		cfg, err := parseConfig(valid)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.ValkeyAddr != "valkey:6379" {
			t.Errorf("valkey_addr = %q, want valkey:6379", cfg.ValkeyAddr)
		}
		if cfg.CookieName != "sid" {
			t.Errorf("cookie_name = %q, want sid", cfg.CookieName)
		}
		if len(cfg.AllowedOrigins) != 1 {
			t.Errorf("allowed_origins length = %d, want 1", len(cfg.AllowedOrigins))
		}
	})

	t.Run("applies defaults for optional fields", func(t *testing.T) {
		cfg, _ := parseConfig(valid)
		if cfg.ValkeyTimeoutMs != 200 {
			t.Errorf("valkey_timeout_ms default = %d, want 200", cfg.ValkeyTimeoutMs)
		}
		if cfg.RefreshTimeoutMs != 3000 {
			t.Errorf("refresh_timeout_ms default = %d, want 3000", cfg.RefreshTimeoutMs)
		}
		if cfg.RefreshLockTTLSeconds != 5 {
			t.Errorf("refresh_lock_ttl_seconds default = %d, want 5", cfg.RefreshLockTTLSeconds)
		}
		if len(cfg.CSRFSafeMethods) != 3 {
			t.Errorf("csrf_safe_methods default = %v, want GET/HEAD/OPTIONS", cfg.CSRFSafeMethods)
		}
	})

	// Misconfiguration must stop the gateway from starting, never degrade silently.
	for _, tc := range []struct {
		name    string
		mutate  func(m map[string]interface{})
		wantErr string
	}{
		{"missing config key", func(m map[string]interface{}) { delete(m, pluginName) }, "not found"},
		{"empty valkey_addr", func(m map[string]interface{}) { m[pluginName].(map[string]interface{})["valkey_addr"] = "" }, "valkey_addr"},
		{"empty cookie_name", func(m map[string]interface{}) { m[pluginName].(map[string]interface{})["cookie_name"] = "" }, "cookie_name"},
		{"empty internal_secret", func(m map[string]interface{}) { m[pluginName].(map[string]interface{})["internal_secret"] = "" }, "internal_secret"},
		{"refresh_url without sid placeholder", func(m map[string]interface{}) {
			m[pluginName].(map[string]interface{})["refresh_url"] = "http://auth-bff:8086/refresh"
		}, "{sid}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := cloneConfigMap(valid)
			tc.mutate(cfg)
			_, err := parseConfig(cfg)
			if err == nil {
				t.Fatalf("expected an error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func cloneConfigMap(src map[string]interface{}) map[string]interface{} {
	inner, ok := src[pluginName].(map[string]interface{})
	if !ok {
		return map[string]interface{}{}
	}
	clonedInner := make(map[string]interface{}, len(inner))
	for k, v := range inner {
		clonedInner[k] = v
	}
	return map[string]interface{}{pluginName: clonedInner}
}

```

- [ ] **Step 3: Write the failing matcher test**

`plugins/session-resolver/matchers_test.go`:
```go
package main

import "testing"

func TestSkipPathMatching(t *testing.T) {
	exact, regexes, err := buildMatchers([]string{"/api/ping", "/auth/*", "/api/*/public"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, tc := range []struct {
		path string
		want bool
	}{
		{"/api/ping", true},
		{"/api/pingx", false},
		{"/auth/login/keycloak", true},
		{"/auth", false},
		{"/api/v1/public", true},
		{"/api/v1/v2/public", false},
		{"/api/projects", false},
	} {
		t.Run(tc.path, func(t *testing.T) {
			got := exact[tc.path] || matchesAny(tc.path, regexes)
			if got != tc.want {
				t.Errorf("match(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

func TestBuildMatchersRejectsInvalidPattern(t *testing.T) {
	// Must contain a wildcard: patterns without "*" go to the exact set and never
	// reach regexp.Compile, so they can never fail.
	if _, _, err := buildMatchers([]string{"/api/*[("}); err == nil {
		t.Fatal("expected an error for an invalid wildcard pattern")
	}
}
```

- [ ] **Step 4: Run both tests and verify they fail**

Run: `cd plugins/session-resolver && go test ./...`
Expected: FAIL — `undefined: parseConfig`, `undefined: buildMatchers`.

- [ ] **Step 5: Write the skeleton and config parser**

`plugins/session-resolver/main.go`:
```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
)

var pluginName = "krakend-session-resolver"

var logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})).
	With("plugin", pluginName)

// HandlerRegisterer is the symbol KrakenD looks for to load the plugin.
var HandlerRegisterer = registerer(pluginName)

type registerer string

func (r registerer) RegisterHandlers(f func(
	name string,
	handler func(context.Context, map[string]interface{}, http.Handler) (http.Handler, error),
)) {
	f(string(r), r.registerHandlers)
}

// There is no valkey_pool_size: valkey-go does not pool connections the way
// go-redis does. It auto-pipelines concurrent commands over a small
// multiplexed set of TCP connections (ClientOption.PipelineMultiplex,
// default 2 -> 4 connections for a single-instance client) instead of
// checking a large logical pool in and out per call. A pool-size knob would
// have nothing to control, so it is dropped rather than kept and ignored.
type pluginConfig struct {
	ValkeyAddr            string   `json:"valkey_addr"`
	ValkeyPassword        string   `json:"valkey_password"`
	ValkeyDB              int      `json:"valkey_db"`
	ValkeyTimeoutMs       int      `json:"valkey_timeout_ms"`
	KeyPrefix             string   `json:"key_prefix"`
	CookieName            string   `json:"cookie_name"`
	IdleTTLSeconds        int      `json:"idle_ttl_seconds"`
	RefreshThresholdSecs  int      `json:"refresh_threshold_seconds"`
	RefreshURL            string   `json:"refresh_url"`
	RefreshTimeoutMs      int      `json:"refresh_timeout_ms"`
	RefreshLockTTLSeconds int      `json:"refresh_lock_ttl_seconds"`
	InternalSecret        string   `json:"internal_secret"`
	AllowedOrigins        []string `json:"allowed_origins"`
	CSRFSafeMethods       []string `json:"csrf_safe_methods"`
	SkipPaths             []string `json:"skip_paths"`
}

func parseConfig(extra map[string]interface{}) (*pluginConfig, error) {
	rawCfg, ok := extra[pluginName]
	if !ok {
		return nil, fmt.Errorf("configuration key %q not found", pluginName)
	}

	b, err := json.Marshal(rawCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal config: %w", err)
	}

	var cfg pluginConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	// Required. A misconfigured session plugin must stop the gateway, not fail open.
	if cfg.ValkeyAddr == "" {
		return nil, fmt.Errorf("valkey_addr is required")
	}
	if cfg.CookieName == "" {
		return nil, fmt.Errorf("cookie_name is required")
	}
	if cfg.InternalSecret == "" {
		return nil, fmt.Errorf("internal_secret is required")
	}
	if !strings.Contains(cfg.RefreshURL, "{sid}") {
		return nil, fmt.Errorf("refresh_url must contain the {sid} placeholder")
	}

	// Defaults.
	if cfg.ValkeyTimeoutMs == 0 {
		cfg.ValkeyTimeoutMs = 200
	}
	if cfg.KeyPrefix == "" {
		cfg.KeyPrefix = "v1:"
	}
	if cfg.IdleTTLSeconds == 0 {
		cfg.IdleTTLSeconds = 1800
	}
	if cfg.RefreshThresholdSecs == 0 {
		cfg.RefreshThresholdSecs = 30
	}
	if cfg.RefreshTimeoutMs == 0 {
		cfg.RefreshTimeoutMs = 3000
	}
	if cfg.RefreshLockTTLSeconds == 0 {
		cfg.RefreshLockTTLSeconds = 5
	}
	if len(cfg.CSRFSafeMethods) == 0 {
		cfg.CSRFSafeMethods = []string{http.MethodGet, http.MethodHead, http.MethodOptions}
	}

	return &cfg, nil
}
```

`plugins/session-resolver/matchers.go` — copy the three functions verbatim from `plugins/jwt-headers/main.go` (`buildMatchers`, `compilePattern`, `matchesAny`), changing only the package-level imports. The duplication is intentional: each plugin is its own Go module and KrakenD loads each `.so` independently, so a shared package would have to become a published module for no benefit at this size.

```go
package main

import (
	"fmt"
	"regexp"
	"strings"
)

// buildMatchers splits skip patterns into a fast exact-match set and compiled
// regexes for wildcard patterns. A trailing "/*" matches any number of trailing
// segments; every other "*" matches exactly one segment.
func buildMatchers(patterns []string) (map[string]bool, []*regexp.Regexp, error) {
	exact := make(map[string]bool, len(patterns))
	regexes := make([]*regexp.Regexp, 0)
	for _, p := range patterns {
		if !strings.Contains(p, "*") {
			exact[p] = true
			continue
		}
		re, err := compilePattern(p)
		if err != nil {
			return nil, nil, fmt.Errorf("pattern %q: %w", p, err)
		}
		regexes = append(regexes, re)
	}
	return exact, regexes, nil
}

func compilePattern(p string) (*regexp.Regexp, error) {
	rest := false
	if strings.HasSuffix(p, "/*") {
		rest = true
		p = strings.TrimSuffix(p, "/*")
	}
	parts := strings.Split(p, "*")
	for i, part := range parts {
		parts[i] = regexp.QuoteMeta(part)
	}
	expr := "^" + strings.Join(parts, "[^/]+")
	if rest {
		expr += "(/.*)?"
	}
	expr += "$"
	return regexp.Compile(expr)
}

func matchesAny(path string, regexes []*regexp.Regexp) bool {
	for _, re := range regexes {
		if re.MatchString(path) {
			return true
		}
	}
	return false
}
```

- [ ] **Step 6: Run the tests and verify they pass**

Run: `cd plugins/session-resolver && go test ./...`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add plugins/session-resolver
git commit -m "feat(session-resolver): plugin skeleton, config parsing and skip matchers"
```

---

### Task 2: The decision function

All branching lives in one pure function so it can be tested without HTTP, Valkey, or a clock.

**Files:**
- Create: `plugins/session-resolver/decide.go`
- Test: `plugins/session-resolver/decide_test.go`

**Interfaces:**
- Consumes: `pluginConfig` (Task 1).
- Produces:
  ```go
  type action int
  const (
      actionPassThrough action = iota // bearer present, skip path, or preflight
      actionUnauthorized
      actionForbidden
      actionResolve                   // read Valkey and inject
  )
  type request struct {
      Path, Method, Authorization, Origin, Referer, Cookie string
  }
  func decide(req request, cfg *pluginConfig, skipExact map[string]bool, skipRegexes []*regexp.Regexp) (action, string)
  ```
  The returned string is the `sid` when the action is `actionResolve`, empty otherwise.

- [ ] **Step 1: Write the failing test**

`plugins/session-resolver/decide_test.go`:
```go
package main

import (
	"regexp"
	"testing"
)

func testConfig() *pluginConfig {
	return &pluginConfig{
		CookieName:      "sid",
		AllowedOrigins:  []string{"http://localhost:5173"},
		CSRFSafeMethods: []string{"GET", "HEAD", "OPTIONS"},
	}
}

func testMatchers(t *testing.T) (map[string]bool, []*regexp.Regexp) {
	t.Helper()
	exact, regexes, err := buildMatchers([]string{"/auth/*", "/api/ping"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return exact, regexes
}

func TestDecide(t *testing.T) {
	validSid := "A123456789012345678901234567890123456789012"
	if len(validSid) != 43 {
		t.Fatalf("test fixture sid must be 43 chars, got %d", len(validSid))
	}

	exact, regexes := testMatchers(t)
	cfg := testConfig()

	for _, tc := range []struct {
		name    string
		req     request
		want    action
		wantSid string
	}{
		{
			name: "skip path passes through untouched",
			req:  request{Path: "/auth/login/keycloak", Method: "GET"},
			want: actionPassThrough,
		},
		{
			name: "preflight passes through",
			req:  request{Path: "/api/projects", Method: "OPTIONS"},
			want: actionPassThrough,
		},
		{
			name: "existing bearer wins over the cookie",
			req:  request{Path: "/api/projects", Method: "GET", Authorization: "Bearer mcp-token", Cookie: validSid},
			want: actionPassThrough,
		},
		{
			name: "no cookie and no bearer defers to jwt-headers",
			req:  request{Path: "/api/projects", Method: "GET"},
			want: actionPassThrough,
		},
		{
			name: "malformed sid is rejected without touching valkey",
			req:  request{Path: "/api/projects", Method: "GET", Cookie: "too-short"},
			want: actionUnauthorized,
		},
		{
			name: "sid with characters outside base64url is rejected",
			req:  request{Path: "/api/projects", Method: "GET", Cookie: "A12345678901234567890123456789012345678901+"},
			want: actionUnauthorized,
		},
		{
			name:    "valid cookie on a safe method resolves",
			req:     request{Path: "/api/projects", Method: "GET", Cookie: validSid},
			want:    actionResolve,
			wantSid: validSid,
		},
		{
			name:    "mutating method with an allowed origin resolves",
			req:     request{Path: "/api/projects", Method: "POST", Cookie: validSid, Origin: "http://localhost:5173"},
			want:    actionResolve,
			wantSid: validSid,
		},
		{
			name: "mutating method with a foreign origin is forbidden",
			req:  request{Path: "/api/projects", Method: "POST", Cookie: validSid, Origin: "http://evil.example"},
			want: actionForbidden,
		},
		{
			name: "mutating method with no origin or referer is forbidden",
			req:  request{Path: "/api/projects", Method: "POST", Cookie: validSid},
			want: actionForbidden,
		},
		{
			name:    "mutating method falls back to referer when origin is absent",
			req:     request{Path: "/api/projects", Method: "POST", Cookie: validSid, Referer: "http://localhost:5173/projects"},
			want:    actionResolve,
			wantSid: validSid,
		},
		{
			name: "csrf check applies before the cookie is trusted",
			req:  request{Path: "/api/projects", Method: "DELETE", Cookie: validSid, Origin: "http://evil.example"},
			want: actionForbidden,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, sid := decide(tc.req, cfg, exact, regexes)
			if got != tc.want {
				t.Errorf("action = %v, want %v", got, tc.want)
			}
			if sid != tc.wantSid {
				t.Errorf("sid = %q, want %q", sid, tc.wantSid)
			}
		})
	}
}
```

- [ ] **Step 2: Run the test and verify it fails**

Run: `cd plugins/session-resolver && go test ./... -run TestDecide`
Expected: FAIL — `undefined: decide`.

- [ ] **Step 3: Write the decision function**

`plugins/session-resolver/decide.go`:
```go
package main

import (
	"net/url"
	"regexp"
	"strings"
)

type action int

const (
	actionPassThrough action = iota
	actionUnauthorized
	actionForbidden
	actionResolve
)

type request struct {
	Path          string
	Method        string
	Authorization string
	Origin        string
	Referer       string
	Cookie        string
}

var sidFormat = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// decide is the whole policy of the plugin, expressed without I/O.
func decide(
	req request,
	cfg *pluginConfig,
	skipExact map[string]bool,
	skipRegexes []*regexp.Regexp,
) (action, string) {
	if skipExact[req.Path] || matchesAny(req.Path, skipRegexes) {
		return actionPassThrough, ""
	}

	// CORS preflights carry no credentials and must never be challenged.
	if req.Method == "OPTIONS" {
		return actionPassThrough, ""
	}

	// A caller-supplied bearer always wins: this is what keeps MCP, CI and mobile
	// working. jwt-headers still validates it, so trusting it costs nothing.
	if req.Authorization != "" {
		return actionPassThrough, ""
	}

	// No cookie either: let jwt-headers issue the 401 with its own message.
	if req.Cookie == "" {
		return actionPassThrough, ""
	}

	if !sidFormat.MatchString(req.Cookie) {
		return actionUnauthorized, ""
	}

	if !isSafeMethod(req.Method, cfg.CSRFSafeMethods) && !originAllowed(req, cfg.AllowedOrigins) {
		return actionForbidden, ""
	}

	return actionResolve, req.Cookie
}

func isSafeMethod(method string, safe []string) bool {
	for _, m := range safe {
		if strings.EqualFold(m, method) {
			return true
		}
	}
	return false
}

// originAllowed checks Origin, falling back to Referer's scheme+host. A mutating
// request with neither is rejected: under SameSite=Lax the cookie can still reach
// us cross-site, and an absent Origin is exactly what a forged request looks like.
func originAllowed(req request, allowed []string) bool {
	candidate := req.Origin
	if candidate == "" && req.Referer != "" {
		if u, err := url.Parse(req.Referer); err == nil && u.Scheme != "" && u.Host != "" {
			candidate = u.Scheme + "://" + u.Host
		}
	}
	if candidate == "" {
		return false
	}
	for _, a := range allowed {
		if a == candidate {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: Run the test and verify it passes**

Run: `cd plugins/session-resolver && go test ./... -run TestDecide -v`
Expected: PASS, 12 subtests.

- [ ] **Step 5: Commit**

```bash
git add plugins/session-resolver/decide.go plugins/session-resolver/decide_test.go
git commit -m "feat(session-resolver): pure decision function with dual mode and CSRF"
```

---

### Task 3: Valkey session reader

**Files:**
- Create: `plugins/session-resolver/session.go`
- Test: `plugins/session-resolver/session_test.go`

**Interfaces:**
- Consumes: `pluginConfig`.
- Produces:
  ```go
  type sessionData struct { AccessToken, Subject string; Exp, AbsExp int64 }
  type store struct { ... }
  func newStore(cfg *pluginConfig) (*store, error)
  func (s *store) load(ctx context.Context, sid string) (*sessionData, error)   // nil, nil on miss
  func (s *store) renewIdle(ctx context.Context, sid string)                     // best effort
  func (s *store) drop(ctx context.Context, sid string)
  ```

  `newStore` now returns an error, unlike a typical go-redis constructor. `valkey.NewClient` is not lazy: without `ForceSingleClient`, it dials immediately to probe cluster topology and returns a non-nil error if that probe fails. This plugin always sets `ForceSingleClient: true` (the topology is always one standalone Valkey node, never a cluster) — and per the client's documented behavior, that flag makes `NewClient` return a *usable, self-reconnecting* client even when the initial dial fails, alongside a non-nil error. `newStore` treats "client is non-nil" as success and discards that transient dial error; only a nil client (which the library returns solely for real misconfiguration, e.g. an empty address — already rejected by `parseConfig`) is treated as a fatal error. This preserves the original behavior: the plugin loads even if Valkey is momentarily unreachable at gateway startup, and every subsequent request still fails closed until Valkey answers.

- [ ] **Step 1: Add test dependencies**

```bash
cd plugins/session-resolver
go get github.com/valkey-io/valkey-go@v1.0.77
go get github.com/testcontainers/testcontainers-go@v0.35.0
go mod tidy
```

Test-only dependencies never reach the `.so`: the plugin build compiles only non-test files.

- [ ] **Step 2: Write the failing test**

`plugins/session-resolver/session_test.go`:
```go
package main

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"github.com/valkey-io/valkey-go"
)

func startValkey(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "valkey/valkey:8-alpine",
			ExposedPorts: []string{"6379/tcp"},
			WaitingFor:   wait.ForListeningPort("6379/tcp"),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("failed to start valkey: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	endpoint, err := container.Endpoint(ctx, "")
	if err != nil {
		t.Fatalf("failed to resolve endpoint: %v", err)
	}
	return endpoint
}

func seed(t *testing.T, addr, sid string, exp, absExp int64) valkey.Client {
	t.Helper()
	client, err := valkey.NewClient(valkey.ClientOption{
		InitAddress:       []string{addr},
		ForceSingleClient: true,
	})
	if err != nil {
		t.Fatalf("failed to connect to valkey: %v", err)
	}
	t.Cleanup(client.Close)

	ctx := context.Background()
	key := "v1:session:" + sid
	hset := client.B().Hset().Key(key).FieldValue().
		FieldValue("ver", "1").
		FieldValue("sub", "user-1").
		FieldValue("kc_sid", "kc-1").
		FieldValue("access_token", "the.jwt").
		FieldValue("exp", strconv.FormatInt(exp, 10)).
		FieldValue("abs_exp", strconv.FormatInt(absExp, 10)).
		FieldValue("refresh_token_enc", "k1.iv.cipher").
		FieldValue("id_token_enc", "k1.iv.cipher").
		FieldValue("created_at", strconv.FormatInt(exp-300, 10)).
		Build()
	if err := client.Do(ctx, hset).Error(); err != nil {
		t.Fatalf("seed failed: %v", err)
	}
	if err := client.Do(ctx, client.B().Expire().Key(key).Seconds(1800).Build()).Error(); err != nil {
		t.Fatalf("expire failed: %v", err)
	}
	return client
}

func ttlOf(t *testing.T, client valkey.Client, key string) time.Duration {
	t.Helper()
	seconds, err := client.Do(context.Background(), client.B().Ttl().Key(key).Build()).AsInt64()
	if err != nil {
		t.Fatalf("TTL failed: %v", err)
	}
	return time.Duration(seconds) * time.Second
}

func TestStoreLoad(t *testing.T) {
	addr := startValkey(t)
	sid := "B123456789012345678901234567890123456789012"
	now := time.Now().Unix()
	seed(t, addr, sid, now+300, now+36000)

	s, err := newStore(&pluginConfig{ValkeyAddr: addr, KeyPrefix: "v1:", ValkeyTimeoutMs: 500, IdleTTLSeconds: 1800})
	if err != nil {
		t.Fatalf("newStore failed: %v", err)
	}
	ctx := context.Background()

	t.Run("reads the four contract fields", func(t *testing.T) {
		data, err := s.load(ctx, sid)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if data == nil {
			t.Fatal("expected a session, got nil")
		}
		if data.AccessToken != "the.jwt" {
			t.Errorf("access token = %q, want the.jwt", data.AccessToken)
		}
		if data.Subject != "user-1" {
			t.Errorf("subject = %q, want user-1", data.Subject)
		}
		if data.Exp != now+300 {
			t.Errorf("exp = %d, want %d", data.Exp, now+300)
		}
		if data.AbsExp != now+36000 {
			t.Errorf("abs_exp = %d, want %d", data.AbsExp, now+36000)
		}
	})

	t.Run("returns nil for a missing session", func(t *testing.T) {
		data, err := s.load(ctx, "C123456789012345678901234567890123456789012")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if data != nil {
			t.Errorf("expected nil for a missing session, got %+v", data)
		}
	})

	t.Run("errors when valkey is unreachable", func(t *testing.T) {
		// newStore still succeeds: ForceSingleClient makes valkey.NewClient
		// return a usable, self-reconnecting client even though the initial
		// dial fails. The failure must surface on load(), not construction.
		dead, err := newStore(&pluginConfig{ValkeyAddr: "127.0.0.1:1", KeyPrefix: "v1:", ValkeyTimeoutMs: 100})
		if err != nil {
			t.Fatalf("newStore should tolerate an unreachable address at construction time: %v", err)
		}
		if _, err := dead.load(ctx, sid); err == nil {
			t.Fatal("expected an error when valkey is unreachable")
		}
	})
}

func TestStoreRenewIdleOnlyBelowHalf(t *testing.T) {
	addr := startValkey(t)
	sid := "D123456789012345678901234567890123456789012"
	now := time.Now().Unix()
	client := seed(t, addr, sid, now+300, now+36000)
	ctx := context.Background()

	s, err := newStore(&pluginConfig{ValkeyAddr: addr, KeyPrefix: "v1:", ValkeyTimeoutMs: 500, IdleTTLSeconds: 1800})
	if err != nil {
		t.Fatalf("newStore failed: %v", err)
	}
	key := "v1:session:" + sid

	// TTL is 30m — above half, so renewal must be a no-op write-wise.
	client.Do(ctx, client.B().Expire().Key(key).Seconds(1800).Build())
	s.renewIdle(ctx, sid)
	if ttl := ttlOf(t, client, key); ttl < 29*time.Minute {
		t.Errorf("ttl = %v, expected it left untouched near 30m", ttl)
	}

	// Drop below half: renewal must push it back to the full idle TTL.
	client.Do(ctx, client.B().Expire().Key(key).Seconds(300).Build())
	s.renewIdle(ctx, sid)
	if ttl := ttlOf(t, client, key); ttl < 29*time.Minute {
		t.Errorf("ttl = %v, expected it renewed to ~30m", ttl)
	}

	// The reverse index belongs to auth-bff and carries a TTL running to
	// abs_exp. Renewal must not shorten it to the idle window: if it does, the
	// index expires under a live session and backchannel logout silently
	// no-ops while answering Keycloak 200 OK. Nothing else in either
	// repository catches that, so it is asserted here.
	index := "v1:kcsid:kc-1"
	client.Do(ctx, client.B().Set().Key(index).Value(sid).Ex(10*time.Hour).Build())
	s.renewIdle(ctx, sid)
	if ttl := ttlOf(t, client, index); ttl < 9*time.Hour {
		t.Errorf("kcsid index ttl = %v, want it left near 10h — renewIdle must not touch the index", ttl)
	}
}

func TestStoreDrop(t *testing.T) {
	addr := startValkey(t)
	sid := "E123456789012345678901234567890123456789012"
	now := time.Now().Unix()
	client := seed(t, addr, sid, now+300, now+36000)
	ctx := context.Background()

	s, err := newStore(&pluginConfig{ValkeyAddr: addr, KeyPrefix: "v1:", ValkeyTimeoutMs: 500, IdleTTLSeconds: 1800})
	if err != nil {
		t.Fatalf("newStore failed: %v", err)
	}
	s.drop(ctx, sid)

	n, err := client.Do(ctx, client.B().Exists().Key("v1:session:"+sid).Build()).AsInt64()
	if err != nil {
		t.Fatalf("EXISTS failed: %v", err)
	}
	if n != 0 {
		t.Errorf("session key still exists after drop")
	}
}
```

- [ ] **Step 3: Run the tests and verify they fail**

Run: `cd plugins/session-resolver && go test ./... -run TestStore`
Expected: FAIL — `undefined: newStore`. Docker must be running.

Note: `newStore` returns two values now (`*store, error`); the failing compile error appears the same way regardless.

- [ ] **Step 4: Write the store**

`plugins/session-resolver/session.go`:
```go
package main

import (
	"context"
	"net"
	"time"

	"github.com/valkey-io/valkey-go"
)

// sessionData is the read projection of v1:session:{sid}. The *_enc fields are
// deliberately absent: they are encrypted with a key only auth-bff holds, and the
// edge has no business decrypting them.
type sessionData struct {
	AccessToken string
	Subject     string
	Exp         int64
	AbsExp      int64
}

type store struct {
	client  valkey.Client
	prefix  string
	timeout time.Duration
	idleTTL time.Duration
}

// newStore returns an error only for real misconfiguration. valkey.NewClient
// dials eagerly to probe cluster topology, but ForceSingleClient (always set
// here: this plugin only ever talks to one standalone Valkey node, never a
// cluster) makes it return a usable, self-reconnecting client even when that
// initial dial fails — alongside a non-nil error carrying the dial failure.
// A nil client is the only case that means real misconfiguration (e.g. an
// empty address, already rejected by parseConfig), so that is the only case
// treated as fatal here; the transient dial error is otherwise discarded and
// left to resurface on the first load().
func newStore(cfg *pluginConfig) (*store, error) {
	timeout := time.Duration(cfg.ValkeyTimeoutMs) * time.Millisecond
	client, err := valkey.NewClient(valkey.ClientOption{
		InitAddress:       []string{cfg.ValkeyAddr},
		Password:          cfg.ValkeyPassword,
		SelectDB:          cfg.ValkeyDB,
		Dialer:            net.Dialer{Timeout: timeout},
		ConnWriteTimeout:  timeout,
		ForceSingleClient: true,
	})
	if client == nil {
		return nil, err
	}
	return &store{
		client:  client,
		prefix:  cfg.KeyPrefix,
		timeout: timeout,
		idleTTL: time.Duration(cfg.IdleTTLSeconds) * time.Second,
	}, nil
}

// load returns (nil, nil) when the session does not exist, and a non-nil error
// only when Valkey itself failed — the caller distinguishes 401 from 503 on
// that. HMGET always replies with an array the length of the requested field
// list, even on a missing key (each element nil); a missing session is
// detected from the first field (access_token) being a nil reply, not from
// the array itself being nil.
func (s *store) load(ctx context.Context, sid string) (*sessionData, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	cmd := s.client.B().Hmget().Key(s.key(sid)).Field("access_token", "sub", "exp", "abs_exp").Build()
	values, err := s.client.Do(ctx, cmd).ToArray()
	if err != nil {
		return nil, err
	}
	if len(values) != 4 {
		return nil, nil
	}

	accessToken, err := values[0].ToString()
	if valkey.IsValkeyNil(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	return &sessionData{
		AccessToken: accessToken,
		Subject:     asString(values[1]),
		Exp:         asInt64(values[2]),
		AbsExp:      asInt64(values[3]),
	}, nil
}

// renewIdle extends the key's TTL only once less than half of it remains, so an
// active session costs one write every ~15 minutes instead of one per request.
//
// It touches v1:session:{sid} and nothing else. The reverse index
// v1:kcsid:{kc_sid} is auth-bff's to manage and carries a TTL that runs to the
// session's abs_exp; applying the idle TTL to it here would shorten it on every
// renewal, letting it expire under a live session and silently breaking
// backchannel logout — which would then no-op and answer Keycloak 200 OK.
func (s *store) renewIdle(ctx context.Context, sid string) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	ttlSeconds, err := s.client.Do(ctx, s.client.B().Ttl().Key(s.key(sid)).Build()).AsInt64()
	if err != nil || ttlSeconds <= 0 {
		return
	}
	ttl := time.Duration(ttlSeconds) * time.Second
	if ttl*2 < s.idleTTL {
		expire := s.client.B().Expire().Key(s.key(sid)).Seconds(int64(s.idleTTL.Seconds())).Build()
		_ = s.client.Do(ctx, expire).Error()
	}
}

func (s *store) drop(ctx context.Context, sid string) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	_ = s.client.Do(ctx, s.client.B().Del().Key(s.key(sid)).Build()).Error()
}

func (s *store) key(sid string) string { return s.prefix + "session:" + sid }

// asString and asInt64 tolerate a nil or unexpected reply by returning the
// zero value, matching the original defensive decoding: a corrupted record
// should not panic the request path.
func asString(m valkey.ValkeyMessage) string {
	v, err := m.ToString()
	if err != nil {
		return ""
	}
	return v
}

func asInt64(m valkey.ValkeyMessage) int64 {
	v, err := m.AsInt64()
	if err != nil {
		return 0
	}
	return v
}
```

- [ ] **Step 5: Run the tests and verify they pass**

Run: `cd plugins/session-resolver && go test ./... -run TestStore`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add plugins/session-resolver
git commit -m "feat(session-resolver): valkey session reader with half-life ttl renewal"
```

---

### Task 4: Lock-guarded refresh

**Files:**
- Create: `plugins/session-resolver/refresh.go`
- Test: `plugins/session-resolver/refresh_test.go`

**Interfaces:**
- Consumes: `store`, `pluginConfig`.
- Produces:
  ```go
  type refresher struct { ... }
  func newRefresher(cfg *pluginConfig, s *store) *refresher
  // refresh returns the new access token. err != nil means fail closed.
  func (r *refresher) refresh(ctx context.Context, sid string) (string, error)
  var errSessionGone = errors.New("session gone")
  ```
  `errSessionGone` maps to 401; any other error maps to 503.

- [ ] **Step 1: Write the failing test**

`plugins/session-resolver/refresh_test.go`:
```go
package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRefresh(t *testing.T) {
	addr := startValkey(t)
	sid := "F123456789012345678901234567890123456789012"
	now := time.Now().Unix()
	client := seed(t, addr, sid, now+5, now+36000)

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		if req.Header.Get("X-Internal-Secret") != "s3cret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if !strings.Contains(req.URL.Path, sid) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		// The new token must land in Valkey, exactly as auth-bff does it.
		hset := client.B().Hset().Key("v1:session:"+sid).FieldValue().FieldValue("access_token", "fresh.jwt").Build()
		client.Do(req.Context(), hset)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"fresh.jwt"}`))
	}))
	defer server.Close()

	cfg := &pluginConfig{
		ValkeyAddr:            addr,
		KeyPrefix:             "v1:",
		ValkeyTimeoutMs:       500,
		IdleTTLSeconds:        1800,
		RefreshURL:            server.URL + "/internal/sessions/{sid}/refresh",
		RefreshTimeoutMs:      2000,
		RefreshLockTTLSeconds: 5,
		InternalSecret:        "s3cret",
	}
	s, err := newStore(cfg)
	if err != nil {
		t.Fatalf("newStore failed: %v", err)
	}
	r := newRefresher(cfg, s)

	t.Run("returns the refreshed access token", func(t *testing.T) {
		token, err := r.refresh(context.Background(), sid)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if token != "fresh.jwt" {
			t.Errorf("token = %q, want fresh.jwt", token)
		}
	})

	t.Run("only one of many concurrent callers hits auth-bff", func(t *testing.T) {
		client.Do(context.Background(), client.B().Del().Key("v1:lock:refresh:"+sid).Build())
		calls.Store(0)

		var wg sync.WaitGroup
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = r.refresh(context.Background(), sid)
			}()
		}
		wg.Wait()

		if got := calls.Load(); got != 1 {
			t.Errorf("auth-bff called %d times, want exactly 1", got)
		}
	})
}

func TestRefreshFailureModes(t *testing.T) {
	addr := startValkey(t)
	sid := "G123456789012345678901234567890123456789012"
	now := time.Now().Unix()
	seed(t, addr, sid, now+5, now+36000)

	t.Run("401 from auth-bff means the session is gone", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer server.Close()

		cfg := refreshConfig(addr, server.URL)
		s, err := newStore(cfg)
		if err != nil {
			t.Fatalf("newStore failed: %v", err)
		}
		r := newRefresher(cfg, s)

		_, err = r.refresh(context.Background(), sid)
		if !errors.Is(err, errSessionGone) {
			t.Errorf("err = %v, want errSessionGone", err)
		}
	})

	t.Run("5xx from auth-bff is a plain error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer server.Close()

		cfg := refreshConfig(addr, server.URL)
		s, err := newStore(cfg)
		if err != nil {
			t.Fatalf("newStore failed: %v", err)
		}
		r := newRefresher(cfg, s)

		_, err = r.refresh(context.Background(), sid)
		if err == nil || errors.Is(err, errSessionGone) {
			t.Errorf("err = %v, want a non-session-gone error", err)
		}
	})
}

func refreshConfig(valkeyAddr, serverURL string) *pluginConfig {
	return &pluginConfig{
		ValkeyAddr:            valkeyAddr,
		KeyPrefix:             "v1:",
		ValkeyTimeoutMs:       500,
		IdleTTLSeconds:        1800,
		RefreshURL:            serverURL + "/internal/sessions/{sid}/refresh",
		RefreshTimeoutMs:      2000,
		RefreshLockTTLSeconds: 5,
		InternalSecret:        "s3cret",
	}
}
```

The concurrency subtest depends on losers re-reading Valkey rather than calling `auth-bff`. Implement it that way.

- [ ] **Step 2: Run the test and verify it fails**

Run: `cd plugins/session-resolver && go test ./... -run TestRefresh`
Expected: FAIL — `undefined: newRefresher`.

- [ ] **Step 3: Write the refresher**

`plugins/session-resolver/refresh.go`:
```go
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/valkey-io/valkey-go"
)

var errSessionGone = errors.New("session gone")

type refresher struct {
	store    *store
	client   *http.Client
	url      string
	secret   string
	lockTTL  time.Duration
	timeout  time.Duration
	waitStep time.Duration
	maxWait  time.Duration
}

func newRefresher(cfg *pluginConfig, s *store) *refresher {
	timeout := time.Duration(cfg.RefreshTimeoutMs) * time.Millisecond
	return &refresher{
		store:    s,
		client:   &http.Client{Timeout: timeout},
		url:      cfg.RefreshURL,
		secret:   cfg.InternalSecret,
		lockTTL:  time.Duration(cfg.RefreshLockTTLSeconds) * time.Second,
		timeout:  timeout,
		waitStep: 50 * time.Millisecond,
		maxWait:  500 * time.Millisecond,
	}
}

// refresh serialises concurrent refreshes for one session behind a Valkey lock.
// The winner calls auth-bff; the losers wait and re-read the token it wrote.
func (r *refresher) refresh(ctx context.Context, sid string) (string, error) {
	won, err := r.acquire(ctx, sid)
	if err != nil {
		return "", err
	}
	if !won {
		return r.waitForOtherRefresh(ctx, sid)
	}
	return r.callAuthBff(ctx, sid)
}

// acquire returns (true, nil) when this caller won the lock, and (false, nil)
// — not an error — when SET NX found the key already held. valkey-go reports
// a "not set" NX outcome as a nil reply, surfaced as the sentinel error
// valkey.Nil, so it must be checked before treating err as a real failure.
func (r *refresher) acquire(ctx context.Context, sid string) (bool, error) {
	lockCtx, cancel := context.WithTimeout(ctx, r.store.timeout)
	defer cancel()

	lockKey := r.store.prefix + "lock:refresh:" + sid
	cmd := r.store.client.B().Set().Key(lockKey).Value("1").Nx().Ex(r.lockTTL).Build()
	err := r.store.client.Do(lockCtx, cmd).Error()
	if valkey.IsValkeyNil(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (r *refresher) waitForOtherRefresh(ctx context.Context, sid string) (string, error) {
	deadline := time.Now().Add(r.maxWait)
	for time.Now().Before(deadline) {
		time.Sleep(r.waitStep)

		data, err := r.store.load(ctx, sid)
		if err != nil {
			return "", err
		}
		if data == nil {
			return "", errSessionGone
		}
		if data.Exp > time.Now().Unix() {
			return data.AccessToken, nil
		}
	}
	return "", errors.New("timed out waiting for a concurrent refresh")
}

func (r *refresher) callAuthBff(ctx context.Context, sid string) (string, error) {
	url := strings.ReplaceAll(r.url, "{sid}", sid)

	reqCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Internal-Secret", r.secret)

	resp, err := r.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return "", errSessionGone
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("auth-bff returned status %d", resp.StatusCode)
	}

	var body struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	if body.AccessToken == "" {
		return "", errors.New("auth-bff returned an empty access token")
	}
	return body.AccessToken, nil
}
```

- [ ] **Step 4: Run the tests and verify they pass**

Run: `cd plugins/session-resolver && go test ./... -run TestRefresh`
Expected: PASS. The concurrency subtest must report exactly one call.

- [ ] **Step 5: Commit**

```bash
git add plugins/session-resolver/refresh.go plugins/session-resolver/refresh_test.go
git commit -m "feat(session-resolver): lock-guarded refresh against auth-bff"
```

---

### Task 5: The HTTP handler

**Files:**
- Modify: `plugins/session-resolver/main.go`
- Test: `plugins/session-resolver/handler_test.go`

**Interfaces:**
- Consumes: `decide`, `store`, `refresher`.
- Produces: `registerHandlers` returning the wired `http.Handler`.

- [ ] **Step 1: Write the failing test**

`plugins/session-resolver/handler_test.go`:
```go
package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func buildHandler(t *testing.T, cfg map[string]interface{}) (http.Handler, *capturedRequest) {
	t.Helper()
	captured := &capturedRequest{}
	next := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		captured.Authorization = req.Header.Get("Authorization")
		captured.Cookie = req.Header.Get("Cookie")
		captured.Called = true
		w.WriteHeader(http.StatusOK)
	})

	handler, err := registerer(pluginName).registerHandlers(
		context.Background(),
		map[string]interface{}{pluginName: cfg},
		next,
	)
	if err != nil {
		t.Fatalf("failed to build handler: %v", err)
	}
	return handler, captured
}

type capturedRequest struct {
	Called        bool
	Authorization string
	Cookie        string
}

func handlerConfig(valkeyAddr string) map[string]interface{} {
	return map[string]interface{}{
		"valkey_addr":               valkeyAddr,
		"cookie_name":               "sid",
		"key_prefix":                "v1:",
		"valkey_timeout_ms":         500,
		"idle_ttl_seconds":          1800,
		"refresh_threshold_seconds": 30,
		"refresh_url":               "http://127.0.0.1:1/internal/sessions/{sid}/refresh",
		"internal_secret":           "s3cret",
		"allowed_origins":           []interface{}{"http://localhost:5173"},
		"skip_paths":                []interface{}{"/auth/*"},
	}
}

func TestHandlerInjectsBearerFromCookie(t *testing.T) {
	addr := startValkey(t)
	sid := "H123456789012345678901234567890123456789012"
	now := time.Now().Unix()
	seed(t, addr, sid, now+300, now+36000)

	handler, captured := buildHandler(t, handlerConfig(addr))

	req := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
	req.AddCookie(&http.Cookie{Name: "sid", Value: sid})
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if captured.Authorization != "Bearer the.jwt" {
		t.Errorf("Authorization = %q, want Bearer the.jwt", captured.Authorization)
	}
	// The backend must never see the session identifier.
	if captured.Cookie != "" {
		t.Errorf("Cookie reached the backend: %q", captured.Cookie)
	}
}

func TestHandlerPassesBearerThroughUntouched(t *testing.T) {
	addr := startValkey(t)
	handler, captured := buildHandler(t, handlerConfig(addr))

	req := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
	req.Header.Set("Authorization", "Bearer mcp-token")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if captured.Authorization != "Bearer mcp-token" {
		t.Errorf("Authorization = %q, want the caller's own token", captured.Authorization)
	}
}

func TestHandlerFailureModes(t *testing.T) {
	addr := startValkey(t)
	sid := "J123456789012345678901234567890123456789012"
	now := time.Now().Unix()
	seed(t, addr, sid, now+300, now+36000)

	expired := "K123456789012345678901234567890123456789012"
	seed(t, addr, expired, now+300, now-1) // abs_exp already passed

	for _, tc := range []struct {
		name       string
		build      func() (*http.Request, string)
		wantStatus int
		wantNext   bool
	}{
		{
			name: "unknown session is 401",
			build: func() (*http.Request, string) {
				r := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
				r.AddCookie(&http.Cookie{Name: "sid", Value: "L123456789012345678901234567890123456789012"})
				return r, addr
			},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "session past its absolute expiry is 401",
			build: func() (*http.Request, string) {
				r := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
				r.AddCookie(&http.Cookie{Name: "sid", Value: expired})
				return r, addr
			},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "cross origin mutation is 403",
			build: func() (*http.Request, string) {
				r := httptest.NewRequest(http.MethodPost, "/api/projects", nil)
				r.AddCookie(&http.Cookie{Name: "sid", Value: sid})
				r.Header.Set("Origin", "http://evil.example")
				return r, addr
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name: "skip path reaches the backend without a token",
			build: func() (*http.Request, string) {
				return httptest.NewRequest(http.MethodGet, "/auth/login/keycloak", nil), addr
			},
			wantStatus: http.StatusOK,
			wantNext:   true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, valkeyAddr := tc.build()
			handler, captured := buildHandler(t, handlerConfig(valkeyAddr))
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if captured.Called != tc.wantNext {
				t.Errorf("backend called = %v, want %v", captured.Called, tc.wantNext)
			}
		})
	}
}

func TestHandlerFailsClosedWhenValkeyIsDown(t *testing.T) {
	cfg := handlerConfig("127.0.0.1:1")
	cfg["valkey_timeout_ms"] = 100
	handler, captured := buildHandler(t, cfg)

	req := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
	req.AddCookie(&http.Cookie{Name: "sid", Value: "M123456789012345678901234567890123456789012"})
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if captured.Called {
		t.Error("backend was called while Valkey was down — must fail closed")
	}
}
```

- [ ] **Step 2: Run the test and verify it fails**

Run: `cd plugins/session-resolver && go test ./... -run TestHandler`
Expected: FAIL — `registerHandlers` is undefined.

- [ ] **Step 3: Write the handler**

Append to `plugins/session-resolver/main.go`:
```go
func (r registerer) registerHandlers(
	_ context.Context,
	extra map[string]interface{},
	h http.Handler,
) (http.Handler, error) {
	cfg, err := parseConfig(extra)
	if err != nil {
		return nil, fmt.Errorf("[%s] failed to parse config: %w", pluginName, err)
	}

	skipExact, skipRegexes, err := buildMatchers(cfg.SkipPaths)
	if err != nil {
		return nil, fmt.Errorf("[%s] invalid skip_paths: %w", pluginName, err)
	}

	sessions, err := newStore(cfg)
	if err != nil {
		return nil, fmt.Errorf("[%s] failed to connect to valkey: %w", pluginName, err)
	}
	refresh := newRefresher(cfg, sessions)
	threshold := int64(cfg.RefreshThresholdSecs)

	logger.Info("plugin loaded",
		"valkey_addr", cfg.ValkeyAddr,
		"cookie_name", cfg.CookieName,
		"skip_paths", cfg.SkipPaths,
		"allowed_origins", cfg.AllowedOrigins)

	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		what, sid := decide(toRequest(req, cfg.CookieName), cfg, skipExact, skipRegexes)

		switch what {
		case actionPassThrough:
			h.ServeHTTP(w, req)
			return
		case actionUnauthorized:
			deny(w, req, http.StatusUnauthorized, "malformed_session")
			return
		case actionForbidden:
			deny(w, req, http.StatusForbidden, "csrf_reject")
			return
		}

		ctx := req.Context()

		data, err := sessions.load(ctx, sid)
		if err != nil {
			// Valkey unreachable: fail closed. Never forward without identity.
			logger.Error("session store unavailable", "path", req.URL.Path, "err", err.Error())
			deny(w, req, http.StatusServiceUnavailable, "store_error")
			return
		}
		if data == nil {
			deny(w, req, http.StatusUnauthorized, "miss")
			return
		}

		now := time.Now().Unix()

		if now >= data.AbsExp {
			sessions.drop(ctx, sid)
			logger.Info("session past absolute expiry", "sub", data.Subject, "outcome", "expired")
			deny(w, req, http.StatusUnauthorized, "expired")
			return
		}

		token := data.AccessToken
		if now >= data.Exp-threshold {
			token, err = refresh.refresh(ctx, sid)
			if err != nil {
				if errors.Is(err, errSessionGone) {
					deny(w, req, http.StatusUnauthorized, "refresh_session_gone")
					return
				}
				logger.Error("refresh failed", "sub", data.Subject, "err", err.Error())
				deny(w, req, http.StatusServiceUnavailable, "refresh_error")
				return
			}
		}

		sessions.renewIdle(ctx, sid)

		// Set, not Add: overwrite anything the caller smuggled in.
		req.Header.Set("Authorization", "Bearer "+token)
		// The session identifier is a credential; backends must never receive it.
		req.Header.Del("Cookie")

		h.ServeHTTP(w, req)
	}), nil
}

func toRequest(req *http.Request, cookieName string) request {
	value := ""
	if c, err := req.Cookie(cookieName); err == nil {
		value = c.Value
	}
	return request{
		Path:          req.URL.Path,
		Method:        req.Method,
		Authorization: req.Header.Get("Authorization"),
		Origin:        req.Header.Get("Origin"),
		Referer:       req.Header.Get("Referer"),
		Cookie:        value,
	}
}

// deny writes a generic body. Never leak why authentication failed, and never
// log the sid or the cookie value.
func deny(w http.ResponseWriter, req *http.Request, status int, outcome string) {
	logger.Info("request denied", "path", req.URL.Path, "method", req.Method,
		"status", status, "outcome", outcome)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"message":"unauthorized"}`))
}
```

Add `"errors"` and `"time"` to the import block in `main.go`.

- [ ] **Step 4: Run the whole suite**

Run: `cd plugins/session-resolver && go test ./...`
Expected: PASS across all files.

- [ ] **Step 5: Verify the plugin compiles as a KrakenD plugin**

```bash
make builder
docker run --rm -v "$(pwd)/plugins/session-resolver:/app" -w /app \
  codehunters-plugin-builder:local \
  go build -buildmode=plugin -o /app/session-resolver.so .
```
Expected: the `.so` is produced. Delete it afterwards — `make plugin-build` writes the real one into `plugins/build/`.

- [ ] **Step 6: Commit**

```bash
git add plugins/session-resolver
git commit -m "feat(session-resolver): http handler with fail-closed session resolution"
```

---

### Task 6: Gateway configuration and template wiring

**Files:**
- Create: `config/settings/session.json`
- Modify: `config/krakend.tmpl` (chain array and a new config block)
- Modify: `Makefile:11` (`PLUGINS`)

**Interfaces:**
- Consumes: the plugin from Task 5.
- Produces: a rendered KrakenD configuration in which `krakend-session-resolver` sits between `krakend-ip-resolver` and `krakend-jwt-headers`.

- [ ] **Step 1: Write the settings file**

`config/settings/session.json`:
```json
{
  "enabled": true,
  "valkey_addr": "valkey:6379",
  "valkey_db": 0,
  "valkey_timeout_ms": 200,
  "key_prefix": "v1:",
  "cookie_name": "sid",
  "idle_ttl_seconds": 1800,
  "refresh_threshold_seconds": 30,
  "refresh_url": "http://auth-bff:8086/internal/sessions/{sid}/refresh",
  "refresh_timeout_ms": 3000,
  "refresh_lock_ttl_seconds": 5,
  "allowed_origins": ["http://localhost:5173"],
  "csrf_safe_methods": ["GET", "HEAD", "OPTIONS"],
  "skip_paths": ["/auth/*", "/api/ping"]
}
```

`valkey_password` and `internal_secret` are absent on purpose: they are secrets and come from the environment through the template. There is no `valkey_pool_size`: see the comment on `pluginConfig` in Task 1 for why that knob does not carry over from go-redis.

- [ ] **Step 2: Add the toggle to the template header**

In `config/krakend.tmpl`, after the `$jwtEnabled` line:
```
{{- $sessEnabled := eq (env "SESSION_ENABLED" | default (printf "%v" .session.enabled)) "true" -}}
```

- [ ] **Step 3: Add the plugin to the chain array**

In the `plugin/http-server.name` array, between the `ip-resolver` and `jwt-headers` entries, replacing the existing `ip-resolver` block:
```
        {{- if $irEnabled }}
        "krakend-ip-resolver"{{ if or $sessEnabled $jwtEnabled }},{{ end }}
        {{- end }}
        {{- if $sessEnabled }}
        "krakend-session-resolver"{{ if $jwtEnabled }},{{ end }}
        {{- end }}
        {{- if $jwtEnabled }}
        "krakend-jwt-headers"
        {{- end }}
```

Order in this array is the execution order. `session-resolver` must precede `jwt-headers`, otherwise `jwt-headers` sees no `Authorization` header and rejects every cookie-authenticated request.

- [ ] **Step 4: Add the configuration block**

Immediately before the `{{- if $jwtEnabled }}` config block, and update the preceding block's trailing comma condition to include `$sessEnabled`:
```
      {{- if $sessEnabled }}
      "krakend-session-resolver": {
        "valkey_addr": "{{ env "VALKEY_ADDR" | default .session.valkey_addr }}",
        "valkey_password": "{{ env "VALKEY_PASSWORD" | default "" }}",
        "valkey_db": {{ .session.valkey_db }},
        "valkey_timeout_ms": {{ .session.valkey_timeout_ms }},
        "key_prefix": "{{ .session.key_prefix }}",
        "cookie_name": "{{ env "SESSION_COOKIE_NAME" | default .session.cookie_name }}",
        "idle_ttl_seconds": {{ .session.idle_ttl_seconds }},
        "refresh_threshold_seconds": {{ .session.refresh_threshold_seconds }},
        "refresh_url": "{{ env "AUTH_BFF_REFRESH_URL" | default .session.refresh_url }}",
        "refresh_timeout_ms": {{ .session.refresh_timeout_ms }},
        "refresh_lock_ttl_seconds": {{ .session.refresh_lock_ttl_seconds }},
        "internal_secret": "{{ env "INTERNAL_SHARED_SECRET" | default "" }}",
        "allowed_origins": {{ marshal .session.allowed_origins }},
        "csrf_safe_methods": {{ marshal .session.csrf_safe_methods }},
        "skip_paths": {{ marshal .session.skip_paths }}
      }{{ if $jwtEnabled }},{{ end }}
      {{- end }}
```

Also extend the `{{- if or $gtEnabled $alEnabled $irEnabled $jwtEnabled }},{{ end }}` guard that follows the `name` array to include `$sessEnabled`, and the equivalent guard at the end of the `krakend-ip-resolver` block.

- [ ] **Step 5: Register the plugin for building**

`Makefile:11`:
```make
PLUGINS = jwt-headers ip-resolver trace-context accept-language gateway-timeout session-resolver
```

- [ ] **Step 6: Verify the rendered configuration**

```bash
FC_ENABLE=1 FC_SETTINGS=config/settings INTERNAL_SHARED_SECRET=dev \
  krakend check -d -t -c config/krakend.tmpl
```
Expected: `Syntax OK!`

Then confirm the chain order in the rendered output:
```bash
FC_ENABLE=1 FC_SETTINGS=config/settings FC_OUT=config/rendered.json INTERNAL_SHARED_SECRET=dev \
  krakend check -d -t -c config/krakend.tmpl
python3 -c "import json;print(json.load(open('config/rendered.json'))['extra_config']['plugin/http-server']['name'])"
rm config/rendered.json
```
Expected: `krakend-session-resolver` appears immediately before `krakend-jwt-headers`.

Render `FC_OUT` inside `config/`, not `/tmp` — Docker's `/tmp` is not shared with the host here.

- [ ] **Step 7: Verify the toggle disables everything**

```bash
FC_ENABLE=1 FC_SETTINGS=config/settings SESSION_ENABLED=false \
  krakend check -d -t -c config/krakend.tmpl
```
Expected: `Syntax OK!` and no `krakend-session-resolver` in the output. This is the rollback path — it must work without `INTERNAL_SHARED_SECRET` being set.

- [ ] **Step 8: Commit**

```bash
git add config/settings/session.json config/krakend.tmpl Makefile
git commit -m "feat(gateway): wire session-resolver into the plugin chain"
```

---

### Task 7: Public `/auth/*` routes

**Files:**
- Modify: `endpoints.yaml`
- Modify: `config/settings/endpoints.json` (generated — never hand-edited)

**Interfaces:**
- Consumes: the `authbff` backend definition added here.
- Produces: five routes reaching `auth-bff`, all `auth: public` so the generator adds them to the JWT plugin's `skip_paths`.

- [ ] **Step 1: Add the backend**

In `endpoints.yaml` under `backends:`:
```yaml
  authbff:
    host_default: http://host.docker.internal:8086
    host_env: AUTH_BFF_HOST
```

- [ ] **Step 2: Add the header set**

Under `x-header-sets:`:
```yaml
  # auth-bff resolves the session cookie itself, so Cookie must reach it.
  session: &session
    - Accept
    - Content-Type
    - Cookie
    - Origin
    - Referer
    - Traceparent
    - Tracestate
```

- [ ] **Step 3: Add the routes**

Append to `endpoints:`:
```yaml
  # Session/auth flows. All public at the gateway: auth-bff owns their security,
  # and requiring a JWT to reach the login endpoint would be circular.
  - path: /auth/login/{provider}
    method: GET
    backend: authbff
    auth: public
    input_headers: *session
    rate_limit:
      max_rate: 20
      client_max_rate: 5
      strategy: ip

  # Keycloak returns code and state as query strings; KrakenD drops any query
  # parameter not declared here, which would silently break the code exchange.
  - path: /auth/callback
    method: GET
    backend: authbff
    auth: public
    input_headers: *session
    input_query_strings:
      - code
      - state
      - session_state
      - iss
      - error
      - error_description
    rate_limit:
      max_rate: 20
      client_max_rate: 5
      strategy: ip

  - path: /auth/session
    method: GET
    backend: authbff
    auth: public
    input_headers: *session

  - path: /auth/logout
    method: POST
    backend: authbff
    auth: public
    input_headers: *session

  # Called by Keycloak server-to-server, never by a browser.
  - path: /auth/backchannel-logout
    method: POST
    backend: authbff
    auth: public
    input_headers: *session
```

- [ ] **Step 4: Regenerate and verify**

```bash
make gen
python3 -c "
import json
d = json.load(open('config/settings/endpoints.json'))
auth = [e for e in d['endpoints'] if e['path'].startswith('/auth/')]
print(len(auth), 'auth routes')
for e in auth:
    print(e['method'], e['path'], e['auth'], e.get('input_query_strings'))
"
```
Expected: five routes, all `public`, with the six query strings present on `/auth/callback`.

- [ ] **Step 5: Confirm the JWT plugin skips them**

```bash
FC_ENABLE=1 FC_SETTINGS=config/settings FC_OUT=config/rendered.json INTERNAL_SHARED_SECRET=dev \
  krakend check -d -t -c config/krakend.tmpl
python3 -c "
import json
c = json.load(open('config/rendered.json'))
print(c['extra_config']['plugin/http-server']['krakend-jwt-headers']['skip_paths'])
"
rm config/rendered.json
```
Expected: all five `/auth/*` paths present alongside `/public/*` and `/api/ping`.

- [ ] **Step 6: Commit**

```bash
git add endpoints.yaml config/settings/endpoints.json
git commit -m "feat(gateway): public auth-bff routes for the session flows"
```

---

### Task 8: Valkey in compose and the environment surface

**Files:**
- Modify: `docker-compose.yml`

**Interfaces:**
- Consumes: everything above.
- Produces: a locally runnable gateway with Valkey reachable at `valkey:6379`.

- [ ] **Step 1: Add the Valkey service**

```yaml
  valkey:
    image: valkey/valkey:8.1-alpine
    container_name: forgeos-gw-valkey
    # appendonly no is deliberate: session tokens must not reach disk.
    command: ["valkey-server", "--requirepass", "${VALKEY_PASSWORD:-devpassword}", "--appendonly", "no"]
    healthcheck:
      test: ["CMD", "valkey-cli", "-a", "${VALKEY_PASSWORD:-devpassword}", "ping"]
      interval: 5s
      timeout: 3s
      retries: 10
```

No `ports:` mapping — Valkey holds credentials and must not be reachable from the host network. Tasks that need to inspect it use `docker compose exec valkey valkey-cli`.

- [ ] **Step 2: Wire the gateway to it**

In the `krakend` service, add `depends_on` and the new environment variables:
```yaml
    depends_on:
      valkey:
        condition: service_healthy
```
```yaml
      - SESSION_ENABLED=${SESSION_ENABLED:-}
      - VALKEY_ADDR=${VALKEY_ADDR:-valkey:6379}
      - VALKEY_PASSWORD=${VALKEY_PASSWORD:-devpassword}
      - SESSION_COOKIE_NAME=${SESSION_COOKIE_NAME:-sid}
      - AUTH_BFF_HOST=${AUTH_BFF_HOST:-http://host.docker.internal:8086}
      - AUTH_BFF_REFRESH_URL=${AUTH_BFF_REFRESH_URL:-http://host.docker.internal:8086/internal/sessions/{sid}/refresh}
      - INTERNAL_SHARED_SECRET=${INTERNAL_SHARED_SECRET:?internal shared secret is required}
```

`INTERNAL_SHARED_SECRET` uses `:?` so compose refuses to start rather than running with an empty secret — and the plugin's `parseConfig` rejects an empty value anyway, so a missing secret fails twice, loudly.

- [ ] **Step 3: Verify the stack starts**

```bash
export INTERNAL_SHARED_SECRET=$(head -c 24 /dev/urandom | base64)
make plugin-build
docker compose up -d
docker compose logs krakend | grep session-resolver
```
Expected: a `plugin loaded` line naming `krakend-session-resolver`, with `valkey_addr=valkey:6379`.

- [ ] **Step 4: Verify bearer traffic is unaffected**

```bash
curl -s -o /dev/null -w '%{http_code}\n' http://localhost:8090/api/ping
```
Expected: the same status as before this plan — the public route still works with no session and no token.

- [ ] **Step 5: Verify fail-closed on a bogus cookie**

```bash
curl -s -o /dev/null -w '%{http_code}\n' \
  -H "Cookie: sid=$(head -c 32 /dev/urandom | base64 | tr '+/' '-_' | tr -d '=')" \
  http://localhost:8090/api/projects
```
Expected: `401` — a well-formed but unknown session id is rejected, and the backend is never called.

- [ ] **Step 6: Commit**

```bash
git add docker-compose.yml
git commit -m "feat(gateway): valkey service and session-resolver environment surface"
```

---

### Task 9: End-to-end verification and documentation

**Files:**
- Create: `docs/session-flow.md`
- Modify: `README.md`

**Interfaces:**
- Consumes: a running `auth-bff` from the companion plan.
- Produces: a documented, reproducible verification of the whole path.

- [ ] **Step 1: Run the full path by hand**

With Keycloak, Valkey, `auth-bff` and the gateway all running:

```bash
# 1. Log in through the gateway; -c stores the cookie jar.
curl -sL -c /tmp/jar.txt -o /dev/null -w '%{http_code}\n' \
  "http://localhost:8090/auth/login/keycloak"
# Complete the Keycloak login in a browser instead if the realm requires a form.

# 2. The jar must hold a sid cookie and no token.
grep sid /tmp/jar.txt

# 3. The session view resolves.
curl -s -b /tmp/jar.txt http://localhost:8090/auth/session
# Expected: {"sub":"...","exp":...}

# 4. A protected API call works with the cookie alone.
curl -s -o /dev/null -w '%{http_code}\n' -b /tmp/jar.txt \
  http://localhost:8090/api/projects
# Expected: whatever the backend returns for an authenticated user, not 401.

# 5. The same call without the cookie is rejected.
curl -s -o /dev/null -w '%{http_code}\n' http://localhost:8090/api/projects
# Expected: 401

# 6. A cross-origin mutation is rejected.
curl -s -o /dev/null -w '%{http_code}\n' -X POST -b /tmp/jar.txt \
  -H "Origin: http://evil.example" http://localhost:8090/api/projects
# Expected: 403
```

- [ ] **Step 2: Verify the backend never receives the session id**

```bash
docker compose logs krakend | grep -ci 'sid=' || echo "no sid in gateway logs (correct)"
docker compose exec valkey valkey-cli -a "$VALKEY_PASSWORD" --scan --pattern 'v1:session:*'
```
Expected: no `sid=` in logs. The Valkey scan should show one key per active session.

- [ ] **Step 3: Verify revocation is immediate**

```bash
SID=$(docker compose exec -T valkey valkey-cli -a "$VALKEY_PASSWORD" --scan --pattern 'v1:session:*' | head -1 | cut -d: -f3)
docker compose exec -T valkey valkey-cli -a "$VALKEY_PASSWORD" DEL "v1:session:$SID"
curl -s -o /dev/null -w '%{http_code}\n' -b /tmp/jar.txt http://localhost:8090/api/projects
```
Expected: `401` on the very next request — no cache window.

- [ ] **Step 4: Verify the MCP path still works**

```bash
curl -s -o /dev/null -w '%{http_code}\n' -X POST http://localhost:8090/mcp \
  -H "Authorization: Bearer $A_REAL_KEYCLOAK_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'
```
Expected: the same result as before this plan. This is the dual-mode guarantee; if it regressed, the plugin is consuming bearer traffic it should be ignoring.

- [ ] **Step 5: Verify rollback**

```bash
SESSION_ENABLED=false docker compose up -d
curl -s -o /dev/null -w '%{http_code}\n' -b /tmp/jar.txt http://localhost:8090/api/projects
```
Expected: `401` — with the plugin disabled, cookie traffic is no longer resolved and the gateway behaves exactly as it did before this plan. Bearer traffic keeps working.

- [ ] **Step 6: Write the documentation**

`docs/session-flow.md` must cover: the plugin chain order and why `session-resolver` precedes `jwt-headers`; the four flows from the spec; the `v1:` Valkey contract with a pointer to `auth-bff` as its owner; every environment variable; the failure-mode table; the rollback switch; and a runbook note that a Valkey outage takes down cookie traffic while bearer traffic survives.

In `README.md`, add `session-resolver` to the plugin list and link to `docs/session-flow.md`.

- [ ] **Step 7: Commit**

```bash
git add docs/session-flow.md README.md
git commit -m "docs: session resolution flow, contract and runbook"
```

---

## Verification Summary

| Property | Verified by |
|---|---|
| Cookie resolves to a valid bearer | Task 5 `TestHandlerInjectsBearerFromCookie`, Task 9 Step 1.4 |
| Bearer clients unaffected | Task 5 `TestHandlerPassesBearerThroughUntouched`, Task 9 Step 4 |
| Backend never sees the cookie | Task 5 `TestHandlerInjectsBearerFromCookie` |
| Fail closed on Valkey outage | Task 5 `TestHandlerFailsClosedWhenValkeyIsDown` |
| Absolute expiry enforced | Task 5 `TestHandlerFailureModes` |
| CSRF rejected | Task 5 `TestHandlerFailureModes`, Task 9 Step 1.6 |
| One refresh under concurrency | Task 4 `TestRefresh` concurrency subtest |
| Idle TTL renewed at half life | Task 3 `TestStoreRenewIdleOnlyBelowHalf` |
| Revocation is immediate | Task 9 Step 3 |
| Chain order correct | Task 6 Step 6 |
| Rollback works | Task 6 Step 7, Task 9 Step 5 |

## Open Items

- The plugin duplicates `buildMatchers` / `compilePattern` / `matchesAny` from `jwt-headers`. Acceptable at two copies; if a third plugin needs them, promote them to a published module rather than copying again.
- Metrics are logs-only in v1 — see the spec deviation at the top of this plan. Update the spec's observability paragraph when this plan is executed.
- `allowed_origins` is duplicated between `session.json` and `cors.json`. They serve different purposes (CSRF enforcement versus CORS advertisement) but must not drift; a future `make check` assertion comparing them would catch it.
