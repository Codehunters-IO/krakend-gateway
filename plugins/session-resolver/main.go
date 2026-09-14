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

// registerHandlers wires the plugin into the KrakenD handler chain. Task 1 owns
// only config parsing and skip-path matcher construction, both of which must
// fail closed: a bad config or an invalid skip_paths pattern prevents the
// plugin — and therefore the gateway — from loading, rather than falling back
// to defaults that would let unauthenticated traffic through.
//
// The session-cookie-to-Bearer decision logic (Tasks 2-5: decision function,
// Valkey reader, lock-guarded refresh, HTTP handler) is not implemented yet;
// until then every request is passed through unchanged.
func (r registerer) registerHandlers(_ context.Context, extra map[string]interface{}, h http.Handler) (http.Handler, error) {
	cfg, err := parseConfig(extra)
	if err != nil {
		return nil, fmt.Errorf("[%s] failed to parse config: %w", pluginName, err)
	}

	skipExact, skipRegexes, err := buildMatchers(cfg.SkipPaths)
	if err != nil {
		return nil, fmt.Errorf("[%s] invalid skip_paths: %w", pluginName, err)
	}

	logger.Info("plugin loaded", "skip_paths", cfg.SkipPaths)

	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_ = skipExact
		_ = skipRegexes
		h.ServeHTTP(w, req)
	}), nil
}

func main() {}
