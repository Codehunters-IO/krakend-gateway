package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
)

var pluginName = "krakend-jwt-headers"

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

type claimMapping struct {
	Claim  string `json:"claim"`
	Header string `json:"header"`
}

// roleRule requires the caller to hold at least one of Roles to reach a path.
// Path segments support a single-segment wildcard: "/api/v1/*/items/reserve".
type roleRule struct {
	Path  string   `json:"path"`
	Roles []string `json:"roles"`
}

type pluginConfig struct {
	JwksURL         string         `json:"jwks_url"`
	Issuer          string         `json:"issuer"`
	CacheTTLMinutes int            `json:"cache_ttl_minutes"`
	ClaimsToHeaders []claimMapping `json:"claims_to_headers"`
	SkipPaths       []string       `json:"skip_paths"`
	RequiredClaims  []string       `json:"required_claims"`
	RolesClaim      string         `json:"roles_claim"`
	RequiredRoles   []roleRule     `json:"required_roles"`
	AddIPHeader     bool           `json:"add_ip_header"`
	IPHeaderName    string         `json:"ip_header_name"`
	// Introspection (RFC 7662): when enabled, each validated token is checked
	// against Keycloak's introspection endpoint so revoked/logged-out sessions
	// are rejected immediately, not just at token expiry.
	IntrospectionEnabled         bool   `json:"introspection_enabled"`
	IntrospectionURL             string `json:"introspection_url"`
	IntrospectionClientID        string `json:"introspection_client_id"`
	IntrospectionClientSecret    string `json:"introspection_client_secret"`
	IntrospectionCacheTTLSeconds int    `json:"introspection_cache_ttl_seconds"`
}

// introspector calls the OAuth2 token introspection endpoint (RFC 7662) with an
// optional short-lived cache keyed by the token's jti to bound load.
type introspector struct {
	clientID     string
	clientSecret string
	ttl          time.Duration
	httpClient   *http.Client
	mu           sync.RWMutex
	cache        map[string]introspectEntry
}

type introspectEntry struct {
	active    bool
	expiresAt time.Time
}

func newIntrospector(cfg *pluginConfig) *introspector {
	return &introspector{
		clientID:     cfg.IntrospectionClientID,
		clientSecret: cfg.IntrospectionClientSecret,
		ttl:          time.Duration(cfg.IntrospectionCacheTTLSeconds) * time.Second,
		httpClient:   &http.Client{Timeout: 5 * time.Second},
		cache:        make(map[string]introspectEntry),
	}
}

// active reports whether the token is still active per the introspection
// endpoint at introspectURL. Results are cached by jti for the TTL (0 = none).
func (in *introspector) active(ctx context.Context, introspectURL, token, jti string) (bool, error) {
	if in.ttl > 0 && jti != "" {
		in.mu.RLock()
		entry, ok := in.cache[jti]
		in.mu.RUnlock()
		if ok && time.Now().Before(entry.expiresAt) {
			return entry.active, nil
		}
	}

	form := url.Values{}
	form.Set("token", token)
	form.Set("client_id", in.clientID)
	form.Set("client_secret", in.clientSecret)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, introspectURL, strings.NewReader(form.Encode()))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := in.httpClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("introspection endpoint returned status %d", resp.StatusCode)
	}
	var body struct {
		Active bool `json:"active"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return false, err
	}

	if in.ttl > 0 && jti != "" {
		in.mu.Lock()
		in.cache[jti] = introspectEntry{active: body.Active, expiresAt: time.Now().Add(in.ttl)}
		in.mu.Unlock()
	}
	return body.Active, nil
}

func (r registerer) registerHandlers(ctx context.Context, extra map[string]interface{}, h http.Handler) (http.Handler, error) {
	cfg, err := parseConfig(extra)
	if err != nil {
		return nil, fmt.Errorf("[%s] failed to parse config: %w", pluginName, err)
	}

	skipExact, skipRegexes, err := buildMatchers(cfg.SkipPaths)
	if err != nil {
		return nil, fmt.Errorf("[%s] invalid skip_paths: %w", pluginName, err)
	}

	cacheTTL := time.Duration(cfg.CacheTTLMinutes) * time.Minute
	if cacheTTL == 0 {
		cacheTTL = 60 * time.Minute
	}

	var tokenIntrospector *introspector
	if cfg.IntrospectionEnabled {
		tokenIntrospector = newIntrospector(cfg)
		logger.Info("introspection enabled",
			"url", cfg.IntrospectionURL, "derive_from_iss", cfg.IntrospectionURL == "",
			"cache_ttl_seconds", cfg.IntrospectionCacheTTLSeconds)
	}

	var (
		jwks      keyfunc.Keyfunc
		jwksMu    sync.RWMutex
		jwksReady bool
	)

	// Initial JWKS load (empty)
	initialJwks, err := keyfunc.NewJWKSetJSON(json.RawMessage("{}"))
	if err != nil {
		return nil, fmt.Errorf("[%s] failed to create initial keyfunc: %w", pluginName, err)
	}
	jwks = initialJwks

	// Start JWKS background fetcher
	go func() {
		for {
			k, fetchErr := keyfunc.NewDefault([]string{cfg.JwksURL})
			if fetchErr != nil {
				logger.Warn("JWKS fetch failed", "err", fetchErr.Error(), "retry_in", "10s")
				time.Sleep(10 * time.Second)
				continue
			}
			jwksMu.Lock()
			jwks = k
			jwksReady = true
			jwksMu.Unlock()
			logger.Info("JWKS loaded", "jwks_url", cfg.JwksURL)
			time.Sleep(cacheTTL)
		}
	}()

	logger.Info("plugin loaded", "skip_paths", cfg.SkipPaths, "claim_mappings", len(cfg.ClaimsToHeaders))

	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// Security: strip any caller-supplied values for the managed headers up front —
		// they can ONLY ever be (re)set from a validated JWT below.
		for _, mapping := range cfg.ClaimsToHeaders {
			req.Header.Del(mapping.Header)
		}
		if cfg.AddIPHeader {
			req.Header.Del(ipHeaderName(cfg))
		}

		// CORS preflights must never require a token
		if req.Method == http.MethodOptions {
			h.ServeHTTP(w, req)
			return
		}

		// Skip public paths (exact match or wildcard pattern)
		if skipExact[req.URL.Path] || matchesAny(req.URL.Path, skipRegexes) {
			h.ServeHTTP(w, req)
			return
		}

		jwksMu.RLock()
		ready := jwksReady
		currentJwks := jwks
		jwksMu.RUnlock()

		// Mandatory auth from here on: JWKS must be ready.
		if !ready {
			http.Error(w, `{"message":"service not ready, JWKS not loaded yet"}`, http.StatusServiceUnavailable)
			return
		}

		// Extract Bearer token
		authHeader := req.Header.Get("Authorization")
		if authHeader == "" || !strings.HasPrefix(authHeader, "Bearer ") {
			http.Error(w, `{"message":"missing or invalid authorization header"}`, http.StatusUnauthorized)
			return
		}
		tokenStr := strings.TrimPrefix(authHeader, "Bearer ")

		// Parse and validate JWT
		parserOpts := []jwt.ParserOption{jwt.WithValidMethods([]string{"RS256", "RS384", "RS512", "ES256", "ES384", "ES512"})}
		if cfg.Issuer != "" {
			parserOpts = append(parserOpts, jwt.WithIssuer(cfg.Issuer))
		}

		token, err := jwt.Parse(tokenStr, currentJwks.KeyfuncCtx(req.Context()), parserOpts...)
		if err != nil || !token.Valid {
			errMsg := ""
			if err != nil {
				errMsg = err.Error()
			}
			logger.Warn("invalid token", "err", errMsg, "path", req.URL.Path)
			http.Error(w, `{"message":"invalid or expired token"}`, http.StatusUnauthorized)
			return
		}

		claims, ok := token.Claims.(jwt.MapClaims)
		if !ok {
			http.Error(w, `{"message":"invalid token claims"}`, http.StatusUnauthorized)
			return
		}

		// Reject valid tokens that lack a required identity claim (e.g. sub).
		for _, rc := range cfg.RequiredClaims {
			if extractClaim(claims, rc) == "" {
				logger.Warn("token missing required claim", "claim", rc, "path", req.URL.Path)
				http.Error(w, `{"message":"token missing required claim"}`, http.StatusUnauthorized)
				return
			}
		}

		// Reject sessions revoked in Keycloak (logout / single-active-session):
		// a signature-valid token can still be inactive. Fail-open on endpoint
		// errors so a Keycloak blip does not take the whole platform down.
		if tokenIntrospector != nil {
			// Prefer a configured URL (reachable host); otherwise derive it from the
			// token's iss claim, which equals the validated cfg.Issuer above.
			introspectURL := cfg.IntrospectionURL
			if introspectURL == "" {
				introspectURL = extractClaim(claims, "iss") + "/protocol/openid-connect/token/introspect"
			}
			active, ierr := tokenIntrospector.active(
				req.Context(), introspectURL, tokenStr, extractClaim(claims, "jti"))
			if ierr != nil {
				logger.Warn("introspection failed, allowing token", "err", ierr.Error(), "path", req.URL.Path)
			} else if !active {
				logger.Warn("token inactive (revoked or logged out)", "path", req.URL.Path)
				http.Error(w, `{"message":"token is no longer active"}`, http.StatusUnauthorized)
				return
			}
		}

		// Enforce per-endpoint role requirements (RBAC at the edge).
		if !hasRequiredRole(cfg, claims, req.URL.Path) {
			logger.Warn("forbidden: missing required role", "path", req.URL.Path)
			http.Error(w, `{"message":"forbidden: missing required role"}`, http.StatusForbidden)
			return
		}

		// Extract claims and set headers
		for _, mapping := range cfg.ClaimsToHeaders {
			value := extractClaim(claims, mapping.Claim)
			if value != "" {
				req.Header.Set(mapping.Header, value)
			}
		}

		// Add IP header
		if cfg.AddIPHeader {
			ipHeaderName := cfg.IPHeaderName
			if ipHeaderName == "" {
				ipHeaderName = "x-ip"
			}
			req.Header.Set(ipHeaderName, extractClientIP(req))
		}

		h.ServeHTTP(w, req)
	}), nil
}

// ipHeaderName returns the configured IP header name, defaulting to "x-ip".
func ipHeaderName(cfg *pluginConfig) string {
	if cfg.IPHeaderName == "" {
		return "x-ip"
	}
	return cfg.IPHeaderName
}

// buildMatchers splits skip/optional patterns into a fast exact-match set and a
// list of compiled regexes for patterns containing wildcards. Wildcard rules:
//   - a trailing "/*" matches the rest of the path (any number of segments):
//     "/svc/api/v1/*" -> "^/svc/api/v1(/.*)?$"
//   - every other "*" matches exactly one path segment ([^/]+):
//     "/svc/api/v1/*/managers/*" -> "^/svc/api/v1/[^/]+/managers(/.*)?$"
//
// Patterns without "*" go into the exact-match set (fast path).
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

// compilePattern converts a wildcard skip/optional pattern into an anchored regex.
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

// matchesAny returns true if path matches any of the compiled regexes.
func matchesAny(path string, regexes []*regexp.Regexp) bool {
	for _, re := range regexes {
		if re.MatchString(path) {
			return true
		}
	}
	return false
}

// hasRequiredRole returns true if the path has no role rule, or the caller holds
// at least one required role. Roles are read from cfg.RolesClaim (dot notation).
func hasRequiredRole(cfg *pluginConfig, claims jwt.MapClaims, path string) bool {
	var required []string
	matched := false
	for _, rule := range cfg.RequiredRoles {
		if matchGlob(rule.Path, path) {
			matched = true
			required = append(required, rule.Roles...)
		}
	}
	if !matched {
		return true
	}
	userRoles := extractStringSlice(claims, cfg.RolesClaim)
	for _, ur := range userRoles {
		for _, rr := range required {
			if ur == rr {
				return true
			}
		}
	}
	return false
}

// matchGlob matches segment by segment; "*" matches exactly one path segment.
func matchGlob(pattern, path string) bool {
	p := strings.Split(strings.Trim(pattern, "/"), "/")
	s := strings.Split(strings.Trim(path, "/"), "/")
	if len(p) != len(s) {
		return false
	}
	for i := range p {
		if p[i] != "*" && p[i] != s[i] {
			return false
		}
	}
	return true
}

// extractStringSlice navigates nested claims (dot notation) and returns a string array.
func extractStringSlice(claims jwt.MapClaims, pathStr string) []string {
	if pathStr == "" {
		return nil
	}
	var current interface{} = map[string]interface{}(claims)
	for _, part := range strings.Split(pathStr, ".") {
		m, ok := current.(map[string]interface{})
		if !ok {
			return nil
		}
		current, ok = m[part]
		if !ok {
			return nil
		}
	}
	arr, ok := current.([]interface{})
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, v := range arr {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// extractClaim navigates nested claims using dot notation.
// For example, "realm_access.roles" navigates claims["realm_access"]["roles"].
func extractClaim(claims jwt.MapClaims, path string) string {
	parts := strings.Split(path, ".")
	var current interface{} = map[string]interface{}(claims)

	for _, part := range parts {
		m, ok := current.(map[string]interface{})
		if !ok {
			return ""
		}
		current, ok = m[part]
		if !ok {
			return ""
		}
	}

	switch v := current.(type) {
	case string:
		return v
	case float64:
		if v == float64(int64(v)) {
			return fmt.Sprintf("%d", int64(v))
		}
		return fmt.Sprintf("%g", v)
	case bool:
		return fmt.Sprintf("%t", v)
	case []interface{}:
		b, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(b)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

// extractClientIP gets the client IP from X-Forwarded-For or RemoteAddr.
func extractClientIP(req *http.Request) string {
	if xff := req.Header.Get("X-Forwarded-For"); xff != "" {
		// X-Forwarded-For can contain multiple IPs; take the first one
		if idx := strings.Index(xff, ","); idx != -1 {
			return strings.TrimSpace(xff[:idx])
		}
		return strings.TrimSpace(xff)
	}

	if xRealIP := req.Header.Get("X-Real-Ip"); xRealIP != "" {
		return strings.TrimSpace(xRealIP)
	}

	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		return req.RemoteAddr
	}
	return host
}

// parseConfig extracts plugin configuration from KrakenD's extra_config map.
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

	if cfg.JwksURL == "" {
		return nil, fmt.Errorf("jwks_url is required")
	}

	return &cfg, nil
}

func main() {}
