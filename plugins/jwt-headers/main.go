package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
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

type pluginConfig struct {
	JwksURL         string         `json:"jwks_url"`
	Issuer          string         `json:"issuer"`
	CacheTTLMinutes int            `json:"cache_ttl_minutes"`
	ClaimsToHeaders []claimMapping `json:"claims_to_headers"`
	SkipPaths       []string       `json:"skip_paths"`
	AddIPHeader     bool           `json:"add_ip_header"`
	IPHeaderName    string         `json:"ip_header_name"`
}

func (r registerer) registerHandlers(ctx context.Context, extra map[string]interface{}, h http.Handler) (http.Handler, error) {
	cfg, err := parseConfig(extra)
	if err != nil {
		return nil, fmt.Errorf("[%s] failed to parse config: %w", pluginName, err)
	}

	skipExact := make(map[string]bool, len(cfg.SkipPaths))
	skipPrefixes := make([]string, 0)
	for _, p := range cfg.SkipPaths {
		if strings.HasSuffix(p, "/*") {
			skipPrefixes = append(skipPrefixes, strings.TrimSuffix(p, "*"))
			continue
		}
		skipExact[p] = true
	}

	cacheTTL := time.Duration(cfg.CacheTTLMinutes) * time.Minute
	if cacheTTL == 0 {
		cacheTTL = 60 * time.Minute
	}

	var (
		jwks    keyfunc.Keyfunc
		jwksMu  sync.RWMutex
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
		// CORS preflights must never require a token
		if req.Method == http.MethodOptions {
			h.ServeHTTP(w, req)
			return
		}

		// Skip public paths (exact match or /* prefix wildcard)
		if skipExact[req.URL.Path] || matchesPrefix(req.URL.Path, skipPrefixes) {
			h.ServeHTTP(w, req)
			return
		}

		// Check JWKS readiness
		jwksMu.RLock()
		ready := jwksReady
		currentJwks := jwks
		jwksMu.RUnlock()
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

// matchesPrefix returns true if path starts with any of the given prefixes.
func matchesPrefix(path string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
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
